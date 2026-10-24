package claude

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// The fake REPL writes the same on-disk usage sources as Claude, then waits
// to be killed. It never invokes Stop, and discovers its identity and
// sidecar destination from the actual production Spawn arguments.
const noStopUsageREPL = `python3 - "$@" <<'PY'
import json, os, pathlib, sys
args = sys.argv[1:]
sid = args[args.index('--session-id') + 1] if '--session-id' in args else '00000000-0000-4000-8000-000000000000'
mode = os.environ['TEST_USAGE_MODE']
config = os.environ.get('CLAUDE_CONFIG_DIR') or str(pathlib.Path.home() / '.claude')
root = pathlib.Path(config) / 'projects' / '-fixture-workspace'
root.mkdir(parents=True, exist_ok=True)
line = {'type':'assistant', 'message':{'id':'message-one','usage':{'input_tokens':100,'output_tokens':20,'cache_read_input_tokens':30,'cache_creation_input_tokens':40}}}
if mode in ('transcript', 'both', 'untrusted', 'subagents', 'ambiguous', 'empty'):
    if mode == 'untrusted':
        line['message']['usage']['input_tokens'] = -1
    (root / (sid + '.jsonl')).write_text('' if mode == 'empty' else json.dumps(line) + '\n' + json.dumps(line) + '\n')
    if mode == 'subagents':
        subs = root / sid / 'subagents'
        subs.mkdir(parents=True)
        line['message']['id'] = 'subagent-message'
        (subs / 'agent-one.jsonl').write_text(json.dumps(line) + '\n')
    if mode == 'ambiguous':
        other = root.parent / '-other-workspace'
        other.mkdir()
        (other / (sid + '.jsonl')).write_text(json.dumps(line) + '\n')
# A concurrent conversation must never become this session's usage.
(root / 'other-session.jsonl').write_text(json.dumps(line) + '\n')
if mode not in ('missing', 'transcript', 'subagents', 'ambiguous'):
    settings = json.loads(args[args.index('--settings') + 1])
    import subprocess
    cw = {'total_input_tokens':700, 'total_output_tokens':80}
    if mode == 'cost-only': cw = {}
    if mode == 'context-only': cw = {'used_percentage':50, 'context_window_size':200000}
    if mode == 'zero': cw = {'total_input_tokens':0, 'total_output_tokens':0}
    if mode == 'negative': cw = {'total_input_tokens':-1, 'total_output_tokens':80}
    subprocess.run(settings['statusLine']['command'], shell=True, input=json.dumps({'context_window':cw, 'cost':{'total_cost_usd':0.25}}).encode(), check=True)
    if mode in ('context-only', 'zero', 'negative', 'cost-invalid', 'cost-missing', 'cost-string', 'cost-overflow'):
        cost = {'total_cost_usd':0}
        if mode == 'negative': cost = {'total_cost_usd':-1}
        if mode == 'cost-invalid': cost = {'total_cost_usd':float('nan')}
        if mode == 'cost-missing': cost = {}
        if mode == 'cost-string': cost = {'total_cost_usd':'0.25'}
        if mode == 'cost-overflow': cost = {'total_cost_usd':float('inf')}
        subprocess.run(settings['statusLine']['command'], shell=True, input=json.dumps({'context_window':cw, 'cost':cost}).encode(), check=True)
    if mode == 'two-turn':
        # Second response shrinks the context; spend still grows. Repeated
        # renders of the same response must not add its cumulative cost twice.
        cw = {'total_input_tokens':300, 'total_output_tokens':20, 'used_percentage':12.5, 'context_window_size':200000}
        for _ in range(2):
            subprocess.run(settings['statusLine']['command'], shell=True, input=json.dumps({'context_window':cw, 'cost':{'total_cost_usd':0.75}}).encode(), check=True)
    if mode in ('malformed', 'fifo'):
        import shlex
        sidecar = pathlib.Path(shlex.split(settings['statusLine']['command'])[-1]).with_name('statusline-sidecar.json')
        if mode == 'malformed': sidecar.write_text('{')
        else:
            sidecar.unlink()
            os.mkfifo(sidecar)
pathlib.Path(os.environ['TEST_USAGE_READY']).write_text(sid)
PY
exec sleep 30
`

