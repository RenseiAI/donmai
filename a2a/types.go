package a2a

import (
	"encoding/json"
	"fmt"
)

const (
	// ProtocolVersion is the A2A major/minor version emitted on the wire.
	ProtocolVersion = "1.0"
	// ProtocolBindingJSONRPC selects the JSON-RPC 2.0 binding from an Agent Card.
	ProtocolBindingJSONRPC = "JSONRPC"
	// AgentCardWellKnownPath is the protocol-defined public Agent Card URI.
	AgentCardWellKnownPath = "/.well-known/agent-card.json"
	// VersionHeader carries the requested A2A major/minor version.
	VersionHeader = "A2A-Version"
	// ExtensionsHeader carries negotiated extension URIs.
	ExtensionsHeader = "A2A-Extensions"
)

// Role identifies the author of a Message.
type Role string

const (
	// RoleUnspecified is the protocol zero value.
	RoleUnspecified Role = "ROLE_UNSPECIFIED"
	// RoleUser identifies a client-authored message.
	RoleUser Role = "ROLE_USER"
	// RoleAgent identifies an agent-authored message.
	RoleAgent Role = "ROLE_AGENT"
)

// TaskState is the protocol lifecycle state of a Task.
type TaskState string

const (
	// TaskStateUnspecified is the protocol zero value.
	TaskStateUnspecified TaskState = "TASK_STATE_UNSPECIFIED"
	// TaskStateSubmitted indicates the task was accepted but has not started.
	TaskStateSubmitted TaskState = "TASK_STATE_SUBMITTED"
	// TaskStateWorking indicates active processing.
	TaskStateWorking TaskState = "TASK_STATE_WORKING"
	// TaskStateCompleted indicates successful terminal completion.
	TaskStateCompleted TaskState = "TASK_STATE_COMPLETED"
	// TaskStateFailed indicates terminal failure.
	TaskStateFailed TaskState = "TASK_STATE_FAILED"
	// TaskStateCanceled indicates terminal cancellation.
	TaskStateCanceled TaskState = "TASK_STATE_CANCELED"
	// TaskStateInputRequired indicates progress needs more user input.
	TaskStateInputRequired TaskState = "TASK_STATE_INPUT_REQUIRED"
	// TaskStateRejected indicates the agent declined the task.
	TaskStateRejected TaskState = "TASK_STATE_REJECTED"
	// TaskStateAuthRequired indicates progress needs external authorization.
	TaskStateAuthRequired TaskState = "TASK_STATE_AUTH_REQUIRED"
)

// StopsPolling reports whether a task has reached a terminal or interrupted
// state. Input and authorization requirements stop automated polling because
// progress requires an external action.
func (s TaskState) StopsPolling() bool {
	switch s {
	case TaskStateCompleted, TaskStateFailed, TaskStateCanceled,
		TaskStateInputRequired, TaskStateRejected, TaskStateAuthRequired:
		return true
	default:
		return false
	}
}

// PartKind identifies the active member of Part's content oneof.
type PartKind uint8

const (
	// PartKindUnspecified indicates that no content member is active.
	PartKindUnspecified PartKind = iota
	// PartKindText identifies string content.
	PartKindText
	// PartKindRaw identifies base64-encoded bytes content.
	PartKindRaw
	// PartKindURL identifies URL-addressed content.
	PartKindURL
	// PartKindData identifies arbitrary structured JSON content.
	PartKindData
)

// Part is one unit of message or artifact content. Its content member is a
// discriminated oneof and can only be set through a constructor or ProtoJSON
// decoding, so a Go value cannot emit multiple content arms.
type Part struct {
	kind      PartKind
	text      string
	raw       []byte
	url       string
	data      any
	Metadata  map[string]any
	Filename  string
	MediaType string
}

// TextPart constructs a text content part.
func TextPart(text string) Part { return Part{kind: PartKindText, text: text} }

// RawPart constructs a raw bytes content part.
func RawPart(raw []byte) Part {
	return Part{kind: PartKindRaw, raw: append([]byte{}, raw...)}
}

