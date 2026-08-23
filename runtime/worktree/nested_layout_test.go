package worktree_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/RenseiAI/donmai/runtime/workarea"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

const (
	nestedLeaseSessionID = "22222222-2222-4222-8222-222222222222"
	nestedLeaseResultID  = "tr_22222222222222222222222222222222"
)

func nestedCloneStub() worktree.CommandRunner {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "git" && len(args) > 0 && args[0] == "clone" {
			destination := args[len(args)-1]
			if err := os.MkdirAll(filepath.Join(destination, ".git"), 0o750); err != nil {
				return nil, err
			}
		}
		if name == "git" && len(args) >= 2 && args[len(args)-2] == "rev-parse" {
			return []byte("deadbeef\n"), nil
		}
		return nil, nil
	}
}

func nestedDeclaration(selected *workarea.RepositoryFilter) workarea.RepositoryDeclarationV1 {
	return workarea.RepositoryDeclarationV1{
		Protocol: workarea.ProtocolSessionRootV1,
		Repositories: []workarea.DeclaredRepositoryV1{
			{Source: workarea.RepositorySource{Repository: "https://example.test/acme/web.git", Ref: "main"}, Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable},
			{Source: workarea.RepositorySource{Repository: "https://example.test/acme/corpus.git", Ref: "main"}, Role: workarea.RepositoryRoleContext, Authority: workarea.RepositoryReadOnly},
		},
		Select: selected,
	}
}

func exactWorkareaCapabilities() workarea.ExecutorWorkareaCapabilities {
	return workarea.ExecutorWorkareaCapabilities{
		MultiRepositoryWorkareaProtocols: []workarea.Protocol{workarea.ProtocolSessionRootV1},
		RepositoryAuthorityEnforcement:   workarea.RepositoryAuthorityIsolatedReadOnlyV1,
	}
}

func newNestedManager(t *testing.T, parent string) *worktree.Manager {
	t.Helper()
	manager, err := worktree.NewManager(worktree.Options{ParentDir: parent, CommandRunner: nestedCloneStub()})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func nestedSpec(sessionID string, selected *workarea.RepositoryFilter) worktree.ProvisionSpec {
	declaration := nestedDeclaration(selected)
	return worktree.ProvisionSpec{
		SessionID: sessionID, RepoURL: declaration.Repositories[0].Source.Repository,
		SourceRef: "main", Strategy: worktree.StrategyClone,
		RepositoryDeclaration: &declaration, ExecutorCapabilities: exactWorkareaCapabilities(),
	}
}

func TestProvisionSessionRootReturnsSelectedRepositoryCWD(t *testing.T) {
	t.Parallel()
	manager := newNestedManager(t, t.TempDir())
	selected := &workarea.RepositoryFilter{Kind: workarea.RepositoryFilterNamed, Name: "corpus"}
	path, err := manager.Provision(context.Background(), nestedSpec("selected-context", selected))
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	layout, err := manager.Layout("selected-context")
	if err != nil {
		t.Fatal(err)
	}
	if !layout.IsNested() || path != layout.Repository.String() || filepath.Base(path) != "corpus" {
		t.Fatalf("layout/path = (%#v, %q), want nested selected corpus CWD", layout, path)
	}
	paths, err := manager.RepositoryPaths("selected-context")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(paths["web"]) != layout.Root.String() || filepath.Dir(paths["corpus"]) != layout.Root.String() {
		t.Fatalf("repository paths are not siblings under root: %#v, root=%q", paths, layout.Root)
	}
	record, err := workarea.ReadDeclaration(layout.Root)
	if err != nil {
		t.Fatal(err)
	}
	if record.SelectedRepository != "corpus" {
		t.Fatalf("selected record = %q, want corpus", record.SelectedRepository)
	}
}

func TestUnnegotiatedProvisionStaysFlat(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	manager := newNestedManager(t, parent)
	path, err := manager.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: "legacy-session", RepoURL: "https://example.test/acme/web.git", Strategy: worktree.StrategyClone,
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(parent, "legacy-session"); path != want {
		t.Fatalf("legacy path = %q, want %q", path, want)
	}
	layout, err := manager.Layout("legacy-session")
	if err != nil {
		t.Fatal(err)
	}
	if layout.IsNested() || layout.Root.String() != path {
		t.Fatalf("legacy layout = %#v, want degenerate flat", layout)
	}
}

func TestConcurrentSessionsOwnDistinctRepositoryLeaves(t *testing.T) {
	t.Parallel()
	manager := newNestedManager(t, t.TempDir())
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, sessionID := range []string{"concurrent-a", "concurrent-b"} {
		sessionID := sessionID
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := manager.Provision(context.Background(), nestedSpec(sessionID, nil))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	pathsA, _ := manager.RepositoryPaths("concurrent-a")
	pathsB, _ := manager.RepositoryPaths("concurrent-b")
	if pathsA["corpus"] == pathsB["corpus"] || filepath.Dir(pathsA["corpus"]) == filepath.Dir(pathsB["corpus"]) {
		t.Fatalf("concurrent context repositories alias: A=%q B=%q", pathsA["corpus"], pathsB["corpus"])
	}
}

func TestConcurrentManagersCannotShareOneSessionRoot(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	managerA := newNestedManager(t, parent)
	managerB := newNestedManager(t, parent)
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, manager := range []*worktree.Manager{managerA, managerB} {
		manager := manager
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := manager.Provision(context.Background(), nestedSpec("same-session", nil))
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	var successes, occupied int
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, worktree.ErrWorkareaRootOccupied):
			occupied++
		default:
			t.Fatalf("unexpected concurrent manager result: %v", err)
		}
	}
	if successes != 1 || occupied != 1 {
		t.Fatalf("concurrent results successes=%d occupied=%d, want 1/1", successes, occupied)
	}
	layout, found, err := workarea.DiscoverLayout(parent, "same-session", "web")
	if err != nil || !found {
		t.Fatalf("winning root was damaged: layout=%#v found=%v err=%v", layout, found, err)
	}
}

