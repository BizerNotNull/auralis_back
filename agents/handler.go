package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"auralis_back/authorization"
	knowledge "auralis_back/knowledge"
	filestore "auralis_back/storage"
	"auralis_back/tts"
	jwt "github.com/appleboy/gin-jwt/v2"
	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// Module 聚合了智能体相关的数据库、存储和知识库依赖。
type Module struct {
	db            *gorm.DB
	avatars       *filestore.AvatarStorage
	reviewEnabled bool
	knowledge     *knowledge.Service
}

const avatarURLExpiry = 15 * time.Minute

const (
	statusPending  = "pending"
	statusActive   = "active"
	statusRejected = "rejected"
	statusPaused   = "paused"
	statusArchived = "archived"

	claimUserIDKey = "user_id"
	claimRolesKey  = "roles"
)

const (
	defaultRatingsPageSize = 10
	maxRatingsPageSize     = 50
	maxListLimit           = 100
)

// applyAvatarURL 对头像地址做清理并生成带时效的签名 URL。
func (m *Module) applyAvatarURL(ctx context.Context, agent *Agent) {
	if m == nil || agent == nil || agent.AvatarURL == nil {
		return
	}

	trimmed := strings.TrimSpace(*agent.AvatarURL)
	if trimmed == "" {
		agent.AvatarURL = nil
		return
	}

	*agent.AvatarURL = trimmed

	if m.avatars == nil {
		return
	}

	signed, err := m.avatars.PresignedURL(ctx, trimmed, avatarURLExpiry)
	if err != nil {
		log.Printf("agents: presign avatar url failed: %v", err)
		return
	}

	*agent.AvatarURL = signed
}

// parseEnvBool 读取布尔环境变量并返回解析结果。
func parseEnvBool(key string) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return false, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("invalid boolean value %q", raw)
	}
	return value, nil
}

// reviewRequired 判断智能体是否需要审核流程。
func (m *Module) reviewRequired() bool {
	if m == nil {
		return false
	}
	raw := strings.TrimSpace(os.Getenv("AGENT_REVIEW_ENABLED"))
	if raw == "" {
		return m.reviewEnabled
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return m.reviewEnabled
	}
	return value
}

// RegisterRoutes 初始化智能体模块并注册所有相关路由。
func RegisterRoutes(router *gin.Engine, guard *authorization.Guard) (*Module, error) {
	db, err := openDatabaseFromEnv()
	if err != nil {
		return nil, err
	}

	if err := db.AutoMigrate(&Agent{}, &AgentChatConfig{}, &AgentRating{}); err != nil {
		return nil, err
	}

	knowledgeService, err := knowledge.NewServiceFromEnv(db)
	if err != nil {
		return nil, err
	}
	if err := knowledgeService.AutoMigrate(); err != nil {
		return nil, err
	}

	avatarStore, err := filestore.NewAvatarStorageFromEnv()
	if err != nil {
		return nil, err
	}

	reviewEnabled, err := parseEnvBool("AGENT_REVIEW_ENABLED")
	if err != nil {
		log.Printf("agents: invalid AGENT_REVIEW_ENABLED value: %v", err)
	}

	module := &Module{db: db, avatars: avatarStore, reviewEnabled: reviewEnabled, knowledge: knowledgeService}

	group := router.Group("/agents")
	group.GET("", module.handleListAgents)
	group.GET("/:id", module.handleGetAgent)
	group.POST("/:id/conversations", module.handleCreateConversation)
	group.DELETE("/:id/conversations", module.handleClearConversation)
	group.GET("/:id/ratings", module.handleGetRatings)
	group.PUT("/:id/ratings", module.handleUpsertRating)

	authGroup := group.Group("")
	if guard != nil {
		authGroup.Use(guard.RequireAuthenticated())
	} else {
		authGroup.Use(func(c *gin.Context) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authorization middleware missing"})
		})
	}
	authGroup.POST("", module.handleCreateAgent)
	authGroup.GET("/mine", module.handleListMyAgents)
	authGroup.GET("/:id/knowledge", module.handleListKnowledgeDocuments)
	authGroup.POST("/:id/knowledge", module.handleCreateKnowledgeDocument)
	authGroup.GET("/:id/knowledge/:docID", module.handleGetKnowledgeDocument)
	authGroup.PUT("/:id/knowledge/:docID", module.handleUpdateKnowledgeDocument)
	authGroup.DELETE("/:id/knowledge/:docID", module.handleDeleteKnowledgeDocument)
	authGroup.PUT("/:id", module.handleUpdateAgent)

	adminGroup := router.Group("/admin/agents")
	if guard != nil {
		adminGroup.Use(guard.RequireAuthenticated(), guard.RequireRole("admin"))
	} else {
		adminGroup.Use(func(c *gin.Context) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authorization middleware missing"})
		})
	}
	adminGroup.GET("", module.handleAdminListAgents)

	return module, nil
}

type createAgentRequest struct {
	Name             string   `json:"name" binding:"required"`
	Gender           string   `json:"gender"`
	TitleAddress     *string  `json:"title_address"`
	OneSentenceIntro *string  `json:"one_sentence_intro"`
	PersonaDesc      *string  `json:"persona_desc"`
	OpeningLine      *string  `json:"opening_line"`
	FirstTurnHint    *string  `json:"first_turn_hint"`
	Live2DModelID    *string  `json:"live2d_model_id"`
	VoiceID          string   `json:"voice_id"`
	VoiceProvider    string   `json:"voice_provider"`
	LangDefault      string   `json:"lang_default"`
	Tags             []string `json:"tags"`
	Notes            *string  `json:"notes"`
	ModelProvider    string   `json:"model_provider" binding:"required"`
	ModelName        string   `json:"model_name" binding:"required"`
	ResponseFormat   string   `json:"response_format"`
	Temperature      *float64 `json:"temperature"`
	MaxTokens        *int     `json:"max_tokens"`
	SystemPrompt     *string  `json:"system_prompt"`
}

