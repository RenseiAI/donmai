package confinement

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

func TestParseLookups(t *testing.T) {
	got := parseLookups("com.a=0 com.b=1100 junk c=x")
	if len(got) != 2 || got["com.a"] != 0 || got["com.b"] != 1100 {
		t.Fatalf("parseLookups = %v", got)
	}
}

// TestRunProbe_FileOperationsUnconfined runs the probe's file operations with
// no boundary: each one must actually perform its change, or a refused
// verdict under a boundary would mean nothing.
func TestRunProbe_FileOperationsUnconfined(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"w", "tr", "ch", "ut", "xa", "rm", "mv", "ln"} {
		if err := seed(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	at := func(name string) string { return filepath.Join(dir, name) }
	steps := []harnessStep{}
	fx := &fixture{}
	fx.add(classPositive, true, probeStep{ID: "create", Op: opCreate, Path: at("n")}, existsEffect(at("n")))
	fx.add(classPositive, true, probeStep{ID: "write", Op: opWrite, Path: at("w")}, changedEffect(at("w")))
	fx.add(classPositive, true, probeStep{ID: "truncate", Op: opTruncate, Path: at("tr")}, changedEffect(at("tr")))
	fx.add(classPositive, true, probeStep{ID: "chmod", Op: opChmod, Path: at("ch")}, modeEffect(at("ch"), 0o644))
	fx.add(classPositive, true, probeStep{ID: "utimes", Op: opUtimes, Path: at("ut")}, timeEffect(at("ut")))
	if runtime.GOOS == "darwin" {
		// Not every Linux test filesystem takes user extended attributes;
		// the only backend, and so the only place this probe matters, is
		// macOS.
		fx.add(classPositive, true, probeStep{ID: "xattr", Op: opXattr, Path: at("xa")}, xattrEffect(at("xa")))
	}
	fx.add(classPositive, true, probeStep{ID: "remove", Op: opRemove, Path: at("rm")}, func(stepResult) bool { return !exists(at("rm")) })
	fx.add(classPositive, true, probeStep{ID: "mkdir", Op: opMkdir, Path: at("d")}, existsEffect(at("d")))
	fx.add(classPositive, true, probeStep{ID: "rename", Op: opRename, Path: at("mv"), Path2: at("mv2")}, existsEffect(at("mv2")))
	fx.add(classPositive, true, probeStep{ID: "link", Op: opLink, Path: at("ln"), Path2: at("ln2")}, existsEffect(at("ln2")))
	fx.add(classPositive, true, probeStep{ID: "symlink", Op: opSymlink, Path: at("sl"), Path2: at("w")}, existsEffect(at("sl")))
	fx.add(classPositive, true, probeStep{ID: "socket", Op: opListen, Path: at("s")}, func(res stepResult) bool { return res.Err == "" })
	steps = append(steps, fx.steps...)

	var probe []probeStep
	for _, step := range steps {
		probe = append(probe, step.probe)
	}
	probe = append(probe, probeStep{ID: "unknown", Op: "format_disk"}, probeStep{ID: "dial-missing", Op: opDial, Path: at("absent")})
	planPath := filepath.Join(dir, "plan.json")
	raw, err := json.Marshal(probePlan{ResultPath: probeResultPath(dir), Steps: probe})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ProbeEnv, planPath)
	handled, code := RunProbeFromEnv()
	if !handled || code != 0 {
		t.Fatalf("RunProbeFromEnv = %v, %d", handled, code)
	}
	raw, err = os.ReadFile(probeResultPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	var results []stepResult
	if err := json.Unmarshal(raw, &results); err != nil {
		t.Fatal(err)
	}
	byID := map[string]stepResult{}
	for _, result := range results {
		byID[result.ID] = result
	}
	for _, step := range steps {
		result := byID[step.probe.ID]
		if result.Err != "" || !step.effect(result) {
			t.Errorf("%s: err=%q effect=%v; the probe operation did not perform its change", step.probe.ID, result.Err, step.effect(result))
		}
	}
	if byID["unknown"].Err == "" {
		t.Error("an unknown probe operation succeeded")
	}
	if byID["dial-missing"].Err == "" {
		t.Error("dialing a missing socket succeeded")
	}
}

func TestRunProbeFromEnv_NotAProbe(t *testing.T) {
	t.Setenv(ProbeEnv, "")
	if handled, _ := RunProbeFromEnv(); handled {
		t.Fatal("RunProbeFromEnv ran without a plan")
	}
	t.Setenv(ProbeEnv, filepath.Join(t.TempDir(), "absent.json"))
	if handled, code := RunProbeFromEnv(); !handled || code == 0 {
		t.Fatalf("RunProbeFromEnv with a missing plan = %v, %d", handled, code)
	}
}

func TestTCPHost(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"", "127.0.0.1"},
		{"127.0.0.1", "127.0.0.1"},
		{"::1", "::1"},
		{"localhost", "localhost"},
		{"example.invalid", "127.0.0.1"},
	} {
		if got := tcpHost(tt.in); got != tt.want {
			t.Errorf("tcpHost(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestRunProbe_TCPDialUnconfined(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	dir := t.TempDir()
	steps := []probeStep{{ID: "open", Op: opTCPDial, Port: port}, {ID: "closed", Op: opTCPDial, Port: closedLoopbackPort(t)}}
	// The IPv6 and hostname variants dial the same open port: without a
	// listener on the matching target each would fail unconfined and the
	// confined verdict would mean nothing.
	if listener6, err := net.Listen("tcp", net.JoinHostPort("::1", itoaPort6(port))); err == nil {
		defer func() { _ = listener6.Close() }()
		go acceptProbeListener(listener6)
		steps = append(steps, probeStep{ID: "open6", Op: opTCPDial, Host: "::1", Port: port})
		steps = append(steps, probeStep{ID: "openhost", Op: opTCPDial, Host: "localhost", Port: port})
	} else {
		t.Logf("no IPv6 loopback listener: %v", err)
	}
	planPath := filepath.Join(dir, "plan.json")
	raw, err := json.Marshal(probePlan{ResultPath: probeResultPath(dir), Steps: steps})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ProbeEnv, planPath)
	if handled, code := RunProbeFromEnv(); !handled || code != 0 {
		t.Fatalf("RunProbeFromEnv = %v, %d", handled, code)
	}
	raw, err = os.ReadFile(probeResultPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	var results []stepResult
	if err := json.Unmarshal(raw, &results); err != nil {
		t.Fatal(err)
	}
	byID := map[string]stepResult{}
	for _, result := range results {
		byID[result.ID] = result
	}
	if byID["open"].Err != "" {
		t.Errorf("dial to a listening loopback port failed unconfined: %q", byID["open"].Err)
	}
	if byID["closed"].Err == "" {
		t.Error("dial to a closed loopback port succeeded; the probe cannot discriminate")
	}
	for _, id := range []string{"open6", "openhost"} {
		result, ok := byID[id]
		if !ok {
			continue
		}
		if result.Err != "" {
			t.Errorf("dial %s to a listening loopback port failed unconfined: %q", id, result.Err)
		}
	}
}

func itoaPort6(port int) string {
	return strconv.Itoa(port)
}

func acceptProbeListener(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		_ = conn.Close()
	}
}

func closedLoopbackPort(t *testing.T) int {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()
	return port
}
