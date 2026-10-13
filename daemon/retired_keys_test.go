package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedRetiredKeyConfig writes a config file carrying the retired key on
// several entries, with surrounding content (comments, other blocks, mixed
// indentation) that the sweep must preserve byte-for-byte.
func seedRetiredKeyConfig(t *testing.T, path string, mode os.FileMode) string {
	t.Helper()
	body := `# operator header comment — must survive the sweep
apiVersion: donmai.dev/v1
kind: LocalDaemon
machine:
  id: sweep-fixture
  region: local
capacity:
  maxConcurrentSessions: 2
projects:
  - id: one
    repository: github.com/foo/one
    cloneStrategy: shallow
  - id: two
    repository: github.com/foo/two
    cloneStrategy: full # trailing comment on the retired line
repositories:
  - id: repo-one
    projectId: one
    source: github.com/foo/one
    cloneStrategy: shallow
orchestrator:
  url: https://platform.example.com
`
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	return body
}

// remainingLines returns every source line except the retired key's own
// lines — the exact bytes the sweep must leave behind.
func remainingLines(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, ln := range strings.SplitAfter(body, "\n") {
		if strings.Contains(ln, "cloneStrategy") {
			continue
		}
		out = append(out, ln)
	}
	return out
}

func TestSweepRetiredKeys_StripsAllFilesPreservesModeAndBytes(t *testing.T) {
	dir := t.TempDir()
	primary := filepath.Join(dir, "daemon.yaml")
	secondary := filepath.Join(dir, "daemon-acme.yaml")
	primaryBody := seedRetiredKeyConfig(t, primary, 0o600)
	secondaryBody := seedRetiredKeyConfig(t, secondary, 0o640)

	summary, err := SweepRetiredKeys([]string{primary, secondary})
	if err != nil {
		t.Fatalf("SweepRetiredKeys: %v", err)
	}
	if summary.FilesChecked() != 2 {
		t.Errorf("FilesChecked = %d, want 2", summary.FilesChecked())
	}
	if got := summary.FilesCleaned(); got != 2 {
		t.Errorf("FilesCleaned = %d, want 2", got)
	}
	if got := summary.KeysRemoved(); got != 6 {
		t.Errorf("KeysRemoved = %d, want 6 (3 entries x 2 files)", got)
	}
	if line := summary.Line(); !strings.Contains(line, "6 retired key(s)") || !strings.Contains(line, "2 of 2") {
		t.Errorf("summary line should name the counts, got %q", line)
	}

	for _, tc := range []struct {
		path string
		body string
		mode os.FileMode
	}{
		{primary, primaryBody, 0o600},
		{secondary, secondaryBody, 0o640},
	} {
		raw, err := os.ReadFile(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "cloneStrategy") {
			t.Errorf("%s still carries the retired key:\n%s", tc.path, raw)
		}
		if string(raw) != strings.Join(remainingLines(t, tc.body), "") {
			t.Errorf("%s: surviving bytes changed.\n got:\n%s\nwant:\n%s", tc.path, raw, strings.Join(remainingLines(t, tc.body), ""))
		}
		if got := osFileMode(t, tc.path); got != tc.mode {
			t.Errorf("%s: mode = %o, want %o", tc.path, got, tc.mode)
		}
		// The header comment and every other block survive verbatim.
		for _, want := range []string{"# operator header comment", "maxConcurrentSessions: 2", "url: https://platform.example.com", "region: local"} {
			if !strings.Contains(string(raw), want) {
				t.Errorf("%s lost %q:\n%s", tc.path, want, raw)
			}
		}
		// The stripped file still loads.
		cfg, err := LoadConfig(tc.path)
		if err != nil {
			t.Errorf("LoadConfig(%s) after sweep: %v", tc.path, err)
		} else if len(cfg.Projects) != 2 || len(cfg.Repositories) != 2 {
			t.Errorf("LoadConfig(%s) = %d projects, %d repositories; want 2, 2", tc.path, len(cfg.Projects), len(cfg.Repositories))
		}
	}

	// A backup holding the pre-strip content sits alongside each file.
	for _, path := range []string{primary, secondary} {
		matches, err := filepath.Glob(path + ".sweep-backup-*")
		if err != nil || len(matches) != 1 {
			t.Fatalf("%s: backups = %v, err = %v; want exactly 1", path, matches, err)
		}
		backup, err := os.ReadFile(matches[0])
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(backup), "cloneStrategy") {
			t.Errorf("backup %s should hold the pre-strip content:\n%s", matches[0], backup)
		}
	}
}

