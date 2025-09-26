package tts

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

var ErrDisabled = errors.New("tts: service disabled")

type Client struct {
	httpClient         *http.Client
	qiniu              *qiniuDriver
	cosy               *cosyVoiceDriver
	voices             []VoiceOption
	voiceIndex         map[string]string
	voiceCatalog       map[string]VoiceOption
	providers          []ProviderStatus
	defaultVoice       string
	defaultProvider    string
	globalDefaultVoice string
	enabled            bool
}

func NewClientFromEnv() (*Client, error) {
	httpClient := &http.Client{Timeout: 45 * time.Second}

	client := &Client{
		httpClient:         httpClient,
		globalDefaultVoice: strings.TrimSpace(os.Getenv("TTS_DEFAULT_VOICE")),
	}

	client.qiniu = newQiniuDriverFromEnv(httpClient)
	client.cosy = newCosyVoiceDriverFromEnv()

	client.bootstrapVoiceCatalog()

	return client, nil
}

func (c *Client) Enabled() bool {
	return c != nil && c.enabled
}

func (c *Client) DefaultVoiceID() string {
	if c == nil {
		return ""
	}
	return c.defaultVoice
}

func (c *Client) DefaultProviderID() string {
	if c == nil {
		return ""
	}
	return c.defaultProvider
}

func (c *Client) Voices() []VoiceOption {
	if c == nil {
		return nil
	}
	out := make([]VoiceOption, len(c.voices))
	copy(out, c.voices)
	return out
}

func (c *Client) Providers() []ProviderStatus {
	if c == nil {
		return nil
	}
	out := make([]ProviderStatus, len(c.providers))
	copy(out, c.providers)
	return out
}

func (c *Client) Synthesize(ctx context.Context, req SpeechRequest) (*SpeechResult, error) {
	if c == nil {
		return nil, ErrDisabled
	}
	if !c.Enabled() {
		return nil, ErrDisabled
	}

	provider := NormalizeProviderID(req.Provider)
	voiceID := strings.TrimSpace(req.VoiceID)
	if provider == "" && voiceID != "" {
		if mapped, ok := c.voiceIndex[strings.ToLower(voiceID)]; ok {
			provider = mapped
		}
	}
	if provider == "" {
		provider = c.defaultProvider
	}

	switch provider {
	case "", "qiniu-openai":
		if c.qiniu == nil || !c.qiniu.Enabled() {
			return nil, ErrDisabled
		}
		if voiceID == "" {
			voiceID = c.qiniu.DefaultVoiceID()
			if voiceID == "" {
				voiceID = c.defaultVoice
			}
			req.VoiceID = voiceID
		}
		req.ResolvedVoice = c.voiceOption(voiceID)
		req.Provider = c.qiniu.ProviderID()
		return c.qiniu.Synthesize(ctx, req)
	case "aliyun-cosyvoice":
		if c.cosy == nil || !c.cosy.Enabled() {
			return nil, ErrDisabled
		}
		if voiceID == "" {
			voiceID = c.cosy.DefaultVoiceID()
			if voiceID == "" {
				voiceID = c.defaultVoice
			}
			req.VoiceID = voiceID
		}
		req.ResolvedVoice = c.voiceOption(voiceID)
		req.Provider = c.cosy.ProviderID()
		return c.cosy.Synthesize(ctx, req)
	default:
		return nil, fmt.Errorf("tts: unsupported provider %q", provider)
	}
}

func (c *Client) voiceOption(id string) *VoiceOption {
	if c == nil {
		return nil
	}
	trimmed := strings.ToLower(strings.TrimSpace(id))
	if trimmed == "" {
		return nil
	}
	if option, ok := c.voiceCatalog[trimmed]; ok {
		clone := option
		return &clone
	}
	return nil
}

func (c *Client) bootstrapVoiceCatalog() {
	if c == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	aggregated := make([]VoiceOption, 0, 16)
	providerStatuses := make([]ProviderStatus, 0, 2)

	if c.qiniu != nil {
		aggregated = append(aggregated, c.qiniu.ensureVoices(ctx)...)
		providerStatuses = append(providerStatuses, c.qiniu.status())
	}

	if c.cosy != nil {
		aggregated = append(aggregated, c.cosy.ensureVoices()...)
		providerStatuses = append(providerStatuses, c.cosy.status())
	}

	if custom := loadVoiceCatalogFromEnv(); len(custom) > 0 {
		aggregated = custom
	}

	voiceIndex := make(map[string]string, len(aggregated))
	voiceCatalog := make(map[string]VoiceOption, len(aggregated))

	for _, voice := range aggregated {
		trimmedID := strings.ToLower(strings.TrimSpace(voice.ID))
		if trimmedID == "" {
			continue
		}
		provider := NormalizeProviderID(voice.Provider)
		voiceIndex[trimmedID] = provider
		voiceCatalog[trimmedID] = voice
	}

	c.voices = aggregated
	c.voiceIndex = voiceIndex
	c.voiceCatalog = voiceCatalog

	providers := make([]ProviderStatus, 0, len(providerStatuses))
	for _, status := range providerStatuses {
		providerID := NormalizeProviderID(status.ID)
		defaultVoice := strings.TrimSpace(status.DefaultVoiceID)
		if defaultVoice != "" {
			if _, ok := voiceIndex[strings.ToLower(defaultVoice)]; !ok {
				defaultVoice = ""
			}
		}
		if defaultVoice == "" {
			for _, voice := range aggregated {
				if NormalizeProviderID(voice.Provider) == providerID {
					defaultVoice = voice.ID
					break
				}
			}
		}
		status.ID = providerID
		status.DefaultVoiceID = defaultVoice
		providers = append(providers, status)
	}

	c.providers = providers

	c.enabled = false
	for _, p := range providers {
		if p.Enabled {
			c.enabled = true
			break
		}
	}

	c.defaultVoice = ""
	c.defaultProvider = ""

	if c.globalDefaultVoice != "" {
		normalized := strings.ToLower(strings.TrimSpace(c.globalDefaultVoice))
		if provider, ok := voiceIndex[normalized]; ok {
			c.defaultVoice = c.globalDefaultVoice
			c.defaultProvider = provider
		}
	}

	if c.defaultVoice == "" {
		for _, p := range providers {
			if !p.Enabled {
				continue
			}
			if p.DefaultVoiceID != "" {
				c.defaultVoice = p.DefaultVoiceID
				c.defaultProvider = p.ID
				break
			}
		}
	}

	if c.defaultVoice == "" && len(aggregated) > 0 {
		c.defaultVoice = aggregated[0].ID
		c.defaultProvider = NormalizeProviderID(aggregated[0].Provider)
	}
}

type qiniuDriver struct {
	httpClient     *http.Client
	baseURL        string
	backupBaseURL  string
	apiKey         string
	model          string
	defaultVoice   string
	responseFormat string
	providerID     string
	voices         []VoiceOption
	enabled        bool
}

