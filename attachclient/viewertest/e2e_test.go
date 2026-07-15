package viewertest_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachclient"
	"github.com/RenseiAI/donmai/attachclient/attachtest"
	"github.com/RenseiAI/donmai/attachclient/viewertest"
	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/ptyhost"
)

// TestFixtureEndToEnd proves the whole viewer-side assert loop through a REAL
// PTY: spawn the vtfixture TUI in a ptyhost session → run the attachclient host
// leg → attachtest stub relay → a viewertest Driver drives input and asserts
// screen state across alt-screen enter/exit and a cursor move, decoding
// authoritative Snapshot frames off the wire.
func TestFixtureEndToEnd(t *testing.T) {
	bin := buildFixture(t)

	// 1. Real PTY session running the deterministic fixture at 80x24, epoch 1.
	sess, err := ptyhost.Spawn(ptyhost.Spec{
		Command: []string{bin},
		Cols:    80,
		Rows:    24,
		Epoch:   1,
		Logger:  discardLogger(),
	})
	if err != nil {
		t.Fatalf("spawn fixture: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = sess.Stop(ctx)
	})

	// 2. Stub relay.
	relay := attachtest.New(attachtest.Config{RoomID: "room-1"})
	if err := relay.Start(); err != nil {
		t.Fatalf("relay start: %v", err)
	}
	t.Cleanup(func() { _ = relay.Close() })

	// 3. Host leg: forward the live session's frames to the relay.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	hostTok := mkHostToken("sess-1", 1, "host-jti-1")
	done := make(chan error, 1)
	go func() {
		done <- attachclient.RunHost(ctx, attachclient.HostConfig{
			AttachURL:         relay.BaseWSURL(),
			TokenSource:       func(context.Context) (string, error) { return hostTok, nil },
			Session:           sessAdapter{sess},
			BackoffMin:        5 * time.Millisecond,
			BackoffMax:        50 * time.Millisecond,
			FinalScreenWindow: 300 * time.Millisecond,
			Logger:            discardLogger(),
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("RunHost did not return after cancel")
		}
	})

	waitBound(t, relay)

	// 4. Driver viewer (driver role → holds the pen so input reaches the host).
	viewerTok := mkViewerToken("sess-1", "user-drv", "vjti-drv", "driver")
	v, err := attachtest.AttachViewer(ctx, relay.BaseWSURL(), viewerTok, attachwire.RoleDriver, nil)
	if err != nil {
		t.Fatalf("attach viewer: %v", err)
	}
	t.Cleanup(func() { _ = v.Close() })
	drv := viewertest.NewDriver(v)

	// ---- ON START: primary screen contract ---------------------------------
	joinCtx, jc := context.WithTimeout(ctx, 10*time.Second)
	defer jc()
	// Poll to a settled primary screen (the join snapshot may briefly precede the
	// fixture's first paint).
	start, err := drv.SnapshotUntil(joinCtx, func(s attachwire.Screen) bool {
		return !viewertest.IsAltScreen(s) && viewertest.RowText(s, 0) == "FIXTURE-PRIMARY"
	})
	if err != nil {
		t.Fatalf("await primary screen: %v", err)
	}
	assertScreen(t, "start", start, screenExpect{
		alt:       false,
		row0:      "FIXTURE-PRIMARY",
		cell:      map[[2]int]string{{0, 0}: "F", {2, 4}: "R"},
		rowAt:     map[int]string{2: "    READY"},
		cursorRow: 5, cursorCol: 10,
	})

	// ---- AFTER 'a': alt-screen enter contract ------------------------------
	altCtx, ac := context.WithTimeout(ctx, 10*time.Second)
	defer ac()
	alt, err := drv.SendInputAndAwait(altCtx, []byte{'a'}, func(s attachwire.Screen) bool {
		return viewertest.IsAltScreen(s) && viewertest.CellText(s, 1, 2) == "A"
	})
	if err != nil {
		t.Fatalf("await alt screen after 'a': %v", err)
	}
	assertScreen(t, "alt", alt, screenExpect{
		alt:       true,
		row0:      "FIXTURE-ALT",
		cell:      map[[2]int]string{{0, 0}: "F", {1, 2}: "A"},
		rowAt:     map[int]string{1: "  ALPHA"},
		cursorRow: 7, cursorCol: 3,
	})

	// ---- AFTER 'q': alt-screen exit contract (primary restored + cursor) ----
	exitCtx, ec := context.WithTimeout(ctx, 10*time.Second)
	defer ec()
	back, err := drv.SendInputAndAwait(exitCtx, []byte{'q'}, func(s attachwire.Screen) bool {
		return !viewertest.IsAltScreen(s) && viewertest.RowText(s, 0) == "FIXTURE-PRIMARY"
	})
	if err != nil {
		t.Fatalf("await primary screen after 'q': %v", err)
	}
	assertScreen(t, "exit", back, screenExpect{
		alt:       false,
		row0:      "FIXTURE-PRIMARY",
		cell:      map[[2]int]string{{2, 4}: "R"},
		cursorRow: 5, cursorCol: 10,
	})
}

