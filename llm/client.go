package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://openai.qiniu.com/v1"
	defaultModelID = "gpt-oss-120b"
)

// ChatClient wraps the HTTP calls to the Qiniu/OpenAI compatible chat completions API.
type ChatClient struct {
	httpClient *http.Client
	baseURL    string
	apiKey     string
	modelID    string
}

// NewChatClientFromEnv constructs a ChatClient using environment variables.
//
// Expected variables:
//   - LLM_API_KEY: required API key for the provider
//   - LLM_BASE_URL: optional override for the API base URL (defaults to defaultBaseURL)
//   - LLM_MODEL_ID: optional override for the target model (defaults to defaultModelID)
func NewChatClientFromEnv() (*ChatClient, error) {
	apiKey := strings.TrimSpace(os.Getenv("LLM_API_KEY"))
	if apiKey == "" {
		return nil, errors.New("llm: LLM_API_KEY environment variable is required")
	}

	baseURL := strings.TrimSpace(os.Getenv("LLM_BASE_URL"))
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		return nil, fmt.Errorf("llm: invalid base URL %q", baseURL)
	}

	modelID := strings.TrimSpace(os.Getenv("LLM_MODEL_ID"))
	if modelID == "" {
		modelID = defaultModelID
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}

	return &ChatClient{
		httpClient: httpClient,
		baseURL:    baseURL,
		apiKey:     apiKey,
		modelID:    modelID,
	}, nil
}

// ChatMessage represents a single turn in a chat conversation payload.
type ChatMessage struct {
	Role    string
	Content string
}

// chatCompletionMessage matches the API payload structure for messages.
type chatCompletionMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatCompletionRequest represents the request body sent to the model.
type chatCompletionRequest struct {
	Model    string                  `json:"model"`
	Stream   bool                    `json:"stream"`
	Messages []chatCompletionMessage `json:"messages"`
}

// chatCompletionResponse captures the subset of fields we consume.
type chatCompletionResponse struct {
	Choices []struct {
		Message chatCompletionMessage `json:"message"`
	} `json:"choices"`
}

// Complete sends the given prompt to the chat completions API and returns the first response message content.
func (c *ChatClient) Complete(ctx context.Context, prompt string) (string, error) {
	trimmed := strings.TrimSpace(prompt)
	if trimmed == "" {
		return "", errors.New("llm: prompt cannot be empty")
	}

	return c.Chat(ctx, []ChatMessage{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: trimmed},
	})
}

// Chat sends the provided conversational messages to the LLM and returns the first assistant reply.
func (c *ChatClient) Chat(ctx context.Context, messages []ChatMessage) (string, error) {
	if c == nil {
		return "", errors.New("llm: client is nil")
	}
	if len(messages) == 0 {
		return "", errors.New("llm: messages cannot be empty")
	}

	payload := chatCompletionRequest{
		Model:    c.modelID,
		Stream:   false,
		Messages: make([]chatCompletionMessage, 0, len(messages)),
	}

	for _, msg := range messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			role = "user"
		}
		content := strings.TrimSpace(msg.Content)
		if content == "" {
			continue
		}
		payload.Messages = append(payload.Messages, chatCompletionMessage{Role: role, Content: content})
	}

	if len(payload.Messages) == 0 {
		return "", errors.New("llm: messages contain no content")
	}

	body := &bytes.Buffer{}
	if err := json.NewEncoder(body).Encode(payload); err != nil {
		return "", fmt.Errorf("llm: encode request: %w", err)
	}

	endpoint := c.baseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return "", fmt.Errorf("llm: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm: execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return "", fmt.Errorf("llm: unexpected status %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}

	var decoded chatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return "", fmt.Errorf("llm: decode response: %w", err)
	}

	if len(decoded.Choices) == 0 {
		return "", errors.New("llm: response contains no choices")
	}

	return strings.TrimSpace(decoded.Choices[0].Message.Content), nil
}
