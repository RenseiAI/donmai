package kit

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingExecer records the commands it is asked to run, in order, and
// returns a scripted exit code / error per call.
type recordingExecer struct {
	mu    sync.Mutex
	calls []string
	// failOn, when non-empty, makes Exec return failCode for the command
	// that contains failOn (substring match).
	failOn   string
	failCode int
	// errOn, when non-empty, makes Exec return a non-nil exec error for
	// the matching command.
	errOn string
	// block, when set, blocks the matching command until ctx is done
	// (used to exercise the timeout path).
	block string
}

func (r *recordingExecer) Exec(ctx context.Context, _, command string, _ map[string]string) (int, error) {
	r.mu.Lock()
	r.calls = append(r.calls, command)
	r.mu.Unlock()

	if r.block != "" && strings.Contains(command, r.block) {
		<-ctx.Done()
		return -1, ctx.Err()
	}
	if r.errOn != "" && strings.Contains(command, r.errOn) {
		return -1, errors.New("boom: cannot exec")
	}
	if r.failOn != "" && strings.Contains(command, r.failOn) {
		return r.failCode, nil
	}
	return 0, nil
}

func (r *recordingExecer) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	copy(out, r.calls)
	return out
}

func demandFixture() *ToolchainDemand {
	return &ToolchainDemand{
		OS:               OSLinux,
		Kits:             []string{"typescript@1.0.0"},
		ToolchainInstall: []string{"install-node", "install-pnpm"},
		PostAcquire:      []string{"npm ci"},
		PreRelease:       []string{"cleanup"},
	}
}

func TestProvisionRunsInOrder(t *testing.T) {
	x := &recordingExecer{}
	p := NewKitProvisioner(nil)
	if err := p.Provision(context.Background(), x, "/work", demandFixture()); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	want := []string{"install-node", "install-pnpm", "npm ci"}
	got := x.recorded()
	if len(got) != len(want) {
		t.Fatalf("ran %v, want %v (install then post_acquire, in order)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("step %d = %q, want %q", i, got[i], want[i])
		}
	}
	// pre_release must NOT run during Provision.
	for _, c := range got {
		if c == "cleanup" {
			t.Error("pre_release ran during Provision; should only run via Release")
		}
	}
}

func TestProvisionAbortsOnNonZeroExit(t *testing.T) {
	x := &recordingExecer{failOn: "install-pnpm", failCode: 3}
	p := NewKitProvisioner(nil)
	err := p.Provision(context.Background(), x, "/work", demandFixture())
	if err == nil {
		t.Fatal("expected abort on non-zero exit")
	}
	if !strings.Contains(err.Error(), "exit 3") || !strings.Contains(err.Error(), "install-pnpm") {
		t.Errorf("error should name exit code + command, got %v", err)
	}
	// Aborts at install-pnpm → npm ci (post_acquire) must NOT run.
	for _, c := range x.recorded() {
		if c == "npm ci" {
			t.Error("post_acquire ran after a failed install; must abort first")
		}
	}
}

func TestProvisionAbortsOnExecError(t *testing.T) {
	x := &recordingExecer{errOn: "install-node"}
	p := NewKitProvisioner(nil)
	err := p.Provision(context.Background(), x, "/work", demandFixture())
	if err == nil {
		t.Fatal("expected abort on exec error")
	}
	if !strings.Contains(err.Error(), "exec error") {
		t.Errorf("want exec-error classification, got %v", err)
	}
	if len(x.recorded()) != 1 {
		t.Errorf("should abort after first failing command, ran %v", x.recorded())
	}
}

func TestProvisionTimeout(t *testing.T) {
	x := &recordingExecer{block: "install-node"}
	p := NewKitProvisioner(nil)
	p.Timeout = 50 * time.Millisecond
	start := time.Now()
	err := p.Provision(context.Background(), x, "/work", demandFixture())
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if time.Since(start) > 2*time.Second {
		t.Error("Provision did not honor the timeout")
	}
}

func TestProvisionEmptyDemandNoop(t *testing.T) {
	x := &recordingExecer{}
	p := NewKitProvisioner(nil)
	if err := p.Provision(context.Background(), x, "/work", &ToolchainDemand{}); err != nil {
		t.Fatalf("empty demand should be a no-op, got %v", err)
	}
	if len(x.recorded()) != 0 {
		t.Errorf("no commands should run for empty demand, ran %v", x.recorded())
	}
}

func TestProvisionNilExecer(t *testing.T) {
	p := NewKitProvisioner(nil)
	if err := p.Provision(context.Background(), nil, "/work", demandFixture()); err == nil {
		t.Error("expected error when Execer is nil and demand non-empty")
	}
}

func TestReleaseRunsPreRelease(t *testing.T) {
	x := &recordingExecer{}
	p := NewKitProvisioner(nil)
	p.Release(context.Background(), x, "/work", demandFixture())
	got := x.recorded()
	if len(got) != 1 || got[0] != "cleanup" {
		t.Errorf("Release ran %v, want [cleanup]", got)
	}
}

func TestReleaseBestEffortOnFailure(t *testing.T) {
	// A failing pre_release must not panic / must be swallowed.
	x := &recordingExecer{failOn: "cleanup", failCode: 1}
	p := NewKitProvisioner(nil)
	p.Release(context.Background(), x, "/work", demandFixture()) // returns nothing; must not panic
	if len(x.recorded()) != 1 {
		t.Errorf("pre_release should still be attempted, ran %v", x.recorded())
	}
}

func TestReleaseNoPreReleaseNoop(t *testing.T) {
	x := &recordingExecer{}
	p := NewKitProvisioner(nil)
	p.Release(context.Background(), x, "/work", &ToolchainDemand{OS: OSLinux})
	if len(x.recorded()) != 0 {
		t.Errorf("Release with no pre_release should be a no-op, ran %v", x.recorded())
	}
}
