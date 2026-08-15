package daemon

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

func TestSessionDetailStore_DeleteIfOwnerRejectsStaleLease(t *testing.T) {
	s := newSessionDetailStore()
	staleLease, ok := s.StoreIfAbsent(&SessionDetail{SessionID: "same", AuthToken: "first"})
	if !ok {
		t.Fatal("first StoreIfAbsent rejected")
	}
	s.Delete("same")
	currentLease, ok := s.StoreIfAbsent(&SessionDetail{SessionID: "same", AuthToken: "second"})
	if !ok {
		t.Fatal("second StoreIfAbsent rejected after delete")
	}

	if s.DeleteIfOwner(staleLease) {
		t.Fatal("stale lease deleted a later generation")
	}
	got, ok := s.Get("same")
	if !ok || got.AuthToken != "second" {
		t.Fatalf("current detail after stale rollback = %#v, %t", got, ok)
	}
	if !s.DeleteIfOwner(currentLease) {
		t.Fatal("current lease did not delete its own generation")
	}
}

func TestSessionDetailStore_UpdateRuntimeCredentials(t *testing.T) {
	s := newSessionDetailStore()
	s.Set(&SessionDetail{SessionID: "sess-1", WorkerID: "wkr_old", AuthToken: "old-token"})
	s.Set(&SessionDetail{SessionID: "sess-2", WorkerID: "wkr_old", AuthToken: "old-token"})

	if updated := s.UpdateRuntimeCredentials("wkr_old", "wkr_new", "fresh-token"); updated != 2 {
		t.Fatalf("UpdateRuntimeCredentials updated %d details, want 2", updated)
	}

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

// TestSessionDetailStore_UpdateRuntimeCredentials_IsWorkerScoped pins the
// scope boundary a credential refresh must never cross.
//
// One daemon process can serve SEVERAL worker identities: a host admitted to
// more than one organisation holds a registration per organisation, each
// refreshes its runtime token on its own schedule, and every identity's
// sessions share this one store. The sweep used to be unconditional, so one
// identity's routine refresh overwrote the others' sessions with its own
// worker id and bearer — those children then presented the wrong identity on
// every subsequent platform call and were rejected for the rest of their
// lives, while the refreshing identity's own sessions looked perfectly fine.
//
// The fixture is therefore two attributed identities plus one unattributed
// detail, and every case asserts the FULL end state of all four rows: a scope
// bug shows up as collateral damage on rows the call had no business touching,
// which an assertion that only checks the intended rows cannot see.
func TestSessionDetailStore_UpdateRuntimeCredentials_IsWorkerScoped(t *testing.T) {
	t.Parallel()

	type creds struct {
		workerID  string
		authToken string
	}

	// The seeded state, and therefore the baseline every unaffected row must
	// still hold after the call under test.
	seed := map[string]creds{
		"sess-a1":     {workerID: "wkr_a", authToken: "token-a"},
		"sess-a2":     {workerID: "wkr_a", authToken: "token-a"},
		"sess-b1":     {workerID: "wkr_b", authToken: "token-b"},
		"sess-orphan": {workerID: "", authToken: "token-orphan"},
	}

	tests := []struct {
		name         string
		prevWorkerID string
		workerID     string
		authToken    string
		wantUpdated  int
		// changed lists only the rows expected to move; every row absent from
		// it must still equal its seeded value.
		changed map[string]creds
	}{
		{
			name:         "in-place refresh touches only its own identity",
			prevWorkerID: "wkr_a",
			workerID:     "wkr_a",
			authToken:    "fresh-a",
			wantUpdated:  2,
			changed: map[string]creds{
				"sess-a1": {workerID: "wkr_a", authToken: "fresh-a"},
				"sess-a2": {workerID: "wkr_a", authToken: "fresh-a"},
			},
		},
		{
			name:         "re-registration moves its own sessions to the new identity",
			prevWorkerID: "wkr_a",
			workerID:     "wkr_a_reregistered",
			authToken:    "fresh-a",
			wantUpdated:  2,
			changed: map[string]creds{
				"sess-a1": {workerID: "wkr_a_reregistered", authToken: "fresh-a"},
				"sess-a2": {workerID: "wkr_a_reregistered", authToken: "fresh-a"},
			},
		},
		{
			name:         "the other identity refreshes independently",
			prevWorkerID: "wkr_b",
			workerID:     "wkr_b",
			authToken:    "fresh-b",
			wantUpdated:  1,
			changed: map[string]creds{
				"sess-b1": {workerID: "wkr_b", authToken: "fresh-b"},
			},
		},
		{
			name:         "an unscoped call is refused rather than fanned out",
			prevWorkerID: "",
			workerID:     "wkr_a",
			authToken:    "fresh-a",
			wantUpdated:  0,
		},
		{
			name:         "an unattributed detail is never adopted by a refresh",
			prevWorkerID: "wkr_unknown",
			workerID:     "wkr_unknown",
			authToken:    "fresh-unknown",
			wantUpdated:  0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newSessionDetailStore()
			for id, c := range seed {
				s.Set(&SessionDetail{SessionID: id, WorkerID: c.workerID, AuthToken: c.authToken})
			}

			got := s.UpdateRuntimeCredentials(tc.prevWorkerID, tc.workerID, tc.authToken)
			if got != tc.wantUpdated {
				t.Errorf("UpdateRuntimeCredentials updated %d details, want %d", got, tc.wantUpdated)
			}

			for id, base := range seed {
				want := base
				if c, ok := tc.changed[id]; ok {
					want = c
				}
				detail, ok := s.Get(id)
				if !ok {
					t.Fatalf("%s: detail vanished", id)
				}
				if detail.WorkerID != want.workerID || detail.AuthToken != want.authToken {
					t.Errorf("%s credentials = (%q, %q), want (%q, %q)",
						id, detail.WorkerID, detail.AuthToken, want.workerID, want.authToken)
				}
			}
		})
	}
}

