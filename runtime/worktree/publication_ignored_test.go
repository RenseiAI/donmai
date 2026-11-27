package worktree

import "testing"

// TestHasMeaningfulIgnoredFilesScopesSessionDocuments pins the interactive
// publication check's ignored-bytes rule. Without a session document
// directory, every ignored .workarea/ path (top level or nested) is
// meaningful, exactly as before negotiated documents existed. With one, only
// that exact directory is session machinery; a sibling pack, other .workarea
// content and a nested sub/.workarea/ copy still retain the checkout.
//
// RED (with the SessionDocuments skip removed): a flat carrier session's own
// published pack retains every interactive checkout as dirty.
func TestHasMeaningfulIgnoredFilesScopesSessionDocuments(t *testing.T) {
	t.Parallel()
	const pack = ".workarea/context/pack1/"
	cases := []struct {
		name             string
		ignored          string
		sessionDocuments string
		want             bool
	}{
		{"harness state only", ".agent/state.json\x00", "", false},
		{"legacy top-level workarea", ".workarea/context/pack1/brief.md\x00", "", true},
		{"legacy nested workarea", "sub/.workarea/context/pack1/brief.md\x00", "", true},
		{"own pack", ".workarea/context/pack1/brief.md\x00.workarea/context/pack1/pack.json\x00.workarea/context/pack1/.gitignore\x00", pack, false},
		{"own pack plus harness state", ".workarea/context/pack1/brief.md\x00.pi/session.jsonl\x00", pack, false},
		{"sibling pack", ".workarea/context/pack10/brief.md\x00", pack, true},
		{"other workarea content", ".workarea/project-state.txt\x00", pack, true},
		{"nested copy of the pack path", "sub/.workarea/context/pack1/brief.md\x00", pack, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := hasMeaningfulIgnoredFiles([]byte(tc.ignored), tc.sessionDocuments); got != tc.want {
				t.Fatalf("hasMeaningfulIgnoredFiles(%q, %q) = %v; want %v", tc.ignored, tc.sessionDocuments, got, tc.want)
			}
		})
	}
}
