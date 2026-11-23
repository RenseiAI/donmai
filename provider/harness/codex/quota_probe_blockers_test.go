package codex

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/internal/testexec"
)

// quotaBlockerFixture answers initialize normally and serves the first
// quota read from the private auth link. Later reads fail as a backend
// outage, the TERM handler records delivery while staying alive so only
// KILL reaps it, and each read appends one line to the count file.
const quotaBlockerFixture = `#!/usr/bin/env python3
import json, os, signal, sys
home = os.environ.get("CODEX_HOME", "")
countfile = os.environ.get("FAKE_QUOTA_COUNT", "")
termfile = os.environ.get("FAKE_QUOTA_TERM", "")
readdone = os.environ.get("FAKE_QUOTA_READ_DONE", "")
pidfile = os.environ.get("FAKE_QUOTA_PID", "")
if termfile:
    try:
        def onterm(signum, frame):
            open(termfile, "w").write("term")
        signal.signal(signal.SIGTERM, onterm)
    except Exception:
        pass
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
        send({"jsonrpc": "2.0", "id": mid, "result": {"codexHome": home}})
    elif method == "account/rateLimits/read":
        n = 0
        if countfile:
            try:
                with open(countfile) as f:
                    content = f.read().strip()
                    n = 0 if content == "" else len(content.split("\n"))
            except Exception:
                n = 0
            try:
                open(countfile, "a").write("read\n")
            except Exception:
                pass
        if n >= 1:
            send({"jsonrpc": "2.0", "id": mid, "error": {"code": -32603, "message": "failed to fetch rate limits"}})
            continue
        acct = ""
        try:
            acct = json.load(open(os.path.join(home, "auth.json"))).get("accountId", "")
        except Exception:
            acct = ""
        send({"jsonrpc": "2.0", "id": mid, "result": {"accountId": acct, "rateLimits": {"limitId": "codex", "planType": "promax", "primary": {"usedPercent": 1, "windowDurationMins": 10080}}}})
        if readdone:
            try:
                open(readdone, "w").write("read")
            except Exception:
                pass
    else:
        send({"jsonrpc": "2.0", "id": mid, "result": {}})
`

func writeQuotaBlockerFixture(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("quota blocker fixture requires POSIX process groups")
	}
	if _, err := os.Stat("/usr/bin/env"); err != nil {
		t.Skip("no env launcher for fixture")
	}
	path := filepath.Join(t.TempDir(), "codex-blocker-fixture")
	if err := testexec.WriteFile(path, []byte(quotaBlockerFixture), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// TestProbeQuotaForLoginSingleReadPublishesFirstReply pins the single-read
// fix: a login whose first quota reply is valid but whose second read fails
// still publishes its account, plan and windows, with exactly one read.
func TestProbeQuotaForLoginSingleReadPublishesFirstReply(t *testing.T) {
	fixture := writeQuotaBlockerFixture(t)
	home, _ := writeQuotaSourceHome(t, "selected-login")
	t.Setenv("CODEX_HOME", t.TempDir())
	root := t.TempDir()
	countFile := filepath.Join(t.TempDir(), "reads")
	opts := quotaOwnedOpts(fixture, root, "", "", "", "")
	opts.Env["FAKE_QUOTA_COUNT"] = countFile
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	accountID, plan, probed := ProbeQuotaForLogin(ctx, opts, home)
	if accountID != "selected-login" || plan != "promax" || probed.Unavailable != nil || len(probed.Windows) == 0 {
		t.Fatalf("single read: account=%q plan=%q probed=%+v, want selected-login/promax with windows", accountID, plan, probed.Unavailable)
	}
	raw, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatalf("read count file: %v", err)
	}
	if n := len(strings.Split(strings.TrimSpace(string(raw)), "\n")); n != 1 {
		t.Fatalf("reads=%d, want exactly one quota RPC", n)
	}
	assertProbeRootClean(t, root)
}

type blockerOutcome struct {
	accountID   string
	plan        string
	unavailable bool
}

// TestProbeQuotaForLoginCancelDuringShutdownReturnsUnknownPromptly pins the
// shutdown fix: cancelling after TERM was delivered to a TERM-ignoring child
// still returns unknown promptly, with the leader reaped and no home left.
func TestProbeQuotaForLoginCancelDuringShutdownReturnsUnknownPromptly(t *testing.T) {
	fixture := writeQuotaBlockerFixture(t)
	home, _ := writeQuotaSourceHome(t, "selected-login")
	t.Setenv("CODEX_HOME", t.TempDir())
	root := t.TempDir()
	termFile := filepath.Join(t.TempDir(), "term")
	readDone := filepath.Join(t.TempDir(), "read-done")
	pidFile := filepath.Join(t.TempDir(), "pid")
	opts := quotaOwnedOpts(fixture, root, "", "", "", pidFile)
	opts.Env["FAKE_QUOTA_TERM"] = termFile
	opts.Env["FAKE_QUOTA_READ_DONE"] = readDone
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan blockerOutcome, 1)
	go func() {
		a, p, pr := ProbeQuotaForLogin(ctx, opts, home)
		done <- blockerOutcome{accountID: a, plan: p, unavailable: pr.Unavailable != nil}
	}()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(readDone); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture never answered the quota read")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for {
		if _, err := os.Stat(termFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("shutdown never delivered TERM to the fixture")
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
	cancel()
	start := time.Now()
	var r blockerOutcome
	select {
	case r = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("owned probe did not return after cancellation during shutdown")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("return took %s, want prompt (within 2s of cancel during shutdown)", elapsed)
	}
	if r.accountID != "" || r.plan != "" {
		t.Fatalf("identity = %q/%q, want empty on cancellation during shutdown", r.accountID, r.plan)
	}
	if !r.unavailable {
		t.Fatal("probe did not report an unavailable snapshot after cancellation")
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("leader %d still live after owned shutdown", pid)
	}
	if state, err := ownedProcessGroupState(pid); err == nil && state.writersRunning {
		t.Fatalf("group %d still has live writers: %+v", pid, state)
	}
	assertProbeRootClean(t, root)
}

// TestProbeQuotaForLoginBlankSelectorYieldsUnknown pins the selector fix: a
// blank explicit selector never falls back to the working default login.
func TestProbeQuotaForLoginBlankSelectorYieldsUnknown(t *testing.T) {
	fixture := writeQuotaOwnedFixture(t)
	defHome := t.TempDir()
	defBody := `{"accountId":"default-login","tokens":{"id_token":"e30","access_token":"a","refresh_token":"r"}}`
	if err := os.WriteFile(filepath.Join(defHome, codexAuthFileName), []byte(defBody), 0o600); err != nil {
		t.Fatalf("write default auth: %v", err)
	}
	t.Setenv("CODEX_HOME", defHome)
	for _, source := range []string{" ", "\t", "  \t  "} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		accountID, plan, probed := ProbeQuotaForLogin(ctx, quotaOwnedOpts(fixture, t.TempDir(), "source-only-marker", "", "", ""), source)
		cancel()
		assertQuotaUnknown(t, accountID, plan, probed)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if accountID, _, probed := ProbeQuotaForLogin(ctx, quotaOwnedOpts(fixture, t.TempDir(), "source-only-marker", "", "", ""), ""); accountID != "default-login" || probed.Unavailable != nil {
		t.Fatalf("default: account=%q probed=%+v, want default-login", accountID, probed.Unavailable)
	}
}
