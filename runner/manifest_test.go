package runner

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/runtime/state"
)

// writeManifestFile writes raw to <dir>/.agent/turn-result.json, creating the
// .agent dir. Returns the worktree dir (the parent of .agent).
func writeManifestFile(t *testing.T, dir, raw string) {
	t.Helper()
	agentDir := filepath.Join(dir, state.AgentDirName)
	if err := os.MkdirAll(agentDir, 0o750); err != nil {
		t.Fatalf("mkdir .agent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, ManifestFileName), []byte(raw), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func TestParseManifest(t *testing.T) {
	tests := []struct {
		name string
		// raw is the manifest file content. When writeFile is false the file is
		// not created at all (exercises the ErrNoManifest path).
		raw       string
		writeFile bool
		wantErr   bool
		// wantNoManifest asserts the error is ErrNoManifest specifically (the
		// benign "agent didn't write one" case, distinct from a real failure).
		wantNoManifest bool
		want           *TurnManifest
	}{
		{
			name:      "valid passed with PR",
			writeFile: true,
			raw:       `{"schemaVersion":1,"verdict":"passed","summary":"shipped it","pullRequestUrl":"https://github.com/o/r/pull/7","commitSha":"abc123"}`,
			want: &TurnManifest{
				SchemaVersion:  1,
				Verdict:        "passed",
				Summary:        "shipped it",
				PullRequestURL: "https://github.com/o/r/pull/7",
				CommitSHA:      "abc123",
			},
		},
		{
			name:      "valid minimal failed",
			writeFile: true,
			raw:       `{"schemaVersion":1,"verdict":"failed"}`,
			want:      &TurnManifest{SchemaVersion: 1, Verdict: "failed"},
		},
		{
			name:      "valid blocked with reason",
			writeFile: true,
			raw:       `{"schemaVersion":1,"verdict":"blocked","blockedReason":"spec is ambiguous"}`,
			want: &TurnManifest{
				SchemaVersion: 1,
				Verdict:       "blocked",
				BlockedReason: "spec is ambiguous",
			},
		},
		{
			name:           "no file is benign",
			writeFile:      false,
			wantErr:        true,
			wantNoManifest: true,
		},
		{
			name:      "malformed json is a real error",
			writeFile: true,
			raw:       `{"schemaVersion":1,"verdict":`,
			wantErr:   true,
		},
		{
			name:      "missing required verdict",
			writeFile: true,
			raw:       `{"schemaVersion":1}`,
			wantErr:   true,
		},
		{
			name:      "missing required schemaVersion",
			writeFile: true,
			raw:       `{"verdict":"passed"}`,
			wantErr:   true,
		},
		{
			name:      "unknown verdict rejected by schema enum",
			writeFile: true,
			raw:       `{"schemaVersion":1,"verdict":"maybe"}`,
			wantErr:   true,
		},
		{
			name:      "wrong type for schemaVersion",
			writeFile: true,
			raw:       `{"schemaVersion":"1","verdict":"passed"}`,
			wantErr:   true,
		},
		{
			name:      "unsupported future schema version",
			writeFile: true,
			raw:       `{"schemaVersion":99,"verdict":"passed"}`,
			wantErr:   true,
		},
		{
			name:      "empty object",
			writeFile: true,
			raw:       `{}`,
			wantErr:   true,
		},
		{
			name:      "not an object",
			writeFile: true,
			raw:       `["passed"]`,
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.writeFile {
				writeManifestFile(t, dir, tt.raw)
			}

			got, err := ParseManifest(dir)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseManifest() = %+v, want error", got)
				}
				if tt.wantNoManifest && !errors.Is(err, ErrNoManifest) {
					t.Fatalf("ParseManifest() err = %v, want ErrNoManifest", err)
				}
				if !tt.wantNoManifest && errors.Is(err, ErrNoManifest) {
					t.Fatalf("ParseManifest() err = ErrNoManifest, want a real failure")
				}
				if got != nil {
					t.Fatalf("ParseManifest() returned non-nil manifest with error: %+v", got)
				}
				return
			}

			if err != nil {
				t.Fatalf("ParseManifest() unexpected error: %v", err)
			}
			if *got != *tt.want {
				t.Fatalf("ParseManifest() = %+v, want %+v", *got, *tt.want)
			}
		})
	}
}

func TestParseManifest_EmptyWorktreePath(t *testing.T) {
	if _, err := ParseManifest(""); err == nil {
		t.Fatal("ParseManifest(\"\") = nil error, want error")
	}
}

