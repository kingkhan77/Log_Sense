package db

import (
	"context"
	"fmt"
	"os"

	"github.com/redis/go-redis/v9"
)

// RedisClient wraps go-redis so callers depend on our abstraction.
type RedisClient struct {
	Client *redis.Client
}

// LogEventQueueKey is the Redis list key used as our ingest queue.
// Using a named constant prevents typos from causing silent queue mismatches
// between the ingest handler (LPUSH) and the worker (BRPOP/LRANGE).
//
// Why a Redis List?
//   - LPUSH adds to the left (head) — O(1)
//   - Worker uses LRANGE + LTRIM to drain in bulk — O(N) but in one round-trip
//   - This is a classic producer/consumer queue pattern with Redis
const LogEventQueueKey = "logsense:queue:log_events"

// NewRedis creates a Redis client from REDIS_URL env var.
//
// go-redis uses a single *redis.Client for pooled connections internally —
// you don't need to manage a pool yourself unlike with raw TCP connections.
func NewRedis(ctx context.Context) (*RedisClient, error) {
	addr := os.Getenv("REDIS_URL")
	if addr == "" {
		addr = "redis:6380" // default for Docker Compose service name
	}

	client := redis.NewClient(&redis.Options{
		Addr: addr,

		// PoolSize controls how many TCP connections go-redis maintains.
		// 10 is a reasonable default for a small service — enough for
		// concurrent handler goroutines + the worker without contention.
		PoolSize: 10,
	})

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis ping failed: %w", err)
	}

	return &RedisClient{Client: client}, nil
}

// Close shuts down the Redis connection pool gracefully.
func (r *RedisClient) Close() error {
	return r.Client.Close()
}

// Ping is used by the health endpoint.
func (r *RedisClient) Ping(ctx context.Context) error {
	return r.Client.Ping(ctx).Err()
}

// PushEvent pushes a JSON-encoded log event onto the LEFT side of the queue list.
// The worker will drain from the right, giving us FIFO ordering.
func (r *RedisClient) PushEvent(ctx context.Context, payload []byte) error {
	// LPUSH: O(1). We push raw JSON bytes as a string.
	// Redis stores everything as bytes internally anyway.
	if err := r.Client.LPush(ctx, LogEventQueueKey, payload).Err(); err != nil {
		return fmt.Errorf("redis LPUSH failed: %w", err)
	}
	return nil
}

// DrainEvents atomically reads up to `limit` events from the queue and
// removes them. Uses LRANGE + LTRIM in a pipeline to avoid a race condition
// where two workers could read the same events.
//
// Why pipeline and not two separate calls?
// Without pipelining, another goroutine (or future second instance) could
// LRANGE the same slice before we LTRIM. A pipeline sends both commands
// to Redis in one round-trip and Redis executes them sequentially.
func (r *RedisClient) DrainEvents(ctx context.Context, limit int) ([]string, error) {
	pipe := r.Client.Pipeline()

	// LRANGE 0 to (limit-1): reads up to `limit` items from the right end.
	// We read from the right (newest pushed last = rightmost in LPUSH model).
	lrangeCmd := pipe.LRange(ctx, LogEventQueueKey, 0, int64(limit-1))

	// LTRIM keeps only the elements NOT in our range — i.e., removes what we just read.
	pipe.LTrim(ctx, LogEventQueueKey, int64(limit), -1)

	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, fmt.Errorf("redis drain pipeline failed: %w", err)
	}

	return lrangeCmd.Val(), nil
}

// QueueLength returns the current number of events waiting to be processed.
// Useful for monitoring and the health endpoint.
func (r *RedisClient) QueueLength(ctx context.Context) (int64, error) {
	return r.Client.LLen(ctx, LogEventQueueKey).Result()
}