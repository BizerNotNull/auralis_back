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
	"strings"
	"time"
	"unicode"
)

var ErrDisabled = errors.New("tts: service disabled")

type Client struct {
	httpClient     *http.Client
	baseURL        string
	backupBaseURL  string
	apiKey         string
	model          string
	defaultVoice   string
	responseFormat string
	voices         []VoiceOption
	provider       string
	enabled        bool
}

func NewClientFromEnv() (*Client, error) {
	baseURL := strings.TrimSpace(os.Getenv("TTS_API_BASE_URL"))
	if baseURL == "" {
		baseURL = "https://openai.qiniu.com/v1"
	}
	baseURL = strings.TrimRight(baseURL, "/")

	backupBaseURL := strings.TrimSpace(os.Getenv("TTS_API_BACKUP_URL"))
	if backupBaseURL == "" {
		backupBaseURL = "https://api.qnaigc.com/v1"
	}
	backupBaseURL = strings.TrimRight(backupBaseURL, "/")
	if backupBaseURL == baseURL {
		backupBaseURL = ""
	}

	apiKey := strings.TrimSpace(os.Getenv("TTS_API_KEY"))
	enabled := apiKey != ""

	model := strings.TrimSpace(os.Getenv("TTS_MODEL_ID"))
	if model == "" {
		model = "tts"
	}

	defaultVoice := strings.TrimSpace(os.Getenv("TTS_DEFAULT_VOICE"))
	if defaultVoice == "" {
		defaultVoice = "qiniu_zh_female_tmjxxy"
	}

	responseFormat := strings.TrimSpace(os.Getenv("TTS_RESPONSE_FORMAT"))
	if responseFormat == "" {
		responseFormat = "mp3"
	}

	voices := loadVoiceCatalogFromEnv(defaultVoice)

	httpClient := &http.Client{Timeout: 45 * time.Second}

	provider := strings.TrimSpace(os.Getenv("TTS_PROVIDER"))
	if provider == "" {
		provider = "qiniu-openai"
	}

	client := &Client{
		httpClient:     httpClient,
		baseURL:        baseURL,
		backupBaseURL:  backupBaseURL,
		apiKey:         apiKey,
		model:          model,
		defaultVoice:   defaultVoice,
		responseFormat: responseFormat,
		voices:         voices,
		provider:       provider,
		enabled:        enabled,
	}

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

func (c *Client) Voices() []VoiceOption {
	if c == nil {
		return nil
	}
	out := make([]VoiceOption, len(c.voices))
	copy(out, c.voices)
	return out
}

func (c *Client) Synthesize(ctx context.Context, req SpeechRequest) (*SpeechResult, error) {
	if c == nil {
		return nil, ErrDisabled
	}
	if !c.enabled {
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
		format = c.responseFormat
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
		c.defaultVoice,
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
		if c.model != "" {
			payload["model"] = c.model
		}

		body, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("tts: encode request: %w", err)
		}

		resp, err := c.requestWithFallback(ctx, body)
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

		audioBytes, mime, err := c.processTTSResponse(responseBody, format, resp.Header.Get("Content-Type"))
		if err != nil {
			return nil, err
		}

		finalVoice := candidateVoice
		if strings.TrimSpace(finalVoice) == "" {
			finalVoice = requestedVoice
			if strings.TrimSpace(finalVoice) == "" {
				finalVoice = c.defaultVoice
			}
		}

		result := &SpeechResult{
			VoiceID:     finalVoice,
			Emotion:     emotion,
			AudioBase64: base64.StdEncoding.EncodeToString(audioBytes),
			MimeType:    mime,
			Speed:       speed,
			Pitch:       pitch,
			Provider:    c.provider,
		}

		return result, nil
	}

	if lastErr != nil {
		return nil, lastErr
	}

	return nil, errors.New("tts: no valid voices available")
}