type updateAgentRequest struct {
	Name             *string   `json:"name"`
	Gender           *string   `json:"gender"`
	TitleAddress     *string   `json:"title_address"`
	OneSentenceIntro *string   `json:"one_sentence_intro"`
	PersonaDesc      *string   `json:"persona_desc"`
	OpeningLine      *string   `json:"opening_line"`
	FirstTurnHint    *string   `json:"first_turn_hint"`
	Live2DModelID    *string   `json:"live2d_model_id"`
	VoiceID          *string   `json:"voice_id"`
	VoiceProvider    *string   `json:"voice_provider"`
	LangDefault      *string   `json:"lang_default"`
	Tags             *[]string `json:"tags"`
	Notes            *string   `json:"notes"`
	ModelProvider    *string   `json:"model_provider"`
	ModelName        *string   `json:"model_name"`
	ResponseFormat   *string   `json:"response_format"`
	Temperature      *float64  `json:"temperature"`
	MaxTokens        *int      `json:"max_tokens"`
	SystemPrompt     *string   `json:"system_prompt"`
	Status           *string   `json:"status"`
	RemoveAvatar     *bool     `json:"remove_avatar"`
}

type knowledgeDocumentRequest struct {
	Title   string   `json:"title"`
	Summary *string  `json:"summary"`
	Source  *string  `json:"source"`
	Content string   `json:"content"`
	Tags    []string `json:"tags"`
	Status  string   `json:"status"`
}

type knowledgeDocumentUpdateRequest struct {
	Title   *string   `json:"title"`
	Summary *string   `json:"summary"`
	Source  *string   `json:"source"`
	Content *string   `json:"content"`
	Tags    *[]string `json:"tags"`
	Status  *string   `json:"status"`
}

// handleCreateAgent godoc
// @Summary 创建智能体
// @Description 创建新的智能体配置并可选上传头像
// @Tags Agents
// @Accept json
// @Accept multipart/form-data
// @Produce json
// @Param request body createAgentRequest true "智能体信息"
// @Param avatar formData file false "头像文件"
// @Success 201 {object} map[string]interface{} "创建成功的智能体"
// @Failure 400 {object} map[string]string "请求参数错误"
// @Failure 401 {object} map[string]string "未授权"
// @Failure 500 {object} map[string]string "服务器错误"
// @Author bizer
// handleCreateAgent 处理创建智能体的请求并落库。
func (m *Module) handleCreateAgent(c *gin.Context) {
	if m.db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database not initialized"})
		return
	}

	userID, _ := currentUserContext(c)
	if userID == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
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

	agentStatus := statusActive
	if m.reviewRequired() {
		agentStatus = statusPending
	}

	agent := Agent{
		Name:        name,
		Gender:      gender,
		Status:      agentStatus,
		LangDefault: lang,
		Version:     1,
		CreatedBy:   userID,
	}

	agent.TitleAddress = normalizeStringPointer(req.TitleAddress)
	agent.OneSentenceIntro = normalizeStringPointer(req.OneSentenceIntro)
	agent.PersonaDesc = normalizeStringPointer(req.PersonaDesc)
	agent.OpeningLine = normalizeStringPointer(req.OpeningLine)
	agent.FirstTurnHint = normalizeStringPointer(req.FirstTurnHint)
	agent.Live2DModelID = normalizeStringPointer(req.Live2DModelID)
	if voice := strings.TrimSpace(req.VoiceID); voice != "" {
		voiceCopy := voice
		agent.VoiceID = &voiceCopy
	}
	if provider := strings.TrimSpace(req.VoiceProvider); provider != "" {
		normalized := tts.NormalizeProviderID(provider)
		if normalized != "" {
			providerCopy := normalized
			agent.VoiceProvider = &providerCopy
		}
	}
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
		avatarURL, uploadErr := m.avatars.Upload(ctx, avatarFile, "agents", fmt.Sprintf("%d", agent.ID))
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

	m.applyAvatarURL(ctx, &agent)

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