func newQiniuDriverFromEnv(httpClient *http.Client) *qiniuDriver {
	baseURL := strings.TrimSpace(firstNonEmpty(
		os.Getenv("TTS_QINIU_API_BASE_URL"),
		os.Getenv("TTS_API_BASE_URL"),
	))
	if baseURL == "" {
		baseURL = "https://openai.qiniu.com/v1"
	}
	baseURL = strings.TrimRight(baseURL, "/")

	backupBaseURL := strings.TrimSpace(firstNonEmpty(
		os.Getenv("TTS_QINIU_API_BACKUP_URL"),
		os.Getenv("TTS_API_BACKUP_URL"),
	))
	if backupBaseURL == "" {
		backupBaseURL = "https://api.qnaigc.com/v1"
	}
	backupBaseURL = strings.TrimRight(backupBaseURL, "/")
	if backupBaseURL == baseURL {
		backupBaseURL = ""
	}

	apiKey := strings.TrimSpace(firstNonEmpty(
		os.Getenv("TTS_QINIU_API_KEY"),
		os.Getenv("QINIU_TTS_API_KEY"),
		os.Getenv("TTS_API_KEY"),
	))

	model := strings.TrimSpace(firstNonEmpty(
		os.Getenv("TTS_QINIU_MODEL_ID"),
		os.Getenv("TTS_MODEL_ID"),
	))
	if model == "" {
		model = "tts"
	}

	responseFormat := strings.TrimSpace(firstNonEmpty(
		os.Getenv("TTS_QINIU_RESPONSE_FORMAT"),
		os.Getenv("TTS_RESPONSE_FORMAT"),
	))
	if responseFormat == "" {
		responseFormat = "mp3"
	}

	defaultVoice := strings.TrimSpace(firstNonEmpty(
		os.Getenv("TTS_QINIU_DEFAULT_VOICE"),
		os.Getenv("TTS_DEFAULT_VOICE"),
	))
	if defaultVoice == "" {
		defaultVoice = "qiniu_zh_female_tmjxxy"
	}

	return &qiniuDriver{
		httpClient:     httpClient,
		baseURL:        baseURL,
		backupBaseURL:  backupBaseURL,
		apiKey:         apiKey,
		model:          model,
		defaultVoice:   defaultVoice,
		responseFormat: responseFormat,
		providerID:     "qiniu-openai",
		enabled:        apiKey != "",
	}
}

func (d *qiniuDriver) ProviderID() string {
	if d == nil {
		return "qiniu-openai"
	}
	return d.providerID
}

func (d *qiniuDriver) Enabled() bool {
	return d != nil && d.enabled
}

func (d *qiniuDriver) DefaultVoiceID() string {
	if d == nil {
		return ""
	}
	return d.defaultVoice
}

func (d *qiniuDriver) ensureVoices(ctx context.Context) []VoiceOption {
	if d == nil {
		return nil
	}
	if len(d.voices) == 0 {
		if remote := d.fetchVoiceCatalog(ctx); len(remote) > 0 {
			d.voices = remote
		}
	}
	if len(d.voices) == 0 {
		d.voices = defaultQiniuVoiceCatalog()
	}
	if d.defaultVoice == "" && len(d.voices) > 0 {
		d.defaultVoice = d.voices[0].ID
	} else if d.defaultVoice != "" && !containsVoiceID(d.voices, d.defaultVoice) && len(d.voices) > 0 {
		log.Printf("tts: default qiniu voice %q not found; using %q", d.defaultVoice, d.voices[0].ID)
		d.defaultVoice = d.voices[0].ID
	}
	out := make([]VoiceOption, len(d.voices))
	copy(out, d.voices)
	return out
}

func (d *qiniuDriver) status() ProviderStatus {
	if d == nil {
		return ProviderStatus{
			ID:              "qiniu-openai",
			Label:           "七牛云 OpenAI",
			Enabled:         false,
			SupportsPreview: true,
		}
	}
	defaultVoice := d.defaultVoice
	if defaultVoice == "" && len(d.voices) > 0 {
		defaultVoice = d.voices[0].ID
	}
	return ProviderStatus{
		ID:              d.ProviderID(),
		Label:           "七牛云 OpenAI",
		Enabled:         d.Enabled(),
		DefaultVoiceID:  defaultVoice,
		SupportsPreview: true,
	}
}

func (d *qiniuDriver) Synthesize(ctx context.Context, req SpeechRequest) (*SpeechResult, error) {
	if d == nil || !d.Enabled() {
		return nil, ErrDisabled
	}

	textValue := strings.TrimSpace(req.Text)
	if textValue == "" {
		return nil, errors.New("tts: text cannot be empty")
	}

	if normalized := normalizeSpeechText(textValue); normalized != "" {
		textValue = normalized
	}

	requestedVoice := strings.TrimSpace(req.VoiceID)
	format := strings.TrimSpace(req.Format)
	if format == "" {
		format = d.responseFormat
	}

	speed := req.Speed
	if speed <= 0 {
		speed = 1.0
	}

	pitch := req.Pitch
	if pitch <= 0 {
		pitch = 1.0
	}

	emotion := strings.TrimSpace(req.Emotion)

	candidateVoices := orderedDistinct([]string{
		requestedVoice,
		d.defaultVoice,
	})
	candidateVoices = append(candidateVoices, "")

	var lastErr error
	for _, candidateVoice := range candidateVoices {
		audioPayload := map[string]any{
			"encoding":    format,
			"speed_ratio": speed,
		}
		if pitch != 1.0 {
			audioPayload["pitch_ratio"] = pitch
		}
		if strings.TrimSpace(candidateVoice) != "" {
			audioPayload["voice_type"] = strings.TrimSpace(candidateVoice)
		}

		requestPayload := map[string]any{
			"text": textValue,
		}
		if emotion != "" {
			requestPayload["emotion"] = emotion
		}

		payload := map[string]any{
			"audio":   audioPayload,
			"request": requestPayload,
		}
		if d.model != "" {
			payload["model"] = d.model
		}

		body, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("tts: encode request: %w", err)
		}

		resp, err := d.requestWithFallback(ctx, body)
		if err != nil {
			lastErr = err
			continue
		}

		responseBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("tts: read response: %w", readErr)
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			if invalid, msg := isInvalidVoiceError(resp.StatusCode, responseBody); invalid {
				lastErr = fmt.Errorf("tts: %s", msg)
				log.Printf("tts: provider rejected voice %q with %s; trying fallback", candidateVoice, msg)
				continue
			}

			snippet := strings.TrimSpace(string(responseBody))
			if len(snippet) > 4096 {
				snippet = snippet[:4096]
			}
			return nil, fmt.Errorf("tts: remote error %s: %s", resp.Status, snippet)
		}

		audioBytes, mime, err := d.processTTSResponse(responseBody, format, resp.Header.Get("Content-Type"))
		if err != nil {
			return nil, err
		}

		finalVoice := candidateVoice
		if strings.TrimSpace(finalVoice) == "" {
			finalVoice = requestedVoice
			if strings.TrimSpace(finalVoice) == "" {
				finalVoice = d.defaultVoice
			}
		}

		result := &SpeechResult{
			VoiceID:     finalVoice,
			Emotion:     emotion,
			AudioBase64: base64.StdEncoding.EncodeToString(audioBytes),
			MimeType:    mime,
			Speed:       speed,
			Pitch:       pitch,
			Provider:    d.ProviderID(),
		}

		return result, nil
	}

	if lastErr != nil {
		return nil, lastErr
	}

	return nil, errors.New("tts: no valid voices available")
}

func (d *qiniuDriver) requestWithFallback(ctx context.Context, body []byte) (*http.Response, error) {
	if d == nil {
		return nil, ErrDisabled
	}

	bases := []string{d.baseURL}
	if d.backupBaseURL != "" && !strings.EqualFold(d.backupBaseURL, d.baseURL) {
		bases = append(bases, d.backupBaseURL)
	}

	client := d.httpClient
	if client == nil {
		client = http.DefaultClient
	}

	var lastErr error
	for idx, base := range bases {
		if strings.TrimSpace(base) == "" {
			continue
		}
		resp, err := d.doRequest(ctx, client, base, body)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if idx == 1 {
				log.Printf("tts: qiniu primary base url failed, fallback %s succeeded", base)
			}
			return resp, nil
		}
		if resp.StatusCode >= 500 && idx+1 < len(bases) {
			resp.Body.Close()
			lastErr = fmt.Errorf("tts: remote error %s", resp.Status)
			continue
		}
		return resp, nil
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("tts: request failed")
}

