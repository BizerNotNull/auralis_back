package agents

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	jwt "github.com/appleboy/gin-jwt/v2"
	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type Module struct {
	db             *gorm.DB
	avatars        *avatarStorage
	authMiddleware gin.HandlerFunc
}

func RegisterRoutes(router *gin.Engine, authMiddleware gin.HandlerFunc) (*Module, error) {
	db, err := openDatabaseFromEnv()
	if err != nil {
		return nil, err
	}

	if err := db.AutoMigrate(&Agent{}, &AgentChatConfig{}); err != nil {
		return nil, err
	}

	storage, err := newAvatarStorageFromEnv()
	if err != nil {
		return nil, err
	}

	module := &Module{db: db, avatars: storage, authMiddleware: authMiddleware}

	group := router.Group("/agents")
	group.GET("", module.handleListAgents)
	group.POST("", module.handleCreateAgent)
	group.GET("/:id", module.handleGetAgent)
	group.POST("/:id/conversations", module.handleCreateConversation)
	group.DELETE("/:id/conversations", module.handleClearConversation)

	adminGroup := group.Group("")
	if authMiddleware != nil {
		adminGroup.Use(authMiddleware)
	}
	adminGroup.Use(requireRole("admin"))
	adminGroup.PUT("/:id", module.handleUpdateAgent)

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

type updateAgentRequest struct {
	Name           *string   `json:"name"`
	Gender         *string   `json:"gender"`
	TitleAddress   *string   `json:"title_address"`
	PersonaDesc    *string   `json:"persona_desc"`
	OpeningLine    *string   `json:"opening_line"`
	FirstTurnHint  *string   `json:"first_turn_hint"`
	Live2DModelID  *string   `json:"live2d_model_id"`
	LangDefault    *string   `json:"lang_default"`
	Tags           *[]string `json:"tags"`
	Notes          *string   `json:"notes"`
	ModelProvider  *string   `json:"model_provider"`
	ModelName      *string   `json:"model_name"`
	ResponseFormat *string   `json:"response_format"`
	Temperature    *float64  `json:"temperature"`
	MaxTokens      *int      `json:"max_tokens"`
	SystemPrompt   *string   `json:"system_prompt"`
	Status         *string   `json:"status"`
	RemoveAvatar   *bool     `json:"remove_avatar"`
}

func (m *Module) handleCreateAgent(c *gin.Context) {
	if m.db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database not initialized"})
		return
	}

	req, avatarFile, err := bindCreateAgentRequest(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
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

	if avatarFile != nil {
		if m.avatars == nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "avatar storage not configured"})
			return
		}
		avatarURL, uploadErr := m.avatars.Upload(ctx, agent.ID, avatarFile)
		if uploadErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to upload avatar", "details": uploadErr.Error()})
			return
		}
		if err := m.db.WithContext(ctx).Model(&Agent{}).Where("id = ?", agent.ID).Update("avatar_url", avatarURL).Error; err != nil {
			_ = m.avatars.Remove(ctx, avatarURL)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to persist avatar", "details": err.Error()})
			return
		}
	}

	if err := m.db.WithContext(ctx).First(&agent, "id = ?", agent.ID).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load agent", "details": err.Error()})
		return
	}

	if err := m.db.WithContext(ctx).First(&cfg, "agent_id = ?", agent.ID).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load agent config", "details": err.Error()})
		return
	}

	response := gin.H{"agent": agent}
	if cfg.AgentID != 0 {
		response["chat_config"] = cfg
	}

	c.JSON(http.StatusCreated, response)
}

