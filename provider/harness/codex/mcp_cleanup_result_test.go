package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// A successful turn whose final MCP config release fails must still deliver
// its result, with the cleanup failure as a warning — contained or not. A
// turn that did not finish successfully keeps failing closed.
func TestHandle_TerminalResultSurvivesContainedMCPCleanupFailure(t *testing.T) {
	cleanupErr := codexMCPApplicationError("clear isolated MCP config: isolated config boundary rejected: isolated Codex home mode is 0750, want 0700")
	cases := []struct {
		name        string
		release     func() error
		params      string
		wantResult  bool
		wantSuccess bool
		wantWarning bool
		wantError   bool
	}{
		{
			name:        "clean release",
			release:     func() error { return nil },
			params:      `{"turn":{"id":"t","status":"completed"}}`,
			wantResult:  true,
			wantSuccess: true,
		},
		{
			name:        "contained failure after completed turn",
			release:     func() error { return &mcpCleanupError{err: cleanupErr, contained: true} },
			params:      `{"turn":{"id":"t","status":"completed"}}`,
			wantResult:  true,
			wantSuccess: true,
			wantWarning: true,
		},
		{
			name:        "contained failure after failed turn keeps the turn's own result",
			release:     func() error { return &mcpCleanupError{err: cleanupErr, contained: true} },
			params:      `{"turn":{"id":"t","status":"failed","error":{"message":"boom"}}}`,
			wantResult:  true,
			wantWarning: true,
		},
		{
			name:        "uncontained failure after successful turn still delivers the result with a warning",
			release:     func() error { return &mcpCleanupError{err: cleanupErr} },
			params:      `{"turn":{"id":"t","status":"completed"}}`,
			wantResult:  true,
			wantSuccess: true,
			wantWarning: true,
		},
		{
			name:        "untyped failure after successful turn still delivers the result with a warning",
			release:     func() error { return cleanupErr },
			params:      `{"turn":{"id":"t","status":"completed"}}`,
			wantResult:  true,
			wantSuccess: true,
			wantWarning: true,
		},
		{
			name:      "uncontained failure after failed turn stays fatal",
			release:   func() error { return &mcpCleanupError{err: cleanupErr} },
			params:    `{"turn":{"id":"t","status":"failed","error":{"message":"boom"}}}`,
			wantError: true,
		},
		{
			name:      "untyped failure after failed turn stays fatal",
			release:   func() error { return cleanupErr },
			params:    `{"turn":{"id":"t","status":"failed","error":{"message":"boom"}}}`,
			wantError: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := notificationHandle()
			h.mcpRelease = tc.release
			var result *agent.ResultEvent
			var warning *agent.SystemEvent
			var cleanup *agent.ErrorEvent
			warningBeforeResult := false
			for _, event := range dispatch(t, h, "turn/completed", tc.params) {
				switch value := event.(type) {
				case agent.ResultEvent:
					result = &value
				case agent.SystemEvent:
					if value.Subtype == "codex_warning" {
						warning = &value
						warningBeforeResult = result == nil
					}
				case agent.ErrorEvent:
					if value.Code == "mcp_cleanup_failed" {
						cleanup = &value
					}
				}
			}
			if (result != nil) != tc.wantResult {
				t.Fatalf("result = %+v, want present=%v", result, tc.wantResult)
			}
			if result != nil && result.Success != tc.wantSuccess {
				t.Fatalf("result success = %v, want %v", result.Success, tc.wantSuccess)
			}
			if (warning != nil) != tc.wantWarning {
				t.Fatalf("cleanup warning = %+v, want present=%v", warning, tc.wantWarning)
			}
			if warning != nil {
				if !warningBeforeResult {
					t.Fatal("cleanup warning must precede the terminal result so it is not dropped after Events closes")
				}
				if !strings.Contains(warning.Message, "home mode is 0750") {
					t.Fatalf("cleanup warning = %q, want the bounded cause", warning.Message)
				}
			}
			if (cleanup != nil) != tc.wantError {
				t.Fatalf("cleanup error = %+v, want present=%v", cleanup, tc.wantError)
			}
		})
	}
}

