package hostwatch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/RenseiAI/donmai/afclient"
)

// fakeHostActions is a test HostActions: it records calls and returns
// scripted outcomes. An error return mirrors the daemon client (nil
// response with the error); ok=false with a message mirrors a 200
// refusal.
type fakeHostActions struct {
	mu sync.Mutex

	pauseCalls, resumeCalls, drainCalls int
	stopCalls                           []string
	drainTimeouts                       []int
	drainDeadlineSet                    bool
	drainDeadlineRemain                 time.Duration

	pauseErr, resumeErr, drainErr, stopErr error
	pauseMsg, resumeMsg, drainMsg, stopMsg string
	pauseOK, resumeOK, drainOK, stopOK     bool
}

func newFakeHostActions() *fakeHostActions {
	return &fakeHostActions{
		pauseMsg: "paused", resumeMsg: "resumed", drainMsg: "drained", stopMsg: "stopped",
		pauseOK: true, resumeOK: true, drainOK: true, stopOK: true,
	}
}

func (f *fakeHostActions) PauseContext(context.Context) (*afclient.DaemonActionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pauseCalls++
	if f.pauseErr != nil {
		return nil, f.pauseErr
	}
	return &afclient.DaemonActionResponse{OK: f.pauseOK, Message: f.pauseMsg}, nil
}

func (f *fakeHostActions) ResumeContext(context.Context) (*afclient.DaemonActionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumeCalls++
	if f.resumeErr != nil {
		return nil, f.resumeErr
	}
	return &afclient.DaemonActionResponse{OK: f.resumeOK, Message: f.resumeMsg}, nil
}

func (f *fakeHostActions) DrainContext(ctx context.Context, timeoutSeconds int) (*afclient.DaemonActionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drainCalls++
	f.drainTimeouts = append(f.drainTimeouts, timeoutSeconds)
	if dl, ok := ctx.Deadline(); ok {
		f.drainDeadlineSet = true
		f.drainDeadlineRemain = time.Until(dl)
	}
	if f.drainErr != nil {
		return nil, f.drainErr
	}
	return &afclient.DaemonActionResponse{OK: f.drainOK, Message: f.drainMsg}, nil
}

func (f *fakeHostActions) StopSessionContext(_ context.Context, sessionID string) (*afclient.DaemonActionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCalls = append(f.stopCalls, sessionID)
	if f.stopErr != nil {
		return nil, f.stopErr
	}
	return &afclient.DaemonActionResponse{OK: f.stopOK, Message: f.stopMsg}, nil
}

func (f *fakeHostActions) counts() (pauses, resumes, drains int, stops []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pauseCalls, f.resumeCalls, f.drainCalls, append([]string(nil), f.stopCalls...)
}

// drainObservation reports the last drain call's timeout, whether its
// context carried a deadline, and the remaining time on that deadline.
func (f *fakeHostActions) drainObservation() (calls int, timeouts []int, hasDeadline bool, remain time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.drainCalls, append([]int(nil), f.drainTimeouts...), f.drainDeadlineSet, f.drainDeadlineRemain
}

// newActionTestModel builds a plain model over a fake daemon index plus a
// fake action surface, sized for rendering with one applied snapshot.
func newActionTestModel(t *testing.T, fd *fakeDaemon, acts HostActions, drainSecs int) *Model {
	t.Helper()
	src := NewSource(fd, nil, "")
	m := New(Options{
		Source:              src,
		Plain:               true,
		IndexInterval:       time.Millisecond,
		TailInterval:        time.Millisecond,
		Now:                 func() time.Time { return time.Date(2026, 6, 13, 14, 2, 11, 0, time.UTC) },
		Actions:             acts,
		DrainTimeoutSeconds: drainSecs,
	})
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	model := mm.(*Model)
	model.applySnapshot(model.src.Snapshot())
	return model
}

func actionTestSessions(ids ...string) *fakeDaemon {
	var handles []afclient.DaemonSessionHandle
	for _, id := range ids {
		handles = append(handles, afclient.DaemonSessionHandle{SessionID: id, State: "running", Repository: "o/a"})
	}
	return &fakeDaemon{sessions: handles, status: &afclient.DaemonStatusResponse{MaxSessions: 8}}
}

