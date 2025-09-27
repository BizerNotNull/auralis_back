package knowledge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type QdrantPoint struct {
	ID      string                 `json:"id"`
	Vector  []float32              `json:"vector"`
	Payload map[string]interface{} `json:"payload,omitempty"`
}

type QdrantSearchResult struct {
	ID      string                 `json:"id"`
	Score   float64                `json:"score"`
	Payload map[string]interface{} `json:"payload"`
}

type qdrantClient struct {
	httpClient *http.Client
	baseURL    string
	apiKey     string
	vectorSize int
}

func newQdrantClientFromEnv() (*qdrantClient, error) {
	baseURL := strings.TrimSpace(os.Getenv("QDRANT_URL"))
	if baseURL == "" {
		baseURL = "http://localhost:6333"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		return nil, fmt.Errorf("knowledge: invalid Qdrant URL %q", baseURL)
	}

	if _, err := url.Parse(baseURL); err != nil {
		return nil, fmt.Errorf("knowledge: parse Qdrant URL: %w", err)
	}

	apiKey := strings.TrimSpace(os.Getenv("QDRANT_API_KEY"))

	vectorSize := 0
	if raw := strings.TrimSpace(os.Getenv("QDRANT_VECTOR_DIM")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			vectorSize = parsed
		}
	}

	client := &qdrantClient{
		httpClient: &http.Client{Timeout: 10 * time.Second},
		baseURL:    baseURL,
		apiKey:     apiKey,
		vectorSize: vectorSize,
	}
	return client, nil
}

func (c *qdrantClient) EnsureCollection(ctx context.Context, name string, vectorSize int) error {
	if c == nil {
		return errors.New("knowledge: qdrant client is not configured")
	}
	size := vectorSize
	if size <= 0 {
		size = c.vectorSize
	}
	if size <= 0 {
		return errors.New("knowledge: vector size must be positive")
	}

	payload := map[string]interface{}{
		"vectors": map[string]interface{}{
			"size":     size,
			"distance": "Cosine",
		},
	}
	body := &bytes.Buffer{}
	if err := json.NewEncoder(body).Encode(payload); err != nil {
		return fmt.Errorf("knowledge: encode collection payload: %w", err)
	}

	endpoint := fmt.Sprintf("%s/collections/%s", c.baseURL, url.PathEscape(name))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, body)
	if err != nil {
		return fmt.Errorf("knowledge: create collection request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("api-key", c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("knowledge: ensure collection request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("knowledge: ensure collection status %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	return nil
}

func (c *qdrantClient) UpsertPoints(ctx context.Context, collection string, points []QdrantPoint) error {
	if c == nil {
		return errors.New("knowledge: qdrant client is not configured")
	}
	if len(points) == 0 {
		return nil
	}

	payload := map[string]interface{}{"points": points}
	body := &bytes.Buffer{}
	if err := json.NewEncoder(body).Encode(payload); err != nil {
		return fmt.Errorf("knowledge: encode upsert payload: %w", err)
	}

	endpoint := fmt.Sprintf("%s/collections/%s/points", c.baseURL, url.PathEscape(collection))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, body)
	if err != nil {
		return fmt.Errorf("knowledge: create upsert request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("api-key", c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("knowledge: upsert request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("knowledge: upsert status %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	return nil
}

func (c *qdrantClient) DeletePoints(ctx context.Context, collection string, pointIDs []string) error {
	if c == nil {
		return errors.New("knowledge: qdrant client is not configured")
	}
	if len(pointIDs) == 0 {
		return nil
	}

	payload := map[string]interface{}{"points": pointIDs}
	body := &bytes.Buffer{}
	if err := json.NewEncoder(body).Encode(payload); err != nil {
		return fmt.Errorf("knowledge: encode delete payload: %w", err)
	}

	endpoint := fmt.Sprintf("%s/collections/%s/points", c.baseURL, url.PathEscape(collection))
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, body)
	if err != nil {
		return fmt.Errorf("knowledge: create delete request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("api-key", c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("knowledge: delete request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("knowledge: delete status %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	return nil
}

func (c *qdrantClient) Search(ctx context.Context, collection string, vector []float32, limit int, filter map[string]interface{}) ([]QdrantSearchResult, error) {
	if c == nil {
		return nil, errors.New("knowledge: qdrant client is not configured")
	}
	if len(vector) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 5
	}

	payload := map[string]interface{}{
		"vector":       vector,
		"limit":        limit,
		"with_payload": true,
	}
	if filter != nil {
		payload["filter"] = filter
	}

	body := &bytes.Buffer{}
	if err := json.NewEncoder(body).Encode(payload); err != nil {
		return nil, fmt.Errorf("knowledge: encode search payload: %w", err)
	}

	endpoint := fmt.Sprintf("%s/collections/%s/points/search", c.baseURL, url.PathEscape(collection))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("knowledge: create search request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("api-key", c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("knowledge: search request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("knowledge: search status %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}

	var decoded struct {
		Result []struct {
			ID      interface{}            `json:"id"`
			Score   float64                `json:"score"`
			Payload map[string]interface{} `json:"payload"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("knowledge: decode search response: %w", err)
	}

	results := make([]QdrantSearchResult, 0, len(decoded.Result))
	for _, item := range decoded.Result {
		identifier := stringifyQdrantID(item.ID)
		results = append(results, QdrantSearchResult{
			ID:      identifier,
			Score:   item.Score,
			Payload: item.Payload,
		})
	}
	return results, nil
}

func stringifyQdrantID(id interface{}) string {
	switch v := id.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatInt(int64(v), 10)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return strconv.FormatInt(n, 10)
		}
		return v.String()
	default:
		return fmt.Sprint(v)
	}
}
