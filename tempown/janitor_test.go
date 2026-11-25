//go:build darwin || linux

package tempown

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// deadPID returns the PID of a child that has already been reaped.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}
	return cmd.Process.Pid
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// later returns a clock two days ahead, so every fixture is past the grace.
func later() func() time.Time {
	at := time.Now().Add(48 * time.Hour)
	return func() time.Time { return at }
}

func mkdir(t *testing.T, root, name string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "payload"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func mkfile(t *testing.T, root, name string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func tag(prefix string, pid int, suffix string) string {
	return prefix + "p" + strconv.Itoa(pid) + "-1234567" + suffix
}

func TestSweepReclaimsRecordedArtefactsOfADeadOwner(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dead := deadPID(t)
	dir := mkdir(t, root, tag(PrefixClaudeUsage, dead, ""))
	file := mkfile(t, root, tag(PrefixClaudeMCPConfig, dead, ".json"))

	report := Sweep(context.Background(), SweepOptions{Root: root, Logger: quiet(), now: later()})

	if exists(dir) || exists(file) {
		t.Fatalf("dead owner's recorded artefacts survived: dir=%v file=%v report=%+v", exists(dir), exists(file), report)
	}
	if report.Reclaimed != 2 || report.Scanned != 2 {
		t.Fatalf("report = %+v, want 2 scanned and 2 reclaimed", report)
	}
}

// The ownership rule's three never-delete guarantees, each against a fixture
// that is otherwise reclaimable: old, owned by this user, and (where it has an
// owner at all) owned by a process the seam reports dead.
func TestSweepNeverDeletesUnrecordedPaths(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	unrecorded := []string{
		mkdir(t, root, "donmai-claude-usage-3920481723"), // legacy: registered prefix, no owner tag
		mkdir(t, root, "donmai-signal-guard-881"),
		mkfile(t, root, "donmai-claude-mcp-12.json"),
		mkdir(t, root, "donmai-claude-usage-px-1"), // malformed tag
		mkdir(t, root, "someone-elses-p1-2"),
		mkdir(t, root, "tmp.ztEx86ihr"),
		mkfile(t, root, "notes.txt"),
	}
	report := Sweep(context.Background(), SweepOptions{
		Root: root, Logger: quiet(), now: later(),
		ownerAlive: func(int, time.Time) (bool, error) { return false, nil },
	})
	for _, path := range unrecorded {
		if !exists(path) {
			t.Errorf("janitor deleted unrecorded path %s (report %+v)", filepath.Base(path), report)
		}
	}
	if report.Reclaimed != 0 || report.SkippedLegacy != 4 {
		t.Fatalf("report = %+v, want 0 reclaimed and 4 legacy", report)
	}
}

func TestSweepNeverDeletesALiveOwnersPath(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// Real live processes: this test binary and its parent. Both started
	// before the fixtures were created, so they are provably not a reuse.
	self := mkdir(t, root, tag(PrefixSignalGuard, os.Getpid(), ""))
	parent := mkdir(t, root, tag(PrefixMCPBearer, os.Getppid(), ""))
	parentFile := mkfile(t, root, tag(PrefixClaudeMCPConfig, os.Getppid(), ".json"))

	report := Sweep(context.Background(), SweepOptions{Root: root, Logger: quiet(), now: later()})

	for _, path := range []string{self, parent, parentFile} {
		if !exists(path) {
			t.Errorf("janitor deleted live owner's path %s (report %+v)", filepath.Base(path), report)
		}
	}
	if report.SkippedLive != 3 || report.Reclaimed != 0 {
		t.Fatalf("report = %+v, want 3 live and 0 reclaimed", report)
	}
}

func TestSweepNeverDeletesSharedToolCaches(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	caches := []string{
		mkdir(t, root, "tsx-501"),
		mkdir(t, root, "node-compile-cache"),
		mkdir(t, root, "go-build3829104"),
		mkfile(t, root, ".com.openai.codex.Xa81kq"),
		mkdir(t, root, "jiti"),
		mkdir(t, root, "donmai-pi-ext-cache"),
		mkdir(t, root, "donmai-codex-plugin-cache"),
	}
	report := Sweep(context.Background(), SweepOptions{
		Root: root, Logger: quiet(), now: later(),
		ownerAlive: func(int, time.Time) (bool, error) { return false, nil },
	})
	for _, path := range caches {
		if !exists(path) {
			t.Errorf("janitor deleted shared cache %s", filepath.Base(path))
		}
	}
	if report.Scanned != 0 {
		t.Fatalf("shared caches entered the fence: %+v", report)
	}
}

func TestSweepLeavesUnprovableEntriesAlone(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dead := deadPID(t)
	outside := t.TempDir()
	target := mkdir(t, outside, "target")

	link := filepath.Join(root, tag(PrefixClaudeLogin, dead, ""))
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	open := mkdir(t, root, tag(PrefixClaudeVersion, dead, ""))
	if err := os.Chmod(open, 0o777); err != nil { //nolint:gosec // fixture: a group/other-writable dir the janitor must refuse
		t.Fatal(err)
	}
	wrongShape := mkfile(t, root, tag(PrefixRescueIndex, dead, ""))
	ambiguous := mkdir(t, root, tag(PrefixMuseLogin, 4242, ""))

	report := Sweep(context.Background(), SweepOptions{
		Root: root, Logger: quiet(), now: later(),
		ownerAlive: func(pid int, changed time.Time) (bool, error) {
			if pid == 4242 {
				return false, errors.New("process table unreadable")
			}
			return ownerAliveOS(pid, changed)
		},
	})
	for _, path := range []string{link, target, open, wrongShape, ambiguous} {
		if !exists(path) {
			t.Errorf("janitor removed unprovable entry %s", path)
		}
	}
	if report.SkippedUnowned != 3 || report.SkippedAmbiguous != 1 || report.Reclaimed != 0 {
		t.Fatalf("report = %+v, want 3 unowned, 1 ambiguous, 0 reclaimed", report)
	}
}

func TestSweepKeepsADeadOwnersArtefactInsideTheGracePeriod(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dead := deadPID(t)
	young := mkdir(t, root, tag(PrefixClaudeStopHook, dead, ""))

	report := Sweep(context.Background(), SweepOptions{Root: root, Logger: quiet()})
	if !exists(young) || report.SkippedYoung != 1 {
		t.Fatalf("young artefact handling: exists=%v report=%+v", exists(young), report)
	}
}

func TestSweepTreatsARecentWriteDeepInsideAsInUse(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dead := deadPID(t)
	dir := mkdir(t, root, tag(PrefixMuseSession, dead, ""))
	sub := filepath.Join(dir, "tmp")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	// Only sub changes now; dir's own times stay old.
	if err := os.WriteFile(filepath.Join(sub, "fresh"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	report := Sweep(context.Background(), SweepOptions{Root: root, Logger: quiet(), Grace: time.Second})
	if !exists(dir) || report.SkippedYoung != 1 {
		t.Fatalf("a dir with a fresh nested write was reclaimed or misjudged: exists=%v report=%+v", exists(dir), report)
	}
}

func TestSweepDryRunRemovesNothing(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dead := deadPID(t)
	dir := mkdir(t, root, tag(PrefixPiCatalogProbe, dead, ""))
	report := Sweep(context.Background(), SweepOptions{Root: root, Logger: quiet(), now: later(), DryRun: true})
	if !exists(dir) || report.Reclaimed != 1 {
		t.Fatalf("dry run: exists=%v report=%+v", exists(dir), report)
	}
	counts := report.Producers()
	if len(counts) != 1 || counts[0].ByVerdict[VerdictReclaimable] != 1 {
		t.Fatalf("per-producer counts = %+v", counts)
	}
}

func TestSweepRespectsItsRemovalBudget(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dead := deadPID(t)
	for i := range 3 {
		mkdir(t, root, PrefixRescueIndex+"p"+strconv.Itoa(dead)+"-"+strconv.Itoa(i+1))
	}
	report := Sweep(context.Background(), SweepOptions{Root: root, Logger: quiet(), now: later(), MaxRemovals: 2})
	if report.Reclaimed != 2 || !report.Partial {
		t.Fatalf("report = %+v, want 2 reclaimed and partial", report)
	}
}

func TestOwnerAliveOSDetectsDeathAndPIDReuse(t *testing.T) {
	t.Parallel()
	if alive, err := ownerAliveOS(deadPID(t), time.Now()); err != nil || alive {
		t.Fatalf("reaped child: alive=%v err=%v", alive, err)
	}
	if alive, err := ownerAliveOS(os.Getppid(), time.Now()); err != nil || !alive {
		t.Fatalf("live parent after its start: alive=%v err=%v", alive, err)
	}
	// An artefact last changed long before the parent started cannot be the
	// parent's: the PID it records was reused.
	if alive, err := ownerAliveOS(os.Getppid(), time.Unix(1, 0)); err != nil || alive {
		t.Fatalf("reused pid: alive=%v err=%v", alive, err)
	}
	if alive, err := ownerAliveOS(os.Getpid(), time.Unix(1, 0)); err != nil || !alive {
		t.Fatalf("self is always alive: alive=%v err=%v", alive, err)
	}
}