// handleUpdateAgent godoc
// @Summary 更新智能体
// @Description 更新指定智能体的基础信息与模型配置
// @Tags Agents
// @Accept json
// @Accept multipart/form-data
// @Produce json
// @Param id path int true "智能体ID"
// @Param request body updateAgentRequest true "更新内容"
// @Param avatar formData file false "头像文件"
// @Success 200 {object} map[string]interface{} "更新后的智能体"
// @Failure 400 {object} map[string]string "请求参数错误"
// @Failure 401 {object} map[string]string "未授权"
// @Failure 403 {object} map[string]string "权限不足"
// @Failure 404 {object} map[string]string "未找到"
// @Failure 500 {object} map[string]string "服务器错误"
// @Author bizer
// handleUpdateAgent 处理智能体信息的更新操作。
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

	userID, roles := currentUserContext(c)
	if userID == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	isAdmin := hasRole(roles, "admin")

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

	if !isAdmin && agent.CreatedBy != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "insufficient privileges"})
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
		uploaded, uploadErr := m.avatars.Upload(ctx, avatarFile, "agents", fmt.Sprintf("%d", agentID))
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
	if req.OneSentenceIntro != nil {
		agentUpdates["one_sentence_intro"] = normalizeStringPointer(req.OneSentenceIntro)
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
	if req.VoiceID != nil {
		agentUpdates["voice_id"] = normalizeStringPointer(req.VoiceID)
	}
	if req.VoiceProvider != nil {
		providerValue := strings.TrimSpace(*req.VoiceProvider)
		if providerValue == "" {
			agentUpdates["voice_provider"] = gorm.Expr("NULL")
		} else {
			normalized := tts.NormalizeProviderID(providerValue)
			if normalized == "" {
				agentUpdates["voice_provider"] = gorm.Expr("NULL")
			} else {
				providerCopy := normalized
				agentUpdates["voice_provider"] = &providerCopy
			}
		}
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
		if !isAdmin {
			if newAvatarURL != "" {
				_ = m.avatars.Remove(ctx, newAvatarURL)
			}
			c.JSON(http.StatusForbidden, gin.H{"error": "status updates require admin privileges"})
			return
		}
		status := strings.ToLower(strings.TrimSpace(*req.Status))
		allowedStatus := map[string]struct{}{
			"draft":        {},
			statusPending:  {},
			statusActive:   {},
			statusPaused:   {},
			statusArchived: {},
			statusRejected: {},
		}
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

	hasChanges := len(agentUpdates) > 0 || cfgChanged

	if !isAdmin && hasChanges {
		if m.reviewRequired() {
			agentUpdates["status"] = statusPending
		} else if agent.Status != statusActive {
			agentUpdates["status"] = statusActive
		}
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

	m.applyAvatarURL(ctx, &agent)

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

// handleListAgents godoc
// @Summary 列出公开智能体
// @Description 按排序和状态筛选可用的智能体列表
// @Tags Agents
// @Produce json
// @Param sort query string false "排序字段，可选 hot|views|rating|updated|created"
// @Param direction query string false "排序方向，默认 desc"
// @Param limit query int false "返回数量上限"
// @Param status query string false "状态筛选，默认 active"
// @Success 200 {object} map[string]interface{} "智能体列表"
// @Failure 400 {object} map[string]string "请求参数错误"
// @Failure 500 {object} map[string]string "服务器错误"
// @Author bizer
// handleListAgents 返回对外展示的智能体列表。
func (m *Module) handleListAgents(c *gin.Context) {
	if m.db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database not initialized"})
		return
	}

	ctx := c.Request.Context()

	sortOrder, ok := normalizeAgentSortOrder(c.Query("sort"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid sort value"})
		return
	}

	direction := normalizeSortDirection(c.Query("direction"))

	limit, err := parsePositiveLimit(c.Query("limit"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid limit value"})
		return
	}

	status := strings.TrimSpace(c.Query("status"))
	query := m.db.WithContext(ctx)
	if status != "" {
		query = query.Where("status = ?", status)
	} else {
		query = query.Where("status = ?", statusActive)
	}

	var agents []Agent
	if err := query.Find(&agents).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list agents", "details": err.Error()})
		return
	}

	agentIDs := make([]uint64, 0, len(agents))
	for _, agent := range agents {
		agentIDs = append(agentIDs, agent.ID)
	}

	summaries := make(map[uint64]ratingSummary, len(agentIDs))
	if len(agentIDs) > 0 {
		var summaryErr error
		summaries, summaryErr = m.loadRatingSummaries(ctx, agentIDs)
		if summaryErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load rating summaries", "details": summaryErr.Error()})
			return
		}
	}

	for i := range agents {
		summary, exists := summaries[agents[i].ID]
		if exists {
			agents[i].AverageRating = summary.AverageScore
			agents[i].RatingCount = summary.RatingCount
		} else {
			agents[i].AverageRating = 0
			agents[i].RatingCount = 0
		}
		agents[i].HotScore = computeAgentHotScore(agents[i])
		m.applyAvatarURL(ctx, &agents[i])
	}

	sortAgents(agents, sortOrder, direction)

	if limit > 0 && limit < len(agents) {
		agents = agents[:limit]
	}

	c.JSON(http.StatusOK, gin.H{"agents": agents})
}

// parsePositiveLimit 解析列表请求中的 limit 参数。
func parsePositiveLimit(raw string) (int, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, nil
	}

	value, err := strconv.Atoi(trimmed)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("invalid limit")
	}

	if value > maxListLimit {
		return maxListLimit, nil
	}

	return value, nil
}

// normalizeSortDirection 规范化排序方向字符串。
func normalizeSortDirection(raw string) string {
	value := strings.ToLower(strings.TrimSpace(raw))
	switch value {
	case "asc", "ascending", "ascend", "up":
		return "asc"
	default:
		return "desc"
	}
}

// normalizeAgentSortOrder 校验并返回智能体列表的排序字段。
func normalizeAgentSortOrder(raw string) (string, bool) {
	value := strings.ToLower(strings.TrimSpace(raw))
	if value == "" {
		return "hot", true
	}

	switch value {
	case "hot", "popular", "popularity", "hotness":
		return "hot", true
	case "views", "view_count", "viewcount", "hits", "traffic":
		return "views", true
	case "rating", "ratings", "score", "reviews":
		return "rating", true
	case "updated", "updated_at", "latest", "modified", "recent":
		return "updated", true
	case "created", "created_at", "creation", "newest":
		return "created", true
	default:
		return "", false
	}
}

// sortAgents 按指定字段和方向对智能体集合排序。
func sortAgents(list []Agent, order, direction string) {
	if len(list) <= 1 {
		return
	}

	ascending := strings.ToLower(direction) == "asc"

	compareNumbers := func(a, b float64) int {
		switch {
		case a < b:
			return -1
		case a > b:
			return 1
		default:
			return 0
		}
	}

	compareCounts := func(a, b uint64) int {
		switch {
		case a < b:
			return -1
		case a > b:
			return 1
		default:
			return 0
		}
	}

	compareTimes := func(a, b time.Time) int {
		switch {
		case a.Before(b):
			return -1
		case a.After(b):
			return 1
		default:
			return 0
		}
	}

	less := func(result int) bool {
		if ascending {
			return result < 0
		}
		return result > 0
	}

	sort.SliceStable(list, func(i, j int) bool {
		switch order {
		case "views":
			if cmp := compareCounts(list[i].ViewCount, list[j].ViewCount); cmp != 0 {
				return less(cmp)
			}
			if cmp := compareTimes(list[i].UpdatedAt, list[j].UpdatedAt); cmp != 0 {
				return less(cmp)
			}
			return less(compareCounts(uint64(list[i].ID), uint64(list[j].ID)))
		case "rating":
			if cmp := compareNumbers(list[i].AverageRating, list[j].AverageRating); cmp != 0 {
				return less(cmp)
			}
			if cmp := compareCounts(uint64(list[i].RatingCount), uint64(list[j].RatingCount)); cmp != 0 {
				return less(cmp)
			}
			return less(compareTimes(list[i].UpdatedAt, list[j].UpdatedAt))
		case "updated":
			if cmp := compareTimes(list[i].UpdatedAt, list[j].UpdatedAt); cmp != 0 {
				return less(cmp)
			}
			return less(compareCounts(uint64(list[i].ID), uint64(list[j].ID)))
		case "created":
			if cmp := compareTimes(list[i].CreatedAt, list[j].CreatedAt); cmp != 0 {
				return less(cmp)
			}
			return less(compareCounts(uint64(list[i].ID), uint64(list[j].ID)))
		default:
			if cmp := compareNumbers(list[i].HotScore, list[j].HotScore); cmp != 0 {
				return less(cmp)
			}
			if cmp := compareCounts(list[i].ViewCount, list[j].ViewCount); cmp != 0 {
				return less(cmp)
			}
			return less(compareTimes(list[i].UpdatedAt, list[j].UpdatedAt))
		}
	})
}