// pressActionKey drives one key through the model like a terminal would.
func pressActionKey(m *Model, key string) (tea.Model, tea.Cmd) {
	var msg tea.KeyPressMsg
	switch key {
	case "esc":
		msg = tea.KeyPressMsg{Code: tea.KeyEsc}
	case "up":
		msg = tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		msg = tea.KeyPressMsg{Code: tea.KeyDown}
	default:
		msg = tea.KeyPressMsg{Code: rune(key[0]), Text: key}
	}
	return m.Update(msg)
}

// runActionCmd executes an action command synchronously and feeds its
// result back through the model, returning the follow-up command (the
// fresh index poll). Callers run and apply that poll when they want the
// post-action snapshot.
func runActionCmd(t *testing.T, m *Model, cmd tea.Cmd) tea.Cmd {
	t.Helper()
	if cmd == nil {
		t.Fatal("want an action command, got nil")
	}
	_, follow := m.Update(cmd())
	return follow
}

// TestModel_StopRequiresExplicitConfirm pins the AC-1 gate: the stop key
// only arms confirmation for the selected session; cancel (n), esc and a
// selection change send zero requests; y sends exactly one request for
// the still-selected id. Bypassing the gate (request on x) makes this RED.
func TestModel_StopRequiresExplicitConfirm(t *testing.T) {
	t.Run("confirm sends exactly one request for the selected session", func(t *testing.T) {
		acts := newFakeHostActions()
		m := newActionTestModel(t, actionTestSessions("sess-a", "sess-b"), acts, 0)

		if _, cmd := pressActionKey(m, "x"); cmd != nil {
			t.Fatalf("stop key must arm confirmation without a request, got a command")
		}
		if m.confirmStopID != "sess-a" {
			t.Fatalf("confirmStopID = %q, want the selected sess-a", m.confirmStopID)
		}
		if _, _, _, stops := acts.counts(); len(stops) != 0 {
			t.Fatalf("armed confirmation sent %d requests, want 0", len(stops))
		}

		_, cmd := pressActionKey(m, "y")
		follow := runActionCmd(t, m, cmd)
		if _, _, _, stops := acts.counts(); len(stops) != 1 || stops[0] != "sess-a" {
			t.Fatalf("confirm sent stops = %v, want exactly [sess-a]", stops)
		}
		if m.confirmStopID != "" {
			t.Fatalf("confirmStopID = %q after confirm, want idle", m.confirmStopID)
		}
		if follow == nil {
			t.Fatal("an accepted stop must return a fresh index poll")
		}
		if !strings.Contains(m.notice, "stopped") || !strings.Contains(m.notice, "sess-a") {
			t.Errorf("notice = %q, want the stop outcome for sess-a", m.notice)
		}
	})

	t.Run("cancel sends no request", func(t *testing.T) {
		for _, key := range []string{"n", "esc"} {
			acts := newFakeHostActions()
			m := newActionTestModel(t, actionTestSessions("sess-a", "sess-b"), acts, 0)
			if _, cmd := pressActionKey(m, "x"); cmd != nil {
				t.Fatalf("%s: stop key must not dispatch", key)
			}
			if _, cmd := pressActionKey(m, key); cmd != nil {
				t.Fatalf("%s: cancel must not dispatch", key)
			}
			if m.confirmStopID != "" {
				t.Fatalf("%s: confirmStopID = %q, want idle", key, m.confirmStopID)
			}
			if _, _, _, stops := acts.counts(); len(stops) != 0 {
				t.Fatalf("%s: cancel sent %d requests, want 0", key, len(stops))
			}
		}
	})

	t.Run("up cancels without a request", func(t *testing.T) {
		acts := newFakeHostActions()
		m := newActionTestModel(t, actionTestSessions("sess-a", "sess-b"), acts, 0)
		if _, cmd := pressActionKey(m, "x"); cmd != nil {
			t.Fatal("stop key must not dispatch")
		}
		// Cursor is already at the top: the key still moves nothing but
		// must clear the gate. Without the up-clear, the y below would
		// stop sess-a while nothing visibly changed.
		if _, cmd := pressActionKey(m, "up"); cmd != nil {
			t.Fatal("selection change must not dispatch")
		}
		if m.confirmKind != "" || m.confirmStopID != "" {
			t.Fatalf("confirmKind = %q confirmStopID = %q after up, want idle", m.confirmKind, m.confirmStopID)
		}
		if _, cmd := pressActionKey(m, "y"); cmd != nil {
			t.Fatal("idle y must not dispatch")
		}
		if _, _, _, stops := acts.counts(); len(stops) != 0 {
			t.Fatalf("up then y sent %d requests, want 0", len(stops))
		}
	})

	t.Run("selection change cancels without a request", func(t *testing.T) {
		acts := newFakeHostActions()
		m := newActionTestModel(t, actionTestSessions("sess-a", "sess-b"), acts, 0)
		if _, cmd := pressActionKey(m, "x"); cmd != nil {
			t.Fatal("stop key must not dispatch")
		}
		if _, cmd := pressActionKey(m, "down"); cmd != nil {
			t.Fatal("selection change must not dispatch")
		}
		if m.confirmStopID != "" {
			t.Fatalf("confirmStopID = %q after selection change, want idle", m.confirmStopID)
		}
		if m.cursor != 1 {
			t.Fatalf("cursor = %d, want the moved selection 1", m.cursor)
		}
		if _, cmd := pressActionKey(m, "y"); cmd != nil {
			t.Fatal("idle y must not dispatch")
		}
		if _, _, _, stops := acts.counts(); len(stops) != 0 {
			t.Fatalf("selection change sent %d requests, want 0", len(stops))
		}
	})

	t.Run("snapshot removing the confirmed session cancels the confirm", func(t *testing.T) {
		fd := actionTestSessions("sess-a", "sess-b")
		acts := newFakeHostActions()
		m := newActionTestModel(t, fd, acts, 0)
		if _, cmd := pressActionKey(m, "x"); cmd != nil {
			t.Fatal("stop key must not dispatch")
		}
		// The daemon reaps sess-a before the operator confirms: the
		// cursor now selects sess-b, so y must send nothing. Without
		// the still-selected re-check, y would stop the reaped id.
		fd.sessions = fd.sessions[1:]
		m.applySnapshot(m.src.Snapshot())
		if m.selectedCardID() != "sess-b" {
			t.Fatalf("selectedCardID = %q after reap, want sess-b", m.selectedCardID())
		}
		if _, cmd := pressActionKey(m, "y"); cmd != nil {
			t.Fatal("y after the confirmed session vanished must not dispatch")
		}
		if m.confirmKind != "" {
			t.Fatalf("confirmKind = %q after stale confirm, want idle", m.confirmKind)
		}
		if _, _, _, stops := acts.counts(); len(stops) != 0 {
			t.Fatalf("stale confirm sent %d requests, want 0", len(stops))
		}
	})

	t.Run("other lifecycle keys are swallowed with the gate still armed", func(t *testing.T) {
		acts := newFakeHostActions()
		m := newActionTestModel(t, actionTestSessions("sess-a", "sess-b"), acts, 0)
		if _, cmd := pressActionKey(m, "x"); cmd != nil {
			t.Fatal("stop key must not dispatch")
		}
		// p/r/d must neither dispatch nor resolve the gate: falling
		// through would pause the host, resume admissions, or drain
		// every session from inside a stop confirmation.
		for _, key := range []string{"p", "r", "d"} {
			if _, cmd := pressActionKey(m, key); cmd != nil {
				t.Fatalf("%s during stop confirm must not dispatch", key)
			}
			if m.confirmKind != "stop" || m.confirmStopID != "sess-a" {
				t.Fatalf("%s resolved the gate: confirmKind = %q confirmStopID = %q, want stop/sess-a", key, m.confirmKind, m.confirmStopID)
			}
		}
		if pauses, resumes, drains, stops := acts.counts(); pauses != 0 || resumes != 0 || drains != 0 || len(stops) != 0 {
			t.Fatalf("swallowed keys dispatched: pauses=%d resumes=%d drains=%d stops=%v", pauses, resumes, drains, stops)
		}
		// The still-armed gate confirms the original session.
		_, cmd := pressActionKey(m, "y")
		runActionCmd(t, m, cmd)
		if _, _, _, stops := acts.counts(); len(stops) != 1 || stops[0] != "sess-a" {
			t.Fatalf("confirm sent stops = %v, want exactly [sess-a]", stops)
		}
	})

	t.Run("duplicate keys while in flight are ignored", func(t *testing.T) {
		acts := newFakeHostActions()
		m := newActionTestModel(t, actionTestSessions("sess-a"), acts, 0)
		if _, cmd := pressActionKey(m, "x"); cmd != nil {
			t.Fatal("stop key must not dispatch")
		}
		_, stopCmd := pressActionKey(m, "y")
		if stopCmd == nil {
			t.Fatal("confirm must dispatch one stop")
		}
		// While the stop is in flight every further action key is dropped.
		// The confirmed command itself is still pending (lazy tea.Cmd).
		for _, key := range []string{"x", "p", "r", "d", "y"} {
			if _, cmd := pressActionKey(m, key); cmd != nil {
				t.Fatalf("%s while in flight must be ignored, got a command", key)
			}
		}
		if pauses, resumes, drains, stops := acts.counts(); pauses != 0 || resumes != 0 || drains != 0 || len(stops) != 0 {
			t.Fatalf("in-flight keys dispatched: pauses=%d resumes=%d drains=%d stops=%v", pauses, resumes, drains, stops)
		}
		// Completing the pending command sends the single confirmed request.
		runActionCmd(t, m, stopCmd)
		if _, _, _, stops := acts.counts(); len(stops) != 1 || stops[0] != "sess-a" {
			t.Fatalf("confirmed stops = %v, want exactly [sess-a]", stops)
		}
	})
}

