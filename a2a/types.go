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

// Part is one unit of message or artifact content. Exactly one of Text, Raw,
// URL, or Data should be present. Pointers retain the oneof distinction when a
// valid value is the type's zero value.
type Part struct {
	Text      *string        `json:"text,omitempty"`
	Raw       *[]byte        `json:"raw,omitempty"`
	URL       *string        `json:"url,omitempty"`
	Data      *any           `json:"data,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	Filename  string         `json:"filename,omitempty"`
	MediaType string         `json:"mediaType,omitempty"`
}

// TextPart constructs a text content part.
func TextPart(text string) Part { return Part{Text: &text} }

// DataPart constructs a structured data content part.
func DataPart(data any) Part { return Part{Data: &data} }

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
	Timestamp string    `json:"timestamp,omitempty"`
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
	AcceptedOutputModes []string `json:"acceptedOutputModes,omitempty"`
	HistoryLength       *int32   `json:"historyLength,omitempty"`
	ReturnImmediately   bool     `json:"returnImmediately,omitempty"`
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
	StatusTimestampAfter string    `json:"statusTimestampAfter,omitempty"`
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
}

func (e *TransportError) Error() string {
	if e.StatusCode == 0 {
		return "a2a transport: " + e.Message
	}
	return fmt.Sprintf("a2a transport: HTTP %d: %s", e.StatusCode, e.Message)
}