// The real release path: a private home widened after acquire (for example
// by a process that inherited CODEX_HOME) is refused by the boundary before
// any RPC. The release poisons the Provider, removes the owned home, and so
// reports a contained failure whose detail names the cause. A failed owner
// removal (parent identity changed) is not contained.
func TestProvider_FinalReleaseBoundaryRejectionContainment(t *testing.T) {
	cases := []struct {
		name          string
		tamper        func(t *testing.T, p *Provider)
		wantContained bool
		wantRetained  bool
		wantDetail    string
	}{
		{
			name: "widened home is contained",
			tamper: func(t *testing.T, p *Provider) {
				if err := os.Chmod(p.config.home, 0o750); err != nil { //nolint:gosec // widen the owned home to trip validate()
					t.Fatal(err)
				}
			},
			wantContained: true,
			wantDetail:    "isolated Codex home mode is 0750, want 0700",
		},
		{
			// A completed turn usually leaves a rollout under sessions/, so the
			// owner keeps the home under the retention rule and removes only
			// cache/. That is still contained (writer stopped, Provider
			// poisoned), and the home is kept exactly as any session end keeps it.
			name: "widened home with resumable session state is contained and retained",
			tamper: func(t *testing.T, p *Provider) {
				sessions := filepath.Join(p.config.home, codexSessionStateSubdir)
				if err := os.MkdirAll(sessions, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(sessions, "rollout-test.jsonl"), []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(p.config.home, codexPluginCacheSubdir), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(p.config.home, 0o750); err != nil { //nolint:gosec // widen the owned home to trip validate()
					t.Fatal(err)
				}
			},
			wantContained: true,
			wantRetained:  true,
			wantDetail:    "isolated Codex home mode is 0750, want 0700",
		},
		{
			name: "failed owner removal is not contained",
			tamper: func(t *testing.T, p *Provider) {
				other, err := os.Stat(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				p.config.parentInfo = other
			},
			wantDetail: "isolated Codex parent identity changed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs, stdinW, stdoutR := newFakeServer()
			go fs.run(t, "thread-release-boundary")
			p, err := New(Options{
				skipProcess: true, stdinOverride: stdinW, stdoutOverride: stdoutR,
				configTempDir: t.TempDir(),
			})
			if err != nil {
				fs.close()
				t.Fatalf("New: %v", err)
			}
			startTestProvider(t, p)
			client := p.client
			t.Cleanup(func() {
				client.Stop(errors.New("test complete"))
				fs.close()
			})
			release, err := p.acquireMCPConfig(t.Context(), map[string]any{}, t.TempDir())
			if err != nil {
				t.Fatalf("acquire: %v", err)
			}
			ownedHome := p.config.home
			tc.tamper(t, p)

			err = release()
			if err == nil {
				t.Fatal("release succeeded over a rejected boundary")
			}
			if got := mcpCleanupContained(err); got != tc.wantContained {
				t.Fatalf("contained = %v, want %v (err %v)", got, tc.wantContained, err)
			}
			if !strings.Contains(err.Error(), tc.wantDetail) {
				t.Fatalf("release error = %v, want detail %q", err, tc.wantDetail)
			}
			var adaptation *agent.ToolAdaptationError
			if !errors.As(err, &adaptation) || adaptation.Code != agent.ToolDenialApplicationFailed {
				t.Fatalf("release error = %v, want typed MCP application failure", err)
			}
			switch {
			case tc.wantRetained:
				if _, statErr := os.Stat(filepath.Join(ownedHome, codexSessionStateSubdir, "rollout-test.jsonl")); statErr != nil {
					t.Fatalf("retained home lost its resumable session state: %v", statErr)
				}
				if _, statErr := os.Stat(filepath.Join(ownedHome, codexPluginCacheSubdir)); !os.IsNotExist(statErr) {
					t.Fatalf("retained home kept cache/: stat err=%v", statErr)
				}
			case tc.wantContained:
				if _, statErr := os.Stat(ownedHome); !os.IsNotExist(statErr) {
					t.Fatalf("contained release left the owned home: stat err=%v", statErr)
				}
			}
			if _, err := p.acquireMCPConfig(context.Background(), map[string]any{}, t.TempDir()); err == nil {
				t.Fatal("provider accepted a new lease after a failed final release")
			}
		})
	}
}

// Production wiring: with a real app-server, the boundary's stopWriter is
// stopProcess, and the process exit makes watchExit (or the read loop's EOF)
// stop the client on another goroutine. onClientClose then fails every
// registered handle. The handle that is releasing after its terminal event
// must not be one of them: a contained failure still delivers the warning and
// the result, with no mcp_cleanup_failed error and no early Events close.
func TestHandle_ContainedCleanupResultSurvivesWriterStopClientClose(t *testing.T) {
	for i := range 3 {
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			fs, stdinW, stdoutR := newFakeServer()
			go fs.run(t, "thread-writer-stop")
			p, err := New(Options{
				skipProcess: true, stdinOverride: stdinW, stdoutOverride: stdoutR,
				configTempDir: t.TempDir(),
			})
			if err != nil {
				fs.close()
				t.Fatalf("New: %v", err)
			}
			startTestProvider(t, p)
			client := p.client
			closed := make(chan struct{})
			client.SetOnClose(func(cause error) {
				p.onClientClose(cause)
				close(closed)
			})
			t.Cleanup(func() {
				client.Stop(errors.New("test complete"))
				fs.close()
			})
			// stopProcess ordering: the writer is stopped, then the exit closes
			// the client asynchronously. Hold the release until onClientClose
			// has snapshotted the live handles, the window in which a still-
			// registered handle is failed over its own result.
			p.config.stopWriter = func() error {
				go client.Stop(errors.New("codex app-server exited"))
				<-client.Done()
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					p.handlesMu.Lock()
					n := len(p.handles)
					p.handlesMu.Unlock()
					if n == 0 {
						return nil
					}
					time.Sleep(time.Millisecond)
				}
				return errors.New("onClientClose never snapshotted the live handles")
			}

			h := newHandle(p, client, agent.Spec{}, HandleOptions{EventBuffer: 512})
			release, err := p.acquireMCPConfig(t.Context(), map[string]any{}, t.TempDir())
			if err != nil {
				t.Fatalf("acquire: %v", err)
			}
			h.mcpRelease = release
			p.registerHandle(h)
			if err := os.Chmod(p.config.home, 0o750); err != nil { //nolint:gosec // widen the owned home to trip validate()
				t.Fatal(err)
			}

			h.handleNotification(notification{Method: "turn/completed", Params: json.RawMessage(`{"turn":{"id":"t","status":"completed"}}`)})
			select {
			case <-closed:
			case <-time.After(10 * time.Second):
				t.Fatal("client close callback never finished")
			}
			// The forwarder's own exit path.
			h.signalClosed()
			h.closeEvents()

			var kinds []string
			for event := range h.Events() {
				switch value := event.(type) {
				case agent.ResultEvent:
					if !value.Success {
						t.Fatalf("result success = false, want the completed turn")
					}
					kinds = append(kinds, "result")
				case agent.SystemEvent:
					if value.Subtype == "codex_warning" {
						kinds = append(kinds, "warning")
					}
				case agent.ErrorEvent:
					kinds = append(kinds, "error:"+value.Code)
				}
			}
			if got := strings.Join(kinds, ","); got != "warning,result" {
				t.Fatalf("events = %q, want %q", got, "warning,result")
			}
		})
	}
}

