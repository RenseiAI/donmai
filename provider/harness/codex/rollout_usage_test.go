package codex

// Fixture session files for the interactive terminal-usage path, in the
// shape real rollouts take: ONE file per thread. The parent thread has
// three model calls in its own file; its subagent runs as a separate
// thread in its own file, named by a session_meta record carrying
// source.subagent.thread_spawn.parent_thread_id. A genuinely different
// thread that merely shares the parent's file never moves the total.
// The terminal cost must be the parent's last cumulative PLUS the
// subagent's own last cumulative — never the parent alone, never a
// per-call count presented as a total.

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

const (
	rolloutFixtureThreadID = "thread-terminal-usage"
	rolloutFixtureSubID    = "thread-subagent-usage"
	rolloutFixtureOtherID  = "thread-other-usage"
)

func writeRolloutFile(t *testing.T, home, threadID string, lines []string) string {
	t.Helper()
	dir := filepath.Join(home, codexSessionStateSubdir, "2026", "10", "09")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-10-09T00-00-00-"+threadID+".jsonl")
	body := ""
	for _, line := range lines {
		body += line + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func subagentMetaLine(threadID, parentID string) string {
	return `{"type":"session_meta","payload":{"id":"` + threadID +
		`","source":{"subagent":{"thread_spawn":{"parent_thread_id":"` + parentID + `"}}}}}`
}

func writeRolloutFixture(t *testing.T, home string) string {
	t.Helper()
	writeRolloutFile(t, home, rolloutFixtureSubID, []string{
		subagentMetaLine(rolloutFixtureSubID, rolloutFixtureThreadID),
		// A subagent's cumulative starts fresh: its first record's
		// usage equals its thread total.
		usageLine(rolloutFixtureSubID, 9000, 1000, 0, 900, 90, 9000, 1000, 0, 900, 90),
		usageLine(rolloutFixtureSubID, 500, 100, 0, 60, 6, 9500, 1100, 0, 960, 96),
	})
	return writeRolloutFile(t, home, rolloutFixtureThreadID, []string{
		`{"type":"session_meta","payload":{"id":"` + rolloutFixtureThreadID + `"}}`,
		usageLine(rolloutFixtureThreadID, 1000, 800, 10, 50, 4, 2000, 1600, 20, 100, 8),
		usageLine(rolloutFixtureThreadID, 1500, 1200, 30, 90, 6, 3000, 2400, 50, 190, 14),
		// A genuinely different thread sharing the parent's file never
		// moves the parent's total.
		usageLine(rolloutFixtureOtherID, 9000, 1000, 0, 400, 40, 9000, 1000, 0, 400, 40),
		`{"type":"event_msg","payload":{"message":"done"}}`,
		`not json at all`,
		usageLine(rolloutFixtureThreadID, 1800, 1500, 40, 120, 9, 3600, 3000, 70, 240, 20),
	})
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
	// out=240 reasoning=20. Input excludes both cache buckets. The other
	// thread's records in the same file never move this total.
	if totals.InputTokens != 530 || totals.CachedInputTokens != 3000 || totals.CacheWriteTokens != 70 ||
		totals.OutputTokens != 240 || totals.ReasoningTokens != 20 {
		t.Fatalf("totals = %+v, want input=530 cached=3000 written=70 output=240 reasoning=20", totals)
	}
}

func TestReadRolloutUsage_OtherThreadInSameFileNeverMovesTheTotal(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	path := writeRolloutFixture(t, home)

	totals := readRolloutUsage(path, rolloutFixtureOtherID)
	if totals == nil || !totals.Found {
		t.Fatal("no totals for the other thread")
	}
	// The other thread's own single record: in=9000 cached=1000 out=400
	// reasoning=40.
	if totals.InputTokens != 8000 || totals.CachedInputTokens != 1000 ||
		totals.OutputTokens != 400 || totals.ReasoningTokens != 40 {
		t.Fatalf("other totals = %+v, want input=8000 cached=1000 output=400 reasoning=40", totals)
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
	// "-<threadID>.jsonl" suffix-matches the evil-prefixed decoy name too,
	// so plant the real file newest and assert it wins by mtime.
	wantPath := writeRolloutFixture(t, home)
	// A still-newer file whose name merely CONTAINS the id — not as the
	// last dash group — must not win: it proves the match is a suffix
	// match, not a substring match. The Contains mutant picks this decoy
	// and goes red. Sleep so its mtime is strictly newest.
	time.Sleep(10 * time.Millisecond)
	containsDecoy := filepath.Join(dir, "rollout-"+rolloutFixtureThreadID+"-extra.jsonl")
	if err := os.WriteFile(containsDecoy, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
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

func TestReadRolloutUsage_UsageOnlyRecordIsSkipped(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	dir := filepath.Join(home, codexSessionStateSubdir, "2026", "10", "09")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A record carrying per-call usage but no cumulative thread total must
	// not move the total: the per-call count is not the thread total, and
	// reporting it as one would undercount while looking exact.
	const thread = "thread-usage-only"
	body := `{"type":"token_usage_record","payload":{"thread_id":"` + thread + `","usage":{"input_tokens":100,` +
		`"cached_input_tokens":10,"cache_write_input_tokens":1,"output_tokens":50,"reasoning_output_tokens":5,"total_tokens":150}}}` + "\n"
	path := filepath.Join(dir, "rollout-2026-10-09T00-00-00-"+thread+".jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if totals := readRolloutUsage(path, thread); totals != nil {
		t.Fatalf("usage-only record totals = %+v, want nil (skipped, not reported as the total)", totals)
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
	// Parent last cumulative (input=530 cached=3000 written=70 output=240
	// reasoning=20) PLUS the subagent's own last cumulative (its first
	// record's usage equals its total, so input=9500-1100=8400 cached=1100
	// output=960 reasoning=96). Reading the parent file alone would
	// report the parent total as complete — a large undercount.
	want := &agent.CostData{
		InputTokens: 8930, CachedInputTokens: 4100, CacheWriteTokens: 70,
		OutputTokens: 1200, ReasoningTokens: 116,
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
	return spawnInteractiveForUsageTestWithSpec(t, bin, boundaryRoot, workdir, mcpServers, extraEnv, beforePTY, agent.Spec{
		SessionName: "chief-of-staff",
		Cwd:         workdir,
		MCPServers:  mcpServers,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
		Env:         nil, // filled below
	})
}

func spawnInteractiveForUsageTestWithSpec(t *testing.T, bin, boundaryRoot, _ string, mcpServers []agent.MCPServerConfig, extraEnv map[string]string, beforePTY func(home string), spec agent.Spec) agent.Handle {
	t.Helper()
	env := map[string]string{
		codexFakeNamedAppServerEnv:         "1",
		codexFakePTYClientCreatesThreadEnv: "1",
		"OPENAI_API_KEY":                   "fixture-key",
	}
	for key, value := range extraEnv {
		env[key] = value
	}
	if spec.Env == nil {
		spec.Env = env
	} else {
		for key, value := range env {
			if _, ok := spec.Env[key]; !ok {
				spec.Env[key] = value
			}
		}
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

// TestSpawnInteractiveAttach_TerminalResultCarriesSessionFileUsage drives
// the attach (ResumeExisting) production path end to end: the session name
// is a thread-id-shaped value whose fixture rollout is pre-planted in the
// spawned home — the re-adoption shape after a daemon restart — and the
// terminal ResultEvent must carry its last cumulative total. The fresh-path
// tests above never exercise spawnNamedInteractivePTY's attach branch
// (currentThreadID = SessionName), so a regression that breaks attach-path
// cost (for example resolving against the wrong home) stays green without
// this test.
func TestSpawnInteractiveAttach_TerminalResultCarriesSessionFileUsage(t *testing.T) {
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
	// A thread-id-shaped session name: the attach path requires it (the
	// resume RPC takes a thread id, never a human-assigned name). The
	// fixture rollout is pre-planted for exactly this id — the re-adoption
	// shape, where the session file already exists when the session starts.
	attachThreadID := testThreadUUID
	plant := func(home string) {
		dir := filepath.Join(home, codexSessionStateSubdir, "2026", "10", "09")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		body := `{"type":"session_meta","payload":{"id":"` + attachThreadID + `"}}` + "\n" +
			usageLine(attachThreadID, 1000, 800, 10, 50, 4, 2000, 1600, 20, 100, 8) + "\n" +
			usageLine(attachThreadID, 1800, 1500, 40, 120, 9, 3600, 3000, 70, 240, 20) + "\n"
		if err := os.WriteFile(filepath.Join(dir, "rollout-2026-10-09T00-00-00-"+attachThreadID+".jsonl"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	spec := agent.Spec{
		SessionName: attachThreadID,
		Cwd:         workdir,
		MCPServers:  mcpServers,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24, ResumeExisting: true},
	}
	h := spawnInteractiveForUsageTestWithSpec(t, self, boundaryRoot, workdir, mcpServers, map[string]string{
		codexFakeNamedAppServerResumeThreadEnv: attachThreadID,
	}, plant, spec)
	result := drainTerminalResult(t, h)
	if !result.Success {
		t.Fatalf("fake codex interactive attach session failed: %+v", result)
	}
	if result.Cost == nil {
		t.Fatal("attach terminal ResultEvent carries no cost; want the session file totals")
	}
	want := &agent.CostData{
		InputTokens: 530, CachedInputTokens: 3000, CacheWriteTokens: 70,
		OutputTokens: 240, ReasoningTokens: 20,
	}
	if *result.Cost != *want {
		t.Fatalf("attach terminal cost = %+v, want %+v", result.Cost, want)
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

func TestRolloutParentThreadID_SubagentNamesItsParent(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	writeRolloutFixture(t, home)

	parentPath := findThreadRollout(home, rolloutFixtureThreadID)
	if got := rolloutParentThreadID(parentPath); got != "" {
		t.Fatalf("parent file's parent = %q, want empty (top-level session)", got)
	}
	subPath := findThreadRollout(home, rolloutFixtureSubID)
	if subPath == "" {
		t.Fatal("no session file for the fixture subagent thread")
	}
	if got := rolloutParentThreadID(subPath); got != rolloutFixtureThreadID {
		t.Fatalf("subagent file's parent = %q, want %q", got, rolloutFixtureThreadID)
	}
	if got := rolloutParentThreadID(filepath.Join(home, "missing.jsonl")); got != "" {
		t.Fatalf("missing file's parent = %q, want empty", got)
	}
}

func TestSubagentThreadIDs_FindsTransitiveDescendantsOnly(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	writeRolloutFixture(t, home)
	// A grandchild names the subagent as its parent: transitive credit must
	// reach it. An unrelated thread names nobody and stays out.
	grandchild := "thread-grandchild-usage"
	unrelated := "thread-unrelated-usage"
	writeRolloutFile(t, home, grandchild, []string{
		subagentMetaLine(grandchild, rolloutFixtureSubID),
		usageLine(grandchild, 100, 10, 0, 20, 2, 100, 10, 0, 20, 2),
	})
	writeRolloutFile(t, home, unrelated, []string{
		`{"type":"session_meta","payload":{"id":"` + unrelated + `"}}`,
		usageLine(unrelated, 700, 70, 0, 80, 8, 700, 70, 0, 80, 8),
	})

	got := subagentThreadIDs(home, rolloutFixtureThreadID)
	want := map[string]bool{rolloutFixtureSubID: true, grandchild: true}
	if len(got) != len(want) {
		t.Fatalf("subagents of %q = %v, want %v", rolloutFixtureThreadID, got, keysOf(want))
	}
	for _, id := range got {
		if !want[id] {
			t.Fatalf("subagents of %q = %v, want %v (unexpected %q)", rolloutFixtureThreadID, got, keysOf(want), id)
		}
	}
	if got := subagentThreadIDs(home, unrelated); len(got) != 0 {
		t.Fatalf("subagents of unrelated thread = %v, want none", got)
	}
	if got := subagentThreadIDs(home, "thread-never-ran"); len(got) != 0 {
		t.Fatalf("subagents of an absent thread = %v, want none", got)
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestRolloutUsageCost_CreditsTransitiveSubagents(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	writeRolloutFixture(t, home)
	grandchild := "thread-grandchild-usage"
	writeRolloutFile(t, home, grandchild, []string{
		subagentMetaLine(grandchild, rolloutFixtureSubID),
		usageLine(grandchild, 100, 10, 0, 20, 2, 100, 10, 0, 20, 2),
	})

	// Parent (input=530 cached=3000 written=70 output=240 reasoning=20) +
	// subagent (input=8400 cached=1100 output=960 reasoning=96) +
	// grandchild (input=90 cached=10 output=20 reasoning=2).
	cost := rolloutUsageCost(home, rolloutFixtureThreadID)
	if cost == nil {
		t.Fatal("no terminal cost for the fixture thread")
	}
	want := &agent.CostData{
		InputTokens: 9020, CachedInputTokens: 4110, CacheWriteTokens: 70,
		OutputTokens: 1220, ReasoningTokens: 118,
	}
	if *cost != *want {
		t.Fatalf("terminal cost = %+v, want %+v", cost, want)
	}
}

func TestReadRolloutUsage_ImpossibleCountsReadAsUnknown(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	// Cache buckets exceeding input, and negative fields, are corruption —
	// not measurement. The last CONSISTENT cumulative wins; a file with no
	// consistent record reads as unknown, never as a negative count on
	// the wire.
	const goodThread = "thread-consistent-usage"
	const badThread = "thread-corrupt-usage"
	writeRolloutFile(t, home, goodThread, []string{
		`{"type":"session_meta","payload":{"id":"` + goodThread + `"}}`,
		usageLine(goodThread, 1000, 800, 10, 50, 4, 2000, 1600, 20, 100, 8),
		// Corrupt tail: cached (900) exceeds input (100).
		`{"type":"token_usage_record","payload":{"thread_id":"` + goodThread + `","usage":{"input_tokens":100,` +
			`"cached_input_tokens":900,"cache_write_input_tokens":0,"output_tokens":50,"reasoning_output_tokens":5,"total_tokens":150},` +
			`"thread_token_usage":{"input_tokens":100,"cached_input_tokens":900,"cache_write_input_tokens":0,` +
			`"output_tokens":50,"reasoning_output_tokens":5,"total_tokens":150}}}`,
		// Negative output is likewise impossible.
		`{"type":"token_usage_record","payload":{"thread_id":"` + goodThread + `","usage":{"input_tokens":100,` +
			`"cached_input_tokens":10,"cache_write_input_tokens":0,"output_tokens":-50,"reasoning_output_tokens":0,"total_tokens":50},` +
			`"thread_token_usage":{"input_tokens":2000,"cached_input_tokens":1600,"cache_write_input_tokens":20,` +
			`"output_tokens":-50,"reasoning_output_tokens":0,"total_tokens":1950}}}`,
	})
	path := findThreadRollout(home, goodThread)
	totals := readRolloutUsage(path, goodThread)
	if totals == nil || !totals.Found {
		t.Fatal("no totals: the corrupt tail must not hide the last consistent cumulative")
	}
	// The first record's cumulative: in=2000 cached=1600 written=20
	// out=100 reasoning=8 → input=380.
	if totals.InputTokens != 380 || totals.CachedInputTokens != 1600 || totals.CacheWriteTokens != 20 ||
		totals.OutputTokens != 100 || totals.ReasoningTokens != 8 {
		t.Fatalf("totals = %+v, want the last consistent cumulative (input=380 cached=1600 written=20 output=100 reasoning=8)", totals)
	}

	writeRolloutFile(t, home, badThread, []string{
		`{"type":"session_meta","payload":{"id":"` + badThread + `"}}`,
		`{"type":"token_usage_record","payload":{"thread_id":"` + badThread + `","usage":{"input_tokens":100,` +
			`"cached_input_tokens":900,"cache_write_input_tokens":0,"output_tokens":50,"reasoning_output_tokens":5,"total_tokens":150},` +
			`"thread_token_usage":{"input_tokens":100,"cached_input_tokens":900,"cache_write_input_tokens":0,` +
			`"output_tokens":50,"reasoning_output_tokens":5,"total_tokens":150}}}`,
	})
	if totals := readRolloutUsage(findThreadRollout(home, badThread), badThread); totals != nil {
		t.Fatalf("all-corrupt totals = %+v, want nil (unreported, not negative)", totals)
	}
	if cost := rolloutUsageCost(home, badThread); cost != nil {
		t.Fatalf("all-corrupt terminal cost = %+v, want nil", cost)
	}
}

func TestThreadIDFromRolloutName_ParsesTimestampedNames(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"rollout-2026-10-09T00-00-00-thread-terminal-usage.jsonl":                "thread-terminal-usage",
		"rollout-2026-10-09T00-00-00-01a0548d-9a06-7a30-a72c-f7c94b8c899c.jsonl": "01a0548d-9a06-7a30-a72c-f7c94b8c899c",
		"rollout-2026-10-09T00-00-00-usage.jsonl":                                "usage",
		"not-a-rollout.jsonl": "",
		"rollout-.jsonl":      "",
	}
	for name, want := range cases {
		if got := threadIDFromRolloutName(name); got != want {
			t.Errorf("threadIDFromRolloutName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestOpenRolloutFile_RefusesNonRegularFiles(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("FIFO fixtures are unix-only")
	}
	dir := t.TempDir()
	fifo := filepath.Join(dir, "rollout-2026-10-09T00-00-00-fifo.jsonl")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	// The open must refuse the FIFO immediately — never block for a
	// writer. The read runs inside the PTY exit path before cleanup, so
	// a hang there would leave the terminal unsent and the session's
	// resources unreleased. Removing the regular-file refusal turns
	// this red (the FIFO opens as a rollout file).
	done := make(chan error, 1)
	go func() {
		f, err := openRolloutFile(fifo)
		if err == nil {
			_ = f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO opened as a rollout file; want refusal")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("openRolloutFile blocked on a FIFO for 5s; want immediate refusal")
	}
	// A FIFO planted where the thread's session file should be reads as
	// unknown (nil cost), never as a hang.
	if totals := readRolloutUsage(fifo, "fifo"); totals != nil {
		t.Fatalf("FIFO totals = %+v, want nil", totals)
	}
}
