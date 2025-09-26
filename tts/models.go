package tts

import "context"

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

type VoiceSettings struct {
	SpeedRange      [2]float64 `json:"speed_range"`
	PitchRange      [2]float64 `json:"pitch_range"`
	DefaultSpeed    float64    `json:"default_speed"`
	DefaultPitch    float64    `json:"default_pitch"`
	SupportsEmotion bool       `json:"supports_emotion"`
}

type ProviderStatus struct {
	ID              string `json:"id"`
	Label           string `json:"label"`
	Enabled         bool   `json:"enabled"`
	DefaultVoiceID  string `json:"default_voice,omitempty"`
	SupportsPreview bool   `json:"supports_preview"`
}

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

type Synthesizer interface {
	Enabled() bool
	DefaultVoiceID() string
	Voices() []VoiceOption
	Synthesize(ctx context.Context, req SpeechRequest) (*SpeechResult, error)
}
