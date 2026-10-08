//go:build linux

package confinement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/ptyhost"
)

func newLinuxLiveConfiner(t *testing.T, backend Backend, extra ExtraRules) (*Confiner, string) {
	t.Helper()
	home, stateHome, profileDir := hostDirs(t)
	c, err := New(Options{Backend: backend, ProfileDir: profileDir, Home: home, StateHome: stateHome, ExtraRules: extra, ExecutableDigest: "sha256:test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, stateHome
}

func linuxProbeCommand(t *testing.T) []string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return []string{exe}
}

// linuxConfinementAvailable reports whether this host can run a confined
// seat at all: the launcher present and able to build a boundary, and the
// Landlock stage stageable — exactly what Check proves. The test skips —
// never passes silently — where any of them is missing, with the
// diagnostic Check carries.
func linuxConfinementAvailable(t *testing.T) bool {
	t.Helper()
	b := DefaultBackend()
	if b == nil {
		t.Skip("no confinement backend for this operating system")
	}
	if err := b.Check(); err != nil {
		t.Skipf("confinement unavailable here: %v", err)
	}
	return true
}

// TestLinux_SelfTestPassesBothModes is the live backend proof: the probe
// set passes through the headless and the PTY spawn path under the
// mount-namespace backend — the confined seat cannot write outside the
// writable set or read hidden paths, and can write inside each writable
// class. It needs the launcher, user namespaces and Landlock, so it skips
// honestly where the kernel or container disallows them.
func TestLinux_SelfTestPassesBothModes(t *testing.T) {
	if !linuxConfinementAvailable(t) {
		return
	}
	c, _ := newLinuxLiveConfiner(t, DefaultBackend(), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	record, err := c.SelfTest(ctx, SelfTestOptions{ProbeCommand: linuxProbeCommand(t), ScratchDir: shortTempDir(t, "dcs")})
	if err != nil {
		t.Fatalf("SelfTest: %v\n%s", err, failureIDs(record))
	}
	if !record.Passed || len(record.SessionModes) != 2 {
		t.Fatalf("record passed=%v modes=%v\n%s", record.Passed, record.SessionModes, failureIDs(record))
	}
	if record.Backend != BackendLinuxMountNamespace {
		t.Fatalf("record backend = %q, want the mount-namespace backend", record.Backend)
	}
	t.Logf("self-test passed: %d probes across %v, backend %s, Landlock ABI %d", len(record.Probes), record.SessionModes, record.BackendVersion, landlockABI())
}

// TestLinux_SelfTestRedWithoutBackend is the discriminating control, probe
// by probe: with the backend replaced by nothing, every probe that expects
// a refusal must fail, in both session modes — each one is held by the
// backend and nothing else — unless the record names what else on this
// host refuses it (HeldBy). A probe the host cannot run is recorded as not
// probed, never counted. Every positive control still passes.
func TestLinux_SelfTestRedWithoutBackend(t *testing.T) {
	c, _ := newLinuxLiveConfiner(t, noopBackend{}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	record, err := c.SelfTest(ctx, SelfTestOptions{ProbeCommand: linuxProbeCommand(t), ScratchDir: shortTempDir(t, "dcs")})
	if reason, _ := ReasonOf(err); reason != ReasonSelfTestFailed {
		t.Fatalf("SelfTest with no backend: err=%v, want self_test_failed", err)
	}
	var unheld, osHeld []string
	refusals, positives := 0, 0
	for _, probe := range record.Probes {
		if probe.Expected == outcomeAccepted {
			positives++
			if !probe.Pass {
				t.Errorf("positive control %s/%s failed with no backend: %s", probe.Mode, probe.ID, probe.Detail)
			}
			continue
		}
		refusals++
		switch {
		case probe.HeldBy != "":
			osHeld = append(osHeld, string(probe.Mode)+"/"+probe.ID+" ("+probe.HeldBy+")")
		case probe.Pass:
			unheld = append(unheld, string(probe.Mode)+"/"+probe.ID)
		}
	}
	if len(unheld) > 0 {
		t.Fatalf("%d refusal probe(s) pass with no backend, so the self-test cannot tell they are held:\n%s", len(unheld), strings.Join(unheld, "\n"))
	}
	t.Logf("no backend: %d refusal probes, every one failed except %d the host itself refuses %v; %d positive controls passed; not probed: %d", refusals, len(osHeld), osHeld, positives, len(record.NotProbed))
}

// liveHTTPSURLEnv overrides the external HTTPS endpoint the live seat test
// reaches; the default is a public module proxy.
const liveHTTPSURLEnv = "DONMAI_CONFINEMENT_LIVE_HTTPS_URL"

// daemonDefaultPort is the local daemon control API's default port. The
// seat test stands its listener there when the port is free, so the dial
// is the one a seat would make.
const daemonDefaultPort = "7734"

// TestLinux_ConfinedSeatReachesNetworkAndHoldsDenials starts a real seat
// through the production path — the self-test gate, Confiner.Prepare,
// plan.Command and the headless launcher — under the workarea read scope,
// and checks from inside it: a loopback listener standing on the daemon
// control port (an undeclared port) answers an HTTP request, an external
// HTTPS endpoint answers, writes land in the mutable leaf, and writes and
// reads outside the work area refuse. The network reach is what the
// backend declares (loopback egress open, outbound TCP unfiltered); the
// denials prove the same seat ran confined. The HTTPS step needs outbound
// network from this host and reports a skip where the same request fails
// outside the boundary too.
func TestLinux_ConfinedSeatReachesNetworkAndHoldsDenials(t *testing.T) {
	if !linuxConfinementAvailable(t) {
		return
	}
	c, stateHome := newLinuxLiveConfiner(t, DefaultBackend(), nil)
	home := filepath.Dir(stateHome)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if record, err := c.SelfTest(ctx, SelfTestOptions{ProbeCommand: linuxProbeCommand(t), ScratchDir: shortTempDir(t, "dcs")}); err != nil {
		t.Fatalf("SelfTest: %v\n%s", err, failureIDs(record))
	}

	ws := filepath.Join(stateHome, "ws")
	mut := filepath.Join(ws, "repo")
	ro := filepath.Join(ws, "ref")
	state := filepath.Join(stateHome, "state", "s1")
	tmp := filepath.Join(stateHome, "tmp", "s1")
	for _, dir := range []string{filepath.Join(mut, ".git"), ro, state, tmp} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	homeSecret := filepath.Join(home, ".seat-secret")
	roFile := filepath.Join(ro, "README")
	for _, file := range []string{homeSecret, roFile} {
		if err := os.WriteFile(file, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	daemonListener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", daemonDefaultPort))
	if err != nil {
		daemonListener, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("daemon stand-in listener: %v", err)
		}
	}
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})}
	go func() { _ = server.Serve(daemonListener) }()
	t.Cleanup(func() { _ = server.Close() })
	daemonURL := "http://" + daemonListener.Addr().String() + "/api/daemon/health"
	t.Logf("daemon stand-in on %s (undeclared for the seat)", daemonListener.Addr())

	steps := []seatCheckStep{
		{ID: "daemon_port", Op: "get", Target: daemonURL},
		{ID: "write_leaf", Op: "write", Target: filepath.Join(mut, "ok")},
		{ID: "read_ro_leaf", Op: "read", Target: roFile},
		{ID: "write_ro_leaf", Op: "write", Target: roFile},
		{ID: "write_home", Op: "write", Target: filepath.Join(home, ".seat-planted")},
		{ID: "write_workarea_root", Op: "write", Target: filepath.Join(ws, "planted")},
		{ID: "write_shared_tmp", Op: "write", Target: filepath.Join(os.TempDir(), "seat-planted-"+randomSuffix())},
		{ID: "read_home_secret", Op: "read", Target: homeSecret},
		{ID: "read_procfs", Op: "read", Target: "/proc/self/environ"},
	}
	// Processes and abstract sockets outside the boundary: a decoy of the
	// same user, open to attachment by any of its processes.
	marker := filepath.Join(shortTempDir(t, "dcm"), "signalled")
	decoy := strconv.Itoa(startTestDecoy(t, marker))
	steps = append(steps,
		seatCheckStep{ID: "whoami", Op: "whoami"},
		seatCheckStep{ID: "signal_outside", Op: "signal", Target: decoy},
		seatCheckStep{ID: "ptrace_outside", Op: "ptrace", Target: decoy},
		seatCheckStep{ID: "read_process_outside", Op: "proc_read", Target: decoy},
	)
	var abstract abstractListener
	if scoped, why := abstractSocketsScoped(); scoped {
		abstract = newAbstractListener(t)
		steps = append(steps, seatCheckStep{ID: "abstract_outside", Op: "dial_abstract", Target: abstract.name})
	} else {
		t.Logf("abstract sockets not probed: %s", why)
	}
	httpsURL := os.Getenv(liveHTTPSURLEnv)
	if httpsURL == "" {
		httpsURL = "https://proxy.golang.org/"
	}
	outside := runSeatCheckStep(seatCheckStep{ID: "https", Op: "get", Target: httpsURL})
	if outside.Err == "" {
		steps = append(steps, seatCheckStep{ID: "https", Op: "get", Target: httpsURL})
	}

	spec := Spec{
		SessionID:      "live-seat",
		HarnessID:      "live-seat",
		SessionMode:    agent.PromptModeAutonomous,
		WorkareaRoot:   ws,
		MutableLeaves:  []string{mut},
		HarnessState:   []string{state},
		SessionTmp:     tmp,
		ReadOnlyLeaves: []string{ro},
		Sockets:        ResolverSockets(),
		ReadScope:      agent.FileReadWorkarea,
	}
	plan, err := c.Prepare(spec)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	t.Cleanup(func() { _ = plan.Release() })
	results := runSeatCheck(t, tmp, steps, func(self, planPath string) (string, error) {
		argv, err := plan.Command([]string{self, "-test.run=^$"})
		if err != nil {
			return "", err
		}
		code, err := HeadlessLauncher()(ctx, argv, append(plan.Environment(), seatCheckEnv+"="+planPath), mut)
		if err == nil && code != 0 {
			err = fmt.Errorf("the seat exited %d", code)
		}
		return "", err
	})

	if res := results["daemon_port"]; res.Err != "" || res.Status != http.StatusNoContent {
		t.Errorf("the seat could not reach the loopback daemon port: %+v", res)
	}
	if res := results["write_leaf"]; res.Err != "" {
		t.Errorf("a write in the mutable leaf refused: %+v", res)
	}
	if res := results["read_ro_leaf"]; res.Err != "" {
		t.Errorf("a read of the read-only leaf refused: %+v", res)
	}
	refused := []string{
		"write_ro_leaf", "write_home", "write_workarea_root", "write_shared_tmp", "read_home_secret", "read_procfs",
		"signal_outside", "ptrace_outside", "read_process_outside",
	}
	if abstract.counter != nil {
		refused = append(refused, "abstract_outside")
	}
	for _, id := range refused {
		if res := results[id]; res.Err == "" {
			t.Errorf("%s: the seat got through; want a refusal", id)
		} else {
			t.Logf("%s refused: %s", id, res.Err)
		}
	}
	// The seat runs in a process namespace of its own, under an init that
	// reaps it: pid 1 there is the launcher's init, never the seat.
	if out := results["whoami"].Output; !strings.HasSuffix(out, " ppid=1") || strings.HasPrefix(out, "pid=1 ") {
		t.Errorf("seat identity %q: want a private process namespace with the launcher's init as pid 1", out)
	} else {
		t.Logf("seat identity inside the boundary: %s", out)
	}
	if exists(marker) {
		t.Error("the decoy outside the boundary received the seat's signal")
	}
	if abstract.counter != nil && abstract.counter.accepted() > 0 {
		t.Error("the abstract socket outside the boundary accepted the seat's connection")
	}
	for _, path := range []string{filepath.Join(home, ".seat-planted"), filepath.Join(ws, "planted")} {
		if _, err := os.Lstat(path); err == nil {
			t.Errorf("a write outside the work area landed on the host: %s", path)
		}
	}
	t.Run("https", func(t *testing.T) {
		if outside.Err != "" {
			t.Skipf("no outbound HTTPS from this host even outside the boundary (%s): %s", httpsURL, outside.Err)
		}
		if res := results["https"]; res.Err != "" || res.Status == 0 {
			t.Fatalf("the seat could not reach %s: %+v", httpsURL, res)
		} else {
			t.Logf("seat reached %s: HTTP %d", httpsURL, res.Status)
		}
	})
	if strings.Contains(results["read_home_secret"].Err, "no such file") {
		t.Errorf("the home secret read refused as missing, not denied: the operator home is bound for metadata, so the refusal must come from the read scope")
	}
}

// TestLinux_ComposerDeniesHoldInsideTheSeat starts a real seat with reads
// open under composer rules, through the self-test gate and the production
// spawn path, and checks from inside it: a read deny on a directory in the
// operator home (which the tree binds read-only and, with reads open, the
// stage grants) refuses, as does one inside the mutable leaf and one nested
// inside it; a write deny inside the mutable leaf refuses writes while its
// contents stay readable; and the controls — a plain home file, a write in
// the leaf — still go through.
func TestLinux_ComposerDeniesHoldInsideTheSeat(t *testing.T) {
	if !linuxConfinementAvailable(t) {
		return
	}
	var rules []Rule
	c, stateHome := newLinuxLiveConfiner(t, DefaultBackend(), func(RuleContext) []Rule { return rules })
	home := filepath.Dir(stateHome)
	ws := filepath.Join(stateHome, "ws")
	mut := filepath.Join(ws, "repo")
	private := filepath.Join(mut, "private")
	vendor := filepath.Join(mut, "vendor")
	secrets := filepath.Join(home, ".secrets")
	state := filepath.Join(stateHome, "state", "s1")
	tmp := filepath.Join(stateHome, "tmp", "s1")
	for _, dir := range []string{filepath.Join(mut, ".git"), private, vendor, secrets, state, tmp} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"secret":  filepath.Join(secrets, "key"),
		"private": filepath.Join(private, "f"),
		"nested":  filepath.Join(private, "token"),
		"vendor":  filepath.Join(vendor, "f"),
		"plain":   filepath.Join(home, "plain"),
	}
	for _, file := range files {
		if err := os.WriteFile(file, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rules = []Rule{
		{Kind: RuleDenyRead, Path: secrets, Scope: ScopeSubtree},
		{Kind: RuleDenyRead, Path: files["nested"], Scope: ScopeLiteral},
		{Kind: RuleDenyRead, Path: private, Scope: ScopeSubtree},
		{Kind: RuleDenyWrite, Path: vendor, Scope: ScopeSubtree},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if record, err := c.SelfTest(ctx, SelfTestOptions{ProbeCommand: linuxProbeCommand(t), ScratchDir: shortTempDir(t, "dcs")}); err != nil {
		t.Fatalf("SelfTest: %v\n%s", err, failureIDs(record))
	}
	plan, err := c.Prepare(Spec{
		SessionID: "live-composer", HarnessID: "live-composer", SessionMode: agent.PromptModeAutonomous,
		WorkareaRoot: ws, MutableLeaves: []string{mut}, HarnessState: []string{state}, SessionTmp: tmp,
		Sockets: ResolverSockets(),
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	t.Cleanup(func() { _ = plan.Release() })
	results := runSeatCheck(t, tmp, []seatCheckStep{
		{ID: "read_secret", Op: "read", Target: files["secret"]},
		{ID: "read_private", Op: "read", Target: files["private"]},
		{ID: "read_nested", Op: "read", Target: files["nested"]},
		{ID: "write_vendor", Op: "write", Target: files["vendor"]},
		{ID: "create_vendor", Op: "write", Target: filepath.Join(vendor, "new")},
		{ID: "read_vendor", Op: "read", Target: files["vendor"]},
		{ID: "read_plain", Op: "read", Target: files["plain"]},
		{ID: "write_leaf", Op: "write", Target: filepath.Join(mut, "ok")},
	}, func(self, planPath string) (string, error) {
		argv, err := plan.Command([]string{self, "-test.run=^$"})
		if err != nil {
			return "", err
		}
		code, err := HeadlessLauncher()(ctx, argv, append(plan.Environment(), seatCheckEnv+"="+planPath), mut)
		if err == nil && code != 0 {
			err = fmt.Errorf("the seat exited %d", code)
		}
		return "", err
	})
	for _, id := range []string{"read_secret", "read_private", "read_nested", "write_vendor", "create_vendor"} {
		if res := results[id]; res.Err == "" {
			t.Errorf("%s: the seat got through a composer deny", id)
		} else {
			t.Logf("%s refused: %s", id, res.Err)
		}
	}
	for _, id := range []string{"read_vendor", "read_plain", "write_leaf"} {
		if res := results[id]; res.Err != "" {
			t.Errorf("%s refused: %s; a composer deny must not reach past its path", id, res.Err)
		}
	}
	if raw, err := os.ReadFile(files["vendor"]); err != nil || string(raw) != "x\n" {
		t.Errorf("the write-denied file changed on the host: %q %v", raw, err)
	}
}

// prepareLiveSeat prepares a real seat under the Linux backend for one
// session mode, gated by a self-test record for this host's fingerprint
// (the self-test itself is proven by the tests above), and returns the
// plan with its mutable leaf and session tmp.
func prepareLiveSeat(t *testing.T, mode agent.PromptSessionMode) (plan *Plan, mut, tmp string) {
	t.Helper()
	c, stateHome := newLinuxLiveConfiner(t, DefaultBackend(), nil)
	current, err := c.fingerprint()
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	c.store(SelfTestRecord{
		Backend: current.backend, BackendVersion: current.backendVersion, ProbeSetVersion: ProbeSetVersion,
		ExecutableDigest: current.executableDigest, Passed: true,
		SessionModes: []agent.PromptSessionMode{agent.PromptModeAutonomous, agent.PromptModeHumanControlled},
		Probes:       []ProbeOutcome{{ID: "p", Pass: true}},
	})
	ws := filepath.Join(stateHome, "ws")
	mut = filepath.Join(ws, "repo")
	state := filepath.Join(stateHome, "state", "s1")
	tmp = filepath.Join(stateHome, "tmp", "s1")
	for _, dir := range []string{filepath.Join(mut, ".git"), state, tmp} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	plan, err = c.Prepare(Spec{
		SessionID: "live-" + string(mode), HarnessID: "live", SessionMode: mode,
		WorkareaRoot: ws, MutableLeaves: []string{mut}, HarnessState: []string{state}, SessionTmp: tmp,
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	t.Cleanup(func() { _ = plan.Release() })
	return plan, mut, tmp
}

// TestLinux_InteractiveSeatOwnsItsTerminal starts a seat through the PTY
// host, as the interactive spawn path does, and checks from inside it: the
// seat is its terminal's foreground process group and opens /dev/tty, it
// starts, signals and reaps a child of its own, and a Ctrl-C typed into the
// terminal reaches it as SIGINT while the boundary around it survives — the
// launcher, outside the foreground group, is never interrupted.
func TestLinux_InteractiveSeatOwnsItsTerminal(t *testing.T) {
	if !linuxConfinementAvailable(t) {
		return
	}
	plan, mut, tmp := prepareLiveSeat(t, agent.PromptModeHumanControlled)
	ready := filepath.Join(tmp, "ready")
	var session *ptyhost.Session
	results := runSeatCheck(t, tmp, []seatCheckStep{
		{ID: "foreground", Op: "foreground"},
		{ID: "child_tree", Op: "child_tree"},
		{ID: "ctrl_c", Op: "await_sigint", Target: ready},
	}, func(self, planPath string) (string, error) {
		argv, err := plan.Command([]string{self, "-test.run=^$"})
		if err != nil {
			return "", err
		}
		session, err = ptyhost.Spawn(ptyhost.Spec{Command: argv, Env: append(plan.Environment(), seatCheckEnv+"="+planPath), Cwd: mut})
		if err != nil {
			return "", err
		}
		if !waitExistsWithin(ready, 30*time.Second) {
			_ = session.Stop(context.Background())
			return "", fmt.Errorf("the seat never became ready")
		}
		if _, err := session.WriteInput([]byte{0x03}); err != nil {
			return "", err
		}
		select {
		case <-session.Done():
		case <-time.After(30 * time.Second):
			_ = session.Stop(context.Background())
			return "", fmt.Errorf("the seat did not finish after Ctrl-C")
		}
		if exit, _ := session.Exit(); exit.ExitCode != 0 {
			return "", fmt.Errorf("the seat exited %d", exit.ExitCode)
		}
		return "", nil
	})
	for _, id := range []string{"foreground", "child_tree", "ctrl_c"} {
		if res := results[id]; res.Err != "" {
			t.Errorf("%s: %s", id, res.Err)
		} else if res.Output != "" {
			t.Logf("%s: %s", id, res.Output)
		}
	}
	if got := results["ctrl_c"].Output; got != "interrupted" {
		t.Errorf("Ctrl-C reached the seat as %q, want interrupted", got)
	}
}

// TestLinux_SeatTreeDiesWithTheLauncher starts a headless seat that starts
// a descendant in a session of its own — out of every process group the
// supervisor could signal — holding a connection open to a listener
// outside. Killing the launcher, as the supervisor does, must close that
// connection: the process namespace's init dies with the launcher and takes
// every process inside with it.
func TestLinux_SeatTreeDiesWithTheLauncher(t *testing.T) {
	if !linuxConfinementAvailable(t) {
		return
	}
	plan, mut, tmp := prepareLiveSeat(t, agent.PromptModeAutonomous)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(tmp, "seat-plan.json")
	raw, err := json.Marshal(seatCheckPlan{ResultPath: filepath.Join(tmp, "seat-result.json"), Steps: []seatCheckStep{{ID: "orphan", Op: "orphan", Target: listener.Addr().String()}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	argv, err := plan.Command([]string{self, "-test.run=^$"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	launched := make(chan error, 1)
	go func() {
		_, err := HeadlessLauncher()(ctx, argv, append(plan.Environment(), seatCheckEnv+"="+planPath), mut)
		launched <- err
	}()
	accepted := make(chan net.Conn, 1)
	go func() {
		if conn, err := listener.Accept(); err == nil {
			accepted <- conn
		}
	}()
	var conn net.Conn
	select {
	case conn = <-accepted:
	case err := <-launched:
		t.Fatalf("the seat ended before its orphan connected: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the seat's orphan never connected")
	}
	defer func() { _ = conn.Close() }()
	cancel() // the supervisor's kill: SIGKILL to the launcher alone
	<-launched
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the orphan outlived its launcher (read: %v): a descendant escaped the boundary's lifetime", err)
	} else {
		t.Logf("the orphan died with the launcher: %v", err)
	}
}

// waitExistsWithin polls for path up to d.
func waitExistsWithin(path string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if exists(path) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return exists(path)
}
