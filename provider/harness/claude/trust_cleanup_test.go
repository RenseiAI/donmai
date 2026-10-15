package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// This file pins the session-end trust cleanup: the entries one session's
// seed added to the harness's global trust record are removed when that
// session ends, while pre-existing entries (the operator's own trust, other
// sessions' seeds) are left untouched. Every test uses a temp config home;
// none touches the operator's real CLI config (see TestMain).

// writeTrustRecord renders projects as the trust record at path.
func writeTrustRecord(t *testing.T, path string, projects map[string]any) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"projects": projects})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// readTrustProjects returns the projects map from the record at path.
func readTrustProjects(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trust record: %v", err)
	}
	var record map[string]any
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatalf("trust record unparseable: %v", err)
	}
	projects, _ := record["projects"].(map[string]any)
	if projects == nil {
		t.Fatalf("trust record carries no projects: %v", record)
	}
	return projects
}

// TestRemoveSeededTrust_RemovesOnlyTheSessionEntries proves the cleanup
// removes exactly what the session seeded: the session dirs go away while a
// pre-existing operator entry — including extra fields on it — survives
// byte-for-byte.
func TestRemoveSeededTrust_RemovesOnlyTheSessionEntries(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ".claude.json")
	writeTrustRecord(t, path, map[string]any{
		"/operator/keep":    map[string]any{claudeTrustAcceptedKey: true, "extra": "kept"},
		"/session/worktree": map[string]any{claudeTrustAcceptedKey: true},
	})

	removeSeededTrust(&seededTrust{path: path, dirs: []string{"/session/worktree"}})

	projects := readTrustProjects(t, path)
	if _, present := projects["/session/worktree"]; present {
		t.Errorf("session entry survives cleanup: %v", projects)
	}
	kept, _ := projects["/operator/keep"].(map[string]any)
	if kept[claudeTrustAcceptedKey] != true || kept["extra"] != "kept" {
		t.Errorf("pre-existing entry was disturbed: %v", kept)
	}
}

// TestRemoveSeededTrust_NeverRewritesAForeignRecord proves the cleanup leaves
// a record it cannot parse — or whose projects field has an unexpected shape —
// byte-for-byte as found. The record is the CLI's whole config, and a
// trust-only rewrite would drop every other field.
func TestRemoveSeededTrust_NeverRewritesAForeignRecord(t *testing.T) {
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
			removeSeededTrust(&seededTrust{path: path, dirs: []string{"/session/worktree"}})
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != body {
				t.Errorf("cleanup rewrote a foreign record: got %q, want it untouched %q", raw, body)
			}
		})
	}
}

// TestRemoveSeededTrust_NilSeedIsANoOp proves a session that seeded nothing
// (a nil record — home roots, relative redirects, empty boundaries) cleans
// up nothing and touches nothing.
func TestRemoveSeededTrust_NilSeedIsANoOp(t *testing.T) {
	t.Parallel()
	removeSeededTrust(nil)
	removeSeededTrust(&seededTrust{})
	removeSeededTrust(&seededTrust{path: filepath.Join(t.TempDir(), "absent.json"), dirs: []string{"/x"}})
}

// TestSeedThenRemove_RoundTrip proves the seed and the cleanup compose: seed
// a record holding a pre-existing entry, remove the seed, and only the
// pre-existing entry remains. RED proof: drop the removeSeededTrust call and
// the session entry survives.
func TestSeedThenRemove_RoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ".claude.json")
	writeTrustRecord(t, path, map[string]any{
		"/pre/existing": map[string]any{claudeTrustAcceptedKey: true},
	})
	seedTrustFile(path, []string{"/session/worktree"})
	if projects := readTrustProjects(t, path); !trustedEntry(projects, "/session/worktree") {
		t.Fatalf("seed did not land: %v", projects)
	}
	removeSeededTrust(&seededTrust{path: path, dirs: []string{"/session/worktree"}})
	projects := readTrustProjects(t, path)
	if _, present := projects["/session/worktree"]; present {
		t.Errorf("session entry survives the round trip: %v", projects)
	}
	if !trustedEntry(projects, "/pre/existing") {
		t.Errorf("pre-existing entry lost in the round trip: %v", projects)
	}
}