// The final clear is retried with a short bounded backoff before it is
// reported as failed: one transient batchWrite failure still releases cleanly
// and the provider stays usable for the next lease.
func TestProvider_FinalReleaseRetriesThenSucceeds(t *testing.T) {
	fs, stdinW, stdoutR := newFakeServer()
	go fs.run(t, "thread-retry")
	p, err := New(Options{
		skipProcess: true, stdinOverride: stdinW, stdoutOverride: stdoutR,
		configTempDir: t.TempDir(), verifyMCPReadback: true,
	})
	if err != nil {
		fs.close()
		t.Fatalf("New: %v", err)
	}
	startTestProvider(t, p)
	t.Cleanup(func() {
		p.client.Stop(errors.New("test complete"))
		fs.close()
	})
	release, err := p.acquireMCPConfig(t.Context(), map[string]any{}, t.TempDir())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// Fail the next clear once with a non-transient code so the client's own
	// RequestWithRetry does not mask the outer release retry.
	fs.mu.Lock()
	fs.failNextBatchWrites = 1
	fs.failBatchWriteCode = -32602
	fs.mu.Unlock()
	writesBefore := func() int {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return len(fs.mcpWrites)
	}()
	if err := release(); err != nil {
		t.Fatalf("release after one transient failure = %v, want nil (retry-then-success)", err)
	}
	fs.mu.Lock()
	writesAfter := len(fs.mcpWrites)
	fs.mu.Unlock()
	if writesAfter < writesBefore+2 {
		t.Fatalf("batchWrite count = %d, want at least %d (failed attempt + retry)", writesAfter, writesBefore+2)
	}
	if _, err := p.acquireMCPConfig(context.Background(), map[string]any{}, t.TempDir()); err != nil {
		t.Fatalf("provider refused a new lease after retry-then-success: %v", err)
	}
}

