package cache

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

var (
	redisOnce   sync.Once
	redisClient *redis.Client
	redisErr    error
)

// GetRedisClient 根据环境变量初始化并返回单例 Redis 客户端。
// 若未配置 REDIS_ADDR 则默认为 localhost:6379，REDIS_DB 与 REDIS_PASSWORD 为可选项。
func GetRedisClient() (*redis.Client, error) {
	redisOnce.Do(func() {
		addr := strings.TrimSpace(os.Getenv("REDIS_ADDR"))
		if addr == "" {
			addr = "localhost:6379"
		}
		password := os.Getenv("REDIS_PASSWORD")
		db := 0
		if rawDB := strings.TrimSpace(os.Getenv("REDIS_DB")); rawDB != "" {
			if parsed, err := strconv.Atoi(rawDB); err == nil {
				db = parsed
			}
		}

		client := redis.NewClient(&redis.Options{
			Addr:     addr,
			Password: password,
			DB:       db,
		})

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		if err := client.Ping(ctx).Err(); err != nil {
			redisErr = fmt.Errorf("cache: ping redis %s failed: %w", addr, err)
			_ = client.Close()
			return
		}

		redisClient = client
	})

	return redisClient, redisErr
}

// Enabled 指示 Redis 客户端是否已正确初始化。
func Enabled() bool {
	client, err := GetRedisClient()
	return err == nil && client != nil
}

// Close 释放缓存的 Redis 连接，主要用于测试场景。
func Close() error {
	if redisClient == nil {
		return nil
	}
	return redisClient.Close()
}