func (d *qiniuDriver) doRequest(ctx context.Context, client *http.Client, base string, body []byte) (*http.Response, error) {
	endpoint := strings.TrimRight(base, "/") + "/voice/tts"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("tts: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+d.apiKey)
	return client.Do(req)
}

func (d *qiniuDriver) processTTSResponse(body []byte, fallbackFormat, contentType string) ([]byte, string, error) {
	trimmedContentType := strings.ToLower(strings.TrimSpace(contentType))
	if strings.Contains(trimmedContentType, "json") || (len(body) > 0 && (body[0] == '{' || body[0] == '[')) {
		audioBytes, encoding, err := decodeAudioFromJSON(body)
		if err != nil {
			return nil, "", err
		}
		mime := encodingToMime(encoding)
		if mime == "" {
			mime = encodingToMime(fallbackFormat)
		}
		if mime == "" {
			mime = "audio/mpeg"
		}
		return audioBytes, mime, nil
	}

	mime := strings.TrimSpace(contentType)
	if mime == "" {
		mime = encodingToMime(fallbackFormat)
	}
	if mime == "" {
		mime = "audio/mpeg"
	}

	return body, mime, nil
}

func (d *qiniuDriver) fetchVoiceCatalog(ctx context.Context) []VoiceOption {
	bases := orderedDistinct([]string{d.baseURL, d.backupBaseURL})
	client := d.httpClient
	if client == nil {
		client = http.DefaultClient
	}

	for _, base := range bases {
		if strings.TrimSpace(base) == "" {
			continue
		}

		endpoint := strings.TrimRight(base, "/") + "/voice/list"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			log.Printf("tts: build voice list request failed: %v", err)
			continue
		}
		if trimmed := strings.TrimSpace(d.apiKey); trimmed != "" {
			req.Header.Set("Authorization", "Bearer "+trimmed)
		}

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("tts: fetch voice list failed: %v", err)
			continue
		}

		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			log.Printf("tts: read voice list response failed: %v", readErr)
			continue
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			snippet := strings.TrimSpace(string(body))
			if len(snippet) > 512 {
				snippet = snippet[:512]
			}
			log.Printf("tts: voice list request to %s failed: %s %s", endpoint, resp.Status, snippet)
			continue
		}

		var remote []struct {
			VoiceName string `json:"voice_name"`
			VoiceType string `json:"voice_type"`
			URL       string `json:"url"`
			Category  string `json:"category"`
		}

		if err := json.Unmarshal(body, &remote); err != nil {
			log.Printf("tts: parse voice list failed: %v", err)
			continue
		}

		voices := make([]VoiceOption, 0, len(remote))
		for _, item := range remote {
			voiceID := strings.TrimSpace(item.VoiceType)
			if voiceID == "" {
				continue
			}

			voices = append(voices, VoiceOption{
				ID:           voiceID,
				Name:         strings.TrimSpace(item.VoiceName),
				Provider:     d.ProviderID(),
				Language:     "zh-CN",
				Description:  strings.TrimSpace(item.Category),
				SampleURL:    strings.TrimSpace(item.URL),
				DefaultStyle: "conversation",
				Settings: VoiceSettings{
					SpeedRange:      [2]float64{0.5, 1.5},
					PitchRange:      [2]float64{0.8, 1.2},
					DefaultSpeed:    1.0,
					DefaultPitch:    1.0,
					SupportsEmotion: false,
				},
			})
		}

		if len(voices) > 0 {
			return voices
		}
	}

	return nil
}

func loadVoiceCatalogFromEnv() []VoiceOption {
	envKeys := []string{
		"TTS_VOICE_CATALOG",
		"VOICE_CATALOG",
	}
	for _, key := range envKeys {
		raw := strings.TrimSpace(os.Getenv(key))
		if raw == "" {
			continue
		}
		voices, err := parseVoiceCatalogPayload([]byte(raw))
		if err != nil {
			log.Printf("tts: failed to parse %s: %v", key, err)
			continue
		}
		if len(voices) > 0 {
			return voices
		}
	}

	urlKeys := []string{
		"TTS_VOICE_CATALOG_URL",
		"VOICE_CATALOG_URL",
	}
	for _, key := range urlKeys {
		if voices := fetchVoiceCatalogFromURL(os.Getenv(key), key); len(voices) > 0 {
			return voices
		}
	}

	return nil
}