func TestSpawnInteractive_ExitUsageWithoutStop(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		mode     string
		homeMode string
		want     *agent.CostData
	}{
		{name: "transcript before first Stop", mode: "transcript", want: &agent.CostData{InputTokens: 100, OutputTokens: 20, CachedInputTokens: 30, CacheWriteTokens: 40, NumTurns: 1}},
		{name: "transcript wins over sidecar", mode: "both", want: &agent.CostData{InputTokens: 100, OutputTokens: 20, CachedInputTokens: 30, CacheWriteTokens: 40, NumTurns: 1}},
		{name: "fallback includes subagent spend", mode: "subagents", want: &agent.CostData{InputTokens: 200, OutputTokens: 40, CachedInputTokens: 60, CacheWriteTokens: 80, NumTurns: 2}},
		{name: "ambiguous transcript stays unknown", mode: "ambiguous"},
		{name: "empty transcript falls back to sidecar", mode: "empty", want: &agent.CostData{TotalCostUsd: 0.25}},
		{name: "sidecar survives cleanup", mode: "sidecar", want: &agent.CostData{TotalCostUsd: 0.25}},
		{name: "cost without context", mode: "cost-only", want: &agent.CostData{TotalCostUsd: 0.25}},
		{name: "two-turn cumulative spend", mode: "two-turn", want: &agent.CostData{TotalCostUsd: 0.75}},
		{name: "invalid cost stays unknown", mode: "cost-invalid"},
		{name: "missing cost stays unknown", mode: "cost-missing"},
		{name: "string cost stays unknown", mode: "cost-string"},
		{name: "overflow cost stays unknown", mode: "cost-overflow"},
		{name: "neither source stays unknown", mode: "missing"},
		{name: "context size is not spend", mode: "context-only"},
		{name: "zero reading stays unknown", mode: "zero"},
		{name: "invalid sidecar stays unknown", mode: "negative"},
		{name: "malformed sidecar stays unknown", mode: "malformed"},
		{name: "sidecar FIFO cannot block exit", mode: "fifo"},
		{name: "relative config stays unknown", mode: "transcript", homeMode: "relative"},
		{name: "session config overrides process", mode: "transcript", homeMode: "overridden", want: &agent.CostData{InputTokens: 100, OutputTokens: 20, CachedInputTokens: 30, CacheWriteTokens: 40, NumTurns: 1}},
		{name: "corrupt transcript stays unknown", mode: "untrusted"},
		{name: "inherited config directory", mode: "transcript", homeMode: "inherited", want: &agent.CostData{InputTokens: 100, OutputTokens: 20, CachedInputTokens: 30, CacheWriteTokens: 40, NumTurns: 1}},
		{name: "session home directory", mode: "transcript", homeMode: "session-home", want: &agent.CostData{InputTokens: 100, OutputTokens: 20, CachedInputTokens: 30, CacheWriteTokens: 40, NumTurns: 1}},
		{name: "session clears inherited config directory", mode: "transcript", homeMode: "cleared-config", want: &agent.CostData{InputTokens: 100, OutputTokens: 20, CachedInputTokens: 30, CacheWriteTokens: 40, NumTurns: 1}},
		{name: "default config directory", mode: "transcript", homeMode: "default", want: &agent.CostData{InputTokens: 100, OutputTokens: 20, CachedInputTokens: 30, CacheWriteTokens: 40, NumTurns: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := t.TempDir()
			ready := filepath.Join(t.TempDir(), "ready")
			env := map[string]string{claudeConfigDirEnv: config, "TEST_USAGE_MODE": tc.mode, "TEST_USAGE_READY": ready}
			if tc.homeMode == "session-home" || tc.homeMode == "cleared-config" {
				t.Setenv("HOME", t.TempDir())
				t.Setenv(claudeConfigDirEnv, "")
				env["HOME"] = config
				delete(env, claudeConfigDirEnv)
				if tc.homeMode == "cleared-config" {
					t.Setenv(claudeConfigDirEnv, t.TempDir())
					env[claudeConfigDirEnv] = ""
				}
			}
			if tc.homeMode == "relative" {
				env[claudeConfigDirEnv] = "relative-config"
			}
			if tc.homeMode == "overridden" {
				t.Setenv(claudeConfigDirEnv, t.TempDir())
			}
			if tc.homeMode == "inherited" {
				t.Setenv(claudeConfigDirEnv, config)
				delete(env, claudeConfigDirEnv)
			}
			if tc.homeMode == "default" {
				t.Setenv(claudeConfigDirEnv, "")
				t.Setenv("HOME", config)
				delete(env, claudeConfigDirEnv)
			}
			p := newFakeInteractiveProvider(t, noStopUsageREPL)
			h, err := p.Spawn(t.Context(), agent.Spec{Cwd: t.TempDir(), Env: env, Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = h.Stop(context.Background()) })
			deadline := time.NewTimer(10 * time.Second)
			defer deadline.Stop()
			poll := time.NewTicker(10 * time.Millisecond)
			defer poll.Stop()
			for {
				if _, err := os.Stat(ready); err == nil {
					break
				}
				select {
				case <-deadline.C:
					t.Fatal("REPL never wrote usage sources")
				case <-poll.C:
				}
			}
			wrapper := h.(*interactiveHandle)
			if _, err := os.Stat(wrapper.notices.path(stopHookTranscriptFile)); !os.IsNotExist(err) {
				t.Fatalf("Stop locator unexpectedly exists: %v", err)
			}
			if tc.mode == "two-turn" {
				raw, err := os.ReadFile(wrapper.ContextUsageSidecarPath())
				if err != nil {
					t.Fatal(err)
				}
				notice, err := agent.ParseClaudeSidecarNotice(raw, "claude")
				if err != nil {
					t.Fatal(err)
				}
				if notice == nil || notice.InputTokens == nil || *notice.InputTokens != 300 ||
					notice.OutputTokens == nil || *notice.OutputTokens != 20 ||
					notice.UsedPercent == nil || *notice.UsedPercent != 12.5 ||
					notice.ContextWindowSize == nil || *notice.ContextWindowSize != 200000 {
					t.Fatalf("context notice = %+v; want latest response context", notice)
				}
			}
			if err := h.Stop(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(wrapper.notices.dir); !os.IsNotExist(err) {
				t.Fatalf("drop directory survived cleanup: %v", err)
			}
			for {
				select {
				case ev, ok := <-h.Events():
					if !ok {
						t.Fatal("no terminal result")
					}
					result, ok := ev.(agent.ResultEvent)
					if !ok {
						continue
					}
					if tc.want == nil {
						if result.Cost != nil {
							t.Fatalf("cost = %+v; want nil", result.Cost)
						}
						return
					}
					if result.Cost == nil {
						t.Fatal("exit usage is nil without a Stop locator")
					}
					if *result.Cost != *tc.want {
						t.Fatalf("cost = %+v; want %+v", result.Cost, tc.want)
					}
					return
				case <-deadline.C:
					t.Fatal("terminal event did not arrive")
				}
			}
		})
	}
}

