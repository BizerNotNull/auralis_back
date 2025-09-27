package llm

import (
	"auralis_back/agents"
	cache "auralis_back/cache"
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
	"github.com/redis/go-redis/v9"
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
	client       *ChatClient
	db           *gorm.DB
	tts          tts.Synthesizer
	memory       *conversationMemory
	modelCatalog []ChatModelOption
	messageCache *messageCache
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

	if err := db.AutoMigrate(&conversation{}, &message{}, &userAgentMemory{}); err != nil {
		return nil, err
	}

	var msgCache *messageCache
	if cacheClient, err := cache.GetRedisClient(); err != nil {
		log.Printf("llm: redis disabled for recent message cache: %v", err)
	} else {
		msgCache = newMessageCache(cacheClient)
	}

	module := &Module{
		client:       client,
		db:           db,
		tts:          synthesizer,
		memory:       newConversationMemory(db, client),
		modelCatalog: loadChatModelCatalog(),
		messageCache: msgCache,
	}

	group := router.Group("/llm")
	group.GET("/models", module.handleListModels)
	group.POST("/complete", module.handleComplete)
	group.GET("/messages", module.handleRecentMessages)
	group.GET("/messages/:id/speech", module.handleMessageSpeech)
	group.GET("/messages/:id/speech/audio", module.handleMessageSpeechAudio)
	group.POST("/messages", module.handleCreateMessage)

	return module, nil
}

// handleListModels godoc
// @Summary 查询聊天模型
// @Description 返回当前可用的聊天模型选项列表
// @Tags LLM
// @Produce json
// @Param provider query string false "按提供方过滤"
// @Success 200 {object} map[string]interface{} "模型列表"
// @Author bizer
// @Router /llm/models [get]
func (m *Module) handleListModels(c *gin.Context) {
	catalog := m.modelCatalog
	if len(catalog) == 0 {
		catalog = loadChatModelCatalog()
		m.modelCatalog = catalog
	}

	providerFilter := strings.TrimSpace(c.Query("provider"))
	result := make([]ChatModelOption, 0, len(catalog))
	for _, option := range catalog {
		if providerFilter != "" && !strings.EqualFold(option.Provider, providerFilter) {
			continue
		}
		result = append(result, option)
	}

	c.JSON(http.StatusOK, gin.H{"models": result})
}

type completeRequest struct {
	Prompt string `json:"prompt" binding:"required"`
}

type completeResponse struct {
	Prompt  string     `json:"prompt"`
	Content string     `json:"content"`
	Usage   *ChatUsage `json:"usage,omitempty"`
}

// handleComplete godoc
// @Summary 文本补全
// @Description 使用默认模型对提示语进行补全生成
// @Tags LLM
// @Accept json
// @Produce json
// @Param request body completeRequest true "补全请求"
// @Success 200 {object} completeResponse "补全结果"
// @Failure 400 {object} map[string]string "请求参数错误"
// @Failure 502 {object} map[string]string "上游服务错误"
// @Author bizer
// @Router /llm/complete [post]
func (m *Module) handleComplete(c *gin.Context) {
	var req completeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "prompt is required"})
		return
	}

	result, err := m.client.Complete(c.Request.Context(), req.Prompt)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, completeResponse{
		Prompt:  req.Prompt,
		Content: result.Content,
		Usage:   result.Usage,
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

// handleRecentMessages godoc
// @Summary 查询最近消息
// @Description 获取指定用户与智能体的最近对话消息
// @Tags LLM
// @Produce json
// @Param agent_id query int true "智能体ID"
// @Param user_id query int true "用户ID"
// @Success 200 {object} map[string]interface{} "消息列表"
// @Failure 400 {object} map[string]string "请求参数错误"
// @Failure 500 {object} map[string]string "服务器错误"
// @Author bizer
// @Router /llm/messages [get]
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

	ctx := c.Request.Context()
	if m.messageCache != nil {
		if cached, cacheErr := m.messageCache.get(ctx, agentID, userID); cacheErr == nil {
			c.JSON(http.StatusOK, gin.H{
				"agent_id": agentID,
				"user_id":  userID,
				"messages": cached,
			})
			return
		} else if cacheErr != nil && !errors.Is(cacheErr, redis.Nil) {
			log.Printf("llm: recent messages cache fetch failed: %v", cacheErr)
		}
	}

	var records []messageRecord
	tx := m.db.WithContext(ctx).
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

	if m.messageCache != nil {
		m.messageCache.store(ctx, agentID, userID, records)
	}

	c.JSON(http.StatusOK, gin.H{
		"agent_id": agentID,
		"user_id":  userID,
		"messages": records,
	})
}