func defaultQiniuVoiceCatalog() []VoiceOption {
	baseSettings := VoiceSettings{
		SpeedRange:      [2]float64{0.5, 1.5},
		PitchRange:      [2]float64{0.8, 1.2},
		DefaultSpeed:    1.0,
		DefaultPitch:    1.0,
		SupportsEmotion: false,
	}

	qiniuCatalogEntries := []VoiceOption{
		{
			ID:           "qiniu_en_female_azyy",
			Name:         "\u6fb3\u6d32\u82f1\u8bed\u5973",
			Provider:     "qiniu-openai",
			Language:     "en",
			Description:  "\u53cc\u8bed\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_en_female_azyy.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_en_female_msyyn",
			Name:         "\u7f8e\u5f0f\u82f1\u8bed\u5973",
			Provider:     "qiniu-openai",
			Language:     "en",
			Description:  "\u53cc\u8bed\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_en_female_msyyn.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_en_female_ysyyn",
			Name:         "\u82f1\u5f0f\u82f1\u8bed\u5973",
			Provider:     "qiniu-openai",
			Language:     "en",
			Description:  "\u53cc\u8bed\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_en_female_ysyyn.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_en_male_azyyn",
			Name:         "\u6fb3\u6d32\u82f1\u8bed\u7537",
			Provider:     "qiniu-openai",
			Language:     "en",
			Description:  "\u53cc\u8bed\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_en_male_azyyn.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_en_male_msyyn",
			Name:         "\u7f8e\u5f0f\u82f1\u8bed\u7537",
			Provider:     "qiniu-openai",
			Language:     "en",
			Description:  "\u53cc\u8bed\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_en_male_msyyn.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_en_male_ysyyn",
			Name:         "\u82f1\u5f0f\u82f1\u8bed\u7537",
			Provider:     "qiniu-openai",
			Language:     "en",
			Description:  "\u53cc\u8bed\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_en_male_ysyyn.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_multi_female_rxsyn1",
			Name:         "\u65e5\u897f\u53cc\u8bed\u59731",
			Provider:     "qiniu-openai",
			Language:     "ja,es",
			Description:  "\u53cc\u8bed\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_multi_female_rxsyn1.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_multi_female_rxsyn2",
			Name:         "\u65e5\u897f\u53cc\u8bed\u59732",
			Provider:     "qiniu-openai",
			Language:     "ja,es",
			Description:  "\u53cc\u8bed\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_multi_female_rxsyn2.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_multi_male_rxsyn1",
			Name:         "\u65e5\u897f\u53cc\u8bed\u75371",
			Provider:     "qiniu-openai",
			Language:     "ja,es",
			Description:  "\u53cc\u8bed\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_multi_male_rxsyn1.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_multi_male_rxsyn2",
			Name:         "\u65e5\u897f\u53cc\u8bed\u75372",
			Provider:     "qiniu-openai",
			Language:     "ja,es",
			Description:  "\u53cc\u8bed\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_multi_male_rxsyn2.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_zh_female_cxjxgw",
			Name:         "\u6148\u7965\u6559\u5b66\u987e\u95ee",
			Provider:     "qiniu-openai",
			Language:     "zh-CN",
			Description:  "\u7279\u6b8a\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_zh_female_cxjxgw.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_zh_female_dmytwz",
			Name:         "\u52a8\u6f2b\u6a31\u6843\u4e38\u5b50",
			Provider:     "qiniu-openai",
			Language:     "zh-CN",
			Description:  "\u7279\u6b8a\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_zh_female_dmytwz.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_zh_female_glktss",
			Name:         "\u5e72\u7ec3\u8bfe\u5802\u601d\u601d",
			Provider:     "qiniu-openai",
			Language:     "zh-CN",
			Description:  "\u4f20\u7edf\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_zh_female_glktss.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_zh_female_kljxdd",
			Name:         "\u5f00\u6717\u6559\u5b66\u7763\u5bfc",
			Provider:     "qiniu-openai",
			Language:     "zh-CN",
			Description:  "\u4f20\u7edf\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_zh_female_kljxdd.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_zh_female_ljfdxx",
			Name:         "\u90bb\u5bb6\u8f85\u5bfc\u5b66\u59d0",
			Provider:     "qiniu-openai",
			Language:     "zh-CN",
			Description:  "\u4f20\u7edf\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_zh_female_ljfdxx.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_zh_female_qwzscb",
			Name:         "\u8da3\u5473\u77e5\u8bc6\u4f20\u64ad",
			Provider:     "qiniu-openai",
			Language:     "zh-CN",
			Description:  "\u7279\u6b8a\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_zh_female_qwzscb.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_zh_female_segsby",
			Name:         "\u5c11\u513f\u6545\u4e8b\u914d\u97f3",
			Provider:     "qiniu-openai",
			Language:     "zh-CN",
			Description:  "\u7279\u6b8a\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_zh_female_segsby.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_zh_female_sqjyay",
			Name:         "\u793e\u533a\u6559\u80b2\u963f\u59e8",
			Provider:     "qiniu-openai",
			Language:     "zh-CN",
			Description:  "\u7279\u6b8a\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_zh_female_sqjyay.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_zh_female_tmjxxy",
			Name:         "\u751c\u7f8e\u6559\u5b66\u5c0f\u6e90",
			Provider:     "qiniu-openai",
			Language:     "zh-CN",
			Description:  "\u4f20\u7edf\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_zh_female_tmjxxy.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_zh_female_wwkjby",
			Name:         "\u6e29\u5a49\u8bfe\u4ef6\u914d\u97f3",
			Provider:     "qiniu-openai",
			Language:     "zh-CN",
			Description:  "\u7279\u6b8a\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_zh_female_wwkjby.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_zh_female_wwxkjx",
			Name:         "\u6e29\u5a49\u5b66\u79d1\u8bb2\u5e08",
			Provider:     "qiniu-openai",
			Language:     "zh-CN",
			Description:  "\u4f20\u7edf\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_zh_female_wwxkjx.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_zh_female_xyqxxj",
			Name:         "\u6821\u56ed\u6e05\u65b0\u5b66\u59d0",
			Provider:     "qiniu-openai",
			Language:     "zh-CN",
			Description:  "\u4f20\u7edf\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_zh_female_xyqxxj.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_zh_female_yyqmpq",
			Name:         "\u82f1\u8bed\u542f\u8499\u4f69\u5947",
			Provider:     "qiniu-openai",
			Language:     "zh-CN",
			Description:  "\u7279\u6b8a\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_zh_female_yyqmpq.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_zh_female_zxjxnjs",
			Name:         "\u77e5\u6027\u6559\u5b66\u5973\u6559\u5e08",
			Provider:     "qiniu-openai",
			Language:     "zh-CN",
			Description:  "\u4f20\u7edf\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_zh_female_zxjxnjs.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_zh_male_cxkjns",
			Name:         "\u78c1\u6027\u8bfe\u4ef6\u7537\u58f0",
			Provider:     "qiniu-openai",
			Language:     "zh-CN",
			Description:  "\u7279\u6b8a\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_zh_male_cxkjns.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_zh_male_etgsxe",
			Name:         "\u513f\u7ae5\u6545\u4e8b\u718a\u4e8c",
			Provider:     "qiniu-openai",
			Language:     "zh-CN",
			Description:  "\u7279\u6b8a\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_zh_male_etgsxe.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_zh_male_gzjjxb",
			Name:         "\u53e4\u88c5\u5267\u6559\u5b66\u7248",
			Provider:     "qiniu-openai",
			Language:     "zh-CN",
			Description:  "\u7279\u6b8a\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_zh_male_gzjjxb.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_zh_male_hllzmz",
			Name:         "\u6d3b\u529b\u7387\u771f\u840c\u4ed4",
			Provider:     "qiniu-openai",
			Language:     "zh-CN",
			Description:  "\u7279\u6b8a\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_zh_male_hllzmz.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_zh_male_hlsnkk",
			Name:         "\u706b\u529b\u5c11\u5e74\u51ef\u51ef",
			Provider:     "qiniu-openai",
			Language:     "zh-CN",
			Description:  "\u4f20\u7edf\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_zh_male_hlsnkk.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_zh_male_ljfdxz",
			Name:         "\u90bb\u5bb6\u8f85\u5bfc\u5b66\u957f",
			Provider:     "qiniu-openai",
			Language:     "zh-CN",
			Description:  "\u4f20\u7edf\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_zh_male_ljfdxz.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_zh_male_mzjsxg",
			Name:         "\u540d\u8457\u89d2\u8272\u7334\u54e5",
			Provider:     "qiniu-openai",
			Language:     "zh-CN",
			Description:  "\u7279\u6b8a\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_zh_male_mzjsxg.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_zh_male_qslymb",
			Name:         "\u8f7b\u677e\u61d2\u97f3\u7ef5\u5b9d",
			Provider:     "qiniu-openai",
			Language:     "zh-CN",
			Description:  "\u7279\u6b8a\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_zh_male_qslymb.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_zh_male_szxyxd",
			Name:         "\u7387\u771f\u6821\u56ed\u5411\u5bfc",
			Provider:     "qiniu-openai",
			Language:     "zh-CN",
			Description:  "\u4f20\u7edf\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_zh_male_szxyxd.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_zh_male_tcsnsf",
			Name:         "\u5929\u624d\u5c11\u5e74\u793a\u8303",
			Provider:     "qiniu-openai",
			Language:     "zh-CN",
			Description:  "\u7279\u6b8a\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_zh_male_tcsnsf.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_zh_male_tyygjs",
			Name:         "\u901a\u7528\u9633\u5149\u8bb2\u5e08",
			Provider:     "qiniu-openai",
			Language:     "zh-CN",
			Description:  "\u4f20\u7edf\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_zh_male_tyygjs.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_zh_male_whxkxg",
			Name:         "\u6e29\u548c\u5b66\u79d1\u5c0f\u54e5",
			Provider:     "qiniu-openai",
			Language:     "zh-CN",
			Description:  "\u4f20\u7edf\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_zh_male_whxkxg.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_zh_male_wncwxz",
			Name:         "\u6e29\u6696\u6c89\u7a33\u5b66\u957f",
			Provider:     "qiniu-openai",
			Language:     "zh-CN",
			Description:  "\u4f20\u7edf\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_zh_male_wncwxz.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
		{
			ID:           "qiniu_zh_male_ybxknjs",
			Name:         "\u6e0a\u535a\u5b66\u79d1\u7537\u6559\u5e08",
			Provider:     "qiniu-openai",
			Language:     "zh-CN",
			Description:  "\u4f20\u7edf\u97f3\u8272",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_zh_male_ybxknjs.mp3",
			DefaultStyle: "conversation",
			Settings:     baseSettings,
		},
	}

	voices := make([]VoiceOption, len(qiniuCatalogEntries))
	copy(voices, qiniuCatalogEntries)
	return voices
}

func fetchVoiceCatalogFromURL(target, sourceLabel string) []VoiceOption {
	trimmed := strings.TrimSpace(target)
	if trimmed == "" {
		return nil
	}

	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Get(trimmed)
	if err != nil {
		label := strings.TrimSpace(sourceLabel)
		if label == "" {
			label = trimmed
		}
		log.Printf("tts: fetch voice catalog from %s failed: %v", label, err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		label := strings.TrimSpace(sourceLabel)
		if label == "" {
			label = trimmed
		}
		log.Printf("tts: voice catalog request to %s failed: %s %s", label, resp.Status, strings.TrimSpace(string(snippet)))
		return nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		label := strings.TrimSpace(sourceLabel)
		if label == "" {
			label = trimmed
		}
		log.Printf("tts: read voice catalog response from %s failed: %v", label, err)
		return nil
	}

	voices, err := parseVoiceCatalogPayload(body)
	if err != nil {
		label := strings.TrimSpace(sourceLabel)
		if label == "" {
			label = trimmed
		}
		log.Printf("tts: parse voice catalog from %s failed: %v", label, err)
		return nil
	}

	return voices
}

func parseVoiceCatalogPayload(data []byte) ([]VoiceOption, error) {
	var direct []VoiceOption
	directErr := json.Unmarshal(data, &direct)
	if directErr == nil && len(direct) > 0 {
		return direct, nil
	}

	var wrapper struct {
		Voices []VoiceOption `json:"voices"`
	}
	wrapperErr := json.Unmarshal(data, &wrapper)
	if wrapperErr == nil && len(wrapper.Voices) > 0 {
		return wrapper.Voices, nil
	}

	if directErr != nil {
		return nil, directErr
	}
	if wrapperErr != nil {
		return nil, wrapperErr
	}

	return nil, fmt.Errorf("voice catalog payload is empty")
}

type cosyVoiceDriver struct {
	endpoint       string
	apiKey         string
	workspace      string
	dataInspection string
	model          string
	defaultVoice   string
	format         string
	sampleRate     int
	volume         int
	timeout        time.Duration
	providerID     string
	voices         []VoiceOption
	enabled        bool
}

func newCosyVoiceDriverFromEnv() *cosyVoiceDriver {
	endpoint := strings.TrimSpace(firstNonEmpty(
		os.Getenv("COSYVOICE_WS_URL"),
		os.Getenv("TTS_COSYVOICE_WS_URL"),
		os.Getenv("TTS_COSYVOICE_ENDPOINT"),
	))
	if endpoint == "" {
		endpoint = "wss://dashscope.aliyuncs.com/api-ws/v1/inference"
	}

	apiKey := strings.TrimSpace(firstNonEmpty(
		os.Getenv("COSYVOICE_API_KEY"),
		os.Getenv("TTS_COSYVOICE_API_KEY"),
		os.Getenv("ALIYUN_COSYVOICE_API_KEY"),
		os.Getenv("DASHSCOPE_API_KEY"),
		os.Getenv("ALIYUN_API_KEY"),
	))

	workspace := strings.TrimSpace(firstNonEmpty(
		os.Getenv("COSYVOICE_WORKSPACE"),
		os.Getenv("TTS_COSYVOICE_WORKSPACE"),
	))

	dataInspection := strings.TrimSpace(firstNonEmpty(
		os.Getenv("COSYVOICE_DATA_INSPECTION"),
		os.Getenv("TTS_COSYVOICE_DATA_INSPECTION"),
	))

	model := strings.TrimSpace(firstNonEmpty(
		os.Getenv("COSYVOICE_MODEL"),
		os.Getenv("TTS_COSYVOICE_MODEL"),
	))
	if model == "" {
		model = "cosyvoice-v1"
	}

	defaultVoice := strings.TrimSpace(firstNonEmpty(
		os.Getenv("COSYVOICE_DEFAULT_VOICE"),
		os.Getenv("TTS_COSYVOICE_DEFAULT_VOICE"),
	))
	if defaultVoice == "" {
		defaultVoice = "longwan"
	}

	format := strings.TrimSpace(firstNonEmpty(
		os.Getenv("COSYVOICE_FORMAT"),
		os.Getenv("TTS_COSYVOICE_FORMAT"),
	))
	if format == "" {
		format = "mp3"
	}

	sampleRate := 22050
	if raw := strings.TrimSpace(firstNonEmpty(
		os.Getenv("COSYVOICE_SAMPLE_RATE"),
		os.Getenv("TTS_COSYVOICE_SAMPLE_RATE"),
	)); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			sampleRate = parsed
		}
	}

	volume := 50
	if raw := strings.TrimSpace(firstNonEmpty(
		os.Getenv("COSYVOICE_VOLUME"),
		os.Getenv("TTS_COSYVOICE_VOLUME"),
	)); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 0 && parsed <= 100 {
			volume = parsed
		}
	}

	return &cosyVoiceDriver{
		endpoint:       endpoint,
		apiKey:         apiKey,
		workspace:      workspace,
		dataInspection: dataInspection,
		model:          model,
		defaultVoice:   defaultVoice,
		format:         format,
		sampleRate:     sampleRate,
		volume:         volume,
		timeout:        75 * time.Second,
		providerID:     "aliyun-cosyvoice",
		enabled:        apiKey != "",
	}
}