func (m *Module) handleUpdateAgent(c *gin.Context) {
	if m.db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database not initialized"})
		return
	}

	agentID, err := strconv.ParseUint(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || agentID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid agent id"})
		return
	}

	req, avatarFile, err := bindUpdateAgentRequest(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
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

	oldAvatar := ""
	if agent.AvatarURL != nil {
		oldAvatar = strings.TrimSpace(*agent.AvatarURL)
	}

	var newAvatarURL string
	if avatarFile != nil {
		if m.avatars == nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "avatar storage not configured"})
			return
		}
		uploaded, uploadErr := m.avatars.Upload(ctx, agentID, avatarFile)
		if uploadErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to upload avatar", "details": uploadErr.Error()})
			return
		}
		newAvatarURL = uploaded
	}

	removeAvatar := req.RemoveAvatar != nil && *req.RemoveAvatar

	agentUpdates := make(map[string]interface{})

	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			if newAvatarURL != "" {
				_ = m.avatars.Remove(ctx, newAvatarURL)
			}
			c.JSON(http.StatusBadRequest, gin.H{"error": "name cannot be empty"})
			return
		}
		agentUpdates["name"] = name
	}

	if req.Gender != nil {
		gender := strings.ToLower(strings.TrimSpace(*req.Gender))
		if gender == "" {
			gender = "neutral"
		}
		agentUpdates["gender"] = gender
	}

	if req.TitleAddress != nil {
		agentUpdates["title_address"] = normalizeStringPointer(req.TitleAddress)
	}
	if req.PersonaDesc != nil {
		agentUpdates["persona_desc"] = normalizeStringPointer(req.PersonaDesc)
	}
	if req.OpeningLine != nil {
		agentUpdates["opening_line"] = normalizeStringPointer(req.OpeningLine)
	}
	if req.FirstTurnHint != nil {
		agentUpdates["first_turn_hint"] = normalizeStringPointer(req.FirstTurnHint)
	}
	if req.Live2DModelID != nil {
		agentUpdates["live2d_model_id"] = normalizeStringPointer(req.Live2DModelID)
	}
	if req.Notes != nil {
		agentUpdates["notes"] = normalizeStringPointer(req.Notes)
	}
	if req.LangDefault != nil {
		lang := strings.TrimSpace(*req.LangDefault)
		if lang == "" {
			lang = "zh-CN"
		}
		agentUpdates["lang_default"] = lang
	}
	if req.Tags != nil {
		tags := normalizeTags(*req.Tags)
		if len(tags) == 0 {
			agentUpdates["tags"] = datatypes.JSON([]byte("[]"))
		} else if data, marshalErr := json.Marshal(tags); marshalErr == nil {
			agentUpdates["tags"] = datatypes.JSON(data)
		} else {
			if newAvatarURL != "" {
				_ = m.avatars.Remove(ctx, newAvatarURL)
			}
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tags payload"})
			return
		}
	}

	if req.Status != nil {
		status := strings.ToLower(strings.TrimSpace(*req.Status))
		allowedStatus := map[string]struct{}{"draft": {}, "active": {}, "paused": {}, "archived": {}}
		if _, ok := allowedStatus[status]; !ok {
			if newAvatarURL != "" {
				_ = m.avatars.Remove(ctx, newAvatarURL)
			}
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status value"})
			return
		}
		agentUpdates["status"] = status
	}

	if newAvatarURL != "" {
		agentUpdates["avatar_url"] = newAvatarURL
	} else if removeAvatar {
		agentUpdates["avatar_url"] = gorm.Expr("NULL")
	}

	var cfg AgentChatConfig
	if err := m.db.WithContext(ctx).First(&cfg, "agent_id = ?", agentID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			cfg = AgentChatConfig{AgentID: agentID}
		} else {
			if newAvatarURL != "" {
				_ = m.avatars.Remove(ctx, newAvatarURL)
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load agent config", "details": err.Error()})
			return
		}
	}

	cfgChanged := false

	if req.ModelProvider != nil {
		provider := strings.TrimSpace(*req.ModelProvider)
		if provider == "" {
			if newAvatarURL != "" {
				_ = m.avatars.Remove(ctx, newAvatarURL)
			}
			c.JSON(http.StatusBadRequest, gin.H{"error": "model_provider cannot be empty"})
			return
		}
		cfg.ModelProvider = provider
		cfgChanged = true
	}
	if req.ModelName != nil {
		modelName := strings.TrimSpace(*req.ModelName)
		if modelName == "" {
			if newAvatarURL != "" {
				_ = m.avatars.Remove(ctx, newAvatarURL)
			}
			c.JSON(http.StatusBadRequest, gin.H{"error": "model_name cannot be empty"})
			return
		}
		cfg.ModelName = modelName
		cfgChanged = true
	}
	if req.ResponseFormat != nil {
		format := strings.TrimSpace(*req.ResponseFormat)
		if format == "" {
			format = "text"
		}
		cfg.ResponseFormat = format
		cfgChanged = true
	}
	if req.SystemPrompt != nil {
		cfg.SystemPrompt = normalizeStringPointer(req.SystemPrompt)
		cfgChanged = true
	}

	params := map[string]interface{}{}
	if len(cfg.ModelParams) > 0 {
		_ = json.Unmarshal(cfg.ModelParams, &params)
	}
	paramsModified := false
	if req.Temperature != nil {
		params["temperature"] = *req.Temperature
		paramsModified = true
	}
	if req.MaxTokens != nil {
		if *req.MaxTokens <= 0 {
			delete(params, "max_tokens")
		} else {
			params["max_tokens"] = *req.MaxTokens
		}
		paramsModified = true
	}

	if paramsModified {
		if len(params) == 0 {
			cfg.ModelParams = nil
		} else if data, marshalErr := json.Marshal(params); marshalErr == nil {
			cfg.ModelParams = datatypes.JSON(data)
		} else {
			if newAvatarURL != "" {
				_ = m.avatars.Remove(ctx, newAvatarURL)
			}
			c.JSON(http.StatusBadRequest, gin.H{"error": "failed to encode model params"})
			return
		}
		cfgChanged = true
	}

	err = m.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if len(agentUpdates) > 0 {
			if err := tx.Model(&Agent{}).Where("id = ?", agentID).Updates(agentUpdates).Error; err != nil {
				return err
			}
		}

		if cfgChanged {
			if cfg.AgentID == 0 {
				cfg.AgentID = agentID
			}
			if err := tx.Save(&cfg).Error; err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		if newAvatarURL != "" {
			_ = m.avatars.Remove(ctx, newAvatarURL)
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update agent", "details": err.Error()})
		return
	}

	if newAvatarURL != "" && oldAvatar != "" && oldAvatar != newAvatarURL {
		_ = m.avatars.Remove(ctx, oldAvatar)
	} else if removeAvatar && oldAvatar != "" && newAvatarURL == "" {
		_ = m.avatars.Remove(ctx, oldAvatar)
	}

	if err := m.db.WithContext(ctx).First(&agent, "id = ?", agentID).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load agent", "details": err.Error()})
		return
	}

	var updatedCfg AgentChatConfig
	if err := m.db.WithContext(ctx).First(&updatedCfg, "agent_id = ?", agentID).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load agent config", "details": err.Error()})
		return
	}

	response := gin.H{"agent": agent}
	if updatedCfg.AgentID != 0 {
		response["chat_config"] = updatedCfg
	}

	c.JSON(http.StatusOK, response)
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

