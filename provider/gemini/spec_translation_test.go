package gemini

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

func TestBuildRequestBody_Minimal(t *testing.T) {
	t.Parallel()
	raw, err := buildRequestBody(agent.Spec{Prompt: "hello"})
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	var got requestBody
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Contents) != 1 {
		t.Fatalf("Contents: want 1, got %d", len(got.Contents))
	}
	if got.Contents[0].Role != "user" {
		t.Errorf("Contents[0].Role: want %q, got %q", "user", got.Contents[0].Role)
	}
	if got.Contents[0].Parts[0].Text != "hello" {
		t.Errorf("Contents[0].Parts[0].Text: want %q, got %q", "hello", got.Contents[0].Parts[0].Text)
	}
	if got.SystemInstruction != nil {
		t.Errorf("SystemInstruction: want nil for minimal spec, got %#v", got.SystemInstruction)
	}
}

func TestBuildRequestBody_EmptyPromptRejected(t *testing.T) {
	t.Parallel()
	for _, p := range []string{"", "   ", "\n\t"} {
		_, err := buildRequestBody(agent.Spec{Prompt: p})
		if err == nil {
			t.Errorf("prompt %q: want error, got nil", p)
		}
	}
}

func TestBuildRequestBody_SystemInstruction(t *testing.T) {
	t.Parallel()
	raw, err := buildRequestBody(agent.Spec{
		Prompt:             "do work",
		BaseInstructions:   "you are a helpful agent",
		SystemPromptAppend: "follow REN-1500",
	})
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	var got requestBody
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SystemInstruction == nil {
		t.Fatal("SystemInstruction: want non-nil")
	}
	text := got.SystemInstruction.Parts[0].Text
	if !strings.Contains(text, "you are a helpful agent") {
		t.Errorf("SystemInstruction missing BaseInstructions: %q", text)
	}
	if !strings.Contains(text, "REN-1500") {
		t.Errorf("SystemInstruction missing SystemPromptAppend: %q", text)
	}
}

func TestBuildRequestBody_MaxTurnsMapsToMaxOutputTokens(t *testing.T) {
	t.Parallel()
	turns := 3
	raw, err := buildRequestBody(agent.Spec{Prompt: "hello", MaxTurns: &turns})
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	var got requestBody
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.GenerationConfig == nil {
		t.Fatal("GenerationConfig: want non-nil when MaxTurns set")
	}
	if got.GenerationConfig.MaxOutputTokens != 6144 {
		t.Errorf("MaxOutputTokens: want %d (3*2048), got %d", 6144, got.GenerationConfig.MaxOutputTokens)
	}
}

func TestBuildSystemInstruction_OnlyAppend(t *testing.T) {
	t.Parallel()
	got := buildSystemInstruction(agent.Spec{SystemPromptAppend: "trailing"})
	if got != "trailing" {
		t.Errorf("buildSystemInstruction: want %q, got %q", "trailing", got)
	}
}
