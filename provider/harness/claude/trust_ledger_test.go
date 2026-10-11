package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// This file pins the crash-safety ledger (trust_ledger.go): a seed records
// what it changed beside the trust record, the live-path cleanup forgets
// those rows, and the daemon-start sweep undoes rows whose session is no
// longer live. Every test uses a temp config home; none touches the
// operator's real CLI config (see TestMain).

// readLedgerEntries returns the ledger rows beside the trust record at path.
func readLedgerEntries(t *testing.T, path string) []trustLedgerEntry {
	t.Helper()
	entries, ok := readTrustLedger(path)
	if !ok {
		t.Fatalf("ledger beside %s is absent or unparseable", path)
	}
	trustLedgerSortForTest(entries)
	return entries
}

// liveNone reads every session as dead.
func liveNone(string) bool { return false }

// TestSeedTrustFile_RecordsLedgerRows proves the seed records one row per
// directory it created or flipped — with the owning session and the
// flipped prior — and no row for an already-trusted entry. RED proof:
// drop the recordSeededTrustLedger call from seedWorkspaceTrust and the
// ledger stays absent.
func TestSeedTrustFile_RecordsLedgerRows(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ".claude.json")
	writeTrustRecord(t, path, map[string]any{
		"/operator/flipped": map[string]any{claudeTrustAcceptedKey: false, "history": []any{"h"}},
		"/operator/kept":    map[string]any{claudeTrustAcceptedKey: true},
	})
	seed := seedTrustFile(path, []string{"/operator/flipped", "/operator/kept", "/session/new"})
	if seed == nil {
		t.Fatal("seed wrote nothing")
	}
	seed.session = "session:test-ledger"
	recordSeededTrustLedger(seed)

	entries := readLedgerEntries(t, path)
	if len(entries) != 2 {
		t.Fatalf("ledger rows = %v, want one created row and one flipped row", entries)
	}
	byDir := map[string]trustLedgerEntry{}
	for _, e := range entries {
		byDir[e.Dir] = e
	}
	created, ok := byDir["/session/new"]
	if !ok || !created.Created || created.Session != "session:test-ledger" {
		t.Errorf("created row wrong: %v", byDir)
	}
	flipped, ok := byDir["/operator/flipped"]
	if !ok || flipped.Created || flipped.Session != "session:test-ledger" {
		t.Errorf("flipped row wrong: %v", byDir)
	}
	if !flipped.HadKey || flipped.Prior != false {
		t.Errorf("flipped row lost its prior value: %+v", flipped)
	}
}

// TestRemoveSeededTrust_ForgetsLedgerRows proves the live-path cleanup
// drops exactly its own rows: after removeSeededTrust the ledger no longer
// names the session's entries, while another session's row for a shared
// directory survives. RED proof: drop the forgetSeededTrustLedger defer
// and the session's rows survive cleanup.
func TestRemoveSeededTrust_ForgetsLedgerRows(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ".claude.json")
	writeTrustRecord(t, path, map[string]any{
		"/shared/dir": map[string]any{claudeTrustAcceptedKey: true},
	})
	mine := &seededTrust{path: path, dirs: []string{"/shared/dir"}, session: "session:mine"}
	theirs := &seededTrust{path: path, dirs: []string{"/shared/dir"}, session: "session:theirs"}
	recordSeededTrustLedger(mine)
	recordSeededTrustLedger(theirs)
	if entries := readLedgerEntries(t, path); len(entries) != 2 {
		t.Fatalf("ledger rows = %v, want both sessions recorded", entries)
	}

	removeSeededTrust(mine)

	entries := readLedgerEntries(t, path)
	if len(entries) != 1 || entries[0].Session != "session:theirs" {
		t.Errorf("live cleanup dropped the wrong rows: %v", entries)
	}
}

