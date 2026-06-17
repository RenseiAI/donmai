package daemon

import (
	"context"
	"errors"
	"testing"
)

// discardPrefixWriter swallows worker stdout/stderr so spawn tests stay quiet.
type discardPrefixWriter struct{}

func (discardPrefixWriter) WriteWorkerLine(string, string) {}

// newRunningTestDaemon wires a Daemon into StateRunning with a spawner that
// runs a hermetic, instantly-exiting stub command (no network, no real worker
// binary) so handlePollWorkItem can be exercised directly. opts lets a test
// supply the two FD-4 hooks; everything else is filled with safe defaults.
//
// onPreSpawn, when non-nil, is layered onto the spawner so a test can capture
// the per-session state the daemon stored before exec. It returns the env
// unchanged (a no-op) so the hermetic spawn still proceeds.
func newRunningTestDaemon(t *testing.T, opts Options, projects []ProjectConfig, onPreSpawn func(spec SessionSpec, env []string) ([]string, error)) *Daemon {
	t.Helper()
	if opts.ConfigPath == "" {
		opts.ConfigPath = "/dev/null"
	}
	d := New(opts)
	d.spawner = NewWorkerSpawner(SpawnerOptions{
		Projects:           projects,
		WorkerCommand:      []string{"/bin/sh", "-c", "exit 0"},
		StdoutPrefixWriter: discardPrefixWriter{},
		StderrPrefixWriter: discardPrefixWriter{},
		OnPreSpawn:         onPreSpawn,
	})
	d.mu.Lock()
	d.workerID = "wkr-test"
	d.mu.Unlock()
	d.setState(StateRunning)
	return d
}

// TestHandlePollWorkItem_LandingRunRoutesToHook verifies a landing-run poll
// item is dispatched to OnLandingWork (returns nil without spawning a session)
// when the hook is wired.
func TestHandlePollWorkItem_LandingRunRoutesToHook(t *testing.T) {
	var got PollWorkItem
	var called int
	var spawned int

	opts := Options{
		OnLandingWork: func(_ context.Context, item PollWorkItem) error {
			called++
			got = item
			return nil
		},
	}
	d := newRunningTestDaemon(t, opts, nil, func(SessionSpec, []string) ([]string, error) {
		spawned++
		return nil, nil
	})

	item := PollWorkItem{
		SessionID:  "land-1",
		WorkType:   LandingWorkType,
		Repository: "github.com/x/repo",
		IssueID:    "ISSUE-42",
		Branch:     "feat/x",
	}
	if err := d.handlePollWorkItem(item, "https://orchestrator.example"); err != nil {
		t.Fatalf("handlePollWorkItem: %v", err)
	}
	if called != 1 {
		t.Errorf("OnLandingWork called %d times, want 1", called)
	}
	if got.Repository != "github.com/x/repo" || got.IssueID != "ISSUE-42" || got.Branch != "feat/x" {
		t.Errorf("OnLandingWork received item %+v, want the full poll item threaded through", got)
	}
	if spawned != 0 {
		t.Errorf("spawn attempted %d times for a landing-run item, want 0 (never becomes a session)", spawned)
	}
	if d.spawner.ActiveCount() != 0 {
		t.Errorf("ActiveCount = %d, want 0 (landing-run never spawns)", d.spawner.ActiveCount())
	}
}

// TestHandlePollWorkItem_LandingHandlerErrorSwallowed verifies a landing-run
// handler error is logged best-effort and the poll item is still reported
// handled (returns nil) without spawning a session.
func TestHandlePollWorkItem_LandingHandlerErrorSwallowed(t *testing.T) {
	var spawned int
	opts := Options{
		OnLandingWork: func(context.Context, PollWorkItem) error {
			return errors.New("landing backend down")
		},
	}
	d := newRunningTestDaemon(t, opts, nil, func(SessionSpec, []string) ([]string, error) {
		spawned++
		return nil, nil
	})

	item := PollWorkItem{SessionID: "land-2", WorkType: LandingWorkType, Repository: "github.com/x/repo"}
	if err := d.handlePollWorkItem(item, "https://orchestrator.example"); err != nil {
		t.Fatalf("handlePollWorkItem should swallow handler error, got: %v", err)
	}
	if spawned != 0 {
		t.Errorf("spawn attempted %d times despite landing handler error, want 0", spawned)
	}
}