// The final clear runs under one shared budget, not a fresh window per
// attempt: when the server stops answering, the first attempt consumes the
// budget and no further clear is sent, so the already-known terminal result
// is reported before the runner's deadline instead of being held past it.
func TestProvider_FinalReleaseHungServerSendsOnce(t *testing.T) {
	fs, stdinW, stdoutR := newFakeServer()
	go fs.run(t, "thread-hung")
	p, err := New(Options{
		skipProcess: true, stdinOverride: stdinW, stdoutOverride: stdoutR,
		configTempDir: t.TempDir(), RPCTimeout: 300 * time.Millisecond,
	})
	if err != nil {
		fs.close()
		t.Fatalf("New: %v", err)
	}
	startTestProvider(t, p)
	t.Cleanup(func() {
		p.client.Stop(errors.New("test complete"))
		fs.close()
	})
	release, err := p.acquireMCPConfig(t.Context(), map[string]any{}, t.TempDir())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	fs.mu.Lock()
	fs.hangBatchWrite = true
	writesBefore := len(fs.mcpWrites)
	fs.mu.Unlock()
	start := time.Now()
	err = release()
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("release against a hung server = nil, want the bounded failure (poison)")
	}
	fs.mu.Lock()
	sends := len(fs.mcpWrites) - writesBefore
	fs.mu.Unlock()
	if sends != 1 {
		t.Fatalf("hung-server clear sends = %d, want exactly 1 (no fresh window per retry)", sends)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("hung-server release took %v, want within one shared budget (well under per-attempt windows stacked)", elapsed)
	}
	if _, err := p.acquireMCPConfig(context.Background(), map[string]any{}, t.TempDir()); err == nil {
		t.Fatal("provider granted a new lease after a failed release, want poisoned")
	}
}

