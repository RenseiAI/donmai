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
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
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

func TestIntegration_RealCodexAppServer_PreparedHarnessReadyPath(t *testing.T) {
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex binary not on PATH")
	}
	cwd := t.TempDir()
	p, err := New(Options{Cwd: cwd, HandshakeTimeout: 30 * time.Second})
	if err != nil {
		t.Skipf("codex unavailable: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	source := agent.Spec{
		PromptMode: agent.PromptModeAutonomous, Autonomous: true,
		SandboxEnabled: true, SandboxLevel: agent.SandboxReadOnly,
		PromptPlan: &agent.PromptPlan{
			ContractVersion:  agent.PromptContractVersion,
			BaseInstructions: agent.BaseInstructionPlan{Strategy: agent.BaseInstructionsPreserve},
			UserPrompt:       agent.PromptContent{ID: "prepared-ready-smoke", Text: "Reply only with ready.", Required: true},
		},
	}
	const operationalDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	var materializations []agent.HarnessMaterialization
	for _, channel := range []string{"worktree", "environment", "credentials", "config", "endpoint_delivery", "services", "child_process", "runtime", "cleanup"} {
		materializations = append(materializations, agent.HarnessMaterialization{Channel: channel, SourceDigest: operationalDigest, Required: true})
	}
	plan, err := agent.CompilePreparedHarness(source, p.Manifest(), operationalDigest, nil, materializations)
	if err != nil {
		t.Fatalf("host CompilePreparedHarness: %v", err)
	}
	if err := agent.ValidatePreparedHarness(plan, operationalDigest); err != nil {
		t.Fatalf("host prepared plan: %v", err)
	}

	materialized := source
	materialized.Cwd = cwd
	materialized.PreparedHarness = plan
	materialized.OnPromptAdapted = func(agent.PromptDeliveryReceipt) error {
		return errors.New("provider attempted to mint a second prompt authority")
	}
	materialized.OnToolLifecycleAdapted = func(agent.ToolLifecycleReceipt) error {
		return errors.New("provider attempted to mint a second tool authority")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, materialized)
	if err != nil {
		t.Fatalf("Spawn prepared ready path: %v", err)
	}
	if h == nil {
		t.Fatal("real Codex accepted the prepared ready path without returning a handle")
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Fatalf("stop prepared ready handle: %v", err)
	}
}

func TestIntegration_RealCodexPTY_PreparedHumanReadyPath(t *testing.T) {
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex binary not on PATH")
	}
	cwd := t.TempDir()
	p, err := New(Options{Cwd: cwd, HandshakeTimeout: 30 * time.Second})
	if err != nil {
		t.Skipf("codex unavailable: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	source := agent.Spec{
		PromptMode:  agent.PromptModeHumanControlled,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
		PromptPlan: &agent.PromptPlan{
			ContractVersion:  agent.PromptContractVersion,
			BaseInstructions: agent.BaseInstructionPlan{Strategy: agent.BaseInstructionsPreserve},
			UserPrompt:       agent.PromptContent{ID: "prepared-human-smoke", Text: "Reply only with ready.", Required: true},
		},
	}
	const operationalDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	var materializations []agent.HarnessMaterialization
	for _, channel := range []string{"worktree", "environment", "credentials", "config", "endpoint_delivery", "services", "child_process", "runtime", "cleanup"} {
		materializations = append(materializations, agent.HarnessMaterialization{Channel: channel, SourceDigest: operationalDigest, Required: true})
	}
	prepared, err := agent.CompilePreparedHarness(source, p.Manifest(), operationalDigest, nil, materializations)
	if err != nil {
		t.Fatalf("host CompilePreparedHarness: %v", err)
	}
	materialized := source
	materialized.Cwd = cwd
	materialized.PreparedHarness = prepared
	materialized.OnPromptAdapted = func(agent.PromptDeliveryReceipt) error { return errors.New("second prompt authority") }
	materialized.OnToolLifecycleAdapted = func(agent.ToolLifecycleReceipt) error { return errors.New("second tool authority") }
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, materialized)
	if err != nil {
		t.Fatalf("Spawn prepared human PTY path: %v", err)
	}
	if _, ok := h.(agent.InteractiveCapable); !ok {
		_ = h.Stop(context.Background())
		t.Fatal("prepared human path did not return an interactive handle")
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Fatalf("stop prepared human PTY handle: %v", err)
	}
}

func intPtr(i int) *int { return &i }

func TestIntegration_RealCodexAppServer_PromptProvenance(t *testing.T) {
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex binary not on PATH")
	}
	cwd := t.TempDir()
	p, err := New(Options{Cwd: cwd, HandshakeTimeout: 30 * time.Second})
	if err != nil {
		t.Skipf("codex unavailable: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	plan := agent.PromptPlan{
		ContractVersion:  agent.PromptContractVersion,
		HarnessProtocol:  &agent.PromptContent{ID: "system", Text: "Remember system marker REN2040DXS3. Report it when asked. Do not use tools.", Required: true},
		BaseInstructions: agent.BaseInstructionPlan{Strategy: agent.BaseInstructionsPreserve},
		InitialContext:   []agent.PromptContent{{ID: "context", Text: "Initial-context marker REN2040DXC5.", Required: true}},
		UserPrompt:       agent.PromptContent{ID: "user", Text: "User-task marker REN2040DXU7. Reply with all four opaque markers and do not use tools.", Required: true},
		UserAmendments: []agent.UserPromptAmendment{{
			ID: "amendment", Position: agent.UserPromptAppend,
			Content: agent.PromptContent{ID: "amendment-content", Text: "Trailing amendment marker REN2040DXA9.", Required: true},
		}},
	}
	var receipt agent.PromptDeliveryReceipt
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{
		PromptPlan: &plan, Cwd: cwd, Autonomous: true, SandboxEnabled: true, SandboxLevel: agent.SandboxReadOnly,
		Model:           "gpt-5.6-terra",
		OnPromptAdapted: func(got agent.PromptDeliveryReceipt) error { receipt = got; return nil },
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()

	var transcript strings.Builder
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				goto done
			}
			switch event := ev.(type) {
			case agent.AssistantTextEvent:
				transcript.WriteString(event.Text)
			case agent.ResultEvent:
				transcript.WriteString(event.Message)
				goto done
			case agent.ErrorEvent:
				t.Fatalf("provider error: %s", event.Message)
			}
		case <-ctx.Done():
			t.Fatalf("prompt provenance smoke timed out: %v", ctx.Err())
		}
	}

done:
	if receipt.Decision != "ready" || receipt.ProfileID != "codex/headless/app-server-v2" {
		t.Fatalf("receipt = %+v", receipt)
	}
	for _, marker := range []string{"REN2040DXS3", "REN2040DXC5", "REN2040DXU7", "REN2040DXA9"} {
		if !strings.Contains(transcript.String(), marker) {
			t.Fatalf("transcript missing %s: %q", marker, transcript.String())
		}
	}
	t.Logf("receipt=%+v transcript=%q", receipt, transcript.String())
}
