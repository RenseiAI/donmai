//go:build darwin

package confinement

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

func newLiveConfiner(t *testing.T, backend Backend, extra ExtraRules) (*Confiner, string) {
	t.Helper()
	home, stateHome, profileDir := hostDirs(t)
	c, err := New(Options{Backend: backend, ProfileDir: profileDir, Home: home, StateHome: stateHome, ExtraRules: extra, ExecutableDigest: "sha256:test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, stateHome
}

func probeCommand(t *testing.T) []string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return []string{exe}
}

func runSelfTest(t *testing.T, c *Confiner) (SelfTestRecord, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	return c.SelfTest(ctx, SelfTestOptions{ProbeCommand: probeCommand(t), ScratchDir: shortTempDir(t, "dcs")})
}

// TestSeatbelt_SelfTestPassesBothModes is the live backend proof: the probe
// set passes through the headless and the PTY spawn path.
func TestSeatbelt_SelfTestPassesBothModes(t *testing.T) {
	c, _ := newLiveConfiner(t, DefaultBackend(), nil)
	record, err := runSelfTest(t, c)
	if err != nil {
		t.Fatalf("SelfTest: %v\n%s", err, failureIDs(record))
	}
	if !record.Passed || len(record.SessionModes) != 2 {
		t.Fatalf("record passed=%v modes=%v", record.Passed, record.SessionModes)
	}
	t.Logf("self-test passed: %d probes across %v, backend %s", len(record.Probes), record.SessionModes, record.BackendVersion)
}

func failedIn(record SelfTestRecord) map[string]bool {
	failed := map[string]bool{}
	for _, probe := range record.Failures() {
		failed[string(probe.Mode)+"/"+probe.ID] = true
	}
	return failed
}

// TestSeatbelt_SelfTestRedWithoutBackend is the discriminating control of
// D1.5: with the backend replaced by nothing, every probe that expects a
// refusal — each one held by the backend and nothing else — fails, in both
// session modes, while every positive control still passes.
func TestSeatbelt_SelfTestRedWithoutBackend(t *testing.T) {
	c, _ := newLiveConfiner(t, noopBackend{}, nil)
	record, err := runSelfTest(t, c)
	if reason, _ := ReasonOf(err); reason != ReasonSelfTestFailed {
		t.Fatalf("SelfTest with no backend: err=%v, want self_test_failed", err)
	}
	if record.Passed || len(record.SessionModes) != 0 {
		t.Fatalf("record passed=%v modes=%v, want no attested mode", record.Passed, record.SessionModes)
	}
	refused := map[string]int{}
	for _, probe := range record.Probes {
		switch {
		case probe.Expected == outcomeRefused && probe.Pass:
			t.Errorf("%s/%s passed with no backend; the probe does not discriminate", probe.Mode, probe.ID)
		case probe.Expected == outcomeAccepted && !probe.Pass:
			t.Errorf("positive control %s/%s failed with no backend: %s", probe.Mode, probe.ID, probe.Detail)
		}
		if probe.Expected == outcomeRefused {
			refused[probe.Class]++
		}
	}
	for _, class := range []string{classOutside, classReadOnly, classProtected, classWidening, classReadScope} {
		if refused[class] == 0 {
			t.Errorf("no %s probe ran", class)
		}
	}
	if _, err := c.Prepare(Spec{SessionMode: "autonomous"}); err == nil {
		t.Fatal("Prepare succeeded after a failed self-test")
	} else if reason, _ := ReasonOf(err); reason != ReasonSelfTestFailed {
		t.Fatalf("Prepare after a failed self-test: %v, want self_test_failed", err)
	}
}

// TestSeatbelt_SelfTestRedWithChmod: same-identity permission bits are not
// enforcement (ADR-2026-08-22 D6.7); the permission-change probe on the
// read-only leaf and every outside probe turn red.
func TestSeatbelt_SelfTestRedWithChmod(t *testing.T) {
	c, _ := newLiveConfiner(t, chmodBackend{}, nil)
	record, err := runSelfTest(t, c)
	if reason, _ := ReasonOf(err); reason != ReasonSelfTestFailed {
		t.Fatalf("SelfTest with chmod: err=%v, want self_test_failed", err)
	}
	failed := failedIn(record)
	for _, mode := range []string{"autonomous", "human_controlled"} {
		for _, id := range []string{"ro.chmod", "ro.chmod_leaf", "ro.rename_leaf", "protected.chmod", "outside.home.create", "widen.reenter_backend"} {
			if !failed[mode+"/"+id] {
				t.Errorf("%s/%s passed under chmod", mode, id)
			}
		}
		if failed[mode+"/ro.write"] {
			t.Errorf("%s/ro.write failed under chmod; the permission bits should have held that one", mode)
		}
	}
}

// liveWorld is a minimal session on disk for direct live tests, under a
// short directory so a socket fits in its session tmp.
func liveWorld(t *testing.T) (specWorld, *Confiner) {
	t.Helper()
	base := shortTempDir(t, "dcl")
	w := specWorld{base: base}
	w.home = filepath.Join(base, "home")
	w.stateHome = filepath.Join(w.home, ".s")
	w.profileDir = filepath.Join(w.stateHome, "p")
	w.ws = filepath.Join(base, "ws")
	w.mut = filepath.Join(w.ws, "mut")
	w.ro = filepath.Join(w.ws, "ro")
	w.state = filepath.Join(w.mut, ".h")
	w.ext = filepath.Join(w.state, "ext")
	w.tmp = filepath.Join(base, "t")
	w.cache = filepath.Join(base, "c")
	for _, dir := range []string{w.profileDir, filepath.Join(w.mut, ".git"), w.ro, w.ext, w.tmp, w.cache, filepath.Join(w.ws, ".workarea")} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}
	c, err := New(Options{Backend: DefaultBackend(), ProfileDir: w.profileDir, Home: w.home, StateHome: w.stateHome})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return w, c
}