// Table-driven over the fake app-server: a successful turn's result survives
// a cleanup failure whether the failure is contained (owner removal clean)
// or not (owner removal failed). Both go through the real release path so the
// retry, poison and retention/sweep wiring is exercised, not only the mapper.
func TestHandle_SuccessfulResultSurvivesCleanupFailureOverFakeServer(t *testing.T) {
	cases := []struct {
		name          string
		failClears    int
		breakRemoval  bool
		wantContained bool
	}{
		{name: "contained", failClears: 10, wantContained: true},
		{name: "non-contained", failClears: 10, breakRemoval: true, wantContained: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs, stdinW, stdoutR := newFakeServer()
			go fs.run(t, "thread-cleanup-"+tc.name)
			p, err := New(Options{
				skipProcess: true, stdinOverride: stdinW, stdoutOverride: stdoutR,
				configTempDir: t.TempDir(),
			})
			if err != nil {
				fs.close()
				t.Fatalf("New: %v", err)
			}
			startTestProvider(t, p)
			t.Cleanup(func() {
				p.client.Stop(errors.New("test complete"))
				fs.close()
			})
			release, err := p.acquireMCPConfig(t.Context(), map[string]any{}, t.TempDir())
			if err != nil {
				t.Fatalf("acquire: %v", err)
			}
			// Fail every clear attempt so the bounded retry exhausts and the
			// release reports a failure; the removal outcome decides
			// contained vs not. Non-transient keeps the client retry out.
			fs.mu.Lock()
			fs.failNextBatchWrites = tc.failClears
			fs.failBatchWriteCode = -32602
			fs.mu.Unlock()
			if tc.breakRemoval {
				other, err := os.Stat(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				p.config.parentInfo = other
			}
			h := newHandle(p, p.client, agent.Spec{}, HandleOptions{EventBuffer: 512})
			h.mcpRelease = release
			p.registerHandle(h)
			h.handleNotification(notification{Method: "turn/completed", Params: json.RawMessage(`{"turn":{"id":"t","status":"completed"}}`)})
			h.signalClosed()
			h.closeEvents()
			var kinds []string
			var warning *agent.SystemEvent
			for event := range h.Events() {
				switch value := event.(type) {
				case agent.ResultEvent:
					if !value.Success {
						t.Fatalf("result success = false, want the completed turn")
					}
					kinds = append(kinds, "result")
				case agent.SystemEvent:
					if value.Subtype == "codex_warning" {
						warning = &value
						kinds = append(kinds, "warning")
					}
				case agent.ErrorEvent:
					kinds = append(kinds, "error:"+value.Code)
				}
			}
			if got := strings.Join(kinds, ","); got != "warning,result" {
				t.Fatalf("events = %q, want %q (contained=%v)", got, "warning,result", tc.wantContained)
			}
			if warning == nil || !strings.Contains(warning.Message, "clear isolated MCP config") {
				t.Fatalf("warning = %+v, want the bounded clear detail", warning)
			}
			// Only a contained removal proves the writer stopped: the
			// rotated-parent case below must say unconfirmed while the
			// app-server still answers, never that it was stopped.
			if tc.wantContained {
				if !strings.Contains(warning.Message, "was stopped") || strings.Contains(warning.Message, "unconfirmed") {
					t.Fatalf("contained warning must report stopped, got %q", warning.Message)
				}
			} else {
				if strings.Contains(warning.Message, "was stopped") || !strings.Contains(warning.Message, "unconfirmed") {
					t.Fatalf("uncontained warning must report shutdown unconfirmed, got %q", warning.Message)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_, err := p.client.Request(ctx, "config/read", map[string]any{"includeLayers": true}, 5*time.Second)
				cancel()
				if err != nil {
					t.Fatalf("app-server ping after uncontained cleanup = %v, want success (writer still alive)", err)
				}
			}
		})
	}
}

// Only the leading clause of a boundary error reaches the event stream; a
// wrapped path or OS error never does, and the reason stays bounded.
func TestMCPBoundaryErrorReasonIsBounded(t *testing.T) {
	long := strings.Repeat("x", 200)
	cases := []struct {
		name string
		err  error
		want string
	}{
		{name: "plain clause", err: errors.New("isolated Codex home mode is 0750, want 0700"), want: "isolated Codex home mode is 0750, want 0700"},
		{name: "wrapped path error", err: errors.New("stat isolated Codex home: /tmp/donmai-codex-home-1/config.toml: permission denied"), want: "stat isolated Codex home"},
		{name: "long clause truncated", err: errors.New(long), want: strings.Repeat("x", 120) + "..."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (&mcpBoundaryError{err: tc.err}).reason(); got != tc.want {
				t.Fatalf("reason() = %q, want %q", got, tc.want)
			}
			detail := codexMCPConfigFailureDetail(&mcpBoundaryError{err: tc.err})
			if strings.Contains(detail, "/tmp/") || strings.Contains(detail, "permission denied") {
				t.Fatalf("detail leaked the wrapped error: %q", detail)
			}
		})
	}
}

// Stream deltas never take the slots reserved for the terminal warning plus
// result: prefilling past cap-2 drops deltas, keeps the head (which carries
// the session identity), and still delivers both warning and result.
func TestHandle_TerminalResultReservedUnderBufferPressure(t *testing.T) {
	cleanupErr := codexMCPApplicationError("clear isolated MCP config: isolated config activation verification failed")
	h := newHandle(nil, nil, agent.Spec{}, HandleOptions{EventBuffer: 8})
	for i := 0; i < 64; i++ {
		h.emit(agent.SystemEvent{Subtype: "codex_progress", Message: "fill"})
	}
	// Two slots stay reserved for the terminal warning plus result.
	if got := len(h.events); got != 8-2 {
		t.Fatalf("buffered deltas = %d, want %d (deltas never take the reserved slots)", got, 8-2)
	}
	// Uncontained cleanup failure after a successful turn: the shape that
	// previously lost the result to the warning.
	h.mcpRelease = func() error { return &mcpCleanupError{err: cleanupErr} }
	h.handleNotification(notification{Method: "turn/completed", Params: json.RawMessage(`{"turn":{"id":"t","status":"completed"}}`)})
	var kinds []string
	for {
		select {
		case ev := <-h.Events():
			switch value := ev.(type) {
			case agent.ResultEvent:
				if !value.Success {
					t.Fatalf("result success = false, want the completed turn")
				}
				kinds = append(kinds, "result")
			case agent.SystemEvent:
				if value.Subtype == "codex_warning" {
					kinds = append(kinds, "warning")
				}
			case agent.ErrorEvent:
				kinds = append(kinds, "error:"+value.Code)
			}
		default:
			goto drained
		}
	}
drained:
	if got := strings.Join(kinds, ","); !strings.Contains(got, "warning") || !strings.Contains(got, "result") {
		t.Fatalf("kinds = %q, want warning and result alongside %d kept deltas", got, 8-2)
	}
	if strings.Contains(strings.Join(kinds, ","), "error:") {
		t.Fatalf("kinds = %q, want no terminal cleanup error after a successful turn", strings.Join(kinds, ","))
	}
	if got := kinds[len(kinds)-2:]; strings.Join(got, ",") != "warning,result" {
		t.Fatalf("terminal order = %q, want warning then result last", strings.Join(got, ","))
	}
}

