package a2a

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// Timestamp is a ProtoJSON google.protobuf.Timestamp string.
type Timestamp string

func (t Timestamp) validate() error {
	if t == "" {
		return errors.New("timestamp is empty")
	}
	parsed, err := time.Parse(time.RFC3339Nano, string(t))
	if err != nil || parsed.Year() < 1 || parsed.Year() > 9999 {
		return fmt.Errorf("invalid ProtoJSON timestamp %q", t)
	}
	return nil
}

// MarshalJSON validates and emits a ProtoJSON timestamp string.
func (t Timestamp) MarshalJSON() ([]byte, error) {
	parsed, err := time.Parse(time.RFC3339Nano, string(t))
	if err != nil || parsed.Year() < 1 || parsed.Year() > 9999 {
		return nil, fmt.Errorf("invalid ProtoJSON timestamp %q", t)
	}
	if err := t.validate(); err != nil {
		return nil, err
	}
	return json.Marshal(formatTimestamp(parsed))
}

// UnmarshalJSON validates a ProtoJSON timestamp string.
func (t *Timestamp) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		*t = ""
		return nil
	}
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("decode timestamp: %w", err)
	}
	parsed := Timestamp(value)
	if err := parsed.validate(); err != nil {
		return err
	}
	parsedTime, _ := time.Parse(time.RFC3339Nano, value)
	*t = Timestamp(formatTimestamp(parsedTime))
	return nil
}

func formatTimestamp(value time.Time) string {
	value = value.UTC()
	base := value.Format("2006-01-02T15:04:05")
	nanos := value.Nanosecond()
	switch {
	case nanos == 0:
		return base + "Z"
	case nanos%1_000_000 == 0:
		return fmt.Sprintf("%s.%03dZ", base, nanos/1_000_000)
	case nanos%1_000 == 0:
		return fmt.Sprintf("%s.%06dZ", base, nanos/1_000)
	default:
		return fmt.Sprintf("%s.%09dZ", base, nanos)
	}
}

// MarshalJSON refuses unknown Role enum values.
func (r Role) MarshalJSON() ([]byte, error) {
	if !validRole(r) {
		return nil, fmt.Errorf("invalid A2A Role %q", r)
	}
	return json.Marshal(string(r))
}

// UnmarshalJSON refuses unknown Role enum values.
func (r *Role) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		*r = RoleUnspecified
		return nil
	}
	parsed, err := parseRole(data)
	if err != nil {
		return err
	}
	if !validRole(parsed) {
		return fmt.Errorf("invalid A2A Role %q", parsed)
	}
	*r = parsed
	return nil
}

func parseRole(data []byte) (Role, error) {
	var value string
	if err := json.Unmarshal(data, &value); err == nil {
		return Role(value), nil
	}
	var number int
	if err := json.Unmarshal(data, &number); err != nil {
		return "", fmt.Errorf("decode A2A Role: %w", err)
	}
	switch number {
	case 0:
		return RoleUnspecified, nil
	case 1:
		return RoleUser, nil
	case 2:
		return RoleAgent, nil
	default:
		return Role(numberString(number)), nil
	}
}

func validRole(role Role) bool {
	return role == RoleUnspecified || role == RoleUser || role == RoleAgent
}

// MarshalJSON refuses unknown TaskState enum values.
func (s TaskState) MarshalJSON() ([]byte, error) {
	if !validTaskState(s) {
		return nil, fmt.Errorf("invalid A2A TaskState %q", s)
	}
	return json.Marshal(string(s))
}

// UnmarshalJSON refuses unknown TaskState enum values.
func (s *TaskState) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		*s = TaskStateUnspecified
		return nil
	}
	parsed, err := parseTaskState(data)
	if err != nil {
		return err
	}
	if !validTaskState(parsed) {
		return fmt.Errorf("invalid A2A TaskState %q", parsed)
	}
	*s = parsed
	return nil
}

