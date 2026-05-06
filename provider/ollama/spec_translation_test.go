package ollama

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

func TestBuildChatRequest_basic(t *testing.T) {
	t.Parallel()
	body, err := buildChatRequest("llama3.3", agent.Spec{
		Prompt: "hello",
	})
	if err != nil {
		t.Fatalf("buildChatRequest returned error: %v", err)
	}
	var got chatRequest
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("encoded body is not valid JSON: %v\nbody=%s", err, body)
	}
	if got.Model != "llama3.3" {
		t.Errorf("model: got %q want llama3.3", got.Model)
	}
	if !got.Stream {
		t.Errorf("stream: got false want true")
	}
	if len(got.Messages) != 1 {
		t.Fatalf("messages len: got %d want 1; messages=%+v", len(got.Messages), got.Messages)
	}
	if got.Messages[0].Role != "user" || got.Messages[0].Content != "hello" {
		t.Errorf("user message: got %+v want {user, hello}", got.Messages[0])
	}
}

func TestBuildChatRequest_withSystemPrompt(t *testing.T) {
	t.Parallel()
	body, err := buildChatRequest("llama3.3", agent.Spec{
		Prompt:             "do work",
		SystemPromptAppend: "you are a careful agent",
	})
	if err != nil {
		t.Fatalf("buildChatRequest: %v", err)
	}
	var got chatRequest
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("messages len: got %d want 2", len(got.Messages))
	}
	if got.Messages[0].Role != "system" || got.Messages[0].Content != "you are a careful agent" {
		t.Errorf("system message: got %+v", got.Messages[0])
	}
	if got.Messages[1].Role != "user" || got.Messages[1].Content != "do work" {
		t.Errorf("user message: got %+v", got.Messages[1])
	}
}

func TestBuildChatRequest_emptyPromptRejected(t *testing.T) {
	t.Parallel()
	_, err := buildChatRequest("llama3.3", agent.Spec{Prompt: ""})
	if err == nil {
		t.Fatal("expected error for empty prompt, got nil")
	}
	if !strings.Contains(err.Error(), "Prompt required") {
		t.Errorf("error message should mention Prompt; got %q", err.Error())
	}
}

func TestBuildChatRequest_htmlNotEscaped(t *testing.T) {
	t.Parallel()
	body, err := buildChatRequest("llama3.3", agent.Spec{
		Prompt: "fix this: <div>&amp;</div>",
	})
	if err != nil {
		t.Fatalf("buildChatRequest: %v", err)
	}
	if !strings.Contains(string(body), "<div>") {
		t.Errorf("HTML escaping should be disabled; body=%s", body)
	}
}

func TestSpawn_rejectsEmptyModel(t *testing.T) {
	t.Parallel()
	// Negative ProbeTimeout disables the probe so we can construct
	// without a live server.
	p, err := New(Options{ProbeTimeout: -1, Endpoint: "http://example.invalid"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = p.Spawn(t.Context(), agent.Spec{Prompt: "hi"})
	if err == nil {
		t.Fatal("expected error for empty model, got nil")
	}
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Errorf("error should wrap ErrSpawnFailed; got %v", err)
	}
	if !strings.Contains(err.Error(), "Spec.Model required") {
		t.Errorf("error message should mention Spec.Model; got %q", err.Error())
	}
}

func TestSpawn_rejectsEmptyPromptViaBuilder(t *testing.T) {
	t.Parallel()
	p, err := New(Options{ProbeTimeout: -1, Endpoint: "http://example.invalid"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Empty prompt is caught by buildChatRequest, surfaced as ErrSpawnFailed.
	_, err = p.Spawn(t.Context(), agent.Spec{Model: "llama3.3", Prompt: ""})
	if err == nil {
		t.Fatal("expected error for empty prompt, got nil")
	}
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Errorf("error should wrap ErrSpawnFailed; got %v", err)
	}
}
