package agents

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type Module struct {
	db *gorm.DB
}

func RegisterRoutes(router *gin.Engine) (*Module, error) {
	db, err := openDatabaseFromEnv()
	if err != nil {
		return nil, err
	}

	if err := db.AutoMigrate(&Agent{}, &AgentChatConfig{}); err != nil {
		return nil, err
	}

	module := &Module{db: db}

	group := router.Group("/agents")
	group.GET("", module.handleListAgents)
	group.POST("", module.handleCreateAgent)
	group.GET("/:id", module.handleGetAgent)
	group.POST("/:id/conversations", module.handleCreateConversation)

	return module, nil
}

type createAgentRequest struct {
	Name           string   `json:"name" binding:"required"`
	Gender         string   `json:"gender"`
	TitleAddress   *string  `json:"title_address"`
	PersonaDesc    *string  `json:"persona_desc"`
	OpeningLine    *string  `json:"opening_line"`
	FirstTurnHint  *string  `json:"first_turn_hint"`
	Live2DModelID  *string  `json:"live2d_model_id"`
	LangDefault    string   `json:"lang_default"`
	Tags           []string `json:"tags"`
	Notes          *string  `json:"notes"`
	ModelProvider  string   `json:"model_provider" binding:"required"`
	ModelName      string   `json:"model_name" binding:"required"`
	ResponseFormat string   `json:"response_format"`
	Temperature    *float64 `json:"temperature"`
	MaxTokens      *int     `json:"max_tokens"`
	SystemPrompt   *string  `json:"system_prompt"`
}

func (m *Module) handleCreateAgent(c *gin.Context) {
	if m.db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database not initialized"})
		return
	}

	var req createAgentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request payload"})
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	gender := strings.ToLower(strings.TrimSpace(req.Gender))
	if gender == "" {
		gender = "neutral"
	}

	lang := strings.TrimSpace(req.LangDefault)
	if lang == "" {
		lang = "zh-CN"
	}

	agent := Agent{
		Name:        name,
		Gender:      gender,
		Status:      "active",
		LangDefault: lang,
		Version:     1,
	}

	agent.TitleAddress = normalizeStringPointer(req.TitleAddress)
	agent.PersonaDesc = normalizeStringPointer(req.PersonaDesc)
	agent.OpeningLine = normalizeStringPointer(req.OpeningLine)
	agent.FirstTurnHint = normalizeStringPointer(req.FirstTurnHint)
	agent.Live2DModelID = normalizeStringPointer(req.Live2DModelID)
	agent.Notes = normalizeStringPointer(req.Notes)

	if len(req.Tags) > 0 {
		if data, err := json.Marshal(req.Tags); err == nil {
			agent.Tags = datatypes.JSON(data)
		}
	}

	provider := strings.TrimSpace(req.ModelProvider)
	modelName := strings.TrimSpace(req.ModelName)
	if provider == "" || modelName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model_provider and model_name are required"})
		return
	}

	cfg := AgentChatConfig{
		ModelProvider:  provider,
		ModelName:      modelName,
		ResponseFormat: "text",
	}

	if rf := strings.TrimSpace(req.ResponseFormat); rf != "" {
		cfg.ResponseFormat = rf
	}

	cfg.SystemPrompt = normalizeStringPointer(req.SystemPrompt)

	params := map[string]any{}
	if req.Temperature != nil {
		params["temperature"] = *req.Temperature
	}
	if req.MaxTokens != nil {
		params["max_tokens"] = *req.MaxTokens
	}
	if len(params) > 0 {
		if data, err := json.Marshal(params); err == nil {
			cfg.ModelParams = datatypes.JSON(data)
		}
	}

	ctx := c.Request.Context()

	if err := m.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&agent).Error; err != nil {
			return err
		}
		cfg.AgentID = agent.ID
		if err := tx.Create(&cfg).Error; err != nil {
			return err
		}
		return nil
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create agent", "details": err.Error()})
		return
	}

	if err := m.db.WithContext(ctx).First(&cfg, "agent_id = ?", agent.ID).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusCreated, gin.H{"agent": agent})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"agent":       agent,
		"chat_config": cfg,
	})
}

func (m *Module) handleListAgents(c *gin.Context) {
	if m.db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database not initialized"})
		return
	}

	status := strings.TrimSpace(c.Query("status"))
	query := m.db.WithContext(c.Request.Context())
	if status != "" {
		query = query.Where("status = ?", status)
	} else {
		query = query.Where("status = ?", "active")
	}

	var agents []Agent
	if err := query.Order("updated_at DESC").Find(&agents).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list agents", "details": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"agents": agents})
}

func (m *Module) handleGetAgent(c *gin.Context) {
	if m.db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database not initialized"})
		return
	}

	id, err := strconv.ParseUint(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid agent id"})
		return
	}

	ctx := c.Request.Context()

	var agent Agent
	if err := m.db.WithContext(ctx).First(&agent, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "agent not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load agent", "details": err.Error()})
		}
		return
	}

	var cfg AgentChatConfig
	if err := m.db.WithContext(ctx).First(&cfg, "agent_id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusOK, gin.H{"agent": agent})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load agent config", "details": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"agent":       agent,
		"chat_config": cfg,
	})
}