// TestSweepTrustLedger_UndoesDeadSessionsKeepsLiveOnes drives the daemon
// crash sweep: a dead session's created entry is removed whole, its flipped
// entry is put back to its prior value, a live session's entries are
// untouched, and the drained rows leave the ledger. RED proof: sweep with a
// live-set that reads every session as live and nothing is undone.
func TestSweepTrustLedger_UndoesDeadSessionsKeepsLiveOnes(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ".claude.json")
	writeTrustRecord(t, path, map[string]any{
		"/operator/keep":    map[string]any{claudeTrustAcceptedKey: true, "extra": "kept"},
		"/operator/flipped": map[string]any{claudeTrustAcceptedKey: false, "history": []any{"h"}},
	})
	dead := &seededTrust{
		path:    path,
		dirs:    []string{"/dead/created"},
		flipped: []string{"/operator/flipped"},
		prior:   map[string]any{"/operator/flipped": false},
		session: "session:dead",
	}
	live := &seededTrust{path: path, dirs: []string{"/live/created"}, session: "session:live"}
	// Land the rows by hand: seedTrustFile would refuse to flip an entry
	// that is already trusted, and the point here is the sweep, not the
	// seed. The record itself is seeded the same way.
	deadSeed := seedTrustFile(path, []string{"/dead/created"})
	if deadSeed == nil {
		t.Fatal("seed wrote nothing")
	}
	deadSeed.session = dead.session
	recordSeededTrustLedger(deadSeed)
	liveSeed := seedTrustFile(path, []string{"/live/created"})
	if liveSeed == nil {
		t.Fatal("seed wrote nothing")
	}
	liveSeed.session = live.session
	recordSeededTrustLedger(liveSeed)
	flippedSeed := seedTrustFile(path, []string{"/operator/flipped"})
	if flippedSeed == nil {
		t.Fatal("flip wrote nothing")
	}
	flippedSeed.session = dead.session
	recordSeededTrustLedger(flippedSeed)

	dropped := sweepTrustLedger(SweepTrustLedgerOptions{
		TrustRecordPath: path,
		Live:            func(s string) bool { return s == "session:live" },
	})
	if dropped != 2 {
		t.Errorf("sweep dropped %d rows, want 2 (the dead session's created + flipped rows)", dropped)
	}

	projects := readTrustProjects(t, path)
	if _, present := projects["/dead/created"]; present {
		t.Errorf("dead session's created entry survives the sweep: %v", projects)
	}
	flipped, ok := projects["/operator/flipped"].(map[string]any)
	if !ok || flipped[claudeTrustAcceptedKey] != false || flipped["history"] == nil {
		t.Errorf("dead session's flipped entry not restored: %v", projects)
	}
	if entry, ok := projects["/live/created"].(map[string]any); !ok || entry[claudeTrustAcceptedKey] != true {
		t.Errorf("live session's entry was swept: %v", projects)
	}
	kept, _ := projects["/operator/keep"].(map[string]any)
	if kept[claudeTrustAcceptedKey] != true || kept["extra"] != "kept" {
		t.Errorf("unrelated entry disturbed by the sweep: %v", projects)
	}
	entries := readLedgerEntries(t, path)
	if len(entries) != 1 || entries[0].Session != "session:live" {
		t.Errorf("ledger after sweep = %v, want only the live session's row", entries)
	}
}

