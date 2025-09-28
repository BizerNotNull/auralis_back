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

// messageCache 缓存用户与智能体的近期消息。
type messageCache struct {
	client *redis.Client
}

// newMessageCache 使用 Redis 客户端创建消息缓存。
func newMessageCache(client *redis.Client) *messageCache {
	if client == nil {
		return nil
	}
	return &messageCache{client: client}
}

// cacheContext 为缓存操作设置超时上下文。
func (m *messageCache) cacheContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		return context.WithTimeout(context.Background(), recentMessagesCacheTimeout)
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= recentMessagesCacheTimeout {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, recentMessagesCacheTimeout)
}

// key 构造缓存键格式。
func (m *messageCache) key(agentID, userID uint64) string {
	if m == nil || m.client == nil || agentID == 0 || userID == 0 {
		return ""
	}
	return fmt.Sprintf("llm:recent:%d:%d", agentID, userID)
}

// get 从缓存中读取近期消息记录。
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

// store 将近期消息写入缓存。
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

// invalidate 清除指定用户与智能体的缓存。
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
