// Live-binary evidence for the interactive PTY spawn mode: the production
// Spawn entry point drives the REAL `muse` TUI under a PTY (start, input,
// resize, teardown) with no model turn — the session spawns bare (no
// positional seed, so nothing is ever submitted) and is stopped before it
// can phone home. No network, no cost, no session log.
//
// Scope (read before trusting a green run): gated on `muse` being on PATH
// AND the host login check passing; skips cleanly otherwise. The login
// gate means hosted CI without an operator login skips this file — the
// platform-free smoke lane (echo provider) is the CI gate; this file is
// the local real-binary proof the capability-matrix flip cites. The two
// legs it does NOT prove (host-login gating, a Meta-authenticated turn)
// are covered by interactive_login_test.go and the headless lane
// respectively — see the smoke-lane follow-up note in the PR body.
package muse

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/attachwire"
)

// liveBinaryAvailable skips unless the real host binary resolves AND the
// host login check passes. A login-less host skips — it does not fail —
// because the interactive spawn refuses there by design.
func liveBinaryAvailable(t *testing.T) (*Provider, string) {
	t.Helper()
	bin, err := exec.LookPath("muse")
	if err != nil {
		t.Skip("live interactive test: `muse` not on PATH — skipping")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := CheckHostSessionLogin(ctx, bin); err != nil {
		t.Skipf("live interactive test: host login check refused (%v) — skipping", err)
	}
	p, err := New(Options{MuseBin: bin, TrustWorkspace: true})
	if err != nil {
		t.Fatalf("New(real binary): %v", err)
	}
	return p, bin
}

// TestLive_Interactive_StartInputResizeTeardown spawns the real TUI bare
// (no seed — nothing is submitted, so no model turn ever starts), proves
// input reaches the terminal (the TUI echoes typed input into the output
// stream), resizes, and tears down. Every assertion observes the live
// child through the production Spawn path.
func TestLive_Interactive_StartInputResizeTeardown(t *testing.T) {
	p, _ := liveBinaryAvailable(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{
		Cwd:         t.TempDir(),
		Interactive: &agent.InteractiveSpec{Cols: 100, Rows: 30},
	})
	if err != nil {
		t.Fatalf("Spawn(real TUI): %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })

	ic, ok := h.(agent.InteractiveCapable)
	if !ok {
		t.Fatal("real-TUI handle does not implement agent.InteractiveCapable")
	}
	sess := ic.InteractiveSession()
	if sess == nil {
		t.Fatal("InteractiveSession() returned nil")
	}

	// Start: InitEvent once the PTY child is up.
	select {
	case ev, ok := <-h.Events():
		if !ok {
			t.Fatal("events channel closed before InitEvent")
		}
		if _, isInit := ev.(agent.InitEvent); !isInit {
			t.Fatalf("first event = %T, want agent.InitEvent", ev)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for InitEvent from the real TUI")
	}

	// Input: subscribe, then type a token the TUI echoes back into the
	// output stream. The token is never submitted (no newline that the
	// TUI would act on as a prompt submit... a lone CR is the submit key,
	// so type without it).
	sub, err := sess.Subscribe(0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = sub.Close() }()
	const token = "donmai-live-pty-probe-7f3a"
	if _, err := sess.WriteInput([]byte(token)); err != nil {
		t.Fatalf("WriteInput: %v", err)
	}
	echoed := false
	echoDeadline := time.After(30 * time.Second)
collect:
	for !echoed {
		select {
		case f, ok := <-sub.Frames():
			if !ok {
				break collect
			}
			if f.Type == attachwire.TypeOutput && containsBytes(f.Payload, token) {
				echoed = true
			}
		case <-echoDeadline:
			break collect
		}
	}
	if !echoed {
		t.Errorf("typed token %q never echoed in the live TUI output stream", token)
	}

	// Resize: the host applies the geometry verbatim.
	if err := sess.Resize(120, 40, 0, 0); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	scr, _, err := sess.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if scr.Cols != 120 || scr.Rows != 40 {
		t.Errorf("snapshot geometry = %dx%d, want 120x40", scr.Cols, scr.Rows)
	}

	// Teardown: Stop ends the child; the terminal ResultEvent arrives and
	// the stream closes.
	if err := h.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	teardown := time.After(30 * time.Second)
	gotResult := false
	for !gotResult {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				t.Fatal("events channel closed before a terminal ResultEvent")
			}
			if _, isResult := ev.(agent.ResultEvent); isResult {
				gotResult = true
			}
		case <-teardown:
			t.Fatal("timed out waiting for the terminal ResultEvent after Stop")
		}
	}
}

func containsBytes(payload []byte, token string) bool {
	if len(payload) == 0 {
		return false
	}
	haystack := string(payload)
	for i := 0; i+len(token) <= len(haystack); i++ {
		if haystack[i:i+len(token)] == token {
			return true
		}
	}
	return false
}
