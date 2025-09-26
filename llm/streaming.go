package llm

import (
	"auralis_back/agents"
	"auralis_back/tts"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// wantsEventStream determines if the client requested a streaming response.
func wantsEventStream(c *gin.Context) bool {
	accept := strings.ToLower(strings.TrimSpace(c.GetHeader("Accept")))
	if strings.Contains(accept, "text/event-stream") {
		return true
	}
	if header := strings.TrimSpace(c.GetHeader("X-Stream")); header != "" {
		if strings.EqualFold(header, "1") || strings.EqualFold(header, "true") || strings.EqualFold(header, "yes") {
			return true
		}
	}
	if q := strings.TrimSpace(c.Query("stream")); q != "" {
		if strings.EqualFold(q, "1") || strings.EqualFold(q, "true") || strings.EqualFold(q, "yes") {
			return true
		}
	}
	return false
}

// streamEvent writes a single Server-Sent Event to the response writer.
func streamEvent(w gin.ResponseWriter, flusher http.Flusher, event string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

type conversationContext struct {
	agent    agents.Agent
	config   *agents.AgentChatConfig
	profile  *userProfile
	summary  string
	history  []message
	messages []ChatMessage
}

func (m *Module) buildConversationContext(ctx context.Context, conv conversation) (*conversationContext, error) {
	var agentModel agents.Agent
	if err := m.db.WithContext(ctx).First(&agentModel, "id = ?", conv.AgentID).Error; err != nil {
		return nil, fmt.Errorf("load agent: %w", err)
	}

	var cfg agents.AgentChatConfig
	cfgErr := m.db.WithContext(ctx).First(&cfg, "agent_id = ?", conv.AgentID).Error
	var cfgPtr *agents.AgentChatConfig
	if cfgErr == nil {
		cfgPtr = &cfg
	} else if !errors.Is(cfgErr, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("load agent config: %w", cfgErr)
	}

	limit := 20
	if m.memory != nil {
		limit = m.memory.recentMessageLimit()
	}
	if limit <= 0 {
		limit = 20
	}

	var history []message
	if err := m.db.WithContext(ctx).
		Where("conversation_id = ?", conv.ID).
		Order("seq DESC").
		Limit(limit).
		Find(&history).Error; err != nil {
		return nil, fmt.Errorf("load history: %w", err)
	}
	if len(history) > 1 {
		for i, j := 0, len(history)-1; i < j; i, j = i+1, j-1 {
			history[i], history[j] = history[j], history[i]
		}
	}

	var summaryText string
	if conv.Summary != nil {
		summaryText = strings.TrimSpace(*conv.Summary)
	}

	var profile *userProfile
	if m.memory != nil {
		prof, err := m.memory.loadUserProfile(ctx, conv.AgentID, conv.UserID)
		if err != nil {
			return nil, fmt.Errorf("load user profile: %w", err)
		}
		profile = prof
	}

	systemPrompt := buildSystemPrompt(&agentModel, cfgPtr)

	messages := make([]ChatMessage, 0, len(history)+3)
	if systemPrompt != "" {
		messages = append(messages, ChatMessage{Role: "system", Content: systemPrompt})
	}
	if summaryText != "" {
		messages = append(messages, ChatMessage{Role: "system", Content: "Conversation memory summary:\n" + summaryText})
	}
	if prompt := profilePrompt(profile); prompt != "" {
		messages = append(messages, ChatMessage{Role: "system", Content: prompt})
	}

	for _, item := range history {
		role := strings.ToLower(strings.TrimSpace(item.Role))
		if role != "user" && role != "assistant" && role != "system" {
			continue
		}
		messages = append(messages, ChatMessage{Role: role, Content: item.Content})
	}

	return &conversationContext{
		agent:    agentModel,
		config:   cfgPtr,
		profile:  profile,
		summary:  summaryText,
		history:  history,
		messages: messages,
	}, nil
}

func profilePrompt(profile *userProfile) string {
	if profile == nil {
		return ""
	}
	parts := make([]string, 0, 3)
	if summary := strings.TrimSpace(profile.Summary); summary != "" {
		parts = append(parts, "User persona: "+summary)
	}
	if pref := formatPreferences(profile.Preferences); pref != "" {
		parts = append(parts, "Known preferences: "+pref)
	}
	if last := strings.TrimSpace(profile.LastTask); last != "" {
		parts = append(parts, "Outstanding task: "+last)
	}
	if len(parts) == 0 {
		return ""
	}
	return "User context:\n" + strings.Join(parts, "\n")
}

func formatPreferences(prefs map[string]any) string {
	if len(prefs) == 0 {
		return ""
	}
	keys := make([]string, 0, len(prefs))
	for key := range prefs {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var builder strings.Builder
	for i, key := range keys {
		if i > 0 {
			builder.WriteString("; ")
		}
		normalizedKey := strings.ReplaceAll(strings.TrimSpace(key), "_", " ")
		builder.WriteString(normalizedKey)
		builder.WriteString(": ")
		builder.WriteString(stringifyPreferenceValue(prefs[key]))
	}
	return builder.String()
}

func stringifyPreferenceValue(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatFloat(v, 'f', 2, 64)
	case float32:
		return strconv.FormatFloat(float64(v), 'f', 2, 64)
	case int:
		return strconv.Itoa(v)
	case int32:
		return strconv.FormatInt(int64(v), 10)
	case int64:
		return strconv.FormatInt(v, 10)
	case uint:
		return strconv.FormatUint(uint64(v), 10)
	case uint32:
		return strconv.FormatUint(uint64(v), 10)
	case uint64:
		return strconv.FormatUint(v, 10)
	case bool:
		if v {
			return "yes"
		}
		return "no"
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(raw)
	}
}

func messageToRecord(msg message, conv conversation) messageRecord {
	return messageRecord{
		ID:              msg.ID,
		ConversationID:  msg.ConversationID,
		AgentID:         conv.AgentID,
		UserID:          conv.UserID,
		Seq:             msg.Seq,
		Role:            msg.Role,
		Format:          msg.Format,
		Content:         msg.Content,
		ParentMessageID: msg.ParentMessageID,
		LatencyMs:       msg.LatencyMs,
		TokenInput:      msg.TokenInput,
		TokenOutput:     msg.TokenOutput,
		ErrCode:         msg.ErrCode,
		ErrMsg:          msg.ErrMsg,
		Extras:          toRawMessage(msg.Extras),
		CreatedAt:       msg.CreatedAt,
	}
}

func mergeExtras(existing datatypes.JSON, updates map[string]any) (datatypes.JSON, error) {
	merged := make(map[string]any)
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &merged); err != nil {
			return nil, err
		}
	}
	for key, value := range updates {
		merged[key] = value
	}
	if len(merged) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	return datatypes.JSON(raw), nil
}