// TestModel_StopAckKeepsCardUntilReap pins the AC-2 stop half: a 200 only
// acknowledges the owned generation, so the card stays until a later
// snapshot omits it. Deleting the card in the result handler makes this RED.
func TestModel_StopAckKeepsCardUntilReap(t *testing.T) {
	fd := actionTestSessions("sess-a", "sess-b")
	acts := newFakeHostActions()
	m := newActionTestModel(t, fd, acts, 0)

	if _, cmd := pressActionKey(m, "x"); cmd != nil {
		t.Fatal("stop key must not dispatch")
	}
	_, stopCmd := pressActionKey(m, "y")
	if stopCmd == nil {
		t.Fatal("confirm must dispatch one stop")
	}
	// The result handler only reports and repolls: the acknowledged card
	// must still be present before the follow-up snapshot even runs.
	_, follow := m.Update(stopCmd())
	if follow == nil {
		t.Fatal("an accepted stop must return a fresh index poll")
	}
	if findCard(m, "sess-a") == nil {
		t.Fatal("result handler removed the acknowledged card instead of reporting")
	}
	// The next snapshot still lists the session: the card must remain.
	if msg := follow(); msg == nil {
		t.Fatal("poll must produce a message")
	} else {
		m.Update(msg)
	}
	if findCard(m, "sess-a") == nil {
		t.Fatal("acknowledged stop removed the card before the reap snapshot")
	}
	// A later snapshot omits it (terminal reap): only then does it go.
	fd.sessions = fd.sessions[1:]
	m.applySnapshot(m.src.Snapshot())
	if findCard(m, "sess-a") != nil {
		t.Fatal("reaped session still has a card after the omitting snapshot")
	}
	if findCard(m, "sess-b") == nil {
		t.Fatal("unrelated card vanished with the reap")
	}
}

