package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	defaultRequestTimeout = 30 * time.Second
	defaultPollInterval   = time.Second
	maxResponseBytes      = 4 << 20
)

// AuthorizationProvider returns the complete Authorization header value for
// one request. Resolving per request permits short-lived credential rotation.
type AuthorizationProvider func(context.Context) (string, error)

// Client implements the A2A v1.0 JSON-RPC binding.
type Client struct {
	endpoint      string
	tenant        string
	extensions    []string
	httpClient    *http.Client
	authorization AuthorizationProvider
	pollInterval  time.Duration
	nextID        atomic.Uint64
}

// ActivatedExtensions returns the card-advertised extension intersection this
// client sends on every request. The returned slice is caller-owned.
func (c *Client) ActivatedExtensions() []string {
	return append([]string(nil), c.extensions...)
}

// Option configures a Client.
type Option func(*Client) error

// WithHTTPClient installs the caller-owned HTTP client.
func WithHTTPClient(client *http.Client) Option {
	return func(c *Client) error {
		if client == nil {
			return errors.New("a2a client: HTTP client is nil")
		}
		c.httpClient = client
		return nil
	}
}

// WithAuthorizationProvider resolves fresh request authorization. The returned
// value is sent verbatim as the Authorization header.
func WithAuthorizationProvider(provider AuthorizationProvider) Option {
	return func(c *Client) error {
		if provider == nil {
			return errors.New("a2a client: authorization provider is nil")
		}
		c.authorization = provider
		return nil
	}
}

// WithBearerToken is a convenience for a static bearer credential.
func WithBearerToken(token string) Option {
	return func(c *Client) error {
		if strings.TrimSpace(token) == "" {
			return errors.New("a2a client: bearer token is empty")
		}
		c.authorization = func(context.Context) (string, error) {
			return "Bearer " + token, nil
		}
		return nil
	}
}

// WithTenant configures an opaque tenant selector for a direct endpoint.
// NewClientFromCard stamps the selected interface's tenant automatically.
func WithTenant(tenant string) Option {
	return func(c *Client) error {
		c.tenant = tenant
		return nil
	}
}

// WithExtensions sends extension URIs in the standard A2A-Extensions header.
func WithExtensions(extensions ...string) Option {
	return func(c *Client) error {
		c.extensions = append([]string(nil), extensions...)
		for _, extension := range c.extensions {
			parsed, err := url.Parse(extension)
			if err != nil || parsed.Scheme == "" || strings.Contains(extension, ",") {
				return fmt.Errorf("a2a client: invalid extension URI %q", extension)
			}
		}
		return nil
	}
}

// WithPollInterval configures WaitTask's interval.
func WithPollInterval(interval time.Duration) Option {
	return func(c *Client) error {
		if interval <= 0 {
			return errors.New("a2a client: poll interval must be positive")
		}
		c.pollInterval = interval
		return nil
	}
}

