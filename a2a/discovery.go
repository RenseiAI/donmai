package a2a

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// DiscoveryError reports that Agent Card discovery failed. It wraps the
// underlying discovery failure without changing its rendered text, so
// embedders can distinguish discovery from later JSON-RPC transport
// failures with errors.As instead of matching error strings. Use
// errors.As to match this type; the wrapped TransportError and its cause
// remain reachable through the same chain.
type DiscoveryError struct {
	Err error
}

// AgentCardDiscoveryError aliases DiscoveryError for callers searching by
// the Agent Card document name. It is the same identity, not a second one.
type AgentCardDiscoveryError = DiscoveryError

// Error returns the underlying discovery failure text unchanged.
func (e *DiscoveryError) Error() string {
	if e == nil || e.Err == nil {
		return "a2a discovery: Agent Card discovery failed"
	}
	return e.Err.Error()
}

// Unwrap returns the underlying discovery failure so errors.As and
// errors.Is continue to reach the nested TransportError and its cause.
func (e *DiscoveryError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// FetchAgentCard retrieves and strictly decodes one v1 Agent Card. cardURL is
// explicit because resolving human handles to card URLs belongs to the caller's
// registry, not to the A2A protocol.
func FetchAgentCard(ctx context.Context, cardURL string, client *http.Client) (*AgentCard, error) {
	parsed, err := url.Parse(cardURL)
	if err != nil || parsed == nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, &DiscoveryError{Err: fmt.Errorf("a2a discovery: card URL must be an absolute HTTP(S) URL")}
	}
	if parsed.User != nil || parsed.Fragment != "" {
		return nil, &DiscoveryError{Err: fmt.Errorf("a2a discovery: card URL must not contain userinfo or a fragment")}
	}
	if client == nil {
		client = &http.Client{Timeout: defaultRequestTimeout}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, &DiscoveryError{Err: fmt.Errorf("a2a discovery: create request: %w", err)}
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set(VersionHeader, ProtocolVersion)
	response, err := client.Do(request)
	if err != nil {
		return nil, &DiscoveryError{Err: &TransportError{Message: "fetch Agent Card failed", Cause: err}}
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, &DiscoveryError{Err: &TransportError{StatusCode: response.StatusCode, Message: "read Agent Card failed", Cause: err}}
	}
	if len(raw) > maxResponseBytes {
		return nil, &DiscoveryError{Err: &TransportError{StatusCode: response.StatusCode, Message: "Agent Card exceeded 4 MiB"}}
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, &DiscoveryError{Err: &TransportError{StatusCode: response.StatusCode, Message: http.StatusText(response.StatusCode)}}
	}
	var card AgentCard
	if err := json.Unmarshal(raw, &card); err != nil {
		return nil, &DiscoveryError{Err: &TransportError{StatusCode: response.StatusCode, Message: "Agent Card was not valid A2A v1 ProtoJSON", Cause: err}}
	}
	return &card, nil
}