// TestModel_PauseResumeChangeStateOnlyViaSnapshot pins the AC-3 half:
// pause and resume each send one authenticated request and change visible
// lifecycle state only through the next snapshot. Writing counters or
// cards from the response makes this RED.
func TestModel_PauseResumeChangeStateOnlyViaSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name       string
		key        string
		wantNotice string
		countCalls func(*fakeHostActions) int
	}{
		{"pause", "p", "paused-marker", func(a *fakeHostActions) int {
			p, _, _, _ := a.counts()
			return p
		}},
		{"resume", "r", "resumed-marker", func(a *fakeHostActions) int {
			_, r, _, _ := a.counts()
			return r
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acts := newFakeHostActions()
			acts.pauseMsg, acts.resumeMsg = "paused-marker", "resumed-marker"
			m := newActionTestModel(t, actionTestSessions("sess-a"), acts, 0)
			beforeCards := fmt.Sprintf("%+v", m.cards)
			beforeCounters := m.counters

			_, cmd := pressActionKey(m, tc.key)
			if cmd == nil {
				t.Fatalf("%s key must dispatch", tc.name)
			}
			// The result handler only reports and repolls: visible state
			// must be untouched before the follow-up snapshot even runs.
			_, follow := m.Update(cmd())
			if got := tc.countCalls(acts); got != 1 {
				t.Fatalf("%s dispatched %d requests, want exactly 1", tc.name, got)
			}
			if follow == nil {
				t.Fatalf("an accepted %s must return a fresh index poll", tc.name)
			}
			if got := fmt.Sprintf("%+v", m.cards); got != beforeCards {
				t.Errorf("%s rewrote cards from the response:\nbefore %s\nafter  %s", tc.name, beforeCards, got)
			}
			if m.counters != beforeCounters {
				t.Errorf("%s rewrote counters from the response: %+v -> %+v", tc.name, beforeCounters, m.counters)
			}
			// The response is reported, never folded into cards/counters.
			if !strings.Contains(m.notice, tc.wantNotice) {
				t.Errorf("notice = %q, want the %s outcome", m.notice, tc.name)
			}
			// The next snapshot owns visible state: with a stable daemon
			// it changes nothing either.
			m.Update(follow())
			if got := fmt.Sprintf("%+v", m.cards); got != beforeCards {
				t.Errorf("%s poll changed cards without a snapshot change", tc.name)
			}
			if m.counters != beforeCounters {
				t.Errorf("%s poll changed counters without a snapshot change: %+v -> %+v", tc.name, beforeCounters, m.counters)
			}
		})
	}
}

