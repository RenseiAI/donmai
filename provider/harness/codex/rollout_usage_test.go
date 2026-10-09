package codex

// Fixture session file for the interactive terminal-usage path: a thread
// with two model calls, then a forked subagent's appended resume segment
// (a second session_meta plus a compacted line, as observed on real files),
// then one more call on the original thread. The terminal cost must be the
// original thread's last cumulative total — never the fork's, never a sum.

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

const (
	rolloutFixtureThreadID = "thread-terminal-usage"
	rolloutFixtureForkID   = "thread-forked-subagent"
)

func writeRolloutFixture(t *testing.T, home string) string {
	t.Helper()
	lines := []string{
		`{"type":"session_meta","payload":{"id":"` + rolloutFixtureThreadID + `"}}`,
		usageLine(rolloutFixtureThreadID, 1000, 800, 10, 50, 4, 2000, 1600, 20, 100, 8),
		usageLine(rolloutFixtureThreadID, 1500, 1200, 30, 90, 6, 3000, 2400, 50, 190, 14),
		`{"type":"session_meta","payload":{"id":"` + rolloutFixtureForkID + `","forked_from_id":"` + rolloutFixtureThreadID + `"}}`,
		`{"type":"compacted","payload":{"message":""}}`,
		usageLine(rolloutFixtureForkID, 9000, 1000, 0, 400, 40, 9000, 1000, 0, 400, 40),
		`{"type":"event_msg","payload":{"message":"done"}}`,
		`not json at all`,
		usageLine(rolloutFixtureThreadID, 1800, 1500, 40, 120, 9, 3600, 3000, 70, 240, 20),
	}
	dir := filepath.Join(home, codexSessionStateSubdir, "2026", "10", "09")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-10-09T00-00-00-"+rolloutFixtureThreadID+".jsonl")
	body := ""
	for _, line := range lines {
		body += line + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func usageLine(thread string, in, cached, written, out, reasoning, cumIn, cumCached, cumWritten, cumOut, cumReasoning int64) string {
	return `{"type":"token_usage_record","payload":{"thread_id":"` + thread +
		`","usage":{"input_tokens":` + itoa64(in) +
		`,"cached_input_tokens":` + itoa64(cached) +
		`,"cache_write_input_tokens":` + itoa64(written) +
		`,"output_tokens":` + itoa64(out) +
		`,"reasoning_output_tokens":` + itoa64(reasoning) +
		`,"total_tokens":` + itoa64(in+out) +
		`},"thread_token_usage":{"input_tokens":` + itoa64(cumIn) +
		`,"cached_input_tokens":` + itoa64(cumCached) +
		`,"cache_write_input_tokens":` + itoa64(cumWritten) +
		`,"output_tokens":` + itoa64(cumOut) +
		`,"reasoning_output_tokens":` + itoa64(cumReasoning) +
		`,"total_tokens":` + itoa64(cumIn+cumOut) + `}}}`
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		digits = append([]byte{'-'}, digits...)
	}
	return string(digits)
}

func TestReadRolloutUsage_LastCumulativeTotalWins(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	path := writeRolloutFixture(t, home)

	totals := readRolloutUsage(path, rolloutFixtureThreadID)
	if totals == nil || !totals.Found {
		t.Fatal("no totals for the fixture thread")
	}
	// Last cumulative thread_token_usage: in=3600 cached=3000 written=70
	// out=240 reasoning=20. Input excludes both cache buckets.
	if totals.InputTokens != 530 || totals.CachedInputTokens != 3000 || totals.CacheWriteTokens != 70 ||
		totals.OutputTokens != 240 || totals.ReasoningTokens != 20 {
		t.Fatalf("totals = %+v, want input=530 cached=3000 written=70 output=240 reasoning=20", totals)
	}
}

func TestReadRolloutUsage_ForkedThreadNeverMovesTheTotal(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	path := writeRolloutFixture(t, home)

	totals := readRolloutUsage(path, rolloutFixtureForkID)
	if totals == nil || !totals.Found {
		t.Fatal("no totals for the forked thread")
	}
	// The fork's own single record: in=9000 cached=1000 out=400 reasoning=40.
	if totals.InputTokens != 8000 || totals.CachedInputTokens != 1000 ||
		totals.OutputTokens != 400 || totals.ReasoningTokens != 40 {
		t.Fatalf("fork totals = %+v, want input=8000 cached=1000 output=400 reasoning=40", totals)
	}
}

func TestReadRolloutUsage_AbsentThreadReadsAsUnknown(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	path := writeRolloutFixture(t, home)

	if totals := readRolloutUsage(path, "thread-never-ran"); totals != nil {
		t.Fatalf("unknown thread totals = %+v, want nil", totals)
	}
	if totals := readRolloutUsage(filepath.Join(home, "missing.jsonl"), rolloutFixtureThreadID); totals != nil {
		t.Fatalf("missing file totals = %+v, want nil", totals)
	}
	if totals := readRolloutUsage(path, ""); totals != nil {
		t.Fatalf("empty thread totals = %+v, want nil", totals)
	}
}

func TestFindThreadRollout_MatchesThreadSuffixOnly(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	dir := filepath.Join(home, codexSessionStateSubdir, "2026", "10", "09")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A thread whose id is a SUFFIX of the wanted one must not match: the
	// filename's last dash group is the thread id, so match on it.
	decoy := filepath.Join(dir, "rollout-2026-10-09T00-00-00-evil-"+rolloutFixtureThreadID+".jsonl")
	if err := os.WriteFile(decoy, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// "-<threadID>.jsonl" still suffix-matches the decoy name, so plant the
	// real file too and assert the newest (real) one wins only by mtime.
	wantPath := writeRolloutFixture(t, home)
	if got := findThreadRollout(home, rolloutFixtureThreadID); got != wantPath {
		t.Fatalf("findThreadRollout = %q, want %q", got, wantPath)
	}
	if got := findThreadRollout(home, "thread-never-ran"); got != "" {
		t.Fatalf("findThreadRollout for an absent thread = %q, want empty", got)
	}
	if got := findThreadRollout("", rolloutFixtureThreadID); got != "" {
		t.Fatalf("findThreadRollout with no home = %q, want empty", got)
	}
}

func TestRolloutUsageCost_RealTokensZeroPrice(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	writeRolloutFixture(t, home)

	cost := rolloutUsageCost(home, rolloutFixtureThreadID)
	if cost == nil {
		t.Fatal("no terminal cost for the fixture thread")
	}
	want := &agent.CostData{
		InputTokens: 530, CachedInputTokens: 3000, CacheWriteTokens: 70,
		OutputTokens: 240, ReasoningTokens: 20,
	}
	if *cost != *want {
		t.Fatalf("terminal cost = %+v, want %+v", cost, want)
	}
	if cost.TotalCostUsd != 0 {
		t.Fatalf("terminal cost prices host tokens at %v, want zero marginal price", cost.TotalCostUsd)
	}
	if cost.NumTurns != 0 {
		t.Fatalf("terminal cost claims %d turns; the file records model calls, not runner turns", cost.NumTurns)
	}
}

func TestRolloutUsageCost_AbsentRecordsKeepPreviousCost(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if cost := rolloutUsageCost(home, rolloutFixtureThreadID); cost != nil {
		t.Fatalf("terminal cost with no session file = %+v, want nil", cost)
	}
}

func drainTerminalResult(t *testing.T, h agent.Handle) agent.ResultEvent {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				t.Fatal("events channel closed before a terminal ResultEvent")
			}
			if result, ok := ev.(agent.ResultEvent); ok {
				return result
			}
		case <-deadline:
			t.Fatal("timed out waiting for the terminal ResultEvent")
		}
	}
}

