// Package a2a implements the A2A Protocol v1.0 JSON-RPC client binding.
//
// It intentionally does not implement a legacy v0.x fallback. Callers select
// a v1 JSONRPC interface from an Agent Card, authenticate through an injected
// authorization provider, and use the protocol's native request and response
// types without a product-specific envelope.
package a2a