// TestModel_DrainRequiresExplicitConfirm pins the host-scope gate: one d
// keypress only arms confirmation with a prompt naming the effect
// (sessions still running past the deadline are stopped); n/esc and a
// selection change send zero requests; y sends exactly one
// deadline-bound request. Dispatching the drain on d makes this RED.
func TestModel_DrainRequiresExplicitConfirm(t *testing.T) {
	t.Run("confirm sends exactly one deadline-bound request", func(t *testing.T) {
		acts := newFakeHostActions()
		m := newActionTestModel(t, actionTestSessions("sess-a", "sess-b"), acts, 0)

		if _, cmd := pressActionKey(m, "d"); cmd != nil {
			t.Fatal("drain key must arm confirmation without a request, got a command")
		}
		if m.confirmKind != "drain" {
			t.Fatalf("confirmKind = %q, want drain", m.confirmKind)
		}
		if _, _, drains, _ := acts.counts(); drains != 0 {
			t.Fatalf("armed confirmation sent %d requests, want 0", drains)
		}
		// The prompt names the effect: one keypress stops every session.
		if help := m.renderHelp(); !strings.Contains(help, "drain host") || !strings.Contains(help, "stopped") {
			t.Fatalf("drain prompt = %q, want the host effect named", help)
		}

		_, cmd := pressActionKey(m, "y")
		runActionCmd(t, m, cmd)
		calls, timeouts, hasDeadline, _ := acts.drainObservation()
		if calls != 1 || len(timeouts) != 1 || timeouts[0] != DefaultDrainTimeoutSeconds {
			t.Fatalf("confirm sent drains = %d timeouts = %v, want 1x[%d]", calls, timeouts, DefaultDrainTimeoutSeconds)
		}
		if !hasDeadline {
			t.Fatal("confirmed drain context carries no deadline")
		}
		if m.confirmKind != "" {
			t.Fatalf("confirmKind = %q after confirm, want idle", m.confirmKind)
		}
		if !strings.Contains(m.notice, "drained") {
			t.Errorf("notice = %q, want the drain outcome", m.notice)
		}
	})

	t.Run("cancel and selection change send no request", func(t *testing.T) {
		for _, key := range []string{"n", "esc", "up", "down"} {
			acts := newFakeHostActions()
			m := newActionTestModel(t, actionTestSessions("sess-a", "sess-b"), acts, 0)
			if _, cmd := pressActionKey(m, "d"); cmd != nil {
				t.Fatalf("%s: drain key must not dispatch", key)
			}
			if _, cmd := pressActionKey(m, key); cmd != nil {
				t.Fatalf("%s: must not dispatch", key)
			}
			if m.confirmKind != "" {
				t.Fatalf("%s: confirmKind = %q, want idle", key, m.confirmKind)
			}
			if _, _, drains, _ := acts.counts(); drains != 0 {
				t.Fatalf("%s: sent %d requests, want 0", key, drains)
			}
			// The gate is idle: a stray y dispatches nothing either.
			if _, cmd := pressActionKey(m, "y"); cmd != nil {
				t.Fatalf("%s: idle y must not dispatch", key)
			}
			if _, _, drains, _ := acts.counts(); drains != 0 {
				t.Fatalf("%s: idle y sent %d requests, want 0", key, drains)
			}
		}
	})

	t.Run("other lifecycle keys are swallowed with the gate still armed", func(t *testing.T) {
		acts := newFakeHostActions()
		m := newActionTestModel(t, actionTestSessions("sess-a", "sess-b"), acts, 0)
		if _, cmd := pressActionKey(m, "d"); cmd != nil {
			t.Fatal("drain key must not dispatch")
		}
		// x/p/r must neither dispatch nor resolve the drain gate: a
		// fall-through would stop a session or flip admissions from
		// inside a host-drain confirmation.
		for _, key := range []string{"x", "p", "r"} {
			if _, cmd := pressActionKey(m, key); cmd != nil {
				t.Fatalf("%s during drain confirm must not dispatch", key)
			}
			if m.confirmKind != "drain" {
				t.Fatalf("%s resolved the gate: confirmKind = %q, want drain", key, m.confirmKind)
			}
		}
		if pauses, resumes, drains, stops := acts.counts(); pauses != 0 || resumes != 0 || drains != 0 || len(stops) != 0 {
			t.Fatalf("swallowed keys dispatched: pauses=%d resumes=%d drains=%d stops=%v", pauses, resumes, drains, stops)
		}
		// The still-armed gate confirms the drain.
		_, cmd := pressActionKey(m, "y")
		runActionCmd(t, m, cmd)
		if _, _, drains, _ := acts.counts(); drains != 1 {
			t.Fatalf("confirm sent drains = %d, want 1", drains)
		}
	})
}

