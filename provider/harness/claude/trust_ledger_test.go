package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
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
//
// The live row deliberately carries a display name that differs from its
// lifecycle UUID: the sweep keys liveness on the UUID (the daemon's adopted
// identity), so a same-string fixture would mask a namespace mismatch.
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
	live := &seededTrust{path: path, dirs: []string{"/live/created"}, session: "session:uuid-live"}
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
		Live:            func(s string) bool { return s == "session:uuid-live" },
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
	if len(entries) != 1 || entries[0].Session != "session:uuid-live" {
		t.Errorf("ledger after sweep = %v, want only the live session's row", entries)
	}
}

// TestSweepTrustLedger_NeverRewritesAForeignRecord proves the sweep leaves
// a record it cannot parse — or whose projects field has an unexpected
// shape — byte-for-byte as found, and keeps the rows for the next start to
// retry. The retry is bounded: repeated passes against a record that can
// never change increment each row's foreign-retry count and drain past the
// bound, so un-appliable rows retire instead of accumulating as residue
// retried every boot.
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
			// The retry is bounded, not forever: repeated passes against
			// a record that can never change retire the row instead of
			// carrying it every boot, and the foreign record itself is
			// never rewritten. RED proof: keep the rows unconditionally
			// and the ledger still carries the dead session's row past
			// the bound.
			for i := 0; i < trustLedgerMaxForeignRetries; i++ {
				sweepTrustLedger(SweepTrustLedgerOptions{TrustRecordPath: path, Live: liveNone})
			}
			if entries, ok := readTrustLedger(path); ok && len(entries) != 0 {
				t.Errorf("foreign-record rows survive %d retries; the retry must be bounded: %v", trustLedgerMaxForeignRetries+1, entries)
			}
			raw2, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw2) != body {
				t.Errorf("bounded foreign retry rewrote the record: got %q, want it untouched %q", raw2, body)
			}
		})
	}
}

