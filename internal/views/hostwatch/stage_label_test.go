package hostwatch

import (
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/stagewire"
)

// TestSessionCardWorkLabel pins the work label a live run card shows: the
// wire stage's "<name> · <role>" when the work item carries a stage, else the
// work type. Stage text is untrusted terminal output.
func TestSessionCardWorkLabel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		card SessionCard
		want string
	}{
		{
			name: "stage name and role",
			card: SessionCard{WorkType: "development", StageName: "Verify", StageRole: "review"},
			want: "Verify · review",
		},
		{
			name: "dispatch role wins over the run role",
			card: SessionCard{WorkType: "development", StageName: "Backlog", StageRole: "intake", StageRunRole: "inflight"},
			want: "Backlog · intake",
		},
		{
			name: "run role fills a missing dispatch role",
			card: SessionCard{WorkType: "development", StageName: "Build", StageRunRole: "inflight"},
			want: "Build · inflight",
		},
		{
			name: "name without a role",
			card: SessionCard{WorkType: "development", StageName: "Triage"},
			want: "Triage",
		},
		{
			name: "no stage falls back to the work type",
			card: SessionCard{WorkType: "qa"},
			want: "qa",
		},
		{
			name: "a role without a name falls back to the work type",
			card: SessionCard{WorkType: "qa", StageRole: "review", StageRunRole: "review"},
			want: "qa",
		},
		{
			name: "neither stage nor work type is said plainly",
			card: SessionCard{},
			want: "work type unknown",
		},
		{
			name: "escape sequences and control characters are stripped",
			card: SessionCard{WorkType: "qa", StageName: "Ver\x1b[31mify\x1b]0;pwned\x07\nnow\t!\x00", StageRole: "re\x7fview"},
			want: "Verify now ! · re view",
		},
		{
			name: "bidi overrides, isolates and marks are stripped",
			card: SessionCard{WorkType: "qa", StageName: "\u202eVerify\u202c \u2066x\u2069\u200e\u200f\u061c", StageRole: "\u202ereview"},
			want: "Verify x · review",
		},
		{
			name: "zero-width and byte-order characters are stripped",
			card: SessionCard{WorkType: "qa", StageName: "Ver\u200bify\ufeff\u2060"},
			want: "Verify",
		},
		{
			name: "a name that sanitizes to nothing falls back to the work type",
			card: SessionCard{WorkType: "qa", StageName: "\u202e\x1b[31m\n\u200b", StageRole: "review"},
			want: "qa",
		},
		{
			name: "a role that sanitizes to nothing is dropped, not the name",
			card: SessionCard{WorkType: "qa", StageName: "Verify", StageRole: "\u202e\n", StageRunRole: "\u200b"},
			want: "Verify",
		},
		{
			name: "a long name is bounded",
			card: SessionCard{WorkType: "qa", StageName: strings.Repeat("n", 500), StageRole: "review"},
			want: strings.Repeat("n", maxStageNameRunes-1) + "…" + " · review",
		},
		{
			name: "a long role is bounded",
			card: SessionCard{WorkType: "qa", StageName: "Verify", StageRole: strings.Repeat("r", 500)},
			want: "Verify · " + strings.Repeat("r", maxStageRoleRunes-1) + "…",
		},
		{
			name: "a wide-character name is bounded by runes",
			card: SessionCard{WorkType: "qa", StageName: strings.Repeat("検", 500)},
			want: strings.Repeat("検", maxStageNameRunes-1) + "…",
		},
		{
			name: "the stage label names the dispatched card",
			card: SessionCard{WorkType: "development", StageName: "Persona", StageRole: "intake", CardHandle: "builder"},
			want: "Persona · intake (builder)",
		},
		{
			name: "the work type fallback names the dispatched card",
			card: SessionCard{WorkType: "qa", CardHandle: "reviewer"},
			want: "qa (reviewer)",
		},
		{
			name: "a stage without a card is unchanged",
			card: SessionCard{WorkType: "development", StageName: "Persona", StageRole: "intake"},
			want: "Persona · intake",
		},
		{
			name: "escape sequences and control characters are stripped from the handle",
			card: SessionCard{WorkType: "qa", StageName: "Verify", StageRole: "review", CardHandle: "\u202ebui\x1b[31mld\x1b]0;pwned\x07er\x00\u200b"},
			want: "Verify · review (builder)",
		},
		{
			name: "a handle that sanitizes to nothing is dropped",
			card: SessionCard{WorkType: "qa", StageName: "Verify", StageRole: "review", CardHandle: "\u202e\x1b[31m\n\u200b"},
			want: "Verify · review",
		},
		{
			name: "a long handle is bounded",
			card: SessionCard{WorkType: "qa", StageName: "Verify", CardHandle: strings.Repeat("h", 500)},
			want: "Verify (" + strings.Repeat("h", maxCardHandleRunes-1) + "…)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.card.workLabel()
			if got != tc.want {
				t.Fatalf("workLabel() = %q; want %q", got, tc.want)
			}
			for _, r := range got {
				if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == utf8.RuneError {
					t.Fatalf("workLabel() = %q carries unsafe rune %U", got, r)
				}
			}
			if bound := maxStageNameRunes + len(" · ") + maxStageRoleRunes + len(" ()") + maxCardHandleRunes; utf8.RuneCountInString(got) > bound && tc.card.StageName != "" {
				t.Fatalf("workLabel() has %d runes; bound is %d", utf8.RuneCountInString(got), bound)
			}
		})
	}
}

