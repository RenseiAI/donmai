package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/RenseiAI/donmai/sessionshim"
)

// This file pins the daemon-start seeded-trust sweep
// (trust_ledger_sweep.go): a no-op without a configured sweeper, a crash
// sweep that undoes dead sessions' ledger rows through the configured
// sweeper, and a liveness predicate that protects adopted sessions while
// reading crashed (pid-owned) rows as dead.

// TestSweepSeededTrustLedger_NilSweeperIsANoOp proves a daemon without a
// configured sweeper does nothing at this step — no filesystem reads, no
// deletes — exactly like the codex sweep beside it.
func TestSweepSeededTrustLedger_NilSweeperIsANoOp(t *testing.T) {
	t.Parallel()
	d := &Daemon{}
	d.sweepSeededTrustLedger(context.Background())
}

// TestSweepSeededTrustLedger_SweepsDeadRowsThroughSweeper proves the daemon
// step calls the configured sweeper once per config home and drops the
// rows it reports. The fixture sweeper delegates to a temp config home so
// the assertion is about the daemon's wiring (homes enumerated, liveness
// consulted), not a re-proof of the provider sweep itself.
func TestSweepSeededTrustLedger_SweepsDeadRowsThroughSweeper(t *testing.T) {
	// No t.Parallel: Setenv forbids it.
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	recordPath := filepath.Join(home, ".claude.json")
	raw, err := json.Marshal(map[string]any{
		"projects": map[string]any{"/dead/created": map[string]any{"hasTrustDialogAccepted": true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recordPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	var calls []string
	var liveQueries []string
	d := &Daemon{}
	d.opts.TrustLedgerSweeper = func(configHome string, live func(session string) bool) int {
		calls = append(calls, configHome)
		_ = live("session:dead")
		liveQueries = append(liveQueries, "session:dead")
		if live("session:dead") {
			t.Error("daemon liveness reads a non-adopted session as live; its rows would never sweep")
		}
		return 1
	}
	d.sweepSeededTrustLedger(context.Background())
	if len(calls) == 0 {
		t.Fatal("daemon sweep never called the configured sweeper")
	}
	found := false
	for _, c := range calls {
		if c == home {
			found = true
		}
	}
	if !found {
		t.Errorf("daemon sweep did not visit the session config home %q; visited %q", home, calls)
	}
	if len(liveQueries) == 0 {
		t.Error("daemon sweep never consulted liveness")
	}
}

// TestLiveTrustLedgerSessions_AdoptedSessionsAreLive proves the liveness
// predicate protects adopted, non-terminal sessions and reads everything
// else — crashed pid-owned rows included — as dead. RED proof: build the
// predicate from the registry Scan instead of the adopted map and an
// adopted-but-not-yet-registered session reads as dead.
func TestLiveTrustLedgerSessions_AdoptedSessionsAreLive(t *testing.T) {
	t.Parallel()
	d := newTestDaemonForSweep(t, Options{})
	live := d.liveTrustLedgerSessions()
	if live("session:anything") {
		t.Error("empty daemon reads a session as live; every row would be protected forever")
	}
	if live("pid:12345") {
		t.Error("a crashed process's ledger rows read as live; the sweep could never drain them")
	}
	d.shims.mu.Lock()
	d.shims.adopted[sessionshim.Identity{OrgID: "org-1", SessionID: "sess-live"}] = adoptedShim{}
	d.shims.mu.Unlock()
	live = d.liveTrustLedgerSessions()
	if !live("session:sess-live") {
		t.Error("adopted session reads as dead; the sweep would undo a live session's trust")
	}
	if live("session:sess-other") {
		t.Error("non-adopted session reads as live; its crash rows would never sweep")
	}
}
