package queue

import "errors"

// ErrInvalidRedisURL is returned when the Redis URL is empty or cannot be parsed.
var ErrInvalidRedisURL = errors.New("queue: invalid redis URL")

// ErrRedisUnavailable is returned when the Redis server cannot be reached.
var ErrRedisUnavailable = errors.New("queue: redis unavailable")

// ErrEmptyQueue is returned when Peek is called on an empty queue.
var ErrEmptyQueue = errors.New("queue: empty queue")