// TestSpawnInteractive_LiveUsageIncludesSidecarCost is AC-1: transcript usage
// and statusline cost written while running surface as tokens plus one
// cost-only LlmCallEvent before Done; the terminal keeps transcript totals.
func TestSpawnInteractive_LiveUsageIncludesSidecarCost(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatal(err)
	}
	config := t.TempDir()
	ready := filepath.Join(t.TempDir(), "ready")
	env := map[string]string{claudeConfigDirEnv: config, "TEST_USAGE_MODE": "both", "TEST_USAGE_READY": ready}
	p := newFakeInteractiveProvider(t, noStopUsageREPL)
	h, err := p.Spawn(t.Context(), agent.Spec{Cwd: t.TempDir(), Env: env, Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("REPL never wrote usage sources")
		case <-poll.C:
		}
	}
	wrapper := h.(*interactiveHandle)
	var tokenEv, costEv *agent.LlmCallEvent
	liveDeadline := time.NewTimer(10 * time.Second)
	defer liveDeadline.Stop()
collect:
	for tokenEv == nil || costEv == nil {
		select {
		case ev, ok := <-wrapper.Events():
			if !ok {
				t.Fatal("Events closed before live usage observed")
			}
			if call, ok := ev.(agent.LlmCallEvent); ok {
				if call.TurnCompleted && call.InputTokens == 100 {
					cp := call
					tokenEv = &cp
				}
				if !call.TurnCompleted && call.ObservedCostUsd != nil {
					cp := call
					costEv = &cp
				}
			}
		case <-liveDeadline.C:
			break collect
		}
	}
	if tokenEv == nil || tokenEv.OutputTokens != 20 || tokenEv.CachedInputTokens != 30 || tokenEv.CacheWriteTokens != 40 {
		t.Fatalf("live token event = %+v; want transcript usage", tokenEv)
	}
	if tokenEv.UsageSource != agent.LlmUsageProvider || tokenEv.ObservedCostUsd != nil {
		t.Fatalf("token event must be provider without cost: %+v", *tokenEv)
	}
	if costEv == nil {
		t.Fatal("no cost-only sidecar event before Done")
	}
	if got := *costEv.ObservedCostUsd; got < 0.249999 || got > 0.250001 {
		t.Fatalf("live cost delta = %v; want 0.25", got)
	}
	if costEv.UsageSource != agent.LlmUsageProvider || costEv.TurnCompleted || costEv.Synthetic {
		t.Fatalf("cost-only event must be provider, no turn, non-synthetic: %+v", *costEv)
	}
	if costEv.InputTokens != 0 || costEv.OutputTokens != 0 || costEv.CachedInputTokens != 0 || costEv.CacheWriteTokens != 0 {
		t.Fatalf("cost-only event invents tokens: %+v", *costEv)
	}
	if err := h.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				t.Fatal("no terminal result")
			}
			if result, ok := ev.(agent.ResultEvent); ok {
				want := agent.CostData{InputTokens: 100, OutputTokens: 20, CachedInputTokens: 30, CacheWriteTokens: 40, NumTurns: 1}
				if result.Cost == nil || *result.Cost != want {
					t.Fatalf("terminal cost = %+v; want %+v", result.Cost, want)
				}
				return
			}
		case <-deadline.C:
			t.Fatal("terminal event did not arrive")
		}
	}
}

