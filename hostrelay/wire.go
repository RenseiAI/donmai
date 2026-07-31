package hostrelay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// Subprotocol is the WebSocket subprotocol negotiated by host-relay-v1.
	Subprotocol = "host-relay-v1"

	// TunnelPath is the relay path hosts dial with their tunnel bearer.
	TunnelPath = "/v1/host-relay/tunnel"

	// Version is the frozen host-relay-v1 protocol version advertised in Hello.
	Version = "host-relay-v1"

	// MaxRequestBodyBytes is the largest request body accepted by v1.
	MaxRequestBodyBytes = 1 << 20

	// MaxResponseBodyBytes bounds an individual response body.
	MaxResponseBodyBytes = 1 << 20

	// MaxFrameBytes bounds a complete encoded v1 message, including JSON framing.
	MaxFrameBytes = MaxResponseBodyBytes + 64<<10

	// DefaultMaxInFlight is the default cap advertised by a host tunnel.
	DefaultMaxInFlight = 16

	// Method is the only loopback HTTP method admitted by v1.
	Method = "POST"
	// LocalRoute is the only loopback HTTP route admitted by v1.
	LocalRoute = "/v1/tools/call"
)

var (
	// PingInterval is the v1 interval between host liveness probes.
	PingInterval = 5 * time.Second
	// DeadPeerTimeout is the v1 maximum elapsed time without a relay pong.
	DeadPeerTimeout = 10 * time.Second
)

var (
	// ErrFrameTooLarge reports an encoded message that exceeds MaxFrameBytes.
	ErrFrameTooLarge = errors.New("hostrelay: frame exceeds maximum size")
	// ErrInvalidMessage reports a malformed or unsupported v1 envelope.
	ErrInvalidMessage = errors.New("hostrelay: invalid message")
)

// Type is the closed v1 message discriminator.
type Type string

const (
	// TypeHello announces a workload connection.
	TypeHello Type = "hello"
	// TypeRequest carries a correlated relay-to-host request.
	TypeRequest Type = "request"
	// TypeResponse carries a correlated host-to-relay response.
	TypeResponse Type = "response"
	// TypeCancel asks the host to cancel a request.
	TypeCancel Type = "cancel"
	// TypePing probes liveness.
	TypePing Type = "ping"
	// TypePong acknowledges a liveness probe.
	TypePong Type = "pong"
)

// Message is a typed host-relay-v1 envelope. Exactly one concrete message is
// encoded per WebSocket binary message.
type Message interface {
	Type() Type
}

// Workload identifies the process that owns one outbound tunnel. All fields are
// opaque identifiers assigned by the caller; v1 rejects an incomplete identity.
type Workload struct {
	OrgID        string `json:"orgId"`
	PoolID       string `json:"poolId"`
	WorkerHostID string `json:"workerHostId"`
	WorkloadID   string `json:"workloadId"`
}

// Hello is the mandatory first tunnel message. LocalRoute is the only local
// route v1 exposes, MaxInFlight is a positive bounded admission limit, and
// Ready reports whether the loopback host can currently serve calls.
type Hello struct {
	Workload    Workload `json:"workload"`
	Version     string   `json:"version"`
	Generation  uint64   `json:"generation"`
	LocalRoute  string   `json:"localRoute"`
	MaxInFlight int      `json:"maxInFlight"`
	Ready       bool     `json:"ready"`
}

// Type implements Message.
func (Hello) Type() Type { return TypeHello }

// Header preserves a selected HTTP header name and all of its values. Values
// are not parsed or normalized, so an Authorization value reaches loopback
// byte-for-byte unchanged.
type Header struct {
	Name   string   `json:"name"`
	Values []string `json:"values"`
}

// Request is a relay-to-host loopback request. DeadlineUnixMilli is an absolute
// Unix millisecond deadline and must be in the future when a host admits it.
type Request struct {
	RequestID         string   `json:"requestId"`
	Method            string   `json:"method"`
	Path              string   `json:"path"`
	Headers           []Header `json:"headers"`
	Body              []byte   `json:"body"`
	DeadlineUnixMilli int64    `json:"deadlineUnixMilli"`
}

// Type implements Message.
func (Request) Type() Type { return TypeRequest }

// Response is the correlated loopback response returned to the relay.
type Response struct {
	RequestID string   `json:"requestId"`
	Status    int      `json:"status"`
	Headers   []Header `json:"headers"`
	Body      []byte   `json:"body"`
}

// Type implements Message.
func (Response) Type() Type { return TypeResponse }

// Cancel asks the host to cancel a previously admitted request. It is
// best-effort: a completed response and a cancel may race.
type Cancel struct {
	RequestID string `json:"requestId"`
}

// Type implements Message.
func (Cancel) Type() Type { return TypeCancel }

// Ping is a liveness probe. Nonce is echoed unchanged in Pong.
type Ping struct {
	Nonce uint64 `json:"nonce"`
}

// Type implements Message.
func (Ping) Type() Type { return TypePing }

// Pong replies to a Ping.
type Pong struct {
	Nonce uint64 `json:"nonce"`
}

// Type implements Message.
func (Pong) Type() Type { return TypePong }