func (m *Module) invalidateRecentMessagesCache(ctx context.Context, agentID, userID uint64) {
	if m == nil || m.messageCache == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.messageCache.invalidate(ctx, agentID, userID)
}

type messageSpeechRecord struct {
	MessageID      uint64
	ConversationID uint64
	Extras         map[string]any
}

func normalizeString(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return strings.TrimSpace(v.String())
	case fmt.Stringer:
		return strings.TrimSpace(v.String())
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func (m *Module) loadMessageSpeechRecord(ctx context.Context, messageID, agentID, userID uint64) (*messageSpeechRecord, error) {
	var row struct {
		ID             uint64
		ConversationID uint64
		Extras         datatypes.JSON
	}

	err := m.db.WithContext(ctx).
		Table("messages").
		Select("messages.id, messages.conversation_id, messages.extras").
		Joins("JOIN conversations ON conversations.id = messages.conversation_id").
		Where("messages.id = ? AND conversations.agent_id = ? AND conversations.user_id = ?", messageID, agentID, userID).
		Take(&row).Error
	if err != nil {
		return nil, err
	}

	extraMap := make(map[string]any)
	if len(row.Extras) > 0 {
		if err := json.Unmarshal(row.Extras, &extraMap); err != nil {
			log.Printf("llm: parse extras for message %d failed: %v", row.ID, err)
			extraMap = make(map[string]any)
		}
	}

	return &messageSpeechRecord{
		MessageID:      row.ID,
		ConversationID: row.ConversationID,
		Extras:         extraMap,
	}, nil
}

func (m *Module) ensureSpeechAudioURL(speech map[string]any) map[string]any {
	if speech == nil {
		return nil
	}
	trimmed := normalizeString(speech["audio_url"])
	trimmedAlt := normalizeString(speech["audioUrl"])
	if trimmed == "" && trimmedAlt == "" {
		return speech
	}
	clone := make(map[string]any, len(speech))
	for k, v := range speech {
		if k == "audio_url" || k == "audioUrl" {
			continue
		}
		clone[k] = v
	}
	return clone
}

// handleMessageSpeech godoc
// @Summary 查询语音生成结果
// @Description 获取指定消息的语音生成状态与元数据
// @Tags LLM
// @Produce json
// @Param id path int true "消息ID"
// @Param agent_id query int true "智能体ID"
// @Param user_id query int true "用户ID"
// @Success 200 {object} map[string]interface{} "语音信息"
// @Failure 400 {object} map[string]string "请求参数错误"
// @Failure 404 {object} map[string]string "未找到"
// @Failure 500 {object} map[string]string "服务器错误"
// @Author bizer
// @Router /llm/messages/{id}/speech [get]
func (m *Module) handleMessageSpeech(c *gin.Context) {
	if m.db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database not initialized"})
		return
	}

	messageIDStr := strings.TrimSpace(c.Param("id"))
	if messageIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "message id is required"})
		return
	}

	messageID, err := strconv.ParseUint(messageIDStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid message id"})
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

	record, err := m.loadMessageSpeechRecord(c.Request.Context(), messageID, agentID, userID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "message not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load message", "details": err.Error()})
		return
	}

	extraMap := record.Extras
	status := normalizeString(extraMap["speech_status"])
	if status == "" {
		status = normalizeString(extraMap["speechStatus"])
	}

	var speechPayload any
	if raw, ok := extraMap["speech"]; ok {
		if speechMap, ok := raw.(map[string]any); ok {
			speechPayload = m.ensureSpeechAudioURL(speechMap)
		} else {
			speechPayload = raw
		}
	}

	speechError := normalizeString(extraMap["speech_error"])
	if speechError == "" {
		speechError = normalizeString(extraMap["speechError"])
	}

	response := gin.H{
		"message_id":      record.MessageID,
		"conversation_id": record.ConversationID,
		"speech_status":   status,
		"speech":          speechPayload,
	}
	if speechError != "" {
		response["speech_error"] = speechError
	}

	c.JSON(http.StatusOK, response)
}