// TestModel_DrainIsDeadlineBound pins the drain door: the explicit timeout
// travels in the request and bounds the command context, and timeout or
// refusal renders without success wording. Dropping deadline propagation
// makes this RED.
func TestModel_DrainIsDeadlineBound(t *testing.T) {
	t.Run("explicit deadline in request and context", func(t *testing.T) {
		for _, secs := range []int{0, 7} {
			acts := newFakeHostActions()
			m := newActionTestModel(t, actionTestSessions("sess-a"), acts, secs)
			if _, cmd := pressActionKey(m, "d"); cmd != nil {
				t.Fatal("drain key must arm confirmation without a request")
			}
			_, cmd := pressActionKey(m, "y")
			runActionCmd(t, m, cmd)
			calls, timeouts, hasDeadline, remain := acts.drainObservation()
			want := secs
			if want <= 0 {
				want = DefaultDrainTimeoutSeconds
			}
			if calls != 1 {
				t.Fatalf("secs=%d: drains = %d, want 1", secs, calls)
			}
			if len(timeouts) != 1 || timeouts[0] != want {
				t.Fatalf("secs=%d: body timeouts = %v, want [%d]", secs, timeouts, want)
			}
			if !hasDeadline {
				t.Fatalf("secs=%d: drain context carries no deadline", secs)
			}
			if remain <= 0 || remain > time.Duration(want)*time.Second {
				t.Fatalf("secs=%d: context deadline in %v, want within (0,%ds]", secs, remain, want)
			}
		}
	})

	t.Run("timeout and refusal never claim success", func(t *testing.T) {
		timeoutErr := fmt.Errorf("daemon drain: request failed: %w", context.DeadlineExceeded)
		for _, tc := range []struct {
			name         string
			drainErr     error
			drainOK      bool
			drainMsg     string
			wantFragment string
		}{
			{"timeout", timeoutErr, false, "", "timed out"},
			{"refusal", nil, false, "drain incomplete: 2 sessions active", "refused"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				acts := newFakeHostActions()
				acts.drainErr, acts.drainOK, acts.drainMsg = tc.drainErr, tc.drainOK, tc.drainMsg
				m := newActionTestModel(t, actionTestSessions("sess-a"), acts, 0)
				if _, cmd := pressActionKey(m, "d"); cmd != nil {
					t.Fatal("drain key must arm confirmation without a request")
				}
				_, cmd := pressActionKey(m, "y")
				runActionCmd(t, m, cmd)
				if !strings.Contains(m.notice, tc.wantFragment) {
					t.Errorf("notice = %q, want %q", m.notice, tc.wantFragment)
				}
				for _, success := range []string{"drained", "success", "accepted", " ok"} {
					if strings.Contains(m.notice, success) {
						t.Errorf("notice = %q claims success with %q", m.notice, success)
					}
				}
			})
		}
	})
}