type screenExpect struct {
	alt                  bool
	row0                 string
	cell                 map[[2]int]string
	rowAt                map[int]string
	cursorRow, cursorCol int
}

func assertScreen(t *testing.T, stage string, s attachwire.Screen, want screenExpect) {
	t.Helper()
	if got := viewertest.IsAltScreen(s); got != want.alt {
		t.Errorf("[%s] IsAltScreen=%t want %t\n%s", stage, got, want.alt, viewertest.Dump(s))
	}
	if want.row0 != "" {
		if got := viewertest.RowText(s, 0); got != want.row0 {
			t.Errorf("[%s] RowText(0)=%q want %q\n%s", stage, got, want.row0, viewertest.Dump(s))
		}
	}
	for rc, txt := range want.cell {
		if got := viewertest.CellText(s, rc[0], rc[1]); got != txt {
			t.Errorf("[%s] CellText(%d,%d)=%q want %q", stage, rc[0], rc[1], got, txt)
		}
	}
	for row, txt := range want.rowAt {
		if got := viewertest.RowText(s, row); got != txt {
			t.Errorf("[%s] RowText(%d)=%q want %q", stage, row, got, txt)
		}
	}
	if r, c := viewertest.CursorAt(s); r != want.cursorRow || c != want.cursorCol {
		t.Errorf("[%s] CursorAt=(%d,%d) want (%d,%d)\n%s", stage, r, c, want.cursorRow, want.cursorCol, viewertest.Dump(s))
	}
}

// ---- helpers ----------------------------------------------------------------

// sessAdapter bridges ptyhost.Session (agent.InteractiveSession-shaped) to
// attachclient.Session — the ~5-line adapter documented in attachclient/session.go
// (only Subscribe's return type differs).
type sessAdapter struct{ *ptyhost.Session }

func (a sessAdapter) Subscribe(from attachwire.HostSeq) (attachclient.Subscription, error) {
	sub, err := a.Session.Subscribe(from)
	return sub, err
}

func buildFixture(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "vtfixture")
	//nolint:gosec // G204: builds a fixed, in-repo package path with the toolchain — no untrusted input
	cmd := exec.Command("go", "build", "-o", bin, "github.com/RenseiAI/donmai/attachclient/viewertest/fixturetui")
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v\n%s", err, out)
	}
	return bin
}

func waitBound(t *testing.T, relay *attachtest.StubRelay) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if relay.HostBound() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("timed out waiting for host leg to bind")
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// ---- unsigned test tokens (the stub checks aud + epoch presence only) -------

func mkHostToken(sessionID string, epoch int64, jti string) string {
	return fakeJWT(map[string]any{
		"sessionId": sessionID, "roomId": sessionID, "role": "host",
		"orgId": "org-1", "aud": "relay", "jti": jti, "epoch": epoch,
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	})
}

func mkViewerToken(sessionID, userID, jti, role string) string {
	return fakeJWT(map[string]any{
		"sessionId": sessionID, "roomId": sessionID, "userId": userID, "role": role,
		"orgId": "org-1", "aud": "relay", "jti": jti,
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	})
}

func fakeJWT(claims map[string]any) string {
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"EdDSA","typ":"JWT"}`))
	pb, _ := json.Marshal(claims)
	return strings.Join([]string{hdr, base64.RawURLEncoding.EncodeToString(pb), base64.RawURLEncoding.EncodeToString([]byte("sig"))}, ".")
}