func (d *cosyVoiceDriver) ProviderID() string {
	if d == nil {
		return "aliyun-cosyvoice"
	}
	return d.providerID
}

func (d *cosyVoiceDriver) Enabled() bool {
	return d != nil && d.enabled
}

func (d *cosyVoiceDriver) DefaultVoiceID() string {
	if d == nil {
		return ""
	}
	return d.defaultVoice
}

func (d *cosyVoiceDriver) ensureVoices() []VoiceOption {
	if d == nil {
		return nil
	}
	if len(d.voices) == 0 {
		if custom := loadCosyVoiceCatalogFromEnv(); len(custom) > 0 {
			filtered := make([]VoiceOption, 0, len(custom))
			for _, voice := range custom {
				if NormalizeProviderID(voice.Provider) == d.ProviderID() || strings.TrimSpace(voice.Provider) == "" {
					voice.Provider = d.ProviderID()
					filtered = append(filtered, voice)
				}
			}
			if len(filtered) > 0 {
				d.voices = filtered
			}
		}
	}
	if len(d.voices) == 0 {
		d.voices = defaultCosyVoiceCatalog()
	}
	if d.defaultVoice == "" && len(d.voices) > 0 {
		d.defaultVoice = d.voices[0].ID
	} else if d.defaultVoice != "" && !containsVoiceID(d.voices, d.defaultVoice) && len(d.voices) > 0 {
		log.Printf("tts: cosyvoice default voice %q not found; fallback to %q", d.defaultVoice, d.voices[0].ID)
		d.defaultVoice = d.voices[0].ID
	}
	out := make([]VoiceOption, len(d.voices))
	copy(out, d.voices)
	return out
}

func (d *cosyVoiceDriver) status() ProviderStatus {
	if d == nil {
		return ProviderStatus{
			ID:              "aliyun-cosyvoice",
			Label:           "阿里云 CosyVoice",
			Enabled:         false,
			SupportsPreview: true,
		}
	}
	defaultVoice := d.defaultVoice
	if defaultVoice == "" && len(d.voices) > 0 {
		defaultVoice = d.voices[0].ID
	}
	return ProviderStatus{
		ID:              d.ProviderID(),
		Label:           "阿里云 CosyVoice",
		Enabled:         d.Enabled(),
		DefaultVoiceID:  defaultVoice,
		SupportsPreview: true,
	}
}