// computeAgentHotScore 根据浏览量和评分计算热度值。
func computeAgentHotScore(agent Agent) float64 {
	viewComponent := math.Log(float64(agent.ViewCount)+1) * 4
	ratingComponent := agent.AverageRating * (float64(agent.RatingCount) + 1)
	recencyComponent := 0.0
	if !agent.UpdatedAt.IsZero() {
		hours := time.Since(agent.UpdatedAt).Hours()
		if hours < 0 {
			hours = 0
		}
		recencyComponent = math.Max(0, 6-math.Log(hours+1)*2)
	}

	return viewComponent + ratingComponent + recencyComponent
}

// handleListMyAgents godoc
// @Summary 列出我的智能体
// @Description 返回当前登录用户创建的所有智能体
// @Tags Agents
// @Produce json
// @Success 200 {object} map[string]interface{} "智能体列表"
// @Failure 401 {object} map[string]string "未授权"
// @Failure 500 {object} map[string]string "服务器错误"
// @Author bizer
// handleListMyAgents 获取当前用户创建的智能体列表。
func (m *Module) handleListMyAgents(c *gin.Context) {
	if m.db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database not initialized"})
		return
	}

	userID, _ := currentUserContext(c)
	if userID == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	ctx := c.Request.Context()

	var agents []Agent
	if err := m.db.WithContext(ctx).Where("created_by = ?", userID).Order("updated_at DESC").Find(&agents).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list agents", "details": err.Error()})
		return
	}

	agentIDs := make([]uint64, 0, len(agents))
	for _, agent := range agents {
		agentIDs = append(agentIDs, agent.ID)
	}

	summaries := make(map[uint64]ratingSummary, len(agentIDs))
	if len(agentIDs) > 0 {
		var err error
		summaries, err = m.loadRatingSummaries(ctx, agentIDs)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load rating summaries", "details": err.Error()})
			return
		}
	}

	for i := range agents {
		summary, exists := summaries[agents[i].ID]
		if exists {
			agents[i].AverageRating = summary.AverageScore
			agents[i].RatingCount = summary.RatingCount
		} else {
			agents[i].AverageRating = 0
			agents[i].RatingCount = 0
		}
		agents[i].HotScore = computeAgentHotScore(agents[i])
		m.applyAvatarURL(ctx, &agents[i])
	}

	c.JSON(http.StatusOK, gin.H{"agents": agents})
}

// handleListKnowledgeDocuments godoc
// @Summary 列出智能体知识库文档
// @Description 返回指定智能体下的知识库文档列表
// @Tags Agents
// @Produce json
// @Param id path int true "智能体 ID"
// @Success 200 {object} map[string]any
// @Failure 400 {object} map[string]string
// @Failure 401 {object} map[string]string
// @Failure 403 {object} map[string]string
// @Failure 404 {object} map[string]string
// handleListKnowledgeDocuments 列出智能体关联的知识库文档。
func (m *Module) handleListKnowledgeDocuments(c *gin.Context) {
	if m == nil || m.knowledge == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "knowledge service not available"})
		return
	}

	agentID, err := parseUintID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid agent id"})
		return
	}

	userID, roles := currentUserContext(c)
	if userID == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	ctx := c.Request.Context()
	_, allowed, err := m.ensureKnowledgeAccess(ctx, agentID, userID, roles)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "agent not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify access"})
		}
		return
	}
	if !allowed {
		c.JSON(http.StatusForbidden, gin.H{"error": "insufficient permissions"})
		return
	}

	documents, err := m.knowledge.ListDocuments(ctx, agentID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load documents"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"documents": documents})
}

// handleGetKnowledgeDocument godoc
// @Summary 获取智能体知识库文档
// @Tags Agents
// @Produce json
// @Param id path int true "智能体 ID"
// @Param docID path int true "文档 ID"
// @Success 200 {object} map[string]any
// @Failure 400 {object} map[string]string
// @Failure 401 {object} map[string]string
// @Failure 403 {object} map[string]string
// @Failure 404 {object} map[string]string
// handleGetKnowledgeDocument 获取单个知识库文档详情。
func (m *Module) handleGetKnowledgeDocument(c *gin.Context) {
	if m == nil || m.knowledge == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "knowledge service not available"})
		return
	}

	agentID, err := parseUintID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid agent id"})
		return
	}
	docID, err := parseUintID(c.Param("docID"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid document id"})
		return
	}

	userID, roles := currentUserContext(c)
	if userID == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	ctx := c.Request.Context()
	_, allowed, err := m.ensureKnowledgeAccess(ctx, agentID, userID, roles)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "agent not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify access"})
		}
		return
	}
	if !allowed {
		c.JSON(http.StatusForbidden, gin.H{"error": "insufficient permissions"})
		return
	}

	document, err := m.knowledge.GetDocument(ctx, agentID, docID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "document not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load document"})
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{"document": document})
}

