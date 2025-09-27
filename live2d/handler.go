package live2d

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"auralis_back/agents"
	"auralis_back/authorization"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Module struct {
	db      *gorm.DB
	storage *assetStorage
}

type createModelForm struct {
	Name               string `form:"name" binding:"required"`
	Description        string `form:"description"`
	EntryFile          string `form:"entry_file"`
	PreviewFile        string `form:"preview_file"`
	ExternalModelURL   string `form:"external_model_url"`
	ExternalPreviewURL string `form:"external_preview_url"`
}

type modelDTO struct {
	ID          uint64  `json:"id"`
	Key         string  `json:"key"`
	Name        string  `json:"name"`
	Description *string `json:"description,omitempty"`
	EntryURL    string  `json:"entry_url"`
	PreviewURL  *string `json:"preview_url,omitempty"`
	StorageType string  `json:"storage_type"`
	CreatedAt   int64   `json:"created_at"`
	UpdatedAt   int64   `json:"updated_at"`
}

func RegisterRoutes(router *gin.Engine, guard *authorization.Guard) (*Module, error) {
	db, err := openDatabaseFromEnv()
	if err != nil {
		return nil, err
	}

	if err := db.AutoMigrate(&Live2DModel{}); err != nil {
		return nil, fmt.Errorf("live2d: migrate tables: %w", err)
	}

	storage, err := newAssetStorageFromEnv()
	if err != nil {
		return nil, err
	}

	module := &Module{db: db, storage: storage}

	group := router.Group("/live2d/models")
	group.GET("", module.handleListModels)
	group.GET("/:id", module.handleGetModel)
	group.GET("/:id/files/*filepath", module.handleServeFile)

	admin := group.Group("")
	if guard != nil {
		admin.Use(guard.RequireAuthenticated(), guard.RequireRole("admin"))
	} else {
		admin.Use(func(c *gin.Context) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authorization middleware missing"})
		})
	}
	admin.POST("", module.handleCreateModel)
	admin.DELETE("/:id", module.handleDeleteModel)

	return module, nil
}

// handleListModels godoc
// @Summary 列出模型
// @Description 返回所有已注册的 Live2D 模型信息
// @Tags Live2D
// @Produce json
// @Success 200 {object} map[string]interface{} "模型列表"
// @Failure 500 {object} map[string]string "服务器错误"
// @Author bizer
// @Router /live2d/models [get]
func (m *Module) handleListModels(c *gin.Context) {
	var models []Live2DModel
	if err := m.db.Order("created_at desc").Find(&models).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list live2d models"})
		return
	}

	result := make([]modelDTO, 0, len(models))
	for _, model := range models {
		dto := m.toDTO(&model)
		result = append(result, dto)
	}

	c.JSON(http.StatusOK, gin.H{"models": result})
}

// handleGetModel godoc
// @Summary 获取模型详情
// @Description 根据 ID 获取单个 Live2D 模型的详细信息
// @Tags Live2D
// @Produce json
// @Param id path int true "模型ID"
// @Success 200 {object} map[string]interface{} "模型详情"
// @Failure 400 {object} map[string]string "请求参数错误"
// @Failure 404 {object} map[string]string "未找到"
// @Author bizer
// @Router /live2d/models/{id} [get]
func (m *Module) handleGetModel(c *gin.Context) {
	model, err := m.fetchModelByParam(c.Param("id"))
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "model not found"})
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{"model": m.toDTO(model)})
}

