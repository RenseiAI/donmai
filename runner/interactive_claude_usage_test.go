package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/claude"
)

// Drive the real Claude Spawn and interactive supervisor together: usage
// written by the statusline command survives PTY cleanup and reaches the
// runner's terminal report even though the Stop hook was never invoked.
func TestInteractive_ClaudeSidecarWithoutStopReachesResult(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envAttachURL, "")
	t.Setenv(envAttachToken, "")
	dir := t.TempDir()
	binary := filepath.Join(dir, "fake-claude")
	script := `#!/bin/sh
python3 - "$@" <<'PY'
import json, subprocess, sys
args = sys.argv[1:]
settings = json.loads(args[args.index('--settings') + 1])
subprocess.run(settings['statusLine']['command'], shell=True, input=b'{"context_window":{"total_input_tokens":1200,"total_output_tokens":75}}', check=True)
PY
exit 0
`
	if err := os.WriteFile(binary, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	// Match the Unix CLI fixtures: owner read/execute only, no write access.
	if err := syscall.Chmod(binary, 0o500); err != nil {
		t.Fatal(err)
	}
	provider, err := claude.New(claude.Options{Binary: binary, LookPath: func(string) (string, error) { return binary, nil }})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := provider.Spawn(t.Context(), agent.Spec{Cwd: dir, Env: map[string]string{"CLAUDE_CONFIG_DIR": filepath.Join(dir, "config")}, Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handle.Stop(context.Background()) })
	r := minimalRunner(t)
	qw := QueuedWork{}
	qw.SessionID = "sidecar-usage"
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	result, err := r.dispatchInteractive(ctx, handle, dir, qw, &Result{SessionID: qw.SessionID}, noopSink{}, nil, nil, agent.NoticeDeliveryPTYNotice)
	if err != nil {
		t.Fatal(err)
	}
	if result.Cost == nil {
		t.Fatal("runner terminal result lost sidecar usage without a Stop hook")
	}
	want := agent.CostData{InputTokens: 1200, OutputTokens: 75}
	if *result.Cost != want {
		t.Fatalf("Cost = %+v; want %+v", result.Cost, want)
	}
}
