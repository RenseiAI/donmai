package worktree_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
