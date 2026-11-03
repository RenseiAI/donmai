package muse

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// newTrustedTestProvider mirrors newTestProvider but starts the host
// trusted: it is the production shape (the registry constructs the
// muse provider with TrustWorkspace on) with the process spawn
// replaced by the fake host.
func newTrustedTestProvider(t *testing.T, script string) (*Provider, *fakeHost) {
	t.Helper()
	fh, stdinW, stdoutR := newFakeHost()
	fh.script = script
	go fh.run(t)
	p, err := New(Options{skipProcess: true, TrustWorkspace: true, stdinOverride: stdinW, stdoutOverride: stdoutR})
	if err != nil {
		fh.close()
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Shutdown(context.Background())
		fh.close()
	})
	return p, fh
}

// TestCardSkills_MaterializeIntoTrustedWorkspace is the pin for the
// trusted-workspace skill channel: a Spawn carrying card skills must
// BOTH write them to the host's project discovery path
// (.agents/skills/donmai-*/SKILL.md, loadable frontmatter included)
// AND start the host trusted (--trust-workspace on the serve argv the
// spawn builds from this provider's own options), while the
// materialization keeps the files out of the user's diff via the git
// exclude glob. It drives the PRODUCTION Spawn entry point against
// the fake host, so dropping the materialization call from the spawn
// path, dropping the flag from the serve argv, or dropping the
// exclude write each turn this test red.
func TestCardSkills_MaterializeIntoTrustedWorkspace(t *testing.T) {
	// Capture the exclude write without needing a git checkout: the
	// seam defaults to the real git-exclude write in production.
	var excluded []string
	prevExclude := ensureCardSkillsExcluded
	ensureCardSkillsExcluded = func(string) error {
		excluded = append(excluded, cardSkillsExcludeGlob)
		return nil
	}
	t.Cleanup(func() { ensureCardSkillsExcluded = prevExclude })

	p, _ := newTrustedTestProvider(t, "single-turn")

	workspace := t.TempDir()
	spec := agent.Spec{
		Prompt:     "do work",
		Cwd:        workspace,
		Autonomous: true,
		CardSkills: []agent.CardSkill{
			{ID: "go-review", Body: "# Go review\n\nReview carefully."},
			{ID: "Spring Debugging", Body: "Debug with delight."},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	drainEvents(t, h.Events(), 10*time.Second)

	// The host rejects non-canonical roots, so the spawn resolves the
	// workspace first: expect the files under the resolved root.
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	for _, want := range []struct {
		dir  string
		body string
	}{
		{"donmai-go-review", "Review carefully."},
		{"donmai-spring-debugging", "Debug with delight."},
	} {
		path := filepath.Join(root, ".agents", "skills", want.dir, "SKILL.md")
		data, err := os.ReadFile(path) //nolint:gosec // G304: path is built from the test's own TempDir plus the asserted discovery layout.
		if err != nil {
			t.Fatalf("card skill file %q missing after Spawn: %v", path, err)
		}
		content := string(data)
		if !strings.Contains(content, "name: \""+want.dir+"\"") {
			t.Errorf("SKILL.md %q lacks the required name frontmatter:\n%s", path, content)
		}
		if !strings.Contains(content, "description: ") {
			t.Errorf("SKILL.md %q lacks the required description frontmatter:\n%s", path, content)
		}
		if !strings.Contains(content, want.body) {
			t.Errorf("SKILL.md %q lacks the card body %q:\n%s", path, content, want.body)
		}
	}

	// The serve argv this provider builds for the spawn must carry the
	// trust flag: without it the host never loads the files above.
	serve := serveArgs(spec, p.opts.TrustWorkspace, p.opts.DisableSandbox)
	if !slices.Contains(serve, "--trust-workspace") {
		t.Fatalf("trusted serve argv missing --trust-workspace: %q", serve)
	}

	// The materialization must keep the skill files out of the user's
	// diff: the git exclude glob covers every donmai- skill dir.
	if !slices.Contains(excluded, ".agents/skills/donmai-*/") {
		t.Errorf("exclude write = %q; want the %q glob", excluded, ".agents/skills/donmai-*/")
	}
}

// TestMaterializeCardSkills_EmptyIsNoop proves a skill-less session
// touches neither the workspace nor the git exclude file: no discovery
// dirs, no exclude write.
func TestMaterializeCardSkills_EmptyIsNoop(t *testing.T) {
	called := false
	prevExclude := ensureCardSkillsExcluded
	ensureCardSkillsExcluded = func(string) error { called = true; return nil }
	t.Cleanup(func() { ensureCardSkillsExcluded = prevExclude })

	workspace := t.TempDir()
	if err := materializeCardSkills(workspace, nil); err != nil {
		t.Fatalf("materializeCardSkills(nil) = %v; want nil", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".agents")); !os.IsNotExist(err) {
		t.Errorf(".agents exists after a skill-less materialization (stat err = %v)", err)
	}
	if called {
		t.Error("exclude write ran for a skill-less session; want no write")
	}
}

// TestMaterializeCardSkills_FailClosed proves a malformed card refuses
// the spawn instead of running the session without its skills: every
// row carries wantErr, and the Spawn half of the first row proves the
// refusal surfaces as a spawn failure.
func TestMaterializeCardSkills_FailClosed(t *testing.T) {
	t.Run("spawn refuses duplicate skills", func(t *testing.T) {
		p, _ := newTestProvider(t, "single-turn")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := p.Spawn(ctx, agent.Spec{
			Prompt: "do work",
			Cwd:    t.TempDir(),
			CardSkills: []agent.CardSkill{
				{ID: "go-review", Body: "one"},
				{ID: "Go Review", Body: "two"},
			},
		})
		if err == nil {
			t.Fatal("Spawn with duplicate card skills succeeded; want refusal")
		}
		if !errors.Is(err, agent.ErrSpawnFailed) {
			t.Errorf("Spawn err = %v; want it to wrap %v", err, agent.ErrSpawnFailed)
		}
	})

	for _, tc := range []struct {
		name   string
		skills []agent.CardSkill
	}{
		{"empty id", []agent.CardSkill{{ID: "", Body: "body"}}},
		{"blank id", []agent.CardSkill{{ID: "   ", Body: "body"}}},
		{"punctuation-only id", []agent.CardSkill{{ID: "!!!", Body: "body"}}},
		{"duplicate ids", []agent.CardSkill{{ID: "a", Body: "one"}, {ID: "a", Body: "two"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := materializeCardSkills(t.TempDir(), tc.skills); err == nil {
				t.Errorf("materializeCardSkills(%q) = nil; want fail-closed error", tc.skills)
			}
		})
	}

	t.Run("unwritable workspace", func(t *testing.T) {
		blocked, err := os.CreateTemp("", "muse-skill-block-")
		if err != nil {
			t.Fatalf("CreateTemp: %v", err)
		}
		_ = blocked.Close()
		t.Cleanup(func() { _ = os.Remove(blocked.Name()) })
		skills := []agent.CardSkill{{ID: "go-review", Body: "body"}}
		if err := materializeCardSkills(blocked.Name(), skills); err == nil {
			t.Error("materializeCardSkills into a file path = nil; want filesystem error")
		}
	})
}

// TestCardSkillName pins the directory-name normalization: lowercase
// slug under the donmai- namespace, no doubled prefix, fail-closed on
// ids with no usable name.
func TestCardSkillName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		id   string
		want string
	}{
		{"go-review", "donmai-go-review"},
		{"Go Review", "donmai-go-review"},
		{"spring_debugging!", "donmai-spring-debugging"},
		{"donmai-triage", "donmai-triage"},
		{"  spaced  ", "donmai-spaced"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			t.Parallel()
			got, err := cardSkillName(tc.id)
			if err != nil {
				t.Fatalf("cardSkillName(%q) = %v; want %q", tc.id, err, tc.want)
			}
			if got != tc.want {
				t.Errorf("cardSkillName(%q) = %q; want %q", tc.id, got, tc.want)
			}
		})
	}
	for _, id := range []string{"", "   ", "---", "!!!"} {
		t.Run("reject "+id, func(t *testing.T) {
			t.Parallel()
			if got, err := cardSkillName(id); err == nil {
				t.Errorf("cardSkillName(%q) = %q, nil; want fail-closed error", id, got)
			}
		})
	}
}

