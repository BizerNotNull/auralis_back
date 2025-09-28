package llm

import (
	"bufio"
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

// ChatClient 封装与七牛/OpenAI 兼容接口的 HTTP 调用。
type ChatClient struct {
	httpClient *http.Client
	baseURL    string
	apiKey     string
	modelID    string
}

// NewChatClientFromEnv 基于环境变量创建 ChatClient 实例。
//
// Expected variables:
//   - LLM_API_KEY: required API key for the provider
//   - LLM_BASE_URL: optional override for the API base URL (defaults to defaultBaseURL)
// NewChatClientFromEnv 基于环境变量创建 ChatClient 实例。
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

// ChatMessage 表示聊天请求中的单轮消息。
type ChatMessage struct {
	Role    string
	Content string
}

// chatCompletionMessage 对应接口要求的消息结构。
type chatCompletionMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatCompletionRequest 描述发送给模型的请求体。
type chatCompletionRequest struct {
	Model    string                  `json:"model"`
	Stream   bool                    `json:"stream"`
	Messages []chatCompletionMessage `json:"messages"`
}

// chatCompletionUsage 记录模型返回的 token 统计。
type chatCompletionUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// chatCompletionResponse 表示响应中需要使用的字段。
type chatCompletionResponse struct {
	Choices []struct {
		Message chatCompletionMessage `json:"message"`
	} `json:"choices"`
	Usage *chatCompletionUsage `json:"usage"`
}

type ChatStreamDelta struct {
	Content      string
	FullContent  string
	FinishReason string
	Done         bool
}

// chatStreamChunk 映射供应商返回的流式增量数据。

type chatStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatCompletionUsage `json:"usage"`
}

// ChatUsage 表示模型返回的 token 使用情况。
type ChatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatResult 组合模型回复内容与使用信息。
type ChatResult struct {
	Content string
	Usage   *ChatUsage
}

// Complete 调用补全接口获取完整回复。
func (c *ChatClient) Complete(ctx context.Context, prompt string) (ChatResult, error) {
	trimmed := strings.TrimSpace(prompt)
	if trimmed == "" {
		return ChatResult{}, errors.New("llm: prompt cannot be empty")
	}

	return c.Chat(ctx, []ChatMessage{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: trimmed},
	})
}

// Chat 调用补全接口并将结果整合为 ChatResult。
func (c *ChatClient) Chat(ctx context.Context, messages []ChatMessage) (ChatResult, error) {
	if c == nil {
		return ChatResult{}, errors.New("llm: client is nil")
	}
	if len(messages) == 0 {
		return ChatResult{}, errors.New("llm: messages cannot be empty")
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
		return ChatResult{}, errors.New("llm: messages contain no content")
	}

	body := &bytes.Buffer{}
	if err := json.NewEncoder(body).Encode(payload); err != nil {
		return ChatResult{}, fmt.Errorf("llm: encode request: %w", err)
	}

	endpoint := c.baseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return ChatResult{}, fmt.Errorf("llm: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ChatResult{}, fmt.Errorf("llm: execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return ChatResult{}, fmt.Errorf("llm: unexpected status %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}

	var decoded chatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return ChatResult{}, fmt.Errorf("llm: decode response: %w", err)
	}

	if len(decoded.Choices) == 0 {
		return ChatResult{}, errors.New("llm: response contains no choices")
	}

	full := strings.TrimSpace(decoded.Choices[0].Message.Content)

	return ChatResult{
		Content: full,
		Usage:   convertUsage(decoded.Usage),
	}, nil
}

// Chat 调用补全接口并将结果整合为 ChatResult。
func (c *ChatClient) ChatStream(ctx context.Context, messages []ChatMessage, handler func(ChatStreamDelta) error) (ChatResult, error) {
	if c == nil {
		return ChatResult{}, errors.New("llm: client is nil")
	}
	if len(messages) == 0 {
		return ChatResult{}, errors.New("llm: messages cannot be empty")
	}

	payload := chatCompletionRequest{
		Model:    c.modelID,
		Stream:   true,
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
		return ChatResult{}, errors.New("llm: messages contain no content")
	}

	body := &bytes.Buffer{}
	if err := json.NewEncoder(body).Encode(payload); err != nil {
		return ChatResult{}, fmt.Errorf("llm: encode request: %w", err)
	}

	endpoint := c.baseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return ChatResult{}, fmt.Errorf("llm: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ChatResult{}, fmt.Errorf("llm: execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return ChatResult{}, fmt.Errorf("llm: unexpected status %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}

	contentType := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
	if strings.Contains(contentType, "application/json") {
		var decoded chatCompletionResponse
		if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
			return ChatResult{}, fmt.Errorf("llm: decode response: %w", err)
		}
		if len(decoded.Choices) == 0 {
			return ChatResult{}, errors.New("llm: response contains no choices")
		}
		full := strings.TrimSpace(decoded.Choices[0].Message.Content)
		if handler != nil && full != "" {
			if err := handler(ChatStreamDelta{Content: full, FullContent: full}); err != nil {
				return ChatResult{}, err
			}
		}
		if handler != nil {
			if err := handler(ChatStreamDelta{FullContent: full, Done: true}); err != nil {
				return ChatResult{}, err
			}
		}
		return ChatResult{
			Content: full,
			Usage:   convertUsage(decoded.Usage),
		}, nil
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var builder strings.Builder
	var usage *chatCompletionUsage

	flushDelta := func(delta ChatStreamDelta) error {
		if handler == nil {
			return nil
		}
		return handler(delta)
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(line[len("data:"):])
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			if err := flushDelta(ChatStreamDelta{FullContent: builder.String(), Done: true}); err != nil {
				return ChatResult{}, err
			}
			return ChatResult{
				Content: builder.String(),
				Usage:   convertUsage(usage),
			}, nil
		}

		var chunk chatStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			deltaText := choice.Delta.Content
			if deltaText != "" {
				builder.WriteString(deltaText)
				if err := flushDelta(ChatStreamDelta{
					Content:      deltaText,
					FullContent:  builder.String(),
					FinishReason: choice.FinishReason,
				}); err != nil {
					return ChatResult{}, err
				}
			}
			if deltaText == "" && choice.FinishReason != "" {
				if err := flushDelta(ChatStreamDelta{
					FullContent:  builder.String(),
					FinishReason: choice.FinishReason,
				}); err != nil {
					return ChatResult{}, err
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return ChatResult{}, fmt.Errorf("llm: read stream: %w", err)
	}

	if err := flushDelta(ChatStreamDelta{FullContent: builder.String(), Done: true}); err != nil {
		return ChatResult{}, err
	}

	return ChatResult{
		Content: builder.String(),
		Usage:   convertUsage(usage),
	}, nil
}

// convertUsage 将底层的 token 统计转换为公共结构。
func convertUsage(raw *chatCompletionUsage) *ChatUsage {
	if raw == nil {
		return nil
	}
	return &ChatUsage{
		PromptTokens:     raw.PromptTokens,
		CompletionTokens: raw.CompletionTokens,
		TotalTokens:      raw.TotalTokens,
	}
}