// TestRenderCardShowsStageLabel renders the run card's project context row
// for a card with and without a stage, in plain and framed output.
func TestRenderCardShowsStageLabel(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 13, 14, 5, 0, 0, time.UTC)
	tests := []struct {
		name     string
		mutate   func(*SessionCard)
		wantRow  string
		wantNone []string
	}{
		{
			name:     "stage replaces the work type",
			mutate:   func(c *SessionCard) { c.StageName, c.StageRole = "Verify", "review" },
			wantRow:  "web · Verify · review",
			wantNone: []string{"development", "("},
		},
		{
			name:     "no stage keeps the work type",
			mutate:   func(_ *SessionCard) {},
			wantRow:  "web · development",
			wantNone: []string{"("},
		},
		{
			name: "a stage with a card names the card",
			mutate: func(c *SessionCard) {
				c.StageName, c.StageRole, c.CardHandle = "Persona", "intake", "builder"
			},
			wantRow:  "web · Persona · intake (builder)",
			wantNone: []string{"development"},
		},
		{
			name: "hostile card handle text never reaches the row",
			mutate: func(c *SessionCard) {
				c.StageName, c.StageRole, c.CardHandle = "Verify", "review", "\u202ebui\x1b[2Jlder\x07"
			},
			wantRow:  "web · Verify · review (builder)",
			wantNone: []string{"\u202e", "\x1b[2J", "\x07"},
		},
		{
			name:     "hostile stage text never reaches the row",
			mutate:   func(c *SessionCard) { c.StageName, c.StageRole = "\u202eVer\x1b[2Jify\x07", "review" },
			wantRow:  "web · Verify · review",
			wantNone: []string{"\u202e", "\x1b[2J", "\x07"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			card := fullCard(now)
			tc.mutate(&card)
			for _, plain := range []bool{true, false} {
				rows := cardText(card, false, plain, now, 60)
				if !plain {
					rows = rows[1 : len(rows)-1] // drop the frame rows
				}
				if !strings.Contains(rows[1], tc.wantRow) {
					t.Errorf("plain=%v context row = %q; want it to contain %q", plain, rows[1], tc.wantRow)
				}
				for _, none := range tc.wantNone {
					if strings.Contains(strings.Join(rows, "\n"), none) {
						t.Errorf("plain=%v card carries %q:\n%s", plain, none, strings.Join(rows, "\n"))
					}
				}
			}
		})
	}
}