// handleCreateModel godoc
// @Summary 创建模型
// @Description 上传 Live2D 资源或登记外部模型链接
// @Tags Live2D
// @Accept multipart/form-data
// @Produce json
// @Param name formData string true "模型名称"
// @Param description formData string false "模型描述"
// @Param archive formData file false "模型压缩包"
// @Param external_model_url formData string false "外部模型地址"
// @Param external_preview_url formData string false "外部预览地址"
// @Success 201 {object} map[string]interface{} "新建的模型"
// @Failure 400 {object} map[string]string "请求参数错误"
// @Failure 500 {object} map[string]string "服务器错误"
// @Author bizer
// @Router /live2d/models [post]
func (m *Module) handleCreateModel(c *gin.Context) {
	var form createModelForm
	if err := c.ShouldBind(&form); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid form payload"})
		return
	}

	name := strings.TrimSpace(form.Name)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	description := strings.TrimSpace(form.Description)
	entryHint := form.EntryFile
	previewHint := form.PreviewFile
	externalModelURL := strings.TrimSpace(form.ExternalModelURL)
	externalPreviewURL := strings.TrimSpace(form.ExternalPreviewURL)

	archive, err := c.FormFile("archive")
	if err != nil && !errors.Is(err, http.ErrMissingFile) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid archive file"})
		return
	}

	if archive == nil && externalModelURL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "either archive or external_model_url is required"})
		return
	}
	if archive != nil && externalModelURL != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provide either archive or external_model_url, not both"})
		return
	}

	var storageType = "external"
	var storagePath string
	var entryFile string
	var previewFile *string

	if archive != nil {
		if m.storage == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "live2d asset storage is not configured"})
			return
		}
		storageType = "local"
		folder, entry, preview, err := m.storage.SaveArchive(archive, entryHint, previewHint)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		storagePath = folder
		entryFile = entry
		previewFile = preview
	} else {
		if !isValidURL(externalModelURL) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "external_model_url must be a valid absolute URL or an absolute path"})
			return
		}
		entryFile = externalModelURL
		if externalPreviewURL != "" {
			if !isValidURL(externalPreviewURL) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "external_preview_url must be a valid absolute URL or an absolute path"})
				return
			}
			previewFile = &externalPreviewURL
		}
	}

	key, err := m.generateKey(name)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate model key"})
		if archive != nil && storageType == "local" {
			m.storage.Remove(storagePath)
		}
		return
	}

	model := Live2DModel{
		Key:         key,
		Name:        name,
		StorageType: storageType,
		StoragePath: storagePath,
		EntryFile:   entryFile,
	}
	if description != "" {
		model.Description = &description
	}
	if previewFile != nil {
		model.PreviewFile = previewFile
	}

	if err := m.db.Create(&model).Error; err != nil {
		if archive != nil && storageType == "local" {
			m.storage.Remove(storagePath)
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create model"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"model": m.toDTO(&model)})
}

// handleDeleteModel godoc
// @Summary 删除模型
// @Description 删除指定的 Live2D 模型及其本地资源
// @Tags Live2D
// @Produce json
// @Param id path int true "模型ID"
// @Success 200 {object} map[string]interface{} "删除结果"
// @Failure 400 {object} map[string]string "请求参数错误"
// @Failure 404 {object} map[string]string "未找到"
// @Failure 409 {object} map[string]string "资源冲突"
// @Failure 500 {object} map[string]string "服务器错误"
// @Author bizer
// @Router /live2d/models/{id} [delete]
func (m *Module) handleDeleteModel(c *gin.Context) {
	model, err := m.fetchModelByParam(c.Param("id"))
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "model not found"})
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		}
		return
	}

	refValue := m.entryURL(model)
	if refValue != "" {
		var count int64
		if err := m.db.Model(&agents.Agent{}).
			Where("live2d_model_id = ?", refValue).
			Count(&count).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check agent references"})
			return
		}
		if count > 0 {
			c.JSON(http.StatusConflict, gin.H{"error": "model is in use by existing agents"})
			return
		}
	}

	if err := m.db.Delete(&Live2DModel{}, model.ID).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete model"})
		return
	}

	if model.StorageType == "local" && model.StoragePath != "" {
		if err := m.storage.Remove(model.StoragePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "model deleted but failed to remove files"})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}

