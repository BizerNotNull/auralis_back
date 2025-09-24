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
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

var allowedMessageRoles = map[string]struct{}{
	"system":    {},
	"user":      {},
	"assistant": {},
	"tool":      {},
}

type Module struct {
	client *ChatClient
	db     *gorm.DB
	tts    tts.Synthesizer
}

// RegisterRoutes mounts the LLM testing endpoint under /llm.
func RegisterRoutes(router *gin.Engine, synthesizer tts.Synthesizer) (*Module, error) {
	client, err := NewChatClientFromEnv()
	if err != nil {
		return nil, err
	}

	db, err := openDatabaseFromEnv()
	if err != nil {
		return nil, err
	}

	if err := db.AutoMigrate(&conversation{}, &message{}); err != nil {
		return nil, err
	}

	module := &Module{client: client, db: db, tts: synthesizer}

	group := router.Group("/llm")
	group.POST("/complete", module.handleComplete)
	group.GET("/messages", module.handleRecentMessages)
	group.POST("/messages", module.handleCreateMessage)

	return module, nil
}

type completeRequest struct {
	Prompt string `json:"prompt" binding:"required"`
}

type completeResponse struct {
	Prompt  string `json:"prompt"`
	Content string `json:"content"`
}

func (m *Module) handleComplete(c *gin.Context) {
	var req completeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "prompt is required"})
		return
	}

	content, err := m.client.Complete(c.Request.Context(), req.Prompt)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, completeResponse{
		Prompt:  req.Prompt,
		Content: content,
	})
}

type messageRecord struct {
	ID              uint64          `json:"id"`
	ConversationID  uint64          `json:"conversation_id"`
	AgentID         uint64          `json:"agent_id"`
	UserID          uint64          `json:"user_id"`
	Seq             int             `json:"seq"`
	Role            string          `json:"role"`
	Format          string          `json:"format"`
	Content         string          `json:"content"`
	ParentMessageID *uint64         `json:"parent_message_id,omitempty" gorm:"column:parent_msg_id"`
	LatencyMs       *int            `json:"latency_ms,omitempty"`
	TokenInput      *int            `json:"token_input,omitempty"`
	TokenOutput     *int            `json:"token_output,omitempty"`
	ErrCode         *string         `json:"err_code,omitempty"`
	ErrMsg          *string         `json:"err_msg,omitempty"`
	Extras          json.RawMessage `json:"extras,omitempty" gorm:"column:extras"`
	CreatedAt       time.Time       `json:"created_at"`
}

func (m *Module) handleRecentMessages(c *gin.Context) {
	if m.db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database not initialized"})
		return
	}

	agentIDStr := strings.TrimSpace(c.Query("agent_id"))
	userIDStr := strings.TrimSpace(c.Query("user_id"))
	if agentIDStr == "" || userIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "agent_id and user_id are required"})
		return
	}

	agentID, err := strconv.ParseUint(agentIDStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid agent_id"})
		return
	}

	userID, err := strconv.ParseUint(userIDStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user_id"})
		return
	}

	var records []messageRecord
	tx := m.db.WithContext(c.Request.Context()).
		Table("messages").
		Select("messages.id, messages.conversation_id, conversations.agent_id, conversations.user_id, messages.seq, messages.role, messages.format, messages.content, messages.parent_msg_id, messages.latency_ms, messages.token_input, messages.token_output, messages.err_code, messages.err_msg, messages.extras, messages.created_at").
		Joins("JOIN conversations ON conversations.id = messages.conversation_id").
		Where("conversations.agent_id = ? AND conversations.user_id = ?", agentID, userID).
		Order("messages.created_at DESC, messages.id DESC").
		Limit(50)

	if err := tx.Scan(&records).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load messages", "details": err.Error()})
		return
	}

	for i, j := 0, len(records)-1; i < j; i, j = i+1, j-1 {
		records[i], records[j] = records[j], records[i]
	}

	c.JSON(http.StatusOK, gin.H{
		"agent_id": agentID,
		"user_id":  userID,
		"messages": records,
	})
}

type createMessageRequest struct {
	AgentID     string   `json:"agent_id" binding:"required"`
	UserID      string   `json:"user_id" binding:"required"`
	Role        string   `json:"role" binding:"required"`
	Content     string   `json:"content" binding:"required"`
	VoiceID     string   `json:"voice_id"`
	EmotionHint string   `json:"emotion_hint"`
	SpeechSpeed *float64 `json:"speech_speed,omitempty"`
	SpeechPitch *float64 `json:"speech_pitch,omitempty"`
}

type speechPreferences struct {
	VoiceID     string
	EmotionHint string
	Speed       float64
	Pitch       float64
}

