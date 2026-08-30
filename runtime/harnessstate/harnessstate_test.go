package harnessstate

import (
	"sort"
	"strings"
	"testing"
)

// TestDirs_ContractOfTheTable pins the properties every consumer relies on,
// so a future row cannot quietly break one of them.
func TestDirs_ContractOfTheTable(t *testing.T) {
	t.Parallel()

	names := Dirs()
	if len(names) == 0 {
		t.Fatal("no state directories declared")
	}

	seen := make(map[string]bool, len(names))
	for _, n := range names {
		switch {
		case n == "":
			t.Error("empty directory name in the table")
		case !strings.HasPrefix(n, "."):
			t.Errorf("%q is not dot-prefixed — a state dir that looks like project content will be committed", n)
		case strings.ContainsAny(n, `/\`):
			t.Errorf("%q contains a path separator; entries are TOP-LEVEL directory names", n)
		case seen[n]:
			t.Errorf("%q is declared twice", n)
		}
		seen[n] = true
	}

	if !sort.StringsAreSorted(names) {
		t.Errorf("Dirs() is not sorted: %v", names)
	}

	// The list is the caller's to keep: mutating it must not corrupt the table.
	names[0] = "mutated"
	if Dirs()[0] == "mutated" {
		t.Fatal("Dirs() handed out the package's own slice")
	}
}

// TestDirs_CoversTheKnownHarnessState is the one place the concrete set is
// asserted. Every other test is written against Dirs(), so this is what makes
// a REMOVAL visible rather than silently narrowing the guarantee everywhere.
func TestDirs_CoversTheKnownHarnessState(t *testing.T) {
	t.Parallel()
	want := []string{".agent", ".claude", ".codex", ".pi"}
	got := Dirs()
	if len(got) != len(want) {
		t.Fatalf("Dirs() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Dirs() = %v, want %v", got, want)
		}
	}
	// `.donmai` is repo-tracked configuration, not harness state. Excluding it
	// would hide a session's legitimate edits to it.
	if IsStateDir(".donmai") {
		t.Error(".donmai must not be treated as harness state — it is tracked project configuration")
	}
}

// TestExcludeEntries_AreDirectoryPatterns proves each entry is a directory
// pattern. Without the trailing slash the pattern would also match a FILE of
// the same name.
func TestExcludeEntries_AreDirectoryPatterns(t *testing.T) {
	t.Parallel()
	entries := ExcludeEntries()
	if len(entries) != len(Dirs()) {
		t.Fatalf("ExcludeEntries() has %d entries for %d dirs", len(entries), len(Dirs()))
	}
	for i, e := range entries {
		if !strings.HasSuffix(e, "/") {
			t.Errorf("entry %q is not a directory pattern", e)
		}
		if strings.TrimSuffix(e, "/") != Dirs()[i] {
			t.Errorf("entry %q does not correspond to dir %q", e, Dirs()[i])
		}
	}
}

// TestIsStateDir_MatchesWholeNames proves a prefix-sharing directory is not
// mistaken for harness state.
func TestIsStateDir_MatchesWholeNames(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		".agent":       true,
		".pi":          true,
		".claude":      true,
		".codex":       true,
		".pi-cache":    false,
		".agentfoo":    false,
		"pi":           false,
		"agent":        false,
		".donmai":      false,
		"":             false,
		"src/.pi":      false,
		".claude.json": false,
	}
	for name, want := range cases {
		if got := IsStateDir(name); got != want {
			t.Errorf("IsStateDir(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestAll_CarriesProvenance proves every row documents who writes it and why
// it matters — the information a future reader needs to decide whether
// removing the row is safe.
func TestAll_CarriesProvenance(t *testing.T) {
	t.Parallel()
	for _, d := range All() {
		if strings.TrimSpace(d.Owner) == "" {
			t.Errorf("%s: no Owner declared", d.Name)
		}
		if strings.TrimSpace(d.Why) == "" {
			t.Errorf("%s: no Why declared", d.Name)
		}
	}
}