// discardRunner returns a Runner whose logger discards everything — enough for
// applyTurnManifest, which only logs + mutates its arguments.
func discardRunner() *Runner {
	return &Runner{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestApplyTurnManifest(t *testing.T) {
	tests := []struct {
		name string
		// manifest file content; empty string ⇒ no file written.
		raw string
		// seed values on the Result + observation before applying.
		seedWorkResult string
		seedSummary    string
		seedPR         string
		seedCommitSHA  string
		// expectations after apply.
		wantWorkResult string
		wantSummary    string
		wantPR         string
		wantCommitSHA  string
		wantBlocked    bool
		wantBlockedRsn string
		wantManifest   bool // res.Manifest non-nil
	}{
		{
			name:           "no manifest leaves scraped marker untouched",
			raw:            "",
			seedWorkResult: "passed",
			seedSummary:    "scraped summary",
			wantWorkResult: "passed",
			wantSummary:    "scraped summary",
			wantManifest:   false,
		},
		{
			name:           "manifest verdict overrides scraped marker",
			raw:            `{"schemaVersion":1,"verdict":"failed","summary":"manifest summary"}`,
			seedWorkResult: "passed", // marker said passed; manifest says failed
			seedSummary:    "scraped summary",
			wantWorkResult: "failed",
			wantSummary:    "manifest summary",
			wantManifest:   true,
		},
		{
			name:           "manifest PR overrides and is preferred",
			raw:            `{"schemaVersion":1,"verdict":"passed","pullRequestUrl":"https://github.com/o/r/pull/9"}`,
			seedPR:         "https://github.com/o/r/pull/1",
			wantWorkResult: "passed",
			wantPR:         "https://github.com/o/r/pull/9",
			wantManifest:   true,
		},
		{
			name:           "blocked manifest feeds blocked signal, not workResult",
			raw:            `{"schemaVersion":1,"verdict":"blocked","blockedReason":"ambiguous spec"}`,
			seedWorkResult: "",
			wantWorkResult: "", // blocked is not a QA verdict
			wantBlocked:    true,
			wantBlockedRsn: "ambiguous spec",
			wantManifest:   true,
		},
		{
			name:           "runner commit sha wins over advisory manifest sha",
			raw:            `{"schemaVersion":1,"verdict":"passed","commitSha":"manifestsha"}`,
			seedCommitSHA:  "runnersha",
			wantWorkResult: "passed",
			wantCommitSHA:  "runnersha",
			wantManifest:   true,
		},
		{
			name:           "manifest sha adopted when runner has none",
			raw:            `{"schemaVersion":1,"verdict":"passed","commitSha":"manifestsha"}`,
			seedCommitSHA:  "",
			wantWorkResult: "passed",
			wantCommitSHA:  "manifestsha",
			wantManifest:   true,
		},
		{
			name:           "malformed manifest is a no-op fallback",
			raw:            `{"schemaVersion":1,"verdict":`,
			seedWorkResult: "passed",
			wantWorkResult: "passed",
			wantManifest:   false,
		},
	}

	r := discardRunner()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.raw != "" {
				writeManifestFile(t, dir, tt.raw)
			}

			res := &Result{Result: agent.Result{
				WorkResult:     tt.seedWorkResult,
				Summary:        tt.seedSummary,
				PullRequestURL: tt.seedPR,
				CommitSHA:      tt.seedCommitSHA,
			}}
			obs := &streamObservation{
				workResult:     tt.seedWorkResult,
				pullRequestURL: tt.seedPR,
			}

			r.applyTurnManifest(dir, QueuedWork{QueuedWork: prompt.QueuedWork{SessionID: "test-session"}}, res, obs)

			if res.WorkResult != tt.wantWorkResult {
				t.Errorf("WorkResult = %q, want %q", res.WorkResult, tt.wantWorkResult)
			}
			if res.Summary != tt.wantSummary {
				t.Errorf("Summary = %q, want %q", res.Summary, tt.wantSummary)
			}
			if res.PullRequestURL != tt.wantPR {
				t.Errorf("PullRequestURL = %q, want %q", res.PullRequestURL, tt.wantPR)
			}
			if res.CommitSHA != tt.wantCommitSHA {
				t.Errorf("CommitSHA = %q, want %q", res.CommitSHA, tt.wantCommitSHA)
			}
			if obs.blocked != tt.wantBlocked {
				t.Errorf("obs.blocked = %v, want %v", obs.blocked, tt.wantBlocked)
			}
			if obs.blockedReason != tt.wantBlockedRsn {
				t.Errorf("obs.blockedReason = %q, want %q", obs.blockedReason, tt.wantBlockedRsn)
			}
			if (res.Manifest != nil) != tt.wantManifest {
				t.Errorf("res.Manifest set = %v, want %v", res.Manifest != nil, tt.wantManifest)
			}
		})
	}
}

// TestManifestTypeAlias asserts at compile time that runner.TurnManifest and
// agent.TurnManifest are the SAME type (an alias, not a distinct named type) —
// the single-source-of-truth contract the wire carrier (agent.Result.Manifest)
// and the parse surface (ParseManifest) share. The bare assignment compiles
// only when the two are identical; a redeclared named type would require an
// explicit conversion and fail here. (TestApplyTurnManifest also relies on this
// identity when it assigns the parsed *TurnManifest onto res.Manifest.)
func TestManifestTypeAlias(_ *testing.T) {
	m := TurnManifest{SchemaVersion: 1, Verdict: "passed"}
	sink := func(agent.TurnManifest) {}
	sink(m) // compiles iff TurnManifest IS agent.TurnManifest
}
