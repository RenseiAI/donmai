package afcli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/daemon"
	providerclaude "github.com/RenseiAI/donmai/provider/harness/claude"
	providercodex "github.com/RenseiAI/donmai/provider/harness/codex"
	providermuse "github.com/RenseiAI/donmai/provider/harness/muse"
	providerstub "github.com/RenseiAI/donmai/provider/harness/stub"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runner"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

// gateRoleFixture builds an inert public role of exactly size bytes. It opens
// with a unique non-whitespace marker, closes with a multibyte sentinel, and
// carries interior tabs, blank lines and spaces. The oracle is the returned
// string itself; callers assert byte length before serialization.
func gateRoleFixture(t *testing.T, size int) string {
	t.Helper()
	head := fmt.Sprintf("GATE-ROLE-%d:", size)
	tail := "🌱"
	prefix := "caf\u00e9 \t blank\n\n"
	chunk := "lorem ipsum\tdolor sit amet  \n\n"
	middleLen := size - len(head) - len(tail)
	if middleLen < len(prefix) {
		t.Fatalf("fixture size %d too small", size)
	}
	var middle strings.Builder
	middle.WriteString(prefix)
	for middle.Len() < middleLen {
		middle.WriteString(chunk)
	}
	role := head + middle.String()[:middleLen] + tail
	if len(role) != size {
		t.Fatalf("fixture bytes = %d, want %d", len(role), size)
	}
	if !utf8.ValidString(role) {
		t.Fatal("fixture is not valid UTF-8")
	}
	if !strings.HasPrefix(role, head) || !strings.HasSuffix(role, tail) {
		t.Fatal("fixture markers misplaced")
	}
	if !strings.Contains(role, "\t") || !strings.Contains(role, "\n\n") {
		t.Fatal("fixture must preserve interior tabs and blank lines")
	}
	return role
}

// gateFirstMismatch returns the first byte offset where a and b differ.
func gateFirstMismatch(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// gateDecodeRole drives raw poll JSON through PollItemToSessionDetail, a real
// detail HTTP fetch and detailToQueuedWork. legacy selects the sibling-only
// door (no operational payload); otherwise the receipted payload door runs
// with a distinguishable card mirror.
func gateDecodeRole(t *testing.T, fixture string, legacy bool) runner.QueuedWork {
	t.Helper()
	sessionID := fmt.Sprintf("gate-%d-legacy-%v", len(fixture), legacy)
	item := daemon.PollWorkItem{
		SessionID:            sessionID,
		ProjectID:            "wire-project",
		Repository:           "https://example.test/repo.git",
		Body:                 "gate budget probe",
		SystemPromptOverride: fixture,
		AgentCardID:          "gate-card",
		AgentCardName:        "gate-card-name",
		ResolvedProfile:      &daemon.SessionResolvedProfile{Provider: "stub"},
	}
	detail := daemon.PollItemToSessionDetail(item, nil, "https://platform.example", "token", "worker-1")
	if legacy {
		detail.OperationalPayload = nil
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(detail) //nolint:gosec // G117: test fixture
	}))
	t.Cleanup(srv.Close)
	fetched, err := fetchSessionDetail(context.Background(), &http.Client{Timeout: 2 * time.Second}, srv.URL, sessionID, "")
	if err != nil {
		t.Fatalf("fetchSessionDetail: %v", err)
	}
	qw, err := detailToQueuedWork(fetched)
	if err != nil {
		t.Fatalf("detailToQueuedWork: %v", err)
	}
	if qw.SystemPromptOverride != fixture {
		t.Fatalf("decoded role bytes = %d, want %d (first mismatch %d)", len(qw.SystemPromptOverride), len(fixture), gateFirstMismatch(qw.SystemPromptOverride, fixture))
	}
	if qw.AgentCardID != "gate-card" {
		t.Fatalf("card mirror = %q, want distinguishable gate-card", qw.AgentCardID)
	}
	return qw
}

// gateCaptureProvider records the Runner-composed Spec and completes at once.
type gateCaptureProvider struct {
	inner agent.Provider
	spec  agent.Spec
}

func (p *gateCaptureProvider) Name() agent.ProviderName         { return p.inner.Name() }
func (p *gateCaptureProvider) Capabilities() agent.Capabilities { return p.inner.Capabilities() }
func (p *gateCaptureProvider) Manifest() agent.HarnessManifest {
	if hp, ok := p.inner.(agent.HarnessProvider); ok {
		return hp.Manifest()
	}
	return agent.HarnessManifest{}
}

