package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// fireCompactHook runs the GENERATED compact script through /bin/sh with
// payload on stdin, exactly as the CLI invokes it.
func fireCompactHook(t *testing.T, c *stopHookChannel, payload string) string {
	t.Helper()
	cmd := exec.Command("/bin/sh", c.path(sessionStartCompactScriptFile)) //nolint:gosec // the script is this test's own fixture
	cmd.Stdin = strings.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("compact script exited non-zero (%v); stderr=%q", err, stderr.String())
	}
	return stdout.String()
}

func compactStdin(source string) string {
	b, _ := json.Marshal(map[string]any{
		"session_id":      "a0fe5c03-fd5c-4113-849c-4f7a81d73ee4",
		"transcript_path": "/tmp/t.jsonl",
		"cwd":             "/tmp/ws",
		"hook_event_name": "SessionStart",
		"source":          source,
	})
	return string(b)
}

// stageCompactChannel builds a channel with optional context and seed.
func stageCompactChannel(t *testing.T, contextText, seed string) *stopHookChannel {
	t.Helper()
	c := newTestStopHookChannel(t)
	if contextText != "" {
		if err := os.WriteFile(c.path(agent.ClaudeLaunchContextFileName), []byte(contextText), 0o600); err != nil {
			t.Fatalf("stage launch context: %v", err)
		}
	}
	if seed != "" {
		if err := os.WriteFile(c.sidecarPath(), []byte(seed), 0o600); err != nil {
			t.Fatalf("seed sidecar: %v", err)
		}
	}
	return c
}

// markedSidecarNotice reads the sidecar through the production parser.
// wantMarked=false expects the file byte-identical to seed.
func markedSidecarNotice(t *testing.T, c *stopHookChannel, seed string, wantMarked bool) []byte {
	t.Helper()
	raw, err := os.ReadFile(c.sidecarPath())
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	if !wantMarked {
		if string(raw) != seed {
			t.Fatalf("non-compact fire rewrote the sidecar: %q", raw)
		}
		return raw
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("sidecar is not JSON: %v (%q)", err, raw)
	}
	if doc["compacted"] != true {
		t.Fatalf("sidecar lost the compaction mark: %q", raw)
	}
	notice, err := agent.ParseClaudeSidecarNotice(raw, "claude-code")
	if err != nil {
		t.Fatalf("ParseClaudeSidecarNotice on the marked sidecar: %v", err)
	}
	if notice == nil || notice.NoticeKind != agent.ContextUsageCompaction {
		t.Fatalf("marked sidecar yielded %+v, want a compaction notice", notice)
	}
	return raw
}

