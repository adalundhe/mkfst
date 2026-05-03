package cache

import (
	"context"
	"errors"
	"fmt"
	"time"

	rds "github.com/redis/go-redis/v9"
)

// RedisOpts configures NewRedisCache.
type RedisOpts struct {
	// Client is the Redis (or Valkey) client. Required. Cache does
	// not own the client — caller manages lifecycle.
	Client rds.UniversalClient

	// KeyPrefix is prepended to every key the cache reads/writes.
	// Lets multiple users of the same Redis namespace coexist.
	// Default empty (no prefix).
	KeyPrefix string
}

// NewRedisCache returns a Cache backed by Redis (or Valkey — same
// protocol). TTL uses Redis's native EX/PX expiry; DeletePrefix uses
// SCAN + UNLINK to avoid blocking the server on large prefixes.
func NewRedisCache(opts RedisOpts) (Cache, error) {
	if opts.Client == nil {
		return nil, errors.New("cache.NewRedisCache: Client is required")
	}
	return &redisCache{client: opts.Client, prefix: opts.KeyPrefix}, nil
}

type redisCache struct {
	client rds.UniversalClient
	prefix string
	closed bool
}

func (c *redisCache) k(key string) string { return c.prefix + key }

func (c *redisCache) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if c.closed {
		return nil, false, ErrClosed
	}
	val, err := c.client.Get(ctx, c.k(key)).Bytes()
	if err != nil {
		if errors.Is(err, rds.Nil) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("redis get: %w", err)
	}
	return val, true, nil
}

func (c *redisCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if c.closed {
		return ErrClosed
	}
	if err := c.client.Set(ctx, c.k(key), value, ttl).Err(); err != nil {
		return fmt.Errorf("redis set: %w", err)
	}
	return nil
}

func (c *redisCache) Delete(ctx context.Context, key string) error {
	if c.closed {
		return ErrClosed
	}
	if err := c.client.Del(ctx, c.k(key)).Err(); err != nil {
		return fmt.Errorf("redis delete: %w", err)
	}
	return nil
}

func (c *redisCache) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	if c.closed {
		return 0, ErrClosed
	}
	full := c.k(prefix)
	pattern := full + "*"

	var deleted int
	var cursor uint64
	for {
		keys, next, err := c.client.Scan(ctx, cursor, pattern, 256).Result()
		if err != nil {
			return deleted, fmt.Errorf("redis scan: %w", err)
		}
		if len(keys) > 0 {
			// UNLINK is non-blocking on the server (frees memory in
			// background); cheaper than DEL for bulk delete.
			n, err := c.client.Unlink(ctx, keys...).Result()
			if err != nil {
				return deleted, fmt.Errorf("redis unlink: %w", err)
			}
			deleted += int(n)
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return deleted, nil
}

func (c *redisCache) Close() error {
	c.closed = true
	// We don't own the client — caller manages its lifecycle.
	return nil
}
