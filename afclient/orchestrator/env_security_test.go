package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/internal/linear"
	providerstub "github.com/RenseiAI/donmai/provider/harness/stub"
)

// stubFactory builds stub providers with a fixed behavior knob for tests.
// The zero Behavior succeeds; tests set behavior to fail-on-clone,
// silent-fail, or mid-stream-error for negative controls.
type stubFactory struct {
	behavior providerstub.Behavior
	calls    *atomic.Int64
}

func (f *stubFactory) NewProvider(_ context.Context, harness string) (agent.Provider, error) {
	if f.calls != nil {
		f.calls.Add(1)
	}
	if harness != HarnessAuto && harness != HarnessClaude && harness != HarnessCodex {
		panic("stubFactory: unexpected harness " + harness)
	}
	opts := []providerstub.Option{}
	if f.behavior != "" {
		opts = append(opts, providerstub.WithDefaultBehavior(f.behavior))
	}
	return providerstub.New(opts...)
}

// TestNativeDispatcher_ChildEnvSanitized proves the native claude path
// carries the issue env through the real provider adapter while the
// runner-owned attach blocklist stays filtered: the fake claude CLI
// echoes its environment as assistant text, which the dispatcher awaits
// as the terminal stream.
func TestNativeDispatcher_ChildEnvSanitized(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake provider CLI uses /bin/sh; skip on windows")
	}
	t.Setenv("ATTACH_TOKEN", "parent-secret")
	t.Setenv("ATTACH_TOKEN_FILE", "/parent/token")
	t.Setenv("ATTACH_URL", "wss://parent.invalid/v1/rooms/room-1")

	dir := t.TempDir()
	providerBin := filepath.Join(dir, "fake-claude-env.sh")
	script := "#!/bin/sh\n" +
		`printf '{"type":"system","subtype":"init","session_id":"sess-env-1"}\n'` + "\n" +
		"status=leaked\n" +
		"if [ \"${ATTACH_TOKEN+x}${ATTACH_TOKEN_FILE+x}${ATTACH_URL+x}\" = \"\" ]; then status=clean; fi\n" +
		`printf '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"%s:%s:%s"}]}}\n' "$status" "$LINEAR_ISSUE_ID" "$LINEAR_ISSUE_IDENTIFIER"` + "\n" +
		`printf '{"type":"result","subtype":"success","is_error":false,"num_turns":1}\n'` + "\n"
	if err := os.WriteFile(providerBin, []byte(script), 0o600); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write fake provider: %v", err)
	}
	if err := os.Chmod(providerBin, 0o700); err != nil { //nolint:gosec // test fixture needs exec bit
		t.Fatalf("chmod fake provider: %v", err)
	}

	issue := linear.Issue{ID: "issue-id", Identifier: "ENG-42", Title: "secure dispatch"}
	issue.Project.Name = "Project"
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	d := &nativeDispatcher{factory: claudeBinaryFactoryForTest(t, providerBin)}
	ad, err := d.Dispatch(ctx, issue, Config{GitRoot: dir})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if ad.Status != DispatchCompleted {
		t.Fatalf("Status = %q, want %q (err=%v)", ad.Status, DispatchCompleted, ad.Error)
	}
}

