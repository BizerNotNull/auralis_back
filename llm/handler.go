package llm

import (
	"auralis_back/agents"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
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
}

// RegisterRoutes mounts the LLM testing endpoint under /llm.
func RegisterRoutes(router *gin.Engine) (*Module, error) {
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

	module := &Module{client: client, db: db}

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
	ID              uint64    `json:"id"`
	ConversationID  uint64    `json:"conversation_id"`
	AgentID         uint64    `json:"agent_id"`
	UserID          uint64    `json:"user_id"`
	Seq             int       `json:"seq"`
	Role            string    `json:"role"`
	Format          string    `json:"format"`
	Content         string    `json:"content"`
	ParentMessageID *uint64   `json:"parent_message_id,omitempty" gorm:"column:parent_msg_id"`
	LatencyMs       *int      `json:"latency_ms,omitempty"`
	TokenInput      *int      `json:"token_input,omitempty"`
	TokenOutput     *int      `json:"token_output,omitempty"`
	ErrCode         *string   `json:"err_code,omitempty"`
	ErrMsg          *string   `json:"err_msg,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
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
		Select("messages.id, messages.conversation_id, conversations.agent_id, conversations.user_id, messages.seq, messages.role, messages.format, messages.content, messages.parent_msg_id, messages.latency_ms, messages.token_input, messages.token_output, messages.err_code, messages.err_msg, messages.created_at").
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
	AgentID uint64 `json:"agent_id" binding:"required"`
	UserID  uint64 `json:"user_id" binding:"required"`
	Role    string `json:"role" binding:"required"`
	Content string `json:"content" binding:"required"`
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

	if req.AgentID == 0 || req.UserID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "agent_id and user_id must be positive"})
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

	ctx := c.Request.Context()

	var convID uint64
	var userMsg message
	var userRecord messageRecord

	err := m.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()

		var conv conversation
		err := tx.Where("agent_id = ? AND user_id = ?", req.AgentID, req.UserID).Take(&conv).Error
		if err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}

			conv = conversation{
				AgentID:   req.AgentID,
				UserID:    req.UserID,
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

	if role == "user" {
		assistantRecord, genErr := m.generateAssistantReply(ctx, conv, userMsg)
		if genErr != nil {
			response.AssistantError = genErr.Error()
		} else if assistantRecord != nil {
			response.AssistantMessage = assistantRecord
		}
	}

	c.JSON(http.StatusCreated, response)
}

func (m *Module) generateAssistantReply(ctx context.Context, conv conversation, userMsg message) (*messageRecord, error) {
	if m.client == nil {
		return nil, errors.New("llm client not configured")
	}

	var agentModel agents.Agent
	if err := m.db.WithContext(ctx).First(&agentModel, "id = ?", conv.AgentID).Error; err != nil {
		return nil, fmt.Errorf("load agent: %w", err)
	}

	var cfg agents.AgentChatConfig
	cfgErr := m.db.WithContext(ctx).First(&cfg, "agent_id = ?", conv.AgentID).Error
	if cfgErr != nil && !errors.Is(cfgErr, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("load agent config: %w", cfgErr)
	}

	historyLimit := 20
	var history []message
	if err := m.db.WithContext(ctx).
		Where("conversation_id = ?", conv.ID).
		Order("seq ASC").
		Limit(historyLimit).
		Find(&history).Error; err != nil {
		return nil, fmt.Errorf("load history: %w", err)
	}

	systemPrompt := buildSystemPrompt(&agentModel, func() *agents.AgentChatConfig {
		if cfgErr == nil {
			return &cfg
		}
		return nil
	}())

	messages := make([]ChatMessage, 0, len(history)+1)
	if systemPrompt != "" {
		messages = append(messages, ChatMessage{Role: "system", Content: systemPrompt})
	}

	for _, item := range history {
		role := strings.ToLower(strings.TrimSpace(item.Role))
		if role != "user" && role != "assistant" && role != "system" {
			continue
		}
		messages = append(messages, ChatMessage{Role: role, Content: item.Content})
	}

	start := time.Now()
	reply, err := m.client.Chat(ctx, messages)
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

	record := messageRecord{
		ID:              assistant.ID,
		ConversationID:  assistant.ConversationID,
		AgentID:         conv.AgentID,
		UserID:          conv.UserID,
		Seq:             assistant.Seq,
		Role:            assistant.Role,
		Format:          assistant.Format,
		Content:         assistant.Content,
		ParentMessageID: assistant.ParentMessageID,
		LatencyMs:       assistant.LatencyMs,
		TokenInput:      assistant.TokenInput,
		TokenOutput:     assistant.TokenOutput,
		ErrCode:         assistant.ErrCode,
		ErrMsg:          assistant.ErrMsg,
		CreatedAt:       assistant.CreatedAt,
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