func (m *Module) createAssistantPlaceholder(ctx context.Context, conv conversation, parent message) (message, error) {
	var created message
	err := m.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var lastSeq int
		if err := tx.Model(&message{}).Where("conversation_id = ?", conv.ID).Select("MAX(seq)").Scan(&lastSeq).Error; err != nil {
			return err
		}
		seq := lastSeq + 1
		parentID := parent.ID
		msg := message{
			ConversationID:  conv.ID,
			Seq:             seq,
			Role:            "assistant",
			Format:          "text",
			Content:         "",
			ParentMessageID: &parentID,
		}
		if err := tx.Create(&msg).Error; err != nil {
			return err
		}
		if err := tx.First(&msg, "id = ?", msg.ID).Error; err != nil {
			return err
		}
		if err := tx.Model(&conversation{}).Where("id = ?", conv.ID).Update("last_msg_at", time.Now().UTC()).Error; err != nil {
			return err
		}
		created = msg
		return nil
	})
	return created, err
}

func (m *Module) enqueueSpeechSynthesis(msgID uint64, conv conversation, content string, selection voiceSelection, speed, pitch float64, emotion *emotionMetadata) {
	if m.tts == nil || !m.tts.Enabled() {
		return
	}

	provider := normalizeVoiceProvider(selection.Provider)
	baseVoiceID := strings.TrimSpace(selection.ID)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
		defer cancel()

		providerLocal := provider
		voiceID := baseVoiceID

		req := tts.SpeechRequest{
			Text:     content,
			VoiceID:  voiceID,
			Provider: providerLocal,
			Speed:    speed,
			Pitch:    pitch,
		}
		if emotion != nil {
			req.Emotion = emotion.Label
			if strings.TrimSpace(req.Instructions) == "" {
				req.Instructions = fmt.Sprintf("Please speak with a %s tone.", emotion.Label)
			}
		}

		result, err := m.tts.Synthesize(ctx, req)
		status := "ready"
		updates := make(map[string]any)
		if err != nil {
			status = "error"
			updates["speech_error"] = err.Error()
			log.Printf("llm: synthesize speech async failed: %v", err)
		} else if result != nil {
			result.AudioURL = ""
			updates["speech"] = result.AsMap()
			if strings.TrimSpace(result.Provider) != "" {
				providerLocal = normalizeVoiceProvider(result.Provider)
			}
			if voiceID == "" {
				voiceID = result.VoiceID
			}
		}
		updates["speech_status"] = status

		dbCtx, cancelDB := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelDB()

		var msg message
		if err := m.db.WithContext(dbCtx).First(&msg, "id = ?", msgID).Error; err != nil {
			log.Printf("llm: load message for speech update failed: %v", err)
			return
		}

		extrasMap := make(map[string]any)
		if len(msg.Extras) > 0 {
			if err := json.Unmarshal(msg.Extras, &extrasMap); err != nil {
				log.Printf("llm: parse message extras failed: %v", err)
				extrasMap = make(map[string]any)
			}
		}

		for k, v := range updates {
			extrasMap[k] = v
		}
		prefsMap, ok := extrasMap["speech_preferences"].(map[string]any)
		if !ok || prefsMap == nil {
			prefsMap = make(map[string]any)
			extrasMap["speech_preferences"] = prefsMap
		}
		if voiceID != "" {
			if existing, ok := prefsMap["voice_id"].(string); !ok || strings.TrimSpace(existing) == "" {
				prefsMap["voice_id"] = voiceID
			}
		}
		if providerLocal != "" {
			if existing, ok := prefsMap["provider"].(string); !ok || strings.TrimSpace(existing) == "" {
				prefsMap["provider"] = providerLocal
			}
		}

		raw, err := json.Marshal(extrasMap)
		if err != nil {
			log.Printf("llm: marshal extras after speech failed: %v", err)
			return
		}

		if err := m.db.WithContext(dbCtx).Model(&message{}).Where("id = ?", msgID).Update("extras", datatypes.JSON(raw)).Error; err != nil {
			log.Printf("llm: update message extras with speech failed: %v", err)
			return
		}
	}()
}