func parseTaskState(data []byte) (TaskState, error) {
	var value string
	if err := json.Unmarshal(data, &value); err == nil {
		return TaskState(value), nil
	}
	var number int
	if err := json.Unmarshal(data, &number); err != nil {
		return "", fmt.Errorf("decode A2A TaskState: %w", err)
	}
	states := [...]TaskState{
		TaskStateUnspecified, TaskStateSubmitted, TaskStateWorking,
		TaskStateCompleted, TaskStateFailed, TaskStateCanceled,
		TaskStateInputRequired, TaskStateRejected, TaskStateAuthRequired,
	}
	if number < 0 || number >= len(states) {
		return TaskState(numberString(number)), nil
	}
	return states[number], nil
}

func numberString(number int) string { return fmt.Sprintf("%d", number) }

func validTaskState(state TaskState) bool {
	switch state {
	case TaskStateUnspecified, TaskStateSubmitted, TaskStateWorking,
		TaskStateCompleted, TaskStateFailed, TaskStateCanceled,
		TaskStateInputRequired, TaskStateRejected, TaskStateAuthRequired:
		return true
	default:
		return false
	}
}

// MarshalJSON emits exactly one Part content arm. An active data arm remains
// present even when its value is JSON null.
func (p Part) MarshalJSON() ([]byte, error) {
	output := make(map[string]any, 4)
	switch p.kind {
	case PartKindUnspecified:
	case PartKindText:
		output["text"] = p.text
	case PartKindRaw:
		output["raw"] = p.raw
	case PartKindURL:
		output["url"] = p.url
	case PartKindData:
		output["data"] = p.data
	default:
		return nil, fmt.Errorf("invalid A2A Part kind %d", p.kind)
	}
	if p.Metadata != nil {
		output["metadata"] = p.Metadata
	}
	if p.Filename != "" {
		output["filename"] = p.Filename
	}
	if p.MediaType != "" {
		output["mediaType"] = p.MediaType
	}
	return json.Marshal(output)
}

// UnmarshalJSON parses Part in source order. Duplicate fields and multiple
// oneof arms use ProtoJSON's last-wins rule; unknown fields fail closed.
func (p *Part) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("decode A2A Part: %w", err)
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return errors.New("decode A2A Part: expected object")
	}
	var decoded Part
	for decoder.More() {
		nameToken, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("decode A2A Part field: %w", err)
		}
		name, ok := nameToken.(string)
		if !ok {
			return errors.New("decode A2A Part: field name was not a string")
		}
		switch name {
		case "text":
			field, err := decodeRawField(decoder)
			if err != nil {
				return fmt.Errorf("decode A2A Part.text: %w", err)
			}
			if isJSONNull(field) {
				continue
			}
			if err := json.Unmarshal(field, &decoded.text); err != nil {
				return fmt.Errorf("decode A2A Part.text: %w", err)
			}
			decoded.clearContent(PartKindText)
		case "raw":
			field, err := decodeRawField(decoder)
			if err != nil {
				return fmt.Errorf("decode A2A Part.raw: %w", err)
			}
			if isJSONNull(field) {
				continue
			}
			if err := json.Unmarshal(field, &decoded.raw); err != nil {
				return fmt.Errorf("decode A2A Part.raw: %w", err)
			}
			decoded.clearContent(PartKindRaw)
		case "url":
			field, err := decodeRawField(decoder)
			if err != nil {
				return fmt.Errorf("decode A2A Part.url: %w", err)
			}
			if isJSONNull(field) {
				continue
			}
			if err := json.Unmarshal(field, &decoded.url); err != nil {
				return fmt.Errorf("decode A2A Part.url: %w", err)
			}
			decoded.clearContent(PartKindURL)
		case "data":
			if err := decoder.Decode(&decoded.data); err != nil {
				return fmt.Errorf("decode A2A Part.data: %w", err)
			}
			decoded.clearContent(PartKindData)
		case "metadata":
			if err := decoder.Decode(&decoded.Metadata); err != nil {
				return fmt.Errorf("decode A2A Part.metadata: %w", err)
			}
		case "filename":
			if err := decoder.Decode(&decoded.Filename); err != nil {
				return fmt.Errorf("decode A2A Part.filename: %w", err)
			}
		case "mediaType", "media_type":
			if err := decoder.Decode(&decoded.MediaType); err != nil {
				return fmt.Errorf("decode A2A Part.mediaType: %w", err)
			}
		default:
			return fmt.Errorf("decode A2A Part: unknown field %q", name)
		}
	}
	if _, err := decoder.Token(); err != nil {
		return fmt.Errorf("decode A2A Part: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return fmt.Errorf("decode A2A Part: %w", err)
	}
	*p = decoded
	return nil
}