// TestSessionStartCompactHook drives the production compact script: a
// compact fire marks the sidecar and returns the staged context, any other
// source stays silent, and the marker never depends on the context file.
func TestSessionStartCompactHook(t *testing.T) {
	const contextText = "recall: the deploy finished\nnext: cut the release"
	seed := `{"used_percentage": 62.5, "total_input_tokens": 125000}`
	t.Run("compact marks and returns context", func(t *testing.T) {
		c := stageCompactChannel(t, contextText, seed)
		out := fireCompactHook(t, c, compactStdin("compact"))
		raw := markedSidecarNotice(t, c, seed, true)
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("sidecar is not JSON: %v", err)
		}
		if doc["used_percentage"] != 62.5 {
			t.Fatalf("compact fire clobbered live measurements: %q", raw)
		}
		var decoded struct {
			HookSpecificOutput struct {
				HookEventName     string `json:"hookEventName"`
				AdditionalContext string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal([]byte(out), &decoded); err != nil {
			t.Fatalf("compact stdout is not the hook JSON: %v (%q)", err, out)
		}
		if decoded.HookSpecificOutput.HookEventName != "SessionStart" {
			t.Errorf("hookEventName = %q, want SessionStart", decoded.HookSpecificOutput.HookEventName)
		}
		if !strings.Contains(decoded.HookSpecificOutput.AdditionalContext, contextText) {
			t.Errorf("additionalContext = %q, want the context file's text", decoded.HookSpecificOutput.AdditionalContext)
		}
	})
	t.Run("other sources are silent", func(t *testing.T) {
		c := stageCompactChannel(t, "launch memory", seed)
		if out := fireCompactHook(t, c, compactStdin("startup")); out != "" {
			t.Fatalf("startup fire emitted %q; want silence", out)
		}
		markedSidecarNotice(t, c, seed, false)
	})
	t.Run("compact without a file still marks", func(t *testing.T) {
		c := stageCompactChannel(t, "", "")
		if out := fireCompactHook(t, c, compactStdin("compact")); out != "" {
			t.Fatalf("context-less compact fire emitted %q; want silence", out)
		}
		markedSidecarNotice(t, c, "", true)
	})
}

// automemRecorderScript records CLAUDE_CODE_DISABLE_AUTO_MEMORY to dir,
// then prints tail on stdout, for the spawn-mode env tests below.
func automemRecorderScript(dir, tail string) string {
	return "#!/bin/sh\n" +
		"if [ -z \"${CLAUDE_CODE_DISABLE_AUTO_MEMORY+x}\" ]; then\n" +
		"  printf 'unset' > " + shQuote(filepath.Join(dir, "automem-env")) + "\n" +
		"else\n" +
		"  printf 'set:%s' \"$CLAUDE_CODE_DISABLE_AUTO_MEMORY\" > " + shQuote(filepath.Join(dir, "automem-env")) + "\n" +
		"fi\n" + tail
}

func assertAutomemSet(t *testing.T, dir string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dir, "automem-env"))
	if err != nil {
		t.Fatalf("read recorded env: %v", err)
	}
	if string(got) != "set:1" {
		t.Fatalf("child auto-memory env = %q, want set:1", got)
	}
}

// TestAutoMemoryDisabledOnEverySpawnMode drives each production spawn
// entry point: every hosted session disables host-local automatic memory.
func TestAutoMemoryDisabledOnEverySpawnMode(t *testing.T) {
	t.Run("headless", func(t *testing.T) {
		dir := t.TempDir()
		cli := writeFakeCLI(t, "fake-claude-automem.sh", automemRecorderScript(dir,
			`printf '{"type":"system","subtype":"init","session_id":"sess-automem-1"}\n'`+"\n"+
				`printf '{"type":"result","subtype":"success","is_error":false,"num_turns":1}\n'`+"\n"))
		p, err := New(Options{Binary: cli, LookPath: func(name string) (string, error) { return name, nil }})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		h, err := p.Spawn(t.Context(), agent.Spec{Prompt: "hello", Cwd: dir})
		if err != nil {
			t.Fatalf("Spawn: %v", err)
		}
		t.Cleanup(func() { _ = h.Stop(t.Context()) })
		_ = drainAllWithIdle(t, h.Events(), 5*time.Second, 45*time.Second)
		assertAutomemSet(t, dir)
	})

	t.Run("interactive", func(t *testing.T) {
		workdir := t.TempDir()
		p := newFakeInteractiveProvider(t, automemRecorderScript(workdir, ""))
		h, err := p.Spawn(context.Background(), agent.Spec{
			Prompt:      "hello",
			Cwd:         workdir,
			Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
		})
		if err != nil {
			t.Fatalf("Spawn: %v", err)
		}
		waitForExit(t, h)
		assertAutomemSet(t, workdir)
	})

	t.Run("one-shot env", func(t *testing.T) {
		// Complete's child environment is exactly oneShotEnv composed onto
		// the parent: assert the logic directly for both endpoint shapes.
		if env := oneShotEnv(agent.OneShotRequest{Messages: []agent.Message{{Role: "user", Content: "hi"}}}); env[autoMemoryDisableEnvVar] != "1" {
			t.Fatalf("unbound one-shot env = %v, want %s=1", env, autoMemoryDisableEnvVar)
		}
		if env := oneShotEnv(agent.OneShotRequest{Endpoint: &agent.EndpointBinding{Model: "m"}}); env[autoMemoryDisableEnvVar] != "1" {
			t.Fatalf("bound one-shot env = %v, want %s=1", env, autoMemoryDisableEnvVar)
		}
	})
}
