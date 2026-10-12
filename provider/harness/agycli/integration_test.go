package agycli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// TestLive_AgyEndToEnd drives the REAL `agy` binary under the host's login
// session, exercising the full provider path (pty + stdout spine + result-
// envelope injection + on-disk transcript enrichment).
//
// It is gated: it SKIPS unless AGYCLI_LIVE=1 is set AND `agy` is on PATH and
// logged in. It consumes the user's agy subscription quota, so it is never part
// of the default `go test` run (CI, pre-commit). Run manually:
//
//	AGYCLI_LIVE=1 GOWORK=off go test -run TestLive_AgyEndToEnd ./provider/harness/agycli/ -v
func TestLive_AgyEndToEnd(t *testing.T) {
	if os.Getenv("AGYCLI_LIVE") == "" {
		t.Skip("set AGYCLI_LIVE=1 to run against the real agy under the host login (consumes quota)")
	}
	p, err := New(Options{}) // real `agy`, real ~/.gemini
	if err != nil {
		t.Skipf("agy unavailable: %v", err)
	}

	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "NOTES.txt"), []byte("The secret word is: ZEBRA-42\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	prompt := "Read the file NOTES.txt in the current working directory and report the secret word it contains. " +
		"Then print WORK_RESULT:passed on its own line."
	h, err := p.Spawn(ctx, agent.Spec{
		Prompt: prompt,
		Cwd:    cwd,
	})
	if err != nil {
		t.Fatalf("Spawn(real agy): %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()
	concrete, ok := h.(*Handle)
	if !ok {
		t.Fatalf("real Spawn handle = %T, want *Handle", h)
	}
	canonical, err := canonicalWorktree(cwd)
	if err != nil {
		t.Fatal(err)
	}
	wantArgv := []string{p.binary, "-p", prompt + strings.TrimSuffix(resultEnvelopeInstruction, "\n"), "--dangerously-skip-permissions", "--add-dir", canonical}
	if strings.Join(concrete.cmd.Args, "\x00") != strings.Join(wantArgv, "\x00") {
		t.Fatalf("real agy argv = %#v, want %#v", concrete.cmd.Args, wantArgv)
	}
	if concrete.cmd.Dir != canonical {
		t.Fatalf("real agy cmd.Dir = %q, want %q", concrete.cmd.Dir, canonical)
	}

	var (
		text    strings.Builder
		result  *agent.ResultEvent
		toolUse int
	)
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				goto done
			}
			switch e := ev.(type) {
			case agent.AssistantTextEvent:
				text.WriteString(e.Text + "\n")
			case agent.ToolUseEvent:
				toolUse++
				t.Logf("tool_use: %s", e.ToolName)
			case agent.ResultEvent:
				r := e
				result = &r
				goto done
			}
		case <-ctx.Done():
			t.Fatalf("live agy run timed out; partial text=%q", text.String())
		}
	}
done:
	full := text.String()
	t.Logf("=== agy assistant text ===\n%s", full)
	t.Logf("=== tool_use events from transcript: %d ===", toolUse)
	if result != nil {
		t.Logf("=== result: success=%v msg=%q ===", result.Success, result.Message)
	}

	if !strings.Contains(full, "ZEBRA-42") {
		t.Errorf("expected the secret word ZEBRA-42 in agy's output; got %q", full)
	}
	if result == nil || !result.Success {
		t.Errorf("expected a successful terminal ResultEvent, got %#v", result)
	}
	// Transcript enrichment SHOULD surface at least one tool_use (agy reads the
	// file). Not fatal — it is best-effort and version-fragile.
	if toolUse == 0 {
		t.Logf("WARN: no tool_use events recovered from transcript (enrichment degraded)")
	}
}

// TestLive_AgyKeyedEndToEnd drives the REAL `agy` binary in API-key mode:
// GEMINI_API_KEY from the environment selects the keyed spawn, which must
// run under an isolated HOME (the operator's real ~/.gemini stays
// untouched) and bill the session key.
//
// It is gated: it SKIPS unless AGYCLI_LIVE_KEYED=1 is set AND
// GEMINI_API_KEY is set AND `agy` (>= the API-key-mode floor) is on PATH.
// It consumes the session key's metered quota, so it is never part of the
// default `go test` run (CI, pre-commit). Run manually:
//
//	AGYCLI_LIVE_KEYED=1 GEMINI_API_KEY=... GOWORK=off go test -run TestLive_AgyKeyedEndToEnd ./provider/harness/agycli/ -v
func TestLive_AgyKeyedEndToEnd(t *testing.T) {
	if os.Getenv("AGYCLI_LIVE_KEYED") == "" {
		t.Skip("set AGYCLI_LIVE_KEYED=1 with GEMINI_API_KEY to run the real keyed agy (consumes metered quota)")
	}
	key := os.Getenv(EnvGeminiAPIKey)
	if key == "" {
		t.Skipf("no %s in env", EnvGeminiAPIKey)
	}
	p, err := New(Options{}) // real `agy`, API-key-mode probe
	if err != nil {
		t.Skipf("agy unavailable: %v", err)
	}

	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "NOTES.txt"), []byte("The secret word is: ZEBRA-42\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	h, err := p.Spawn(ctx, agent.Spec{
		Prompt: "Read the file NOTES.txt in the current working directory and report the secret word it contains.",
		Cwd:    cwd,
		Env:    map[string]string{EnvGeminiAPIKey: key},
	})
	if err != nil {
		t.Fatalf("Spawn(real keyed agy): %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()
	concrete, ok := h.(*Handle)
	if !ok {
		t.Fatalf("real Spawn handle = %T, want *Handle", h)
	}
	if concrete.keyedHome == "" {
		t.Fatal("keyed spawn has no isolated HOME")
	}
	if _, err := os.Stat(filepath.Join(concrete.keyedHome, keyedSettingsRelPath)); err != nil {
		t.Fatalf("isolated settings.json missing during the run: %v", err)
	}

	var (
		text   strings.Builder
		result *agent.ResultEvent
	)
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				goto done
			}
			switch e := ev.(type) {
			case agent.AssistantTextEvent:
				text.WriteString(e.Text + "\n")
			case agent.ResultEvent:
				r := e
				result = &r
				goto done
			}
		case <-ctx.Done():
			t.Fatalf("live keyed agy run timed out; partial text=%q", text.String())
		}
	}
done:
	if !strings.Contains(text.String(), "ZEBRA-42") {
		t.Errorf("expected the secret word ZEBRA-42 in agy's output; got %q", text.String())
	}
	if result == nil || !result.Success {
		t.Errorf("expected a successful terminal ResultEvent, got %#v", result)
	}
	if _, err := os.Stat(concrete.keyedHome); !os.IsNotExist(err) {
		t.Errorf("isolated HOME %q still exists after session end", concrete.keyedHome)
	}
}