// runConfined runs argv inside the plan through the headless spawn path and
// returns its exit code and output.
func runConfined(t *testing.T, plan *Plan, dir string, env []string, argv ...string) (int, string) {
	t.Helper()
	wrapped, err := plan.Command(argv)
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, wrapped[0], wrapped[1:]...) //nolint:gosec // G204: the confined argv under test.
	cmd.Dir = dir
	cmd.Env = overlayEnv(os.Environ(), append(plan.Environment(), env...))
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), string(out)
	}
	if err != nil {
		t.Fatalf("run %v: %v", argv, err)
	}
	return 0, string(out)
}

// runProbeSteps runs probe steps inside the plan and returns their results.
func runProbeSteps(t *testing.T, plan *Plan, w specWorld, steps ...probeStep) map[string]stepResult {
	t.Helper()
	planPath := filepath.Join(w.base, "plan-"+randomSuffix()+".json")
	raw, err := json.Marshal(probePlan{ResultPath: probeResultPath(w.tmp), Steps: steps})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out := runConfined(t, plan, w.mut, []string{ProbeEnv + "=" + planPath}, probeCommand(t)...); code != 0 {
		t.Fatalf("probe exit %d: %s", code, out)
	}
	raw, err = os.ReadFile(probeResultPath(w.tmp))
	if err != nil {
		t.Fatalf("probe results: %v", err)
	}
	var results []stepResult
	if err := json.Unmarshal(raw, &results); err != nil {
		t.Fatal(err)
	}
	byID := map[string]stepResult{}
	for _, result := range results {
		byID[result.ID] = result
	}
	return byID
}

