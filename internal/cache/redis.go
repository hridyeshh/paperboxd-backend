package cache

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// Client wraps a Redis client with typed helpers.
type Client struct {
	rdb *redis.Client
}

// New returns a Client backed by the given redis.Client.
func New(rdb *redis.Client) *Client {
	return &Client{rdb: rdb}
}

// ErrMiss is returned when a key is not found in the cache.
var ErrMiss = errors.New("cache miss")

// Get retrieves a raw string value. Returns ErrMiss on absence.
func (c *Client) Get(ctx context.Context, key string) (string, error) {
	val, err := c.rdb.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", ErrMiss
	}
	return val, err
}

// Set stores a raw string value with TTL.
func (c *Client) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	return c.rdb.Set(ctx, key, value, ttl).Err()
}

// Del deletes one or more keys.
func (c *Client) Del(ctx context.Context, keys ...string) error {
	return c.rdb.Del(ctx, keys...).Err()
}

// GetJSON deserialises JSON from cache into dst. Returns ErrMiss on absence.
func (c *Client) GetJSON(ctx context.Context, key string, dst any) error {
	raw, err := c.Get(ctx, key)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(raw), dst)
}

// SetJSON serialises src to JSON and stores it with TTL.
func (c *Client) SetJSON(ctx context.Context, key string, src any, ttl time.Duration) error {
	b, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return c.Set(ctx, key, string(b), ttl)
}

// Incr atomically increments an integer key and returns the new value.
func (c *Client) Incr(ctx context.Context, key string) (int64, error) {
	return c.rdb.Incr(ctx, key).Result()
}

// GetInt retrieves an integer value. Returns ErrMiss on absence.
func (c *Client) GetInt(ctx context.Context, key string) (int64, error) {
	val, err := c.rdb.Get(ctx, key).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, ErrMiss
	}
	return val, err
}

// SetInt stores an integer value with TTL.
func (c *Client) SetInt(ctx context.Context, key string, value int64, ttl time.Duration) error {
	return c.rdb.Set(ctx, key, value, ttl).Err()
}

// Expire refreshes the TTL on an existing key.
func (c *Client) Expire(ctx context.Context, key string, ttl time.Duration) error {
	return c.rdb.Expire(ctx, key, ttl).Err()
}
