package queue

import "context"

// Queue defines the interface for the governor's work queue operations.
// Implementations must be safe for concurrent use.
type Queue interface {
	// Ping verifies connectivity to the backing store.
	Ping(ctx context.Context) error

	// Enqueue appends payload to the tail of the queue.
	Enqueue(ctx context.Context, payload []byte) error

	// Peek returns the oldest payload without removing it.
	// Returns ErrEmptyQueue when the queue is empty.
	Peek(ctx context.Context) ([]byte, error)

	// IncrDispatchCounter atomically increments the named counter and
	// returns the new value.
	IncrDispatchCounter(ctx context.Context, key string) (int64, error)

	// GetDispatchCounter returns the current value of the named counter,
	// or 0 if it has never been set.
	GetDispatchCounter(ctx context.Context, key string) (int64, error)

	// Close releases resources held by the client.
	Close() error
}
