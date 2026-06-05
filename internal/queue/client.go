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
	// primaryQueueKey is the canonical Redis list key for the governor work queue.
	// Introduced during the agentfactory→donmai debrand transition (DT-5).
	primaryQueueKey = "donmai:governor:queue"

	// legacyQueueKey is the previous Redis list key that was used before the
	// agentfactory→donmai debrand. During this transition release, the producer
	// writes to BOTH keys and the consumer reads from BOTH (new key first) so
	// that a one-sided deploy (only donmai updated, platform workers still on
	// the old key) remains non-breaking in either direction.
	//
	// TODO(debrand): Drop this key in the NEXT major release once all governor
	// instances and platform consumers have been restarted and drained. At that
	// point remove legacyQueueKey, the dual-write in Enqueue, and the fallback
	// in Peek.
	legacyQueueKey = "agentfactory:governor:queue"
)

// Client wraps a *redis.Client and implements the Queue interface.
type Client struct {
	rdb *redis.Client
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
	return &Client{rdb: rdb}, nil
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

// Enqueue appends payload to the tail of both queue keys using RPUSH.
//
// DT-5 dual-write: payload is written to primaryQueueKey ("donmai:governor:queue")
// AND legacyQueueKey ("agentfactory:governor:queue") so that platform workers
// still reading the old key continue to receive work during the transition window.
// Once all consumers have migrated, the legacy write will be removed.
func (c *Client) Enqueue(ctx context.Context, payload []byte) error {
	if err := c.rdb.RPush(ctx, primaryQueueKey, payload).Err(); err != nil {
		return fmt.Errorf("queue: enqueue (primary): %w", err)
	}
	if err := c.rdb.RPush(ctx, legacyQueueKey, payload).Err(); err != nil {
		return fmt.Errorf("queue: enqueue (legacy): %w", err)
	}
	return nil
}

// Peek returns the oldest payload (head of the list) without removing it.
// Returns ErrEmptyQueue when both lists are empty.
//
// DT-5 dual-read: the new primaryQueueKey ("donmai:governor:queue") is checked
// first; if it is empty, the legacyQueueKey ("agentfactory:governor:queue") is
// checked as a fallback so that consumers see items regardless of which key the
// producer wrote to. The fallback is removed once all producers have migrated.
func (c *Client) Peek(ctx context.Context) ([]byte, error) {
	vals, err := c.rdb.LRange(ctx, primaryQueueKey, 0, 0).Result()
	if err != nil {
		return nil, fmt.Errorf("queue: peek (primary): %w", err)
	}
	if len(vals) > 0 {
		return []byte(vals[0]), nil
	}

	// Primary key empty — fall back to the legacy key during the transition.
	vals, err = c.rdb.LRange(ctx, legacyQueueKey, 0, 0).Result()
	if err != nil {
		return nil, fmt.Errorf("queue: peek (legacy): %w", err)
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
