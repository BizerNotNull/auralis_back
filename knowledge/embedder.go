package knowledge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type Embedder interface {
	Embed(ctx context.Context, inputs []string) ([][]float32, error)
}

type httpEmbedder struct {
	httpClient  *http.Client
	baseURL     string
	apiKey      string
	modelID     string
	maxBatch    int
	expectDim   int
	dimensions  int
	inputType   string
	textType    string
	instruct    string
	outputType  string
	extraHeader http.Header
}

type embeddingRequest struct {
	Model      string      `json:"model"`
	Input      []string    `json:"input"`
	User       string      `json:"user,omitempty"`
	Meta       interface{} `json:"metadata,omitempty"`
	Dimensions *int        `json:"dimensions,omitempty"`
	InputType  string      `json:"input_type,omitempty"`
	TextType   string      `json:"text_type,omitempty"`
	Instruct   string      `json:"instruct,omitempty"`
	OutputType string      `json:"output_type,omitempty"`
}

type embeddingResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
}

func NewHTTPEmbedderFromEnv() (Embedder, error) {
	apiKey := strings.TrimSpace(os.Getenv("EMBEDDING_API_KEY"))
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("DASHSCOPE_API_KEY"))
	}
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("LLM_API_KEY"))
	}
	if apiKey == "" {
		return nil, errors.New("knowledge: embedding API key is required")
	}

	baseURL := strings.TrimSpace(os.Getenv("EMBEDDING_BASE_URL"))
	if baseURL == "" {
		baseURL = strings.TrimSpace(os.Getenv("DASHSCOPE_BASE_URL"))
	}
	if baseURL == "" {
		baseURL = strings.TrimSpace(os.Getenv("LLM_BASE_URL"))
	}
	if baseURL == "" {
		baseURL = "https://dashscope.aliyuncs.com/compatible-mode/v1"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		return nil, fmt.Errorf("knowledge: invalid embedding base URL %q", baseURL)
	}

	modelID := strings.TrimSpace(os.Getenv("EMBEDDING_MODEL_ID"))
	if modelID == "" {
		modelID = strings.TrimSpace(os.Getenv("DASHSCOPE_EMBEDDING_MODEL"))
	}
	if modelID == "" {
		modelID = "text-embedding-v4"
	}

	maxBatch := 16
	if raw := strings.TrimSpace(os.Getenv("EMBEDDING_MAX_BATCH")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			maxBatch = parsed
		}
	}

	expectDim := 0
	if raw := strings.TrimSpace(os.Getenv("EMBEDDING_VECTOR_DIM")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			expectDim = parsed
		}
	}
	if expectDim == 0 {
		if raw := strings.TrimSpace(os.Getenv("QDRANT_VECTOR_DIM")); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
				expectDim = parsed
			}
		}
	}

	dimensions := 0
	if raw := strings.TrimSpace(os.Getenv("EMBEDDING_DIMENSIONS")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			dimensions = parsed
		}
	}
	if dimensions == 0 && expectDim > 0 {
		dimensions = expectDim
	}

	inputType := strings.TrimSpace(os.Getenv("EMBEDDING_INPUT_TYPE"))
	if inputType != "" {
		inputType = strings.ToLower(inputType)
	}

	textType := strings.TrimSpace(os.Getenv("EMBEDDING_TEXT_TYPE"))
	if textType != "" {
		textType = strings.ToLower(textType)
	}

	outputType := strings.TrimSpace(os.Getenv("EMBEDDING_OUTPUT_TYPE"))
	if outputType != "" {
		outputType = strings.ToLower(outputType)
	}

	instruct := strings.TrimSpace(os.Getenv("EMBEDDING_INSTRUCT"))

	client := &http.Client{Timeout: 30 * time.Second}

	return &httpEmbedder{
		httpClient: client,
		baseURL:    baseURL,
		apiKey:     apiKey,
		modelID:    modelID,
		maxBatch:   maxBatch,
		expectDim:  expectDim,
		dimensions: dimensions,
		inputType:  inputType,
		textType:   textType,
		instruct:   instruct,
		outputType: outputType,
		extraHeader: http.Header{
			"User-Agent": []string{"auralis-knowledge/1.0"},
		},
	}, nil
}

func (e *httpEmbedder) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if e == nil {
		return nil, errors.New("knowledge: embedder is not configured")
	}
	sanitized := make([]string, 0, len(inputs))
	for _, item := range inputs {
		trimmed := strings.TrimSpace(item)
		if trimmed == "" {
			continue
		}
		sanitized = append(sanitized, trimmed)
	}
	if len(sanitized) == 0 {
		return nil, nil
	}

	if e.maxBatch <= 0 {
		e.maxBatch = 16
	}

	var results [][]float32
	for start := 0; start < len(sanitized); start += e.maxBatch {
		end := start + e.maxBatch
		if end > len(sanitized) {
			end = len(sanitized)
		}
		batch := sanitized[start:end]
		batchVectors, err := e.embedBatch(ctx, batch)
		if err != nil {
			return nil, err
		}
		results = append(results, batchVectors...)
	}
	return results, nil
}

func (e *httpEmbedder) embedBatch(ctx context.Context, batch []string) ([][]float32, error) {
	payload := embeddingRequest{
		Model: e.modelID,
		Input: batch,
	}
	if e.dimensions > 0 {
		dim := e.dimensions
		payload.Dimensions = &dim
	}
	if e.inputType != "" {
		payload.InputType = e.inputType
	}
	if e.textType != "" {
		payload.TextType = e.textType
	}
	if e.instruct != "" {
		payload.Instruct = e.instruct
	}
	if e.outputType != "" {
		payload.OutputType = e.outputType
	}

	body := &bytes.Buffer{}
	if err := json.NewEncoder(body).Encode(payload); err != nil {
		return nil, fmt.Errorf("knowledge: encode embedding payload: %w", err)
	}

	endpoint := e.baseURL + "/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("knowledge: create embedding request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	for key, values := range e.extraHeader {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("knowledge: embedding request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("knowledge: embedding API status %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}

	var decoded embeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("knowledge: decode embedding response: %w", err)
	}

	if len(decoded.Data) != len(batch) {
		return nil, fmt.Errorf("knowledge: embedding response count mismatch (expected %d, got %d)", len(batch), len(decoded.Data))
	}

	vectors := make([][]float32, len(decoded.Data))
	for i, item := range decoded.Data {
		vector := make([]float32, 0, len(item.Embedding))
		for _, value := range item.Embedding {
			vector = append(vector, float32(value))
		}
		if e.expectDim > 0 && len(vector) != e.expectDim {
			return nil, fmt.Errorf("knowledge: embedding length %d does not match expected %d", len(vector), e.expectDim)
		}
		vectors[i] = vector
	}

	return vectors, nil
}