// TestSweepTrustLedger_KeepsLiveDiscoveryLinksDrainsDeadOnes proves a
// directory-less discovery row drains only with its own dead session. A
// live session's pointer survives its home's own sweep pass — while the
// rows beside the isolated home it names stay protected too — so a daemon
// restart mid-run keeps the pointer and a later crash still drains. Once
// the session is gone, the pointer drains with its own pass. RED proof:
// drain directory-less rows unconditionally and the live pointer is gone
// after the first sweep, stranding the isolated home's debris.
func TestSweepTrustLedger_KeepsLiveDiscoveryLinksDrainsDeadOnes(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ".claude.json")
	if err := os.WriteFile(path, []byte(`{"projects":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	override := t.TempDir()
	linkHome := override
	// Plant the rows by hand: the point here is the sweep's liveness gate
	// on directory-less rows, not the seed. The trust row and the
	// directory-less pointer carry the same owning session, the way the
	// production seed writes them.
	rewriteLedgerForTest(t, path, []trustLedgerEntry{
		{Dir: "/live/created", Created: true, Session: "session:uuid-live-link", ConfigHome: linkHome},
		{Session: "session:uuid-live-link", ConfigHome: linkHome},
	})
	live := func(s string) bool { return s == "session:uuid-live-link" }
	if dropped := sweepTrustLedger(SweepTrustLedgerOptions{TrustRecordPath: path, Live: live}); dropped != 0 {
		t.Errorf("sweep with the session live dropped %d rows, want 0 — the pointer must survive restarts", dropped)
	}
	entries, ok := readTrustLedger(path)
	if !ok {
		t.Fatal("ledger unreadable after the live sweep")
	}
	links := 0
	for _, e := range entries {
		if e.Dir == "" {
			links++
			if e.Session != "session:uuid-live-link" || e.ConfigHome != linkHome {
				t.Errorf("live sweep rewrote the discovery pointer: %+v", e)
			}
		}
	}
	if links != 1 {
		t.Errorf("live sweep left %d discovery rows, want the session's pointer kept: %v", links, entries)
	}
	// The discovery pointer still names the isolated home while the
	// session is live — the daemon follows it without draining it.
	visited := map[string]struct{}{filepath.Dir(path): {}}
	if got := LedgerConfigHomes(path, visited); len(got) != 1 || got[0] != linkHome {
		t.Errorf("LedgerConfigHomes after the live sweep = %q, want [%q]", got, linkHome)
	}
	if dropped := sweepTrustLedger(SweepTrustLedgerOptions{TrustRecordPath: path, Live: liveNone}); dropped != 2 {
		t.Errorf("sweep with the session dead dropped %d rows, want 2 (the trust row + its pointer)", dropped)
	}
	if entries, ok := readTrustLedger(path); ok && len(entries) != 0 {
		t.Errorf("dead session's rows survive the sweep: %v", entries)
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

// TestLedgerConfigHomes_TakesTheTrustLockForDiscovery proves the sweep's
// discovery read serializes with concurrent writers: with the trust lock
// held elsewhere, LedgerConfigHomes lists nothing rather than tearing —
// the sweep skips the home for this boot instead of reading half a
// ledger. The lock is the real per-home trust lock (acquireTrustLock),
// not a test double, so this drives the production serialization.
// RED proof: read the ledger without the lock and the homes list through
// even while the lock is held.
func TestLedgerConfigHomes_TakesTheTrustLockForDiscovery(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), claudeTrustFileName)
	seed := &seededTrust{path: path, dirs: []string{"/dead/created"}, session: "session:dead"}
	recordSeededTrustLedger(seed)
	entries, ok := readTrustLedger(path)
	if !ok || len(entries) != 1 {
		t.Fatalf("ledger rows = %v, want the one recorded row", entries)
	}
	entries[0].ConfigHome = t.TempDir()
	rewriteLedgerForTest(t, path, entries)

	release, ok := acquireTrustLock(path)
	if !ok {
		t.Fatal("could not take the trust lock for the contention probe")
	}
	defer release()
	if got := LedgerConfigHomes(path, map[string]struct{}{}); len(got) != 0 {
		t.Errorf("LedgerConfigHomes under a held lock = %q, want nothing — the discovery read must wait on the lock, not tear", got)
	}
}

// TestLedgerConfigHomes_NamesSessionScopedOverrideHomes proves the sweep
// can find a crashed session's isolated home: a ledger wrote beside one
// home names a second absolute home in its rows, and LedgerConfigHomes
// returns exactly the unvisited ones. Rows that predate the per-row field
// — or name a relative home — list nothing, so the sweep never chases a
// path the seed could not have written.
func TestLedgerConfigHomes_NamesSessionScopedOverrideHomes(t *testing.T) {
	t.Parallel()
	override := t.TempDir()
	path := filepath.Join(t.TempDir(), claudeTrustFileName)
	seed := &seededTrust{
		path:    path,
		dirs:    []string{"/dead/created"},
		session: "session:dead",
	}
	recordSeededTrustLedger(seed)
	// Rewrite the row with an explicit override home, as a seed under a
	// session-scoped CLAUDE_CONFIG_DIR would have recorded it.
	entries, ok := readTrustLedger(path)
	if !ok || len(entries) != 1 {
		t.Fatalf("ledger rows = %v, want the one recorded row", entries)
	}
	entries[0].ConfigHome = override
	rewriteLedgerForTest(t, path, entries)

	visited := map[string]struct{}{filepath.Dir(path): {}}
	got := LedgerConfigHomes(path, visited)
	if len(got) != 1 || got[0] != override {
		t.Errorf("LedgerConfigHomes = %q, want [%q]", got, override)
	}
	// A second call names nothing new: the home is now visited.
	if again := LedgerConfigHomes(path, visited); len(again) != 0 {
		t.Errorf("LedgerConfigHomes revisit = %q, want nothing new", again)
	}

	// Rows without a home — or with a relative one — list nothing.
	bare := filepath.Join(t.TempDir(), claudeTrustFileName)
	bareSeed := &seededTrust{path: bare, dirs: []string{"/dead/created"}, session: "session:dead"}
	recordSeededTrustLedger(bareSeed)
	bareEntries, ok := readTrustLedger(bare)
	if !ok || len(bareEntries) != 1 {
		t.Fatalf("bare ledger rows = %v, want one", bareEntries)
	}
	bareEntries[0].ConfigHome = ""
	rewriteLedgerForTest(t, bare, bareEntries)
	if got := LedgerConfigHomes(bare, map[string]struct{}{}); len(got) != 0 {
		t.Errorf("LedgerConfigHomes for a home-less row = %q, want nothing", got)
	}
}

// TestSeedWorkspaceTrust_LeavesADiscoveryLinkBehindTheProcessHome proves
// the production seed under a session-scoped CLAUDE_CONFIG_DIR override
// ALSO leaves a discovery pointer behind the daemon process's own home: a
// directory-less ledger row naming the override home, so the daemon-start
// sweep visits the isolated home even though the process enumeration can
// never name it. Same-home seeds leave no link — the process enumeration
// finds them itself. RED proof: drop the recordTrustLedgerDiscoveryLink
// call and the process-home ledger stays absent while the isolated seed
// still lands.
func TestSeedWorkspaceTrust_LeavesADiscoveryLinkBehindTheProcessHome(t *testing.T) {
	// No t.Parallel: Setenv forbids it.
	processHome := t.TempDir()
	t.Setenv(claudeConfigDirEnv, processHome)
	t.Setenv("HOME", processHome)
	overrideHome := t.TempDir()
	workdir := t.TempDir()

	seeded := SeedWorkspaceTrustForSweepTest(workdir, overrideHome, "uuid-link-prod")
	seedPath := seeded.TrustRecordPath
	if seedPath == "" {
		t.Fatal("production seed under the session-scoped override wrote nothing")
	}
	if want := filepath.Join(overrideHome, claudeTrustFileName); seedPath != want {
		t.Fatalf("seed path = %q, want the override home %q", seedPath, want)
	}
	for _, e := range readLedgerEntries(t, seedPath) {
		if e.Session != "session:uuid-link-prod" {
			t.Errorf("isolated row carries the wrong owner: %+v", e)
		}
	}

	// The isolated seed landed beside the override home.
	isolated := readLedgerEntries(t, seedPath)
	if len(isolated) == 0 {
		t.Fatal("isolated ledger beside the override home is empty")
	}
	for _, e := range isolated {
		if e.Dir == "" {
			t.Errorf("isolated ledger carries a directory-less row; links belong to the process home: %+v", e)
		}
		if e.ConfigHome != overrideHome {
			t.Errorf("isolated row names home %q, want the override home %q", e.ConfigHome, overrideHome)
		}
	}

	// The process home carries ONLY the discovery link: no directory, no
	// trust to undo, just the override home to visit.
	linkEntries, ok := readTrustLedger(filepath.Join(processHome, claudeTrustFileName))
	if !ok || len(linkEntries) == 0 {
		t.Fatalf("process-home ledger carries no discovery link for %q", overrideHome)
	}
	named := false
	for _, e := range linkEntries {
		if e.Dir != "" {
			t.Errorf("process-home ledger carries a trust row %+v; it must carry only links", e)
		}
		if e.ConfigHome == overrideHome && e.Session == "session:uuid-link-prod" {
			named = true
		}
	}
	if !named {
		t.Errorf("process-home ledger names no link to %q: %v", overrideHome, linkEntries)
	}
	// The live path drops the link by session+home (see
	// forgetTrustLedgerDiscoveryLink): forget the crashed session's link
	// through the production forget path and prove its link drains while
	// the isolated trust rows — owned by the same session but beside the
	// override home — are untouched by the process-home forget.
	forgetSeededTrustLedger(&seededTrust{path: seedPath, dirs: []string{"defunct"}, session: "session:uuid-link-prod"})
	if entries, ok := readTrustLedger(filepath.Join(processHome, claudeTrustFileName)); !ok {
		t.Fatal("process-home ledger unreadable after forgetting the session's link")
	} else {
		for _, e := range entries {
			if e.Session == "session:uuid-link-prod" && e.ConfigHome == overrideHome {
				t.Errorf("forgotten session's link survives: %v", entries)
			}
		}
	}
	// The isolated rows are still beside the override home — the
	// process-home forget never touches another home's ledger — and a
	// full live cleanup of a session that DID run drains both.
	if entries := readLedgerEntries(t, seedPath); len(entries) == 0 {
		t.Error("process-home forget drained the override home's ledger; homes must forget independently")
	}
	liveSecond := t.TempDir()
	live := seedWorkspaceTrust(agent.Spec{
		SessionID: "uuid-link-live",
		Cwd:       liveSecond,
		Env:       map[string]string{claudeConfigDirEnv: overrideHome},
	})
	if live == nil {
		t.Fatal("second production seed wrote nothing")
	}
	removeSeededTrust(live)
	if entries, ok := readTrustLedger(filepath.Join(processHome, claudeTrustFileName)); ok && len(entries) != 0 {
		t.Errorf("live cleanup left the discovery link behind: %v", entries)
	}
}

// TestSeedWorkspaceTrust_SameHomeSeedLeavesNoDiscoveryLink proves a seed
// beside the daemon process's own home leaves no discovery pointer: the
// process enumeration finds that home itself, so a link would only add a
// no-op sweep pass.
func TestSeedWorkspaceTrust_SameHomeSeedLeavesNoDiscoveryLink(t *testing.T) {
	// No t.Parallel: Setenv forbids it.
	processHome := t.TempDir()
	t.Setenv(claudeConfigDirEnv, processHome)
	t.Setenv("HOME", processHome)
	workdir := t.TempDir()

	seed := seedWorkspaceTrust(agent.Spec{SessionID: "uuid-same-home", Cwd: workdir})
	if seed == nil {
		t.Fatal("same-home seed wrote nothing")
	}
	if want := filepath.Join(processHome, claudeTrustFileName); seed.path != want {
		t.Fatalf("seed path = %q, want the process home %q", seed.path, want)
	}
	for _, e := range readLedgerEntries(t, seed.path) {
		if e.Dir == "" {
			t.Errorf("same-home seed left a discovery link; the sweep enumerates this home itself: %+v", e)
		}
	}
	removeSeededTrust(seed)
}

// TestTrustLedgerSessionLive_PidRowsFollowProcessLiveness proves the
// id-less fallback: a ledger identity naming a RUNNING process reads as
// live (a standalone/legacy session keeps its trust across a daemon
// restart mid-run), while a definitely-dead pid and every malformed
// identity read as dead (crash debris still drains). Adopted session
// identities read as live regardless of any process. RED proof: read
// pid rows as dead-by-design and the live id-less session's trust is
// swept from under it.
func TestTrustLedgerSessionLive_PidRowsFollowProcessLiveness(t *testing.T) {
	t.Parallel()
	adopted := map[string]struct{}{"session:uuid-live": {}}
	if !TrustLedgerSessionLive("session:uuid-live", adopted) {
		t.Error("adopted session reads as dead")
	}
	if TrustLedgerSessionLive("session:uuid-other", adopted) {
		t.Error("non-adopted session reads as live")
	}
	if !TrustLedgerSessionLive("pid:1", adopted) {
		t.Log("note: pid 1 not visible from this sandbox; live-pid half checked below")
	}
	liveSelf := "pid:" + strconv.Itoa(os.Getpid())
	if !TrustLedgerSessionLive(liveSelf, nil) {
		t.Errorf("%s (this test process) reads as dead; the daemon restart would sweep a live id-less session", liveSelf)
	}
	// A pid that cannot exist is dead: crash debris drains.
	if TrustLedgerSessionLive("pid:2147483647", nil) {
		t.Error("unallocatable pid reads as live; crash debris would never drain")
	}
	for _, malformed := range []string{"", "session:", "pid:", "pid:abc", "pid:-3", "pid:0", "host:1"} {
		if TrustLedgerSessionLive(malformed, nil) {
			t.Errorf("%q reads as live; malformed rows must sweep", malformed)
		}
		if TrustLedgerSessionLive(malformed, adopted) && malformed != "session:uuid-live" {
			t.Errorf("%q reads as live against an adopted set that does not name it", malformed)
		}
	}
}

// rewriteLedgerForTest replaces the ledger beside path under the trust
// lock. Test-only helper for hand-editing rows the seed wrote.
func rewriteLedgerForTest(t *testing.T, path string, entries []trustLedgerEntry) {
	t.Helper()
	release, ok := acquireTrustLock(path)
	if !ok {
		t.Fatal("could not take the trust lock for the test rewrite")
	}
	defer release()
	if err := writeTrustLedger(path, entries); err != nil {
		t.Fatal(err)
	}
}

// PRODUCTION seed entry point: seedWorkspaceTrust records the ledger rows
// under the isolated config home with the session's lifecycle UUID as
// owner — the same namespace the daemon sweep's liveness set keys on — so
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
		SessionID:   "uuid-ledger-owner-prod",
		Cwd:         workdir,
		SessionName: "ledger-owner-prod",
	})
	if seed == nil {
		t.Fatal("seedWorkspaceTrust seeded nothing")
	}
	if seed.session != "session:uuid-ledger-owner-prod" {
		t.Errorf("seed owner = %q, want the lifecycle UUID so the sweep can re-home it", seed.session)
	}
	entries := readLedgerEntries(t, filepath.Join(cfgHome, claudeTrustFileName))
	for _, e := range entries {
		if e.Session != "session:uuid-ledger-owner-prod" {
			t.Errorf("ledger row carries the wrong owner: %+v", e)
		}
		if e.ConfigHome != cfgHome {
			t.Errorf("ledger row names config home %q, want the home the seed wrote %q", e.ConfigHome, cfgHome)
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

// TestTrustLedgerOwner_KeysOnTheLifecycleUUID proves the ledger owner is
// the session's lifecycle UUID — the same namespace the daemon sweep's
// liveness set keys on — never the user-facing display name. A session
// without a lifecycle id still gets a ledger owner — the process identity
// — so its rows are swept only when no live session claims them.
func TestTrustLedgerOwner_KeysOnTheLifecycleUUID(t *testing.T) {
	t.Parallel()
	if got := trustLedgerOwner(agent.Spec{SessionID: "uuid-1", SessionName: "different-name"}); got != "session:uuid-1" {
		t.Errorf("ledger owner = %q, want session:uuid-1: the display name is a different namespace", got)
	}
	owner := trustLedgerOwner(agent.Spec{})
	if owner == "" {
		t.Fatal("session without a lifecycle id has no ledger owner; its rows could never be swept")
	}
	raw, err := json.Marshal(trustLedgerEntry{Dir: "/x", Session: owner, ConfigHome: "/cfg"})
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip trustLedgerEntry
	if err := json.Unmarshal(raw, &roundTrip); err != nil || roundTrip.Session != owner || roundTrip.ConfigHome != "/cfg" {
		t.Errorf("ledger owner does not survive a ledger round trip: %q", raw)
	}
}