// TestModel_ActionErrorsAreBoundedAndSecretFree pins AC-5: unauthorized
// and unavailable outcomes are visible and bounded, and bearer material
// or header names never render. Rendering the raw credential-bearing
// fixture makes this RED.
func TestModel_ActionErrorsAreBoundedAndSecretFree(t *testing.T) {
	const leaked = "hunter2-token"
	for _, tc := range []struct {
		name     string
		key      string
		err      error
		kindWord string
	}{
		{"pause unauthorized", "p", fmt.Errorf("daemon pause: /api/daemon/pause: %w: detail Bearer "+leaked+" Authorization: Bearer "+leaked, afclient.ErrUnauthorized), "unauthorized"},
		{"resume unauthorized", "r", fmt.Errorf("daemon resume: /api/daemon/resume: %w: Bearer "+leaked, afclient.ErrNotAuthenticated), "unauthorized"},
		{"drain unavailable", "d", fmt.Errorf("daemon drain: /api/daemon/drain: %w: capacity Bearer "+leaked, afclient.ErrUnavailable), "unavailable"},
		{"stop server error", "y", fmt.Errorf("daemon stop session: /api/daemon/sessions/x/stop: %w: Bearer "+leaked, afclient.ErrServerError), "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acts := newFakeHostActions()
			acts.pauseErr, acts.resumeErr, acts.drainErr, acts.stopErr = tc.err, tc.err, tc.err, tc.err
			m := newActionTestModel(t, actionTestSessions("sess-a"), acts, 0)
			var cmd tea.Cmd
			if tc.key == "y" {
				if _, c := pressActionKey(m, "x"); c != nil {
					t.Fatal("stop key must not dispatch")
				}
				_, cmd = pressActionKey(m, "y")
			} else if tc.key == "d" {
				if _, c := pressActionKey(m, "d"); c != nil {
					t.Fatal("drain key must arm confirmation without a request")
				}
				_, cmd = pressActionKey(m, "y")
			} else {
				_, cmd = pressActionKey(m, tc.key)
			}
			runActionCmd(t, m, cmd)
			if !strings.Contains(m.notice, tc.kindWord) {
				t.Errorf("notice = %q, want the %q classification", m.notice, tc.kindWord)
			}
			if strings.Contains(m.notice, leaked) {
				t.Errorf("notice = %q leaks the bearer value", m.notice)
			}
			assertNoBareBearer(t, m.notice)
			// Bounded: the classification prefix plus one sanitized line.
			prefix := len([]rune(m.notice)) - len([]rune(sanitizeHostActionText(tc.err.Error())))
			_ = prefix
			if got := len([]rune(m.notice)); got > maxHostActionNoticeRunes+32 {
				t.Errorf("notice is %d runes, want at most %d", got, maxHostActionNoticeRunes+32)
			}
		})
	}
}

// assertNoBareBearer fails when the notice still carries a bearer token:
// every "bearer " span must be followed by the redaction placeholder, so a
// leaked header value can never survive sanitizing.
func assertNoBareBearer(t *testing.T, notice string) {
	t.Helper()
	lower := strings.ToLower(notice)
	for i := 0; i < len(lower); {
		idx := strings.Index(lower[i:], "bearer ")
		if idx < 0 {
			return
		}
		abs := i + idx + len("bearer ")
		if rest := notice[abs:]; !strings.HasPrefix(rest, "[redacted]") {
			t.Errorf("notice = %q carries an unredacted bearer span", notice)
			return
		}
		i = abs
	}
}