func (d *cosyVoiceDriver) Synthesize(ctx context.Context, req SpeechRequest) (*SpeechResult, error) {
	if d == nil || !d.Enabled() {
		return nil, ErrDisabled
	}
	if strings.TrimSpace(d.apiKey) == "" {
		return nil, ErrDisabled
	}

	textValue := strings.TrimSpace(req.Text)
	if textValue == "" {
		return nil, errors.New("tts: text cannot be empty")
	}

	if normalized := normalizeSpeechText(textValue); normalized != "" {
		textValue = normalized
	}

	voiceID := strings.TrimSpace(req.VoiceID)
	if voiceID == "" {
		voiceID = d.defaultVoice
	}
	if voiceID == "" {
		return nil, errors.New("tts: cosyvoice default voice not configured")
	}

	format := strings.TrimSpace(req.Format)
	if format == "" {
		format = d.format
	}
	if format == "" {
		format = "mp3"
	}

	sampleRate := d.sampleRate
	model := d.model
	if req.ResolvedVoice != nil {
		if v := strings.TrimSpace(req.ResolvedVoice.Format); v != "" {
			format = v
		}
		if req.ResolvedVoice.SampleRate > 0 {
			sampleRate = req.ResolvedVoice.SampleRate
		}
		if v := strings.TrimSpace(req.ResolvedVoice.Model); v != "" {
			model = v
		}
	}
	if model == "" {
		model = "cosyvoice-v3"
	}
	if sampleRate <= 0 {
		sampleRate = 22050
	}

	speed := req.Speed
	if speed <= 0 {
		speed = 1.0
	}
	if speed < 0.5 {
		speed = 0.5
	}
	if speed > 1.6 {
		speed = 1.6
	}

	pitch := req.Pitch
	if pitch <= 0 {
		pitch = 1.0
	}
	if pitch < 0.7 {
		pitch = 0.7
	}
	if pitch > 1.4 {
		pitch = 1.4
	}

	header := http.Header{}
	header.Set("Authorization", "bearer "+d.apiKey)
	header.Set("User-Agent", "auralis-cosyvoice-client/1.0")
	if d.workspace != "" {
		header.Set("X-DashScope-WorkSpace", d.workspace)
	}
	if d.dataInspection != "" {
		header.Set("X-DashScope-DataInspection", d.dataInspection)
	}

	dialer := websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: 8 * time.Second,
	}

	conn, resp, err := dialer.DialContext(ctx, d.endpoint, header)
	if err != nil {
		if resp != nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			resp.Body.Close()
			if len(body) > 0 {
				return nil, fmt.Errorf("tts: cosyvoice connect failed: %v (%s)", err, strings.TrimSpace(string(body)))
			}
		}
		return nil, fmt.Errorf("tts: cosyvoice connect failed: %w", err)
	}
	defer conn.Close()

	taskID := uuid.NewString()

	runPayload := map[string]any{
		"header": map[string]any{
			"action":    "run-task",
			"task_id":   taskID,
			"streaming": "duplex",
		},
		"payload": map[string]any{
			"task_group": "audio",
			"task":       "tts",
			"function":   "SpeechSynthesizer",
			"model":      model,
			"parameters": map[string]any{
				"text_type":   "PlainText",
				"voice":       voiceID,
				"format":      strings.ToLower(format),
				"sample_rate": sampleRate,
				"volume":      d.volume,
				"rate":        speed,
				"pitch":       pitch,
			},
			"input": map[string]any{},
		},
	}

	if strings.TrimSpace(req.Instructions) != "" {
		runPayload["payload"].(map[string]any)["parameters"].(map[string]any)["instruction"] = strings.TrimSpace(req.Instructions)
	}
	if strings.TrimSpace(req.Emotion) != "" {
		runPayload["payload"].(map[string]any)["parameters"].(map[string]any)["emotion"] = strings.TrimSpace(req.Emotion)
	}

	if err := conn.WriteJSON(runPayload); err != nil {
		return nil, fmt.Errorf("tts: cosyvoice run-task failed: %w", err)
	}

	audioBuf := &bytes.Buffer{}

	if err := d.waitForCosyEvent(ctx, conn, taskID, "task-started", audioBuf); err != nil {
		return nil, err
	}

	continuePayload := map[string]any{
		"header": map[string]any{
			"action":    "continue-task",
			"task_id":   taskID,
			"streaming": "duplex",
		},
		"payload": map[string]any{
			"input": map[string]any{
				"text": textValue,
			},
		},
	}

	if err := conn.WriteJSON(continuePayload); err != nil {
		return nil, fmt.Errorf("tts: cosyvoice continue-task failed: %w", err)
	}

	finishPayload := map[string]any{
		"header": map[string]any{
			"action":    "finish-task",
			"task_id":   taskID,
			"streaming": "duplex",
		},
		"payload": map[string]any{
			"input": map[string]any{},
		},
	}

	if err := conn.WriteJSON(finishPayload); err != nil {
		return nil, fmt.Errorf("tts: cosyvoice finish-task failed: %w", err)
	}

	if err := d.waitForCosyEvent(ctx, conn, taskID, "task-finished", audioBuf); err != nil {
		return nil, err
	}

	audioBytes := audioBuf.Bytes()
	if len(audioBytes) == 0 {
		return nil, errors.New("tts: cosyvoice returned empty audio")
	}

	mime := encodingToMime(format)
	if mime == "" {
		mime = "audio/mpeg"
	}

	result := &SpeechResult{
		VoiceID:     voiceID,
		Emotion:     strings.TrimSpace(req.Emotion),
		AudioBase64: base64.StdEncoding.EncodeToString(audioBytes),
		MimeType:    mime,
		Speed:       speed,
		Pitch:       pitch,
		Provider:    d.ProviderID(),
	}

	return result, nil
}

func (d *cosyVoiceDriver) waitForCosyEvent(ctx context.Context, conn *websocket.Conn, taskID string, target string, audioBuf *bytes.Buffer) error {
	target = strings.ToLower(strings.TrimSpace(target))
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.timeout > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(d.timeout))
		}
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("tts: cosyvoice read failed: %w", err)
		}
		if msgType == websocket.BinaryMessage {
			if len(data) > 0 && audioBuf != nil {
				if _, err := audioBuf.Write(data); err != nil {
					return fmt.Errorf("tts: cosyvoice buffer audio: %w", err)
				}
			}
			continue
		}
		if msgType != websocket.TextMessage {
			continue
		}
		var event cosyVoiceEvent
		if err := json.Unmarshal(data, &event); err != nil {
			log.Printf("tts: cosyvoice parse event failed: %v", err)
			continue
		}
		if taskID != "" && event.Header.TaskID != "" && !strings.EqualFold(event.Header.TaskID, taskID) {
			continue
		}

		eventType := strings.ToLower(strings.TrimSpace(event.Header.Event))
		switch eventType {
		case "task-failed":
			message := strings.TrimSpace(event.Header.ErrorMessage)
			if message == "" {
				message = "unknown error"
			}
			return fmt.Errorf("tts: cosyvoice task failed: %s (%s)", message, event.Header.ErrorCode)
		case target:
			return nil
		default:
			// ignore
		}
	}
}

type cosyVoiceEvent struct {
	Header struct {
		TaskID       string `json:"task_id"`
		Event        string `json:"event"`
		ErrorCode    string `json:"error_code"`
		ErrorMessage string `json:"error_message"`
	} `json:"header"`
}