// handleServeFile godoc
// @Summary 下载模型资源
// @Description 获取指定模型的静态资源文件
// @Tags Live2D
// @Produce application/octet-stream
// @Param id path int true "模型ID"
// @Param filepath path string true "资源路径"
// @Success 200 {file} binary "模型资源文件"
// @Failure 404 {object} map[string]string "未找到"
// @Author bizer
// @Router /live2d/models/{id}/files/{filepath} [get]
func (m *Module) handleServeFile(c *gin.Context) {
	model, err := m.fetchModelByParam(c.Param("id"))
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	if model.StorageType != "local" || m.storage == nil {
		c.Status(http.StatusNotFound)
		return
	}

	rel := normalizeArchivePath(c.Param("filepath"))
	if rel == "" {
		c.Status(http.StatusNotFound)
		return
	}

	base := filepath.Join(m.storage.BaseDir(), model.StoragePath)
	target := filepath.Join(base, filepath.FromSlash(rel))
	if !strings.HasPrefix(target, base+string(os.PathSeparator)) && target != base {
		c.Status(http.StatusForbidden)
		return
	}

	if _, err := os.Stat(target); err != nil {
		c.Status(http.StatusNotFound)
		return
	}

	c.Header("Cache-Control", "public, max-age=86400")
	c.Header("Access-Control-Allow-Origin", "*")
	c.Header("Access-Control-Allow-Methods", "GET, OPTIONS")
	c.Header("Access-Control-Expose-Headers", "Content-Type")

	c.File(target)
}

func (m *Module) fetchModelByParam(param string) (*Live2DModel, error) {
	trimmed := strings.TrimSpace(param)
	if trimmed == "" {
		return nil, errors.New("missing id")
	}

	var model Live2DModel
	if id, err := strconv.ParseUint(trimmed, 10, 64); err == nil {
		if err := m.db.First(&model, "id = ?", id).Error; err != nil {
			return nil, err
		}
		return &model, nil
	}

	if err := m.db.First(&model, "`key` = ?", trimmed).Error; err != nil {
		return nil, err
	}
	return &model, nil
}

func (m *Module) toDTO(model *Live2DModel) modelDTO {
	dto := modelDTO{
		ID:          model.ID,
		Key:         model.Key,
		Name:        model.Name,
		EntryURL:    m.entryURL(model),
		StorageType: model.StorageType,
		CreatedAt:   model.CreatedAt.Unix(),
		UpdatedAt:   model.UpdatedAt.Unix(),
	}
	if model.Description != nil {
		dto.Description = model.Description
	}
	if preview := m.previewURL(model); preview != "" {
		dto.PreviewURL = &preview
	}
	return dto
}

func (m *Module) entryURL(model *Live2DModel) string {
	if model.StorageType == "external" {
		return strings.TrimSpace(model.EntryFile)
	}
	if model.StorageType == "local" {
		return buildFileURL(model.ID, model.EntryFile)
	}
	return strings.TrimSpace(model.EntryFile)
}

func (m *Module) previewURL(model *Live2DModel) string {
	if model.PreviewFile == nil {
		return ""
	}
	if model.StorageType == "external" {
		return strings.TrimSpace(*model.PreviewFile)
	}
	if model.StorageType == "local" {
		return buildFileURL(model.ID, *model.PreviewFile)
	}
	return strings.TrimSpace(*model.PreviewFile)
}

func (m *Module) generateKey(name string) (string, error) {
	base := slugify(name)
	if base == "" {
		base = fmt.Sprintf("model-%s", uuidChunk())
	}
	key := base
	for i := 1; i < 50; i++ {
		var count int64
		if err := m.db.Model(&Live2DModel{}).Where("`key` = ?", key).Count(&count).Error; err != nil {
			return "", err
		}
		if count == 0 {
			return key, nil
		}
		key = fmt.Sprintf("%s-%d", base, i)
	}
	return fmt.Sprintf("%s-%s", base, uuidChunk()), nil
}

func slugify(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	prevHyphen := true
	for _, r := range value {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
			prevHyphen = false
		default:
			if !prevHyphen {
				b.WriteRune('-')
				prevHyphen = true
			}
		}
	}
	result := strings.Trim(b.String(), "-")
	return result
}

func uuidChunk() string {
	id := uuid.NewString()
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func buildFileURL(id uint64, relative string) string {
	trimmed := strings.TrimPrefix(strings.TrimSpace(relative), "/")
	if trimmed == "" {
		return ""
	}
	parts := strings.Split(trimmed, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return fmt.Sprintf("/live2d/models/%d/files/%s", id, strings.Join(parts, "/"))
}

func isValidURL(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false
	}
	if strings.HasPrefix(trimmed, "/") {
		return true
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return false
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return false
	}
	return true
}
