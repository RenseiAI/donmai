package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	providerclaude "github.com/RenseiAI/donmai/provider/harness/claude"
	"github.com/RenseiAI/donmai/sessionshim"
)

// This file pins the daemon-start seeded-trust sweep
// (trust_ledger_sweep.go): a no-op without a configured sweeper, a crash
// sweep that undoes dead sessions' ledger rows through the configured
// sweeper, a liveness predicate that protects adopted sessions while
// reading crashed (pid-owned) rows as dead, and ledger-driven discovery of
// session-scoped override homes the process enumeration never names.

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

// TestSweepSeededTrustLedger_DrainsSessionScopedOverrideHomes drives the
// PRODUCTION sweeper through the daemon step: a crashed session seeded its
// trust under a session-scoped CLAUDE_CONFIG_DIR override the daemon
// process itself never ran with, so the process-home enumeration never
// names that home. The ledger row records the home its seed wrote beside,
// and the sweep follows it — a crash there still drains. RED proof: sweep
// only the process homes (skip the ledger-driven discovery) and the
// override home keeps its seeded entry.
func TestSweepSeededTrustLedger_DrainsSessionScopedOverrideHomes(t *testing.T) {
	// No t.Parallel: Setenv forbids it.
	processHome := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", processHome)
	// The process-home enumeration also visits the operator home; point
	// it at the scratch dir so the sweep never reads the operator's real
	// trust record.
	t.Setenv("HOME", processHome)
	overrideHome := t.TempDir()

	// Land a crashed session's seed + ledger under the override home by
	// hand: seedWorkspaceTrust reads the session env layer first, so a
	// session-scoped override writes exactly here while the daemon
	// process home stays empty. The process home ALSO carries a ledger
	// whose row names the override home — that is the discovery link the
	// sweep follows (a crashed isolated session's own home is never
	// enumerated directly).
	overrideRecord := filepath.Join(overrideHome, providerclaude.TrustRecordFileName)
	raw, err := json.Marshal(map[string]any{
		"projects": map[string]any{"/dead/created": map[string]any{"hasTrustDialogAccepted": true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overrideRecord, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ledgerRaw, err := json.Marshal([]map[string]any{{
		"dir":        "/dead/created",
		"created":    true,
		"session":    "session:uuid-dead",
		"configHome": overrideHome,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(overrideHome, ".claude.json.seeded-trust-ledger"), ledgerRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	// The discovery link: the process home's own ledger names the
	// override home in its rows. The link row's own sweep drains
	// independently; the override home is discovered BEFORE the naming
	// home sweeps, so the link row may name a live session and still
	// route the sweep to the isolated home.
	linkRaw, err := json.Marshal([]map[string]any{{
		"dir":        "/dead/link",
		"created":    true,
		"session":    "session:uuid-dead",
		"configHome": overrideHome,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(processHome, ".claude.json"), []byte(`{"projects":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(processHome, ".claude.json.seeded-trust-ledger"), linkRaw, 0o600); err != nil {
		t.Fatal(err)
	}

	d := newTestDaemonForSweep(t, Options{})
	d.opts.TrustLedgerSweeper = providerclaude.SweepSeededTrustForConfigHome
	d.sweepSeededTrustLedger(context.Background())

	raw, err = os.ReadFile(overrideRecord)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	projects, _ := record["projects"].(map[string]any)
	if _, present := projects["/dead/created"]; present {
		t.Errorf("crashed session's override-home entry survives the daemon sweep: %v", projects)
	}
}

// predicate protects adopted, non-terminal sessions and reads everything
// else — crashed pid-owned rows included — as dead. The adopted identity
// carries a lifecycle UUID while the display name differs on purpose: the
// sweep keys liveness on the UUID the ledger rows carry, never on the
// user-facing name, so a same-string fixture would mask a namespace
// mismatch. RED proof: build the predicate from the registry Scan instead
// of the adopted map and an adopted-but-not-yet-registered session reads
// as dead.
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
	d.shims.adopted[sessionshim.Identity{OrgID: "org-1", SessionID: "uuid-1"}] = adoptedShim{}
	d.shims.mu.Unlock()
	live = d.liveTrustLedgerSessions()
	if !live("session:uuid-1") {
		t.Error("adopted session reads as dead; the sweep would undo a live session's trust")
	}
	// The user-facing display name is a different namespace: a ledger row
	// keyed on it must NOT read as live just because a session with that
	// name is adopted. The sweep protects the UUID-keyed row; the name
	// alone protects nothing.
	if live("session:chief-of-staff") {
		t.Error("display name reads as live; rows keyed on the name would escape the sweep forever")
	}
	if live("session:uuid-other") {
		t.Error("non-adopted session reads as live; its crash rows would never sweep")
	}
}
