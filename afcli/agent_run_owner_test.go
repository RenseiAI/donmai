package afcli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/attachwire"
	providerstub "github.com/RenseiAI/donmai/provider/harness/stub"
	"github.com/RenseiAI/donmai/runner"
	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
	"github.com/spf13/cobra"
)

const ownerProcessFixtureEnv = "DONMAI_TEST_AGENT_OWNER"

type ownerDeadlineProvider struct{ agent.HarnessProvider }

func (p ownerDeadlineProvider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, fmt.Errorf("fixture requires a real runner stage deadline")
	}
	if err := json.NewEncoder(os.Stdout).Encode(map[string]int64{"fixtureDeadline": deadline.UnixNano()}); err != nil {
		return nil, err
	}
	return p.HarnessProvider.Spawn(ctx, spec)
}

// This re-executes the actual CLI RunE path with a real runner, native stub PTY,
// and shim. It disables the race runtime's incidental exit sleep so that delay
// cannot impersonate process-owner retention. No model or installed daemon runs.
func TestAgentRunOwnerSurvivesCompletedStageDeadline(t *testing.T) {
	if os.Getenv(ownerProcessFixtureEnv) == "1" {
		buildRegistryForAgentRun = func(*slog.Logger, agentRunCtorHints, string) *runner.Registry {
			p, err := providerstub.New(providerstub.WithStubAgentCommand("/bin/sh", "-c", `while IFS= read -r line; do if [ "$line" = finish-owner ]; then printf final-owner-screen; exit 0; fi; done`))
			if err != nil {
				t.Fatal(err)
			}
			registry := runner.NewRegistry()
			if err := registry.Register(ownerDeadlineProvider{p.(agent.HarnessProvider)}); err != nil {
				t.Fatal(err)
			}
			return registry
		}
		cmd := &cobra.Command{}
		cmd.SetOut(os.Stdout)
		opts := agentRunOptions(Config{}, "donmai")
		opts.sessionID = "owner-session"
		opts.daemonURL = os.Getenv("DONMAI_TEST_OWNER_URL")
		opts.worktree = filepath.Join(os.Getenv("HOME"), "worktrees")
		opts.jsonOut = true
		// The interactive command deliberately ignores the legacy StageBudget cap.
		// Supply the real embedding caller budget instead; Runner inherits it.
		budgetCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		if err := runAgentRun(budgetCtx, cmd, opts); err != nil {
			t.Fatal(err)
		}
		return
	}
	for _, termination := range []string{"detach", "signal", "early-signal"} {
		t.Run(termination, func(t *testing.T) { runAgentOwnerProcessCase(t, termination) })
	}
}