// TestDaemonUpdateSessionRuntimeCredentials covers the exported seam an
// embedding binary drives from its own per-identity re-registration path, for
// the worker identities the daemon does not own itself.
func TestDaemonUpdateSessionRuntimeCredentials(t *testing.T) {
	t.Parallel()

	d := New(Options{})
	d.sessionDetails.Set(&SessionDetail{SessionID: "sess-own", WorkerID: "wkr_own", AuthToken: "token-own"})
	d.sessionDetails.Set(&SessionDetail{SessionID: "sess-guest", WorkerID: "wkr_guest", AuthToken: "token-guest"})

	if got := d.UpdateSessionRuntimeCredentials("wkr_guest", "wkr_guest_2", "fresh-guest"); got != 1 {
		t.Fatalf("UpdateSessionRuntimeCredentials updated %d details, want 1", got)
	}

	guest, ok := d.SessionDetail("sess-guest")
	if !ok {
		t.Fatal("guest detail vanished")
	}
	if guest.WorkerID != "wkr_guest_2" || guest.AuthToken != "fresh-guest" {
		t.Errorf("guest credentials = (%q, %q), want (wkr_guest_2, fresh-guest)",
			guest.WorkerID, guest.AuthToken)
	}

	own, ok := d.SessionDetail("sess-own")
	if !ok {
		t.Fatal("own detail vanished")
	}
	if own.WorkerID != "wkr_own" || own.AuthToken != "token-own" {
		t.Errorf("the daemon's own session was modified by another identity's refresh: (%q, %q)",
			own.WorkerID, own.AuthToken)
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

func TestDaemon_AcceptWorkWithDetail_RejectsMismatchedSessionID(t *testing.T) {
	d := newSessionDetailLifecycleDaemon(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
	})
	detail := &SessionDetail{SessionID: "detail-id", AuthToken: "must-not-store"}

	_, err := d.AcceptWorkWithDetail(SessionSpec{
		SessionID:  "spec-id",
		Repository: "github.com/a/b",
	}, detail)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("AcceptWorkWithDetail error = %v, want session-id mismatch", err)
	}
	if got := d.sessionDetails.Len(); got != 0 {
		t.Fatalf("session detail count = %d, want 0 after mismatch", got)
	}
	if got := d.spawner.ActiveCount(); got != 0 {
		t.Fatalf("active session count = %d, want 0 after mismatch", got)
	}
}

