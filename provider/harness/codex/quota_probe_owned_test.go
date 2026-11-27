package codex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/internal/testexec"
)

// quotaOwnedFixture is a fake `codex app-server` that derives its account
// from the private auth link, refuses a non-private home or a leaked
// source-only marker, and optionally stalls at one method while ignoring
// TERM so only KILL (zero grace) can reap it.
const quotaOwnedFixture = `#!/usr/bin/env python3
import json, os, signal, sys, time
home = os.environ.get("CODEX_HOME", "")
stall = os.environ.get("FAKE_QUOTA_STALL_AT", "")
if stall:
    try:
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
    except Exception:
        pass
marker = os.environ.get("FAKE_QUOTA_MARKER", "")
ready = os.environ.get("FAKE_QUOTA_READY", "")
pidfile = os.environ.get("FAKE_QUOTA_PID", "")
if pidfile:
    open(pidfile, "w").write(str(os.getpid()))
def send(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        msg = json.loads(line)
    except Exception:
        continue
    mid, method = msg.get("id"), msg.get("method")
    if mid is None:
        continue
    if method == "initialize":
        if stall == "initialize":
            if ready:
                open(ready, "w").write("ready")
            time.sleep(30)
        send({"jsonrpc": "2.0", "id": mid, "result": {"codexHome": home}})
    elif method == "account/rateLimits/read":
        if stall == "read":
            if ready:
                open(ready, "w").write("ready")
            time.sleep(30)
        base = os.path.basename(home)
        if not base.startswith("donmai-codex-home-"):
            send({"jsonrpc": "2.0", "id": mid, "error": {"code": -32600, "message": "authentication required"}})
            continue
        if marker and os.path.exists(os.path.join(home, marker)):
            send({"jsonrpc": "2.0", "id": mid, "error": {"code": -32600, "message": "authentication required"}})
            continue
        acct = ""
        try:
            acct = json.load(open(os.path.join(home, "auth.json"))).get("accountId", "")
        except Exception:
            acct = ""
        send({"jsonrpc": "2.0", "id": mid, "result": {"accountId": acct, "rateLimits": {"limitId": "codex", "planType": "promax", "primary": {"usedPercent": 1, "windowDurationMins": 10080}}}})
    else:
        send({"jsonrpc": "2.0", "id": mid, "result": {}})
`

func writeQuotaOwnedFixture(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("quota owned fixture requires POSIX process groups")
	}
	if _, err := os.Stat("/usr/bin/env"); err != nil {
		t.Skip("no env launcher for fixture")
	}
	path := filepath.Join(t.TempDir(), "codex-quota-fixture")
	if err := testexec.WriteFile(path, []byte(quotaOwnedFixture), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func writeQuotaSourceHome(t *testing.T, accountID string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	body := fmt.Sprintf(`{"accountId":%q,"tokens":{"id_token":"e30","access_token":"a","refresh_token":"r"}}`, accountID)
	if err := os.WriteFile(filepath.Join(dir, codexAuthFileName), []byte(body), 0o600); err != nil {
		t.Fatalf("write source auth: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "source-only-marker"), []byte("source"), 0o600); err != nil {
		t.Fatalf("write source marker: %v", err)
	}
	return dir, body
}

func quotaOwnedOpts(fixture, probeRoot, marker, stall, ready, pid string) Options {
	env := map[string]string{"FAKE_QUOTA_MARKER": marker}
	if stall != "" {
		env["FAKE_QUOTA_STALL_AT"] = stall
	}
	if ready != "" {
		env["FAKE_QUOTA_READY"] = ready
	}
	if pid != "" {
		env["FAKE_QUOTA_PID"] = pid
	}
	return Options{
		CodexBin: fixture, Env: env, configTempDir: probeRoot,
		HostSessionAuth: true, HandshakeTimeout: 10 * time.Second, RPCTimeout: RateLimitsProbeTimeout,
	}
}

func assertQuotaUnknown(t *testing.T, accountID, plan string, probed agent.UsageLimits) {
	t.Helper()
	if accountID != "" || plan != "" {
		t.Fatalf("identity = %q/%q, want empty on unknown", accountID, plan)
	}
	if probed.Unavailable == nil || probed.Unavailable.Reason != agent.UsageUnavailableProbeFailed {
		t.Fatalf("probed = %+v, want probeFailed", probed.Unavailable)
	}
	if probed.Unavailable.Answered {
		t.Fatal("local failure marked answered; only a live login refusal answers")
	}
}

func assertProbeRootClean(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read probe root: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), codexHomePrefix) {
			t.Fatalf("private home survived: %s", e.Name())
		}
	}
}