type envelope struct {
	Type    Type            `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// Encode deterministically encodes m as a bounded JSON envelope. Struct field
// order is fixed, and []byte values use standard base64 JSON encoding.
func Encode(m Message) ([]byte, error) {
	if err := validate(m); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("hostrelay: marshal %s payload: %w", m.Type(), err)
	}
	out, err := json.Marshal(envelope{Type: m.Type(), Payload: payload})
	if err != nil {
		return nil, fmt.Errorf("hostrelay: marshal envelope: %w", err)
	}
	if len(out) > MaxFrameBytes {
		return nil, ErrFrameTooLarge
	}
	return out, nil
}

// Decode strictly parses one bounded v1 envelope. Unknown message types and
// fields are rejected so a v1 peer never silently misinterprets traffic.
func Decode(data []byte) (Message, error) {
	if len(data) > MaxFrameBytes {
		return nil, ErrFrameTooLarge
	}
	var env envelope
	if err := decodeStrict(data, &env); err != nil {
		return nil, fmt.Errorf("%w: envelope: %v", ErrInvalidMessage, err)
	}
	var message Message
	switch env.Type {
	case TypeHello:
		message = &Hello{}
	case TypeRequest:
		message = &Request{}
	case TypeResponse:
		message = &Response{}
	case TypeCancel:
		message = &Cancel{}
	case TypePing:
		message = &Ping{}
	case TypePong:
		message = &Pong{}
	default:
		return nil, fmt.Errorf("%w: unknown type %q", ErrInvalidMessage, env.Type)
	}
	if err := decodeStrict(env.Payload, message); err != nil {
		return nil, fmt.Errorf("%w: %s payload: %v", ErrInvalidMessage, env.Type, err)
	}
	if err := validate(message); err != nil {
		return nil, err
	}
	return message, nil
}

func decodeStrict(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values")
	}
	return nil
}

func validate(m Message) error {
	switch m := m.(type) {
	case Hello:
		return validateHello(m)
	case *Hello:
		return validateHello(*m)
	case Request:
		return validateRequest(m)
	case *Request:
		return validateRequest(*m)
	case Response:
		return validateResponse(m)
	case *Response:
		return validateResponse(*m)
	case Cancel:
		return validateRequestID(m.RequestID)
	case *Cancel:
		return validateRequestID(m.RequestID)
	case Ping, *Ping, Pong, *Pong:
		return nil
	default:
		return fmt.Errorf("%w: unsupported Go message %T", ErrInvalidMessage, m)
	}
}

func validateHello(m Hello) error {
	if !validID(m.Workload.OrgID) || !validID(m.Workload.PoolID) || !validID(m.Workload.WorkerHostID) || !validID(m.Workload.WorkloadID) {
		return fmt.Errorf("%w: incomplete workload identity", ErrInvalidMessage)
	}
	if m.Version != Version || m.LocalRoute != LocalRoute || m.MaxInFlight < 1 || m.MaxInFlight > DefaultMaxInFlight {
		return fmt.Errorf("%w: invalid hello", ErrInvalidMessage)
	}
	return nil
}

func validateRequest(m Request) error {
	if err := validateRequestID(m.RequestID); err != nil {
		return err
	}
	if m.Method != Method || m.Path != LocalRoute || m.DeadlineUnixMilli <= 0 || len(m.Body) > MaxRequestBodyBytes {
		return fmt.Errorf("%w: invalid request", ErrInvalidMessage)
	}
	return validateHeaders(m.Headers, true)
}

func validateResponse(m Response) error {
	if err := validateRequestID(m.RequestID); err != nil {
		return err
	}
	if m.Status < 100 || m.Status > 599 || len(m.Body) > MaxResponseBodyBytes {
		return fmt.Errorf("%w: invalid response", ErrInvalidMessage)
	}
	return validateHeaders(m.Headers, false)
}

func validateRequestID(id string) error {
	if !validID(id) {
		return fmt.Errorf("%w: invalid request ID", ErrInvalidMessage)
	}
	return nil
}

func validateHeaders(headers []Header, request bool) error {
	if len(headers) > 4 {
		return fmt.Errorf("%w: too many headers", ErrInvalidMessage)
	}
	for _, header := range headers {
		name := strings.ToLower(header.Name)
		if name == "" || len(header.Values) == 0 || len(header.Values) > 2 {
			return fmt.Errorf("%w: invalid header", ErrInvalidMessage)
		}
		if request {
			if name != "authorization" && name != "content-type" && name != "accept" {
				return fmt.Errorf("%w: request header %q is not allowed", ErrInvalidMessage, header.Name)
			}
		} else if name != "content-type" && name != "www-authenticate" {
			return fmt.Errorf("%w: response header %q is not allowed", ErrInvalidMessage, header.Name)
		}
		for _, value := range header.Values {
			if !utf8.ValidString(value) || strings.ContainsAny(value, "\r\n") {
				return fmt.Errorf("%w: invalid header value", ErrInvalidMessage)
			}
		}
	}
	return nil
}

func validID(value string) bool {
	if value == "" || len(value) > 256 || !utf8.ValidString(value) {
		return false
	}
	return !strings.ContainsAny(value, "\x00\r\n")
}
