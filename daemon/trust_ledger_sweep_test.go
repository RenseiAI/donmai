package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// TestSweepSeededTrustLedger_CapsLedgerNamedDiscovery proves the sweep
// visits only a bounded number of ledger-named override homes: a ledger
// naming far more distinct homes than the cap still leaves the daemon
// starting promptly, and the completion line says the cap engaged. RED
// proof: follow ledger-named homes without a cap and the sweeper visits
// every named home.
func TestSweepSeededTrustLedger_CapsLedgerNamedDiscovery(t *testing.T) {
	// No t.Parallel: Setenv forbids it.
	processHome := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", processHome)
	t.Setenv("HOME", processHome)
	if err := os.WriteFile(filepath.Join(processHome, ".claude.json"), []byte(`{"projects":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Name more distinct homes than the sweep may visit. Every named
	// home exists on disk so the sweeper cannot distinguish them from a
	// real isolated home; the cap — not a missing directory — is what
	// stops the walk.
	const named = trustLedgerMaxDiscoveredHomes + 20
	for i := 0; i < named; i++ {
		dir := t.TempDir()
		// Plant one directory-less discovery row per home in the
		// ledger file the daemon's discovery pass reads
		// (LedgerConfigHomes): the row shape matches the production
		// seed's link, so this is the production discovery input
		// without re-running the seed 84 times.
		plantDiscoveryRow(t, processHome, dir)
	}
	var calls []string
	d := newTestDaemonForSweep(t, Options{})
	d.opts.TrustLedgerSweeper = func(configHome string, _ func(session string) bool) int {
		calls = append(calls, configHome)
		return 0
	}
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	d.sweepSeededTrustLedger(context.Background())
	if len(calls) > 1+trustLedgerMaxDiscoveredHomes {
		t.Errorf("sweep visited %d homes for %d ledger-named homes, want at most 1 process home + %d discovered", len(calls), named, trustLedgerMaxDiscoveredHomes)
	}
	if got := logs.String(); !strings.Contains(got, "discoveryCapped=true") {
		t.Errorf("completion line does not report the engaged discovery cap: %q", got)
	}
}

// plantDiscoveryRow appends one directory-less discovery row naming home
// to the ledger beside the process home's trust record: the ledger file
// the daemon's discovery pass reads (LedgerConfigHomes under the trust
// lock). The row shape matches the production seed's link
// (recordTrustLedgerDiscoveryLink), with a fixed probe session that reads
// as dead — the cap test's sweeper never consults liveness — so the
// planted rows exercise only the discovery enumeration the cap bounds.
// The ledger basename is the exported TrustLedgerFileName beside the
// exported TrustRecordFileName: the same file the production seed writes.
func plantDiscoveryRow(t *testing.T, processHome, home string) {
	t.Helper()
	ledger := filepath.Join(processHome, providerclaude.TrustLedgerFileName)
	raw, err := os.ReadFile(ledger)
	var rows []map[string]any
	if err == nil {
		if err := json.Unmarshal(raw, &rows); err != nil {
			t.Fatal(err)
		}
	}
	rows = append(rows, map[string]any{"session": "session:cap-probe", "configHome": home})
	body, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledger, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestSweepSeededTrustLedger_DrainsSessionScopedOverrideHomes drives the
// PRODUCTION sweeper through the daemon step from a PRODUCTION seed: a
// crashed session seeded its trust under a session-scoped CLAUDE_CONFIG_DIR
// override the daemon process itself never ran with, so the process-home
// enumeration never names that home. The seed left a discovery link behind
// the process home (a directory-less ledger row naming the override home),
// and the sweep follows it — a crash there still drains. RED proof: seed
// with the discovery link disabled and the override home keeps its seeded
// entry while the process home drains clean.
func TestSweepSeededTrustLedger_DrainsSessionScopedOverrideHomes(t *testing.T) {
	// No t.Parallel: Setenv forbids it.
	processHome := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", processHome)
	// The process-home enumeration also visits the operator home; point
	// it at the scratch dir so the sweep never reads the operator's real
	// trust record.
	t.Setenv("HOME", processHome)
	overrideHome := t.TempDir()

	// A crashed isolated session's seed, built the way production builds
	// it: the session env layer carries the override home, the daemon
	// process home stays out of the picture. The disk layout — trust
	// record, ledger rows, and the process-home discovery link — is
	// whatever the production seed wrote; the test only supplies homes.
	if err := os.WriteFile(filepath.Join(processHome, ".claude.json"), []byte(`{"projects":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	seeded := providerclaude.SeedWorkspaceTrustForSweepTest(workdir, overrideHome, "uuid-dead-isolated")
	overrideRecord := seeded.TrustRecordPath
	if overrideRecord == "" {
		t.Fatal("production seed under the session-scoped override wrote nothing")
	}
	if want := filepath.Join(overrideHome, providerclaude.TrustRecordFileName); overrideRecord != want {
		t.Fatalf("production seed wrote %q, want the override home %q", overrideRecord, want)
	}
	raw, err := os.ReadFile(overrideRecord)
	if err != nil {
		t.Fatal(err)
	}
	var before map[string]any
	if err := json.Unmarshal(raw, &before); err != nil {
		t.Fatal(err)
	}
	if projects, _ := before["projects"].(map[string]any); len(projects) == 0 {
		t.Fatal("production seed left no trust entry under the override home to drain")
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
	if len(projects) != 0 {
		t.Errorf("crashed session's override-home entries survive the daemon sweep: %v", projects)
	}
}

// TestSweepSeededTrustLedger_LiveRestartKeepsTheDiscoveryLink proves the
// live-restart-then-crash sequence drains: a session seeds its trust under
// a session-scoped config-home override through the PRODUCTION seed, the
// daemon restarts mid-run with that session adopted and live — the
// process-home sweep keeps the live discovery pointer while the isolated
// rows stay protected — then the session crashes and the next start drains
// the override home. RED proof: drain directory-less rows unconditionally
// and the live sweep drops the pointer, so the post-crash sweep discovers
// no homes and the override-home entries survive.
func TestSweepSeededTrustLedger_LiveRestartKeepsTheDiscoveryLink(t *testing.T) {
	// No t.Parallel: Setenv forbids it.
	processHome := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", processHome)
	// The process-home enumeration also visits the operator home; point
	// it at the scratch dir so the sweep never reads the operator's real
	// trust record.
	t.Setenv("HOME", processHome)
	overrideHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(processHome, ".claude.json"), []byte(`{"projects":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	seeded := providerclaude.SeedWorkspaceTrustForSweepTest(workdir, overrideHome, "uuid-live-then-crash")
	overrideRecord := seeded.TrustRecordPath
	if overrideRecord == "" {
		t.Fatal("production seed under the session-scoped override wrote nothing")
	}
	// Live boot: the daemon restarts mid-run with the session adopted.
	// Nothing under either home may drain — and the process-home sweep
	// must keep the session's discovery pointer, the only route to the
	// isolated home.
	live := newTestDaemonForSweep(t, Options{})
	live.shims.mu.Lock()
	live.shims.adopted[sessionshim.Identity{OrgID: "org-1", SessionID: "uuid-live-then-crash"}] = adoptedShim{}
	live.shims.mu.Unlock()
	live.opts.TrustLedgerSweeper = providerclaude.SweepSeededTrustForConfigHome
	live.sweepSeededTrustLedger(context.Background())
	readProjects := func(path string) map[string]any {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var record map[string]any
		if err := json.Unmarshal(raw, &record); err != nil {
			t.Fatal(err)
		}
		projects, _ := record["projects"].(map[string]any)
		return projects
	}
	if projects := readProjects(overrideRecord); len(projects) == 0 {
		t.Fatal("live sweep drained a live session's isolated trust entries")
	}
	linkLedger := filepath.Join(processHome, providerclaude.TrustRecordFileName)
	survived := false
	for _, e := range readLinkLedger(t, linkLedger) {
		if e.ConfigHome == overrideHome {
			survived = true
		}
	}
	if !survived {
		t.Fatalf("live sweep drained the live session's discovery link to %q; a later crash would strand that home's debris", overrideHome)
	}
	// Crash: the session is gone (a fresh daemon adopts nothing) and the
	// next start drains the override home through the surviving pointer.
	crashed := newTestDaemonForSweep(t, Options{})
	crashed.opts.TrustLedgerSweeper = providerclaude.SweepSeededTrustForConfigHome
	crashed.sweepSeededTrustLedger(context.Background())
	if projects := readProjects(overrideRecord); len(projects) != 0 {
		t.Errorf("crashed session's override-home entries survive the post-crash sweep: %v", projects)
	}
}

// readLinkLedger returns the daemon test's view of one home's ledger:
// the trust rows SweepSeededTrustForConfigHome reports on, resolved
// through the production config-home entry point. Directory-less
// discovery rows never surface here — the entry point's sweep keeps a
// live session's pointer (see sweepTrustLedger) — so the test reads the
// link through the discovery pass itself: the homes the production
// LedgerConfigHomes names beyond the home under test.
func readLinkLedger(t *testing.T, trustRecordPath string) []struct{ ConfigHome string } {
	t.Helper()
	home := filepath.Dir(trustRecordPath)
	visited := map[string]struct{}{home: {}}
	var out []struct{ ConfigHome string }
	for _, named := range providerclaude.LedgerConfigHomes(trustRecordPath, visited) {
		out = append(out, struct{ ConfigHome string }{ConfigHome: named})
	}
	return out
}

// predicate protects adopted, non-terminal sessions, live pid-owned rows
// else. The adopted identity carries a lifecycle UUID while the display
// name differs on purpose: the sweep keys liveness on the UUID the ledger
// rows carry, never on the user-facing name, so a same-string fixture
// would mask a namespace mismatch. RED proof: build the predicate from
// the registry Scan instead of the adopted map and an
// adopted-but-not-yet-registered session reads as dead.
func TestLiveTrustLedgerSessions_AdoptedSessionsAreLive(t *testing.T) {
	t.Parallel()
	d := newTestDaemonForSweep(t, Options{})
	live := d.liveTrustLedgerSessions()
	if live("session:anything") {
		t.Error("empty daemon reads a session as live; every row would be protected forever")
	}
	// A definitely-dead pid reads as dead, so crash debris still drains;
	// the test process's own pid reads as live, so an id-less session
	// keeps its trust across a daemon restart mid-run. Both verdicts go
	// through the shared provider verdict, so daemon and provider can
	// never disagree about one identity.
	if live("pid:12345") != providerclaude.TrustLedgerSessionLive("pid:12345", nil) {
		t.Error("dead-pid verdict disagrees with the shared provider verdict; daemon and provider must agree")
	}
	if live("pid:1") != providerclaude.TrustLedgerSessionLive("pid:1", nil) {
		t.Error("pid-1 verdict disagrees with the shared provider verdict; daemon and provider must agree")
	}
	if myself := "pid:" + strconv.Itoa(os.Getpid()); !live(myself) {
		t.Errorf("%s (this test process) reads as dead; a live id-less session would lose its trust", myself)
	}
	if live("") {
		t.Error("empty session identity reads as live; malformed rows would escape the sweep forever")
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