// TestSourceSnapshotCarriesStage carries the session handle's wire stage and
// card into the card model, and leaves a legacy handle's fields empty.
func TestSourceSnapshotCarriesStage(t *testing.T) {
	t.Parallel()
	fd := &fakeDaemon{sessions: []afclient.DaemonSessionHandle{
		{
			SessionID: "staged", State: "running", WorkType: "development",
			Stage: &stagewire.Stage{ID: "stg", Key: "backlog", Name: "Backlog", Role: stagewire.RoleIntake, RunRole: stagewire.RoleInflight},
			Card:  &stagewire.Card{ID: "card_1", Handle: "builder"},
		},
		{SessionID: "legacy", State: "running", WorkType: "qa"},
	}}
	snap := NewSource(fd, &noopState{}, "").Snapshot()
	if len(snap.Cards) != 2 {
		t.Fatalf("want 2 cards, got %d", len(snap.Cards))
	}
	bySession := map[string]SessionCard{}
	for _, c := range snap.Cards {
		bySession[c.SessionID] = c
	}
	staged := bySession["staged"]
	if staged.StageName != "Backlog" || staged.StageRole != "intake" || staged.StageRunRole != "inflight" {
		t.Fatalf("staged card stage = %q/%q/%q; want Backlog/intake/inflight", staged.StageName, staged.StageRole, staged.StageRunRole)
	}
	if staged.CardHandle != "builder" {
		t.Fatalf("staged card handle = %q; want builder", staged.CardHandle)
	}
	if got, want := staged.workLabel(), "Backlog · intake (builder)"; got != want {
		t.Fatalf("staged workLabel() = %q; want %q", got, want)
	}
	legacy := bySession["legacy"]
	if legacy.StageName != "" || legacy.StageRole != "" || legacy.StageRunRole != "" || legacy.CardHandle != "" {
		t.Fatalf("legacy card gained stage fields: %+v", legacy)
	}
	if got, want := legacy.workLabel(), "qa"; got != want {
		t.Fatalf("legacy workLabel() = %q; want %q", got, want)
	}
}

// TestCardMatchesFilterStage finds a run by its stage label.
func TestCardMatchesFilterStage(t *testing.T) {
	t.Parallel()
	card := SessionCard{SessionID: "s", WorkType: "development", StageName: "Verify", StageRole: "review", CardHandle: "Builder"}
	for _, query := range []string{"verify", "review", "development", "builder"} {
		if !cardMatchesFilter(card, query) {
			t.Errorf("cardMatchesFilter(%q) = false; want true", query)
		}
	}
	if cardMatchesFilter(card, "nomatch") {
		t.Error("cardMatchesFilter(nomatch) = true; want false")
	}
	hostile := SessionCard{SessionID: "s", StageName: "Ver\u202eify"}
	if !cardMatchesFilter(hostile, "verify") {
		t.Error("filter should match the sanitized stage name")
	}
}

// TestDetailFieldsStage lists the wire stage and run role in the session
// detail only when the work item carries them.
func TestDetailFieldsStage(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 13, 14, 5, 0, 0, time.UTC)
	tests := []struct {
		name         string
		mutate       func(*SessionCard)
		wantStage    string
		wantRunRole  string
		wantFieldNum int
	}{
		{
			name:         "stage with both roles",
			mutate:       func(c *SessionCard) { c.StageName, c.StageRole, c.StageRunRole = "Backlog", "intake", "inflight" },
			wantStage:    "Backlog · intake",
			wantRunRole:  "inflight",
			wantFieldNum: 2,
		},
		{
			name:         "hostile stage text is sanitized",
			mutate:       func(c *SessionCard) { c.StageName, c.StageRunRole = "\u202eVer\x1b[2Jify", "in\nflight" },
			wantStage:    "Verify · in flight",
			wantRunRole:  "in flight",
			wantFieldNum: 2,
		},
		{
			name:   "legacy session gains no rows",
			mutate: func(_ *SessionCard) {},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			legacy := len(detailFields(fullCard(now), now))
			card := fullCard(now)
			tc.mutate(&card)
			fields := detailFields(card, now)
			if got, want := len(fields), legacy+tc.wantFieldNum; got != want {
				t.Fatalf("detail has %d fields; want %d", got, want)
			}
			if got := detailValue(fields, "Stage"); got != tc.wantStage {
				t.Errorf("Stage = %q; want %q", got, tc.wantStage)
			}
			if got := detailValue(fields, "Run role"); got != tc.wantRunRole {
				t.Errorf("Run role = %q; want %q", got, tc.wantRunRole)
			}
		})
	}
}
