package llm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	defaultMemoryRecentLimit    = 12
	defaultMemorySummaryWindow  = 40
	defaultMemorySummaryTrigger = 6
	defaultMemorySummaryMax     = 800
)

type conversationMemory struct {
	db     *gorm.DB
	client *ChatClient
	cfg    memoryConfig
}

type memoryConfig struct {
	recentLimit     int
	summaryWindow   int
	summaryTrigger  int
	summaryMaxChars int
	summaryPrompt   string
}

type userProfile struct {
	Preferences map[string]any
	Summary     string
	LastTask    string
}

func newConversationMemory(db *gorm.DB, client *ChatClient) *conversationMemory {
	if db == nil {
		return nil
	}

	cfg := memoryConfig{
		recentLimit:     readIntEnv("LLM_MEMORY_RECENT_LIMIT", defaultMemoryRecentLimit),
		summaryWindow:   readIntEnv("LLM_MEMORY_SUMMARY_WINDOW", defaultMemorySummaryWindow),
		summaryTrigger:  readIntEnv("LLM_MEMORY_SUMMARY_THRESHOLD", defaultMemorySummaryTrigger),
		summaryMaxChars: readIntEnv("LLM_MEMORY_SUMMARY_MAX_CHARS", defaultMemorySummaryMax),
		summaryPrompt:   strings.TrimSpace(os.Getenv("LLM_MEMORY_SUMMARY_PROMPT")),
	}

	if cfg.recentLimit <= 0 {
		cfg.recentLimit = defaultMemoryRecentLimit
	}
	if cfg.summaryWindow < cfg.recentLimit {
		cfg.summaryWindow = cfg.recentLimit * 2
	}
	if cfg.summaryWindow <= 0 {
		cfg.summaryWindow = defaultMemorySummaryWindow
	}
	if cfg.summaryTrigger < 2 {
		cfg.summaryTrigger = defaultMemorySummaryTrigger
	}
	if cfg.summaryMaxChars <= 100 {
		cfg.summaryMaxChars = defaultMemorySummaryMax
	}
	if cfg.summaryPrompt == "" {
		cfg.summaryPrompt = "You are an assistant that maintains running conversation notes. Keep summaries concise, factual, and focused on user goals, preferences, commitments, and unresolved items."
	}

	return &conversationMemory{db: db, client: client, cfg: cfg}
}

func readIntEnv(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

func (m *conversationMemory) recentMessageLimit() int {
	if m == nil || m.cfg.recentLimit <= 0 {
		return defaultMemoryRecentLimit
	}
	return m.cfg.recentLimit
}

func (m *conversationMemory) loadUserProfile(ctx context.Context, agentID, userID uint64) (*userProfile, error) {
	if m == nil || agentID == 0 || userID == 0 {
		return &userProfile{Preferences: map[string]any{}}, nil
	}

	var record userAgentMemory
	err := m.db.WithContext(ctx).
		Where("agent_id = ? AND user_id = ?", agentID, userID).
		Take(&record).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return &userProfile{Preferences: map[string]any{}}, nil
		}
		return nil, err
	}

	profile := &userProfile{Preferences: map[string]any{}}
	if record.ProfileSummary != nil {
		profile.Summary = strings.TrimSpace(*record.ProfileSummary)
	}
	if record.LastTask != nil {
		profile.LastTask = strings.TrimSpace(*record.LastTask)
	}
	if len(record.Preferences) > 0 {
		var prefs map[string]any
		if err := json.Unmarshal(record.Preferences, &prefs); err == nil {
			profile.Preferences = prefs
		}
	}

	return profile, nil
}