// TestHandlePollWorkItem_NilLandingHookFallsThrough verifies that with no
// OnLandingWork wired, a landing-run item is NOT diverted — it flows to the
// normal session-spawn path unchanged (proven by the spawn being attempted).
// This is the mixed-version-safe default: no producer emits LandingWorkType
// today, so the path is byte-identical to current behaviour.
func TestHandlePollWorkItem_NilLandingHookFallsThrough(t *testing.T) {
	var spawned int
	projects := []ProjectConfig{{ID: "proj", Repository: "github.com/x/repo"}}
	// OnLandingWork left nil.
	d := newRunningTestDaemon(t, Options{}, projects, func(SessionSpec, []string) ([]string, error) {
		spawned++
		return nil, nil // allow the hermetic spawn to proceed
	})

	item := PollWorkItem{SessionID: "land-3", WorkType: LandingWorkType, Repository: "github.com/x/repo"}
	if err := d.handlePollWorkItem(item, "https://orchestrator.example"); err != nil {
		t.Fatalf("handlePollWorkItem: %v", err)
	}
	if spawned != 1 {
		t.Errorf("spawn attempted %d times, want 1 (nil landing hook must fall through to the session path)", spawned)
	}
}

// TestHandlePollWorkItem_WorkerCapabilitiesThreaded verifies WorkerCapabilitiesFunc
// threads its flags onto the stored SessionDetail; nil ⇒ no capabilities. The
// detail is captured inside OnPreSpawn, which runs synchronously after the
// daemon stores the detail and before the child execs.
func TestHandlePollWorkItem_WorkerCapabilitiesThreaded(t *testing.T) {
	projects := []ProjectConfig{{ID: "proj", Repository: "github.com/x/repo"}}

	tests := []struct {
		name     string
		capsFunc func() map[string]bool
		wantCaps map[string]bool // nil means "Capabilities must be nil"
	}{
		{
			name:     "nil func leaves capabilities nil",
			capsFunc: nil,
			wantCaps: nil,
		},
		{
			name:     "func returning empty map leaves capabilities nil",
			capsFunc: func() map[string]bool { return map[string]bool{} },
			wantCaps: nil,
		},
		{
			name:     "func returning flags advertises them",
			capsFunc: func() map[string]bool { return map[string]bool{"deterministic-landing": true} },
			wantCaps: map[string]bool{"deterministic-landing": true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// holder lets the OnPreSpawn closure reach the daemon's
			// SessionDetail accessor; it is filled in right after construction
			// (OnPreSpawn only fires later, during the spawn).
			var holder *Daemon
			var captured *SessionDetail
			d := newRunningTestDaemon(t, Options{WorkerCapabilitiesFunc: tt.capsFunc}, projects,
				func(spec SessionSpec, env []string) ([]string, error) {
					// The daemon stores the detail before exec; read it back here.
					if dt, ok := holder.SessionDetail(spec.SessionID); ok {
						captured = dt
					}
					return env, nil
				})
			holder = d

			item := PollWorkItem{SessionID: "caps-1", Repository: "github.com/x/repo"}
			if err := d.handlePollWorkItem(item, "https://orchestrator.example"); err != nil {
				t.Fatalf("handlePollWorkItem: %v", err)
			}
			if captured == nil {
				t.Fatal("OnPreSpawn never captured a SessionDetail (spawn did not proceed)")
			}
			if tt.wantCaps == nil {
				if captured.Capabilities != nil {
					t.Errorf("Capabilities = %v, want nil (no caps advertised)", captured.Capabilities)
				}
				return
			}
			if len(captured.Capabilities) != len(tt.wantCaps) {
				t.Fatalf("Capabilities = %v, want %v", captured.Capabilities, tt.wantCaps)
			}
			for k, v := range tt.wantCaps {
				if captured.Capabilities[k] != v {
					t.Errorf("Capabilities[%q] = %v, want %v", k, captured.Capabilities[k], v)
				}
			}
		})
	}
}