// The session identity survives buffer pressure: a thread/started plus a
// flood of deltas and then a failed cleanup still delivers the head
// InitEvent (with the provider session id) first and the successful result
// last. Discarding the oldest queued event here would lose the InitEvent the
// runner uses for the result and recovery state.
func TestHandle_BufferPressureKeepsSessionIdentity(t *testing.T) {
	cleanupErr := codexMCPApplicationError("clear isolated MCP config: isolated config activation verification failed")
	h := newHandle(nil, nil, agent.Spec{}, HandleOptions{EventBuffer: 8})
	// start() records the thread id before the forwarder runs; mirror that
	// so the handle identity matches the InitEvent below.
	h.threadID = "thread-A"
	h.handleNotification(notification{Method: "thread/started", Params: json.RawMessage(`{"thread":{"id":"thread-A"}}`)})
	for i := 0; i < 64; i++ {
		h.emit(agent.SystemEvent{Subtype: "codex_progress", Message: "fill"})
	}
	h.mcpRelease = func() error { return &mcpCleanupError{err: cleanupErr} }
	h.handleNotification(notification{Method: "turn/completed", Params: json.RawMessage(`{"turn":{"id":"t","status":"completed"}}`)})
	var events []agent.Event
	for {
		select {
		case ev := <-h.Events():
			events = append(events, ev)
		default:
			goto drained
		}
	}
drained:
	if len(events) == 0 {
		t.Fatal("no events delivered")
	}
	init, ok := events[0].(agent.InitEvent)
	if !ok {
		t.Fatalf("first event = %T, want InitEvent (head must survive buffer pressure)", events[0])
	}
	if init.SessionID != "thread-A" {
		t.Fatalf("InitEvent session = %q, want thread-A", init.SessionID)
	}
	result, ok := events[len(events)-1].(agent.ResultEvent)
	if !ok || !result.Success {
		t.Fatalf("last event = %T %+v, want successful ResultEvent", events[len(events)-1], events[len(events)-1])
	}
	if got := h.SessionID(); got != "thread-A" {
		t.Fatalf("handle session = %q, want thread-A", got)
	}
}

// A channel with exactly one free slot still delivers the terminal without
// discarding the head: the warning is dropped (it stays in the logs) and the
// successful result is delivered after the queued history, in order.
func TestHandle_OneSlotLeftKeepsHeadForTerminal(t *testing.T) {
	cleanupErr := codexMCPApplicationError("clear isolated MCP config: isolated config activation verification failed")
	h := newHandle(nil, nil, agent.Spec{}, HandleOptions{EventBuffer: 4})
	for i := 0; i < 3; i++ {
		h.events <- agent.SystemEvent{Subtype: "codex_progress", Message: "direct"}
	}
	h.mcpRelease = func() error { return &mcpCleanupError{err: cleanupErr} }
	h.handleNotification(notification{Method: "turn/completed", Params: json.RawMessage(`{"turn":{"id":"t","status":"completed"}}`)})
	var events []agent.Event
	for {
		select {
		case ev := <-h.Events():
			events = append(events, ev)
		default:
			goto drained
		}
	}
drained:
	if len(events) != 4 {
		t.Fatalf("events = %d, want 3 head progress events plus the terminal result", len(events))
	}
	for i := 0; i < 3; i++ {
		if _, ok := events[i].(agent.SystemEvent); !ok {
			t.Fatalf("event %d = %T, want the head progress history in order (oldest must not be discarded)", i, events[i])
		}
	}
	result, ok := events[3].(agent.ResultEvent)
	if !ok || !result.Success {
		t.Fatalf("last event = %T %+v, want successful ResultEvent", events[3], events[3])
	}
}

