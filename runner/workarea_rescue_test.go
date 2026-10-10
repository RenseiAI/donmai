package runner

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/stub"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runtime/workarea"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

// onlyRescuePatch returns the single archived patch under dir, failing the
// test when there is not exactly one.
func onlyRescuePatch(t *testing.T, dir string) string {
	t.Helper()
	patches := rescuePatches(t, dir)
	if len(patches) != 1 {
		t.Fatalf("rescue patches under %s = %v; want exactly one", dir, patches)
	}
	return patches[0]
}

func rescuePatches(t *testing.T, dir string) []string {
	t.Helper()
	var patches []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".patch") {
			patches = append(patches, path)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return patches
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path) //nolint:gosec // G304: test-owned path.
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

// cloneForRescue clones a fresh bare repository and returns the clone, its
// remote and the commit it starts at.
func cloneForRescue(t *testing.T) (clone, remote, base string) {
	t.Helper()
	remote = makeBareRepo(t)
	clone = filepath.Join(t.TempDir(), "checkout")
	gitRun(t, filepath.Dir(clone), "clone", "--quiet", remote, clone)
	for _, kv := range [][2]string{{"user.email", "test@example.com"}, {"user.name", "test"}, {"commit.gpgsign", "false"}} {
		gitRun(t, clone, "config", kv[0], kv[1])
	}
	return clone, remote, gitRun(t, clone, "rev-parse", "HEAD")
}

// TestPreserveUnpublishedWork_ArchivesUncommittedAndUnpushedWork pins the
// rescue: tracked edits, untracked files and an unpushed commit all land in
// one patch that reproduces them on the starting commit; non-work paths are
// left out; and the checkout itself is not touched.
func TestPreserveUnpublishedWork_ArchivesUncommittedAndUnpushedWork(t *testing.T) {
	clone, remote, base := cloneForRescue(t)
	writeFile(t, clone, "committed.txt", "committed but never pushed\n")
	gitRun(t, clone, "add", "committed.txt")
	gitRun(t, clone, "commit", "-q", "-m", "local only")
	writeFile(t, clone, "README.md", "# edited, not committed\n")
	writeFile(t, clone, "src/new.go", "package src\n")
	writeFile(t, clone, "node_modules/pkg/index.js", "dependency output\n")
	writeFile(t, clone, "debug.log", "log output\n")
	statusBefore := gitRun(t, clone, "status", "--porcelain")

	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	res := &Result{rescueTargets: []rescueTarget{{path: clone, base: base}}}
	if !r.preserveUnpublishedWork(QueuedWork{QueuedWork: queuedWorkBase("RESCUE-1")}, res) {
		t.Fatal("preserveUnpublishedWork = false; want the work archived")
	}
	if got := gitRun(t, clone, "status", "--porcelain"); got != statusBefore {
		t.Errorf("the rescue changed the checkout's status:\nbefore:\n%s\nafter:\n%s", statusBefore, got)
	}

	patch := onlyRescuePatch(t, r.rescueDir)
	var meta rescueMetadata
	if err := json.Unmarshal([]byte(readFile(t, strings.TrimSuffix(patch, ".patch")+".json")), &meta); err != nil {
		t.Fatalf("decode rescue metadata: %v", err)
	}
	if meta.Base != base || len(meta.UnpushedCommits) != 1 || !strings.Contains(meta.UnpushedCommits[0], "local only") {
		t.Errorf("metadata = %+v; want base %s and the one unpushed commit", meta, base)
	}
	for _, path := range meta.ChangedPaths {
		if strings.HasPrefix(path, "node_modules/") || path == "debug.log" {
			t.Errorf("metadata lists non-work path %q", path)
		}
	}

	// The patch reproduces the work on a fresh checkout of the base.
	restored := filepath.Join(t.TempDir(), "restored")
	gitRun(t, filepath.Dir(restored), "clone", "--quiet", remote, restored)
	gitRun(t, restored, "checkout", "--quiet", base)
	gitRun(t, restored, "apply", patch)
	for path, want := range map[string]string{
		"committed.txt": "committed but never pushed\n",
		"README.md":     "# edited, not committed\n",
		"src/new.go":    "package src\n",
	} {
		if got := readFile(t, filepath.Join(restored, path)); got != want {
			t.Errorf("restored %s = %q; want %q", path, got, want)
		}
	}
	for _, path := range []string{"node_modules/pkg/index.js", "debug.log"} {
		if _, err := os.Stat(filepath.Join(restored, path)); !os.IsNotExist(err) {
			t.Errorf("non-work path %s was archived (stat err %v)", path, err)
		}
	}
}

