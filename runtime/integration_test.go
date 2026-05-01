//go:build runtime_integration

package runtime_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/agent"
	"github.com/RenseiAI/agentfactory-tui/runtime/env"
	"github.com/RenseiAI/agentfactory-tui/runtime/heartbeat"
	"github.com/RenseiAI/agentfactory-tui/runtime/mcp"
	"github.com/RenseiAI/agentfactory-tui/runtime/state"
	"github.com/RenseiAI/agentfactory-tui/runtime/worktree"
)

// TestEndToEndRuntimeFlow exercises Provision → Compose → Build →
// Write state → Pulse heartbeat → Teardown.
func TestEndToEndRuntimeFlow(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	// 1. Bare repo as the source-of-truth fixture.
	work := t.TempDir()
	bare := t.TempDir()
	runIn := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
		}
	}
	runIn(work, "init", "-b", "main")
	runIn(work, "config", "user.email", "test@example.com")
	runIn(work, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(work, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	runIn(work, "add", ".")
	runIn(work, "commit", "-m", "init")
	cmd := exec.Command("git", "clone", "--bare", work, bare)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone --bare: %v\n%s", err, out)
	}

	// 2. Worktree manager.
	parent := t.TempDir()
	wt, err := worktree.NewManager(worktree.Options{ParentDir: parent})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	wpath, err := wt.Provision(ctx, worktree.ProvisionSpec{
		SessionID: "sess-e2e",
		RepoURL:   bare,
		Branch:    "main",
		Strategy:  worktree.StrategyClone,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// 3. Compose env.
	composer := env.NewComposer()
	envOut := composer.Compose(map[string]string{
		"PATH":              "/usr/bin",
		"ANTHROPIC_API_KEY": "leak", // must be filtered
	}, agent.Spec{Env: map[string]string{
		"AGENTFACTORY_SESSION_ID": "sess-e2e",
	}})
	hasAPI := false
	hasSession := false
	for _, kv := range envOut {
		if kv == "ANTHROPIC_API_KEY=leak" {
			hasAPI = true
		}
		if kv == "AGENTFACTORY_SESSION_ID=sess-e2e" {
			hasSession = true
		}
	}
	if hasAPI {
		t.Fatal("blocklist did not filter ANTHROPIC_API_KEY")
	}
	if !hasSession {
		t.Fatal("Spec env did not propagate")
	}

	// 4. Build MCP config tmpfile.
	mb := mcp.NewBuilder()
	mcpPath, mcpCleanup, err := mb.Build([]agent.MCPServerConfig{
		{Name: "af-linear", Command: "/usr/local/bin/af", Args: []string{"linear-mcp"}},
	})
	if err != nil {
		t.Fatalf("mcp.Build: %v", err)
	}
	t.Cleanup(mcpCleanup)
	if _, err := os.Stat(mcpPath); err != nil {
		t.Fatalf("mcp tmpfile missing: %v", err)
	}

	// 5. Write state.
	store := state.NewStore()
	if err := store.Write(wpath, &state.State{
		IssueIdentifier:   "REN-E2E",
		SessionID:         "sess-e2e",
		ProviderName:      agent.ProviderStub,
		ProviderSessionID: "stub-1",
		StartedAt:         time.Now().UnixMilli(),
		AttemptCount:      1,
	}); err != nil {
		t.Fatalf("state.Write: %v", err)
	}
	read, err := store.Read(wpath)
	if err != nil {
		t.Fatalf("state.Read: %v", err)
	}
	if read.SessionID != "sess-e2e" {
		t.Fatalf("state roundtrip lost SessionID: %+v", read)
	}

	// 6. Heartbeat against an httptest server.
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"refreshed": true})
	}))
	defer srv.Close()

	pulser, err := heartbeat.New(heartbeat.Config{
		SessionID:  "sess-e2e",
		WorkerID:   "w1",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		Interval:   24 * time.Hour, // we only want the first synchronous tick here
	})
	if err != nil {
		t.Fatalf("heartbeat.New: %v", err)
	}
	if err := pulser.Start(ctx); err != nil {
		t.Fatalf("pulser.Start: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("expected 1 heartbeat hit, got %d", hits.Load())
	}
	if err := pulser.Stop(); err != nil {
		t.Fatalf("pulser.Stop: %v", err)
	}

	// 7. Teardown.
	if err := wt.Teardown(ctx, "sess-e2e"); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if _, err := os.Stat(wpath); !os.IsNotExist(err) {
		t.Fatalf("expected worktree removed, stat err=%v", err)
	}
}
