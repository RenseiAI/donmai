package runner

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/pi"
)

// The usage provider/harness/pi/testdata/fakepi reports on every turn_end
// (its turnUsage* constants), in pi's own shape: input excludes both cache
// buckets.
const (
	fakePiTurnInput      = 1200
	fakePiTurnOutput     = 80
	fakePiTurnCacheRead  = 48000
	fakePiTurnCacheWrite = 512
)

// fakePiSessionProvider plays the session on a real pi handle: the stub
// harness keeps the runner's harness identity and manifest, while Spawn
// starts the pi provider against the fakepi binary, so every event the
// runner consumes comes out of pi's own handle and event mapper.
type fakePiSessionProvider struct {
	agent.HarnessProvider
	pi *pi.Provider
}

func (p *fakePiSessionProvider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	return p.pi.Spawn(ctx, agent.Spec{Cwd: spec.Cwd, Prompt: spec.Prompt})
}

func (p *fakePiSessionProvider) Resume(context.Context, string, agent.Spec) (agent.Handle, error) {
	return nil, agent.ErrUnsupported
}

// buildFakePiBinary compiles provider/harness/pi/testdata/fakepi into a
// temporary directory and returns its path.
func buildFakePiBinary(t *testing.T) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("go toolchain not on PATH: %v", err)
	}
	out := filepath.Join(t.TempDir(), "fakepi")
	// #nosec G204 -- goBin resolved from PATH above; args are fixed.
	cmd := exec.Command(goBin, "build", "-o", out, "../provider/harness/pi/testdata/fakepi")
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build fakepi: %v\n%s", err, combined)
	}
	return out
}

// TestRun_PiCacheUsageReachesTheStatusPost drives the whole usage path for a
// pi session: a real pi handle reads a turn_end whose usage carries cache
// reads and writes, the event mapper turns it into per-call and terminal
// usage, the runner meters it, and the terminal status the platform receives
// carries cacheReadTokens and cacheWriteTokens. Before the mapper read pi's
// cache buckets, the status carried input and output only, so a pi session's
// cache reads never reached the platform's cost ledger.
func TestRun_PiCacheUsageReachesTheStatusPost(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fakepi subprocess test is unix-only")
	}
	bin := buildFakePiBinary(t)
	// The QA verdict ends the session after its first turn; fakepi reads
	// its reply from the inherited environment.
	t.Setenv("FAKEPI_REPLY", "Findings complete.\nWORK_RESULT: passed\nREVIEW_VERDICT: APPROVE")
	piProvider, err := pi.New(pi.Options{
		PiBin:            bin,
		HandshakeTimeout: 20 * time.Second,
		VersionProbe:     func(context.Context, string) (string, error) { return pi.PinnedVersion, nil },
	})
	if err != nil {
		t.Fatalf("pi.New: %v", err)
	}
	t.Cleanup(func() { _ = piProvider.Shutdown(context.Background()) })

	platform := newRecordingPlatformServer(t)
	res, _ := runScriptedSession(t, scriptedSession{
		workType:   "qa",
		repository: followUpRepository,
		platform:   platform,
		provider: func(base agent.HarnessProvider) agent.Provider {
			return &fakePiSessionProvider{HarnessProvider: base, pi: piProvider}
		},
	})
	if res.Status != "completed" {
		t.Fatalf("Status = %q (%s: %s); want completed", res.Status, res.FailureMode, res.Error)
	}
	want := agent.CostData{
		InputTokens:       fakePiTurnInput,
		OutputTokens:      fakePiTurnOutput,
		CachedInputTokens: fakePiTurnCacheRead,
		CacheWriteTokens:  fakePiTurnCacheWrite,
		NumTurns:          1,
	}
	if res.Cost == nil || *res.Cost != want {
		t.Errorf("Cost = %+v; want %+v", res.Cost, want)
	}

	status := platform.terminalStatus(t)
	for key, want := range map[string]int64{
		"inputTokens":      fakePiTurnInput,
		"outputTokens":     fakePiTurnOutput,
		"cacheReadTokens":  fakePiTurnCacheRead,
		"cacheWriteTokens": fakePiTurnCacheWrite,
	} {
		raw, ok := status[key]
		if !ok {
			t.Errorf("posted status has no %s; want %d (body keys: %v)", key, want, statusKeys(status))
			continue
		}
		var got int64
		if err := json.Unmarshal(raw, &got); err != nil || got != want {
			t.Errorf("posted %s = %s; want %s", key, raw, strconv.FormatInt(want, 10))
		}
	}
}

func statusKeys(status map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(status))
	for k := range status {
		keys = append(keys, k)
	}
	return keys
}