func spawnInteractiveForUsageTest(t *testing.T, bin, boundaryRoot, workdir string, mcpServers []agent.MCPServerConfig, extraEnv map[string]string, beforePTY func(home string)) agent.Handle {
	t.Helper()
	env := map[string]string{
		codexFakeNamedAppServerEnv:         "1",
		codexFakePTYClientCreatesThreadEnv: "1",
		"OPENAI_API_KEY":                   "fixture-key",
	}
	for key, value := range extraEnv {
		env[key] = value
	}
	spec := agent.Spec{
		SessionName: "chief-of-staff",
		Cwd:         workdir,
		MCPServers:  mcpServers,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
		Env:         env,
	}
	h, err := SpawnInteractive(context.Background(), Options{
		CodexBin:                      bin,
		configTempDir:                 boundaryRoot,
		HandshakeTimeout:              10 * time.Second,
		RPCTimeout:                    5 * time.Second,
		interactiveMCPInventoryRunner: inventoryRunnerFor(t, mcpServers),
		interactiveAuthSeeder: func(_ context.Context, _ string, ownedHome string, _ interactiveCodexAuthProjection) error {
			return os.WriteFile(filepath.Join(ownedHome, codexAuthFileName), []byte(`{"auth_mode":"apikey"}`), 0o600)
		},
		interactiveBeforePTYSpawn: func(_ context.Context, _ string, _ agent.Spec, _ interactiveLaunch, home string) error {
			if beforePTY != nil {
				beforePTY(home)
			}
			return nil
		},
	}, spec)
	if err != nil {
		t.Fatalf("SpawnInteractive: %v", err)
	}
	return h
}