type emotionMetadata struct {
	Label           string  `json:"label"`
	Intensity       float64 `json:"intensity"`
	Confidence      float64 `json:"confidence"`
	Reason          string  `json:"reason,omitempty"`
	SuggestedMotion string  `json:"suggested_motion,omitempty"`
}

type createMessageResponse struct {
	ConversationID   uint64         `json:"conversation_id"`
	AgentID          uint64         `json:"agent_id"`
	UserID           uint64         `json:"user_id"`
	UserMessage      messageRecord  `json:"user_message"`
	AssistantMessage *messageRecord `json:"assistant_message,omitempty"`
	AssistantError   string         `json:"assistant_error,omitempty"`
}

func (m *Module) handleCreateMessage(c *gin.Context) {
	if m.db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database not initialized"})
		return
	}

	var req createMessageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request payload"})
		return
	}

	agentID, parseErr := parsePositiveUint(req.AgentID, "agent_id")
	if parseErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": parseErr.Error()})
		return
	}

	userID, parseErr := parsePositiveUint(req.UserID, "user_id")
	if parseErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": parseErr.Error()})
		return
	}

	role := strings.ToLower(strings.TrimSpace(req.Role))
	if _, ok := allowedMessageRoles[role]; !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid role"})
		return
	}

	content := req.Content
	if strings.TrimSpace(content) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "content cannot be empty"})
		return
	}

	prefs := speechPreferences{
		VoiceID:     strings.TrimSpace(req.VoiceID),
		EmotionHint: strings.TrimSpace(req.EmotionHint),
		Speed:       1.0,
		Pitch:       1.0,
	}
	if req.SpeechSpeed != nil {
		prefs.Speed = *req.SpeechSpeed
	}
	if req.SpeechPitch != nil {
		prefs.Pitch = *req.SpeechPitch
	}

	ctx := c.Request.Context()

	var convID uint64
	var userMsg message
	var userRecord messageRecord

	err := m.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()

		var conv conversation
		err := tx.Where("agent_id = ? AND user_id = ?", agentID, userID).Take(&conv).Error
		if err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}

			conv = conversation{
				AgentID:   agentID,
				UserID:    userID,
				Status:    "active",
				StartedAt: now,
				LastMsgAt: now,
			}
			if err := tx.Create(&conv).Error; err != nil {
				return err
			}
		} else {
			if err := tx.Model(&conversation{}).Where("id = ?", conv.ID).Update("last_msg_at", now).Error; err != nil {
				return err
			}
		}

		var seq int
		var parentID *uint64

		var last message
		if err := tx.Where("conversation_id = ?", conv.ID).Order("seq DESC").Take(&last).Error; err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			seq = 1
		} else {
			seq = last.Seq + 1
			parent := last.ID
			parentID = &parent
		}

		msg := message{
			ConversationID:  conv.ID,
			Seq:             seq,
			Role:            role,
			Format:          "text",
			Content:         content,
			ParentMessageID: parentID,
		}

		if err := tx.Create(&msg).Error; err != nil {
			return err
		}

		if err := tx.Where("id = ?", msg.ID).Take(&msg).Error; err != nil {
			return err
		}

		userMsg = msg
		convID = conv.ID

		userRecord = messageRecord{
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

		return nil
	})

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create message", "details": err.Error()})
		return
	}

	var conv conversation
	if err := m.db.WithContext(ctx).First(&conv, "id = ?", convID).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load conversation", "details": err.Error()})
		return
	}

	response := createMessageResponse{
		ConversationID: conv.ID,
		AgentID:        conv.AgentID,
		UserID:         conv.UserID,
		UserMessage:    userRecord,
	}

	if role == "user" && wantsEventStream(c) {
		m.handleCreateMessageStream(c, conv, userMsg, userRecord, prefs)
		return
	}

	if role == "user" {
		assistantRecord, genErr := m.generateAssistantReply(ctx, conv, userMsg, prefs)
		if genErr != nil {
			response.AssistantError = genErr.Error()
		} else if assistantRecord != nil {
			response.AssistantMessage = assistantRecord
		}
	}

	c.JSON(http.StatusCreated, response)
}

func parsePositiveUint(value, field string) (uint64, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, fmt.Errorf("%s is required", field)
	}

	id, err := strconv.ParseUint(trimmed, 10, 64)
	if err != nil || id == 0 {
		return 0, fmt.Errorf("%s must be a positive integer", field)
	}

	return id, nil
}