// NewClient creates a strict v1 JSON-RPC client for an already-selected
// endpoint. It never rewrites method names or falls back to a v0.x binding.
func NewClient(endpoint string, options ...Option) (*Client, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed == nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("a2a client: endpoint must be an absolute HTTP(S) URL")
	}
	if parsed.User != nil || parsed.Fragment != "" {
		return nil, fmt.Errorf("a2a client: endpoint must not contain userinfo or a fragment")
	}

	c := &Client{
		endpoint:     parsed.String(),
		httpClient:   &http.Client{Timeout: defaultRequestTimeout},
		pollInterval: defaultPollInterval,
	}
	for _, option := range options {
		if option == nil {
			return nil, errors.New("a2a client: option is nil")
		}
		if err := option(c); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// NewClientFromCard selects the first preferred JSONRPC v1.0-compatible
// interface. Patch suffixes are accepted but the wire header remains 1.0. A
// card without that major/minor interface is refused instead of degrading.
func NewClientFromCard(card AgentCard, options ...Option) (*Client, error) {
	for _, candidate := range card.SupportedInterfaces {
		if candidate.ProtocolBinding != ProtocolBindingJSONRPC || !supportsProtocolVersion(candidate.ProtocolVersion) {
			continue
		}
		client, err := NewClient(candidate.URL, options...)
		if err != nil {
			return nil, err
		}
		// AgentInterface.tenant is the server-advertised routing selector. A
		// caller option must never replace it with a different authority.
		client.tenant = candidate.Tenant
		active, err := negotiateExtensions(card.Capabilities.Extensions, client.extensions)
		if err != nil {
			return nil, err
		}
		client.extensions = active
		return client, nil
	}
	return nil, fmt.Errorf("a2a client: Agent Card has no %s v%s interface", ProtocolBindingJSONRPC, ProtocolVersion)
}

// SendMessage invokes the v1 SendMessage method.
func (c *Client) SendMessage(ctx context.Context, request SendMessageRequest) (*SendMessageResponse, error) {
	if request.Message.MessageID == "" || request.Message.Role == RoleUnspecified || len(request.Message.Parts) == 0 {
		return nil, errors.New("a2a SendMessage: messageId, role, and at least one part are required")
	}
	params, err := withTenant(c.tenant, request)
	if err != nil {
		return nil, fmt.Errorf("a2a SendMessage: encode params: %w", err)
	}
	var response SendMessageResponse
	if err := c.call(ctx, "SendMessage", params, &response); err != nil {
		return nil, err
	}
	if err := response.Validate(); err != nil {
		return nil, &TransportError{Message: err.Error()}
	}
	return &response, nil
}

// GetTask invokes the v1 GetTask method.
func (c *Client) GetTask(ctx context.Context, request GetTaskRequest) (*Task, error) {
	if request.ID == "" {
		return nil, errors.New("a2a GetTask: id is required")
	}
	params, err := withTenant(c.tenant, request)
	if err != nil {
		return nil, fmt.Errorf("a2a GetTask: encode params: %w", err)
	}
	var task Task
	if err := c.call(ctx, "GetTask", params, &task); err != nil {
		return nil, err
	}
	return &task, nil
}

// ListTasks invokes the v1 ListTasks method.
func (c *Client) ListTasks(ctx context.Context, request ListTasksRequest) (*ListTasksResponse, error) {
	if request.PageSize != nil && (*request.PageSize < 1 || *request.PageSize > 100) {
		return nil, errors.New("a2a ListTasks: pageSize must be between 1 and 100")
	}
	params, err := withTenant(c.tenant, request)
	if err != nil {
		return nil, fmt.Errorf("a2a ListTasks: encode params: %w", err)
	}
	var response ListTasksResponse
	if err := c.call(ctx, "ListTasks", params, &response); err != nil {
		return nil, err
	}
	return &response, nil
}

// CancelTask invokes the v1 CancelTask method.
func (c *Client) CancelTask(ctx context.Context, request CancelTaskRequest) (*Task, error) {
	if request.ID == "" {
		return nil, errors.New("a2a CancelTask: id is required")
	}
	params, err := withTenant(c.tenant, request)
	if err != nil {
		return nil, fmt.Errorf("a2a CancelTask: encode params: %w", err)
	}
	var task Task
	if err := c.call(ctx, "CancelTask", params, &task); err != nil {
		return nil, err
	}
	return &task, nil
}

// WaitTask polls GetTask until the task becomes terminal/interrupted or ctx is
// canceled. The first GetTask call happens immediately.
func (c *Client) WaitTask(ctx context.Context, id string) (*Task, error) {
	if id == "" {
		return nil, errors.New("a2a WaitTask: id is required")
	}
	for {
		task, err := c.GetTask(ctx, GetTaskRequest{ID: id})
		if err != nil {
			return nil, err
		}
		if task.Status.State.StopsPolling() {
			return task, nil
		}
		timer := time.NewTimer(c.pollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, fmt.Errorf("a2a WaitTask %q: %w", id, ctx.Err())
		case <-timer.C:
		}
	}
}

type requestEnvelope struct {
	JSONRPC string `json:"jsonrpc"`
	ID      string `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type responseEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *RPCError       `json:"error"`
}

func (c *Client) call(ctx context.Context, method string, params any, target any) error {
	id := strconv.FormatUint(c.nextID.Add(1), 10)
	body, err := json.Marshal(requestEnvelope{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return fmt.Errorf("a2a %s: encode request: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("a2a %s: create request: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set(VersionHeader, ProtocolVersion)
	if len(c.extensions) > 0 {
		req.Header.Set(ExtensionsHeader, strings.Join(c.extensions, ","))
	}
	if c.authorization != nil {
		authorization, err := c.authorization(ctx)
		if err != nil {
			return fmt.Errorf("a2a %s: resolve authorization: %w", method, err)
		}
		if strings.TrimSpace(authorization) == "" {
			return fmt.Errorf("a2a %s: resolve authorization: empty value", method)
		}
		req.Header.Set("Authorization", authorization)
	}

	response, err := c.httpClient.Do(req)
	if err != nil {
		return &TransportError{Message: "send request failed", Cause: err}
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return &TransportError{StatusCode: response.StatusCode, Message: "read response failed", Cause: err}
	}
	if len(raw) > maxResponseBytes {
		return &TransportError{StatusCode: response.StatusCode, Message: "response exceeded 4 MiB"}
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return &TransportError{StatusCode: response.StatusCode, Message: http.StatusText(response.StatusCode)}
	}

	var envelope responseEnvelope
	if err := unmarshalStrict(raw, &envelope); err != nil {
		return &TransportError{StatusCode: response.StatusCode, Message: "response was not valid JSON-RPC JSON"}
	}
	if envelope.JSONRPC != "2.0" {
		return &TransportError{StatusCode: response.StatusCode, Message: "response jsonrpc must equal 2.0"}
	}
	var responseID string
	if err := json.Unmarshal(envelope.ID, &responseID); err != nil || responseID != id {
		return &TransportError{StatusCode: response.StatusCode, Message: "response id did not match request"}
	}
	hasResult := len(envelope.Result) > 0 && string(envelope.Result) != "null"
	if hasResult == (envelope.Error != nil) {
		return &TransportError{StatusCode: response.StatusCode, Message: "response must contain exactly one of result or error"}
	}
	if envelope.Error != nil {
		return envelope.Error
	}
	if err := json.Unmarshal(envelope.Result, target); err != nil {
		return &TransportError{StatusCode: response.StatusCode, Message: "result did not match the expected A2A response"}
	}
	return nil
}

func supportsProtocolVersion(version string) bool {
	parts := strings.Split(version, ".")
	if len(parts) != 2 && len(parts) != 3 {
		return false
	}
	if parts[0] != "1" || parts[1] != "0" {
		return false
	}
	if len(parts) == 2 {
		return true
	}
	if parts[2] == "" {
		return false
	}
	for _, digit := range parts[2] {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

func withTenant(tenant string, value any) (map[string]any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	params := make(map[string]any)
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, err
	}
	if tenant != "" {
		params["tenant"] = tenant
	}
	return params, nil
}

func negotiateExtensions(advertised []AgentExtension, implemented []string) ([]string, error) {
	implementedSet := make(map[string]struct{}, len(implemented))
	for _, extension := range implemented {
		implementedSet[extension] = struct{}{}
	}
	active := make([]string, 0, len(implemented))
	for _, extension := range advertised {
		_, ok := implementedSet[extension.URI]
		if extension.Required && !ok {
			return nil, fmt.Errorf("a2a client: required extension %q is not implemented", extension.URI)
		}
		if ok {
			active = append(active, extension.URI)
		}
	}
	return active, nil
}