// handleCreateKnowledgeDocument godoc
// @Summary 创建知识库文档
// @Tags Agents
// @Accept json
// @Produce json
// @Param id path int true "智能体 ID"
// @Param request body knowledgeDocumentRequest true "文档内容"
// @Success 201 {object} map[string]any
// @Failure 400 {object} map[string]string
// @Failure 401 {object} map[string]string
// @Failure 403 {object} map[string]string
// @Failure 404 {object} map[string]string
// handleCreateKnowledgeDocument 新建智能体的知识库文档。
func (m *Module) handleCreateKnowledgeDocument(c *gin.Context) {
	if m == nil || m.knowledge == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "knowledge service not available"})
		return
	}

	agentID, err := parseUintID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid agent id"})
		return
	}

	userID, roles := currentUserContext(c)
	if userID == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	var req knowledgeDocumentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request payload"})
		return
	}

	ctx := c.Request.Context()
	_, allowed, err := m.ensureKnowledgeAccess(ctx, agentID, userID, roles)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "agent not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify access"})
		}
		return
	}
	if !allowed {
		c.JSON(http.StatusForbidden, gin.H{"error": "insufficient permissions"})
		return
	}

	input := knowledge.DocumentInput{
		Title:   req.Title,
		Summary: req.Summary,
		Source:  req.Source,
		Content: req.Content,
		Tags:    req.Tags,
		Status:  req.Status,
	}

	record, err := m.knowledge.CreateDocument(ctx, agentID, userID, input)
	if err != nil {
		msg := strings.TrimSpace(err.Error())
		if strings.HasPrefix(msg, "knowledge:") {
			c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create document"})
		}
		return
	}

	c.JSON(http.StatusCreated, gin.H{"document": record})
}

// handleUpdateKnowledgeDocument godoc
// @Summary 更新知识库文档
// @Tags Agents
// @Accept json
// @Produce json
// @Param id path int true "智能体 ID"
// @Param docID path int true "文档 ID"
// @Param request body knowledgeDocumentUpdateRequest true "更新内容"
// @Success 200 {object} map[string]any
// @Failure 400 {object} map[string]string
// @Failure 401 {object} map[string]string
// @Failure 403 {object} map[string]string
// @Failure 404 {object} map[string]string
// handleUpdateKnowledgeDocument 更新知识库文档内容或元信息。
func (m *Module) handleUpdateKnowledgeDocument(c *gin.Context) {
	if m == nil || m.knowledge == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "knowledge service not available"})
		return
	}

	agentID, err := parseUintID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid agent id"})
		return
	}
	docID, err := parseUintID(c.Param("docID"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid document id"})
		return
	}

	userID, roles := currentUserContext(c)
	if userID == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	var req knowledgeDocumentUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request payload"})
		return
	}

	if req.Title == nil && req.Summary == nil && req.Source == nil && req.Content == nil && req.Tags == nil && req.Status == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no fields to update"})
		return
	}

	ctx := c.Request.Context()
	_, allowed, err := m.ensureKnowledgeAccess(ctx, agentID, userID, roles)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "agent not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify access"})
		}
		return
	}
	if !allowed {
		c.JSON(http.StatusForbidden, gin.H{"error": "insufficient permissions"})
		return
	}

	update := knowledge.DocumentUpdate{}
	if req.Title != nil {
		title := strings.TrimSpace(*req.Title)
		update.Title = &title
	}
	if req.Summary != nil {
		summary := strings.TrimSpace(*req.Summary)
		update.Summary = &summary
	}
	if req.Source != nil {
		source := strings.TrimSpace(*req.Source)
		update.Source = &source
	}
	if req.Content != nil {
		content := *req.Content
		update.Content = &content
	}
	if req.Tags != nil {
		update.Tags = req.Tags
	}
	if req.Status != nil {
		status := strings.TrimSpace(*req.Status)
		update.Status = &status
	}

	record, err := m.knowledge.UpdateDocument(ctx, agentID, docID, userID, update)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "document not found"})
		} else {
			msg := strings.TrimSpace(err.Error())
			if strings.HasPrefix(msg, "knowledge:") {
				c.JSON(http.StatusBadRequest, gin.H{"error": msg})
			} else {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update document"})
			}
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{"document": record})
}

// handleDeleteKnowledgeDocument godoc
// @Summary 删除知识库文档
// @Tags Agents
// @Param id path int true "智能体 ID"
// @Param docID path int true "文档 ID"
// @Success 204 ""
// @Failure 400 {object} map[string]string
// @Failure 401 {object} map[string]string
// @Failure 403 {object} map[string]string
// @Failure 404 {object} map[string]string
// handleDeleteKnowledgeDocument 删除指定的知识库文档。
func (m *Module) handleDeleteKnowledgeDocument(c *gin.Context) {
	if m == nil || m.knowledge == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "knowledge service not available"})
		return
	}

	agentID, err := parseUintID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid agent id"})
		return
	}
	docID, err := parseUintID(c.Param("docID"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid document id"})
		return
	}

	userID, roles := currentUserContext(c)
	if userID == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	ctx := c.Request.Context()
	_, allowed, err := m.ensureKnowledgeAccess(ctx, agentID, userID, roles)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "agent not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify access"})
		}
		return
	}
	if !allowed {
		c.JSON(http.StatusForbidden, gin.H{"error": "insufficient permissions"})
		return
	}

	if err := m.knowledge.DeleteDocument(ctx, agentID, docID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "document not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete document"})
		}
		return
	}

	c.Status(http.StatusNoContent)
}