// TestRedactBearerMaterial pins the sanitizer the error test relies on.
func TestRedactBearerMaterial(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Bearer abc123 failed", "Bearer [redacted] failed"},
		{"bearer abc123 failed", "Bearer [redacted] failed"},
		{`Authorization: Bearer abc123`, `Authorization: Bearer [redacted]`},
		{"no credentials here", "no credentials here"},
		{"Bearer ", "Bearer "},
	} {
		if got := redactBearerMaterial(tc.in); got != tc.want {
			t.Errorf("redactBearerMaterial(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestModel_ProductionClientDispatchesActions pins AC-6 at the model seam:
// the real daemon client against an owned loopback daemon sends one
// authenticated request per action. Removing the wiring (nil Actions)
// sends nothing, which makes this RED.
func TestModel_ProductionClientDispatchesActions(t *testing.T) {
	const token = "hostwatch-action-token"
	var (
		mu    sync.Mutex
		calls = map[string]int{}
		auths = map[string]string{}
	)
	var drainTimeout int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls[r.Method+" "+r.URL.EscapedPath()]++
		auths[r.URL.EscapedPath()] = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/daemon/sessions":
			_, _ = fmt.Fprint(w, `[{"sessionId":"sess-live-1","pid":42,"state":"running","repository":"o/a"}]`)
		case r.URL.Path == "/api/daemon/status":
			_, _ = fmt.Fprint(w, `{"status":"running","version":"t","maxSessions":4}`)
		case r.URL.Path == "/api/daemon/stats":
			_, _ = fmt.Fprint(w, `{"queueDepth":0}`)
		case r.URL.Path == "/api/daemon/pause":
			_, _ = fmt.Fprint(w, `{"ok":true,"message":"paused"}`)
		case r.URL.Path == "/api/daemon/drain":
			var req afclient.DaemonDrainRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			drainTimeout = req.TimeoutSeconds
			mu.Unlock()
			_, _ = fmt.Fprint(w, `{"ok":true,"message":"drained"}`)
		case r.URL.Path == "/api/daemon/sessions/sess-live-1/stop":
			_, _ = fmt.Fprint(w, `{"ok":true,"message":"stopped"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	client := afclient.NewDaemonClientFromURL(srv.URL)
	client.SetControlToken(token)
	var _ HostActions = client

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	m := New(Options{
		Source:        NewSource(client, nil, ""),
		Plain:         true,
		IndexInterval: time.Millisecond,
		TailInterval:  time.Millisecond,
		Now:           func() time.Time { return time.Date(2026, 6, 13, 14, 2, 11, 0, time.UTC) },
		Actions:       client,
		ActionContext: ctx,
		// One-second drain keeps the deadline-bound door fast here.
		DrainTimeoutSeconds: 1,
	})
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = mm.(*Model)
	m.applySnapshot(m.src.Snapshot())
	if len(m.cards) != 1 || m.cards[0].SessionID != "sess-live-1" {
		t.Fatalf("owned snapshot cards = %+v, want [sess-live-1]", m.cards)
	}

	// Pause: one authenticated request, outcome reported, poll follows.
	_, pauseCmd := m.Update(keyMsg("down")) // cursor clamp on one card; exercises nav
	_ = pauseCmd
	_, pauseCmd = m.Update(actionKey("p"))
	pauseFollow := runActionCmd(t, m, pauseCmd)
	if pauseFollow == nil {
		t.Fatal("accepted pause must return a fresh index poll")
	}
	m.Update(pauseFollow())

	// Stop: confirm gate then exactly one authenticated request.
	if _, cmd := m.Update(actionKey("x")); cmd != nil {
		t.Fatal("stop key must not dispatch")
	}
	_, stopCmd := m.Update(actionKey("y"))
	stopFollow := runActionCmd(t, m, stopCmd)
	if stopFollow == nil {
		t.Fatal("accepted stop must return a fresh index poll")
	}

	// Drain: the confirm gate arms on d, then the explicit deadline
	// travels in the JSON body on y.
	if _, cmd := m.Update(actionKey("d")); cmd != nil {
		t.Fatal("drain key must arm confirmation without a request")
	}
	_, drainCmd := m.Update(actionKey("y"))
	runActionCmd(t, m, drainCmd)

	mu.Lock()
	for path, want := range map[string]int{
		"POST /api/daemon/pause":                     1,
		"POST /api/daemon/sessions/sess-live-1/stop": 1,
		"POST /api/daemon/drain":                     1,
	} {
		if calls[path] != want {
			t.Errorf("owned daemon %s calls = %d, want %d (all calls: %v)", path, calls[path], want, calls)
		}
	}
	for _, path := range []string{"/api/daemon/pause", "/api/daemon/sessions/sess-live-1/stop", "/api/daemon/drain"} {
		if auths[path] != "Bearer "+token {
			t.Errorf("%s Authorization = %q, want the operator bearer", path, auths[path])
		}
	}
	if drainTimeout != 1 {
		t.Errorf("drain body timeoutSeconds = %d, want the explicit 1", drainTimeout)
	}
	totalCalls := len(calls)
	mu.Unlock()

	// Without the wiring the same keys send nothing.
	unwired := New(Options{
		Source: NewSource(client, nil, ""),
		Plain:  true,
		Now:    func() time.Time { return time.Date(2026, 6, 13, 14, 2, 11, 0, time.UTC) },
	})
	um, _ := unwired.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	unwired = um.(*Model)
	unwired.applySnapshot(unwired.src.Snapshot())
	for _, key := range []string{"p", "r", "d", "x", "y"} {
		if _, cmd := unwired.Update(actionKey(key)); cmd != nil {
			t.Fatalf("unwired key %q must not dispatch", key)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != totalCalls {
		t.Fatalf("unwired model sent requests: %v", calls)
	}
}

// actionKey builds a printable key press for lifecycle tests.
func actionKey(s string) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: rune(s[0]), Text: s}
}