type conversationInitRequest struct {
	UserID uint64 `json:"user_id" binding:"required"`
}

func (m *Module) handleCreateConversation(c *gin.Context) {
	if m.db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database not initialized"})
		return
	}

	agentID, err := strconv.ParseUint(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || agentID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid agent id"})
		return
	}

	var req conversationInitRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.UserID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_id is required"})
		return
	}

	ctx := c.Request.Context()

	var agent Agent
	if err := m.db.WithContext(ctx).First(&agent, "id = ?", agentID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "agent not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load agent", "details": err.Error()})
		}
		return
	}

	if strings.EqualFold(agent.Status, "archived") || strings.EqualFold(agent.Status, "paused") {
		c.JSON(http.StatusConflict, gin.H{"error": "agent is not active"})
		return
	}

	var convID uint64

	err = m.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing conversation
		if err := tx.Where("agent_id = ? AND user_id = ?", agentID, req.UserID).Take(&existing).Error; err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}

			now := time.Now().UTC()
			conv := conversation{
				AgentID:   agentID,
				UserID:    req.UserID,
				Status:    "active",
				StartedAt: now,
				LastMsgAt: now,
			}
			if err := tx.Create(&conv).Error; err != nil {
				return err
			}
			convID = conv.ID

			if opening := normalizeStringPointer(agent.OpeningLine); opening != nil {
				msg := message{
					ConversationID: conv.ID,
					Seq:            1,
					Role:           "assistant",
					Format:         "text",
					Content:        *opening,
				}
				if err := tx.Create(&msg).Error; err != nil {
					return err
				}
				if err := tx.Model(&conversation{}).Where("id = ?", conv.ID).Update("last_msg_at", msg.CreatedAt).Error; err != nil {
					return err
				}
			}
		} else {
			convID = existing.ID
		}
		return nil
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to init conversation", "details": err.Error()})
		return
	}

	var conv conversation
	if err := m.db.WithContext(ctx).First(&conv, "id = ?", convID).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load conversation", "details": err.Error()})
		return
	}

	var messages []messageRecord
	if err := m.db.WithContext(ctx).
		Table("messages").
		Select("messages.id, messages.conversation_id, conversations.agent_id, conversations.user_id, messages.seq, messages.role, messages.format, messages.content, messages.parent_msg_id, messages.latency_ms, messages.token_input, messages.token_output, messages.err_code, messages.err_msg, messages.created_at").
		Joins("JOIN conversations ON conversations.id = messages.conversation_id").
		Where("messages.conversation_id = ?", conv.ID).
		Order("messages.created_at ASC, messages.id ASC").
		Limit(50).
		Scan(&messages).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load messages", "details": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"agent": agent,
		"conversation": gin.H{
			"id":          conv.ID,
			"agent_id":    conv.AgentID,
			"user_id":     conv.UserID,
			"status":      conv.Status,
			"started_at":  conv.StartedAt,
			"last_msg_at": conv.LastMsgAt,
			"created_at":  conv.CreatedAt,
			"updated_at":  conv.UpdatedAt,
		},
		"messages": messages,
	})
}

type conversation struct {
	ID        uint64    `gorm:"primaryKey"`
	AgentID   uint64    `gorm:"column:agent_id"`
	UserID    uint64    `gorm:"column:user_id"`
	Status    string    `gorm:"column:status"`
	LastMsgAt time.Time `gorm:"column:last_msg_at"`
	StartedAt time.Time `gorm:"column:started_at"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (conversation) TableName() string {
	return "conversations"
}

type message struct {
	ID              uint64    `gorm:"primaryKey"`
	ConversationID  uint64    `gorm:"column:conversation_id"`
	Seq             int       `gorm:"column:seq"`
	Role            string    `gorm:"column:role"`
	Format          string    `gorm:"column:format"`
	Content         string    `gorm:"column:content"`
	ParentMessageID *uint64   `gorm:"column:parent_msg_id"`
	LatencyMs       *int      `gorm:"column:latency_ms"`
	TokenInput      *int      `gorm:"column:token_input"`
	TokenOutput     *int      `gorm:"column:token_output"`
	ErrCode         *string   `gorm:"column:err_code"`
	ErrMsg          *string   `gorm:"column:err_msg"`
	CreatedAt       time.Time `gorm:"column:created_at"`
}

func (message) TableName() string {
	return "messages"
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
	ParentMessageID *uint64   `json:"parent_message_id,omitempty"`
	LatencyMs       *int      `json:"latency_ms,omitempty"`
	TokenInput      *int      `json:"token_input,omitempty"`
	TokenOutput     *int      `json:"token_output,omitempty"`
	ErrCode         *string   `json:"err_code,omitempty"`
	ErrMsg          *string   `json:"err_msg,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

func normalizeStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	copy := trimmed
	return &copy
}