// TestProbeQuotaForLoginSelectsExplicitLogin pins AC-1 through the production
// operation: A/B homes yield their own accounts in either order, the default
// works when selection is omitted, and missing/symlinked/permissive explicit
// auth yields unknown even though the default works. Both sources survive.
func TestProbeQuotaForLoginSelectsExplicitLogin(t *testing.T) {
	fixture := writeQuotaOwnedFixture(t)
	homeA, bodyA := writeQuotaSourceHome(t, "login-A")
	homeB, bodyB := writeQuotaSourceHome(t, "login-B")
	defHome := t.TempDir()
	defBody := `{"accountId":"default-login","tokens":{"id_token":"e30","access_token":"a","refresh_token":"r"}}`
	if err := os.WriteFile(filepath.Join(defHome, codexAuthFileName), []byte(defBody), 0o600); err != nil {
		t.Fatalf("write default auth: %v", err)
	}
	t.Setenv("CODEX_HOME", defHome)

	probe := func(source string) (string, string, agent.UsageLimits) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return ProbeQuotaForLogin(ctx, quotaOwnedOpts(fixture, t.TempDir(), "source-only-marker", "", "", ""), source)
	}
	for _, source := range []string{homeA, homeB, homeB, homeA} {
		accountID, plan, probed := probe(source)
		want := "login-A"
		if strings.HasSuffix(source, filepath.Base(homeB)) || source == homeB {
			want = "login-B"
		}
		if accountID != want || plan != "promax" || probed.Unavailable != nil || len(probed.Windows) == 0 {
			t.Fatalf("source %s: account=%q plan=%q probed=%+v, want %q/promax with windows", source, accountID, plan, probed.Unavailable, want)
		}
	}
	if accountID, _, probed := probe(""); accountID != "default-login" || probed.Unavailable != nil {
		t.Fatalf("default: account=%q probed=%+v, want default-login", accountID, probed.Unavailable)
	}
	missing := filepath.Join(t.TempDir(), "no-such-home")
	for name, source := range map[string]string{"missing": missing, "relative": "relative-home"} {
		accountID, plan, probed := probe(source)
		assertQuotaUnknown(t, accountID, plan, probed)
		_ = name
	}
	looseDir := t.TempDir()
	looseAuth := filepath.Join(looseDir, codexAuthFileName)
	if err := os.WriteFile(looseAuth, []byte(bodyA), 0o644); err != nil { //nolint:gosec // unsafe mode is the fixture
		t.Fatalf("write loose auth: %v", err)
	}
	if accountID, plan, probed := probe(looseDir); probed.Unavailable == nil {
		t.Fatalf("loose auth answered: %q/%q %+v", accountID, plan, probed.Windows)
	} else {
		assertQuotaUnknown(t, accountID, plan, probed)
	}
	linkDir := t.TempDir()
	if err := os.Symlink(filepath.Join(homeA, codexAuthFileName), filepath.Join(linkDir, codexAuthFileName)); err != nil {
		t.Fatalf("symlink auth: %v", err)
	}
	accountID, plan, probed := probe(linkDir)
	assertQuotaUnknown(t, accountID, plan, probed)
	for dir, body := range map[string]string{homeA: bodyA, homeB: bodyB} {
		got, err := os.ReadFile(filepath.Join(dir, codexAuthFileName))
		if err != nil || string(got) != body {
			t.Fatalf("source %s changed: %q err=%v", dir, got, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "source-only-marker")); err != nil {
			t.Fatalf("source marker lost in %s: %v", dir, err)
		}
	}
}

// TestProbeQuotaForLoginStallCancelsPromptly pins AC-2 through the production
// operation: a fixture stalled at initialize (cancelled) and at a read
// (expired) while ignoring TERM still returns unknown within a second, with
// the leader reaped, no live group writer and no private home left behind.
func TestProbeQuotaForLoginStallCancelsPromptly(t *testing.T) {
	fixture := writeQuotaOwnedFixture(t)
	home, _ := writeQuotaSourceHome(t, "login-stall")
	t.Setenv("CODEX_HOME", t.TempDir())
	for _, tc := range []struct {
		name, stall string
		expire      bool
	}{
		{name: "initialize/cancel", stall: "initialize"},
		{name: "read/expiry", stall: "read", expire: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			ready := filepath.Join(t.TempDir(), "ready")
			pidFile := filepath.Join(t.TempDir(), "pid")
			var ctx context.Context
			var cancel context.CancelFunc
			if tc.expire {
				ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
			} else {
				ctx, cancel = context.WithCancel(context.Background())
			}
			defer cancel()
			type result struct {
				accountID, plan string
				probed          agent.UsageLimits
			}
			done := make(chan result, 1)
			go func() {
				a, p, pr := ProbeQuotaForLogin(ctx, quotaOwnedOpts(fixture, root, "", tc.stall, ready, pidFile), home)
				done <- result{a, p, pr}
			}()
			deadline := time.Now().Add(15 * time.Second)
			for {
				if _, err := os.Stat(ready); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("fixture never entered the stall")
				}
				time.Sleep(20 * time.Millisecond)
			}
			pidBytes, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatalf("read fixture pid: %v", err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
			if err != nil || pid <= 0 {
				t.Fatalf("bad fixture pid %q: %v", pidBytes, err)
			}
			if !tc.expire {
				cancel()
			} else {
				select {
				case <-ctx.Done():
				case <-time.After(15 * time.Second):
					t.Fatal("attempt deadline never expired")
				}
			}
			start := time.Now()
			var r result
			select {
			case r = <-done:
			case <-time.After(15 * time.Second):
				t.Fatal("owned probe did not return after cancellation/expiry")
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("return took %s, want prompt (within 1s of cancel/expiry)", elapsed)
			}
			assertQuotaUnknown(t, r.accountID, r.plan, r.probed)
			if err := syscall.Kill(pid, 0); err == nil {
				t.Fatalf("leader %d still live after owned shutdown", pid)
			}
			if state, err := ownedProcessGroupState(pid); err == nil && state.writersRunning {
				t.Fatalf("group %d still has live writers: %+v", pid, state)
			}
			assertProbeRootClean(t, root)
		})
	}
}