type conversationClearRequest struct {
	UserID uint64 `json:"user_id" binding:"required"`
}

func (m *Module) handleClearConversation(c *gin.Context) {
	if m.db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database not initialized"})
		return
	}

	agentID, err := strconv.ParseUint(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || agentID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid agent id"})
		return
	}

	var req conversationClearRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.UserID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_id is required"})
		return
	}

	ctx := c.Request.Context()

	var conv conversation
	if err := m.db.WithContext(ctx).Where("agent_id = ? AND user_id = ?", agentID, req.UserID).Take(&conv).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusOK, gin.H{"cleared": false})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load conversation", "details": err.Error()})
		return
	}

	if err := m.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("conversation_id = ?", conv.ID).Delete(&message{}).Error; err != nil {
			return err
		}
		if err := tx.Delete(&conversation{}, "id = ?", conv.ID).Error; err != nil {
			return err
		}
		return nil
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to clear conversation", "details": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"cleared": true})
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

func bindCreateAgentRequest(c *gin.Context) (createAgentRequest, *multipart.FileHeader, error) {
	var req createAgentRequest
	contentType := strings.ToLower(c.GetHeader("Content-Type"))
	if strings.HasPrefix(contentType, "multipart/form-data") {
		if err := c.Request.ParseMultipartForm(25 << 20); err != nil {
			return req, nil, fmt.Errorf("invalid multipart payload: %w", err)
		}

		form := c.Request.MultipartForm
		req.Name = strings.TrimSpace(firstFormValue(form.Value["name"]))
		req.Gender = firstFormValue(form.Value["gender"])
		req.LangDefault = firstFormValue(form.Value["lang_default"])
		req.ModelProvider = firstFormValue(form.Value["model_provider"])
		req.ModelName = firstFormValue(form.Value["model_name"])
		req.ResponseFormat = firstFormValue(form.Value["response_format"])
		req.TitleAddress = optionalStringPointer(form.Value["title_address"])
		req.PersonaDesc = optionalStringPointer(form.Value["persona_desc"])
		req.OpeningLine = optionalStringPointer(form.Value["opening_line"])
		req.FirstTurnHint = optionalStringPointer(form.Value["first_turn_hint"])
		req.Live2DModelID = optionalStringPointer(form.Value["live2d_model_id"])
		req.Notes = optionalStringPointer(form.Value["notes"])
		req.SystemPrompt = optionalStringPointer(form.Value["system_prompt"])

		if values, ok := form.Value["tags"]; ok {
			tags, err := parseTagsField(values)
			if err != nil {
				return req, nil, err
			}
			req.Tags = tags
		}

		if tempStr := firstFormValue(form.Value["temperature"]); tempStr != "" {
			temperature, err := strconv.ParseFloat(tempStr, 64)
			if err != nil {
				return req, nil, fmt.Errorf("invalid temperature value")
			}
			req.Temperature = &temperature
		}

		if maxStr := firstFormValue(form.Value["max_tokens"]); maxStr != "" {
			tokens, err := strconv.Atoi(maxStr)
			if err != nil {
				return req, nil, fmt.Errorf("invalid max_tokens value")
			}
			req.MaxTokens = &tokens
		}

		var avatar *multipart.FileHeader
		if files := form.File["avatar"]; len(files) > 0 {
			avatar = files[0]
		} else if files := form.File["avatar[]"]; len(files) > 0 {
			avatar = files[0]
		}

		return req, avatar, nil
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		return req, nil, fmt.Errorf("invalid request payload: %w", err)
	}

	return req, nil, nil
}

