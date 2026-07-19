package daemon

import (
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// TestSessionDetailStore_BasicOps exercises Set/Get/Delete/Len.
func TestSessionDetailStore_BasicOps(t *testing.T) {
	s := newSessionDetailStore()
	if s.Len() != 0 {
		t.Errorf("Len = %d, want 0", s.Len())
	}
	if _, ok := s.Get("missing"); ok {
		t.Errorf("Get on empty store returned ok=true")
	}

	d := &SessionDetail{SessionID: "sess-1", AuthToken: "tok"}
	s.Set(d)
	if s.Len() != 1 {
		t.Errorf("Len = %d, want 1", s.Len())
	}
	got, ok := s.Get("sess-1")
	if !ok {
		t.Fatal("Get failed after Set")
	}
	if got.AuthToken != "tok" {
		t.Errorf("AuthToken = %q", got.AuthToken)
	}

	s.Delete("sess-1")
	if s.Len() != 0 {
		t.Errorf("Len after Delete = %d", s.Len())
	}
}

// TestSessionDetailStore_IgnoresEmpty verifies Set tolerates nil and
// empty-id entries.
func TestSessionDetailStore_IgnoresEmpty(t *testing.T) {
	s := newSessionDetailStore()
	s.Set(nil)
	s.Set(&SessionDetail{}) // empty SessionID
	if s.Len() != 0 {
		t.Errorf("Set(nil)/Set(empty) leaked entries: Len=%d", s.Len())
	}
}

func TestSessionDetailStore_UpdateRuntimeCredentials(t *testing.T) {
	s := newSessionDetailStore()
	s.Set(&SessionDetail{SessionID: "sess-1", WorkerID: "wkr_old", AuthToken: "old-token"})
	s.Set(&SessionDetail{SessionID: "sess-2", WorkerID: "wkr_old", AuthToken: "old-token"})

	s.UpdateRuntimeCredentials("wkr_new", "fresh-token")

	for _, id := range []string{"sess-1", "sess-2"} {
		got, ok := s.Get(id)
		if !ok {
			t.Fatalf("missing %s", id)
		}
		if got.WorkerID != "wkr_new" || got.AuthToken != "fresh-token" {
			t.Fatalf("%s credentials = (%q, %q), want (wkr_new, fresh-token)", id, got.WorkerID, got.AuthToken)
		}
	}
}

// TestSessionDetailStore_ConcurrentAccess sanity-checks the mutex
// against concurrent readers and writers. Run with -race to surface
// data races.
func TestSessionDetailStore_ConcurrentAccess(t *testing.T) {
	s := newSessionDetailStore()
	done := make(chan struct{})
	for i := 0; i < 50; i++ {
		go func(i int) {
			s.Set(&SessionDetail{SessionID: idFor(i)})
			_, _ = s.Get(idFor(i))
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < 50; i++ {
		<-done
	}
	if s.Len() != 50 {
		t.Errorf("Len = %d, want 50", s.Len())
	}
}

func idFor(i int) string {
	return "sess-" + intToStr(i)
}

func intToStr(i int) string {
	if i == 0 {
		return "0"
	}
	out := []byte{}
	for i > 0 {
		out = append([]byte{byte('0' + i%10)}, out...)
		i /= 10
	}
	return string(out)
}

func newSessionDetailLifecycleDaemon(spawnerOpts SpawnerOptions) *Daemon {
	d := New(Options{})
	d.spawner = NewWorkerSpawner(spawnerOpts)
	d.spawner.On(func(ev SessionEvent) {
		if ev.Kind == SessionEventEnded {
			d.sessionDetails.Delete(ev.Spec.SessionID)
		}
	})
	d.setState(StateRunning)
	return d
}

func TestDaemon_AcceptWorkWithDetail_AdmissionFailureRemovesDetail(t *testing.T) {
	d := newSessionDetailLifecycleDaemon(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "allowed", Repository: "github.com/a/allowed"}},
		MaxConcurrentSessions: 1,
	})
	detail := &SessionDetail{SessionID: "admission-fails", AuthToken: "transient-token"}

	_, err := d.AcceptWorkWithDetail(SessionSpec{
		SessionID:  "admission-fails",
		Repository: "github.com/a/rejected",
	}, detail)
	if err == nil || !strings.Contains(err.Error(), "is not configured") {
		t.Fatalf("AcceptWorkWithDetail error = %v, want admission failure", err)
	}
	if _, ok := d.SessionDetail(detail.SessionID); ok {
		t.Fatal("rejected session detail remained stored")
	}
	if got := d.sessionDetails.Len(); got != 0 {
		t.Errorf("session detail count = %d, want 0", got)
	}
}