func TestDaemon_AcceptWorkWithDetail_ActiveRetryPreservesOwner(t *testing.T) {
	d := newSessionDetailLifecycleDaemon(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		WorkerCommand:         []string{"/bin/sh", "-c", "sleep 30"},
		StdoutPrefixWriter:    discardPrefixWriter{},
		StderrPrefixWriter:    discardPrefixWriter{},
	})
	t.Cleanup(func() { _ = d.spawner.Drain(time.Second) })

	spec := SessionSpec{SessionID: "duplicate-delivery", Repository: "github.com/a/b"}
	original := &SessionDetail{SessionID: spec.SessionID, AuthToken: "owner-token"}
	if _, err := d.AcceptWorkWithDetail(spec, original); err != nil {
		t.Fatalf("first accept: %v", err)
	}

	retry := &SessionDetail{SessionID: spec.SessionID, AuthToken: "retry-token"}
	if _, err := d.AcceptWorkWithDetail(spec, retry); err == nil || !strings.Contains(err.Error(), "already has an active detail") {
		t.Fatalf("retry error = %v, want active-detail rejection", err)
	}

	got, ok := d.SessionDetail(spec.SessionID)
	if !ok {
		t.Fatal("active session detail was deleted by rejected retry")
	}
	if got != original || got.AuthToken != "owner-token" {
		t.Fatalf("active session detail = %#v, want original owner detail", got)
	}
	if got := d.spawner.ActiveCount(); got != 1 {
		t.Fatalf("active session count = %d, want 1 after rejected retry", got)
	}
}

func TestDaemon_AcceptWorkWithDetail_ConcurrentSameIDHasSingleOwner(t *testing.T) {
	preSpawnEntered := make(chan struct{}, 1)
	releasePreSpawn := make(chan struct{})
	var releaseOnce sync.Once
	d := newSessionDetailLifecycleDaemon(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 8,
		WorkerCommand:         []string{"/bin/sh", "-c", "sleep 30"},
		StdoutPrefixWriter:    discardPrefixWriter{},
		StderrPrefixWriter:    discardPrefixWriter{},
		OnPreSpawn: func(_ SessionSpec, env []string) ([]string, error) {
			preSpawnEntered <- struct{}{}
			<-releasePreSpawn
			return env, nil
		},
	})
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releasePreSpawn) })
		_ = d.spawner.Drain(time.Second)
	})

	const attempts = 16
	type acceptResult struct {
		attempt int
		err     error
	}
	start := make(chan struct{})
	results := make(chan acceptResult, attempts)
	for i := 0; i < attempts; i++ {
		go func(attempt int) {
			<-start
			_, err := d.AcceptWorkWithDetail(
				SessionSpec{SessionID: "concurrent-same-id", Repository: "github.com/a/b"},
				&SessionDetail{SessionID: "concurrent-same-id", AuthToken: "attempt-" + idFor(attempt)},
			)
			results <- acceptResult{attempt: attempt, err: err}
		}(i)
	}
	close(start)

	select {
	case <-preSpawnEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("no same-id attempt reached pre-spawn")
	}
	for i := 0; i < attempts-1; i++ {
		select {
		case result := <-results:
			if result.err == nil || !strings.Contains(result.err.Error(), "already has an active detail") {
				t.Fatalf("attempt %d error = %v, want active-detail rejection", result.attempt, result.err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("received %d/%d rejected same-id attempts", i, attempts-1)
		}
	}

	releaseOnce.Do(func() { close(releasePreSpawn) })
	var owner acceptResult
	select {
	case owner = <-results:
		if owner.err != nil {
			t.Fatalf("owner attempt %d failed: %v", owner.attempt, owner.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("owning same-id attempt did not finish")
	}

	got, ok := d.SessionDetail("concurrent-same-id")
	if !ok {
		t.Fatal("owning same-id detail is missing")
	}
	wantToken := "attempt-" + idFor(owner.attempt)
	if got.AuthToken != wantToken {
		t.Fatalf("stored token = %q, want owner token %q", got.AuthToken, wantToken)
	}
	if got := d.spawner.ActiveCount(); got != 1 {
		t.Fatalf("active session count = %d, want exactly 1", got)
	}
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
