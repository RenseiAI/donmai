package prompt_test

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/prompt"
	"github.com/RenseiAI/agentfactory-tui/templates"
)

// updateGolden controls whether table tests rewrite their golden
// fixtures rather than diff against them. Run with `go test -update` to
// regenerate after an intentional template change.
var updateGolden = flag.Bool("update", false, "rewrite testdata/*.golden fixtures")

// fixtureSession mirrors the live Redis session payload observed in
// F.2.7 verification (REN2-1). Keeping the fixture in code rather than
// JSON keeps the golden tests' inputs documented inline.
func fixtureSession() prompt.QueuedWork {
	return prompt.QueuedWork{
		SessionID:         "0b5e88d9-32d0-4aca-9f8c-caf82f2b399c",
		IssueID:           "08f26531-f5d2-49dc-b412-b42cef0cbffa",
		IssueIdentifier:   "REN2-1",
		LinearSessionID:   "0b5e88d9-32d0-4aca-9f8c-caf82f2b399c",
		ProviderSessionID: "",
		ProjectName:       "smoke-alpha",
		OrganizationID:    "org_ejkmv9ojdyifipydw5l1",
		Repository:        "github.com/RenseiAI/rensei-smokes-alpha",
		Ref:               "main",
		PromptContext: "<issue identifier=\"REN2-1\">\n" +
			"<title>Wave 6 smoke test — daemon E2E verification</title>\n" +
			"<description>\nrensei please create a file `hello-from-wave6.md` " +
			"at the repo root containing a single line with the current ISO-8601 " +
			"UTC timestamp, then commit and open a PR.\n</description>\n" +
			"<team name=\"Rensei Smokes\"/>\n<project name=\"smoke-alpha\"/>\n</issue>",
	}
}

func TestBuilderBuild_GoldenSnapshots(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name     string
		work     prompt.QueuedWork
		modify   func(*prompt.QueuedWork)
		appendIn string
	}{
		{
			name: "development",
			work: fixtureSession(),
			modify: func(qw *prompt.QueuedWork) {
				qw.WorkType = string(prompt.WorkTypeDevelopment)
			},
		},
		{
			name: "qa",
			work: fixtureSession(),
			modify: func(qw *prompt.QueuedWork) {
				qw.WorkType = string(prompt.WorkTypeQA)
				qw.MentionContext = "Please verify the smoke walkthrough end-to-end."
			},
		},
		{
			name: "research",
			work: fixtureSession(),
			modify: func(qw *prompt.QueuedWork) {
				qw.WorkType = string(prompt.WorkTypeResearch)
				qw.ParentContext = "Tracked under PHASE_F_HANDOFF.md."
			},
		},
		{
			name:     "with-system-append",
			work:     fixtureSession(),
			appendIn: "Always run `make verify` before opening a PR.",
			modify: func(qw *prompt.QueuedWork) {
				qw.WorkType = string(prompt.WorkTypeDevelopment)
			},
		},
		{
			name: "fallback-no-promptcontext",
			work: prompt.QueuedWork{
				SessionID:       "session-fallback",
				IssueIdentifier: "REN-9999",
				Title:           "Fallback path",
				Body:            "No prompt context, just a body.",
				WorkType:        string(prompt.WorkTypeDevelopment),
				Repository:      "github.com/RenseiAI/agentfactory-tui",
				Ref:             "main",
			},
		},
		{
			name: "unknown-worktype-falls-through",
			work: fixtureSession(),
			modify: func(qw *prompt.QueuedWork) {
				qw.WorkType = "imaginary-future-type"
			},
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			work := tc.work
			if tc.modify != nil {
				tc.modify(&work)
			}
			b := &prompt.Builder{SystemAppend: tc.appendIn}
			system, user, err := b.Build(work)
			if err != nil {
				t.Fatalf("Build returned error: %v", err)
			}

			assertGolden(t, tc.name+".system", system)
			assertGolden(t, tc.name+".user", user)
		})
	}
}

func TestBuilderBuild_Determinism(t *testing.T) {
	t.Parallel()
	work := fixtureSession()
	work.WorkType = string(prompt.WorkTypeDevelopment)
	var b prompt.Builder
	system1, user1, err := b.Build(work)
	if err != nil {
		t.Fatalf("first Build error: %v", err)
	}
	for i := 0; i < 5; i++ {
		system2, user2, err := b.Build(work)
		if err != nil {
			t.Fatalf("Build #%d error: %v", i+2, err)
		}
		if system1 != system2 || user1 != user2 {
			t.Fatalf("Build was not deterministic on iteration %d", i+2)
		}
	}
}