// URLPart constructs a URL-addressed content part.
func URLPart(url string) Part { return Part{kind: PartKindURL, url: url} }

// DataPart constructs a structured data content part.
// A nil value is an active data arm and emits as `"data": null`.
func DataPart(data any) Part { return Part{kind: PartKindData, data: data} }

// Kind reports the active content member.
func (p Part) Kind() PartKind { return p.kind }

// Text returns text content when that arm is active.
func (p Part) Text() (string, bool) { return p.text, p.kind == PartKindText }

// Raw returns a copy of raw content when that arm is active.
func (p Part) Raw() ([]byte, bool) {
	if p.kind != PartKindRaw {
		return nil, false
	}
	return append([]byte(nil), p.raw...), true
}

// URL returns URL content when that arm is active.
func (p Part) URL() (string, bool) { return p.url, p.kind == PartKindURL }

// Data returns structured content when that arm is active. The boolean
// distinguishes an active JSON null from an unspecified Part.
func (p Part) Data() (any, bool) { return p.data, p.kind == PartKindData }

// Message is one unit of communication between a client and an agent.
type Message struct {
	MessageID        string         `json:"messageId"`
	ContextID        string         `json:"contextId,omitempty"`
	TaskID           string         `json:"taskId,omitempty"`
	Role             Role           `json:"role"`
	Parts            []Part         `json:"parts"`
	Metadata         map[string]any `json:"metadata,omitempty"`
	Extensions       []string       `json:"extensions,omitempty"`
	ReferenceTaskIDs []string       `json:"referenceTaskIds,omitempty"`
}

