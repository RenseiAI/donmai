// Package queue provides a Redis-backed work queue for the governor.
package queue

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/redis/go-redis/v9"
)

const (
	// defaultQueueKey is the Redis list key used for the governor work queue.
	defaultQueueKey = "agentfactory:governor:queue"
)

// Client wraps a *redis.Client and implements the Queue interface.
type Client struct {
	rdb      *redis.Client
	queueKey string
}

// NewClient parses url and returns a connected Client.
// Returns ErrInvalidRedisURL if url is empty or malformed.
func NewClient(url string) (*Client, error) {
	if url == "" {
		return nil, fmt.Errorf("%w: URL must not be empty", ErrInvalidRedisURL)
	}

	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidRedisURL, err.Error())
	}

	rdb := redis.NewClient(opts)
	return &Client{
		rdb:      rdb,
		queueKey: defaultQueueKey,
	}, nil
}

// Ping verifies connectivity to Redis.
// Returns ErrRedisUnavailable (wrapping the underlying error) on failure.
func (c *Client) Ping(ctx context.Context) error {
	if err := c.rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("%w: %s", ErrRedisUnavailable, err.Error())
	}
	return nil
}

// Close releases the underlying Redis connection pool.
func (c *Client) Close() error {
	return c.rdb.Close()
}

// Enqueue appends payload to the tail of the queue using RPUSH.
func (c *Client) Enqueue(ctx context.Context, payload []byte) error {
	if err := c.rdb.RPush(ctx, c.queueKey, payload).Err(); err != nil {
		return fmt.Errorf("queue: enqueue: %w", err)
	}
	return nil
}

// Peek returns the oldest payload (head of the list) without removing it.
// Returns ErrEmptyQueue when the list is empty.
func (c *Client) Peek(ctx context.Context) ([]byte, error) {
	vals, err := c.rdb.LRange(ctx, c.queueKey, 0, 0).Result()
	if err != nil {
		return nil, fmt.Errorf("queue: peek: %w", err)
	}
	if len(vals) == 0 {
		return nil, ErrEmptyQueue
	}
	return []byte(vals[0]), nil
}

// IncrDispatchCounter atomically increments the named counter key and
// returns the resulting value.
func (c *Client) IncrDispatchCounter(ctx context.Context, key string) (int64, error) {
	val, err := c.rdb.Incr(ctx, key).Result()
	if err != nil {
		return 0, fmt.Errorf("queue: incr counter %q: %w", key, err)
	}
	return val, nil
}

// GetDispatchCounter returns the current integer value stored at key.
// Returns 0 if the key does not exist.
func (c *Client) GetDispatchCounter(ctx context.Context, key string) (int64, error) {
	val, err := c.rdb.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("queue: get counter %q: %w", key, err)
	}
	n, err := strconv.ParseInt(val, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("queue: parse counter %q: %w", key, err)
	}
	return n, nil
}

// Compile-time assertion: Client must satisfy Queue.
var _ Queue = (*Client)(nil)