func TestBuilderBuild_EmptyWorkErr(t *testing.T) {
	t.Parallel()
	var b prompt.Builder
	_, _, err := b.Build(prompt.QueuedWork{})
	if !errors.Is(err, prompt.ErrEmptyWork) {
		t.Fatalf("expected ErrEmptyWork, got %v", err)
	}
}

func TestBuilderBuild_ConcurrentSafe(t *testing.T) {
	t.Parallel()
	work := fixtureSession()
	work.WorkType = string(prompt.WorkTypeDevelopment)
	var b prompt.Builder

	const N = 16
	errCh := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			_, _, err := b.Build(work)
			errCh <- err
		}()
	}
	for i := 0; i < N; i++ {
		if err := <-errCh; err != nil {
			t.Errorf("concurrent Build #%d error: %v", i, err)
		}
	}
}

// TestBuilderBuild_RaymondShim verifies that setting Builder.Registry routes
// rendering through the raymond path and produces output containing the same
// structural elements as the legacy text/template path.
func TestBuilderBuild_RaymondShim(t *testing.T) {
	t.Parallel()
	reg, err := templates.New()
	if err != nil {
		t.Fatalf("templates.New() error: %v", err)
	}

	work := fixtureSession()
	work.WorkType = string(prompt.WorkTypeDevelopment)

	// Legacy path.
	legacySystem, legacyUser, err := prompt.NewBuilder().Build(work)
	if err != nil {
		t.Fatalf("legacy Build error: %v", err)
	}

	// Raymond path via shim.
	b := &prompt.Builder{Registry: reg}
	raymondSystem, raymondUser, err := b.Build(work)
	if err != nil {
		t.Fatalf("raymond Build error: %v", err)
	}

	// Both paths must contain the canonical identity section.
	for _, s := range []string{"# Identity", "# Operating rules", work.SessionID} {
		if !strings.Contains(legacySystem, s) {
			t.Errorf("legacy system missing %q", s)
		}
		if !strings.Contains(raymondSystem, s) {
			t.Errorf("raymond system missing %q", s)
		}
	}

	// Both paths must contain the development prompt contract markers.
	for _, s := range []string{"WORK_RESULT:passed", "WORK_RESULT:failed", "gh pr create"} {
		if !strings.Contains(legacyUser, s) {
			t.Errorf("legacy user missing %q", s)
		}
		if !strings.Contains(raymondUser, s) {
			t.Errorf("raymond user missing %q", s)
		}
	}
}

// TestBuilderBuild_RaymondShim_NilRegistryPreservesLegacy verifies that
// when Registry is nil the legacy text/template path is taken (no panic,
// no error). This is the cardinal invariant of the H+1 shim.
func TestBuilderBuild_RaymondShim_NilRegistryPreservesLegacy(t *testing.T) {
	t.Parallel()
	work := fixtureSession()
	work.WorkType = string(prompt.WorkTypeQA)

	b := &prompt.Builder{} // nil Registry
	system, user, err := b.Build(work)
	if err != nil {
		t.Fatalf("Build with nil Registry error: %v", err)
	}
	if !strings.Contains(system, "# Identity") {
		t.Error("system prompt missing '# Identity' on legacy path")
	}
	if !strings.Contains(user, "WORK_RESULT:passed") {
		t.Error("user prompt missing 'WORK_RESULT:passed' on legacy path")
	}
}

// assertGolden compares got against testdata/<name>.golden, rewriting
// the file when -update is set. A golden mismatch dumps a unified diff
// to make template diffs reviewable.
func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run `go test ./prompt -update` to create): %v", path, err)
	}
	if string(want) != got {
		t.Errorf("golden mismatch for %s\n--- want ---\n%s\n--- got ---\n%s",
			name, truncate(string(want)), truncate(got))
	}
}

func truncate(s string) string {
	const limit = 4096
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "\n... [truncated]"
}

// Compile-time guard that the testdata directory is reachable from the
// package directory regardless of where `go test` was invoked.
var _ = strings.TrimSpace