func bindUpdateAgentRequest(c *gin.Context) (updateAgentRequest, *multipart.FileHeader, error) {
	var req updateAgentRequest
	contentType := strings.ToLower(c.GetHeader("Content-Type"))
	if strings.HasPrefix(contentType, "multipart/form-data") {
		if err := c.Request.ParseMultipartForm(25 << 20); err != nil {
			return req, nil, fmt.Errorf("invalid multipart payload: %w", err)
		}

		form := c.Request.MultipartForm
		req.Name = formStringPointer(form.Value["name"])
		req.Gender = formStringPointer(form.Value["gender"])
		req.TitleAddress = formStringPointer(form.Value["title_address"])
		req.PersonaDesc = formStringPointer(form.Value["persona_desc"])
		req.OpeningLine = formStringPointer(form.Value["opening_line"])
		req.FirstTurnHint = formStringPointer(form.Value["first_turn_hint"])
		req.Live2DModelID = formStringPointer(form.Value["live2d_model_id"])
		req.LangDefault = formStringPointer(form.Value["lang_default"])
		req.Notes = formStringPointer(form.Value["notes"])
		req.ModelProvider = formStringPointer(form.Value["model_provider"])
		req.ModelName = formStringPointer(form.Value["model_name"])
		req.ResponseFormat = formStringPointer(form.Value["response_format"])
		req.SystemPrompt = formStringPointer(form.Value["system_prompt"])
		req.Status = formStringPointer(form.Value["status"])

		if values, ok := form.Value["tags"]; ok {
			tags, err := parseTagsField(values)
			if err != nil {
				return req, nil, err
			}
			parsed := tags
			req.Tags = &parsed
		}

		if tempStr := firstFormValue(form.Value["temperature"]); tempStr != "" {
			temperature, err := strconv.ParseFloat(tempStr, 64)
			if err != nil {
				return req, nil, fmt.Errorf("invalid temperature value")
			}
			req.Temperature = &temperature
		}

		if maxStr := firstFormValue(form.Value["max_tokens"]); maxStr != "" {
			tokens, err := strconv.Atoi(maxStr)
			if err != nil {
				return req, nil, fmt.Errorf("invalid max_tokens value")
			}
			req.MaxTokens = &tokens
		}

		if values, ok := form.Value["remove_avatar"]; ok {
			flag, err := parseBoolField(values)
			if err != nil {
				return req, nil, err
			}
			req.RemoveAvatar = flag
		}

		var avatar *multipart.FileHeader
		if files := form.File["avatar"]; len(files) > 0 {
			avatar = files[0]
		} else if files := form.File["avatar[]"]; len(files) > 0 {
			avatar = files[0]
		}

		return req, avatar, nil
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		return req, nil, fmt.Errorf("invalid request payload: %w", err)
	}

	return req, nil, nil
}