func decodeRawField(decoder *json.Decoder) (json.RawMessage, error) {
	var field json.RawMessage
	if err := decoder.Decode(&field); err != nil {
		return nil, err
	}
	return field, nil
}

func isJSONNull(data []byte) bool {
	return bytes.Equal(bytes.TrimSpace(data), []byte("null"))
}

func (p *Part) clearContent(kind PartKind) {
	p.kind = kind
	if kind != PartKindText {
		p.text = ""
	}
	if kind != PartKindRaw {
		p.raw = nil
	}
	if kind != PartKindURL {
		p.url = ""
	}
	if kind != PartKindData {
		p.data = nil
	}
}

func unmarshalStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}

func normalizeProtoFieldNames(data []byte, aliases map[string]string) ([]byte, error) {
	if len(aliases) == 0 {
		return data, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return nil, errors.New("expected ProtoJSON object")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		nameToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name, ok := nameToken.(string)
		if !ok {
			return nil, errors.New("ProtoJSON field name was not a string")
		}
		field, err := decodeRawField(decoder)
		if err != nil {
			return nil, err
		}
		if normalized, ok := aliases[name]; ok {
			name = normalized
		}
		// Assignment in source order implements last-wins for exact keys and
		// alternate proto/lowerCamel spellings before strict decoding.
		fields[name] = field
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.New("unexpected trailing JSON value")
}

// The wire aliases below avoid recursive UnmarshalJSON calls while applying
// one strict unknown-field posture throughout the public protocol model.

// UnmarshalJSON decodes Message with strict ProtoJSON field semantics.
func (m *Message) UnmarshalJSON(data []byte) error {
	type wire Message
	return unmarshalAlias(data, (*wire)(m), messageAliases)
}

// UnmarshalJSON decodes Artifact with strict ProtoJSON field semantics.
func (a *Artifact) UnmarshalJSON(data []byte) error {
	type wire Artifact
	return unmarshalAlias(data, (*wire)(a), artifactAliases)
}

// UnmarshalJSON decodes TaskStatus with strict ProtoJSON field semantics.
func (s *TaskStatus) UnmarshalJSON(data []byte) error {
	type wire TaskStatus
	return unmarshalAlias(data, (*wire)(s), nil)
}

// UnmarshalJSON decodes Task with strict ProtoJSON field semantics.
func (t *Task) UnmarshalJSON(data []byte) error {
	type wire Task
	return unmarshalAlias(data, (*wire)(t), taskAliases)
}

// UnmarshalJSON decodes SendMessageConfiguration with strict ProtoJSON fields.
func (c *SendMessageConfiguration) UnmarshalJSON(data []byte) error {
	type wire SendMessageConfiguration
	return unmarshalAlias(data, (*wire)(c), sendConfigurationAliases)
}

// UnmarshalJSON decodes SendMessageRequest with strict ProtoJSON fields.
func (r *SendMessageRequest) UnmarshalJSON(data []byte) error {
	type wire SendMessageRequest
	return unmarshalAlias(data, (*wire)(r), nil)
}

// UnmarshalJSON decodes and validates the SendMessageResponse oneof.
func (r *SendMessageResponse) UnmarshalJSON(data []byte) error {
	type wire SendMessageResponse
	if err := unmarshalAlias(data, (*wire)(r), nil); err != nil {
		return err
	}
	return r.Validate()
}

// UnmarshalJSON decodes GetTaskRequest with strict ProtoJSON fields.
func (r *GetTaskRequest) UnmarshalJSON(data []byte) error {
	type wire GetTaskRequest
	return unmarshalAlias(data, (*wire)(r), getTaskAliases)
}

// UnmarshalJSON decodes ListTasksRequest with strict ProtoJSON fields.
func (r *ListTasksRequest) UnmarshalJSON(data []byte) error {
	type wire ListTasksRequest
	return unmarshalAlias(data, (*wire)(r), listTasksAliases)
}

// UnmarshalJSON decodes ListTasksResponse with strict ProtoJSON fields.
func (r *ListTasksResponse) UnmarshalJSON(data []byte) error {
	type wire ListTasksResponse
	return unmarshalAlias(data, (*wire)(r), listTasksResponseAliases)
}

// UnmarshalJSON decodes CancelTaskRequest with strict ProtoJSON fields.
func (r *CancelTaskRequest) UnmarshalJSON(data []byte) error {
	type wire CancelTaskRequest
	return unmarshalAlias(data, (*wire)(r), nil)
}

// UnmarshalJSON decodes AgentInterface with strict ProtoJSON fields.
func (i *AgentInterface) UnmarshalJSON(data []byte) error {
	type wire AgentInterface
	return unmarshalAlias(data, (*wire)(i), agentInterfaceAliases)
}

// UnmarshalJSON decodes AgentExtension with strict ProtoJSON fields.
func (e *AgentExtension) UnmarshalJSON(data []byte) error {
	type wire AgentExtension
	return unmarshalAlias(data, (*wire)(e), nil)
}

// UnmarshalJSON decodes AgentCapabilities with strict ProtoJSON fields.
func (c *AgentCapabilities) UnmarshalJSON(data []byte) error {
	type wire AgentCapabilities
	return unmarshalAlias(data, (*wire)(c), agentCapabilitiesAliases)
}

// UnmarshalJSON decodes AgentSkill with strict ProtoJSON fields.
func (s *AgentSkill) UnmarshalJSON(data []byte) error {
	type wire AgentSkill
	return unmarshalAlias(data, (*wire)(s), agentSkillAliases)
}

// UnmarshalJSON decodes AgentCard with strict ProtoJSON fields.
func (c *AgentCard) UnmarshalJSON(data []byte) error {
	type wire AgentCard
	return unmarshalAlias(data, (*wire)(c), agentCardAliases)
}

// UnmarshalJSON decodes RPCError without accepting unknown envelope members.
func (e *RPCError) UnmarshalJSON(data []byte) error {
	type wire RPCError
	return unmarshalAlias(data, (*wire)(e), nil)
}

func unmarshalAlias(data []byte, target any, aliases map[string]string) error {
	normalized, err := normalizeProtoFieldNames(data, aliases)
	if err != nil {
		return fmt.Errorf("decode A2A ProtoJSON: %w", err)
	}
	if err := unmarshalStrict(normalized, target); err != nil {
		return fmt.Errorf("decode A2A ProtoJSON: %w", err)
	}
	return nil
}

var (
	messageAliases = map[string]string{
		"message_id": "messageId", "context_id": "contextId", "task_id": "taskId",
		"reference_task_ids": "referenceTaskIds",
	}
	artifactAliases          = map[string]string{"artifact_id": "artifactId"}
	taskAliases              = map[string]string{"context_id": "contextId"}
	sendConfigurationAliases = map[string]string{
		"accepted_output_modes": "acceptedOutputModes", "task_push_notification_config": "taskPushNotificationConfig",
		"history_length": "historyLength", "return_immediately": "returnImmediately",
	}
	getTaskAliases   = map[string]string{"history_length": "historyLength"}
	listTasksAliases = map[string]string{
		"context_id": "contextId", "page_size": "pageSize", "page_token": "pageToken",
		"history_length": "historyLength", "status_timestamp_after": "statusTimestampAfter",
		"include_artifacts": "includeArtifacts",
	}
	listTasksResponseAliases = map[string]string{
		"next_page_token": "nextPageToken", "page_size": "pageSize", "total_size": "totalSize",
	}
	agentInterfaceAliases = map[string]string{
		"protocol_binding": "protocolBinding", "protocol_version": "protocolVersion",
	}
	agentCapabilitiesAliases = map[string]string{
		"push_notifications": "pushNotifications", "extended_agent_card": "extendedAgentCard",
	}
	agentSkillAliases = map[string]string{
		"input_modes": "inputModes", "output_modes": "outputModes", "security_requirements": "securityRequirements",
	}
	agentCardAliases = map[string]string{
		"supported_interfaces": "supportedInterfaces", "documentation_url": "documentationUrl",
		"security_schemes": "securitySchemes", "security_requirements": "securityRequirements",
		"default_input_modes": "defaultInputModes", "default_output_modes": "defaultOutputModes",
		"icon_url": "iconUrl",
	}
)
