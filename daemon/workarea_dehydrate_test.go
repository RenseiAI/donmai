package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/internal/kit"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

const dehydrateSweepManifestTOML = `api = "donmai.dev/v2"

[kit]
id = "node/pnpm"
version = "1.0.0"
name = "pnpm Node"
priority = 60

[supports]
os = ["linux", "macos"]

[detect]
files = ["pnpm-lock.yaml"]

[[provide.dependency_store]]
manager = "pnpm"
ecosystem = "node"
lockfiles = ["pnpm-lock.yaml"]
inputs = ["pnpm-workspace.yaml"]
version = "pnpm --version"

[provide.dependency_store.store]
env = ["NPM_CONFIG_STORE_DIR"]
sharing = "content-addressed"
integrity = "on-use"
content = ["v10/files"]

[provide.dependency_store.commands]
fetch = "pnpm fetch"
install = "pnpm install --offline --frozen-lockfile"

[provide.dependency_store.snapshot]
installed = ["node_modules"]

[composition]
order = "foundation"
`

func dehydrateSweepGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	cmd := exec.Command("git", args...) //nolint:gosec // fixed test binary and temp-dir arguments
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func dehydrateSweepBareRemote(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	dehydrateSweepGit(t, "", "init", "--bare", "--quiet", remote)
	dehydrateSweepGit(t, "", "init", "--quiet", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "tracked.txt"), []byte("published bytes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "pnpm-lock.yaml"), []byte("lockfileVersion: '9.0'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dehydrateSweepGit(t, seed, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "add", "tracked.txt", "pnpm-lock.yaml")
	dehydrateSweepGit(t, seed, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "--quiet", "-m", "initial")
	dehydrateSweepGit(t, seed, "remote", "add", "origin", remote)
	dehydrateSweepGit(t, seed, "push", "--quiet", "-u", "origin", "main")
	dehydrateSweepGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	return remote
}

// A preserved root dehydrates through the daemon sweep: the sweep
// resolves the kit-declared stores, the engine classifies and records,
// and past the 24-hour age the recorded installed tree is removed with a
// receipt naming the policy and the measured bytes.
func TestRunDehydratePassRemovesRecordedPaths(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	scanDir := t.TempDir()
	writeV2Kit(t, scanDir, "pnpm", dehydrateSweepManifestTOML)
	registry := NewKitRegistry([]string{scanDir})

	remote := dehydrateSweepBareRemote(t)
	parent := t.TempDir()
	now := time.Now()
	owner, err := worktree.NewManager(worktree.Options{ParentDir: parent, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	path, err := owner.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		RepoURL:   remote,
		Strategy:  worktree.StrategyClone,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, ".gitignore"), []byte("node_modules/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dehydrateSweepGit(t, path, "add", ".gitignore")
	dehydrateSweepGit(t, path, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "--quiet", "-m", "ignore installed tree")
	if err := os.MkdirAll(filepath.Join(path, "node_modules", ".pnpm"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "node_modules", ".pnpm", "lock.yaml"), []byte("installed\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	manager, err := worktree.NewManager(worktree.Options{ParentDir: parent, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	first, err := RunDehydratePass(context.Background(), manager, registry, kit.OSLinux, RetentionSettings{DehydrationAgeHours: DefaultDehydrationAgeHours})
	if err != nil {
		t.Fatal(err)
	}
	if first.Dehydrated != 0 {
		t.Fatalf("first sweep dehydrated = %d, want 0: classification writes the record first", first.Dehydrated)
	}
	record, err := manager.DehydrateInstallRecord(path)
	if err != nil {
		t.Fatalf("install record missing after first sweep: %v", err)
	}
	if len(record.Paths) != 1 || record.Paths[0] != "node_modules" {
		t.Fatalf("install record = %+v, want [node_modules]", record)
	}

	later := NewKitRegistry([]string{scanDir})
	laterManager, err := worktree.NewManager(worktree.Options{ParentDir: parent, Now: func() time.Time { return now.Add(25 * time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	second, err := RunDehydratePass(context.Background(), laterManager, later, kit.OSLinux, RetentionSettings{DehydrationAgeHours: DefaultDehydrationAgeHours})
	if err != nil {
		t.Fatal(err)
	}
	if second.Dehydrated != 1 {
		t.Fatalf("second sweep dehydrated = %d, want 1", second.Dehydrated)
	}
	if second.BytesReclaimed < 0 {
		t.Fatalf("bytes reclaimed = %d, want >= 0: the change is measured, not estimated", second.BytesReclaimed)
	}
	if _, err := os.Stat(filepath.Join(path, "node_modules")); !os.IsNotExist(err) {
		t.Fatalf("recorded installed tree survives: %v", err)
	}
	if _, err := os.Stat(filepath.Join(path, "tracked.txt")); err != nil {
		t.Fatalf("tracked source removed: %v", err)
	}
}

// The sweep is a no-op without a registry or manager: embedders without
// kits keep today's behavior byte for byte.
func TestRunDehydratePassNilRegistryIsNoop(t *testing.T) {
	parent := t.TempDir()
	manager, err := worktree.NewManager(worktree.Options{ParentDir: parent})
	if err != nil {
		t.Fatal(err)
	}
	report, err := RunDehydratePass(context.Background(), manager, nil, kit.OSLinux, RetentionSettings{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Dehydrated != 0 || report.BytesReclaimed != 0 {
		t.Fatalf("report = %+v, want zero: no registry means no stores to classify", report)
	}
	report, err = RunDehydratePass(context.Background(), nil, NewKitRegistry([]string{t.TempDir()}), kit.OSLinux, RetentionSettings{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Dehydrated != 0 {
		t.Fatalf("report = %+v, want zero: no manager means no roots", report)
	}
}

// Ambiguous leaves select nothing: two stores of one ecosystem matching a
// leaf with no declaration to decide between them keep the root rather
// than guessing which installed tree is safe.
func TestDehydrationStoresAmbiguousLeafSelectsNothing(t *testing.T) {
	scanDir := t.TempDir()
	second := `api = "donmai.dev/v2"

[kit]
id = "node/pnpm-alt"
version = "1.0.0"
name = "pnpm Node alt"
priority = 60

[supports]
os = ["linux", "macos"]

[detect]
files = ["pnpm-lock.yaml"]

[[provide.dependency_store]]
manager = "pnpm-alt"
ecosystem = "node"
lockfiles = ["pnpm-lock.yaml"]
version = "pnpm --version"

[provide.dependency_store.store]
sharing = "content-addressed"
integrity = "on-use"
content = ["v10/files"]

[provide.dependency_store.commands]
fetch = "pnpm fetch"
install = "pnpm install --offline --frozen-lockfile"

[provide.dependency_store.snapshot]
installed = ["node_modules"]

[composition]
order = "framework"
`
	first := `api = "donmai.dev/v2"

[kit]
id = "node/pnpm"
version = "1.0.0"
name = "pnpm Node"
priority = 60

[supports]
os = ["linux", "macos"]

[detect]
files = ["pnpm-lock.yaml"]

[[provide.dependency_store]]
manager = "pnpm"
ecosystem = "node"
lockfiles = ["pnpm-lock.yaml"]
version = "pnpm --version"

[provide.dependency_store.store]
sharing = "content-addressed"
integrity = "on-use"
content = ["v10/files"]

[provide.dependency_store.commands]
fetch = "pnpm fetch"
install = "pnpm install --offline --frozen-lockfile"

[provide.dependency_store.snapshot]
installed = ["node_modules"]

[composition]
order = "framework"
`
	writeV2Kit(t, scanDir, "pnpm", first)
	writeV2Kit(t, scanDir, "pnpm-alt", second)
	registry := NewKitRegistry([]string{scanDir})

	leaf := t.TempDir()
	if err := os.WriteFile(filepath.Join(leaf, "pnpm-lock.yaml"), []byte("lockfileVersion: '9.0'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	selected, err := registry.DehydrationStores(leaf, kit.OSLinux)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 0 {
		t.Fatalf("selected = %+v, want none: manager_ambiguous keeps the root", selected)
	}
}

// A negative dehydration age classifies only: the record is written but
// nothing is removed even past the default 24-hour age.
func TestRunDehydratePassNegativeAgeClassifiesOnly(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	scanDir := t.TempDir()
	writeV2Kit(t, scanDir, "pnpm", dehydrateSweepManifestTOML)
	registry := NewKitRegistry([]string{scanDir})

	remote := dehydrateSweepBareRemote(t)
	parent := t.TempDir()
	now := time.Now()
	owner, err := worktree.NewManager(worktree.Options{ParentDir: parent, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	path, err := owner.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
		RepoURL:   remote,
		Strategy:  worktree.StrategyClone,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, ".gitignore"), []byte("node_modules/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dehydrateSweepGit(t, path, "add", ".gitignore")
	dehydrateSweepGit(t, path, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "--quiet", "-m", "ignore installed tree")
	if err := os.MkdirAll(filepath.Join(path, "node_modules"), 0o750); err != nil {
		t.Fatal(err)
	}

	manager, err := worktree.NewManager(worktree.Options{ParentDir: parent, Now: func() time.Time { return now.Add(72 * time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	report, err := RunDehydratePass(context.Background(), manager, registry, kit.OSLinux, RetentionSettings{DehydrationAgeHours: -1})
	if err != nil {
		t.Fatal(err)
	}
	if report.Dehydrated != 0 {
		t.Fatalf("dehydrated = %d, want 0: negative age classifies only", report.Dehydrated)
	}
	if _, err := manager.DehydrateInstallRecord(path); err != nil {
		t.Fatalf("install record missing: classify-only still records (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(path, "node_modules")); err != nil {
		t.Fatalf("installed tree removed under classify-only: %v", err)
	}
}
