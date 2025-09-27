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

// GetRedisClient returns a singleton Redis client configured from environment variables.
// REDIS_ADDR defaults to localhost:6379 when unset. REDIS_DB and REDIS_PASSWORD are optional.
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

// Enabled reports whether a usable Redis client was initialized.
func Enabled() bool {
	client, err := GetRedisClient()
	return err == nil && client != nil
}

// Close releases the cached Redis connection. Mainly useful for tests.
func Close() error {
	if redisClient == nil {
		return nil
	}
	return redisClient.Close()
}