// TestRenderCardSkillFile pins the loadable shape: exactly one
// frontmatter block carrying the host-required name + description,
// the card body verbatim below it, and quoted values that survive
// YAML special characters.
func TestRenderCardSkillFile(t *testing.T) {
	t.Parallel()
	out := renderCardSkillFile("donmai-quote", agent.CardSkill{
		ID:   "quote",
		Body: "# Title\n\nSay \"hi\" with C:\\path.",
	})
	if !strings.HasPrefix(out, "---\n") {
		t.Errorf("rendered file does not open with a frontmatter fence:\n%s", out)
	}
	if count := strings.Count(out, "\n---\n"); count != 1 {
		t.Errorf("rendered file has %d frontmatter fences; want exactly one:\n%s", count, out)
	}
	if !strings.Contains(out, "name: \"donmai-quote\"") {
		t.Errorf("rendered file lacks the name frontmatter:\n%s", out)
	}
	if !strings.Contains(out, "description: ") {
		t.Errorf("rendered file lacks the description frontmatter:\n%s", out)
	}
	if !strings.Contains(out, "Say \"hi\" with C:\\path.") {
		t.Errorf("rendered file lacks the verbatim card body:\n%s", out)
	}

	// A body with no substantive line still validates: the fallback
	// description names the skill's card origin.
	fallback := renderCardSkillFile("donmai-empty", agent.CardSkill{ID: "empty"})
	if !strings.Contains(fallback, "description: \"Session card skill donmai-empty.\"") {
		t.Errorf("empty-body skill lacks the fallback description:\n%s", fallback)
	}

	// Long first lines are bounded to one short description line.
	long := renderCardSkillFile("donmai-long", agent.CardSkill{ID: "long", Body: strings.Repeat("w", 400)})
	for line := range strings.Lines(long) {
		if strings.HasPrefix(line, "description: ") && len(line) > cardSkillDescriptionLimit+len("description: \"\"")+8 {
			t.Errorf("description line exceeds the bound (%d bytes):\n%s", len(line), long)
		}
	}
}

// TestMaterializeCardSkills_ExcludeFailureIsBestEffort proves git
// hygiene never sinks a session: even when the exclude write fails,
// the skill files land and the spawn proceeds.
func TestMaterializeCardSkills_ExcludeFailureIsBestEffort(t *testing.T) {
	prevExclude := ensureCardSkillsExcluded
	ensureCardSkillsExcluded = func(string) error { return io.ErrUnexpectedEOF }
	t.Cleanup(func() { ensureCardSkillsExcluded = prevExclude })

	workspace := t.TempDir()
	skills := []agent.CardSkill{{ID: "go-review", Body: "Review carefully."}}
	if err := materializeCardSkills(workspace, skills); err != nil {
		t.Fatalf("materializeCardSkills with failing exclude = %v; want nil (best-effort)", err)
	}
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".agents", "skills", "donmai-go-review", "SKILL.md")); err != nil {
		t.Errorf("skill file missing after best-effort exclude failure: %v", err)
	}
}