func (p *gateCaptureProvider) Spawn(_ context.Context, spec agent.Spec) (agent.Handle, error) {
	p.spec = spec
	ch := make(chan agent.Event, 2)
	ch <- agent.InitEvent{SessionID: "gate"}
	ch <- agent.ResultEvent{Success: true, Message: "gate-ok"}
	close(ch)
	return &gateCaptureHandle{ch: ch}, nil
}

func (p *gateCaptureProvider) Resume(context.Context, string, agent.Spec) (agent.Handle, error) {
	return nil, agent.ErrUnsupported
}
func (p *gateCaptureProvider) Shutdown(context.Context) error { return nil }

type gateCaptureHandle struct{ ch chan agent.Event }

func (h *gateCaptureHandle) SessionID() string                    { return "gate" }
func (h *gateCaptureHandle) Events() <-chan agent.Event           { return h.ch }
func (h *gateCaptureHandle) Inject(context.Context, string) error { return nil }
func (h *gateCaptureHandle) Stop(context.Context) error           { return nil }

// gateRunToSpec passes decoded work through Runner.Run and returns the exact
// Spec the runner composed for Spawn.
func gateRunToSpec(t *testing.T, qw runner.QueuedWork) agent.Spec {
	t.Helper()
	repo := makeSpecDecoratorBareRepo(t)
	qw.Repository = repo
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"refreshed":true,"ok":true}`))
	}))
	t.Cleanup(platform.Close)
	manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: platform.URL, WorkerID: "worker-1", AuthToken: "token",
		HTTPClient: platform.Client(), BaseDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	inner, err := providerstub.New()
	if err != nil {
		t.Fatal(err)
	}
	capture := &gateCaptureProvider{inner: inner}
	reg := runner.NewRegistry()
	if err := reg.Register(capture); err != nil {
		t.Fatal(err)
	}
	run, err := runner.New(runner.Options{
		Registry: reg, WorktreeManager: manager, Poster: poster,
		HTTPClient: platform.Client(), SkipBackstop: true, SkipSteering: true,
		SkipPostSession: true, PreserveWorktreeAlways: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := run.Run(ctx, qw)
	if err != nil || res.Status != "completed" {
		t.Fatalf("Runner.Run = %+v, %v; want completed", res, err)
	}
	return capture.spec
}

// gateAssertContainsOnce pins the oracle arrives byte-intact exactly once in
// the delivered field. Diagnostics stay at lengths plus first mismatch.
func gateAssertContainsOnce(t *testing.T, delivered, oracle, transport string) {
	t.Helper()
	if n := strings.Count(delivered, oracle); n != 1 {
		t.Fatalf("%s: oracle occurrences = %d, want 1 (delivered %d, oracle %d)", transport, n, len(delivered), len(oracle))
	}
}

// gateClaudeChild delivers spec through a real claude Spawn to a local fake
// CLI that records the exact --append-system-prompt argument bytes.
func gateClaudeChild(t *testing.T, spec agent.Spec) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "role")
	script := "#!/bin/sh\n" +
		"prev=\"\"\n" +
		"for a in \"$@\"; do\n" +
		"  if [ \"$prev\" = \"--append-system-prompt\" ]; then printf '%s' \"$a\" > " + out + "\n  fi\n" +
		"  prev=\"$a\"\n" +
		"done\n" +
		`printf '{"type":"system","subtype":"init","session_id":"sess-gate"}\n'` + "\n" +
		`printf '{"type":"result","subtype":"success","is_error":false,"num_turns":1}\n'` + "\n"
	cli := filepath.Join(dir, "fake-claude.sh")
	if err := os.WriteFile(cli, []byte(script), 0o600); err != nil { //nolint:gosec // G306: fixture rewritten executable below
		t.Fatal(err)
	}
	if err := os.Chmod(cli, 0o700); err != nil { //nolint:gosec // G306: test fake CLI must execute
		t.Fatal(err)
	}
	p, err := providerclaude.New(providerclaude.Options{Binary: cli, LookPath: func(string) (string, error) { return cli, nil }})
	if err != nil {
		t.Fatal(err)
	}
	spec.Cwd = dir
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("claude Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	deadline := time.Now().Add(30 * time.Second)
	for ev := range h.Events() {
		if _, ok := ev.(agent.ResultEvent); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out draining claude events")
		}
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read recorded role: %v", err)
	}
	return string(raw)
}

func TestGateRoleBudget_Transports(t *testing.T) {
	t.Parallel()
	for _, size := range []int{32768, 38912, 80000} {
		size := size
		t.Run(fmt.Sprintf("role-%d", size), func(t *testing.T) {
			t.Parallel()
			oracle := gateRoleFixture(t, size)
			for _, legacy := range []bool{true, false} {
				qw := gateDecodeRole(t, oracle, legacy)
				spec := gateRunToSpec(t, qw)
				if !strings.Contains(spec.SystemPromptAppend, oracle) {
					t.Fatalf("composed Spec bytes = %d, oracle %d (mismatch %d)", len(spec.SystemPromptAppend), len(oracle), gateFirstMismatch(spec.SystemPromptAppend, oracle))
				}
				gateAssertContainsOnce(t, spec.SystemPromptAppend, oracle, "runner-composition")
				claudeRole := gateClaudeChild(t, spec)
				gateAssertContainsOnce(t, claudeRole, oracle, "claude-argv")
				codexPlan := providercodex.NewSpawnPlan(spec)
				dev, _ := codexPlan.ThreadStart["developerInstructions"].(string)
				framed, err := json.Marshal(codexPlan.ThreadStart)
				if err != nil {
					t.Fatal(err)
				}
				var peer map[string]any
				if err := json.Unmarshal(framed, &peer); err != nil {
					t.Fatal(err)
				}
				if peerDev, _ := peer["developerInstructions"].(string); peerDev != dev {
					t.Fatal("codex JSON framing altered developerInstructions")
				}
				buf := bytes.NewBuffer(framed)
				dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
				var round map[string]any
				if err := dec.Decode(&round); err != nil {
					t.Fatal(err)
				}
				gateAssertContainsOnce(t, dev, oracle, "codex-thread-start")
				musePlan := providermuse.NewSpawnPlan(spec, t.TempDir())
				var texts []string
				for _, part := range musePlan.PromptInput {
					if part.Text != nil {
						texts = append(texts, *part.Text)
					}
				}
				joined := strings.Join(texts, "\n")
				encoded, err := json.Marshal(musePlan.PromptInput)
				if err != nil {
					t.Fatal(err)
				}
				var decoded []map[string]any
				if err := json.Unmarshal(encoded, &decoded); err != nil {
					t.Fatal(err)
				}
				var peerTexts []string
				for _, part := range decoded {
					if s, _ := part["text"].(string); s != "" {
						peerTexts = append(peerTexts, s)
					}
				}
				if strings.Join(peerTexts, "\n") != joined {
					t.Fatal("muse JSON framing altered turn/start text")
				}
				gateAssertContainsOnce(t, joined, oracle, "muse-turn-start")
			}
		})
	}
}

// TestGateRoleBudget_DetailEnvelopeLimit pins the 1 MiB detail read ceiling:
// the exact limit returns the complete role while one byte more yields a
// decode error and no detail to run.
func TestGateRoleBudget_DetailEnvelopeLimit(t *testing.T) {
	t.Parallel()
	role := gateRoleFixture(t, 32768)
	serve := func(t *testing.T, total int) (*httptest.Server, string) {
		t.Helper()
		detail := &daemon.SessionDetail{SessionID: "gate-envelope", SystemPromptOverride: role}
		raw, err := json.Marshal(detail) //nolint:gosec // G117: test fixture
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) > total {
			t.Fatalf("base envelope %d exceeds target %d", len(raw), total)
		}
		padLen := total - len(raw)
		padded := make([]byte, 0, total)
		padded = append(padded, raw[:len(raw)-1]...)
		padded = append(padded, bytes.Repeat([]byte(" "), padLen)...)
		padded = append(padded, '}')
		if len(padded) != total {
			t.Fatalf("envelope bytes = %d, want %d", len(padded), total)
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(padded)
		}))
		t.Cleanup(srv.Close)
		return srv, role
	}
	t.Run("exact-limit", func(t *testing.T) {
		t.Parallel()
		srv, want := serve(t, 1<<20)
		got, err := fetchSessionDetail(context.Background(), &http.Client{Timeout: 2 * time.Second}, srv.URL, "gate-envelope", "")
		if err != nil {
			t.Fatalf("fetch at limit: %v", err)
		}
		qw, err := detailToQueuedWork(got)
		if err != nil {
			t.Fatalf("detailToQueuedWork at limit: %v", err)
		}
		if qw.SystemPromptOverride != want {
			t.Fatalf("role at limit bytes = %d, want %d", len(qw.SystemPromptOverride), len(want))
		}
	})
	t.Run("one-over", func(t *testing.T) {
		t.Parallel()
		srv, _ := serve(t, (1<<20)+1)
		if _, err := fetchSessionDetail(context.Background(), &http.Client{Timeout: 2 * time.Second}, srv.URL, "gate-envelope", ""); err == nil {
			t.Fatal("fetch one byte over limit succeeded, want decode error")
		}
	})
}