func TestNestedTeardownAndLeaseBindWholeRoot(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	manager := newNestedManager(t, parent)
	ctx := context.Background()
	if _, err := manager.Provision(ctx, nestedSpec(nestedLeaseSessionID, nil)); err != nil {
		t.Fatal(err)
	}
	layout, _ := manager.Layout(nestedLeaseSessionID)
	lease, err := manager.AcquireTerminalLease(ctx, workarea.AcquireSpec{
		SessionID: nestedLeaseSessionID, TerminalResultID: nestedLeaseResultID, Policy: workarea.DefaultLeasePolicy(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if lease.WorkareaPath != layout.Root.String() || !strings.HasPrefix(layout.Repository.String(), lease.WorkareaPath+string(filepath.Separator)) {
		t.Fatalf("lease path = %q, layout=%#v", lease.WorkareaPath, layout)
	}
	if err := manager.Teardown(ctx, nestedLeaseSessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(layout.Root.String(), "corpus", ".git")); err != nil {
		t.Fatalf("leased context repository was removed: %v", err)
	}
	_, err = manager.Provision(ctx, nestedSpec(nestedLeaseSessionID, nil))
	if !errors.Is(err, workarea.ErrWorkareaLeased) {
		t.Fatalf("Provision retained root error = %v, want ErrWorkareaLeased", err)
	}
}

func TestNestedProvisionCoexistsBesideRetainedLegacyFlat(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	legacy, err := workarea.FlatLayout(parent, "migration-session")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(legacy.Root.String(), ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	manager := newNestedManager(t, parent)
	if _, err := manager.Provision(context.Background(), nestedSpec("migration-session", nil)); err != nil {
		t.Fatalf("nested Provision beside legacy: %v", err)
	}
	nested, _ := manager.Layout("migration-session")
	if nested.Root == legacy.Root {
		t.Fatalf("new root extended legacy flat workarea: nested=%q legacy=%q", nested.Root, legacy.Root)
	}
	if _, err := os.Stat(filepath.Join(legacy.Root.String(), ".git")); err != nil {
		t.Fatalf("legacy flat workarea was modified: %v", err)
	}
}

func TestSessionGenerationsFromSameRepositoryHaveDistinctRootsAndWorkareaIDs(t *testing.T) {
	t.Parallel()
	manager := newNestedManager(t, t.TempDir())
	var roots []workarea.RootPath
	var ids []string
	for _, sessionID := range []string{"generation-a", "generation-b"} {
		if _, err := manager.Provision(context.Background(), nestedSpec(sessionID, nil)); err != nil {
			t.Fatal(err)
		}
		layout, err := manager.Layout(sessionID)
		if err != nil {
			t.Fatal(err)
		}
		record, err := workarea.ReadDeclaration(layout.Root)
		if err != nil {
			t.Fatal(err)
		}
		roots = append(roots, layout.Root)
		ids = append(ids, record.WorkareaID)
	}
	if roots[0] == roots[1] || ids[0] == ids[1] {
		t.Fatalf("session generations reused identity: roots=%v ids=%v", roots, ids)
	}
	if err := manager.Teardown(context.Background(), "generation-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(roots[1].String()); err != nil {
		t.Fatalf("tearing down first generation damaged second: %v", err)
	}
}