func TestSweepRetiredKeys_IsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")
	seedRetiredKeyConfig(t, path, 0o600)

	first, err := SweepRetiredKeys([]string{path})
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if first.KeysRemoved() == 0 {
		t.Fatal("first sweep removed nothing; fixture is wrong")
	}
	afterFirst, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	backups := func() []string {
		matches, err := filepath.Glob(path + ".sweep-backup-*")
		if err != nil {
			t.Fatal(err)
		}
		return matches
	}
	if len(backups()) != 1 {
		t.Fatalf("backups after first sweep = %d, want 1", len(backups()))
	}

	second, err := SweepRetiredKeys([]string{path})
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if second.KeysRemoved() != 0 || second.FilesCleaned() != 0 {
		t.Errorf("second sweep = %+v; want a no-op", second)
	}
	if line := second.Line(); !strings.Contains(line, "no retired keys") {
		t.Errorf("idempotent summary should say nothing was found, got %q", line)
	}
	afterSecond, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterSecond) != string(afterFirst) {
		t.Errorf("second sweep rewrote the file.\nfirst:\n%s\nsecond:\n%s", afterFirst, afterSecond)
	}
	if len(backups()) != 1 {
		t.Errorf("backups after second sweep = %d, want still 1 (no new backup)", len(backups()))
	}
}

func TestSweepRetiredKeys_LeavesCleanAndMissingFilesAlone(t *testing.T) {
	dir := t.TempDir()
	clean := filepath.Join(dir, "daemon.yaml")
	if err := os.WriteFile(clean, []byte("machine:\n  id: clean\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(clean)
	if err != nil {
		t.Fatal(err)
	}

	summary, err := SweepRetiredKeys([]string{clean, filepath.Join(dir, "daemon-absent.yaml")})
	if err != nil {
		t.Fatalf("SweepRetiredKeys: %v", err)
	}
	if summary.FilesChecked() != 1 {
		t.Errorf("FilesChecked = %d, want 1 (missing candidate is a silent skip)", summary.FilesChecked())
	}
	if summary.KeysRemoved() != 0 {
		t.Errorf("KeysRemoved = %d, want 0", summary.KeysRemoved())
	}
	after, err := os.ReadFile(clean)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("clean file was rewritten:\n%s", after)
	}
	if matches, _ := filepath.Glob(clean + ".sweep-backup-*"); len(matches) != 0 {
		t.Errorf("clean file gained a backup: %v", matches)
	}
}

func TestSweepRetiredKeys_SkipsUnsafeFileUntouched(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")
	// A block-scalar value cannot be removed by deleting one line; the
	// sweep must refuse the file rather than corrupt it.
	body := "projects:\n- id: one\n  repository: github.com/foo/one\n  cloneStrategy: |\n    shallow\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	summary, err := SweepRetiredKeys([]string{path})
	if err == nil {
		t.Fatal("expected a skip error for the non-inline value, got nil")
	}
	if summary.KeysRemoved() != 0 {
		t.Errorf("KeysRemoved = %d, want 0", summary.KeysRemoved())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != body {
		t.Errorf("unsafe file was modified:\n%s", raw)
	}
}

// sweepRefusalHeader is the shared top-level block every refusal fixture
// carries so the post-skip LoadConfig assertion exercises a valid file.
const sweepRefusalHeader = "machine:\n  id: sweep-fixture\norchestrator:\n  url: https://platform.example.com\n"

// TestSweepRetiredKeys_RefusesWholeEntryRemoval pins the safe shapes for
// line deletion: a retired key that shares its source line with any other
// key cannot be removed by deleting that line, so the whole file is skipped
// and left byte-identical. That covers a flow-style entry such as
// `- {id: alpha, cloneStrategy: full}` (deleting its line would drop the
// whole entry, not just the key) and a block entry whose key sits on the
// `- ` line. One table, one decision function, no shape left unpinned.
func TestSweepRetiredKeys_RefusesWholeEntryRemoval(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		body string
	}{
		"flow-style entry": {
			body: sweepRefusalHeader +
				"projects:\n" +
				"  - {id: alpha, repository: github.com/o/a, cloneStrategy: full}\n" +
				"  - id: beta\n" +
				"    repository: github.com/o/b\n",
		},
		"flow-style sequence": {
			body: sweepRefusalHeader +
				"projects: [{id: alpha, repository: github.com/o/a, cloneStrategy: full}]\n",
		},
		"key first on the dash line": {
			body: sweepRefusalHeader +
				"projects:\n" +
				"  - cloneStrategy: full\n" +
				"    id: alpha\n" +
				"    repository: github.com/o/a\n" +
				"  - id: beta\n" +
				"    repository: github.com/o/b\n",
		},
		// A quoted flow fragment on its own line would be the entry's
		// only line for that mapping, so the dash-line rule refuses it.
		// Any other same-line key sharing needs flow braces (already
		// refused above), so this pins the degenerate survivor: an
		// entry whose retired key IS its only content.
		"entry holding only the retired key": {
			body: sweepRefusalHeader +
				"projects:\n" +
				"  - cloneStrategy: full\n" +
				"  - id: beta\n" +
				"    repository: github.com/o/b\n",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "daemon.yaml")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			summary, err := SweepRetiredKeys([]string{path})
			if err == nil {
				t.Fatal("expected a skip error for the unsafe shape, got nil")
			}
			if summary.KeysRemoved() != 0 {
				t.Errorf("KeysRemoved = %d, want 0 (file must be skipped)", summary.KeysRemoved())
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != tc.body {
				t.Errorf("unsafe file was modified.\n got:\n%s\nwant:\n%s", raw, tc.body)
			}
			if matches, _ := filepath.Glob(path + ".sweep-backup-*"); len(matches) != 0 {
				t.Errorf("unsafe file gained a backup: %v", matches)
			}
			// The entries still decode with every field intact: the raw
			// tree must hold the same projects entry count and the
			// same non-retired keys per entry as before the sweep ran.
			// (LoadConfig itself rejects some of these fixtures — an
			// id-less entry is invalid — so the pin is on the raw
			// shape, which is what the sweep reasons about.)
			before, err := retiredEntryShapes([]byte(tc.body))
			if err != nil {
				t.Fatalf("retiredEntryShapes(before): %v", err)
			}
			after, err := retiredEntryShapes(raw)
			if err != nil {
				t.Fatalf("retiredEntryShapes(after): %v", err)
			}
			if len(after) != len(before) {
				t.Errorf("entry count changed: %d before, %d after", len(before), len(after))
			}
			if !strings.Contains(string(raw), "cloneStrategy") {
				t.Errorf("refused sweep should leave the retired key in place for a hand edit:\n%s", raw)
			}
		})
	}
}

