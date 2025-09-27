package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	recentMessagesCacheTTL     = 30 * time.Second
	recentMessagesCacheTimeout = 300 * time.Millisecond
)

type messageCache struct {
	client *redis.Client
}

func newMessageCache(client *redis.Client) *messageCache {
	if client == nil {
		return nil
	}
	return &messageCache{client: client}
}

func (m *messageCache) cacheContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		return context.WithTimeout(context.Background(), recentMessagesCacheTimeout)
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= recentMessagesCacheTimeout {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, recentMessagesCacheTimeout)
}

func (m *messageCache) key(agentID, userID uint64) string {
	if m == nil || m.client == nil || agentID == 0 || userID == 0 {
		return ""
	}
	return fmt.Sprintf("llm:recent:%d:%d", agentID, userID)
}

func (m *messageCache) get(ctx context.Context, agentID, userID uint64) ([]messageRecord, error) {
	if m == nil || m.client == nil {
		return nil, redis.Nil
	}
	key := m.key(agentID, userID)
	if key == "" {
		return nil, redis.Nil
	}

	ctx, cancel := m.cacheContext(ctx)
	defer cancel()

	data, err := m.client.Get(ctx, key).Bytes()
	if err != nil {
		return nil, err
	}

	var records []messageRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, err
	}
	return records, nil
}

func (m *messageCache) store(ctx context.Context, agentID, userID uint64, records []messageRecord) {
	if m == nil || m.client == nil {
		return
	}
	key := m.key(agentID, userID)
	if key == "" {
		return
	}

	payload, err := json.Marshal(records)
	if err != nil {
		log.Printf("llm: marshal recent messages cache payload failed: %v", err)
		return
	}

	ctx, cancel := m.cacheContext(ctx)
	defer cancel()

	if err := m.client.Set(ctx, key, payload, recentMessagesCacheTTL).Err(); err != nil {
		log.Printf("llm: store recent messages cache failed: %v", err)
	}
}

func (m *messageCache) invalidate(ctx context.Context, agentID, userID uint64) {
	if m == nil || m.client == nil {
		return
	}
	key := m.key(agentID, userID)
	if key == "" {
		return
	}

	ctx, cancel := m.cacheContext(ctx)
	defer cancel()

	if err := m.client.Del(ctx, key).Err(); err != nil {
		log.Printf("llm: invalidate recent messages cache failed: %v", err)
	}
}