// TestSeatbelt_ProfileParsesUnderSandboxExec: every rule the renderer can
// emit, composer rules of each kind included, is accepted by the kernel's
// profile compiler.
func TestSeatbelt_ProfileParsesUnderSandboxExec(t *testing.T) {
	w, _ := liveWorld(t)
	c, err := New(Options{Backend: DefaultBackend(), ProfileDir: w.profileDir, Home: w.home, StateHome: w.stateHome, ExtraRules: func(RuleContext) []Rule {
		return []Rule{
			{Kind: RuleDenyRead, Path: filepath.Join(w.home, ".secret"), Scope: ScopeSubtree},
			{Kind: RuleDenyRead, Path: filepath.Join(w.home, ".netrc"), Scope: ScopeLiteral},
			{Kind: RuleDenyWrite, Path: filepath.Join(w.mut, "vendor"), Scope: ScopeSubtree},
			{Kind: RuleDenyServiceLookup, Service: "com.example.composer.agent"},
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	spec := w.spec()
	spec.Sockets = ResolverSockets()
	plan, err := c.prepare(spec, "")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer func() { _ = plan.Release() }()
	if code, out := runConfined(t, plan, w.mut, nil, "/usr/bin/true"); code != 0 {
		t.Fatalf("sandbox-exec rejected the rendered profile: exit %d: %s", code, out)
	}
}

// TestSeatbelt_NestedSandboxRefused: from inside an outer profile the backend
// refuses with nested_sandbox instead of running under the outer profile and
// reporting the inner level (D4.2).
func TestSeatbelt_NestedSandboxRefused(t *testing.T) {
	cmd := exec.Command(sandboxExec, append([]string{"-p", "(version 1)(allow default)"}, probeCommand(t)...)...) //nolint:gosec // G204: fixed launcher, the test binary.
	cmd.Env = append(os.Environ(), nestedCheckEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("nested check: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "reason=nested_sandbox") {
		t.Fatalf("nested check output %q, want reason=nested_sandbox", out)
	}
	if err := DefaultBackend().Check(); err != nil {
		t.Fatalf("Check outside any profile: %v", err)
	}
}

// TestSeatbelt_ComposerRulesWinLast: a composer deny inside the writable set
// holds against the writable allow, its ancestors are pinned, and a service
// deny closes the service.
func TestSeatbelt_ComposerRulesWinLast(t *testing.T) {
	w, _ := liveWorld(t)
	vendor := filepath.Join(w.mut, "vendor")
	if err := os.MkdirAll(filepath.Join(vendor, "pkg"), 0o750); err != nil {
		t.Fatal(err)
	}
	c, err := New(Options{Backend: DefaultBackend(), ProfileDir: w.profileDir, Home: w.home, StateHome: w.stateHome, ExtraRules: func(RuleContext) []Rule {
		return []Rule{
			{Kind: RuleDenyWrite, Path: filepath.Join(vendor, "pkg"), Scope: ScopeSubtree},
			{Kind: RuleDenyServiceLookup, Service: lookupControlService},
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := c.prepare(w.spec(), "")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer func() { _ = plan.Release() }()
	results := runProbeSteps(t, plan, w,
		probeStep{ID: "denied", Op: opCreate, Path: filepath.Join(vendor, "pkg", "x")},
		probeStep{ID: "beside", Op: opCreate, Path: filepath.Join(vendor, "y")},
		probeStep{ID: "rename-ancestor", Op: opRename, Path: vendor, Path2: filepath.Join(w.mut, "vendor-moved")},
		probeStep{ID: "lookup", Op: opLookup, Services: []string{lookupControlService}},
	)
	if results["denied"].Err == "" || exists(filepath.Join(vendor, "pkg", "x")) {
		t.Error("a write under the composer deny got through")
	}
	if results["beside"].Err != "" {
		t.Errorf("a write beside the composer deny was refused: %s", results["beside"].Err)
	}
	if results["rename-ancestor"].Err == "" || !exists(vendor) {
		t.Error("renaming an ancestor carried the composer-denied path out from under its rule")
	}
	if code, ok := results["lookup"].Lookups[lookupControlService]; !ok || code == 0 {
		t.Errorf("the composer service deny did not close %s: %+v", lookupControlService, results["lookup"])
	}
}

// TestSeatbelt_ComposerUnrenderableFailsSelfTest: a composer that returns a
// rule the backend cannot render fails at start, not at the first session
// (D4.3).
func TestSeatbelt_ComposerUnrenderableFailsSelfTest(t *testing.T) {
	c, _ := newLiveConfiner(t, DefaultBackend(), func(RuleContext) []Rule {
		return []Rule{{Kind: RuleDenyWrite, Path: "relative/path", Scope: ScopeSubtree}}
	})
	record, err := runSelfTest(t, c)
	if reason, _ := ReasonOf(err); reason != ReasonRuleUnrenderable {
		t.Fatalf("SelfTest: err=%v, want rule_unrenderable", err)
	}
	if record.Passed || len(record.SessionModes) != 0 {
		t.Fatalf("record passed=%v modes=%v", record.Passed, record.SessionModes)
	}
	if _, ok := c.Attestation(); ok {
		t.Fatal("a self-test that never ran a probe attests")
	}
}

// TestSeatbelt_DeclaredSocketsOnly: a local socket outside the writable set
// is reachable only when the adapter declares it.
func TestSeatbelt_DeclaredSocketsOnly(t *testing.T) {
	resolver := ResolverSockets()[0]
	for _, tt := range []struct {
		name     string
		declared []string
		wantOpen bool
	}{
		{"undeclared", nil, false},
		{"declared", []string{resolver}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w, c := liveWorld(t)
			spec := w.spec()
			spec.Sockets = tt.declared
			plan, err := c.prepare(spec, "")
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			defer func() { _ = plan.Release() }()
			result := runProbeSteps(t, plan, w, probeStep{ID: "dial", Op: opDial, Path: resolver})["dial"]
			if open := result.Err == ""; open != tt.wantOpen {
				t.Fatalf("dial %s: err=%q, want open=%v", tt.name, result.Err, tt.wantOpen)
			}
		})
	}
}

// TestSeatbelt_ToolsWorkInsideTheSet: ordinary tools work on the writable
// set with TMPDIR redirected, and the same shell cannot write the home.
func TestSeatbelt_ToolsWorkInsideTheSet(t *testing.T) {
	w, c := liveWorld(t)
	plan, err := c.prepare(w.spec(), "")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer func() { _ = plan.Release() }()
	repo := filepath.Join(w.mut, "repo")
	script := strings.Join([]string{
		`set -e`,
		// macOS mktemp prefers the OS-assigned per-user directory to TMPDIR
		// unless given a template: refused there, it works with one.
		`if mktemp >/dev/null 2>&1; then echo "mktemp wrote the per-user temporary directory"; exit 3; fi`,
		`f=$(mktemp "$TMPDIR/x.XXXXXX")`,
		`cat > "$f" <<HEREDOC`,
		`a here-document needs a temporary file`,
		`HEREDOC`,
		`grep -q here-document "$f"`,
		`mkdir -p "` + repo + `" && cd "` + repo + `"`,
		`git init -q .`,
		`echo hello > a.txt`,
		`git add a.txt`,
		`git -c user.name=probe -c user.email=probe@example.invalid commit -q -m first`,
		`git log --oneline | grep -q first`,
		`echo ok`,
	}, "\n")
	if code, out := runConfined(t, plan, w.mut, []string{"HOME=" + w.home, "GIT_CONFIG_NOSYSTEM=1"}, "/bin/sh", "-c", script); code != 0 || !strings.Contains(out, "ok") {
		t.Fatalf("tools inside the set: exit %d: %s", code, out)
	}
	if code, _ := runConfined(t, plan, w.mut, nil, "/usr/bin/touch", filepath.Join(w.home, "planted")); code == 0 || exists(filepath.Join(w.home, "planted")) {
		t.Fatal("the confined shell wrote the operator home")
	}
}

// TestSeatbelt_LoopbackTCPDeclaredPortsOnly is the live proof for the
// loopback deny: listeners answer on an undeclared loopback port over the
// numeric IPv4 and IPv6 loopbacks, the confined connects to each fail, and
// the connects to a declared port succeed — including through the hostname,
// which may resolve to either family.
func TestSeatbelt_LoopbackTCPDeclaredPortsOnly(t *testing.T) {
	openPort := freeLoopbackPort(t)
	closedPort := freeLoopbackPort(t)
	if openPort == closedPort {
		t.Skip("could not allocate two distinct loopback ports")
	}
	openListener := startLoopbackListener(t, openPort)
	defer func() { _ = openListener.Process.Kill(); _ = openListener.Wait() }()
	closedListener := startLoopbackListener(t, closedPort)
	defer func() { _ = closedListener.Process.Kill(); _ = closedListener.Wait() }()
	waitLoopbackListener(t, closedPort)
	// The IPv6 listeners answer on the same two ports, one declared and
	// one not, so the test judges address family rather than port number.
	open6, closed6 := startLoopbackListener6(t, openPort), startLoopbackListener6(t, closedPort)
	defer closeListener6(open6)
	defer closeListener6(closed6)

	w, c := liveWorld(t)
	spec := w.spec()
	spec.LoopbackTCPPorts = []int{openPort}
	plan, err := c.prepare(spec, "")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer func() { _ = plan.Release() }()
	for _, target := range []struct{ host, port string }{
		{"127.0.0.1", itoaPort(openPort)},
		{"::1", itoaPort(openPort)},
	} {
		if code, out := runConfined(t, plan, w.mut, nil, "/usr/bin/nc", "-z", "-w", "2", target.host, target.port); code != 0 {
			t.Fatalf("confined nc to declared %s port %s failed: exit %d: %s", target.host, target.port, code, out)
		}
	}
	for _, target := range []struct{ host, port string }{
		{"127.0.0.1", itoaPort(closedPort)},
		{"::1", itoaPort(closedPort)},
		{"localhost", itoaPort(closedPort)},
	} {
		if code, out := runConfined(t, plan, w.mut, nil, "/usr/bin/nc", "-z", "-w", "2", target.host, target.port); code == 0 {
			t.Fatalf("confined nc to undeclared %s port %s succeeded: %s", target.host, target.port, out)
		}
	}
}

// TestSeatbelt_LoopbackTCPRedWithoutDeny is the discriminating control: the
// same two nc connects with the loopback deny stripped from the rendered
// profile both succeed, so the test above pins the deny and nothing else.
func TestSeatbelt_LoopbackTCPRedWithoutDeny(t *testing.T) {
	openPort := freeLoopbackPort(t)
	closedPort := freeLoopbackPort(t)
	if openPort == closedPort {
		t.Skip("could not allocate two distinct loopback ports")
	}
	openListener := startLoopbackListener(t, openPort)
	defer func() { _ = openListener.Process.Kill(); _ = openListener.Wait() }()
	closedListener := startLoopbackListener(t, closedPort)
	defer func() { _ = closedListener.Process.Kill(); _ = closedListener.Wait() }()
	waitLoopbackListener(t, closedPort)

	w, c := liveWorld(t)
	plan, err := c.prepare(w.spec(), "")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer func() { _ = plan.Release() }()
	runRed := func(args ...string) int {
		code, _ := runStripped(t, plan, w.mut, loopbackTCPDeny+"\n", args...)
		return code
	}
	if code := runRed("/usr/bin/nc", "-z", "-w", "2", "127.0.0.1", itoaPort(openPort)); code != 0 {
		t.Fatalf("nc to port %d without the deny failed: exit %d", openPort, code)
	}
	if code := runRed("/usr/bin/nc", "-z", "-w", "2", "127.0.0.1", itoaPort(closedPort)); code != 0 {
		t.Fatalf("nc to port %d without the deny failed: exit %d; the red control does not discriminate", closedPort, code)
	}
}

// runStripped runs argv under the plan's rendered profile with one rule
// removed, through the same launcher and environment: the red control that
// shows the removed rule, and nothing else, is what holds. It fails the test
// when the profile does not hold the rule.
func runStripped(t *testing.T, plan *Plan, dir, remove string, argv ...string) (int, string) {
	t.Helper()
	wrapped, err := plan.Command([]string{"/usr/bin/true"})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	// The wrapped argv is sandbox-exec -f <profile> -- <binary>.
	var profilePath string
	for i, arg := range wrapped {
		if arg == "-f" && i+1 < len(wrapped) {
			profilePath = wrapped[i+1]
		}
	}
	if profilePath == "" {
		t.Fatal("wrapped command names no profile file")
	}
	raw, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	stripped := strings.ReplaceAll(string(raw), remove, "")
	if stripped == string(raw) {
		t.Fatalf("rendered profile holds no %q to strip", remove)
	}
	redPath := profilePath + ".red"
	if err := os.WriteFile(redPath, []byte(stripped), 0o600); err != nil { //nolint:gosec // G703: the profile path comes from the plan under test.
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(redPath) }()
	full := []string{sandboxExec, "-f", redPath, "--"}
	full = append(full, argv...)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, full[0], full[1:]...) //nolint:gosec // G204: the confined argv under test.
	cmd.Dir = dir
	cmd.Env = overlayEnv(os.Environ(), plan.Environment())
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), string(out)
	}
	if err != nil {
		t.Fatalf("run %v: %v", argv, err)
	}
	return 0, string(out)
}

func itoaPort(port int) string {
	return strconv.Itoa(port)
}

// freeLoopbackPort returns a currently free loopback TCP port: bind port
// zero, read the assigned port, close, and hand it to nc. The nc listener
// rebinds it immediately; a collision fails the test loudly instead of
// passing quietly.
func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}

// startLoopbackListener starts an nc listener on a loopback port. The -k
// flag keeps it answering across the readiness probe and both connects.
func startLoopbackListener(t *testing.T, port int) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/usr/bin/nc", "-l", "-k", "127.0.0.1", itoaPort(port)) //nolint:gosec // G204: fixed test tool, allocated port.
	if err := cmd.Start(); err != nil {
		t.Skipf("nc listener on %d: %v", port, err)
	}
	return cmd
}

// startLoopbackListener6 listens on the IPv6 loopback in-process: the
// stock listener tool binds IPv4 only, so the IPv6 half cannot use it.
// It returns nil where IPv6 loopback is unavailable; the caller skips
// the IPv6 cases rather than passing without judging them.
func startLoopbackListener6(t *testing.T, port int) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", net.JoinHostPort("::1", itoaPort(port)))
	if err != nil {
		t.Skipf("no IPv6 loopback listener: %v", err)
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("::1", itoaPort(port)), time.Second)
		if err == nil {
			_ = conn.Close()
			return listener
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = listener.Close()
	t.Fatalf("IPv6 loopback listener on %d never answered", port)
	return nil
}

func closeListener6(listener net.Listener) {
	if listener != nil {
		_ = listener.Close()
	}
}

// waitLoopbackListener waits until a loopback port accepts, so the confined
// connects race nothing.
func waitLoopbackListener(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+itoaPort(port), time.Second)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("nc listener on %d never answered", port)
}

// TestSeatbelt_PasteboardClosed is the live proof for the pasteboard deny:
// the confined lookup of each pasteboard service fails while the positive
// control still resolves, and a confined pbcopy write fails.
func TestSeatbelt_PasteboardClosed(t *testing.T) {
	w, c := liveWorld(t)
	plan, err := c.prepare(w.spec(), "")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer func() { _ = plan.Release() }()
	pasteboardServices := []string{
		"com.apple.pasteboard.1",
		"com.apple.coreservices.uauseractivitypasteboardclient.xpc",
	}
	results := runProbeSteps(t, plan, w,
		probeStep{ID: "pasteboard", Op: opLookup, Services: pasteboardServices},
		probeStep{ID: "control", Op: opLookup, Services: []string{lookupControlService}},
	)
	for _, service := range pasteboardServices {
		if code, ok := results["pasteboard"].Lookups[service]; !ok || code == 0 {
			t.Errorf("confined lookup of the pasteboard service %s succeeded: %+v", service, results["pasteboard"].Lookups)
		}
	}
	if code, ok := results["control"].Lookups[lookupControlService]; !ok || code != 0 {
		t.Errorf("positive control lookup failed under the profile: %+v", results["control"].Lookups)
	}
	if code, out := runConfined(t, plan, w.mut, nil, "/bin/sh", "-c", "echo probe | /usr/bin/pbcopy -pboard probe-test-board"); code == 0 {
		t.Errorf("confined pbcopy succeeded: %s", out)
	}
}

// openReadsBackend is the real macOS backend rendering every session with
// reads open: the read-scope rules, and only they, are missing.
type openReadsBackend struct{ *seatbeltBackend }

func (b openReadsBackend) Apply(req ApplyRequest) (Applied, error) {
	open := *req.Resolved
	open.ReadScope, open.ReadPaths = "", nil
	req.Resolved = &open
	return b.seatbeltBackend.Apply(req)
}

// TestSeatbelt_SelfTestRedWithOpenReads is the read scope's discriminating
// control: with the read rules dropped from an otherwise real profile, every
// read-scope refusal turns red in both session modes, and nothing else does.
func TestSeatbelt_SelfTestRedWithOpenReads(t *testing.T) {
	c, _ := newLiveConfiner(t, openReadsBackend{&seatbeltBackend{exe: sandboxExec}}, nil)
	record, err := runSelfTest(t, c)
	if reason, _ := ReasonOf(err); reason != ReasonSelfTestFailed {
		t.Fatalf("SelfTest with open reads: err=%v, want self_test_failed", err)
	}
	if record.Passed {
		t.Fatal("the self-test passed with the read rules dropped")
	}
	readRefusals := 0
	for _, probe := range record.Probes {
		switch {
		case probe.Class == classReadScope && probe.Pass:
			t.Errorf("%s/%s passed with open reads; the probe does not discriminate the read scope", probe.Mode, probe.ID)
		case probe.Class != classReadScope && !probe.Pass:
			t.Errorf("%s/%s failed although only the read rules are missing: %s", probe.Mode, probe.ID, probe.Detail)
		}
		if probe.Class == classReadScope {
			readRefusals++
		}
	}
	if readRefusals == 0 {
		t.Fatal("no read-scope probe ran")
	}
}

// readScopedWorld is a live session under the workarea read scope.
func readScopedWorld(t *testing.T, readPaths ...string) (specWorld, *Plan) {
	t.Helper()
	w, c := liveWorld(t)
	spec := w.spec()
	spec.ReadScope = agent.FileReadWorkarea
	spec.ReadPaths = readPaths
	plan, err := c.prepare(spec, "")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	t.Cleanup(func() { _ = plan.Release() })
	return w, plan
}

// TestSeatbelt_ReadScopeFindIsFastAndBlind is the whole-disk search under
// the workarea read scope: find over the root to depth six finishes in
// under five seconds and never reports a sentinel in the operator's home,
// and find over the home is refused at the top instead of walking it. The
// same search with the blanket read deny stripped finds the sentinel.
func TestSeatbelt_ReadScopeFindIsFastAndBlind(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	if home, err = filepath.EvalSymlinks(home); err != nil {
		t.Fatal(err)
	}
	name := ".donmai-read-scope-sentinel-" + randomSuffix()
	sentinel := filepath.Join(home, name)
	if err := os.WriteFile(sentinel, []byte("sentinel\n"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(sentinel) })
	out, err := exec.Command("/usr/bin/find", home, "-maxdepth", "1", "-name", name).Output() //nolint:gosec // G204: fixed tool, test paths.
	if err != nil || !strings.Contains(string(out), sentinel) {
		t.Fatalf("unconfined control: find over the home did not report the sentinel (err %v): %q", err, out)
	}

	w, plan := readScopedWorld(t)
	start := time.Now()
	_, out2 := runConfined(t, plan, w.mut, nil, "/usr/bin/find", "/", "-maxdepth", "6", "-name", name)
	elapsed := time.Since(start)
	if strings.Contains(out2, sentinel) {
		t.Errorf("confined find over the root reported the sentinel in the home")
	}
	if elapsed >= 5*time.Second {
		t.Errorf("confined find over the root to depth 6 took %s, want under 5s", elapsed)
	}
	t.Logf("confined find / -maxdepth 6: %s", elapsed.Round(time.Millisecond))

	start = time.Now()
	_, out2 = runConfined(t, plan, w.mut, nil, "/usr/bin/find", home, "-maxdepth", "4", "-name", name)
	if strings.Contains(out2, sentinel) {
		t.Error("confined find over the home reported the sentinel")
	}
	if !strings.Contains(out2, "Operation not permitted") {
		t.Errorf("confined find over the home was not refused at the top: %q", out2)
	}
	t.Logf("confined find <home> -maxdepth 4: %s", time.Since(start).Round(time.Millisecond))
	if code, out := runConfined(t, plan, w.mut, nil, "/bin/ls", home); code == 0 {
		t.Errorf("confined ls of the home succeeded: %q", out)
	}
	if code, _ := runConfined(t, plan, w.mut, nil, "/bin/cat", sentinel); code == 0 {
		t.Error("confined cat of the sentinel succeeded")
	}
	if code, _ := runConfined(t, plan, w.mut, nil, "/bin/test", "-f", sentinel); code != 0 {
		t.Error("confined stat of the sentinel failed; metadata must stay readable")
	}

	_, red := runStripped(t, plan, w.mut, "(deny file-read-data)\n", "/usr/bin/find", home, "-maxdepth", "1", "-name", name)
	if !strings.Contains(red, sentinel) {
		t.Fatalf("with the read deny stripped, find over the home did not report the sentinel; the test does not discriminate: %q", red)
	}
}

// goEnv returns one go env value from outside the boundary.
func goEnv(t *testing.T, key string) string {
	t.Helper()
	out, err := exec.Command("go", "env", key).Output() //nolint:gosec // G204: fixed tool, fixed key.
	if err != nil {
		t.Fatalf("go env %s: %v", key, err)
	}
	return strings.TrimSpace(string(out))
}

// TestSeatbelt_ReadScopeSeatWork: ordinary seat work still succeeds inside
// the workarea under the read scope — git status and commit with the user
// config declared readable, a go build with the toolchain declared, and
// node and pnpm where they are installed — while an undeclared git user
// config is a fatal error for git, which is why an adapter declares it.
func TestSeatbelt_ReadScopeSeatWork(t *testing.T) {
	base := shortTempDir(t, "dcg")
	gitconfig := filepath.Join(base, "gitconfig")
	if err := os.WriteFile(gitconfig, []byte("[user]\n\tname = probe\n\temail = probe@example.invalid\n[init]\n\tdefaultBranch = main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reads := []string{goEnv(t, "GOROOT"), gitconfig}
	optional := map[string]bool{}
	for _, tool := range []string{"node", "pnpm"} {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Logf("%s is not installed here; not exercised", tool)
			continue
		}
		root, err := InstallRoot(path)
		if err != nil {
			t.Fatalf("InstallRoot(%s): %v", tool, err)
		}
		if resolved, _ := filepath.EvalSymlinks(path); strings.Contains(resolved, string(filepath.Separator)+"corepack"+string(filepath.Separator)) {
			// A corepack shim runs a package manager it downloaded into a
			// cache under the home: a host declares that cache, this test
			// does not guess it.
			t.Logf("%s is a corepack shim; not exercised", tool)
			continue
		}
		if out, err := exec.Command(path, "--version").CombinedOutput(); err != nil { //nolint:gosec // G204: a tool found on PATH.
			t.Logf("%s --version fails unconfined (%v: %s); not exercised", tool, err, strings.TrimSpace(string(out)))
			continue
		}
		reads = append(reads, root)
		optional[tool] = true
	}
	w, plan := readScopedWorld(t, reads...)
	gomod, gopath := filepath.Join(w.cache, "go-mod"), filepath.Join(w.cache, "go-path")
	for _, dir := range []string{gomod, gopath} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	env := []string{"GIT_CONFIG_GLOBAL=" + gitconfig, "GIT_CONFIG_NOSYSTEM=1", "GOMODCACHE=" + gomod, "GOPATH=" + gopath, "GOTOOLCHAIN=local", "GOFLAGS=", "GOENV=off"}
	repo := filepath.Join(w.mut, "repo")
	script := strings.Join([]string{
		`set -e`,
		`mkdir -p "` + repo + `" && cd "` + repo + `"`,
		`git init -q .`,
		`printf 'module example.com/probe\n\ngo 1.21\n' > go.mod`,
		`printf 'package main\n\nfunc main() { println("ok") }\n' > main.go`,
		`git add -A`,
		`git commit -q -m first`,
		`git status --porcelain`,
		`git log --oneline | grep -q first`,
		`go build -o "$TMPDIR/probe" .`,
		`"$TMPDIR/probe"`,
	}, "\n")
	if code, out := runConfined(t, plan, w.mut, env, "/bin/sh", "-c", script); code != 0 || !strings.Contains(out, "ok") {
		t.Fatalf("git and go under the read scope: exit %d: %s", code, out)
	}
	for tool := range optional {
		if code, out := runConfined(t, plan, w.mut, nil, tool, "--version"); code != 0 {
			t.Errorf("%s --version under the read scope: exit %d: %s", tool, code, out)
		}
	}

	undeclared := filepath.Join(base, "undeclared-gitconfig")
	if err := os.WriteFile(undeclared, []byte("[user]\n\tname = probe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := runConfined(t, plan, w.mut, []string{"GIT_CONFIG_GLOBAL=" + undeclared, "GIT_CONFIG_NOSYSTEM=1"}, "git", "-C", repo, "status")
	if code == 0 || !strings.Contains(out, "Operation not permitted") {
		t.Fatalf("git with an undeclared user config: exit %d: %s; want a refusal", code, out)
	}
}