// TestInteractiveUsageTailer_SidecarCostEmitsDeltasOnly is AC-4: 0.25, 0.25,
// 0.10, 0.75 emit exactly 0.25 then 0.50; invalid readings move no baseline.
// sweepBoth and flushBoth carry the same delta beside transcript usage.
func TestInteractiveUsageTailer_SidecarCostEmitsDeltasOnly(t *testing.T) {
	t.Parallel()
	c := newTestStopHookChannel(t)
	tailer := newInteractiveUsageTailer(c)
	writeSidecar := func(body string) {
		t.Helper()
		if err := os.WriteFile(c.sidecarPath(), []byte(body), 0o600); err != nil {
			t.Fatalf("write sidecar: %v", err)
		}
	}
	checkCostOnly := func(ev agent.LlmCallEvent, want float64) {
		t.Helper()
		if ev.ObservedCostUsd == nil {
			t.Fatalf("no price: %+v", ev)
		}
		if got := *ev.ObservedCostUsd; got < want-1e-9 || got > want+1e-9 {
			t.Fatalf("delta = %v; want %v", got, want)
		}
		if ev.UsageSource != agent.LlmUsageProvider || ev.TurnCompleted || ev.Synthetic {
			t.Fatalf("must be provider cost-only: %+v", ev)
		}
		if ev.InputTokens != 0 || ev.OutputTokens != 0 || ev.CachedInputTokens != 0 || ev.CacheWriteTokens != 0 {
			t.Fatalf("cost-only invents tokens: %+v", ev)
		}
	}
	if _, ok := tailer.sweepSidecarCost(); ok {
		t.Fatal("missing sidecar emitted")
	}
	writeSidecar(`{"total_cost_usd":0.25}`)
	ev, ok := tailer.sweepSidecarCost()
	if !ok {
		t.Fatal("first 0.25 emitted nothing")
	}
	checkCostOnly(ev, 0.25)
	writeSidecar(`{"total_cost_usd":0.25}`)
	if _, ok := tailer.sweepSidecarCost(); ok {
		t.Fatal("repeat 0.25 re-emitted")
	}
	writeSidecar(`{"total_cost_usd":0.10}`)
	if _, ok := tailer.sweepSidecarCost(); ok {
		t.Fatal("regressing 0.10 emitted")
	}
	writeSidecar(`{"total_cost_usd":0.75}`)
	ev, ok = tailer.sweepSidecarCost()
	if !ok {
		t.Fatal("0.75 catch-up emitted nothing")
	}
	checkCostOnly(ev, 0.50)
	for _, body := range []string{`{"total_cost_usd":0}`, `{"total_cost_usd":-1}`, `{"total_cost_usd": NaN}`, `{"total_cost_usd": Infinity}`, `{`, `{}`, `{"total_cost_usd":"0.25"}`, ``} {
		writeSidecar(body)
		if _, ok := tailer.sweepSidecarCost(); ok {
			t.Fatalf("invalid %q emitted", body)
		}
	}
	writeSidecar(`{"total_cost_usd":1.0}`)
	ev, ok = tailer.sweepSidecarCost()
	if !ok {
		t.Fatal("1.0 after invalids emitted nothing")
	}
	checkCostOnly(ev, 0.25)
	// sweepBoth pairs one token turn with one cost delta; flushBoth emits a
	// trailing sidecar write once.
	c2 := newTestStopHookChannel(t)
	path := writeTranscript(t, []string{usageLine(t, "msg-1", 100, 20, 1000, 50)})
	fireHook(t, c2, hookStdin(path, false))
	if err := os.WriteFile(c2.sidecarPath(), []byte(`{"total_cost_usd":0.25}`), 0o600); err != nil {
		t.Fatal(err)
	}
	events := newInteractiveUsageTailer(c2).sweepBoth()
	var sawTokens, sawCost bool
	for _, e := range events {
		if call, ok := e.(agent.LlmCallEvent); ok {
			if call.TurnCompleted && call.InputTokens == 100 {
				sawTokens = true
			}
			if !call.TurnCompleted && call.ObservedCostUsd != nil {
				sawCost = true
			}
		}
	}
	if !sawTokens || !sawCost {
		t.Fatalf("sweepBoth = %+v; want tokens plus cost delta", events)
	}
	c3 := newTestStopHookChannel(t)
	p3 := writeTranscript(t, []string{usageLine(t, "msg-1", 100, 20, 1000, 50)})
	fireHook(t, c3, hookStdin(p3, false))
	t3 := newInteractiveUsageTailer(c3)
	_ = t3.sweepBoth()
	if err := os.WriteFile(c3.sidecarPath(), []byte(`{"total_cost_usd":0.40}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var got []agent.Event
	t3.flushBoth(func(ev agent.Event) { got = append(got, ev) })
	n := 0
	for _, e := range got {
		if call, ok := e.(agent.LlmCallEvent); ok && call.ObservedCostUsd != nil {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("flushBoth emitted %d cost deltas; want 1", n)
	}
}