// handleAdminListAgents godoc
// @Summary 管理员查询智能体
// @Description 管理员按状态、创建者和排序获取智能体列表
// @Tags Agents
// @Produce json
// @Param status query string false "状态筛选，支持 all"
// @Param created_by query int false "按创建者过滤"
// @Param sort query string false "排序字段"
// @Param direction query string false "排序方向"
// @Param limit query int false "返回数量上限"
// @Success 200 {object} map[string]interface{} "智能体列表"
// @Failure 400 {object} map[string]string "请求参数错误"
// @Failure 500 {object} map[string]string "服务器错误"
// @Author bizer
// handleAdminListAgents 提供管理员视角的智能体列表。
func (m *Module) handleAdminListAgents(c *gin.Context) {
	if m.db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database not initialized"})
		return
	}

	ctx := c.Request.Context()
	query := m.db.WithContext(ctx)

	status := strings.TrimSpace(c.Query("status"))
	if status != "" && !strings.EqualFold(status, "all") {
		query = query.Where("status = ?", status)
	}

	creatorParam := strings.TrimSpace(c.Query("created_by"))
	if creatorParam != "" {
		creatorID, err := strconv.ParseUint(creatorParam, 10, 64)
		if err != nil || creatorID == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid created_by value"})
			return
		}
		query = query.Where("created_by = ?", creatorID)
	}

	sortOrder, ok := normalizeAgentSortOrder(c.Query("sort"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid sort value"})
		return
	}

	direction := normalizeSortDirection(c.Query("direction"))

	limit, err := parsePositiveLimit(c.Query("limit"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid limit value"})
		return
	}

	var agents []Agent
	if err := query.Find(&agents).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list agents", "details": err.Error()})
		return
	}

	agentIDs := make([]uint64, 0, len(agents))
	for _, agent := range agents {
		agentIDs = append(agentIDs, agent.ID)
	}

	summaries := make(map[uint64]ratingSummary, len(agentIDs))
	if len(agentIDs) > 0 {
		var err error
		summaries, err = m.loadRatingSummaries(ctx, agentIDs)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load rating summaries", "details": err.Error()})
			return
		}
	}

	for i := range agents {
		summary, exists := summaries[agents[i].ID]
		if exists {
			agents[i].AverageRating = summary.AverageScore
			agents[i].RatingCount = summary.RatingCount
		} else {
			agents[i].AverageRating = 0
			agents[i].RatingCount = 0
		}
		agents[i].HotScore = computeAgentHotScore(agents[i])
		m.applyAvatarURL(ctx, &agents[i])
	}

	sortAgents(agents, sortOrder, direction)

	if limit > 0 && limit < len(agents) {
		agents = agents[:limit]
	}

	c.JSON(http.StatusOK, gin.H{"agents": agents})
}

// handleGetAgent godoc
// @Summary 获取智能体详情
// @Description 获取单个智能体的完整信息和对话配置
// @Tags Agents
// @Produce json
// @Param id path int true "智能体ID"
// @Success 200 {object} map[string]interface{} "智能体详情"
// @Failure 400 {object} map[string]string "请求参数错误"
// @Failure 404 {object} map[string]string "未找到"
// @Failure 500 {object} map[string]string "服务器错误"
// @Author bizer
// handleGetAgent 返回单个智能体的详细信息。
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

	if err := m.db.WithContext(ctx).
		Model(&Agent{}).
		Where("id = ?", id).
		UpdateColumn("view_count", gorm.Expr("view_count + ?", 1)).Error; err != nil {
		log.Printf("agents: failed to increment view count for %d: %v", id, err)
	} else {
		agent.ViewCount++
	}

	summary, err := m.loadRatingSummary(ctx, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load rating summary", "details": err.Error()})
		return
	}
	agent.AverageRating = summary.AverageScore
	agent.RatingCount = summary.RatingCount
	agent.HotScore = computeAgentHotScore(agent)

	m.applyAvatarURL(ctx, &agent)

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

type ratingSummary struct {
	AgentID      uint64  `json:"agent_id"`
	AverageScore float64 `json:"average_score"`
	RatingCount  int64   `json:"rating_count"`
}

type upsertRatingRequest struct {
	UserID  uint64  `json:"user_id" binding:"required"`
	Score   int     `json:"score" binding:"required"`
	Comment *string `json:"comment"`
}

// handleGetRatings godoc
// @Summary 查询智能体评分
// @Description 获取评分汇总并分页返回详细评价
// @Tags Agents
// @Produce json
// @Param id path int true "智能体ID"
// @Param user_id query int false "指定用户的评分"
// @Param page query int false "页码，从1开始"
// @Param page_size query int false "每页条数，默认10"
// @Success 200 {object} map[string]interface{} "评分数据"
// @Failure 400 {object} map[string]string "请求参数错误"
// @Failure 500 {object} map[string]string "服务器错误"
// @Author bizer
// handleGetRatings 分页查询智能体的评分记录。
func (m *Module) handleGetRatings(c *gin.Context) {
	if m.db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database not initialized"})
		return
	}

	agentID, err := strconv.ParseUint(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || agentID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid agent id"})
		return
	}

	ctx := c.Request.Context()

	summary, err := m.loadRatingSummary(ctx, agentID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load rating summary", "details": err.Error()})
		return
	}

	response := gin.H{"summary": summary}

	userParam := strings.TrimSpace(c.Query("user_id"))
	if userParam != "" {
		userID, err := strconv.ParseUint(userParam, 10, 64)
		if err != nil || userID == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user_id"})
			return
		}

		var existing AgentRating
		if err := m.db.WithContext(ctx).Where("agent_id = ? AND user_id = ?", agentID, userID).Take(&existing).Error; err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load user rating", "details": err.Error()})
				return
			}
		} else {
			response["user_rating"] = existing
		}
	}

	page := 1
	if pageParam := strings.TrimSpace(c.Query("page")); pageParam != "" {
		if value, convErr := strconv.Atoi(pageParam); convErr == nil && value > 0 {
			page = value
		}
	}

	pageSize := defaultRatingsPageSize
	if sizeParam := strings.TrimSpace(c.Query("page_size")); sizeParam != "" {
		if value, convErr := strconv.Atoi(sizeParam); convErr == nil && value > 0 {
			if value > maxRatingsPageSize {
				value = maxRatingsPageSize
			}
			pageSize = value
		}
	}

	if pageSize <= 0 {
		pageSize = defaultRatingsPageSize
	}

	offset := (page - 1) * pageSize
	if offset < 0 {
		offset = 0
	}

	var totalCount int64
	if err := m.db.WithContext(ctx).Model(&AgentRating{}).Where("agent_id = ?", agentID).Count(&totalCount).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to count ratings", "details": err.Error()})
		return
	}

	tableName := AgentRating{}.TableName()
	var ratings []struct {
		ID              uint64    `json:"id"`
		AgentID         uint64    `json:"agent_id"`
		UserID          uint64    `json:"user_id"`
		Score           int       `json:"score"`
		Comment         *string   `json:"comment,omitempty"`
		CreatedAt       time.Time `json:"created_at"`
		UpdatedAt       time.Time `json:"updated_at"`
		UserDisplayName string    `json:"user_display_name"`
		UserAvatarURL   *string   `json:"user_avatar_url"`
	}

	listQuery := m.db.WithContext(ctx).
		Table(tableName).
		Select("agent_ratings.id, agent_ratings.agent_id, agent_ratings.user_id, agent_ratings.score, agent_ratings.comment, agent_ratings.created_at, agent_ratings.updated_at, users.display_name AS user_display_name, users.avatar_url AS user_avatar_url").
		Joins("LEFT JOIN users ON users.id = agent_ratings.user_id").
		Where("agent_ratings.agent_id = ?", agentID).
		Order("agent_ratings.updated_at DESC")

	if pageSize > 0 {
		listQuery = listQuery.Limit(pageSize).Offset(offset)
	}

	if err := listQuery.Scan(&ratings).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load ratings", "details": err.Error()})
		return
	}

	response["ratings"] = ratings
	response["pagination"] = gin.H{
		"page":      page,
		"page_size": pageSize,
		"total":     totalCount,
	}
	response["total_count"] = totalCount

	c.JSON(http.StatusOK, response)
}

