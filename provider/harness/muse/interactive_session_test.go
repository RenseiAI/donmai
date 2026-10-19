package muse

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// TestSpawn_Interactive_InputResizeTeardown proves the full interactive
// contract against the production Spawn entry point: the session starts
// (InitEvent), takes input (bytes written via WriteInput reach the child),
// honors resize (Snapshot reflects the new geometry), and tears down
// (Stop ends the child with a terminal ResultEvent and closes the stream).
// The fake TUI appends every stdin byte to input.txt and exits when it
// sees the quit token — so each assertion observes the child, not the
// harness wrapper. RED proof: route Spawn away from ptycli (or break the
// WriteInput/Resize/Stop passthrough) and the file stays empty, the
// geometry never moves, or no terminal event arrives.
func TestSpawn_Interactive_InputResizeTeardown(t *testing.T) {
	workdir := t.TempDir()
	p := newFakeInteractiveProvider(t, `
touch "$PWD/input.txt"
while IFS= read -r -n1 ch; do
  printf '%s' "$ch" >> "$PWD/input.txt"
  if grep -q QUIT "$PWD/input.txt" 2>/dev/null; then exit 0; fi
done
exit 0
`)

	h, err := p.Spawn(context.Background(), agent.Spec{
		Prompt:      "hello",
		Cwd:         workdir,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	ic, ok := h.(agent.InteractiveCapable)
	if !ok {
		t.Fatal("handle does not implement agent.InteractiveCapable")
	}
	sess := ic.InteractiveSession()
	if sess == nil {
		t.Fatal("InteractiveSession() returned nil")
	}

	// Start: the InitEvent arrives once the PTY child is up.
	deadline := time.After(20 * time.Second)
	select {
	case ev, ok := <-h.Events():
		if !ok {
			t.Fatal("events channel closed before InitEvent")
		}
		if _, isInit := ev.(agent.InitEvent); !isInit {
			t.Fatalf("first event = %T, want agent.InitEvent", ev)
		}
	case <-deadline:
		t.Fatal("timed out waiting for InitEvent")
	}

	// Input: written bytes reach the child verbatim. The quit token goes
	// LAST: the child exits on QUIT, and a resize after exit refuses.
	if _, err := sess.WriteInput([]byte("hello-input\r")); err != nil {
		t.Fatalf("WriteInput: %v", err)
	}
	waitFor := func(want string) {
		t.Helper()
		limit := time.Now().Add(15 * time.Second)
		for {
			body, err := os.ReadFile(filepath.Join(workdir, "input.txt"))
			if err == nil && strings.Contains(string(body), want) {
				return
			}
			if time.Now().After(limit) {
				t.Fatalf("child never received %q (input.txt=%q, err=%v)", want, string(body), err)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitFor("hello-input")

	// Resize: the host applies the geometry verbatim; the snapshot
	// reflects it.
	if err := sess.Resize(100, 40, 0, 0); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	scr, _, err := sess.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if scr.Cols != 100 || scr.Rows != 40 {
		t.Errorf("snapshot geometry = %dx%d, want 100x40", scr.Cols, scr.Rows)
	}

	// Quit only after input + resize are proven: the child exits, which
	// drives the terminal ResultEvent below.
	if _, err := sess.WriteInput([]byte("QUIT")); err != nil {
		t.Fatalf("WriteInput quit token: %v", err)
	}
	waitFor("QUIT")

	// Teardown: the terminal ResultEvent arrives and the stream closes.
	// The child exits on its own after QUIT; Stop is idempotent cover.
	gotResult := false
	limit := time.After(20 * time.Second)
loop:
	for !gotResult {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				break loop
			}
			if res, isResult := ev.(agent.ResultEvent); isResult {
				gotResult = true
				if !res.Success {
					t.Errorf("terminal ResultEvent success=false, errors=%v", res.Errors)
				}
			}
		case <-limit:
			t.Fatal("timed out waiting for the terminal ResultEvent")
		}
	}
	if !gotResult {
		t.Fatal("never observed a terminal ResultEvent")
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// After teardown the stream is closed: no further events, no hang.
	drained := false
	for !drained {
		select {
		case _, ok := <-h.Events():
			if !ok {
				drained = true
			}
		case <-time.After(10 * time.Second):
			t.Fatal("events channel never closed after teardown")
		}
	}
}

// TestSpawn_Interactive_ZeroGeometryFallsBackToDefaults pins the
// defensive branch: a nil-geometry interactive spec still spawns (the
// driver falls back to its own defaults) rather than refusing.
func TestSpawn_Interactive_ZeroGeometryFallsBackToDefaults(t *testing.T) {
	p := newFakeInteractiveProvider(t, `exit 0`)

	h, err := p.Spawn(context.Background(), agent.Spec{
		Prompt:      "hello",
		Cwd:         t.TempDir(),
		Interactive: &agent.InteractiveSpec{},
	})
	if err != nil {
		t.Fatalf("Spawn with zero geometry: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })

	ic, ok := h.(agent.InteractiveCapable)
	if !ok || ic.InteractiveSession() == nil {
		t.Fatal("handle has no live interactive surface")
	}
	deadline := time.After(20 * time.Second)
	for {
		select {
		case _, ok := <-h.Events():
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for fake TUI exit")
		}
	}
}

// TestSpawn_Interactive_AmbientMetaKeyDoesNotReachPTYChild pins the B1
// contract through the production Spawn entry point: with an ambient
// META_API_KEY in the launching process, the interactive PTY child must
// not see it — serveEnv strips the key unless the Spec sets it, and the
// spawn asks the PTY host for an exact environment so the parent's own
// environment cannot ride back in around that strip. The fake TUI dumps
// its environment to a file; the test asserts no META_API_KEY line
// survives. RED proof: drop ExactEnv from the spawn call and the ambient
// key layers back in under the overrides (ptyhost composeEnv), failing
// the assertion below.
func TestSpawn_Interactive_AmbientMetaKeyDoesNotReachPTYChild(t *testing.T) {
	t.Setenv("META_API_KEY", "ambient-operator-key")
	workdir := t.TempDir()
	p := newFakeInteractiveProvider(t, `env > "$PWD/child.env"; exit 0`)

	h, err := p.Spawn(context.Background(), agent.Spec{
		Prompt:      "hello",
		Cwd:         workdir,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })

	// Drain to the terminal ResultEvent: the fake TUI writes the dump,
	// then exits on its own.
	deadline := time.After(20 * time.Second)
	for {
		select {
		case _, ok := <-h.Events():
			if !ok {
				goto exited
			}
		case <-deadline:
			t.Fatal("timed out waiting for fake TUI exit")
		}
	}
exited:
	raw, err := os.ReadFile(filepath.Join(workdir, "child.env"))
	if err != nil {
		t.Fatalf("read child env dump: %v", err)
	}
	for line := range strings.Lines(string(raw)) {
		if strings.HasPrefix(line, "META_API_KEY=") {
			t.Errorf("ambient META_API_KEY reached the interactive PTY child: %q", line)
		}
	}
}