// TestSweepTrustLedger_NeverRewritesAForeignRecord proves the sweep leaves
// a record it cannot parse — or whose projects field has an unexpected
// shape — byte-for-byte as found, and keeps the rows for the next start to
// retry.
func TestSweepTrustLedger_NeverRewritesAForeignRecord(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"malformed":       "{not json",
		"truncated":       `{"oauthAccount":{"id":"x"},"projects":{`,
		"empty":           "",
		"projects-string": `{"oauthAccount":{"id":"x"},"projects":"unexpected"}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), ".claude.json")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			seed := &seededTrust{path: path, dirs: []string{"/dead/created"}, session: "session:dead"}
			recordSeededTrustLedger(seed)

			if dropped := sweepTrustLedger(SweepTrustLedgerOptions{TrustRecordPath: path, Live: liveNone}); dropped != 0 {
				t.Errorf("sweep dropped %d rows against a foreign record, want 0 — the rows stay for retry", dropped)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != body {
				t.Errorf("sweep rewrote a foreign record: got %q, want it untouched %q", raw, body)
			}
			if entries := readLedgerEntries(t, path); len(entries) != 1 {
				t.Errorf("sweep drained rows it did not apply: %v", entries)
			}
		})
	}
}

// TestSweepSeededTrustForConfigHome_SweepsOnlyTheLedger proves the config-
// home entry point sweeps the ledger beside the trust record: a dead
// session's entry goes away, and a home with no ledger — or a relative
// home — sweeps nothing.
func TestSweepSeededTrustForConfigHome_SweepsOnlyTheLedger(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	path := filepath.Join(home, claudeTrustFileName)
	writeTrustRecord(t, path, map[string]any{
		"/operator/keep": map[string]any{claudeTrustAcceptedKey: true},
	})
	seed := seedTrustFile(path, []string{"/dead/created"})
	if seed == nil {
		t.Fatal("seed wrote nothing")
	}
	seed.session = "session:dead"
	recordSeededTrustLedger(seed)

	if dropped := SweepSeededTrustForConfigHome(home, liveNone); dropped != 1 {
		t.Errorf("config-home sweep dropped %d rows, want 1", dropped)
	}
	if projects := readTrustProjects(t, path); !trustedEntry(projects, "/operator/keep") {
		t.Errorf("unrelated entry lost in the config-home sweep: %v", projects)
	} else if _, present := projects["/dead/created"]; present {
		t.Errorf("dead entry survives the config-home sweep: %v", projects)
	}

	if dropped := SweepSeededTrustForConfigHome(t.TempDir(), liveNone); dropped != 0 {
		t.Errorf("config-home sweep without a ledger dropped %d rows, want 0", dropped)
	}
	if dropped := SweepSeededTrustForConfigHome("relative/home", liveNone); dropped != 0 {
		t.Errorf("config-home sweep with a relative home dropped %d rows, want 0", dropped)
	}
}

// TestSeedWorkspaceTrust_RecordsLedgerWithOwningSession drives the
// PRODUCTION seed entry point: seedWorkspaceTrust records the ledger rows
// under the isolated config home with the session's own name as owner, so
// the daemon sweep can tell this live session's entries from a crashed
// one's. RED proof: drop the ledger write from seedWorkspaceTrust and the
// ledger stays absent.
func TestSeedWorkspaceTrust_RecordsLedgerWithOwningSession(t *testing.T) {
	// No t.Parallel: Setenv forbids it.
	workdir := t.TempDir()
	cfgHome := t.TempDir()
	t.Setenv(claudeConfigDirEnv, cfgHome)
	t.Setenv("HOME", t.TempDir())

	seed := seedWorkspaceTrust(agent.Spec{
		Cwd:         workdir,
		SessionName: "ledger-owner-prod",
	})
	if seed == nil {
		t.Fatal("seedWorkspaceTrust seeded nothing")
	}
	if seed.session != "session:ledger-owner-prod" {
		t.Errorf("seed owner = %q, want the session name so the sweep can re-home it", seed.session)
	}
	entries := readLedgerEntries(t, filepath.Join(cfgHome, claudeTrustFileName))
	for _, e := range entries {
		if e.Session != "session:ledger-owner-prod" {
			t.Errorf("ledger row carries the wrong owner: %+v", e)
		}
	}
	if len(entries) == 0 {
		t.Error("production seed recorded no ledger rows")
	}
	removeSeededTrust(seed)
	if entries, ok := readTrustLedger(filepath.Join(cfgHome, claudeTrustFileName)); ok && len(entries) != 0 {
		t.Errorf("live cleanup left ledger rows behind: %v", entries)
	}
}

// TestSweepTrustLedger_AbsentRecordDrainsRows proves a sweep against a
// ledger whose trust record is gone (deleted out from under the session)
// still drains the rows: there is nothing to undo, so keeping them would
// retry forever.
func TestSweepTrustLedger_AbsentRecordDrainsRows(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ".claude.json")
	seed := &seededTrust{path: path, dirs: []string{"/dead/created"}, session: "session:dead"}
	recordSeededTrustLedger(seed)

	if dropped := sweepTrustLedger(SweepTrustLedgerOptions{TrustRecordPath: path, Live: liveNone}); dropped != 1 {
		t.Errorf("sweep against an absent record dropped %d rows, want 1", dropped)
	}
	if entries, ok := readTrustLedger(path); ok && len(entries) != 0 {
		t.Errorf("sweep left rows behind with no record to apply them to: %v", entries)
	}
}

// TestTrustLedgerOwner_FallsBackToProcessIdentity proves an unnamed session
// still gets a ledger owner — the process identity — so its rows are swept
// only when no live session in this process claims them.
func TestTrustLedgerOwner_FallsBackToProcessIdentity(t *testing.T) {
	t.Parallel()
	owner := trustLedgerOwner(agent.Spec{})
	if owner == "" {
		t.Fatal("unnamed session has no ledger owner; its rows could never be swept")
	}
	if got := trustLedgerOwner(agent.Spec{SessionName: "named"}); got != "session:named" {
		t.Errorf("named session owner = %q, want session:named", got)
	}
	raw, err := json.Marshal(trustLedgerEntry{Dir: "/x", Session: owner})
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip trustLedgerEntry
	if err := json.Unmarshal(raw, &roundTrip); err != nil || roundTrip.Session != owner {
		t.Errorf("ledger owner does not survive a ledger round trip: %q", raw)
	}
}
