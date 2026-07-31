// Package hostrelay defines the host-relay-v1 WebSocket wire contract.
//
// The package is deliberately transport-neutral: callers carry each encoded
// Message as one binary WebSocket message. It contains no relay endpoint,
// credential storage, or retry policy. Host tunnel clients and relays import
// this package so the bounded v1 envelope has one implementation.
package hostrelay