func firstFormValue(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[0])
}

func optionalStringPointer(values []string) *string {
	if len(values) == 0 {
		return nil
	}
	trimmed := strings.TrimSpace(values[0])
	if trimmed == "" {
		return nil
	}
	result := trimmed
	return &result
}

func formStringPointer(values []string) *string {
	if len(values) == 0 {
		return nil
	}
	trimmed := strings.TrimSpace(values[0])
	result := trimmed
	return &result
}

func parseTagsField(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	if len(values) == 1 {
		raw := strings.TrimSpace(values[0])
		if raw == "" {
			return []string{}, nil
		}
		var parsed []string
		if strings.HasPrefix(raw, "[") {
			if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
				return normalizeTags(parsed), nil
			}
		}
		parts := strings.Split(raw, ",")
		return normalizeTags(parts), nil
	}
	return normalizeTags(values), nil
}

func parseBoolField(values []string) (*bool, error) {
	if len(values) == 0 {
		return nil, nil
	}
	raw := strings.TrimSpace(values[0])
	if raw == "" {
		return nil, nil
	}
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid boolean value")
	}
	return &parsed, nil
}

func normalizeTags(tags []string) []string {
	seen := make(map[string]struct{}, len(tags))
	result := make([]string, 0, len(tags))
	for _, tag := range tags {
		trimmed := strings.TrimSpace(tag)
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		result = append(result, trimmed)
	}
	return result
}

func requireRole(role string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if strings.TrimSpace(role) == "" {
			c.Next()
			return
		}

		claims := jwt.ExtractClaims(c)
		if len(claims) == 0 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}

		roles := extractRoles(claims["roles"])
		if !hasRole(roles, role) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "insufficient privileges"})
			return
		}

		c.Next()
	}
}

func extractRoles(value interface{}) []string {
	switch roles := value.(type) {
	case []string:
		result := make([]string, 0, len(roles))
		for _, role := range roles {
			trimmed := strings.ToLower(strings.TrimSpace(role))
			if trimmed != "" {
				result = append(result, trimmed)
			}
		}
		return result
	case []interface{}:
		result := make([]string, 0, len(roles))
		for _, item := range roles {
			if str, ok := item.(string); ok {
				trimmed := strings.ToLower(strings.TrimSpace(str))
				if trimmed != "" {
					result = append(result, trimmed)
				}
			}
		}
		return result
	case string:
		parts := strings.Split(roles, ",")
		result := make([]string, 0, len(parts))
		for _, part := range parts {
			trimmed := strings.ToLower(strings.TrimSpace(part))
			if trimmed != "" {
				result = append(result, trimmed)
			}
		}
		return result
	default:
		return nil
	}
}

func hasRole(roles []string, role string) bool {
	target := strings.ToLower(strings.TrimSpace(role))
	if target == "" {
		return false
	}
	for _, candidate := range roles {
		if strings.ToLower(strings.TrimSpace(candidate)) == target {
			return true
		}
	}
	return false
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