func (m *Module) handleCreateMessageStream(
	c *gin.Context,
	conv conversation,
	userMsg message,
	userRecord messageRecord,
	prefs speechPreferences,
) {
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		assistantRecord, genErr := m.generateAssistantReply(c.Request.Context(), conv, userMsg, prefs)
		response := createMessageResponse{
			ConversationID: conv.ID,
			AgentID:        conv.AgentID,
			UserID:         conv.UserID,
			UserMessage:    userRecord,
		}
		if genErr != nil {
			response.AssistantError = genErr.Error()
		} else if assistantRecord != nil {
			response.AssistantMessage = assistantRecord
		}
		c.JSON(http.StatusCreated, response)
		return
	}

	ctx := c.Request.Context()

	contextData, err := m.buildConversationContext(ctx, conv)
	if err != nil {
		c.Status(http.StatusInternalServerError)
		_ = streamEvent(c.Writer, flusher, "error", gin.H{"error": err.Error()})
		return
	}

	applyPreferenceDefaults(&prefs, contextData)

	placeholder, err := m.createAssistantPlaceholder(ctx, conv, userMsg)
	if err != nil {
		c.Status(http.StatusInternalServerError)
		_ = streamEvent(c.Writer, flusher, "error", gin.H{"error": "failed to prepare assistant message"})
		return
	}

	assistantRecord := messageToRecord(placeholder, conv)

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache, no-transform")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Status(http.StatusCreated)
	flusher.Flush()

	if err := streamEvent(c.Writer, flusher, "user_message", userRecord); err != nil {
		return
	}
	if err := streamEvent(c.Writer, flusher, "assistant_placeholder", assistantRecord); err != nil {
		return
	}

	start := time.Now()

	updateContent := func(content string) error {
		return m.db.WithContext(ctx).
			Model(&message{}).
			Where("id = ?", placeholder.ID).
			Update("content", content).
			Error
	}

	streamHandler := func(delta ChatStreamDelta) error {
		if delta.Content != "" {
			if err := updateContent(delta.FullContent); err != nil {
				return err
			}
		}
		if delta.Content == "" && !delta.Done {
			if delta.FinishReason == "" {
				return nil
			}
		}
		payload := gin.H{
			"id":   placeholder.ID,
			"full": delta.FullContent,
		}
		if delta.Content != "" {
			payload["delta"] = delta.Content
		}
		if delta.FinishReason != "" {
			payload["finish_reason"] = delta.FinishReason
		}
		if delta.Done {
			payload["done"] = true
		}
		return streamEvent(c.Writer, flusher, "assistant_delta", payload)
	}

	streamResult, streamErr := m.client.ChatStream(ctx, contextData.messages, streamHandler)
	reply := streamResult.Content
	usage := streamResult.Usage
	if streamErr != nil {
		log.Printf("llm: streaming fallback to non-streaming: %v", streamErr)
		fallback, err := m.client.Chat(ctx, contextData.messages)
		if err != nil {
			_ = streamEvent(c.Writer, flusher, "error", gin.H{"error": err.Error()})
			return
		}
		reply = fallback.Content
		usage = fallback.Usage
		if err := updateContent(reply); err != nil {
			_ = streamEvent(c.Writer, flusher, "error", gin.H{"error": "failed to update assistant message"})
			return
		}
		if err := streamHandler(ChatStreamDelta{Content: reply, FullContent: reply, Done: true}); err != nil {
			return
		}
	}

	if reply == "" {
		var refreshed message
		if err := m.db.WithContext(ctx).First(&refreshed, "id = ?", placeholder.ID).Error; err == nil {
			reply = refreshed.Content
			placeholder = refreshed
		}
	} else {
		if err := m.db.WithContext(ctx).First(&placeholder, "id = ?", placeholder.ID).Error; err != nil {
			log.Printf("llm: failed to reload assistant message: %v", err)
		}
	}

	if reply == "" {
		_ = streamEvent(c.Writer, flusher, "error", gin.H{"error": "assistant reply empty"})
		return
	}

	if usage != nil {
		updates := make(map[string]any, 2)
		if usage.PromptTokens > 0 {
			placeholder.TokenInput = intPointerIfPositive(usage.PromptTokens)
			updates["token_input"] = usage.PromptTokens
		}
		if usage.CompletionTokens > 0 {
			placeholder.TokenOutput = intPointerIfPositive(usage.CompletionTokens)
			updates["token_output"] = usage.CompletionTokens
		}
		if len(updates) > 0 {
			if err := m.db.WithContext(ctx).Model(&message{}).Where("id = ?", placeholder.ID).Updates(updates).Error; err != nil {
				log.Printf("llm: failed to update token usage: %v", err)
			} else {
				m.incrementConversationTokens(ctx, conv.ID, usage)
			}
		}
	}

	latency := int(time.Since(start).Milliseconds())
	if err := m.db.WithContext(ctx).
		Model(&message{}).
		Where("id = ?", placeholder.ID).
		Update("latency_ms", latency).
		Error; err != nil {
		log.Printf("llm: failed to update latency: %v", err)
	}

	selection := resolveVoiceSelection(prefs.VoiceID, prefs.Provider, m.tts)
	prefs.Provider = selection.Provider
	voiceID := selection.ID
	speed := sanitizeSpeed(prefs.Speed)
	pitch := sanitizePitch(prefs.Pitch)
	emotionMeta := inferEmotion(reply, prefs.EmotionHint)

	extrasPayload := make(map[string]any)
	if emotionMeta != nil {
		extrasPayload["emotion"] = emotionMeta
	}
	if voiceID != "" || speed != 1.0 || pitch != 1.0 {
		prefsMap := map[string]any{
			"voice_id": voiceID,
			"speed":    speed,
			"pitch":    pitch,
		}
		if selection.Provider != "" {
			prefsMap["provider"] = selection.Provider
		}
		extrasPayload["speech_preferences"] = prefsMap
	}
	speechEnabled := m.tts != nil && m.tts.Enabled()
	if speechEnabled {
		extrasPayload["speech_status"] = "pending"
	}

	if len(extrasPayload) > 0 {
		if merged, err := mergeExtras(placeholder.Extras, extrasPayload); err != nil {
			log.Printf("llm: merge extras failed: %v", err)
		} else {
			if err := m.db.WithContext(ctx).
				Model(&message{}).
				Where("id = ?", placeholder.ID).
				Update("extras", merged).
				Error; err != nil {
				log.Printf("llm: failed to update extras: %v", err)
			} else {
				placeholder.Extras = merged
			}
		}
	}

	if err := m.db.WithContext(ctx).First(&placeholder, "id = ?", placeholder.ID).Error; err != nil {
		_ = streamEvent(c.Writer, flusher, "error", gin.H{"error": "failed to reload assistant message"})
		return
	}

	assistantRecord = messageToRecord(placeholder, conv)
	if err := streamEvent(c.Writer, flusher, "assistant_message", assistantRecord); err != nil {
		return
	}

	if speechEnabled {
		m.enqueueSpeechSynthesis(placeholder.ID, conv, reply, selection, speed, pitch, emotionMeta)
	}

	if m.memory != nil {
		if summary, err := m.memory.ensureSummary(ctx, conv); err != nil {
			log.Printf("llm: update conversation summary: %v", err)
		} else if summary != "" {
			conv.Summary = &summary
		}
	}

	_ = streamEvent(c.Writer, flusher, "done", gin.H{"id": placeholder.ID})
}