func (m *conversationMemory) upsertSpeechPreferences(ctx context.Context, agentID, userID uint64, prefs speechPreferences) error {
	if m == nil || agentID == 0 || userID == 0 {
		return nil
	}

	payload := make(map[string]any)
	if v := strings.TrimSpace(prefs.VoiceID); v != "" {
		payload["voice_id"] = v
	}
	if v := strings.TrimSpace(prefs.Provider); v != "" {
		payload["voice_provider"] = v
	}
	if prefs.Speed > 0 {
		payload["speech_speed"] = sanitizeSpeed(prefs.Speed)
	}
	if prefs.Pitch > 0 {
		payload["speech_pitch"] = sanitizePitch(prefs.Pitch)
	}
	if hint := strings.TrimSpace(prefs.EmotionHint); hint != "" {
		payload["emotion_hint"] = hint
	}
	if len(payload) == 0 {
		return nil
	}

	var record userAgentMemory
	err := m.db.WithContext(ctx).
		Where("agent_id = ? AND user_id = ?", agentID, userID).
		Take(&record).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}

	merged := make(map[string]any)
	if len(record.Preferences) > 0 {
		_ = json.Unmarshal(record.Preferences, &merged)
	}
	for k, v := range payload {
		merged[k] = v
	}

	raw, err := json.Marshal(merged)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	upsert := userAgentMemory{
		AgentID:     agentID,
		UserID:      userID,
		Preferences: datatypes.JSON(raw),
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	return m.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "agent_id"}, {Name: "user_id"}},
		DoUpdates: clause.Assignments(map[string]any{"preferences": datatypes.JSON(raw), "updated_at": now}),
	}).Create(&upsert).Error
}

func (m *conversationMemory) ensureSummary(ctx context.Context, conv conversation) (string, error) {
	if m == nil || m.cfg.summaryTrigger <= 0 {
		return "", nil
	}

	var total int64
	if err := m.db.WithContext(ctx).
		Model(&message{}).
		Where("conversation_id = ?", conv.ID).
		Count(&total).Error; err != nil {
		return "", err
	}
	if int(total) < m.cfg.summaryTrigger {
		return "", nil
	}

	window := m.cfg.summaryWindow
	if window <= 0 {
		window = defaultMemorySummaryWindow
	}

	var history []message
	if err := m.db.WithContext(ctx).
		Where("conversation_id = ?", conv.ID).
		Order("seq DESC").
		Limit(window).
		Find(&history).Error; err != nil {
		return "", err
	}
	if len(history) == 0 {
		return "", nil
	}
	for i, j := 0, len(history)-1; i < j; i, j = i+1, j-1 {
		history[i], history[j] = history[j], history[i]
	}

	summary, err := m.generateSummary(ctx, conv, history)
	if err != nil {
		return "", err
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return "", nil
	}
	if len(summary) > m.cfg.summaryMaxChars {
		summary = truncateString(summary, m.cfg.summaryMaxChars)
	}

	updates := map[string]any{
		"summary":            summary,
		"summary_updated_at": time.Now().UTC(),
	}
	if err := m.db.WithContext(ctx).
		Model(&conversation{}).
		Where("id = ?", conv.ID).
		Updates(updates).Error; err != nil {
		return "", err
	}

	return summary, nil
}

func (m *conversationMemory) generateSummary(ctx context.Context, conv conversation, history []message) (string, error) {
	transcript := buildTranscript(conv, history)

	if m.client == nil {
		return fallbackSummary(transcript), nil
	}

	messages := []ChatMessage{
		{Role: "system", Content: m.cfg.summaryPrompt},
		{Role: "user", Content: transcript},
	}
	result, err := m.client.Chat(ctx, messages)
	if err != nil {
		return fallbackSummary(transcript), nil
	}

	return result.Content, nil
}

func buildTranscript(conv conversation, history []message) string {
	var builder strings.Builder
	if conv.Summary != nil {
		if prior := strings.TrimSpace(*conv.Summary); prior != "" {
			builder.WriteString("Existing summary:\n")
			builder.WriteString(prior)
			builder.WriteString("\n\n")
		}
	}

	builder.WriteString("Recent turns:\n")
	for _, msg := range history {
		role := strings.ToUpper(strings.TrimSpace(msg.Role))
		if role == "" {
			role = "UNKNOWN"
		}
		builder.WriteString(role)
		builder.WriteString(": ")
		builder.WriteString(strings.TrimSpace(msg.Content))
		builder.WriteRune('\n')
	}

	return builder.String()
}

func fallbackSummary(transcript string) string {
	transcript = strings.TrimSpace(transcript)
	if transcript == "" {
		return ""
	}
	lines := strings.Split(transcript, "\n")
	if len(lines) > 10 {
		lines = lines[len(lines)-10:]
	}
	return strings.Join(lines, " \n")
}
