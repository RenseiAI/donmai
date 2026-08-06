// Package executioncell defines the versioned, OSS execution-cell wire
// contract. It is additive: callers adapt existing queued work into a
// DispatchIntent sidecar while the operational payload remains untouched.
//
//go:generate go run ./gen
package executioncell