func TestDaemon_AcceptWorkWithDetail_PreSpawnFailureRemovesDetail(t *testing.T) {
	preSpawnErr := errors.New("credential gate refused spawn")
	d := newSessionDetailLifecycleDaemon(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		OnPreSpawn: func(SessionSpec, []string) ([]string, error) {
			return nil, preSpawnErr
		},
	})
	detail := &SessionDetail{SessionID: "pre-spawn-fails", AuthToken: "transient-token"}

	_, err := d.AcceptWorkWithDetail(SessionSpec{
		SessionID:  "pre-spawn-fails",
		Repository: "github.com/a/b",
	}, detail)
	if !errors.Is(err, preSpawnErr) {
		t.Fatalf("AcceptWorkWithDetail error = %v, want original pre-spawn error", err)
	}
	if _, ok := d.SessionDetail(detail.SessionID); ok {
		t.Fatal("pre-spawn-rejected session detail remained stored")
	}
}

func TestDaemon_AcceptWorkWithDetail_StartFailureCleanupIsIdempotent(t *testing.T) {
	var d *Daemon
	var abortCalls atomic.Int32
	var abortedErr error
	d = newSessionDetailLifecycleDaemon(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		WorkerCommand:         []string{filepath.Join(t.TempDir(), "missing-worker")},
		OnPreSpawn: func(_ SessionSpec, env []string) ([]string, error) {
			return env, nil
		},
		OnSpawnAborted: func(spec SessionSpec, err error) {
			abortCalls.Add(1)
			abortedErr = err
			// A composing caller may own per-spawn resources and release them in
			// this hook. Deleting the detail here races neither with nor changes
			// the daemon's own unconditional error cleanup: Delete is idempotent.
			d.sessionDetails.Delete(spec.SessionID)
		},
	})
	detail := &SessionDetail{SessionID: "start-fails", AuthToken: "transient-token"}

	_, err := d.AcceptWorkWithDetail(SessionSpec{
		SessionID:  "start-fails",
		Repository: "github.com/a/b",
	}, detail)
	if err == nil {
		t.Fatal("AcceptWorkWithDetail: expected start failure")
	}
	if abortedErr != err {
		t.Fatalf("abort callback error = %v, want exact returned error %v", abortedErr, err)
	}
	if got := abortCalls.Load(); got != 1 {
		t.Errorf("OnSpawnAborted calls = %d, want 1", got)
	}
	if _, ok := d.SessionDetail(detail.SessionID); ok {
		t.Fatal("start-failed session detail remained stored")
	}
	if got := d.sessionDetails.Len(); got != 0 {
		t.Errorf("session detail count = %d, want 0 after duplicate-safe cleanup", got)
	}
}

func TestDaemon_AcceptWorkWithDetail_SuccessStoresUntilSessionEnd(t *testing.T) {
	var d *Daemon
	var sawDetailDuringPreSpawn bool
	d = newSessionDetailLifecycleDaemon(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		WorkerCommand:         []string{"/bin/sh", "-c", "exit 0"},
		StdoutPrefixWriter:    discardPrefixWriter{},
		StderrPrefixWriter:    discardPrefixWriter{},
		OnPreSpawn: func(spec SessionSpec, env []string) ([]string, error) {
			stored, ok := d.SessionDetail(spec.SessionID)
			sawDetailDuringPreSpawn = ok && stored.AuthToken == "transient-token"
			return env, nil
		},
	})
	ended := sessionEnds(d.spawner)
	detail := &SessionDetail{SessionID: "success", AuthToken: "transient-token"}

	if _, err := d.AcceptWorkWithDetail(SessionSpec{
		SessionID:  "success",
		Repository: "github.com/a/b",
	}, detail); err != nil {
		t.Fatalf("AcceptWorkWithDetail: %v", err)
	}
	if !sawDetailDuringPreSpawn {
		t.Fatal("session detail was not stored before OnPreSpawn")
	}
	waitSessionEnd(t, ended)
	if _, ok := d.SessionDetail(detail.SessionID); ok {
		t.Fatal("completed session detail remained stored after SessionEventEnded")
	}
}