// handleUpsertRating godoc
// @Summary 提交或更新评分
// @Description 为指定智能体创建或更新用户评分
// @Tags Agents
// @Accept json
// @Produce json
// @Param id path int true "智能体ID"
// @Param request body upsertRatingRequest true "评分内容"
// @Success 200 {object} map[string]interface{} "最新评分信息"
// @Failure 400 {object} map[string]string "请求参数错误"
// @Failure 500 {object} map[string]string "服务器错误"
// @Author bizer
// handleUpsertRating 创建或更新用户对智能体的评分。
func (m *Module) handleUpsertRating(c *gin.Context) {
	if m.db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database not initialized"})
		return
	}

	agentID, err := strconv.ParseUint(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || agentID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid agent id"})
		return
	}

	var req upsertRatingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request payload"})
		return
	}

	if req.UserID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_id is required"})
		return
	}

	if req.Score < 1 || req.Score > 5 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "score must be between 1 and 5"})
		return
	}

	ctx := c.Request.Context()

	var rating AgentRating
	err = m.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("agent_id = ? AND user_id = ?", agentID, req.UserID).Take(&rating).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				rating = AgentRating{
					AgentID: agentID,
					UserID:  req.UserID,
					Score:   req.Score,
					Comment: normalizeStringPointer(req.Comment),
				}
				return tx.Create(&rating).Error
			}
			return err
		}

		rating.Score = req.Score
		rating.Comment = normalizeStringPointer(req.Comment)

		return tx.Save(&rating).Error
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save rating", "details": err.Error()})
		return
	}

	summary, err := m.loadRatingSummary(ctx, agentID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load rating summary", "details": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"rating":  rating,
		"summary": summary,
	})
}

// loadRatingSummaries 批量加载智能体的评分统计信息。
func (m *Module) loadRatingSummaries(ctx context.Context, agentIDs []uint64) (map[uint64]ratingSummary, error) {
	summaries := make(map[uint64]ratingSummary, len(agentIDs))
	for _, id := range agentIDs {
		summaries[id] = ratingSummary{
			AgentID:      id,
			AverageScore: 0,
			RatingCount:  0,
		}
	}

	if len(agentIDs) == 0 || m == nil || m.db == nil {
		return summaries, nil
	}

	var rows []struct {
		AgentID      uint64
		AverageScore float64
		RatingCount  int64
	}
	dbResult := m.db.WithContext(ctx).
		Model(&AgentRating{}).
		Select("agent_id, AVG(score) AS average_score, COUNT(*) AS rating_count").
		Where("agent_id IN ?", agentIDs).
		Group("agent_id").
		Scan(&rows)
	if dbResult.Error != nil {
		return nil, dbResult.Error
	}

	for _, row := range rows {
		summaries[row.AgentID] = ratingSummary{
			AgentID:      row.AgentID,
			AverageScore: roundRating(row.AverageScore),
			RatingCount:  row.RatingCount,
		}
	}

	return summaries, nil
}

// loadRatingSummary 返回指定智能体的评分统计。
func (m *Module) loadRatingSummary(ctx context.Context, agentID uint64) (ratingSummary, error) {
	summaries, err := m.loadRatingSummaries(ctx, []uint64{agentID})
	if err != nil {
		return ratingSummary{AgentID: agentID, AverageScore: 0, RatingCount: 0}, err
	}
	if summary, ok := summaries[agentID]; ok {
		return summary, nil
	}
	return ratingSummary{AgentID: agentID, AverageScore: 0, RatingCount: 0}, nil
}

// roundRating 对评分值进行一位小数的四舍五入。
func roundRating(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return math.Round(value*10) / 10
}

type conversationInitRequest struct {
	UserID uint64 `json:"user_id" binding:"required"`
}

type conversationClearRequest struct {
	UserID uint64 `json:"user_id" binding:"required"`
}

// handleClearConversation godoc
// @Summary 清除会话记录
// @Description 删除用户与智能体的历史会话及消息
// @Tags Agents
// @Accept json
// @Produce json
// @Param id path int true "智能体ID"
// @Param request body conversationClearRequest true "清理请求"
// @Success 200 {object} map[string]interface{} "是否清理成功"
// @Failure 400 {object} map[string]string "请求参数错误"
// @Failure 500 {object} map[string]string "服务器错误"
// @Author bizer
// handleClearConversation 清空智能体会话的聊天记录。
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

// handleCreateConversation godoc
// @Summary 初始化会话
// @Description 为用户与智能体建立新的对话会话
// @Tags Agents
// @Accept json
// @Produce json
// @Param id path int true "智能体ID"
// @Param request body conversationInitRequest true "会话初始化参数"
// @Success 200 {object} map[string]interface{} "会话信息"
// @Failure 400 {object} map[string]string "请求参数错误"
// @Failure 404 {object} map[string]string "未找到"
// @Failure 409 {object} map[string]string "状态冲突"
// @Failure 500 {object} map[string]string "服务器错误"
// @Author bizer
// handleCreateConversation 为智能体创建新的会话上下文。
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