func loadCosyVoiceCatalogFromEnv() []VoiceOption {
	envKeys := []string{
		"COSYVOICE_VOICE_CATALOG",
		"TTS_COSYVOICE_VOICE_CATALOG",
	}
	for _, key := range envKeys {
		raw := strings.TrimSpace(os.Getenv(key))
		if raw == "" {
			continue
		}
		voices, err := parseVoiceCatalogPayload([]byte(raw))
		if err != nil {
			log.Printf("tts: failed to parse %s: %v", key, err)
			continue
		}
		if len(voices) > 0 {
			return voices
		}
	}

	urlKeys := []string{
		"COSYVOICE_VOICE_CATALOG_URL",
		"TTS_COSYVOICE_VOICE_CATALOG_URL",
	}
	for _, key := range urlKeys {
		if voices := fetchVoiceCatalogFromURL(os.Getenv(key), key); len(voices) > 0 {
			return voices
		}
	}

	return nil
}

func defaultCosyVoiceCatalog() []VoiceOption {
	baseSettings := VoiceSettings{
		SpeedRange:      [2]float64{0.6, 1.6},
		PitchRange:      [2]float64{0.8, 1.2},
		DefaultSpeed:    1.0,
		DefaultPitch:    1.0,
		SupportsEmotion: false,
	}

	voices := []VoiceOption{
		{
			ID:           "longwan",
			Name:         "Longwan (\u9f99\u5a49)",
			Provider:     "aliyun-cosyvoice",
			Language:     "zh-CN",
			Description:  "Voice assistant, navigation guidance, chat avatar.",
			DefaultStyle: "general",
			Model:        "cosyvoice-v1",
			Format:       "mp3",
			SampleRate:   22050,
			Settings:     baseSettings,
		},
		{
			ID:           "longcheng",
			Name:         "Longcheng (\u9f99\u6a59)",
			Provider:     "aliyun-cosyvoice",
			Language:     "zh-CN",
			Description:  "Voice assistant, navigation guidance, chat avatar.",
			DefaultStyle: "general",
			Model:        "cosyvoice-v1",
			Format:       "mp3",
			SampleRate:   22050,
			Settings:     baseSettings,
		},
		{
			ID:           "longhua",
			Name:         "Longhua (\u9f99\u534e)",
			Provider:     "aliyun-cosyvoice",
			Language:     "zh-CN",
			Description:  "Voice assistant, navigation guidance, chat avatar.",
			DefaultStyle: "general",
			Model:        "cosyvoice-v1",
			Format:       "mp3",
			SampleRate:   22050,
			Settings:     baseSettings,
		},
		{
			ID:           "longxiaochun",
			Name:         "Longxiaochun (\u9f99\u5c0f\u6df3)",
			Provider:     "aliyun-cosyvoice",
			Language:     "zh-CN,en",
			Description:  "Voice assistant, navigation guidance, chat avatar.",
			DefaultStyle: "general",
			Model:        "cosyvoice-v1",
			Format:       "mp3",
			SampleRate:   22050,
			Settings:     baseSettings,
		},
		{
			ID:           "longxiaoxia",
			Name:         "Longxiaoxia (\u9f99\u5c0f\u590f)",
			Provider:     "aliyun-cosyvoice",
			Language:     "zh-CN",
			Description:  "Voice assistant, chat avatar.",
			DefaultStyle: "general",
			Model:        "cosyvoice-v1",
			Format:       "mp3",
			SampleRate:   22050,
			Settings:     baseSettings,
		},
		{
			ID:           "longxiaocheng",
			Name:         "Longxiaocheng (\u9f99\u5c0f\u8bda)",
			Provider:     "aliyun-cosyvoice",
			Language:     "zh-CN,en",
			Description:  "Voice assistant, navigation guidance, chat avatar.",
			DefaultStyle: "general",
			Model:        "cosyvoice-v1",
			Format:       "mp3",
			SampleRate:   22050,
			Settings:     baseSettings,
		},
		{
			ID:           "longxiaobai",
			Name:         "Longxiaobai (\u9f99\u5c0f\u767d)",
			Provider:     "aliyun-cosyvoice",
			Language:     "zh-CN",
			Description:  "Chat avatar, audiobooks, voice assistant.",
			DefaultStyle: "narration",
			Model:        "cosyvoice-v1",
			Format:       "mp3",
			SampleRate:   22050,
			Settings:     baseSettings,
		},
		{
			ID:           "longlaotie",
			Name:         "Longlaotie (\u9f99\u8001\u94c1)",
			Provider:     "aliyun-cosyvoice",
			Language:     "zh-CN",
			Description:  "News, audiobooks, assistant, livestream, navigation with Dongbei accent.",
			DefaultStyle: "broadcast",
			Model:        "cosyvoice-v1",
			Format:       "mp3",
			SampleRate:   22050,
			Settings:     baseSettings,
		},
		{
			ID:           "longshu",
			Name:         "Longshu (\u9f99\u4e66)",
			Provider:     "aliyun-cosyvoice",
			Language:     "zh-CN",
			Description:  "Audiobooks, assistant, navigation, news, support agent.",
			DefaultStyle: "narration",
			Model:        "cosyvoice-v1",
			Format:       "mp3",
			SampleRate:   22050,
			Settings:     baseSettings,
		},
		{
			ID:           "longshuo",
			Name:         "Longshuo (\u9f99\u7855)",
			Provider:     "aliyun-cosyvoice",
			Language:     "zh-CN",
			Description:  "Assistant, navigation, news, collections.",
			DefaultStyle: "general",
			Model:        "cosyvoice-v1",
			Format:       "mp3",
			SampleRate:   22050,
			Settings:     baseSettings,
		},
		{
			ID:           "longjing",
			Name:         "Longjing (\u9f99\u5a77)",
			Provider:     "aliyun-cosyvoice",
			Language:     "zh-CN",
			Description:  "Assistant, navigation, news, collections.",
			DefaultStyle: "general",
			Model:        "cosyvoice-v1",
			Format:       "mp3",
			SampleRate:   22050,
			Settings:     baseSettings,
		},
		{
			ID:           "longmiao",
			Name:         "Longmiao (\u9f99\u5999)",
			Provider:     "aliyun-cosyvoice",
			Language:     "zh-CN",
			Description:  "Collections, navigation, audiobooks, assistant.",
			DefaultStyle: "general",
			Model:        "cosyvoice-v1",
			Format:       "mp3",
			SampleRate:   22050,
			Settings:     baseSettings,
		},
		{
			ID:           "longyue",
			Name:         "Longyue (\u9f99\u60a6)",
			Provider:     "aliyun-cosyvoice",
			Language:     "zh-CN",
			Description:  "Assistant, poetry reading, audiobooks, navigation, news, collections.",
			DefaultStyle: "narration",
			Model:        "cosyvoice-v1",
			Format:       "mp3",
			SampleRate:   22050,
			Settings:     baseSettings,
		},
		{
			ID:           "longyuan",
			Name:         "Longyuan (\u9f99\u5a9b)",
			Provider:     "aliyun-cosyvoice",
			Language:     "zh-CN",
			Description:  "Audiobooks, assistant, chat avatar.",
			DefaultStyle: "narration",
			Model:        "cosyvoice-v1",
			Format:       "mp3",
			SampleRate:   22050,
			Settings:     baseSettings,
		},
		{
			ID:           "longfei",
			Name:         "Longfei (\u9f99\u98de)",
			Provider:     "aliyun-cosyvoice",
			Language:     "zh-CN",
			Description:  "Conference narration, news, audiobooks.",
			DefaultStyle: "broadcast",
			Model:        "cosyvoice-v1",
			Format:       "mp3",
			SampleRate:   22050,
			Settings:     baseSettings,
		},
		{
			ID:           "longjielidou",
			Name:         "Longjielidou (\u9f99\u6770\u529b\u8c46)",
			Provider:     "aliyun-cosyvoice",
			Language:     "zh-CN,en",
			Description:  "News, audiobooks, chat assistant.",
			DefaultStyle: "general",
			Model:        "cosyvoice-v1",
			Format:       "mp3",
			SampleRate:   22050,
			Settings:     baseSettings,
		},
		{
			ID:           "longtong",
			Name:         "Longtong (\u9f99\u6850)",
			Provider:     "aliyun-cosyvoice",
			Language:     "zh-CN",
			Description:  "Audiobooks, navigation, chat avatar.",
			DefaultStyle: "general",
			Model:        "cosyvoice-v1",
			Format:       "mp3",
			SampleRate:   22050,
			Settings:     baseSettings,
		},
		{
			ID:           "longxiang",
			Name:         "Longxiang (\u9f99\u7965)",
			Provider:     "aliyun-cosyvoice",
			Language:     "zh-CN",
			Description:  "News, audiobooks, navigation.",
			DefaultStyle: "broadcast",
			Model:        "cosyvoice-v1",
			Format:       "mp3",
			SampleRate:   22050,
			Settings:     baseSettings,
		},
		{
			ID:           "loongstella",
			Name:         "Stella (\u9f99Stella)",
			Provider:     "aliyun-cosyvoice",
			Language:     "zh-CN,en",
			Description:  "Assistant, livestream, navigation, collections, audiobooks.",
			DefaultStyle: "general",
			Model:        "cosyvoice-v1",
			Format:       "mp3",
			SampleRate:   22050,
			Settings:     baseSettings,
		},
		{
			ID:           "loongbella",
			Name:         "Bella (\u9f99Bella)",
			Provider:     "aliyun-cosyvoice",
			Language:     "zh-CN",
			Description:  "Assistant, collections, news, navigation.",
			DefaultStyle: "general",
			Model:        "cosyvoice-v1",
			Format:       "mp3",
			SampleRate:   22050,
			Settings:     baseSettings,
		},
	}

	out := make([]VoiceOption, len(voices))
	copy(out, voices)
	return out
}