func runAgentOwnerProcessCase(t *testing.T, termination string) {
	t.Helper()

	repository := makeSpecDecoratorBareRepo(t)
	home := t.TempDir()
	registryDir, err := os.MkdirTemp("/tmp", "owner")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(registryDir) })
	terminalPost := make(chan struct{})
	releasePost := make(chan struct{})
	var releaseOnce sync.Once
	releaseTerminal := func() { releaseOnce.Do(func() { close(releasePost) }) }
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/api/daemon/sessions/owner-session" {
			// Fixed wire fixture, not serialization of a credential-bearing live detail.
			detail := map[string]any{
				"sessionId": "owner-session", "mode": "interactive", "repository": repository,
				"ref": "main", "branch": "fixture-owner", "workType": "development",
				"title": "owned final screen", "body": "wait for terminal input",
				"platformUrl": "http://" + r.Host, "workerId": "fixture-worker", "authToken": "fixture-token",
				"resolvedProfile": map[string]string{"provider": "stub"},
				"stageBudget":     map[string]int{"maxDurationSeconds": 6},
			}
			if err := json.NewEncoder(w).Encode(detail); err != nil {
				t.Error(err)
			}
			return
		}
		if termination == "early-signal" && r.Method == http.MethodPost && r.URL.Path == "/api/sessions/owner-session/completion" {
			close(terminalPost)
			<-releasePost
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(backend.Close)
	t.Cleanup(releaseTerminal)
	launch := sessionshim.Launch{Identity: sessionshim.Identity{OrgID: "org-owner", SessionID: "owner-session"}, RegistryDir: registryDir, Orphan: sessionshim.DefaultOrphanPolicy(), ProcessEpoch: 1}
	//nolint:gosec // this test executable, exact helper test and owned directories only.
	cmd := exec.Command(os.Args[0], "-test.run=^TestAgentRunOwnerSurvivesCompletedStageDeadline$")
	cmd.Dir = home
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "TMPDIR=" + home, "GOMAXPROCS=2", "GORACE=atexit_sleep_ms=0", ownerProcessFixtureEnv + "=1", "DONMAI_TEST_OWNER_URL=" + backend.URL}
	for k, v := range launch.Env() {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	stdoutDrained := make(chan struct{})
	go func() { <-stdoutDrained; done <- cmd.Wait() }()
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	messages := make(chan map[string]any, 4)
	go func() {
		defer close(stdoutDrained)
		defer close(messages)
		dec := json.NewDecoder(stdout)
		for {
			var m map[string]any
			if err := dec.Decode(&m); err != nil {
				_, _ = io.Copy(io.Discard, stdout)
				return
			}
			messages <- m
		}
	}()
	var deadline time.Time
	select {
	case m := <-messages:
		t.Logf("first worker message: %+v", m)
		if value, ok := m["fixtureDeadline"].(float64); ok {
			deadline = time.Unix(0, int64(value))
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runner did not start")
	}
	if deadline.IsZero() {
		t.Fatal("runner did not supply its actual deadline")
	}
	reg, err := sessionshim.NewRegistry(registryDir)
	if err != nil {
		t.Fatal(err)
	}
	// Spawn announces its deadline before it publishes the shim. The registry
	// poll is bounded discovery, not a delay used to mask terminal ordering.
	var adopted sessionshim.AdoptionResult
	discovery := time.NewTicker(10 * time.Millisecond)
	defer discovery.Stop()
	discoveryBound := time.NewTimer(5 * time.Second)
	defer discoveryBound.Stop()
	for len(adopted.Adopted) == 0 {
		select {
		case <-discovery.C:
			adopted, err = sessionshim.Adopt(context.Background(), sessionshim.AdoptOptions{Registry: reg, ControllerID: "owner-test-controller", ProtocolMin: shimwire.V1, ProtocolMax: shimwire.V2})
			if err != nil {
				t.Fatal(err)
			}
		case <-discoveryBound.C:
			t.Fatal("shim was not published before the stage deadline")
		}
	}
	t.Cleanup(adopted.Close)
	c := adopted.Adopted[0]
	finishAt := time.NewTimer(time.Until(deadline.Add(-2 * time.Second)))
	defer finishAt.Stop()
	select {
	case <-finishAt.C:
	case err := <-done:
		t.Fatalf("owner exited before harness completion: %v", err)
	}
	if err := c.WriteInput([]byte("finish-owner\r")); err != nil {
		t.Fatal(err)
	}
	if termination == "early-signal" {
		select {
		case <-terminalPost:
		case <-time.After(5 * time.Second):
			t.Fatal("terminal publication did not reach its barrier")
		}
		// Expire the business deadline while its already-completed terminal
		// publication is held, then interrupt before owner draining begins.
		expires := time.NewTimer(time.Until(deadline))
		defer expires.Stop()
		<-expires.C
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		releaseTerminal()
	}
	select {
	case m := <-messages:
		if m["status"] != "completed" {
			t.Fatalf("business outcome before owner drain=%+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("business result delayed by final-screen service")
	}
	if termination != "detach" {
		if termination == "signal" {
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("post-result shutdown changed successful outcome: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("explicit command shutdown did not close retained owners")
		}
		return
	}
	// Cross the real stage deadline AFTER successful business completion; the
	// retained owner must still answer the seq-zero final snapshot.
	expires := time.NewTimer(time.Until(deadline))
	defer expires.Stop()
	select {
	case <-expires.C:
	case err := <-done:
		t.Fatalf("owner exited during final-screen window: %v", err)
	}
	final, err := c.EmitSnapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot after stage expiry/runner cleanup: %v", err)
	}
	frame, err := attachwire.DecodeFrame(final.Bytes)
	if err != nil || final.InStream || frame.Type != attachwire.TypeSnapshot || frame.Seq != attachwire.PostExitSnapshotSeq {
		t.Fatalf("post-Exit final=%+v frame=%+v err=%v", final, frame, err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("successful business outcome became process failure: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("owner did not exit after controller detach")
	}
}
