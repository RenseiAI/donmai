//go:build codex_integration

// Package codex integration tests run against a real `codex
// app-server` subprocess. They are gated behind the build tag
// `codex_integration` so the default `go test ./...` run never tries
// to spawn codex (it requires network access + a configured OpenAI
// key).
//
// To run: `go test -tags codex_integration -timeout 120s ./provider/codex/`.
//
// Pre-requisites:
//   - `codex` on PATH (see https://developers.openai.com/codex/)
//   - OPENAI_API_KEY (or whatever auth codex requires) configured
//   - network access
//
// The test does the bare minimum lifecycle smoke: spawn a session
// against a `read-only` sandbox with a trivial prompt, verify we get
// at least one InitEvent, then Stop the session and Shutdown the
// Provider.

package codex

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

func TestIntegration_RealCodexAppServer_SmokeLifecycle(t *testing.T) {
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex binary not on PATH")
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	p, err := New(Options{Cwd: cwd, HandshakeTimeout: 30 * time.Second})
	if err != nil {
		if errors.Is(err, agent.ErrProviderUnavailable) {
			t.Skipf("codex unavailable: %v", err)
		}
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	h, err := p.Spawn(ctx, agent.Spec{
		Prompt:         "say hello",
		Cwd:            cwd,
		Autonomous:     true,
		SandboxEnabled: true,
		SandboxLevel:   agent.SandboxReadOnly,
		MaxTurns:       intPtr(1),
		Effort:         agent.EffortLow,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()

	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()

	var sawInit bool
	for !sawInit {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				if !sawInit {
					t.Fatalf("events channel closed before InitEvent")
				}
				return
			}
			if ev.Kind() == agent.EventInit {
				sawInit = true
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for InitEvent")
		}
	}
}

func intPtr(i int) *int { return &i }