// Artifact is task output.
type Artifact struct {
	ArtifactID  string         `json:"artifactId"`
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
	Parts       []Part         `json:"parts"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	Extensions  []string       `json:"extensions,omitempty"`
}

// TaskStatus is the current state and optional explanatory message of a Task.
type TaskStatus struct {
	State     TaskState `json:"state"`
	Message   *Message  `json:"message,omitempty"`
	Timestamp Timestamp `json:"timestamp,omitempty"`
}

// Task is the protocol's core unit of action.
type Task struct {
	ID        string         `json:"id"`
	ContextID string         `json:"contextId,omitempty"`
	Status    TaskStatus     `json:"status"`
	Artifacts []Artifact     `json:"artifacts,omitempty"`
	History   []Message      `json:"history,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// SendMessageConfiguration controls synchronous versus asynchronous return.
type SendMessageConfiguration struct {
	AcceptedOutputModes        []string        `json:"acceptedOutputModes,omitempty"`
	TaskPushNotificationConfig json.RawMessage `json:"taskPushNotificationConfig,omitempty"`
	HistoryLength              *int32          `json:"historyLength,omitempty"`
	ReturnImmediately          bool            `json:"returnImmediately,omitempty"`
}

// SendMessageRequest starts or continues an interaction.
type SendMessageRequest struct {
	Message       Message                   `json:"message"`
	Configuration *SendMessageConfiguration `json:"configuration,omitempty"`
	Metadata      map[string]any            `json:"metadata,omitempty"`
}

// SendMessageResponse is a protocol oneof. Exactly one of Task or Message is
// set by a conforming server.
type SendMessageResponse struct {
	Task    *Task    `json:"task,omitempty"`
	Message *Message `json:"message,omitempty"`
}

// Validate checks the SendMessageResponse oneof.
func (r SendMessageResponse) Validate() error {
	if (r.Task == nil) == (r.Message == nil) {
		return fmt.Errorf("a2a send response: exactly one of task or message is required")
	}
	return nil
}

// GetTaskRequest retrieves the latest state of one task.
type GetTaskRequest struct {
	ID            string `json:"id"`
	HistoryLength *int32 `json:"historyLength,omitempty"`
}

// ListTasksRequest filters and pages tasks visible to the caller.
type ListTasksRequest struct {
	ContextID            string    `json:"contextId,omitempty"`
	Status               TaskState `json:"status,omitempty"`
	PageSize             *int32    `json:"pageSize,omitempty"`
	PageToken            string    `json:"pageToken,omitempty"`
	HistoryLength        *int32    `json:"historyLength,omitempty"`
	StatusTimestampAfter Timestamp `json:"statusTimestampAfter,omitempty"`
	IncludeArtifacts     *bool     `json:"includeArtifacts,omitempty"`
}

// ListTasksResponse is a page of tasks.
type ListTasksResponse struct {
	Tasks         []Task `json:"tasks"`
	NextPageToken string `json:"nextPageToken"`
	PageSize      int32  `json:"pageSize"`
	TotalSize     int32  `json:"totalSize"`
}

// CancelTaskRequest asks an agent to cancel an in-progress task.
type CancelTaskRequest struct {
	ID       string         `json:"id"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// AgentInterface declares one protocol endpoint on an Agent Card.
type AgentInterface struct {
	URL             string `json:"url"`
	ProtocolBinding string `json:"protocolBinding"`
	Tenant          string `json:"tenant,omitempty"`
	ProtocolVersion string `json:"protocolVersion"`
}

// AgentExtension declares an optional or required protocol extension.
type AgentExtension struct {
	URI         string         `json:"uri"`
	Description string         `json:"description,omitempty"`
	Required    bool           `json:"required,omitempty"`
	Params      map[string]any `json:"params,omitempty"`
}

// AgentCapabilities is the protocol capability advertisement.
type AgentCapabilities struct {
	Streaming         *bool            `json:"streaming,omitempty"`
	PushNotifications *bool            `json:"pushNotifications,omitempty"`
	Extensions        []AgentExtension `json:"extensions,omitempty"`
	ExtendedAgentCard *bool            `json:"extendedAgentCard,omitempty"`
}

// AgentSkill is the descriptive skill surface of an Agent Card. Fields not
// consumed by transport selection remain raw so clients preserve extensions.
type AgentSkill struct {
	ID                   string            `json:"id"`
	Name                 string            `json:"name"`
	Description          string            `json:"description"`
	Tags                 []string          `json:"tags"`
	Examples             []string          `json:"examples,omitempty"`
	InputModes           []string          `json:"inputModes,omitempty"`
	OutputModes          []string          `json:"outputModes,omitempty"`
	SecurityRequirements []json.RawMessage `json:"securityRequirements,omitempty"`
}

// AgentCard is the public v1 discovery document. Open-ended security and
// signature fields are preserved without making transport selection depend on
// a particular credential implementation.
type AgentCard struct {
	Name                 string                     `json:"name"`
	Description          string                     `json:"description"`
	SupportedInterfaces  []AgentInterface           `json:"supportedInterfaces"`
	Provider             json.RawMessage            `json:"provider,omitempty"`
	Version              string                     `json:"version"`
	DocumentationURL     string                     `json:"documentationUrl,omitempty"`
	Capabilities         AgentCapabilities          `json:"capabilities"`
	SecuritySchemes      map[string]json.RawMessage `json:"securitySchemes,omitempty"`
	SecurityRequirements []json.RawMessage          `json:"securityRequirements,omitempty"`
	DefaultInputModes    []string                   `json:"defaultInputModes"`
	DefaultOutputModes   []string                   `json:"defaultOutputModes"`
	Skills               []AgentSkill               `json:"skills"`
	Signatures           []json.RawMessage          `json:"signatures,omitempty"`
	IconURL              string                     `json:"iconUrl,omitempty"`
}

// RPCError is an application-level JSON-RPC error returned with an otherwise
// successful HTTP response.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("a2a rpc error %d: %s", e.Code, e.Message)
}

// TransportError reports an HTTP status or malformed transport response.
type TransportError struct {
	StatusCode int
	Message    string
	Cause      error
}

// Unwrap preserves cancellation, deadline, DNS, connection, and body-read
// causes for errors.Is/errors.As callers.
func (e *TransportError) Unwrap() error { return e.Cause }

func (e *TransportError) Error() string {
	if e.StatusCode == 0 {
		return "a2a transport: " + e.Message
	}
	return fmt.Sprintf("a2a transport: HTTP %d: %s", e.StatusCode, e.Message)
}