// TestPreserveUnpublishedWork_NothingToPreserve pins the quiet cases: a clean
// checkout, one whose only leftovers are non-work paths, and one whose commits
// are all pushed write no archive.
func TestPreserveUnpublishedWork_NothingToPreserve(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, clone string)
	}{
		{name: "clean", setup: func(*testing.T, string) {}},
		{name: "only non-work leftovers", setup: func(t *testing.T, clone string) {
			writeFile(t, clone, "node_modules/pkg/index.js", "dependency output\n")
			writeFile(t, clone, ".agent/state.json", "{}\n")
			writeFile(t, clone, "run.log", "log\n")
		}},
		{name: "committed and pushed", setup: func(t *testing.T, clone string) {
			writeFile(t, clone, "pushed.txt", "published\n")
			gitRun(t, clone, "add", "pushed.txt")
			gitRun(t, clone, "commit", "-q", "-m", "published")
			gitRun(t, clone, "push", "-q", "origin", "HEAD:refs/heads/agent/session")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clone, _, base := cloneForRescue(t)
			tc.setup(t, clone)
			r := minimalRunner(t)
			r.rescueDir = t.TempDir()
			res := &Result{rescueTargets: []rescueTarget{{path: clone, base: base}}}
			if !r.preserveUnpublishedWork(QueuedWork{QueuedWork: queuedWorkBase("RESCUE-2")}, res) {
				t.Fatal("preserveUnpublishedWork = false; want true")
			}
			if patches := rescuePatches(t, r.rescueDir); len(patches) != 0 {
				t.Fatalf("archived %v; want nothing", patches)
			}
		})
	}
}

// TestPreserveUnpublishedWork_ReportsAnArchiveItCannotWrite pins the
// fail-closed side: when the archive cannot be written the caller is told, so
// it keeps the workarea.
func TestPreserveUnpublishedWork_ReportsAnArchiveItCannotWrite(t *testing.T) {
	clone, _, base := cloneForRescue(t)
	writeFile(t, clone, "work.txt", "uncommitted\n")
	r := minimalRunner(t)
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	writeFile(t, filepath.Dir(blocked), filepath.Base(blocked), "a file where the rescue directory should go\n")
	r.rescueDir = blocked
	res := &Result{rescueTargets: []rescueTarget{{path: clone, base: base}}}
	if r.preserveUnpublishedWork(QueuedWork{QueuedWork: queuedWorkBase("RESCUE-3")}, res) {
		t.Fatal("preserveUnpublishedWork = true with an unwritable rescue directory; want false")
	}
}

// TestRun_TeardownKeepsAWorkareaWhoseWorkCannotBePreserved pins Run's side of
// the fail-closed rule: when the rescue cannot archive uncommitted work, the
// completed session's worktree is kept rather than deleted. The failed
// variants pin the same guard on the teardown paths this change opens:
// a crashed headless session and a failed terminal session whose rescue
// directory is unusable both keep their worktrees.
func TestRun_TeardownKeepsAWorkareaWhoseWorkCannotBePreserved(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	writeFile(t, filepath.Dir(blocked), filepath.Base(blocked), "a file where the rescue directory should go\n")
	res, _ := runScriptedSession(t, scriptedSession{
		workType:     "development",
		skipSteering: true,
		teardown:     true,
		rescueDir:    blocked,
		turns: []verdictScriptTurn{{
			files: map[string]string{"work.txt": "uncommitted\n"},
			text:  "Still working on it.",
		}},
	})
	if res.Status != "completed" {
		t.Fatalf("Status = %q (%s: %s); want completed", res.Status, res.FailureMode, res.Error)
	}
	if got := readFile(t, filepath.Join(res.WorktreePath, "work.txt")); got != "uncommitted\n" {
		t.Fatalf("work.txt = %q; want the uncommitted work kept in place", got)
	}
}

