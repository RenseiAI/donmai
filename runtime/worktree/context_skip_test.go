package worktree_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/RenseiAI/donmai/runtime/workarea"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

var errCloneFailed = errors.New("clone failed")

// failingContextStub fails clones whose destination leaf is "corpus" while
// materializing every other clone like the nested fixture.
func failingContextStub() worktree.CommandRunner {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "git" && len(args) > 0 && args[0] == "clone" {
			destination := args[len(args)-1]
			if filepath.Base(destination) == "corpus" {
				return []byte("fatal: remote hung up unexpectedly"), errCloneFailed
			}
			if err := os.MkdirAll(filepath.Join(destination, ".git"), 0o750); err != nil {
				return nil, err
			}
			return []byte(""), nil
		}
		if name == "git" && len(args) >= 2 && args[len(args)-2] == "rev-parse" {
			return []byte("deadbeef\n"), nil
		}
		return nil, nil
	}
}

func failingPrimaryStub() worktree.CommandRunner {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "git" && len(args) > 0 && args[0] == "clone" {
			destination := args[len(args)-1]
			if filepath.Base(destination) == "web" {
				return []byte("fatal: remote hung up unexpectedly"), errCloneFailed
			}
			if err := os.MkdirAll(filepath.Join(destination, ".git"), 0o750); err != nil {
				return nil, err
			}
			return []byte(""), nil
		}
		if name == "git" && len(args) >= 2 && args[len(args)-2] == "rev-parse" {
			return []byte("deadbeef\n"), nil
		}
		return nil, nil
	}
}

func TestProvisionSkipsFailedReadOnlyContextRepository(t *testing.T) {
	t.Parallel()
	manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir(), CommandRunner: failingContextStub()})
	if err != nil {
		t.Fatal(err)
	}
	path, err := manager.Provision(context.Background(), nestedSpec("skip-context", nil))
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if got := filepath.Base(path); got != "web" {
		t.Fatalf("provisioned path leaf = %q; want primary web", got)
	}
	skipped, err := manager.SkippedRepositories("skip-context")
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 || skipped[0] != "corpus" {
		t.Fatalf("skipped = %v; want [corpus]", skipped)
	}
	paths, err := manager.RepositoryPaths("skip-context")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := paths["corpus"]; ok {
		t.Fatalf("skipped repository still has a path: %#v", paths)
	}
	if _, ok := paths["web"]; !ok {
		t.Fatalf("primary repository missing from paths: %#v", paths)
	}
	layout, err := manager.Layout("skip-context")
	if err != nil {
		t.Fatal(err)
	}
	record, err := workarea.ReadDeclaration(layout.Root)
	if err != nil {
		t.Fatalf("ReadDeclaration: %v", err)
	}
	for _, repository := range record.Repositories {
		if repository.Name == "corpus" {
			t.Fatalf("skipped repository persisted in declaration record: %#v", record.Repositories)
		}
	}
	if _, err := os.Stat(filepath.Join(layout.Root.String(), "corpus")); !os.IsNotExist(err) {
		t.Fatalf("partial skipped checkout left behind: err = %v", err)
	}
}

func TestProvisionFailsFailedWritablePrimaryRepository(t *testing.T) {
	t.Parallel()
	manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir(), CommandRunner: failingPrimaryStub()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Provision(context.Background(), nestedSpec("fail-primary", nil)); err == nil {
		t.Fatal("Provision succeeded; want failure when the writable primary clone fails")
	} else if !strings.Contains(err.Error(), `"web"`) {
		t.Fatalf("error = %v; want it to name the failed primary repository", err)
	}
}

func TestProvisionSkippedContextLeavesSingleRepoSessionUnchanged(t *testing.T) {
	t.Parallel()
	manager := newNestedManager(t, t.TempDir())
	declaration := workarea.RepositoryDeclarationV1{
		Protocol: workarea.ProtocolSessionRootV1,
		Repositories: []workarea.DeclaredRepositoryV1{
			{Source: workarea.RepositorySource{Repository: "https://example.test/acme/web.git", Ref: "main"}, Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable},
		},
	}
	spec := worktree.ProvisionSpec{
		SessionID: "single-repo", RepoURL: declaration.Repositories[0].Source.Repository,
		SourceRef: "main", Strategy: worktree.StrategyClone,
		RepositoryDeclaration: &declaration, ExecutorCapabilities: exactWorkareaCapabilities(),
	}
	if _, err := manager.Provision(context.Background(), spec); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	skipped, err := manager.SkippedRepositories("single-repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %v; want empty for an unchanged single-repo session", skipped)
	}
	paths, err := manager.RepositoryPaths("single-repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 {
		t.Fatalf("paths = %#v; want exactly the primary repository", paths)
	}
}

