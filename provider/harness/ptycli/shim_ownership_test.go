package ptycli

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/ptyhost"
	"github.com/RenseiAI/donmai/sessionshim"
)

// shortRegistryDir keeps the registry path inside the platform's Unix socket
// length limit (as low as 104 bytes), which t.TempDir()'s test-name-derived
// paths can exceed.
func shortRegistryDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "psh")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// TestSpawnStaysDirectlyOwnedWithoutTheLaunchContract is the §D11 migration
// pin at the worker side: a binary that ships the shim code but is launched
// without the contract must behave exactly as it did before shim ownership
// existed — no registry, no socket, no second owner.
func TestSpawnStaysDirectlyOwnedWithoutTheLaunchContract(t *testing.T) {
	requireShell(t)

	dir := shortRegistryDir(t)
	sess, shim, err := spawnSession(ptyhost.Spec{Command: []string{"sh", "-c", "exit 0"}}, dir)
	if err != nil {
		t.Fatalf("spawnSession: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sess.Stop(ctx)
	})
	if shim != nil {
		t.Fatal("spawnSession created a shim with no launch contract in the environment")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("the direct-ownership path wrote %d registry entries; it must write none", len(entries))
	}
}

// TestSpawnBecomesShimOwnedUnderTheLaunchContract is the other half: given the
// contract the daemon composes, this process OWNS the session — publishing a
// discovery record a controller can adopt, and parenting the harness itself.
func TestSpawnBecomesShimOwnedUnderTheLaunchContract(t *testing.T) {
	requireShell(t)

	dir := shortRegistryDir(t)
	id := sessionshim.Identity{OrgID: "org-p", SessionID: "sess-p"}
	launch := sessionshim.Launch{
		Identity:     id,
		RegistryDir:  dir,
		Orphan:       sessionshim.DefaultOrphanPolicy(),
		ProcessEpoch: 1,
	}
	for k, v := range launch.Env() {
		t.Setenv(k, v)
	}

	workarea := dir + "/workarea"
	sess, shim, err := spawnSession(ptyhost.Spec{Command: []string{"sh", "-c", "sleep 30"}}, workarea)
	if err != nil {
		t.Fatalf("spawnSession under the launch contract: %v", err)
	}
	if shim == nil {
		t.Fatal("spawnSession did not take shim ownership despite the launch contract")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = shim.Terminate(ctx)
	})

	// The PTY session the driver drives IS the shim's — one owner, not two (§D1).
	if sess != shim.Session() {
		t.Fatal("the handle drives a different session than the shim owns; that is two owners of one PTY")
	}

	registry, err := sessionshim.NewRegistry(dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	rec, err := registry.Get(id)
	if err != nil {
		t.Fatalf("no discovery record published: %v", err)
	}
	if rec.PID != os.Getpid() {
		t.Errorf("record pid = %d, want this process (%d) — the shim is the owning process", rec.PID, os.Getpid())
	}
	if rec.WorkareaPath != workarea {
		t.Errorf("record workarea = %q, want %q; adoption compares this against the controller's expectation", rec.WorkareaPath, workarea)
	}
	if rec.SocketPath == "" || !strings.HasPrefix(rec.SocketPath, dir) {
		t.Errorf("record socket path %q is not inside the registry directory", rec.SocketPath)
	}
	// §D6: the record is a bounded, secret-free discovery artifact.
	if rec.ProcessStartedAt == 0 {
		t.Error("record has no process start identity; a bare pid is not evidence")
	}
	if shim.HarnessIdentity().PID == os.Getpid() {
		t.Error("the shim reports itself as the harness; the harness is the child under the PTY")
	}
}

// TestSpawnFailsClosedOnAnUnusableLaunchContract proves the worker does not
// silently demote itself to direct ownership when it was told to be a shim and
// cannot be one. A silent demotion would leave the controller adopting nothing
// while a terminal it believes survives restarts quietly does not.
func TestSpawnFailsClosedOnAnUnusableLaunchContract(t *testing.T) {
	requireShell(t)

	t.Setenv(sessionshim.EnvOwnership, "1")
	t.Setenv(sessionshim.EnvOrgID, "org-p")
	t.Setenv(sessionshim.EnvSessionID, "") // selected, but unusable
	t.Setenv(sessionshim.EnvRegistryDir, shortRegistryDir(t))
	t.Setenv(sessionshim.EnvProcessEpoch, "1")

	sess, shim, err := spawnSession(ptyhost.Spec{Command: []string{"sh", "-c", "exit 0"}}, "/tmp")
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = sess.Stop(ctx)
		cancel()
		t.Fatal("spawnSession fell back to direct ownership after being told to be a shim")
	}
	if shim != nil || sess != nil {
		t.Fatal("a failed shim launch returned a live session")
	}
	if !strings.Contains(err.Error(), "session shim launch") {
		t.Errorf("error %q does not name the launch contract as the cause", err)
	}
}

// TestSpawnWithCleanupReportsAShimLaunchFailureAsASpawnFailure keeps the
// driver's public contract intact: whatever goes wrong underneath, the caller
// sees agent.ErrSpawnFailed and its cleanup still runs exactly once.
func TestSpawnWithCleanupReportsAShimLaunchFailureAsASpawnFailure(t *testing.T) {
	requireShell(t)

	t.Setenv(sessionshim.EnvOwnership, "1")
	t.Setenv(sessionshim.EnvOrgID, "org-p")
	t.Setenv(sessionshim.EnvSessionID, "sess-p")
	t.Setenv(sessionshim.EnvRegistryDir, "") // selected, but nowhere to publish
	t.Setenv(sessionshim.EnvProcessEpoch, "1")

	cleaned := 0
	_, err := SpawnWithCleanup(context.Background(), "sh", []string{"-c", "exit 0"},
		agent.Spec{Interactive: &agent.InteractiveSpec{}}, agent.HarnessManifest{},
		func() error { cleaned++; return nil })
	if err == nil {
		t.Fatal("SpawnWithCleanup succeeded with an unusable launch contract")
	}
	if cleaned != 1 {
		t.Errorf("cleanup ran %d times, want exactly 1", cleaned)
	}
}