// TestRun_FailedSessionKeepsAWorkareaWhoseWorkCannotBePreserved pins the
// fail-closed guard on the failed headless path: a crashed session whose
// rescue directory cannot be written keeps its worktree instead of
// deleting unpublished work no patch preserves.
func TestRun_FailedSessionKeepsAWorkareaWhoseWorkCannotBePreserved(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	writeFile(t, filepath.Dir(blocked), filepath.Base(blocked), "a file where the rescue directory should go\n")
	res, _ := runScriptedSession(t, scriptedSession{
		workType:     "development",
		skipSteering: true,
		teardown:     true,
		rescueDir:    blocked,
		turns: []verdictScriptTurn{{
			files: map[string]string{"work.txt": "uncommitted\n"},
			text:  "Still working on it.",
			crash: true,
		}},
	})
	if res.Status != "failed" {
		t.Fatalf("Status = %q (%s: %s); want failed", res.Status, res.FailureMode, res.Error)
	}
	if got := readFile(t, filepath.Join(res.WorktreePath, "work.txt")); got != "uncommitted\n" {
		t.Fatalf("work.txt = %q; want the unpreserved work kept in place", got)
	}
}

// TestPreserveUnpublishedWork_PatchIgnoresDiffConfiguration pins that user or
// repository diff configuration cannot alter the archived patch: each of these
// settings once produced a patch that did not apply (or applied converted
// content) while the rescue reported success and the worktree was deleted.
func TestPreserveUnpublishedWork_PatchIgnoresDiffConfiguration(t *testing.T) {
	cases := []struct {
		name string
		cfg  [][2]string
		attr string
	}{
		{name: "diff.noprefix", cfg: [][2]string{{"diff.noprefix", "true"}}},
		{name: "diff.mnemonicPrefix", cfg: [][2]string{{"diff.mnemonicPrefix", "true"}}},
		{name: "color.diff always", cfg: [][2]string{{"color.diff", "always"}, {"color.ui", "always"}}},
		{name: "diff.external", cfg: [][2]string{{"diff.external", "/bin/echo"}}},
		{name: "textconv attribute", cfg: [][2]string{{"diff.upper.textconv", "tr a-z A-Z <"}}, attr: "*.txt diff=upper\n"},
		{name: "rename detection", cfg: [][2]string{{"diff.renames", "copies"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clone, remote, base := cloneForRescue(t)
			for _, kv := range tc.cfg {
				gitRun(t, clone, "config", kv[0], kv[1])
			}
			if tc.attr != "" {
				writeFile(t, clone, ".git/info/attributes", tc.attr)
			}
			writeFile(t, clone, "notes.txt", "lower case work\n")
			gitRun(t, clone, "mv", "README.md", "READ-ME.md")
			r := minimalRunner(t)
			r.rescueDir = t.TempDir()
			res := &Result{rescueTargets: []rescueTarget{{path: clone, base: base}}}
			if !r.preserveUnpublishedWork(QueuedWork{QueuedWork: queuedWorkBase("RESCUE-CONFIG")}, res) {
				t.Fatal("preserveUnpublishedWork = false; want the work archived")
			}
			patch := onlyRescuePatch(t, r.rescueDir)
			restored := filepath.Join(t.TempDir(), "restored")
			gitRun(t, filepath.Dir(restored), "clone", "--quiet", remote, restored)
			gitRun(t, restored, "checkout", "--quiet", base)
			gitRun(t, restored, "apply", patch)
			if got := readFile(t, filepath.Join(restored, "notes.txt")); got != "lower case work\n" {
				t.Fatalf("restored notes.txt = %q; want the work byte for byte", got)
			}
			if _, err := os.Stat(filepath.Join(restored, "READ-ME.md")); err != nil {
				t.Errorf("the rename was not restored: %v", err)
			}
		})
	}
}

// TestPreserveUnpublishedWork_RefusesAPatchThatDoesNotReproduceTheWork pins
// the proof step: a patch that does not rebuild the working state from its
// base is not a preservation, so the caller is told to keep the workarea.
func TestPreserveUnpublishedWork_RefusesAPatchThatDoesNotReproduceTheWork(t *testing.T) {
	clone, _, base := cloneForRescue(t)
	writeFile(t, clone, "README.md", "# edited\n")
	writeFile(t, clone, "notes.txt", "more work\n")
	original := rescuePatchArgs
	t.Cleanup(func() { rescuePatchArgs = original })
	// A patch that silently leaves notes.txt out.
	rescuePatchArgs = func(base, tree string) []string {
		return append(original(base, tree), "--", "README.md")
	}
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	res := &Result{rescueTargets: []rescueTarget{{path: clone, base: base}}}
	if r.preserveUnpublishedWork(QueuedWork{QueuedWork: queuedWorkBase("RESCUE-PROOF")}, res) {
		t.Fatal("preserveUnpublishedWork = true for a patch that loses notes.txt; want false so the workarea is kept")
	}
}

// TestPreserveUnpublishedWork_KeepsTheNewestArchivesPerSession pins the
// retention cap: a session keeps its last rescueKeepPerSession archives, and
// another session's archives are untouched.
func TestPreserveUnpublishedWork_KeepsTheNewestArchivesPerSession(t *testing.T) {
	clone, _, base := cloneForRescue(t)
	writeFile(t, clone, "work.txt", "uncommitted\n")
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	clock := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	r.now = func() time.Time { return clock }
	preserve := func(session string) {
		t.Helper()
		clock = clock.Add(time.Minute)
		res := &Result{rescueTargets: []rescueTarget{{path: clone, base: base}}}
		if !r.preserveUnpublishedWork(QueuedWork{QueuedWork: queuedWorkBase(session)}, res) {
			t.Fatal("preserveUnpublishedWork = false")
		}
	}
	preserve("OTHER")
	for i := 0; i < rescueKeepPerSession+2; i++ {
		preserve("RETAIN")
	}
	sessionRoot := filepath.Join(r.rescueDir, "test-session-RETAIN")
	entries, err := os.ReadDir(sessionRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != rescueKeepPerSession {
		t.Fatalf("archives kept = %d; want %d", len(entries), rescueKeepPerSession)
	}
	// OTHER ran at 03:05:05 and RETAIN at 03:06:05 … 03:12:05: the two
	// oldest RETAIN archives are pruned.
	if oldest := entries[0].Name(); !strings.HasPrefix(oldest, "20260102T030805Z-") {
		t.Errorf("oldest kept archive = %s; want the third one (the two oldest pruned)", oldest)
	}
	if got := len(rescuePatches(t, filepath.Join(r.rescueDir, "test-session-OTHER"))); got != 1 {
		t.Errorf("another session's archives = %d; want its 1 untouched", got)
	}
}

// TestRun_FailedSessionTearsDownWorktreeAfterRescue pins the no-preserve
// path end to end through Run: a failed session's unpublished work is
// archived to the rescue directory first, and only then is the worktree
// removed. The preserved variant pins the other side: with preservation
// on, the same failed session keeps its worktree for post-mortem.
func TestRun_FailedSessionTearsDownWorktreeAfterRescue(t *testing.T) {
	t.Run("teardown", func(t *testing.T) {
		rescueDir := t.TempDir()
		res, _ := runScriptedSession(t, scriptedSession{
			workType:     "development",
			skipSteering: true,
			teardown:     true,
			rescueDir:    rescueDir,
			turns: []verdictScriptTurn{{
				files: map[string]string{"work.txt": "uncommitted\n"},
				text:  "Still working on it.",
				crash: true,
			}},
		})
		if res.Status != "failed" {
			t.Fatalf("Status = %q (%s: %s); want failed", res.Status, res.FailureMode, res.Error)
		}
		if res.WorktreePath == "" {
			t.Fatal("failed session has no worktree path")
		}
		// The rescue runs before the teardown: the patch carries the work
		// the crashed session left uncommitted.
		patch := onlyRescuePatch(t, rescueDir)
		if body := readFile(t, patch); !strings.Contains(body, "work.txt") {
			t.Errorf("rescue patch does not carry the uncommitted work.txt")
		}
		if _, err := os.Stat(res.WorktreePath); !os.IsNotExist(err) {
			t.Fatalf("worktree stat err = %v; want the failed session's worktree torn down", err)
		}
	})
	t.Run("preserved", func(t *testing.T) {
		rescueDir := t.TempDir()
		res, _ := runScriptedSession(t, scriptedSession{
			workType:     "development",
			skipSteering: true,
			rescueDir:    rescueDir,
			turns: []verdictScriptTurn{{
				files: map[string]string{"work.txt": "uncommitted\n"},
				text:  "Still working on it.",
				crash: true,
			}},
		})
		if res.Status != "failed" {
			t.Fatalf("Status = %q (%s: %s); want failed", res.Status, res.FailureMode, res.Error)
		}
		if got := readFile(t, filepath.Join(res.WorktreePath, "work.txt")); got != "uncommitted\n" {
			t.Fatalf("work.txt = %q; want the failed session's work kept in place", got)
		}
		if patches := rescuePatches(t, rescueDir); len(patches) != 0 {
			t.Fatalf("rescue patches = %v; want none when the worktree is preserved", patches)
		}
	})
}

// TestRun_ReenteredSessionRescuesWorkCommittedBeforeReentry pins the
// re-entry base through the production Run entry point: the first attempt
// commits work without pushing, the second attempt re-enters the same
// generation on a shared worktree manager and crashes, and the teardown's
// rescue patch still carries the first attempt's commit. Without reseeding
// the base from the generation's declared resolved ref, the patch would
// measure from the re-entry HEAD and the earlier commit would be deleted
// with the worktree.
func TestRun_ReenteredSessionRescuesWorkCommittedBeforeReentry(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	bare := githubRepositoryFixture(t, "https://github.com/example/reentry-repo")
	_ = bare
	declaration := &workarea.RepositoryDeclarationV1{
		Protocol: workarea.ProtocolSessionRootV1,
		Repositories: []workarea.DeclaredRepositoryV1{
			{Source: workarea.RepositorySource{Repository: "https://github.com/example/reentry-repo"}, Name: "primary", Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable},
		},
	}
	workareaCaps := &agent.HarnessCaps{
		MultiRepositoryWorkareaProtocols: []string{string(workarea.ProtocolSessionRootV1)},
		RepositoryAuthorityEnforcement:   string(workarea.RepositoryAuthorityIsolatedReadOnlyV1),
		SupportsReadOnlySelectedCWD:      true,
	}
	sharedParent := t.TempDir()
	rescueDir := t.TempDir()
	platform := newRecordingPlatformServer(t)
	newRunner := func(turns []verdictScriptTurn, preserveAlways bool) (*Runner, *verdictScriptProvider) {
		t.Helper()
		base, err := stub.New()
		if err != nil {
			t.Fatalf("stub.New: %v", err)
		}
		harness, ok := base.(agent.HarnessProvider)
		if !ok {
			t.Fatal("stub provider is not a HarnessProvider")
		}
		provider := &verdictScriptProvider{HarnessProvider: harness, t: t, turns: turns, workareaCaps: workareaCaps}
		manager, err := worktree.NewManager(worktree.Options{ParentDir: sharedParent})
		if err != nil {
			t.Fatalf("worktree.NewManager: %v", err)
		}
		poster, err := result.NewPoster(result.Options{
			PlatformURL: platform.URL, WorkerID: "worker-1", AuthToken: "tok",
			HTTPClient: platform.Client(), BaseDelay: 1,
		})
		if err != nil {
			t.Fatalf("result.NewPoster: %v", err)
		}
		reg := NewRegistry()
		if err := reg.Register(provider); err != nil {
			t.Fatalf("Register: %v", err)
		}
		r, err := New(Options{
			Registry: reg, WorktreeManager: manager, Poster: poster,
			HTTPClient: platform.Client(), SkipBackstop: true, SkipPostSession: true,
			PreserveWorktreeAlways: preserveAlways,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		r.skipSteering = true
		r.rescueDir = rescueDir
		r.providerRetryBackoff = func(int) time.Duration { return 0 }
		return r, provider
	}
	queuedWork := func() QueuedWork {
		qw := QueuedWork{
			QueuedWork:      queuedWorkBase("REENTRY"),
			WorkerID:        "worker-1",
			AuthToken:       "tok",
			PlatformURL:     platform.URL,
			ResolvedProfile: ResolvedProfile{Provider: agent.ProviderStub},
		}
		qw.SessionID = "test-session-REENTRY"
		qw.RepositoryDeclaration = declaration
		qw.Repository = "https://github.com/example/reentry-repo"
		return qw
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// First attempt: provisions fresh, leaves uncommitted work, keeps the
	// worktree.
	first, _ := newRunner([]verdictScriptTurn{{
		files: map[string]string{"attempt1.txt": "first attempt work\n"},
		text:  "Stopping here.",
	}}, true)
	res1, err := first.Run(ctx, queuedWork())
	if err != nil && res1 == nil {
		t.Fatalf("first Run: %v", err)
	}
	if res1.WorktreePath == "" {
		t.Fatal("first attempt has no worktree path")
	}
	// The agent's first attempt commits locally without pushing — exactly
	// the state a re-entry finds at HEAD. The identity travels as -c
	// flags so the fixture commits with no ambient git identity, as on
	// CI runners.
	gitRun(t, res1.WorktreePath, "add", "attempt1.txt")
	gitRun(t, res1.WorktreePath, "-c", "user.email=test@example.com", "-c", "user.name=test", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "first attempt commit")
	attemptHead := gitRun(t, res1.WorktreePath, "rev-parse", "HEAD")

	// Second attempt: re-enters the same generation on a manager that
	// already owns it, crashes, and tears down.
	second, _ := newRunner([]verdictScriptTurn{{
		files: map[string]string{"attempt2.txt": "second attempt work\n"},
		text:  "Still working on it.",
		crash: true,
	}}, false)
	res2, err := second.Run(ctx, queuedWork())
	if err != nil && res2 == nil {
		t.Fatalf("second Run: %v", err)
	}
	if res2.Status != "failed" {
		t.Fatalf("Status = %q (%s: %s); want failed", res2.Status, res2.FailureMode, res2.Error)
	}
	patch := onlyRescuePatch(t, rescueDir)
	body := readFile(t, patch)
	if !strings.Contains(body, "attempt1.txt") || !strings.Contains(body, "first attempt work") {
		t.Errorf("rescue patch does not carry the pre-re-entry commit (HEAD %s)", attemptHead)
	}
	if !strings.Contains(body, "attempt2.txt") {
		t.Errorf("rescue patch does not carry the re-entered attempt's work")
	}
	if _, err := os.Stat(res2.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree stat err = %v; want the re-entered session's worktree torn down", err)
	}
}