// A channel filled bypassing emit (production deltas can never do this: the
// reservation caps them at cap-2) still delivers the terminal without
// discarding the head: the terminal waits for the consumer to drain rather
// than dropping the oldest queued event.
func TestHandle_FullBufferDeliversTerminalKeepsHead(t *testing.T) {
	cleanupErr := codexMCPApplicationError("clear isolated MCP config: isolated config activation verification failed")
	h := newHandle(nil, nil, agent.Spec{}, HandleOptions{EventBuffer: 4})
	for i := 0; i < 4; i++ {
		h.events <- agent.SystemEvent{Subtype: "codex_progress", Message: "direct"}
	}
	h.mcpRelease = func() error { return &mcpCleanupError{err: cleanupErr} }
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.handleNotification(notification{Method: "turn/completed", Params: json.RawMessage(`{"turn":{"id":"t","status":"completed"}}`)})
	}()
	var events []agent.Event
	timeout := time.After(10 * time.Second)
collect:
	for {
		select {
		case ev := <-h.Events():
			events = append(events, ev)
			if result, ok := ev.(agent.ResultEvent); ok {
				if !result.Success {
					t.Fatalf("result success = false, want the completed turn")
				}
				break collect
			}
		case <-timeout:
			t.Fatalf("timed out waiting for the terminal result (got %d events)", len(events))
		}
	}
	<-done
	if len(events) == 0 {
		t.Fatal("no events delivered")
	}
	if _, ok := events[0].(agent.SystemEvent); !ok {
		t.Fatalf("first event = %T, want the head progress event (oldest must not be discarded)", events[0])
	}
	progress := 0
	for _, ev := range events[:len(events)-1] {
		if sys, ok := ev.(agent.SystemEvent); ok && sys.Subtype == "codex_progress" {
			progress++
		}
		if _, ok := ev.(agent.ErrorEvent); ok {
			t.Fatalf("got terminal cleanup error after a successful turn")
		}
	}
	if progress != 4 {
		t.Fatalf("kept progress events = %d, want 4 (the whole head survives)", progress)
	}
}

// Only a contained removal proves the writer stopped. An uncontained (or
// untyped) cleanup failure after a successful turn reports shutdown as
// unconfirmed so a live writer is never described as stopped.
func TestHandle_CleanupWarningStatesWriterShutdownAccurately(t *testing.T) {
	cleanupErr := codexMCPApplicationError("clear isolated MCP config: isolated config boundary rejected: isolated Codex home mode is 0750, want 0700")
	cases := []struct {
		name            string
		release         func() error
		wantStopped     bool
		wantUnconfirmed bool
	}{
		{
			name:            "contained reports stopped",
			release:         func() error { return &mcpCleanupError{err: cleanupErr, contained: true} },
			wantStopped:     true,
			wantUnconfirmed: false,
		},
		{
			name:            "uncontained reports unconfirmed",
			release:         func() error { return &mcpCleanupError{err: cleanupErr} },
			wantStopped:     false,
			wantUnconfirmed: true,
		},
		{
			name:            "untyped reports unconfirmed",
			release:         func() error { return cleanupErr },
			wantStopped:     false,
			wantUnconfirmed: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := notificationHandle()
			h.mcpRelease = tc.release
			var warning *agent.SystemEvent
			for _, ev := range dispatch(t, h, "turn/completed", `{"turn":{"id":"t","status":"completed"}}`) {
				if value, ok := ev.(agent.SystemEvent); ok && value.Subtype == "codex_warning" {
					warning = &value
				}
			}
			if warning == nil {
				t.Fatal("no codex_warning emitted alongside the successful result")
			}
			if got := strings.Contains(warning.Message, "was stopped"); got != tc.wantStopped {
				t.Fatalf("warning stopped claim = %v, want %v: %q", got, tc.wantStopped, warning.Message)
			}
			if got := strings.Contains(warning.Message, "unconfirmed"); got != tc.wantUnconfirmed {
				t.Fatalf("warning unconfirmed claim = %v, want %v: %q", got, tc.wantUnconfirmed, warning.Message)
			}
		})
	}
}
