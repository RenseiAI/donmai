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