// handleMessageSpeechAudio godoc
// @Summary 下载语音音频
// @Description 当前环境未开放语音音频下载功能
// @Tags LLM
// @Produce application/json
// @Param id path int true "消息ID"
// @Failure 404 {object} map[string]string "功能未启用"
// @Author bizer
// @Router /llm/messages/{id}/speech/audio [get]
func (m *Module) handleMessageSpeechAudio(c *gin.Context) {
	c.JSON(http.StatusNotFound, gin.H{"error": "speech streaming disabled"})
}

type createMessageRequest struct {
	AgentID       string   `json:"agent_id" binding:"required"`
	UserID        string   `json:"user_id" binding:"required"`
	Role          string   `json:"role" binding:"required"`
	Content       string   `json:"content" binding:"required"`
	VoiceID       string   `json:"voice_id"`
	VoiceProvider string   `json:"voice_provider"`
	EmotionHint   string   `json:"emotion_hint"`
	SpeechSpeed   *float64 `json:"speech_speed,omitempty"`
	SpeechPitch   *float64 `json:"speech_pitch,omitempty"`
}

type speechPreferences struct {
	VoiceID     string
	Provider    string
	EmotionHint string
	Speed       float64
	Pitch       float64
}

type voiceSelection struct {
	ID       string
	Provider string
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
	TokensUsed       *int           `json:"tokens_used,omitempty"`
	TokenBalance     *int64         `json:"token_balance,omitempty"`
}