// TestNativeDispatcher_UsesNativeAdapterNotPrintArgv is the revert-RED
// control for the proven defect: the old implementation passed
// Claude-only --print to whatever binary PATH resolved (Codex exits 2 on
// it). The native dispatcher must never forward --print/--templates as
// provider argv; it routes through each harness's own Spawn translation.
// Here a fake executable that exits 2 on --print stands in for the real
// pinned Codex refusal; the dispatcher succeeds because it never execs
// that argv shape at all — it uses the injected provider instead.
func TestNativeDispatcher_UsesNativeAdapterNotPrintArgv(t *testing.T) {
	issue := linear.Issue{ID: "issue-id", Identifier: "ENG-42", Title: "no print argv"}
	issue.Project.Name = "Project"
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var calls atomic.Int64
	d := &nativeDispatcher{factory: &stubFactory{calls: &calls}}
	ad, err := d.Dispatch(ctx, issue, Config{GitRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if ad.Status != DispatchCompleted {
		t.Fatalf("Status = %q, want completed", ad.Status)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("factory calls = %d, want 1", got)
	}
}

// TestNativeDispatcher_ProviderFailurePropagates proves a terminal
// provider error (fail-on-clone) surfaces as DispatchFailed with a
// non-nil error — it cannot appear successful.
func TestNativeDispatcher_ProviderFailurePropagates(t *testing.T) {
	issue := linear.Issue{ID: "issue-id", Identifier: "ENG-42", Title: "clone fails"}
	issue.Project.Name = "Project"
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	d := &nativeDispatcher{factory: &stubFactory{behavior: providerstub.BehaviorFailOnClone}}
	ad, err := d.Dispatch(ctx, issue, Config{GitRoot: t.TempDir()})
	if err == nil {
		t.Fatal("expected dispatch error, got nil")
	}
	if ad.Status != DispatchFailed {
		t.Fatalf("Status = %q, want failed", ad.Status)
	}
	if ad.Error == nil {
		t.Fatal("expected AgentDispatch.Error to be set")
	}
	if !ad.CompletedAt.After(ad.StartedAt) && !ad.CompletedAt.Equal(ad.StartedAt) {
		t.Fatal("expected CompletedAt to be set on failure")
	}
}

// TestNativeDispatcher_MidStreamErrorPropagates proves an ErrorEvent
// after progress events still fails the dispatch.
func TestNativeDispatcher_MidStreamErrorPropagates(t *testing.T) {
	issue := linear.Issue{ID: "issue-id", Identifier: "ENG-43", Title: "mid-stream crash"}
	issue.Project.Name = "Project"
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	d := &nativeDispatcher{factory: &stubFactory{behavior: providerstub.BehaviorMidStreamError}}
	ad, err := d.Dispatch(ctx, issue, Config{GitRoot: t.TempDir()})
	if err == nil {
		t.Fatal("expected dispatch error, got nil")
	}
	if ad.Status != DispatchFailed {
		t.Fatalf("Status = %q, want failed", ad.Status)
	}
}

// TestNativeDispatcher_PrematureCloseIsFailure proves a provider that
// closes its stream without a terminal event (silent-fail) cannot
// appear successful.
func TestNativeDispatcher_PrematureCloseIsFailure(t *testing.T) {
	issue := linear.Issue{ID: "issue-id", Identifier: "ENG-44", Title: "silent close"}
	issue.Project.Name = "Project"
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	d := &nativeDispatcher{factory: &stubFactory{behavior: providerstub.BehaviorSilentFail}}
	ad, err := d.Dispatch(ctx, issue, Config{GitRoot: t.TempDir()})
	if err == nil {
		t.Fatal("expected dispatch error on premature close, got nil")
	}
	if ad.Status != DispatchFailed {
		t.Fatalf("Status = %q, want failed", ad.Status)
	}
	if ad.Error == nil || !strings.Contains(ad.Error.Error(), "closed before terminal") {
		t.Fatalf("Error = %v, want premature-close failure", ad.Error)
	}
}

// TestNativeDispatcher_UnsupportedHarnessFailsClosed proves an unknown
// --harness value fails before any provider constructs.
func TestNativeDispatcher_UnsupportedHarnessFailsClosed(t *testing.T) {
	issue := linear.Issue{ID: "issue-id", Identifier: "ENG-45", Title: "bad harness"}
	issue.Project.Name = "Project"
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var calls atomic.Int64
	d := &nativeDispatcher{factory: &stubFactory{calls: &calls}}
	_, err := d.Dispatch(ctx, issue, Config{GitRoot: t.TempDir(), Harness: "nope"})
	if err == nil {
		t.Fatal("expected unsupported-harness error, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported harness") {
		t.Fatalf("Error = %v, want unsupported-harness failure", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("factory calls = %d, want 0 (fail before construct)", got)
	}
}

// TestNativeDispatcher_BadTemplateDirFailsBeforeSpawn proves a missing
// template directory fails the dispatch without constructing a provider.
func TestNativeDispatcher_BadTemplateDirFailsBeforeSpawn(t *testing.T) {
	issue := linear.Issue{ID: "issue-id", Identifier: "ENG-46", Title: "bad templates"}
	issue.Project.Name = "Project"
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var calls atomic.Int64
	d := &nativeDispatcher{factory: &stubFactory{calls: &calls}}
	_, err := d.Dispatch(ctx, issue, Config{GitRoot: t.TempDir(), TemplateDir: filepath.Join(t.TempDir(), "does-not-exist")})
	if err == nil {
		t.Fatal("expected template-dir error, got nil")
	}
	if !strings.Contains(err.Error(), "template dir") {
		t.Fatalf("Error = %v, want template-dir failure", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("factory calls = %d, want 0 (fail before spawn)", got)
	}
}

// TestNativeDispatcher_CancelStopsOwnedWork proves cancelling the
// dispatch context fails the run and releases the owned provider.
func TestNativeDispatcher_CancelStopsOwnedWork(t *testing.T) {
	issue := linear.Issue{ID: "issue-id", Identifier: "ENG-47", Title: "cancel me"}
	issue.Project.Name = "Project"
	ctx, cancel := context.WithCancel(t.Context())
	f := &shutdownTrackingFactory{inner: &stubFactory{behavior: providerstub.BehaviorHangThenTimeout}}
	d := &nativeDispatcher{factory: f}
	type outcome struct {
		ad  *AgentDispatch
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		ad, err := d.Dispatch(ctx, issue, Config{GitRoot: t.TempDir()})
		done <- outcome{ad, err}
	}()
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case out := <-done:
		if out.err == nil {
			t.Fatal("expected cancellation error, got nil")
		}
		if out.ad.Status != DispatchFailed {
			t.Fatalf("Status = %q, want failed after cancel", out.ad.Status)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for cancelled dispatch")
	}
	if got := f.shutdowns.Load(); got != 1 {
		t.Fatalf("provider Shutdown calls = %d, want 1 (owned resource released)", got)
	}
}

// scriptedProvider is a minimal agent.Provider for terminal-event controls
// the stub behavior set does not cover (ResultEvent{Success:false}).
type scriptedProvider struct {
	events      []agent.Event
	shut        *atomic.Int64
	shutdownErr error
}

func (p *scriptedProvider) Name() agent.ProviderName { return agent.ProviderStub }
func (p *scriptedProvider) Capabilities() agent.Capabilities {
	return agent.Capabilities{}
}

func (p *scriptedProvider) Spawn(_ context.Context, _ agent.Spec) (agent.Handle, error) {
	return &scriptedHandle{events: append([]agent.Event(nil), p.events...)}, nil
}

func (p *scriptedProvider) Resume(_ context.Context, _ string, _ agent.Spec) (agent.Handle, error) {
	return nil, agent.ErrUnsupported
}

func (p *scriptedProvider) Shutdown(_ context.Context) error {
	if p.shut != nil {
		p.shut.Add(1)
	}
	return p.shutdownErr
}

// failingShutdownProvider wraps an agent.Provider, delegates Spawn to
// the inner provider, and fails Shutdown with the configured error.
// It models a provider whose session succeeds while releasing the
// owned resource fails.
type failingShutdownProvider struct {
	agent.Provider
	err error
}

func (p *failingShutdownProvider) Shutdown(_ context.Context) error { return p.err }

type scriptedHandle struct {
	events []agent.Event
	once   sync.Once
	ch     chan agent.Event
}

func (h *scriptedHandle) SessionID() string { return "scripted-session" }
func (h *scriptedHandle) Events() <-chan agent.Event {
	h.once.Do(func() {
		h.ch = make(chan agent.Event, len(h.events)+1)
		for _, ev := range h.events {
			h.ch <- ev
		}
		close(h.ch)
	})
	return h.ch
}
func (h *scriptedHandle) Inject(_ context.Context, _ string) error { return agent.ErrUnsupported }
func (h *scriptedHandle) Stop(_ context.Context) error             { return nil }

// TestNativeDispatcher_ShutdownFailureAfterSuccessPropagates is the
// causal control for the cleanup-error defect: a successful terminal
// ResultEvent followed by a Shutdown failure must label the dispatch
// failed AND return a non-nil error to the caller. The deferred
// cleanup runs after `return ad, ad.Error` values are evaluated, so
// the fix must assign the joined error to the named return — without
// it this test sees ad.Status=failed with err=nil.
func TestNativeDispatcher_ShutdownFailureAfterSuccessPropagates(t *testing.T) {
	issue := linear.Issue{ID: "issue-id", Identifier: "ENG-49", Title: "shutdown fails"}
	issue.Project.Name = "Project"
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	shutdownErr := errors.New("fixture shutdown failure")
	factory := ProviderFactoryFunc(func(ctx context.Context, _ string) (agent.Provider, error) {
		inner, err := (&stubFactory{}).NewProvider(ctx, HarnessClaude)
		if err != nil {
			return nil, err
		}
		return &failingShutdownProvider{Provider: inner, err: shutdownErr}, nil
	})
	d := &nativeDispatcher{factory: factory}
	ad, err := d.Dispatch(ctx, issue, Config{GitRoot: t.TempDir()})
	if err == nil {
		t.Fatal("expected dispatch error for Shutdown failure after success, got nil")
	}
	if !errors.Is(err, shutdownErr) {
		t.Fatalf("returned error = %v, want it to wrap the shutdown failure", err)
	}
	if ad.Status != DispatchFailed {
		t.Fatalf("Status = %q, want failed", ad.Status)
	}
	if ad.Error == nil || !errors.Is(ad.Error, shutdownErr) {
		t.Fatalf("AgentDispatch.Error = %v, want the shutdown failure recorded", ad.Error)
	}
}

// TestNativeDispatcher_TerminalAndShutdownErrorsJoin proves neither
// error is lost when the terminal outcome already failed and Shutdown
// also fails: the returned error must wrap both the terminal error
// and the shutdown failure.
func TestNativeDispatcher_TerminalAndShutdownErrorsJoin(t *testing.T) {
	issue := linear.Issue{ID: "issue-id", Identifier: "ENG-50", Title: "both fail"}
	issue.Project.Name = "Project"
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	shutdownErr := errors.New("fixture shutdown failure")
	factory := ProviderFactoryFunc(func(_ context.Context, _ string) (agent.Provider, error) {
		return &scriptedProvider{
			events: []agent.Event{
				agent.ResultEvent{Success: false, Message: "tests failed"},
			},
			shutdownErr: shutdownErr,
		}, nil
	})
	d := &nativeDispatcher{factory: factory}
	ad, err := d.Dispatch(ctx, issue, Config{GitRoot: t.TempDir()})
	if err == nil {
		t.Fatal("expected joined dispatch error, got nil")
	}
	if !errors.Is(err, shutdownErr) {
		t.Fatalf("returned error = %v, want it to wrap the shutdown failure", err)
	}
	if ad.Error == nil || !strings.Contains(ad.Error.Error(), "tests failed") {
		t.Fatalf("AgentDispatch.Error = %v, want the terminal failure preserved", ad.Error)
	}
	if ad.Status != DispatchFailed {
		t.Fatalf("Status = %q, want failed", ad.Status)
	}
}

// TestNativeDispatcher_FailedResultPropagates proves a terminal
// ResultEvent{Success:false} maps to DispatchFailed — the literal
// revert-RED control for the fire-and-forget blindness: with the old
// code (exit-code-ignored background wait / status never re-read) this
// failure mode was invisible.
func TestNativeDispatcher_FailedResultPropagates(t *testing.T) {
	issue := linear.Issue{ID: "issue-id", Identifier: "ENG-48", Title: "failed result"}
	issue.Project.Name = "Project"
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	factory := ProviderFactoryFunc(func(_ context.Context, _ string) (agent.Provider, error) {
		return &scriptedProvider{events: []agent.Event{
			agent.AssistantTextEvent{Text: "working"},
			agent.ResultEvent{Success: false, Message: "tests failed", ErrorSubtype: "error_during_execution"},
		}}, nil
	})
	d := &nativeDispatcher{factory: factory}
	ad, err := d.Dispatch(ctx, issue, Config{GitRoot: t.TempDir()})
	if err == nil {
		t.Fatal("expected dispatch error for Success=false result, got nil")
	}
	if ad.Status != DispatchFailed {
		t.Fatalf("Status = %q, want failed", ad.Status)
	}
}
