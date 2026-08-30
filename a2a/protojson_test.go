package a2a

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPartProtoJSONOneofAndNullSemantics(t *testing.T) {
	t.Parallel()
	t.Run("active data null round trip", func(t *testing.T) {
		part := DataPart(nil)
		raw, err := json.Marshal(part)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if string(raw) != `{"data":null}` {
			t.Fatalf("wire = %s, want active data null", raw)
		}
		var decoded Part
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		value, ok := decoded.Data()
		if !ok || value != nil {
			t.Fatalf("Data = (%#v, %v), want active null", value, ok)
		}
	})

	t.Run("multiple arms use last wins and marshal one arm", func(t *testing.T) {
		var part Part
		if err := json.Unmarshal([]byte(`{"text":"first","data":{"last":true}}`), &part); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if part.Kind() != PartKindData {
			t.Fatalf("Kind = %v, want data", part.Kind())
		}
		raw, err := json.Marshal(part)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if string(raw) != `{"data":{"last":true}}` {
			t.Fatalf("wire = %s, want only last oneof arm", raw)
		}
	})

	t.Run("later null scalar does not clear active oneof", func(t *testing.T) {
		var part Part
		if err := json.Unmarshal([]byte(`{"data":null,"text":null}`), &part); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		value, ok := part.Data()
		if !ok || value != nil {
			t.Fatalf("Data = (%#v, %v), want active null", value, ok)
		}
	})

	t.Run("unknown field fails", func(t *testing.T) {
		var part Part
		err := json.Unmarshal([]byte(`{"text":"hello","surprise":true}`), &part)
		if err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("error = %v, want unknown-field refusal", err)
		}
	})
}

func TestProtoJSONStrictEnumTimestampAndUnknownFields(t *testing.T) {
	t.Parallel()
	for name, fixture := range map[string]string{
		"role":       `{"messageId":"m","role":"USER","parts":[{"text":"x"}]}`,
		"task state": `{"id":"t","status":{"state":"WORKING"}}`,
		"timestamp":  `{"id":"t","status":{"state":"TASK_STATE_WORKING","timestamp":"not-a-time"}}`,
		"unknown":    `{"id":"t","status":{"state":"TASK_STATE_WORKING"},"surprise":true}`,
	} {
		name, fixture := name, fixture
		t.Run(name, func(t *testing.T) {
			var target any
			if name == "role" {
				target = &Message{}
			} else {
				target = &Task{}
			}
			if err := json.Unmarshal([]byte(fixture), target); err == nil {
				t.Fatalf("Unmarshal(%s) succeeded, want strict refusal", fixture)
			}
		})
	}

	for _, timestamp := range []string{
		"2026-08-30T07:04:05+00:00",
		"2026-08-30T03:04:05-04:00",
		"2026-08-30T07:04:05z",
	} {
		timestamp := timestamp
		t.Run("refuse_"+timestamp, func(t *testing.T) {
			var task Task
			fixture := `{"id":"t","status":{"state":"TASK_STATE_WORKING","timestamp":"` + timestamp + `"}}`
			if err := json.Unmarshal([]byte(fixture), &task); err == nil {
				t.Fatalf("Unmarshal timestamp %q succeeded, want literal-Z refusal", timestamp)
			}
			if _, err := json.Marshal(Timestamp(timestamp)); err == nil {
				t.Fatalf("Marshal timestamp %q succeeded, want literal-Z refusal", timestamp)
			}
		})
	}

	for input, want := range map[string]Timestamp{
		"2026-08-30T07:04:05Z":           "2026-08-30T07:04:05Z",
		"2026-08-30T07:04:05.12Z":        "2026-08-30T07:04:05.120Z",
		"2026-08-30T07:04:05.123Z":       "2026-08-30T07:04:05.123Z",
		"2026-08-30T07:04:05.123456Z":    "2026-08-30T07:04:05.123456Z",
		"2026-08-30T07:04:05.123456789Z": "2026-08-30T07:04:05.123456789Z",
	} {
		var task Task
		fixture := `{"id":"t","status":{"state":"TASK_STATE_WORKING","timestamp":"` + input + `"}}`
		if err := json.Unmarshal([]byte(fixture), &task); err != nil {
			t.Fatalf("valid timestamp %q: %v", input, err)
		}
		if task.Status.Timestamp != want {
			t.Fatalf("timestamp %q = %q, want %q", input, task.Status.Timestamp, want)
		}
		raw, err := json.Marshal(Timestamp(input))
		if err != nil {
			t.Fatalf("Marshal valid timestamp %q: %v", input, err)
		}
		if string(raw) != `"`+string(want)+`"` {
			t.Fatalf("Marshal timestamp %q = %s, want %q", input, raw, want)
		}
	}

	var numeric Message
	if err := json.Unmarshal([]byte(`{"messageId":"m","role":1,"parts":[{"text":"x"}]}`), &numeric); err != nil {
		t.Fatalf("numeric enum: %v", err)
	}
	if numeric.Role != RoleUser {
		t.Fatalf("numeric role = %q, want ROLE_USER", numeric.Role)
	}
	if _, err := json.Marshal(Message{MessageID: "m", Role: Role("UNKNOWN"), Parts: []Part{TextPart("x")}}); err == nil {
		t.Fatal("Marshal unknown Role succeeded, want strict refusal")
	}
}

func TestProtoJSONAcceptsSnakeCaseAndUsesOrderedLastWins(t *testing.T) {
	t.Parallel()
	var message Message
	fixture := `{
		"messageId":"camel-first",
		"message_id":"snake-last",
		"context_id":"context-1",
		"role":"ROLE_USER",
		"parts":[{"media_type":"text/plain","text":"hello"}],
		"reference_task_ids":["task-1"]
	}`
	if err := json.Unmarshal([]byte(fixture), &message); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if message.MessageID != "snake-last" || message.ContextID != "context-1" || len(message.ReferenceTaskIDs) != 1 {
		t.Fatalf("message = %+v, want normalized snake_case and last-wins id", message)
	}
	if message.Parts[0].MediaType != "text/plain" {
		t.Fatalf("mediaType = %q, want snake_case alias", message.Parts[0].MediaType)
	}
}