func (m *Module) generateAssistantReply(ctx context.Context, conv conversation, userMsg message, prefs speechPreferences) (*messageRecord, error) {
	if m.client == nil {
		return nil, errors.New("llm client not configured")
	}

	contextData, err := m.buildConversationContext(ctx, conv)
	if err != nil {
		return nil, err
	}

	start := time.Now()
	reply, err := m.client.Chat(ctx, contextData.messages)
	if err != nil {
		short := truncateString(err.Error(), 256)
		_ = m.db.WithContext(ctx).Model(&message{}).Where("id = ?", userMsg.ID).Updates(map[string]any{
			"err_code": "llm_error",
			"err_msg":  short,
		})
		return nil, err
	}

	latency := int(time.Since(start).Milliseconds())
	parentID := userMsg.ID

	voiceID := resolveVoiceID(prefs.VoiceID, m.tts)
	speed := sanitizeSpeed(prefs.Speed)
	pitch := sanitizePitch(prefs.Pitch)
	emotionMeta := inferEmotion(reply, prefs.EmotionHint)

	extrasPayload := make(map[string]any)
	if emotionMeta != nil {
		extrasPayload["emotion"] = emotionMeta
	}
	if voiceID != "" || speed != 1.0 || pitch != 1.0 {
		extrasPayload["speech_preferences"] = map[string]any{
			"voice_id": voiceID,
			"speed":    speed,
			"pitch":    pitch,
		}
	}
	speechEnabled := m.tts != nil && m.tts.Enabled()
	if speechEnabled {
		extrasPayload["speech_status"] = "pending"
	}

	assistant := message{
		ConversationID:  conv.ID,
		Role:            "assistant",
		Format:          "text",
		Content:         reply,
		ParentMessageID: &parentID,
	}
	if latency > 0 {
		assistant.LatencyMs = &latency
	}

	if len(extrasPayload) > 0 {
		if raw, marshalErr := json.Marshal(extrasPayload); marshalErr != nil {
			log.Printf("llm: marshal message extras: %v", marshalErr)
		} else {
			assistant.Extras = datatypes.JSON(raw)
		}
	}

	if err := m.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var lastSeq int
		if err := tx.Model(&message{}).Where("conversation_id = ?", conv.ID).Select("MAX(seq)").Scan(&lastSeq).Error; err != nil {
			return err
		}
		assistant.Seq = lastSeq + 1

		if err := tx.Create(&assistant).Error; err != nil {
			return err
		}

		return tx.Model(&conversation{}).Where("id = ?", conv.ID).Update("last_msg_at", time.Now().UTC()).Error
	}); err != nil {
		return nil, err
	}

	if err := m.db.WithContext(ctx).First(&assistant, "id = ?", assistant.ID).Error; err != nil {
		return nil, err
	}

	record := messageToRecord(assistant, conv)

	if speechEnabled {
		m.enqueueSpeechSynthesis(assistant.ID, conv, reply, voiceID, speed, pitch, emotionMeta)
	}

	return &record, nil
}

func buildSystemPrompt(agent *agents.Agent, cfg *agents.AgentChatConfig) string {
	if agent == nil {
		return "You are a helpful assistant."
	}

	parts := []string{fmt.Sprintf("You are %s, a virtual smart companion. Stay engaging, empathetic, and professional.", agent.Name)}

	if agent.PersonaDesc != nil {
		if desc := strings.TrimSpace(*agent.PersonaDesc); desc != "" {
			parts = append(parts, desc)
		}
	}

	if cfg != nil && cfg.SystemPrompt != nil {
		if custom := strings.TrimSpace(*cfg.SystemPrompt); custom != "" {
			parts = append(parts, custom)
		}
	}

	if agent.FirstTurnHint != nil {
		if hint := strings.TrimSpace(*agent.FirstTurnHint); hint != "" {
			parts = append(parts, "Conversation guidance: "+hint)
		}
	}

	if lang := strings.TrimSpace(agent.LangDefault); lang != "" {
		parts = append(parts, fmt.Sprintf("Reply in %s unless the user explicitly requests another language.", lang))
	}

	return strings.Join(parts, "\n\n")
}

func truncateString(value string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(value) <= max {
		return value
	}
	if max <= 3 {
		return value[:max]
	}
	return value[:max-3] + "..."
}

func toRawMessage(data datatypes.JSON) json.RawMessage {
	if len(data) == 0 {
		return nil
	}
	clone := make([]byte, len(data))
	copy(clone, data)
	return json.RawMessage(clone)
}

func resolveVoiceID(candidate string, synth tts.Synthesizer) string {
	trimmed := strings.TrimSpace(candidate)
	if trimmed != "" {
		return trimmed
	}
	if synth != nil {
		if voice := strings.TrimSpace(synth.DefaultVoiceID()); voice != "" {
			return voice
		}
		if voices := synth.Voices(); len(voices) > 0 {
			return strings.TrimSpace(voices[0].ID)
		}
	}
	return ""
}