// TableName 指定会话对象在数据库中的表名。
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

// TableName 指定消息对象在数据库中的表名。
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

// bindCreateAgentRequest 解析并校验创建智能体的表单数据。
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
		req.OneSentenceIntro = optionalStringPointer(form.Value["one_sentence_intro"])
		req.PersonaDesc = optionalStringPointer(form.Value["persona_desc"])
		req.OpeningLine = optionalStringPointer(form.Value["opening_line"])
		req.FirstTurnHint = optionalStringPointer(form.Value["first_turn_hint"])
		req.Live2DModelID = optionalStringPointer(form.Value["live2d_model_id"])
		req.VoiceID = firstFormValue(form.Value["voice_id"])

		req.VoiceProvider = firstFormValue(form.Value["voice_provider"])
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

// bindUpdateAgentRequest 解析并校验更新智能体的表单数据。
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
		req.OneSentenceIntro = formStringPointer(form.Value["one_sentence_intro"])
		req.PersonaDesc = formStringPointer(form.Value["persona_desc"])
		req.OpeningLine = formStringPointer(form.Value["opening_line"])
		req.FirstTurnHint = formStringPointer(form.Value["first_turn_hint"])
		req.Live2DModelID = formStringPointer(form.Value["live2d_model_id"])
		req.VoiceID = formStringPointer(form.Value["voice_id"])

		req.VoiceProvider = formStringPointer(form.Value["voice_provider"])
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

// firstFormValue 获取表单字段的第一个取值。
func firstFormValue(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[0])
}

// optionalStringPointer 将可选字段转换为去空格的指针。
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

// formStringPointer 从表单值生成去空格的字符串指针。
func formStringPointer(values []string) *string {
	if len(values) == 0 {
		return nil
	}
	trimmed := strings.TrimSpace(values[0])
	result := trimmed
	return &result
}

// parseTagsField 解析标签字段并确保唯一性。
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

// parseBoolField 将表单布尔字段解析为指针值。
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

// parseUintID 将字符串形式的 ID 转换为无符号整数。
func parseUintID(raw string) (uint64, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, errors.New("invalid id")
	}
	id, err := strconv.ParseUint(trimmed, 10, 64)
	if err != nil || id == 0 {
		return 0, errors.New("invalid id")
	}
	return id, nil
}

// ensureKnowledgeAccess 校验用户对智能体知识库的访问权限。
func (m *Module) ensureKnowledgeAccess(ctx context.Context, agentID, userID uint64, roles []string) (*Agent, bool, error) {
	if m == nil || m.db == nil {
		return nil, false, errors.New("database not initialized")
	}
	var agent Agent
	if err := m.db.WithContext(ctx).Where("id = ?", agentID).Take(&agent).Error; err != nil {
		return nil, false, err
	}
	if hasRole(roles, "admin") || agent.CreatedBy == userID {
		return &agent, true, nil
	}
	return &agent, false, nil
}

// KnowledgeService 返回内部持有的知识库服务实例。
func (m *Module) KnowledgeService() *knowledge.Service {
	if m == nil {
		return nil
	}
	return m.knowledge
}

// currentUserContext 从请求上下文提取用户 ID 和角色信息。
func currentUserContext(c *gin.Context) (uint64, []string) {
	if c == nil {
		return 0, nil
	}

	claims := jwt.ExtractClaims(c)
	if len(claims) == 0 {
		return 0, nil
	}

	userID := parseUserIDClaim(claims[claimUserIDKey])
	roles := extractRolesClaim(claims[claimRolesKey])

	return userID, roles
}

// parseUserIDClaim 从 JWT 声明中解析用户 ID。
func parseUserIDClaim(raw interface{}) uint64 {
	switch v := raw.(type) {
	case float64:
		if v <= 0 {
			return 0
		}
		return uint64(v)
	case float32:
		if v <= 0 {
			return 0
		}
		return uint64(v)
	case int:
		if v <= 0 {
			return 0
		}
		return uint64(v)
	case int32:
		if v <= 0 {
			return 0
		}
		return uint64(v)
	case int64:
		if v <= 0 {
			return 0
		}
		return uint64(v)
	case uint:
		return uint64(v)
	case uint32:
		return uint64(v)
	case uint64:
		return v
	case json.Number:
		parsed, err := v.Int64()
		if err != nil || parsed <= 0 {
			return 0
		}
		return uint64(parsed)
	default:
		return 0
	}
}

// extractRolesClaim 从 JWT 声明中解析角色列表。
func extractRolesClaim(raw interface{}) []string {
	switch values := raw.(type) {
	case []string:
		result := make([]string, 0, len(values))
		for _, role := range values {
			trimmed := strings.TrimSpace(role)
			if trimmed != "" {
				result = append(result, trimmed)
			}
		}
		return result
	case []interface{}:
		result := make([]string, 0, len(values))
		for _, value := range values {
			if label, ok := value.(string); ok {
				trimmed := strings.TrimSpace(label)
				if trimmed != "" {
					result = append(result, trimmed)
				}
			}
		}
		return result
	case string:
		trimmed := strings.TrimSpace(values)
		if trimmed == "" {
			return []string{}
		}
		return []string{trimmed}
	default:
		return []string{}
	}
}

// hasRole 判断角色列表中是否包含目标角色。
func hasRole(roles []string, target string) bool {
	if len(roles) == 0 {
		return false
	}

	normalized := strings.ToLower(strings.TrimSpace(target))
	if normalized == "" {
		return false
	}

	for _, role := range roles {
		if strings.ToLower(strings.TrimSpace(role)) == normalized {
			return true
		}
	}

	return false
}

// normalizeTags 去重并清理标签列表。
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
// normalizeStringPointer 去除字符串指针中的多余空白。
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