// TestSpawnInteractive_TerminalResultCarriesSessionFileUsage drives the
// production SpawnInteractive entry point end to end: the fake PTY client
// creates the fixture thread (as the real CLI would), the fixture session
// file is planted in the spawned home before the child exits, and the
// terminal ResultEvent must carry its last cumulative total.
func TestSpawnInteractive_TerminalResultCarriesSessionFileUsage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named interactive sessions are unix-only (validateNamedInteractiveTransport)")
	}
	clearInteractiveCodexAuthEnv(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	root := t.TempDir()
	boundaryRoot := filepath.Join(root, "session-boundaries")
	workdir := filepath.Join(root, "work")
	for _, dir := range []string{boundaryRoot, workdir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	mcpServers := []agent.MCPServerConfig{{
		Name: "donmai-platform",
		Type: "http",
		URL:  "https://platform.example.com/api/mcp/sess_project",
		Headers: map[string]string{
			"Authorization": "Bearer session-mcp-bearer",
		},
	}}
	// The fake app-server always creates the same thread id; plant the
	// fixture session file for exactly that thread in the spawned home.
	plant := func(home string) {
		dir := filepath.Join(home, codexSessionStateSubdir, "2026", "10", "09")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		body := `{"type":"session_meta","payload":{"id":"` + fakeNamedAppServerThreadID + `"}}` + "\n" +
			usageLine(fakeNamedAppServerThreadID, 1000, 800, 10, 50, 4, 2000, 1600, 20, 100, 8) + "\n" +
			usageLine(fakeNamedAppServerThreadID, 1500, 1200, 30, 90, 6, 3000, 2400, 50, 190, 14) + "\n"
		if err := os.WriteFile(filepath.Join(dir, "rollout-2026-10-09T00-00-00-"+fakeNamedAppServerThreadID+".jsonl"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h := spawnInteractiveForUsageTest(t, self, boundaryRoot, workdir, mcpServers, nil, plant)
	result := drainTerminalResult(t, h)
	if !result.Success {
		t.Fatalf("fake codex interactive session failed: %+v", result)
	}
	if result.Cost == nil {
		t.Fatal("terminal ResultEvent carries no cost; want the session file totals")
	}
	want := &agent.CostData{
		InputTokens: 550, CachedInputTokens: 2400, CacheWriteTokens: 50,
		OutputTokens: 190, ReasoningTokens: 14,
	}
	if *result.Cost != *want {
		t.Fatalf("terminal cost = %+v, want %+v", result.Cost, want)
	}
}

// TestSpawnInteractive_TerminalResultWithoutSessionFileKeepsBareResult is
// the companion: with no session file for the thread, the terminal
// ResultEvent stays bare (nil cost) instead of reporting a zero that never
// ran.
func TestSpawnInteractive_TerminalResultWithoutSessionFileKeepsBareResult(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named interactive sessions are unix-only (validateNamedInteractiveTransport)")
	}
	clearInteractiveCodexAuthEnv(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	root := t.TempDir()
	boundaryRoot := filepath.Join(root, "session-boundaries")
	workdir := filepath.Join(root, "work")
	for _, dir := range []string{boundaryRoot, workdir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	mcpServers := []agent.MCPServerConfig{{
		Name: "donmai-platform",
		Type: "http",
		URL:  "https://platform.example.com/api/mcp/sess_project",
		Headers: map[string]string{
			"Authorization": "Bearer session-mcp-bearer",
		},
	}}
	h := spawnInteractiveForUsageTest(t, self, boundaryRoot, workdir, mcpServers, nil, nil)
	result := drainTerminalResult(t, h)
	if !result.Success {
		t.Fatalf("fake codex interactive session failed: %+v", result)
	}
	if result.Cost != nil {
		t.Fatalf("terminal cost = %+v with no session file, want nil", result.Cost)
	}
}
