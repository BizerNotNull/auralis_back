package tts

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

type Module struct {
	client *Client
}

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

func (m *Module) Enabled() bool {
	return m != nil && m.client != nil && m.client.Enabled()
}

func (m *Module) DefaultVoiceID() string {
	if m == nil || m.client == nil {
		return ""
	}
	return m.client.DefaultVoiceID()
}

func (m *Module) Voices() []VoiceOption {
	if m == nil || m.client == nil {
		return nil
	}
	return m.client.Voices()
}

func (m *Module) Synthesize(ctx context.Context, req SpeechRequest) (*SpeechResult, error) {
	if m == nil || m.client == nil {
		return nil, ErrDisabled
	}
	return m.client.Synthesize(ctx, req)
}

func (m *Module) handleVoices(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"enabled":       m.Enabled(),
		"default_voice": m.DefaultVoiceID(),
		"voices":        m.Voices(),
	})
}

type previewRequest struct {
	Text    string   `json:"text" binding:"required"`
	VoiceID string   `json:"voice_id"`
	Emotion string   `json:"emotion"`
	Speed   *float64 `json:"speed"`
	Pitch   *float64 `json:"pitch"`
	Format  string   `json:"format"`
}

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
		Text:    req.Text,
		VoiceID: strings.TrimSpace(req.VoiceID),
		Emotion: strings.TrimSpace(req.Emotion),
		Speed:   speed,
		Pitch:   pitch,
		Format:  strings.TrimSpace(req.Format),
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

func clampFloat(value, min, max float64) float64 {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}
