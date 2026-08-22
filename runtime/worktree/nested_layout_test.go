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

// cloneStub returns a CommandRunner that materializes whatever destination a
// `git clone` names, so Provision's post-clone bookkeeping runs against a real
// directory without touching the network.
func cloneStub() worktree.CommandRunner {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "git" && len(args) > 0 && args[0] == "clone" {
			dst := args[len(args)-1]
			if err := os.MkdirAll(filepath.Join(dst, ".git"), 0o750); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
}

// Canonical identities the durable lease store requires (a UUID session id and
// a tr_-prefixed terminal result id).
const (
	nestedLeaseSessionID = "22222222-2222-4222-8222-222222222222"
	nestedLeaseResultID  = "tr_22222222222222222222222222222222"
)

func newNestedManager(t *testing.T, parent string) *worktree.Manager {
	t.Helper()
	m, err := worktree.NewManager(worktree.Options{ParentDir: parent, CommandRunner: cloneStub()})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestProvisionNestsRepositoryUnderSessionRoot is the core nesting control:
// a RepositoryLeaf-bearing spec provisions
// <parent>/<sessionID>/<repo-leaf>, Provision keeps returning the REPOSITORY
// path (the agent CWD, unchanged semantics), and Layout exposes the
// session-owned root as a separate typed value.
//
// RED without the nesting change: Provision returns <parent>/<sessionID> and
// Layout reports Root == Repository.
func TestProvisionNestsRepositoryUnderSessionRoot(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	m := newNestedManager(t, parent)

	path, err := m.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID:      "sess-1",
		RepoURL:        "https://example.com/acme/web.git",
		Strategy:       worktree.StrategyClone,
		RepositoryLeaf: workarea.RepositoryLeaf("https://example.com/acme/web.git"),
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	wantRoot := filepath.Join(parent, "sess-1")
	wantRepo := filepath.Join(wantRoot, "web")
	if path != wantRepo {
		t.Errorf("Provision returned %q; want the repository path %q", path, wantRepo)
	}

	layout, err := m.Layout("sess-1")
	if err != nil {
		t.Fatalf("Layout: %v", err)
	}
	if layout.Root.String() != wantRoot {
		t.Errorf("Layout.Root = %q; want %q", layout.Root, wantRoot)
	}
	if layout.Repository.String() != wantRepo {
		t.Errorf("Layout.Repository = %q; want %q", layout.Repository, wantRepo)
	}
	if !layout.IsNested() {
		t.Error("Layout.IsNested() = false; want true")
	}
	// Path() keeps its pre-nesting meaning: the repository the agent works in.
	if p, err := m.Path("sess-1"); err != nil || p != wantRepo {
		t.Errorf("Path = (%q, %v); want (%q, nil)", p, err, wantRepo)
	}
}

// TestProvisionWithoutRepositoryLeafKeepsFlatLayout pins mixed-version
// compatibility: a caller that names no repository leaf still gets the retained
// flat workarea, byte-identical to the pre-nesting shape.
func TestProvisionWithoutRepositoryLeafKeepsFlatLayout(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	m := newNestedManager(t, parent)

	path, err := m.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: "legacy-session",
		RepoURL:   "https://example.com/acme/web.git",
		Strategy:  worktree.StrategyClone,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	want := filepath.Join(parent, "legacy-session")
	if path != want {
		t.Errorf("Provision returned %q; want the flat path %q", path, want)
	}
	layout, err := m.Layout("legacy-session")
	if err != nil {
		t.Fatalf("Layout: %v", err)
	}
	if layout.IsNested() {
		t.Errorf("flat provisioning reported nested: %+v", layout)
	}
	if layout.Root.String() != want {
		t.Errorf("Layout.Root = %q; want %q", layout.Root, want)
	}
}

// TestTeardownRemovesWholeSessionRootAndOnlyThatSession is the cleanup and
// cleanup-isolation control: teardown owns the session
// ROOT atomically — the selected repository AND this session's context repos go
// together — while a concurrent session's root, holding a clone of the SAME
// secondary repo, is untouched.
//
// RED without the nesting change: the context repo is a global peer under the
// worktree root, so tearing down one session either leaves it behind (leak) or
// deletes the copy the other session is reading.
func TestTeardownRemovesWholeSessionRootAndOnlyThatSession(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	m := newNestedManager(t, parent)
	ctx := context.Background()

	leaf := workarea.RepositoryLeaf("https://example.com/acme/web.git")
	roots := map[string]workarea.Layout{}
	for _, sessionID := range []string{"session-a", "session-b"} {
		if _, err := m.Provision(ctx, worktree.ProvisionSpec{
			SessionID:      sessionID,
			RepoURL:        "https://example.com/acme/web.git",
			Strategy:       worktree.StrategyClone,
			RepositoryLeaf: leaf,
		}); err != nil {
			t.Fatalf("Provision %s: %v", sessionID, err)
		}
		layout, err := m.Layout(sessionID)
		if err != nil {
			t.Fatal(err)
		}
		// Materialize a per-session context repository beside the selected one.
		sibling, err := layout.SiblingPath("corpus")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(sibling, ".git"), 0o750); err != nil {
			t.Fatal(err)
		}
		// The context repo is a SIBLING of the selected repository inside the
		// session root — not a global peer under the worktree parent, and not
		// buried inside the repository the agent commits from.
		if filepath.Dir(sibling) == parent {
			t.Fatalf("context repo %q is a global peer under the worktree parent", sibling)
		}
		if strings.HasPrefix(sibling, layout.Repository.String()+string(filepath.Separator)) {
			t.Fatalf("context repo %q landed inside the selected repository %q", sibling, layout.Repository)
		}
		roots[sessionID] = layout
	}

	if err := m.Teardown(ctx, "session-a"); err != nil {
		t.Fatalf("Teardown: %v", err)
	}

	if _, err := os.Stat(roots["session-a"].Root.String()); !os.IsNotExist(err) {
		t.Errorf("session-a root survived teardown: stat err = %v", err)
	}
	for _, p := range []string{
		roots["session-b"].Root.String(),
		roots["session-b"].Repository.String(),
		filepath.Join(roots["session-b"].Root.String(), "corpus", ".git"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("tearing down session-a damaged session-b path %q: %v", p, err)
		}
	}
}

// TestTerminalLeaseRetainsTheSessionRoot pins that bounded retention owns the
// session root, not one leaf inside it: an active lease blocks re-provisioning
// the session and defers teardown of the WHOLE workarea.
func TestTerminalLeaseRetainsTheSessionRoot(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	m := newNestedManager(t, parent)
	ctx := context.Background()

	leaf := workarea.RepositoryLeaf("https://example.com/acme/web.git")
	if _, err := m.Provision(ctx, worktree.ProvisionSpec{
		SessionID:      nestedLeaseSessionID,
		RepoURL:        "https://example.com/acme/web.git",
		Strategy:       worktree.StrategyClone,
		RepositoryLeaf: leaf,
	}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	layout, err := m.Layout(nestedLeaseSessionID)
	if err != nil {
		t.Fatal(err)
	}

	lease, err := m.AcquireTerminalLease(ctx, workarea.AcquireSpec{
		SessionID:        nestedLeaseSessionID,
		TerminalResultID: nestedLeaseResultID,
		Policy:           workarea.DefaultLeasePolicy(),
	})
	if err != nil {
		t.Fatalf("AcquireTerminalLease: %v", err)
	}
	if lease.WorkareaPath != layout.Root.String() {
		t.Errorf("lease retains %q; want the session root %q", lease.WorkareaPath, layout.Root)
	}
	// The retained path must CONTAIN the repository, not be it: retaining only
	// the repository leaf would leave the session's context repos collectable.
	if !strings.HasPrefix(layout.Repository.String(), lease.WorkareaPath+string(filepath.Separator)) {
		t.Errorf("lease retains %q, which does not contain the selected repository %q",
			lease.WorkareaPath, layout.Repository)
	}

	// Teardown defers while the lease is active, and the whole root survives.
	if err := m.Teardown(ctx, nestedLeaseSessionID); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if _, err := os.Stat(layout.Repository.String()); err != nil {
		t.Errorf("leased repository removed despite retention: %v", err)
	}

	// A fresh acquisition of the same session root is refused while retained.
	_, err = m.Provision(ctx, worktree.ProvisionSpec{
		SessionID:      nestedLeaseSessionID,
		RepoURL:        "https://example.com/acme/web.git",
		Strategy:       worktree.StrategyClone,
		RepositoryLeaf: leaf,
	})
	if !errors.Is(err, workarea.ErrWorkareaLeased) {
		t.Errorf("Provision over a retained root: err = %v; want ErrWorkareaLeased", err)
	}
}