func orderedDistinct(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		key := strings.ToLower(trimmed)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, trimmed)
	}
	return out
}

func containsVoiceID(list []VoiceOption, id string) bool {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return false
	}
	for _, item := range list {
		if strings.EqualFold(item.ID, trimmed) {
			return true
		}
	}
	return false
}

func encodingToMime(encoding string) string {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "mp3", "mpeg", "audio/mpeg":
		return "audio/mpeg"
	case "wav", "wave", "audio/wav":
		return "audio/wav"
	case "pcm":
		return "audio/wave"
	case "opus", "audio/opus":
		return "audio/opus"
	default:
		return ""
	}
}

func decodeAudioFromJSON(body []byte) ([]byte, string, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, "", errors.New("tts: empty json response")
	}

	var payload struct {
		Data []struct {
			Audio    string `json:"audio"`
			Encoding string `json:"encoding"`
		} `json:"data"`
		AudioContent string `json:"audio_content"`
		Format       string `json:"format"`
	}

	if err := json.Unmarshal(trimmed, &payload); err != nil {
		return nil, "", fmt.Errorf("tts: parse json response: %w", err)
	}

	if len(payload.Data) > 0 {
		audioRaw := payload.Data[0].Audio
		bytesOut, err := base64.StdEncoding.DecodeString(audioRaw)
		if err != nil {
			return nil, "", fmt.Errorf("tts: decode audio: %w", err)
		}
		encoding := payload.Data[0].Encoding
		if encoding == "" {
			encoding = payload.Format
		}
		return bytesOut, encoding, nil
	}

	if payload.AudioContent != "" {
		bytesOut, err := base64.StdEncoding.DecodeString(payload.AudioContent)
		if err != nil {
			return nil, "", fmt.Errorf("tts: decode audio: %w", err)
		}
		return bytesOut, payload.Format, nil
	}

	return nil, "", errors.New("tts: json response missing audio content")
}

func isInvalidVoiceError(status int, body []byte) (bool, string) {
	if status < 400 {
		return false, ""
	}

	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return false, ""
	}

	var payload struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal(trimmed, &payload); err != nil {
		return false, ""
	}

	errType := strings.ToLower(strings.TrimSpace(payload.Error.Type))
	errMessage := strings.TrimSpace(payload.Error.Message)

	combined := strings.TrimSpace(errType + " " + strings.ToLower(errMessage))
	if strings.Contains(combined, "voice") && (strings.Contains(combined, "invalid") || strings.Contains(combined, "not found") || strings.Contains(combined, "unsupported")) {
		if errMessage == "" {
			errMessage = "invalid voice selection"
		}
		return true, errMessage
	}

	return false, ""
}

func NormalizeProviderID(value string) string {
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

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		trimmed := strings.TrimSpace(v)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}

var newlineToPausePattern = regexp.MustCompile(`[\r\n]+`)
var multiSpacePattern = regexp.MustCompile(`\s{2,}`)
var repeatedPausePattern = regexp.MustCompile(`([，。、！？；：]){2,}`)
var strayPunctuationPattern = regexp.MustCompile(`["'\[\]\{\}\(\)<>]+`)

var asciiPauseMapping = map[rune]rune{
	',': '，',
	'.': '。',
	'!': '！',
	'?': '？',
	';': '；',
	':': '：',
}

var preservedPauseRunes = map[rune]struct{}{
	'，': {},
	'。': {},
	'！': {},
	'？': {},
	'；': {},
	'：': {},
	'、': {},
}

func normalizeSpeechText(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}

	cleaned := newlineToPausePattern.ReplaceAllString(trimmed, "，")
	cleaned = strings.ReplaceAll(cleaned, "...", "……")
	cleaned = strayPunctuationPattern.ReplaceAllString(cleaned, " ")
	var builder strings.Builder
	builder.Grow(len(cleaned))

	var lastRune rune
	var lastRuneSet bool
	lastWasSpace := false

	for _, r := range cleaned {
		if unicode.IsControl(r) {
			continue
		}
		if mapped, ok := asciiPauseMapping[r]; ok {
			if builder.Len() == 0 || lastWasSpace {
				continue
			}
			if lastRuneSet && lastRune == mapped {
				continue
			}
			builder.WriteRune(mapped)
			lastRune = mapped
			lastRuneSet = true
			lastWasSpace = false
			continue
		}
		if _, ok := preservedPauseRunes[r]; ok {
			if builder.Len() == 0 || lastWasSpace {
				continue
			}
			if lastRuneSet && lastRune == r {
				continue
			}
			builder.WriteRune(r)
			lastRune = r
			lastRuneSet = true
			lastWasSpace = false
			continue
		}
		if unicode.IsSpace(r) {
			if builder.Len() == 0 {
				continue
			}
			if !lastWasSpace {
				builder.WriteRune(' ')
				lastRune = ' '
				lastRuneSet = true
				lastWasSpace = true
			}
			continue
		}
		if unicode.IsPunct(r) || unicode.In(r, unicode.Sm, unicode.So, unicode.Sk, unicode.Sc) {
			continue
		}
		builder.WriteRune(r)
		lastRune = r
		lastRuneSet = true
		lastWasSpace = false
	}

	normalized := builder.String()
	normalized = multiSpacePattern.ReplaceAllString(normalized, " ")
	normalized = repeatedPausePattern.ReplaceAllStringFunc(normalized, func(match string) string {
		runes := []rune(match)
		if len(runes) == 0 {
			return ""
		}
		return string(runes[len(runes)-1])
	})
	normalized = strings.Trim(normalized, " ，。、！？；：")

	if normalized == "" {
		return trimmed
	}
	return normalized
}