// TestSpawn_Interactive_SessionEndRemovesSeededTrust drives the PRODUCTION
// entry point end to end under an isolated config home: a pre-existing
// operator entry is present before Spawn, the session seeds its own cwd
// during the run (the gate script holds the child until the seed lands),
// and after the child exits only the operator entry remains. RED proof:
// drop removeSeededTrust from the spawn cleanup and the session cwd entry
// survives alongside the operator's.
func TestSpawn_Interactive_SessionEndRemovesSeededTrust(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	workdir := t.TempDir()
	cfgHome := t.TempDir()
	t.Setenv(claudeConfigDirEnv, cfgHome)
	writeTrustRecord(t, filepath.Join(cfgHome, claudeTrustFileName), map[string]any{
		"/operator/keep": map[string]any{claudeTrustAcceptedKey: true},
	})
	gate := filepath.Join(t.TempDir(), "gate")

	p := newFakeInteractiveProvider(t, "while [ ! -e "+shellQuote(gate)+` ]; do sleep 0.05; done
`+captureEnvArgvScript)
	h, err := p.Spawn(context.Background(), agent.Spec{
		Cwd:         workdir,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })

	if !trustSeededFor(t, cfgHome, workdir) {
		t.Fatal("session cwd was not trusted during the run; the cleanup test has nothing to remove")
	}
	if err := os.WriteFile(gate, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	awaitPTYExit(t, h)

	projects := readTrustProjects(t, filepath.Join(cfgHome, claudeTrustFileName))
	clean := filepath.Clean(workdir)
	if _, present := projects[clean]; present {
		t.Errorf("session cwd %q survives session end: %v", clean, projects)
	}
	if resolved, err := filepath.EvalSymlinks(clean); err == nil && resolved != clean {
		if _, present := projects[resolved]; present {
			t.Errorf("resolved session cwd %q survives session end: %v", resolved, projects)
		}
	}
	if !trustedEntry(projects, "/operator/keep") {
		t.Errorf("pre-existing operator entry lost at session end: %v", projects)
	}
}

// TestSeedWorkspaceTrust_ReturnsNilWhenNothingIsSeeded proves the seed
// record is nil exactly when there is nothing to clean up: an empty spec,
// and a home-rooted cwd the seeder must refuse.
func TestSeedWorkspaceTrust_ReturnsNilWhenNothingIsSeeded(t *testing.T) {
	// No t.Parallel: Setenv forbids it.
	if seed := seedWorkspaceTrust(agent.Spec{}); seed != nil {
		t.Errorf("seedWorkspaceTrust(empty) = %+v, want nil — nothing seeded, nothing to clean", seed)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cfg := t.TempDir()
	if seed := seedWorkspaceTrust(agent.Spec{
		Cwd: home,
		Env: map[string]string{claudeConfigDirEnv: cfg},
	}); seed != nil {
		t.Errorf("seedWorkspaceTrust(home cwd) = %+v, want nil — a refused seed cleans up nothing", seed)
	}
}

// TestSeedThenRemove_NeverRemovesTrustThatPredatesTheSeed pins the
// ownership rule at the SAME path the session seeds: an entry that was
// already trusted before the seed (the operator's own project entry, with
// its per-project fields) is not the seed's, so the session-end cleanup
// leaves it exactly as found. An entry that existed WITHOUT trust gets only
// the trust key put back to its prior state; its other fields stay. RED
// proof: record every boundary dir as seeded (the earlier behaviour) and
// the operator's entries are deleted whole.
func TestSeedThenRemove_NeverRemovesTrustThatPredatesTheSeed(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ".claude.json")
	writeTrustRecord(t, path, map[string]any{
		"/operator/trusted":            map[string]any{claudeTrustAcceptedKey: true, "allowedTools": []any{"Bash(make:*)"}},
		"/operator/untrusted-explicit": map[string]any{claudeTrustAcceptedKey: false, "history": []any{"h"}},
		"/operator/untrusted-implicit": map[string]any{"history": []any{"h"}},
	})
	seed := seedTrustFile(path, []string{
		"/operator/trusted", "/operator/untrusted-explicit", "/operator/untrusted-implicit", "/session/new",
	})
	if seed == nil {
		t.Fatal("seed wrote nothing")
	}
	mid := readTrustProjects(t, path)
	for _, dir := range []string{"/operator/trusted", "/operator/untrusted-explicit", "/operator/untrusted-implicit", "/session/new"} {
		if !trustedEntry(mid, dir) {
			t.Fatalf("seed did not trust %s: %v", dir, mid)
		}
	}
	removeSeededTrust(seed)
	projects := readTrustProjects(t, path)

	trusted, ok := projects["/operator/trusted"].(map[string]any)
	if !ok || trusted[claudeTrustAcceptedKey] != true || trusted["allowedTools"] == nil {
		t.Errorf("trust that predates the seed was removed or disturbed: %v", projects)
	}
	explicit, ok := projects["/operator/untrusted-explicit"].(map[string]any)
	if !ok || explicit[claudeTrustAcceptedKey] != false || explicit["history"] == nil {
		t.Errorf("flipped entry not restored to its prior state: %v", projects)
	}
	implicit, ok := projects["/operator/untrusted-implicit"].(map[string]any)
	if !ok || implicit["history"] == nil {
		t.Errorf("flipped entry lost its own fields: %v", projects)
	}
	if _, has := implicit[claudeTrustAcceptedKey]; has {
		t.Errorf("seeded trust key survives on a flipped entry: %v", implicit)
	}
	if _, present := projects["/session/new"]; present {
		t.Errorf("entry the seed created survives cleanup: %v", projects)
	}
}

// TestSeedTrustFile_AllAlreadyTrustedWritesNothing proves a seed that adds
// nothing returns nil (nothing for the cleanup to undo) and leaves the
// record byte-for-byte.
func TestSeedTrustFile_AllAlreadyTrustedWritesNothing(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ".claude.json")
	writeTrustRecord(t, path, map[string]any{
		"/operator/trusted": map[string]any{claudeTrustAcceptedKey: true},
	})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if seed := seedTrustFile(path, []string{"/operator/trusted"}); seed != nil {
		t.Errorf("seedTrustFile(already trusted) = %+v, want nil", seed)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("record rewritten although nothing was added:\nbefore %s\nafter  %s", before, after)
	}
}