// TestSweepRetiredKeys_UnsafeMixedWithSafeSkipsWholeFile proves the skip is
// per file, not per entry: one unsafe occurrence holds the entire file back
// (leaving even the safe entries' keys in place) rather than rewriting a
// file the sweep cannot verify end to end.
func TestSweepRetiredKeys_UnsafeMixedWithSafeSkipsWholeFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")
	body := sweepRefusalHeader +
		"projects:\n" +
		"  - id: safe\n" +
		"    repository: github.com/o/safe\n" +
		"    cloneStrategy: shallow\n" +
		"  - {id: flowed, repository: github.com/o/flowed, cloneStrategy: full}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	summary, err := SweepRetiredKeys([]string{path})
	if err == nil {
		t.Fatal("expected a skip error when any occurrence is unsafe, got nil")
	}
	if summary.KeysRemoved() != 0 {
		t.Errorf("KeysRemoved = %d, want 0", summary.KeysRemoved())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != body {
		t.Errorf("mixed file was modified.\n got:\n%s\nwant:\n%s", raw, body)
	}
}

func TestDefaultSweepPaths_CoversSiblings(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	stateDir := filepath.Join(dir, ".donmai")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	primary := filepath.Join(stateDir, "daemon.yaml")
	secondary := filepath.Join(stateDir, "daemon-acme.yaml")
	for _, p := range []string{primary, secondary} {
		if err := os.WriteFile(p, []byte("machine:\n  id: x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	paths := DefaultSweepPaths("")
	found := make(map[string]bool)
	for _, p := range paths {
		found[p] = true
	}
	if !found[primary] || !found[secondary] {
		t.Errorf("DefaultSweepPaths(%q) = %v; want both daemon*.yaml siblings", "", paths)
	}
	explicit := filepath.Join(dir, "custom.yaml")
	if err := os.WriteFile(explicit, []byte("machine:\n  id: y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths = DefaultSweepPaths(explicit)
	found = make(map[string]bool)
	for _, p := range paths {
		found[p] = true
	}
	if !found[explicit] {
		t.Errorf("DefaultSweepPaths(explicit) = %v; want the explicit path included", paths)
	}
}

func osFileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