func (c *Client) requestWithFallback(ctx context.Context, body []byte) (*http.Response, error) {
	bases := []string{c.baseURL}
	if c.backupBaseURL != "" && !strings.EqualFold(c.backupBaseURL, c.baseURL) {
		bases = append(bases, c.backupBaseURL)
	}

	var lastErr error
	for idx, base := range bases {
		if base == "" {
			continue
		}
		resp, err := c.doRequest(ctx, base, body)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if idx == 1 {
				log.Printf("tts: primary base url failed, fallback %s succeeded", base)
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

func (c *Client) doRequest(ctx context.Context, base string, body []byte) (*http.Response, error) {
	endpoint := strings.TrimRight(base, "/") + "/voice/tts"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("tts: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	return c.httpClient.Do(req)
}

func (c *Client) processTTSResponse(body []byte, fallbackFormat, contentType string) ([]byte, string, error) {
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

func decodeAudioFromJSON(body []byte) ([]byte, string, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, "", errors.New("tts: empty json response")
	}

	var envelope any
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return nil, "", fmt.Errorf("tts: parse json response: %w", err)
	}

	audioB64, encoding, err := extractAudioPayload(envelope, "")
	if err != nil {
		return nil, "", err
	}
	if strings.TrimSpace(audioB64) == "" {
		return nil, "", errors.New("tts: response missing audio payload")
	}

	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(audioB64))
	if err != nil {
		return nil, "", fmt.Errorf("tts: decode audio payload: %w", err)
	}

	return decoded, encoding, nil
}

func extractAudioPayload(node any, inheritedEncoding string) (string, string, error) {
	switch value := node.(type) {
	case map[string]any:
		if code, ok := value["code"].(float64); ok && code != 0 {
			message := firstNonEmptyString(value, "message", "msg", "error", "error_msg", "error_message")
			return "", "", fmt.Errorf("tts: provider error code %.0f: %s", code, message)
		}

		localEncoding := firstNonEmptyString(value, "encoding", "format", "mime_type", "mime")
		if localEncoding == "" {
			localEncoding = inheritedEncoding
		}

		audioKeys := []string{"data", "audio_base64", "audio", "audio_data", "audioBytes", "audio_bytes", "audioContent"}
		for _, key := range audioKeys {
			if raw, ok := value[key]; ok {
				if str, ok := raw.(string); ok && strings.TrimSpace(str) != "" {
					return strings.TrimSpace(str), localEncoding, nil
				}
			}
		}

		for _, v := range value {
			audio, enc, err := extractAudioPayload(v, localEncoding)
			if err != nil {
				return "", "", err
			}
			if strings.TrimSpace(audio) != "" {
				if enc == "" {
					enc = localEncoding
				}
				return audio, enc, nil
			}
		}
	case []any:
		for _, item := range value {
			audio, enc, err := extractAudioPayload(item, inheritedEncoding)
			if err != nil {
				return "", "", err
			}
			if strings.TrimSpace(audio) != "" {
				return audio, enc, nil
			}
		}
	case string:
		trimmed := strings.TrimSpace(value)
		if trimmed != "" && isLikelyBase64(trimmed) {
			return trimmed, inheritedEncoding, nil
		}
	}

	return "", inheritedEncoding, nil
}

func isLikelyBase64(value string) bool {
	if len(value) < 32 {
		return false
	}
	for _, r := range value {
		if !(r == '+' || r == '/' || r == '=' || (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')) {
			return false
		}
	}
	return true
}

func firstNonEmptyString(node map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := node[key]; ok {
			if str, ok := value.(string); ok && strings.TrimSpace(str) != "" {
				return strings.TrimSpace(str)
			}
		}
	}
	return ""
}

func encodingToMime(encoding string) string {
	encoding = strings.ToLower(strings.TrimSpace(encoding))
	switch encoding {
	case "", "mp3", "mpeg", "audio/mpeg":
		return "audio/mpeg"
	case "wav", "wave", "audio/wav", "audio/x-wav":
		return "audio/wav"
	case "ogg", "opus", "audio/ogg":
		return "audio/ogg"
	default:
		return ""
	}
}

func orderedDistinct(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		key := trimmed
		if trimmed == "" {
			key = "__empty__"
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		if key == "__empty__" {
			out = append(out, "")
		} else {
			out = append(out, trimmed)
		}
	}
	return out
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

func (c *Client) bootstrapVoiceCatalog() {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	if len(c.voices) == 0 && c.apiKey != "" {
		if remote := c.fetchVoiceCatalog(ctx); len(remote) > 0 {
			c.voices = remote
		}
	}

	if len(c.voices) == 0 {
		c.voices = defaultVoiceCatalog()
	}

	if c.defaultVoice == "" && len(c.voices) > 0 {
		c.defaultVoice = c.voices[0].ID
	}

	if !containsVoiceID(c.voices, c.defaultVoice) && len(c.voices) > 0 {
		log.Printf("tts: default voice %q not found in catalog; falling back to %q", c.defaultVoice, c.voices[0].ID)
		c.defaultVoice = c.voices[0].ID
	}
}

func (c *Client) fetchVoiceCatalog(ctx context.Context) []VoiceOption {
	bases := orderedDistinct([]string{c.baseURL, c.backupBaseURL})
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
		req.Header.Set("Authorization", "Bearer "+c.apiKey)

		resp, err := c.httpClient.Do(req)
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
				Provider:     c.provider,
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

func loadVoiceCatalogFromEnv(defaultVoice string) []VoiceOption {
	raw := strings.TrimSpace(os.Getenv("TTS_VOICE_CATALOG"))
	if raw == "" {
		return nil
	}

	var custom []VoiceOption
	if err := json.Unmarshal([]byte(raw), &custom); err != nil {
		log.Printf("tts: failed to parse TTS_VOICE_CATALOG: %v", err)
		return nil
	}
	if len(custom) == 0 {
		return nil
	}
	return custom
}

func defaultVoiceCatalog() []VoiceOption {
	return []VoiceOption{
		{
			ID:           "qiniu_zh_female_tmjxxy",
			Name:         "Qiniu Female TMJXXY",
			Provider:     "qiniu-openai",
			Language:     "zh-CN",
			Description:  "Default Mandarin voice provided by Qiniu.",
			SampleURL:    "https://aitoken-public.qnaigc.com/ai-voice/qiniu_zh_female_tmjxxy.mp3",
			DefaultStyle: "conversation",
			Settings: VoiceSettings{
				SpeedRange:      [2]float64{0.5, 1.5},
				PitchRange:      [2]float64{0.8, 1.2},
				DefaultSpeed:    1.0,
				DefaultPitch:    1.0,
				SupportsEmotion: false,
			},
		},
		{
			ID:           "verse",
			Name:         "Verse",
			Provider:     "openai",
			Language:     "zh-CN,en",
			Description:  "Balanced warm timbre suitable for empathetic companions.",
			Emotions:     []string{"neutral", "happy", "gentle"},
			DefaultStyle: "conversational",
			Settings: VoiceSettings{
				SpeedRange:      [2]float64{0.7, 1.35},
				PitchRange:      [2]float64{0.75, 1.25},
				DefaultSpeed:    1.0,
				DefaultPitch:    1.0,
				SupportsEmotion: true,
			},
		},
		{
			ID:           "alloy",
			Name:         "Alloy",
			Provider:     "openai",
			Language:     "en",
			Description:  "Bright narrative tone for energetic replies.",
			Emotions:     []string{"neutral", "confident", "surprised"},
			DefaultStyle: "presenter",
			Settings: VoiceSettings{
				SpeedRange:      [2]float64{0.8, 1.4},
				PitchRange:      [2]float64{0.8, 1.3},
				DefaultSpeed:    1.05,
				DefaultPitch:    1.05,
				SupportsEmotion: true,
			},
		},
		{
			ID:           "sol",
			Name:         "Sol",
			Provider:     "openai",
			Language:     "en",
			Description:  "Low, calm register for grounded explanations.",
			Emotions:     []string{"neutral", "serious"},
			DefaultStyle: "narrator",
			Settings: VoiceSettings{
				SpeedRange:      [2]float64{0.6, 1.2},
				PitchRange:      [2]float64{0.6, 1.1},
				DefaultSpeed:    0.9,
				DefaultPitch:    0.9,
				SupportsEmotion: false,
			},
		},
	}
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

var newlineToPausePattern = regexp.MustCompile(`[\r\n]+`)
var multiSpacePattern = regexp.MustCompile(`\s{2,}`)
var repeatedPausePattern = regexp.MustCompile(`([，。！？；：]){2,}`)
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

	cleaned := newlineToPausePattern.ReplaceAllString(trimmed, "。")
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
	normalized = strings.Trim(normalized, "，。！？；： ")

	if normalized == "" {
		return trimmed
	}
	return normalized
}
