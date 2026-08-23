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
	"github.com/RenseiAI/donmai/runtime/workarea"
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

func TestParseInlineManifest(t *testing.T) {
	tests := []struct {
		name    string
		message string
		// wantErr asserts ParseInlineManifest returns the ErrNoInlineManifest
		// sentinel (every degrade case maps to it).
		wantErr bool
		want    *TurnManifest
	}{
		{
			name:    "well-formed inline manifest recovered",
			message: `Done. Intended manifest: {"schemaVersion":1,"verdict":"passed","summary":"shipped it"}`,
			want:    &TurnManifest{SchemaVersion: 1, Verdict: "passed", Summary: "shipped it"},
		},
		{
			name:    "full inline manifest with PR and sha",
			message: `Intended manifest: {"schemaVersion":1,"verdict":"failed","summary":"tests red","pullRequestUrl":"https://github.com/o/r/pull/7","commitSha":"abc123"}`,
			want: &TurnManifest{
				SchemaVersion:  1,
				Verdict:        "failed",
				Summary:        "tests red",
				PullRequestURL: "https://github.com/o/r/pull/7",
				CommitSHA:      "abc123",
			},
		},
		{
			name: "brace inside summary string does not truncate",
			// The summary contains a `}` (and a `{`) inside the JSON string —
			// a naive first-`}` scan would truncate here. The literal-aware scan
			// must keep going to the real closing brace.
			message: `Intended manifest: {"schemaVersion":1,"verdict":"passed","summary":"refactored the func(x) { return x } body"}`,
			want:    &TurnManifest{SchemaVersion: 1, Verdict: "passed", Summary: "refactored the func(x) { return x } body"},
		},
		{
			name:    "escaped quote inside summary tolerated",
			message: `Intended manifest: {"schemaVersion":1,"verdict":"passed","summary":"used \"quoted\" }text{ inside"}`,
			want:    &TurnManifest{SchemaVersion: 1, Verdict: "passed", Summary: `used "quoted" }text{ inside`},
		},
		{
			name:    "trailing WORK_RESULT marker after JSON tolerated",
			message: "Intended manifest: {\"schemaVersion\":1,\"verdict\":\"passed\",\"summary\":\"ok\"}\n\nWORK_RESULT:passed",
			want:    &TurnManifest{SchemaVersion: 1, Verdict: "passed", Summary: "ok"},
		},
		{
			name:    "label case-insensitive",
			message: `INTENDED MANIFEST: {"schemaVersion":1,"verdict":"blocked","blockedReason":"ambiguous spec"}`,
			want:    &TurnManifest{SchemaVersion: 1, Verdict: "blocked", BlockedReason: "ambiguous spec"},
		},
		{
			name:    "label whitespace tolerance",
			message: "Intended    manifest\t:   {\"schemaVersion\":1,\"verdict\":\"passed\"}",
			want:    &TurnManifest{SchemaVersion: 1, Verdict: "passed"},
		},
		{
			name:    "nested object inside manifest stays balanced",
			message: `Intended manifest: {"schemaVersion":1,"verdict":"passed","summary":"a {nested {deeper}} note"}`,
			want:    &TurnManifest{SchemaVersion: 1, Verdict: "passed", Summary: "a {nested {deeper}} note"},
		},
		{
			name:    "no label is sentinel",
			message: `All done. WORK_RESULT:passed`,
			wantErr: true,
		},
		{
			name:    "empty message is sentinel",
			message: "",
			wantErr: true,
		},
		{
			name:    "label but no brace is sentinel",
			message: `Intended manifest: see above`,
			wantErr: true,
		},
		{
			name:    "unbalanced JSON is sentinel",
			message: `Intended manifest: {"schemaVersion":1,"verdict":"passed"`,
			wantErr: true,
		},
		{
			name:    "malformed JSON is sentinel",
			message: `Intended manifest: {"schemaVersion":1 "verdict" "passed"}`,
			wantErr: true,
		},
		{
			name:    "invalid verdict is sentinel",
			message: `Intended manifest: {"schemaVersion":1,"verdict":"maybe"}`,
			wantErr: true,
		},
		{
			name:    "missing schemaVersion is sentinel",
			message: `Intended manifest: {"verdict":"passed"}`,
			wantErr: true,
		},
		{
			name:    "wrong schemaVersion is sentinel",
			message: `Intended manifest: {"schemaVersion":99,"verdict":"passed"}`,
			wantErr: true,
		},
		{
			name:    "missing required verdict is sentinel",
			message: `Intended manifest: {"schemaVersion":1}`,
			wantErr: true,
		},
		{
			name:    "non-object json after label is sentinel",
			message: `Intended manifest: ["passed"]`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseInlineManifest(tt.message)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseInlineManifest() = %+v, want error", got)
				}
				if !errors.Is(err, ErrNoInlineManifest) {
					t.Fatalf("ParseInlineManifest() err = %v, want ErrNoInlineManifest", err)
				}
				if got != nil {
					t.Fatalf("ParseInlineManifest() returned non-nil manifest with error: %+v", got)
				}
				return
			}

			if err != nil {
				t.Fatalf("ParseInlineManifest() unexpected error: %v", err)
			}
			if *got != *tt.want {
				t.Fatalf("ParseInlineManifest() = %+v, want %+v", *got, *tt.want)
			}
		})
	}
}