// handleCreateMessage godoc
// @Summary 发送对话消息
// @Description 创建用户或助手消息并按需触发回复生成
// @Tags LLM
// @Accept json
// @Produce json
// @Param request body createMessageRequest true "消息内容"
// @Success 200 {object} createMessageResponse "消息创建结果"
// @Failure 400 {object} map[string]string "请求参数错误"
// @Failure 402 {object} map[string]string "余额不足"
// @Failure 404 {object} map[string]string "未找到"
// @Failure 500 {object} map[string]string "服务器错误"
// @Author bizer
// @Router /llm/messages [post]
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
		Provider:    strings.TrimSpace(req.VoiceProvider),
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

	startingBalance := int64(-1)
	if role == "user" {
		balance, err := m.getUserTokenBalance(ctx, userID)
		if err != nil {
			switch {
			case errors.Is(err, gorm.ErrRecordNotFound):
				c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			default:
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load token balance"})
			}
			return
		}
		if balance <= 0 {
			c.JSON(http.StatusPaymentRequired, gin.H{"error": "insufficient token balance"})
			return
		}
		startingBalance = balance
	}

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

	if m.memory != nil {
		if prefErr := m.memory.upsertSpeechPreferences(ctx, agentID, userID, prefs); prefErr != nil {
			log.Printf("llm: persist speech preferences: %v", prefErr)
		}
	}

	var conv conversation
	if err := m.db.WithContext(ctx).First(&conv, "id = ?", convID).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load conversation", "details": err.Error()})
		return
	}

	m.invalidateRecentMessagesCache(ctx, conv.AgentID, conv.UserID)

	response := createMessageResponse{
		ConversationID: conv.ID,
		AgentID:        conv.AgentID,
		UserID:         conv.UserID,
		UserMessage:    userRecord,
	}

	remainingBalance := startingBalance
	var tokensUsedTotal int64

	if role == "user" && wantsEventStream(c) {
		m.handleCreateMessageStream(c, conv, userMsg, userRecord, prefs, startingBalance)
		return
	}

	if role == "user" {
		assistantRecord, usage, genErr := m.generateAssistantReply(ctx, conv, userMsg, prefs)
		if genErr != nil {
			response.AssistantError = genErr.Error()
		} else if assistantRecord != nil {
			response.AssistantMessage = assistantRecord
		}
		if usage != nil {
			tokensUsedTotal = totalTokensUsed(usage)
			updatedBalance, err := m.applyUsageToUserTokens(ctx, conv.UserID, usage, startingBalance)
			if err != nil {
				log.Printf("llm: failed to apply token usage: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to finalize token usage"})
				return
			}
			remainingBalance = updatedBalance
		}
	}

	if startingBalance >= 0 {
		if remainingBalance < 0 {
			remainingBalance = 0
		}
		response.TokenBalance = int64Pointer(remainingBalance)
	}
	if tokensUsedTotal > 0 {
		if ptr := intPointerIfPositive(int(tokensUsedTotal)); ptr != nil {
			response.TokensUsed = ptr
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

func (m *Module) generateAssistantReply(ctx context.Context, conv conversation, userMsg message, prefs speechPreferences) (*messageRecord, *ChatUsage, error) {
	if m.client == nil {
		return nil, nil, errors.New("llm client not configured")
	}

	contextData, err := m.buildConversationContext(ctx, conv)
	if err != nil {
		return nil, nil, err
	}

	applyPreferenceDefaults(&prefs, contextData)

	start := time.Now()
	result, err := m.client.Chat(ctx, contextData.messages)
	if err != nil {
		short := truncateString(err.Error(), 256)
		_ = m.db.WithContext(ctx).Model(&message{}).Where("id = ?", userMsg.ID).Updates(map[string]any{
			"err_code": "llm_error",
			"err_msg":  short,
		})
		return nil, nil, err
	}
	reply := result.Content
	usage := result.Usage

	latency := int(time.Since(start).Milliseconds())
	parentID := userMsg.ID

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

	if usage != nil {
		if ptr := intPointerIfPositive(usage.PromptTokens); ptr != nil {
			assistant.TokenInput = ptr
		}
		if ptr := intPointerIfPositive(usage.CompletionTokens); ptr != nil {
			assistant.TokenOutput = ptr
		}
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
		return nil, nil, err
	}

	if err := m.db.WithContext(ctx).First(&assistant, "id = ?", assistant.ID).Error; err != nil {
		return nil, nil, err
	}

	if usage != nil {
		m.incrementConversationTokens(ctx, conv.ID, usage)
	}

	record := messageToRecord(assistant, conv)

	if speechEnabled {
		m.enqueueSpeechSynthesis(assistant.ID, conv, reply, selection, speed, pitch, emotionMeta)
	}

	if m.memory != nil {
		if summary, err := m.memory.ensureSummary(ctx, conv); err != nil {
			log.Printf("llm: update conversation summary: %v", err)
		} else if summary != "" {
			conv.Summary = &summary
		}
	}

	return &record, usage, nil
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

func applyPreferenceDefaults(prefs *speechPreferences, ctxData *conversationContext) {
	if prefs == nil || ctxData == nil {
		return
	}

	profile := ctxData.profile
	if profile != nil {
		if prefs.VoiceID == "" {
			if voice := getPreferenceString(profile.Preferences, "voice_id"); voice != "" {
				prefs.VoiceID = voice
			}
		}
		if prefs.Provider == "" {
			if provider := getPreferenceString(profile.Preferences, "voice_provider"); provider != "" {
				prefs.Provider = provider
			}
		}
		if prefs.EmotionHint == "" {
			if hint := getPreferenceString(profile.Preferences, "emotion_hint"); hint != "" {
				prefs.EmotionHint = hint
			}
		}
		if prefs.Speed == 1.0 {
			if speed, ok := getPreferenceFloat(profile.Preferences, "speech_speed"); ok && speed > 0 {
				prefs.Speed = speed
			}
		}
		if prefs.Pitch == 1.0 {
			if pitch, ok := getPreferenceFloat(profile.Preferences, "speech_pitch"); ok && pitch > 0 {
				prefs.Pitch = pitch
			}
		}
	}

	if prefs.Provider == "" && ctxData.agent.VoiceProvider != nil {
		if provider := strings.TrimSpace(*ctxData.agent.VoiceProvider); provider != "" {
			prefs.Provider = provider
		}
	}
	if prefs.VoiceID == "" && ctxData.agent.VoiceID != nil {
		if voice := strings.TrimSpace(*ctxData.agent.VoiceID); voice != "" {
			prefs.VoiceID = voice
		}
	}
}

func getPreferenceString(prefs map[string]any, key string) string {
	if prefs == nil {
		return ""
	}
	value, ok := prefs[key]
	if !ok {
		return ""
	}
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case fmt.Stringer:
		return strings.TrimSpace(v.String())
	case json.Number:
		return strings.TrimSpace(v.String())
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func getPreferenceFloat(prefs map[string]any, key string) (float64, bool) {
	if prefs == nil {
		return 0, false
	}
	value, ok := prefs[key]
	if !ok {
		return 0, false
	}
	switch v := value.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int32:
		return float64(v), true
	case int64:
		return float64(v), true
	case uint:
		return float64(v), true
	case uint32:
		return float64(v), true
	case uint64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		if err == nil {
			return f, true
		}
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(trimmed, 64)
		if err == nil {
			return f, true
		}
	default:
		trimmed := strings.TrimSpace(fmt.Sprint(v))
		if trimmed == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(trimmed, 64)
		if err == nil {
			return f, true
		}
	}
	return 0, false
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

func resolveVoiceSelection(candidateID string, candidateProvider string, synth tts.Synthesizer) voiceSelection {
	selection := voiceSelection{
		ID:       strings.TrimSpace(candidateID),
		Provider: normalizeVoiceProvider(candidateProvider),
	}
	if synth == nil {
		return selection
	}

	voices := synth.Voices()
	if selection.ID != "" {
		if selection.Provider == "" {
			selection.Provider = normalizeVoiceProvider(providerForVoiceOption(voices, selection.ID))
		}
		if selection.Provider != "" {
			return selection
		}
	}

	if selection.ID == "" {
		selection.ID = strings.TrimSpace(synth.DefaultVoiceID())
	}
	if selection.Provider == "" && selection.ID != "" {
		selection.Provider = normalizeVoiceProvider(providerForVoiceOption(voices, selection.ID))
	}
	if selection.ID == "" && len(voices) > 0 {
		selection.ID = strings.TrimSpace(voices[0].ID)
		selection.Provider = normalizeVoiceProvider(voices[0].Provider)
	} else if selection.Provider == "" && len(voices) > 0 {
		selection.Provider = normalizeVoiceProvider(voices[0].Provider)
	}

	return selection
}

func providerForVoiceOption(options []tts.VoiceOption, id string) string {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return ""
	}
	for _, option := range options {
		if strings.EqualFold(option.ID, trimmed) {
			return option.Provider
		}
	}
	return ""
}

func normalizeVoiceProvider(value string) string {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	switch trimmed {
	case "", "qiniu", "qiniu-openai", "qiniu_openai", "qiniuopenai":
		return "qiniu-openai"
	case "aliyun", "ali", "aliyun-cosyvoice", "aliyun_cosyvoice", "cosyvoice", "cosy-voice":
		return "aliyun-cosyvoice"
	default:
		return trimmed
	}
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