// joinShared provisions sessionID as a shared participant of the owner's
// root, selecting the primary repository, with declaration as its bound
// declaration.
func joinShared(t *testing.T, parent string, runner worktree.CommandRunner, ownerRoot workarea.RootPath, sessionID string, declaration workarea.RepositoryDeclarationV1) (*worktree.Manager, error) {
	t.Helper()
	record, err := workarea.ReadDeclaration(ownerRoot)
	if err != nil {
		t.Fatal(err)
	}
	child, err := worktree.NewManager(worktree.Options{ParentDir: parent, CommandRunner: runner, RestoreSessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	filter := &workarea.RepositoryFilter{Kind: workarea.RepositoryFilterNamed, Name: "web"}
	declaration.Select = filter
	spec := worktree.ProvisionSpec{
		SessionID: sessionID, RepoURL: declaration.Repositories[0].Source.Repository,
		SourceRef: "main", Strategy: worktree.StrategyClone,
		RepositoryDeclaration: &declaration, ExecutorCapabilities: exactWorkareaCapabilities(),
		Mode: worktree.ModeShared, ParentWorkareaID: record.WorkareaID, RepositoryFilter: filter,
	}
	_, err = child.Provision(context.Background(), spec)
	return child, err
}

// TestSharedJoinAcceptsOnlyARecordedSkip pins re-entry matching: a
// participant may join a root that omits a declared read-only context
// repository only when the root's durable record says that very repository
// was skipped. A root that never had the repository, or whose skipped entry
// describes another repository, does not match.
func TestSharedJoinAcceptsOnlyARecordedSkip(t *testing.T) {
	t.Run("the omission is the root's recorded skip", func(t *testing.T) {
		parent := t.TempDir()
		owner, err := worktree.NewManager(worktree.Options{ParentDir: parent, CommandRunner: failingContextStub(), RestoreSessionID: "owner"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Provision(context.Background(), nestedSpec("owner", nil)); err != nil {
			t.Fatalf("owner Provision: %v", err)
		}
		layout, _ := owner.Layout("owner")
		record, err := workarea.ReadDeclaration(layout.Root)
		if err != nil {
			t.Fatal(err)
		}
		if got := record.SkippedRepositoryNames(); len(got) != 1 || got[0] != "corpus" {
			t.Fatalf("durable skipped set = %v; want [corpus]", got)
		}
		child, err := joinShared(t, parent, nestedCloneStub(), layout.Root, "child", nestedDeclaration(nil))
		if err != nil {
			t.Fatalf("join a root that recorded the skip: %v", err)
		}
		if skipped, err := child.SkippedRepositories("child"); err != nil || len(skipped) != 1 || skipped[0] != "corpus" {
			t.Fatalf("participant skipped = %v, %v; want [corpus] from the durable record", skipped, err)
		}
	})
	t.Run("the root never had the repository", func(t *testing.T) {
		parent := t.TempDir()
		owner, err := worktree.NewManager(worktree.Options{ParentDir: parent, CommandRunner: nestedCloneStub(), RestoreSessionID: "owner"})
		if err != nil {
			t.Fatal(err)
		}
		single := nestedDeclaration(nil)
		single.Repositories = single.Repositories[:1]
		ownerSpec := nestedSpec("owner", nil)
		ownerSpec.RepositoryDeclaration = &single
		if _, err := owner.Provision(context.Background(), ownerSpec); err != nil {
			t.Fatalf("owner Provision: %v", err)
		}
		layout, _ := owner.Layout("owner")
		if _, err := joinShared(t, parent, nestedCloneStub(), layout.Root, "child", nestedDeclaration(nil)); err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("join a root that never had the context repository: err = %v; want a declaration mismatch", err)
		}
	})
	t.Run("the recorded skip is another repository", func(t *testing.T) {
		parent := t.TempDir()
		owner, err := worktree.NewManager(worktree.Options{ParentDir: parent, CommandRunner: failingContextStub(), RestoreSessionID: "owner"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Provision(context.Background(), nestedSpec("owner", nil)); err != nil {
			t.Fatalf("owner Provision: %v", err)
		}
		layout, _ := owner.Layout("owner")
		other := nestedDeclaration(nil)
		other.Repositories[1].Source.Repository = "https://example.test/elsewhere/corpus.git"
		if _, err := joinShared(t, parent, nestedCloneStub(), layout.Root, "child", other); err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("join a root whose skip is another repository: err = %v; want a declaration mismatch", err)
		}
	})
}

// recordingCloneStub materializes every clone like nestedCloneStub, fails
// the clones whose destination leaf is in fail, and records each clone's
// arguments.
func recordingCloneStub(fail ...string) (worktree.CommandRunner, func() [][]string) {
	var mu sync.Mutex
	var clones [][]string
	runner := func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "git" && len(args) > 0 && args[0] == "clone" {
			mu.Lock()
			clones = append(clones, append([]string(nil), args...))
			mu.Unlock()
			destination := args[len(args)-1]
			if slices.Contains(fail, filepath.Base(destination)) {
				return []byte("fatal: remote hung up unexpectedly"), errCloneFailed
			}
			return nil, os.MkdirAll(filepath.Join(destination, ".git"), 0o750)
		}
		if name == "git" && len(args) >= 2 && args[len(args)-2] == "rev-parse" {
			return []byte("deadbeef\n"), nil
		}
		return nil, nil
	}
	return runner, func() [][]string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(clones)
	}
}

// workareaClone returns the recorded clone of the session root's leaf
// repository (not the seed's), or nil.
func workareaClone(clones [][]string, leaf string) []string {
	for _, clone := range clones {
		destination := clone[len(clone)-1]
		if filepath.Base(destination) == leaf && !strings.Contains(destination, ".workarea-seeds") {
			return clone
		}
	}
	return nil
}

// TestCacheSeedIsAFastPathNotSessionFate pins the seed fallback. A seed that
// builds is used: the workarea clones reference it. A seed that cannot be
// built — here because a read-only context repository fails to clone into
// it — no longer fails the session: provisioning falls back to unseeded
// clones, where that repository is skipped like any failed context clone.
// The fallback never rescues a failing primary: its unseeded clone fails the
// session as before.
func TestCacheSeedIsAFastPathNotSessionFate(t *testing.T) {
	t.Run("a seed that builds is used", func(t *testing.T) {
		runner, clones := recordingCloneStub()
		manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir(), CommandRunner: runner})
		if err != nil {
			t.Fatal(err)
		}
		spec := nestedSpec("seeded", nil)
		spec.CacheSeedID = "seed-ok"
		if _, err := manager.Provision(context.Background(), spec); err != nil {
			t.Fatalf("Provision: %v", err)
		}
		for _, leaf := range []string{"web", "corpus"} {
			if clone := workareaClone(clones(), leaf); !slices.Contains(clone, "--reference") {
				t.Fatalf("workarea clone of %s = %q; want it to reference the seed", leaf, clone)
			}
		}
	})
	t.Run("a seed that cannot be built falls back and skips the context repository", func(t *testing.T) {
		runner, clones := recordingCloneStub("corpus")
		manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir(), CommandRunner: runner})
		if err != nil {
			t.Fatal(err)
		}
		spec := nestedSpec("seed-fallback", nil)
		spec.CacheSeedID = "seed-broken"
		if _, err := manager.Provision(context.Background(), spec); err != nil {
			t.Fatalf("Provision: %v; want the session provisioned without the seed", err)
		}
		if skipped, err := manager.SkippedRepositories("seed-fallback"); err != nil || len(skipped) != 1 || skipped[0] != "corpus" {
			t.Fatalf("skipped = %v, %v; want [corpus]", skipped, err)
		}
		if clone := workareaClone(clones(), "web"); clone == nil || slices.Contains(clone, "--reference") {
			t.Fatalf("workarea clone of web = %q; want an unseeded clone", clone)
		}
	})
	t.Run("a failing primary still fails the session", func(t *testing.T) {
		runner, _ := recordingCloneStub("web")
		manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir(), CommandRunner: runner})
		if err != nil {
			t.Fatal(err)
		}
		spec := nestedSpec("seed-primary", nil)
		spec.CacheSeedID = "seed-primary"
		if _, err := manager.Provision(context.Background(), spec); err == nil || !strings.Contains(err.Error(), `"web"`) {
			t.Fatalf("Provision err = %v; want the failed primary named", err)
		}
	})
}