// TestExtractBalancedJSONObject exercises the string-literal-aware brace scan in
// isolation — the load-bearing guarantee is that a `}` inside a JSON string
// value does not close the object early.
func TestExtractBalancedJSONObject(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{
		{name: "simple object", in: `{"a":1}`, want: `{"a":1}`, wantOK: true},
		{name: "leading prose then object", in: `noise {"a":1} trailing`, want: `{"a":1}`, wantOK: true},
		{name: "brace in string value", in: `{"s":"a } b"}`, want: `{"s":"a } b"}`, wantOK: true},
		{name: "open brace in string value", in: `{"s":"a { b"}`, want: `{"s":"a { b"}`, wantOK: true},
		{name: "nested objects", in: `{"o":{"i":1}} tail`, want: `{"o":{"i":1}}`, wantOK: true},
		{name: "escaped quote then brace in string", in: `{"s":"x\" } y"}`, want: `{"s":"x\" } y"}`, wantOK: true},
		{name: "escaped backslash before quote", in: `{"s":"x\\"}`, want: `{"s":"x\\"}`, wantOK: true},
		{name: "no opening brace", in: `no object here`, wantOK: false},
		{name: "unbalanced never closes", in: `{"a":1`, wantOK: false},
		{name: "unbalanced nested", in: `{"o":{"i":1}`, wantOK: false},
		{name: "empty string", in: ``, wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := extractBalancedJSONObject(tt.in)
			if ok != tt.wantOK {
				t.Fatalf("extractBalancedJSONObject() ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Fatalf("extractBalancedJSONObject() = %q, want %q", got, tt.want)
			}
		})
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
		// inlineMessage seeds obs.lastAssistantText — the agent's final message
		// the inline tier scans when no file was written. Empty ⇒ no inline text.
		inlineMessage string
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
		{
			// Resolution order: no file ⇒ the inline tier recovers the structured
			// manifest from the final message, beating the bare scraped marker.
			name:           "inline manifest recovered when file absent",
			raw:            "",
			inlineMessage:  `Intended manifest: {"schemaVersion":1,"verdict":"failed","summary":"inline summary"}` + "\nWORK_RESULT:failed",
			seedWorkResult: "failed", // marker scraped only the bare verdict
			seedSummary:    "scraped summary",
			wantWorkResult: "failed",
			wantSummary:    "inline summary", // the structured summary the marker lost
			wantManifest:   true,
		},
		{
			// Resolution order: a written file WINS over an inline block — the
			// file tier is reached first and the inline tier is never consulted.
			name:           "file manifest preferred over inline when both present",
			raw:            `{"schemaVersion":1,"verdict":"passed","summary":"file summary"}`,
			inlineMessage:  `Intended manifest: {"schemaVersion":1,"verdict":"failed","summary":"inline summary"}`,
			seedWorkResult: "passed",
			wantWorkResult: "passed",
			wantSummary:    "file summary",
			wantManifest:   true,
		},
		{
			// No file AND no recoverable inline block ⇒ both structured tiers
			// degrade and the scraped marker stands untouched.
			name:           "no file and no inline leaves scraped marker",
			raw:            "",
			inlineMessage:  "all done, see WORK_RESULT below\nWORK_RESULT:passed",
			seedWorkResult: "passed",
			seedSummary:    "scraped summary",
			wantWorkResult: "passed",
			wantSummary:    "scraped summary",
			wantManifest:   false,
		},
		{
			// Inline blocked verdict feeds the blocked signal like a file would.
			name:           "inline blocked manifest feeds blocked signal",
			raw:            "",
			inlineMessage:  `Intended manifest: {"schemaVersion":1,"verdict":"blocked","blockedReason":"needs spec"}`,
			seedWorkResult: "",
			wantWorkResult: "",
			wantBlocked:    true,
			wantBlockedRsn: "needs spec",
			wantManifest:   true,
		},
		{
			// A malformed inline block degrades to the marker, never fails.
			name:           "malformed inline block is a no-op fallback",
			raw:            "",
			inlineMessage:  `Intended manifest: {"schemaVersion":1,"verdict":`,
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
				workResult:        tt.seedWorkResult,
				pullRequestURL:    tt.seedPR,
				lastAssistantText: tt.inlineMessage,
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

func TestParseManifestPerRepositoryMembers(t *testing.T) {
	dir := t.TempDir()
	writeManifestFile(t, dir, `{"schemaVersion":1,"verdict":"passed","repositories":[{"name":"primary","verdict":"passed","commitSha":"abc"}]}`)
	manifest, err := ParseManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	entries := manifestRepositoryEntries(manifest)
	if len(entries) != 1 || entries[0].Name != "primary" || entries[0].CommitSHA != "abc" {
		t.Fatalf("repository results = %#v", entries)
	}

	duplicateDir := t.TempDir()
	writeManifestFile(t, duplicateDir, `{"schemaVersion":1,"verdict":"passed","repositories":[{"name":"primary"},{"name":"primary"}]}`)
	if _, err := ParseManifest(duplicateDir); err == nil {
		t.Fatal("duplicate per-repository result was accepted")
	}
}

func TestManifestDeclarationExcludesReadOnlyAndRequiresEveryMutableRepository(t *testing.T) {
	declaration := &workarea.RepositoryDeclarationV1{
		Protocol: workarea.ProtocolSessionRootV1,
		Repositories: []workarea.DeclaredRepositoryV1{
			{Source: workarea.RepositorySource{Repository: "https://example.test/primary.git"}, Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable},
			{Source: workarea.RepositorySource{Repository: "https://example.test/secondary.git"}, Role: workarea.RepositoryRoleSecondary, Authority: workarea.RepositoryMutable},
			{Source: workarea.RepositorySource{Repository: "https://example.test/context.git"}, Role: workarea.RepositoryRoleContext, Authority: workarea.RepositoryReadOnly},
		},
	}
	qw := QueuedWork{RepositoryDeclaration: declaration}
	readOnlyEntries := []agent.TurnManifestRepository{{Name: "context", CommitSHA: "forbidden"}}
	if err := validateManifestDeclaration(qw, &TurnManifest{Repositories: &readOnlyEntries}); err == nil {
		t.Fatal("read-only repository entered completion contract")
	}
	missingEntries := []agent.TurnManifestRepository{{Name: "primary"}}
	if err := validateManifestDeclaration(qw, &TurnManifest{Repositories: &missingEntries}); err == nil {
		t.Fatal("omitted mutable secondary repository was accepted")
	}
	completeEntries := []agent.TurnManifestRepository{{Name: "primary"}, {Name: "secondary"}}
	if err := validateManifestDeclaration(qw, &TurnManifest{Repositories: &completeEntries}); err != nil {
		t.Fatalf("complete mutable set: %v", err)
	}
}
