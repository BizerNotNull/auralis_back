package tts

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// Module 对接语音合成客户端并暴露 HTTP 处理器。
type Module struct {
	client *Client
}

// RegisterRoutes 注册语音合成路由并初始化模块。
func RegisterRoutes(router *gin.Engine) (*Module, error) {
	client, err := NewClientFromEnv()
	if err != nil {
		return nil, err
	}

	module := &Module{client: client}

	group := router.Group("/tts")
	group.GET("/voices", module.handleVoices)
	group.POST("/preview", module.handlePreview)

	return module, nil
}

// Enabled 表示语音模块是否可用。
func (m *Module) Enabled() bool {
	return m != nil && m.client != nil && m.client.Enabled()
}

// DefaultVoiceID 返回默认语音 ID。
func (m *Module) DefaultVoiceID() string {
	if m == nil || m.client == nil {
		return ""
	}
	return m.client.DefaultVoiceID()
}

// DefaultProviderID 返回默认语音供应商。
func (m *Module) DefaultProviderID() string {
	if m == nil || m.client == nil {
		return ""
	}
	return m.client.DefaultProviderID()
}

// Providers 列出可用的语音供应商及状态。
func (m *Module) Providers() []ProviderStatus {
	if m == nil || m.client == nil {
		return nil
	}
	return m.client.Providers()
}

// Voices 返回全部语音选项。
func (m *Module) Voices() []VoiceOption {
	if m == nil || m.client == nil {
		return nil
	}
	return m.client.Voices()
}

// Synthesize 调用底层客户端执行语音合成。
func (m *Module) Synthesize(ctx context.Context, req SpeechRequest) (*SpeechResult, error) {
	if m == nil || m.client == nil {
		return nil, ErrDisabled
	}
	return m.client.Synthesize(ctx, req)
}

// Stream 调用底层客户端执行流式语音合成。
func (m *Module) Stream(ctx context.Context, req SpeechStreamRequest) (SpeechStreamSession, error) {
	if m == nil || m.client == nil {
		return nil, ErrDisabled
	}
	return m.client.Stream(ctx, req)
}

// handleVoices godoc
// @Summary 查询语音列表
// @Description 返回当前可用的语音提供方与默认配置
// @Tags TTS
// @Produce json
// @Success 200 {object} map[string]interface{} "语音列表"
// @Author bizer
// @Router /tts/voices [get]
func (m *Module) handleVoices(c *gin.Context) {
	providers := m.Providers()
	c.JSON(http.StatusOK, gin.H{
		"enabled":          m.Enabled(),
		"default_voice":    m.DefaultVoiceID(),
		"default_provider": m.DefaultProviderID(),
		"providers":        providers,
		"voices":           m.Voices(),
	})
}

// previewRequest 描述语音预览接口的请求参数。
type previewRequest struct {
	Text     string   `json:"text" binding:"required"`
	VoiceID  string   `json:"voice_id"`
	Provider string   `json:"provider"`
	Emotion  string   `json:"emotion"`
	Speed    *float64 `json:"speed"`
	Pitch    *float64 `json:"pitch"`
	Format   string   `json:"format"`
}

// handlePreview godoc
// @Summary 语音预览
// @Description 根据文本生成一次性语音预览
// @Tags TTS
// @Accept json
// @Produce json
// @Param request body previewRequest true "预览参数"
// @Success 200 {object} map[string]interface{} "预览结果"
// @Failure 400 {object} map[string]string "请求参数错误"
// @Failure 503 {object} map[string]string "服务未启用"
// @Failure 500 {object} map[string]string "服务器错误"
// @Author bizer
// @Router /tts/preview [post]
func (m *Module) handlePreview(c *gin.Context) {
	if !m.Enabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "text-to-speech is disabled"})
		return
	}

	var req previewRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request payload"})
		return
	}

	speed := 1.0
	if req.Speed != nil && *req.Speed > 0 {
		speed = clampFloat(*req.Speed, 0.5, 1.6)
	}

	pitch := 1.0
	if req.Pitch != nil && *req.Pitch > 0 {
		pitch = clampFloat(*req.Pitch, 0.7, 1.4)
	}

	speechReq := SpeechRequest{
		Text:     req.Text,
		VoiceID:  strings.TrimSpace(req.VoiceID),
		Provider: strings.TrimSpace(req.Provider),
		Emotion:  strings.TrimSpace(req.Emotion),
		Speed:    speed,
		Pitch:    pitch,
		Format:   strings.TrimSpace(req.Format),
	}
	if speechReq.Emotion != "" {
		speechReq.Instructions = fmt.Sprintf("Please speak with a %s tone.", speechReq.Emotion)
	}

	result, err := m.client.Synthesize(c.Request.Context(), speechReq)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, ErrDisabled) {
			status = http.StatusServiceUnavailable
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"speech": result})
}

// clampFloat 将浮点值限制在指定范围。
func clampFloat(value, min, max float64) float64 {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}
