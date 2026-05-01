//go:build integration

// Package claude integration tests run only when the `integration`
// build tag is set AND a real `claude` binary is present on PATH.
//
// These tests exercise a no-op session against the real CLI to verify
// the provider's CLI args, JSONL parser, and lifecycle wiring do not
// drift when the upstream `claude` binary is upgraded. They issue
// real model requests and DO incur cost — keep prompts trivial.
//
// Run via:
//
//	go test -race -tags=integration ./provider/claude/...
//
// or, for a single test:
//
//	go test -race -tags=integration -run TestIntegration_Smoke ./provider/claude/...
package claude

import (
	"os/exec"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

func skipIfNoCLAUDE(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skipf("claude binary not on PATH; skipping integration test")
	}
}

func TestIntegration_Smoke(t *testing.T) {
	skipIfNoCLAUDE(t)

	p, err := New(Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	maxTurns := 1
	spec := agent.Spec{
		Prompt:     "Reply with the literal word OK and nothing else.",
		MaxTurns:   &maxTurns,
		Autonomous: true,
	}

	h, err := p.Spawn(t.Context(), spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(t.Context()) })

	deadline := time.NewTimer(120 * time.Second)
	defer deadline.Stop()

	gotInit := false
	gotResult := false
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				if !gotInit {
					t.Error("events channel closed without InitEvent")
				}
				if !gotResult {
					t.Error("events channel closed without ResultEvent")
				}
				return
			}
			switch e := ev.(type) {
			case agent.InitEvent:
				gotInit = true
				if e.SessionID == "" {
					t.Error("InitEvent.SessionID is empty")
				}
			case agent.ResultEvent:
				gotResult = true
				if e.Cost == nil {
					t.Error("ResultEvent.Cost should be populated")
				}
			case agent.ErrorEvent:
				t.Logf("ErrorEvent during smoke: %s (%s)", e.Message, e.Code)
			}
		case <-deadline.C:
			t.Fatal("integration smoke timed out after 120s")
		}
	}
}