func sanitizeSpeed(value float64) float64 {
	if value <= 0 {
		return 1.0
	}
	if value < 0.5 {
		return 0.5
	}
	if value > 1.6 {
		return 1.6
	}
	return value
}

func sanitizePitch(value float64) float64 {
	if value <= 0 {
		return 1.0
	}
	if value < 0.7 {
		return 0.7
	}
	if value > 1.4 {
		return 1.4
	}
	return value
}

func clampFloat(value, min, max float64) float64 {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func normalizeEmotionLabel(value string) string {
	trimmed := strings.TrimSpace(strings.ToLower(value))
	switch trimmed {
	case "", "neutral", "平静", "calm", "默认":
		return "neutral"
	case "开心", "高兴", "喜悦", "太棒了", "happy", "cheerful":
		return "happy"
	case "sad", "难过", "伤心", "悲伤", "遗憾":
		return "sad"
	case "angry", "生气", "愤怒", "怒火":
		return "angry"
	case "surprised", "惊讶", "惊喜", "没想到":
		return "surprised"
	case "坚定", "自信", "confident":
		return "confident"
	case "温柔", "gentle", "soft":
		return "gentle"
	default:
		return trimmed
	}
}

func motionForEmotion(label string, intensity float64) string {
	switch label {
	case "happy":
		if intensity > 0.6 {
			return "happy_jump"
		}
		return "happy_smile"
	case "sad":
		if intensity > 0.6 {
			return "sad_drop"
		}
		return "sad_idle"
	case "angry":
		if intensity > 0.6 {
			return "angry_point"
		}
		return "angry_idle"
	case "surprised":
		return "surprised_react"
	case "confident":
		return "pose_proud"
	case "gentle":
		return "gentle_wave"
	default:
		if intensity > 0.5 {
			return "idle_emphatic"
		}
		return "idle_breathe"
	}
}

func inferEmotion(text, hint string) *emotionMetadata {
	trimmed := strings.TrimSpace(text)
	normalizedHint := normalizeEmotionLabel(hint)
	if trimmed == "" && normalizedHint == "" {
		return nil
	}
	lowerText := strings.ToLower(trimmed)
	label := normalizedHint
	reasons := make([]string, 0, 3)
	if normalizedHint != "" {
		reasons = append(reasons, "hint:"+normalizedHint)
	}
	keywordSets := map[string][]string{
		"happy":     {"开心", "高兴", "喜悦", "太棒了", "amazing", "great", "wonderful"},
		"sad":       {"难过", "伤心", "悲伤", "遗憾", "unfortunate", "sad"},
		"angry":     {"生气", "愤怒", "气死", "annoyed", "frustrated"},
		"surprised": {"惊讶", "惊喜", "没想到", "surprise", "wow"},
		"gentle":    {"温柔", "放松", "calm", "轻声"},
		"confident": {"坚定", "自信", "相信", "confident"},
	}
	for candidate, words := range keywordSets {
		for _, word := range words {
			if word == "" {
				continue
			}
			if strings.Contains(trimmed, word) || strings.Contains(lowerText, strings.ToLower(word)) {
				label = candidate
				reasons = append(reasons, "keyword:"+word)
				break
			}
		}
		if label == candidate {
			break
		}
	}
	if label == "" {
		label = "neutral"
	}
	intensity := 0.35
	confidence := 0.35
	exclamations := strings.Count(trimmed, "!")
	questionMarks := strings.Count(trimmed, "?")
	ellipsis := strings.Count(trimmed, "...") + strings.Count(trimmed, "…")
	if label == "happy" {
		intensity += float64(exclamations) * 0.08
	}
	if label == "sad" {
		intensity += float64(ellipsis) * 0.05
	}
	if label == "angry" {
		intensity += float64(exclamations) * 0.1
	}
	if label == "surprised" {
		intensity += float64(questionMarks) * 0.05
	}
	if strings.Contains(trimmed, "!!!") {
		intensity += 0.15
	}
	if len(reasons) > 0 {
		confidence += 0.2
	}
	intensity = clampFloat(intensity, 0.2, 1.0)
	confidence = clampFloat(confidence, 0.2, 0.95)
	if label == "neutral" {
		intensity = clampFloat(intensity, 0.2, 0.5)
	}
	reasonText := strings.Join(reasons, "; ")
	return &emotionMetadata{
		Label:           label,
		Intensity:       intensity,
		Confidence:      confidence,
		Reason:          reasonText,
		SuggestedMotion: motionForEmotion(label, intensity),
	}
}
