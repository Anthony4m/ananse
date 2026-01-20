package database

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisDB wraps a Redis client
type RedisDB struct {
	Client *redis.Client
}

// RedisConfig holds Redis configuration
type RedisConfig struct {
	Host     string
	Port     string
	Password string
	DB       int
}

// TokenData represents token information stored in Redis
type TokenData struct {
	UserID    string `json:"user_id"`
	ExpiresAt int64  `json:"expires_at"`
}

// NewRedisDB creates a new Redis client
func NewRedisDB(ctx context.Context, cfg RedisConfig) (*RedisDB, error) {
	client := redis.NewClient(&redis.Options{
		Addr:         fmt.Sprintf("%s:%s", cfg.Host, cfg.Port),
		Password:     cfg.Password,
		DB:           cfg.DB,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
		PoolSize:     10,
		MinIdleConns: 2,
	})

	// Verify connection
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	log.Printf("[database] Connected to Redis at %s:%s", cfg.Host, cfg.Port)

	return &RedisDB{Client: client}, nil
}

// NewRedisDBFromEnv creates a Redis connection from environment variables
func NewRedisDBFromEnv(ctx context.Context) (*RedisDB, error) {
	cfg := RedisConfig{
		Host:     getEnv("REDIS_HOST", "redis"),
		Port:     getEnv("REDIS_PORT", "6379"),
		Password: getEnv("REDIS_PASSWORD", "ananse-redis-pw"),
		DB:       0,
	}
	return NewRedisDB(ctx, cfg)
}

// Close closes the Redis connection
func (r *RedisDB) Close() error {
	if r.Client != nil {
		log.Println("[database] Redis connection closed")
		return r.Client.Close()
	}
	return nil
}

// HealthCheck verifies Redis connectivity
func (r *RedisDB) HealthCheck(ctx context.Context) error {
	return r.Client.Ping(ctx).Err()
}

// SetToken stores a token with the given user ID and TTL
func (r *RedisDB) SetToken(ctx context.Context, token, userID string, ttl time.Duration) error {
	data := TokenData{
		UserID:    userID,
		ExpiresAt: time.Now().Add(ttl).Unix(),
	}

	jsonData, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("failed to marshal token data: %w", err)
	}

	key := fmt.Sprintf("tokens:%s", token)
	return r.Client.Set(ctx, key, jsonData, ttl).Err()
}

// GetToken retrieves token data for the given token
func (r *RedisDB) GetToken(ctx context.Context, token string) (*TokenData, error) {
	key := fmt.Sprintf("tokens:%s", token)

	data, err := r.Client.Get(ctx, key).Result()
	if err == redis.Nil {
		return nil, nil // Token not found
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get token: %w", err)
	}

	var tokenData TokenData
	if err := json.Unmarshal([]byte(data), &tokenData); err != nil {
		return nil, fmt.Errorf("failed to unmarshal token data: %w", err)
	}

	return &tokenData, nil
}

// DeleteToken removes a token
func (r *RedisDB) DeleteToken(ctx context.Context, token string) error {
	key := fmt.Sprintf("tokens:%s", token)
	return r.Client.Del(ctx, key).Err()
}

// RefreshToken extends a token's TTL
func (r *RedisDB) RefreshToken(ctx context.Context, token string, ttl time.Duration) error {
	key := fmt.Sprintf("tokens:%s", token)
	return r.Client.Expire(ctx, key, ttl).Err()
}

// WaitForRedis waits for Redis to become available
func WaitForRedis(ctx context.Context, cfg RedisConfig, maxRetries int, retryInterval time.Duration) (*RedisDB, error) {
	var rdb *RedisDB
	var err error

	for i := 0; i < maxRetries; i++ {
		rdb, err = NewRedisDB(ctx, cfg)
		if err == nil {
			return rdb, nil
		}

		log.Printf("[database] Waiting for Redis... attempt %d/%d: %v", i+1, maxRetries, err)
		time.Sleep(retryInterval)
	}

	return nil, fmt.Errorf("failed to connect to Redis after %d attempts: %w", maxRetries, err)
}

// WaitForRedisFromEnv waits for Redis using environment variables
func WaitForRedisFromEnv(ctx context.Context, maxRetries int) (*RedisDB, error) {
	cfg := RedisConfig{
		Host:     getEnv("REDIS_HOST", "redis"),
		Port:     getEnv("REDIS_PORT", "6379"),
		Password: getEnv("REDIS_PASSWORD", "ananse-redis-pw"),
		DB:       0,
	}
	return WaitForRedis(ctx, cfg, maxRetries, 2*time.Second)
}
