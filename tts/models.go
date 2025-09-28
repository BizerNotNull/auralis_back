package tts

import "context"

// VoiceOption 描述可供选择的语音参数。
type VoiceOption struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Provider     string        `json:"provider"`
	Language     string        `json:"language"`
	Description  string        `json:"description,omitempty"`
	Emotions     []string      `json:"emotions,omitempty"`
	SampleURL    string        `json:"sample_url,omitempty"`
	DefaultStyle string        `json:"default_style,omitempty"`
	Model        string        `json:"model,omitempty"`
	Format       string        `json:"format,omitempty"`
	SampleRate   int           `json:"sample_rate,omitempty"`
	Settings     VoiceSettings `json:"settings"`
}

// VoiceSettings 定义语速音调的取值范围与默认值。
type VoiceSettings struct {
	SpeedRange      [2]float64 `json:"speed_range"`
	PitchRange      [2]float64 `json:"pitch_range"`
	DefaultSpeed    float64    `json:"default_speed"`
	DefaultPitch    float64    `json:"default_pitch"`
	SupportsEmotion bool       `json:"supports_emotion"`
}

// ProviderStatus 表示语音服务供应商的可用状态。
type ProviderStatus struct {
	ID              string `json:"id"`
	Label           string `json:"label"`
	Enabled         bool   `json:"enabled"`
	DefaultVoiceID  string `json:"default_voice,omitempty"`
	SupportsPreview bool   `json:"supports_preview"`
}

// SpeechRequest 表示一次语音合成请求。
type SpeechRequest struct {
	Text          string
	VoiceID       string
	Provider      string
	Emotion       string
	Speed         float64
	Pitch         float64
	Format        string
	Instructions  string
	ResolvedVoice *VoiceOption `json:"-"`
}

// SpeechResult 保存语音合成的结果数据。
type SpeechResult struct {
	VoiceID     string  `json:"voice_id"`
	Emotion     string  `json:"emotion,omitempty"`
	AudioBase64 string  `json:"audio_base64"`
	MimeType    string  `json:"mime_type"`
	Speed       float64 `json:"speed,omitempty"`
	Pitch       float64 `json:"pitch,omitempty"`
	Provider    string  `json:"provider"`
	DurationMs  int     `json:"duration_ms,omitempty"`
	AudioURL    string  `json:"audio_url,omitempty"`
}

// AsMap 将语音合成结果转换为通用字典。
func (r *SpeechResult) AsMap() map[string]any {
	if r == nil {
		return nil
	}
	payload := map[string]any{
		"voice_id":     r.VoiceID,
		"audio_base64": r.AudioBase64,
		"mime_type":    r.MimeType,
		"provider":     r.Provider,
	}
	if r.AudioURL != "" {
		payload["audio_url"] = r.AudioURL
	}
	if r.Emotion != "" {
		payload["emotion"] = r.Emotion
	}
	if r.Speed > 0 {
		payload["speed"] = r.Speed
	}
	if r.Pitch > 0 {
		payload["pitch"] = r.Pitch
	}
	if r.DurationMs > 0 {
		payload["duration_ms"] = r.DurationMs
	}
	return payload
}

// SpeechStreamRequest 描述流式语音合成的参数。
type SpeechStreamRequest struct {
	VoiceID       string
	Provider      string
	Emotion       string
	Speed         float64
	Pitch         float64
	Format        string
	Instructions  string
	InitialText   string
	ResolvedVoice *VoiceOption `json:"-"`
}

// SpeechStreamMetadata 返回流式会话的基础信息。
type SpeechStreamMetadata struct {
	VoiceID    string
	Provider   string
	Format     string
	MimeType   string
	SampleRate int
	Speed      float64
	Pitch      float64
	Emotion    string
}

// SpeechStreamChunk 表示流式语音的单个音频片段。
type SpeechStreamChunk struct {
	Sequence int
	Audio    []byte
}

// SpeechStreamSession 定义流式语音会话的行为接口。
type SpeechStreamSession interface {
	Metadata() SpeechStreamMetadata
	Audio() <-chan SpeechStreamChunk
	AppendText(ctx context.Context, text string) error
	Finalize(ctx context.Context) error
	Err() error
	Close() error
}

// StreamingSynthesizer 扩展合成器以支持流式能力。
type StreamingSynthesizer interface {
	Synthesizer
	Stream(ctx context.Context, req SpeechStreamRequest) (SpeechStreamSession, error)
}

// Synthesizer 定义语音合成器的通用接口。
type Synthesizer interface {
	Enabled() bool
	DefaultVoiceID() string
	Voices() []VoiceOption
	Synthesize(ctx context.Context, req SpeechRequest) (*SpeechResult, error)
}
