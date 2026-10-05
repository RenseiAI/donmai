package confinement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// ProbeEnv names the environment variable that turns an executable into the
// self-test probe. Its value is the path of the probe plan. An executable
// that can serve as the probe calls RunProbeFromEnv first thing in main.
const ProbeEnv = "DONMAI_CONFINEMENT_PROBE"

// probeXattr is the extended attribute the probe sets.
const probeXattr = "user.donmai.confinement-probe"

// probeTime is the timestamp the probe writes.
var probeTime = time.Date(2001, time.September, 9, 1, 46, 40, 0, time.UTC)

// stepOp is the probe's closed operation vocabulary. Every external command
// a step runs is built here from fixed absolute paths; a plan names
// operations and paths, never a command line.
type stepOp string

const (
	opCreate       stepOp = "create"
	opWrite        stepOp = "write"
	opTruncate     stepOp = "truncate"
	opChmod        stepOp = "chmod"
	opUtimes       stepOp = "utimes"
	opXattr        stepOp = "xattr"
	opRemove       stepOp = "remove"
	opMkdir        stepOp = "mkdir"
	opRename       stepOp = "rename"
	opLink         stepOp = "link"
	opSymlink      stepOp = "symlink"
	opSymlinkWrite stepOp = "symlink_write"
	opListen       stepOp = "listen"
	opDial         stepOp = "dial"
	opTCPDial      stepOp = "tcp_dial"
	opDevWrite     stepOp = "dev_write"
	opStdout       stepOp = "stdout"
	opReenter      stepOp = "reenter"
	opJobSubmit    stepOp = "job_submit"
	opAppOpen      stepOp = "app_open"
	opLookup       stepOp = "lookup"
	opAttach       stepOp = "attach"
	opMount        stepOp = "mount"
	opPrefWrite    stepOp = "preference_write"
	opRead         stepOp = "read"
	opList         stepOp = "list"
	opStat         stepOp = "stat"
)

type probeStep struct {
	ID       string   `json:"id"`
	Op       stepOp   `json:"op"`
	Path     string   `json:"path,omitempty"`
	Path2    string   `json:"path2,omitempty"`
	Label    string   `json:"label,omitempty"`
	Services []string `json:"services,omitempty"`
	PID      int      `json:"pid,omitempty"`
	Port     int      `json:"port,omitempty"`
	// Host selects the dial target for the loopback TCP probe: one of
	// "127.0.0.1", "::1" or "localhost". Empty means the IPv4
	// loopback, so older plans keep their meaning.
	Host string `json:"host,omitempty"`
}

type probePlan struct {
	ResultPath string      `json:"resultPath"`
	Steps      []probeStep `json:"steps"`
}

type stepResult struct {
	ID      string         `json:"id"`
	Err     string         `json:"err,omitempty"`
	Exit    int            `json:"exit"`
	Output  string         `json:"output,omitempty"`
	Lookups map[string]int `json:"lookups,omitempty"`
}

// RunProbeFromEnv runs the self-test probe when ProbeEnv is set and reports
// whether it did, with the exit code the process should end with. The
// caller exits; a library never does.
func RunProbeFromEnv() (handled bool, exitCode int) {
	planPath := os.Getenv(ProbeEnv)
	if planPath == "" {
		return false, 0
	}
	if err := runProbe(planPath); err != nil {
		fmt.Fprintf(os.Stderr, "confinement probe: %v\n", err)
		return true, 2
	}
	return true, 0
}

func runProbe(planPath string) error {
	raw, err := os.ReadFile(planPath) //nolint:gosec // G304: the plan path is the probe's own input.
	if err != nil {
		return fmt.Errorf("read plan: %w", err)
	}
	var plan probePlan
	if err := json.Unmarshal(raw, &plan); err != nil {
		return fmt.Errorf("decode plan: %w", err)
	}
	results := make([]stepResult, 0, len(plan.Steps))
	for _, step := range plan.Steps {
		results = append(results, runStep(step))
	}
	encoded, err := json.Marshal(results)
	if err != nil {
		return fmt.Errorf("encode results: %w", err)
	}
	tmp := plan.ResultPath + ".partial"
	if err := os.WriteFile(tmp, encoded, 0o600); err != nil {
		return fmt.Errorf("write results: %w", err)
	}
	if err := os.Rename(tmp, plan.ResultPath); err != nil {
		return fmt.Errorf("write results: %w", err)
	}
	return nil
}

func runStep(step probeStep) stepResult {
	result := stepResult{ID: step.ID}
	var err error
	switch step.Op {
	case opCreate:
		err = createFile(step.Path)
	case opWrite:
		err = appendFile(step.Path)
	case opTruncate:
		err = os.Truncate(step.Path, 0)
	case opChmod:
		err = os.Chmod(step.Path, 0o777) //nolint:gosec // G302: the probe is meant to try a permission change.
	case opUtimes:
		err = os.Chtimes(step.Path, probeTime, probeTime)
	case opXattr:
		err = unix.Setxattr(step.Path, probeXattr, []byte("1"), 0)
	case opRemove:
		err = os.Remove(step.Path)
	case opMkdir:
		err = os.Mkdir(step.Path, 0o755) //nolint:gosec // G301: an ordinary directory.
	case opRename:
		err = os.Rename(step.Path, step.Path2)
	case opLink:
		err = os.Link(step.Path, step.Path2)
	case opSymlink:
		err = os.Symlink(step.Path2, step.Path)
	case opSymlinkWrite:
		if err = os.Symlink(step.Path, step.Path2); err == nil {
			err = appendFile(step.Path2)
		}
	case opListen:
		err = listenAndDial(step.Path)
	case opDial:
		err = dial(step.Path)
	case opTCPDial:
		err = dialTCP(tcpHost(step.Host), step.Port)
	case opDevWrite:
		err = appendFile(step.Path)
	case opStdout:
		_, err = os.Stdout.WriteString("confinement probe\n")
	case opReenter:
		result.Exit, result.Output, err = runTool("/usr/bin/sandbox-exec", "-p", "(version 1)(allow default)", "/usr/bin/touch", step.Path)
	case opJobSubmit:
		result.Exit, result.Output, err = runTool("/bin/launchctl", "submit", "-l", step.Label, "--", "/usr/bin/touch", step.Path)
	case opAppOpen:
		result.Exit, result.Output, err = runTool("/usr/bin/open", "-g", "-j", step.Path2)
	case opLookup:
		args := append([]string{"-l", "JavaScript", "-e", lookupScript}, step.Services...)
		result.Exit, result.Output, err = runTool("/usr/bin/osascript", args...)
		result.Lookups = parseLookups(result.Output)
	case opAttach:
		result.Exit, result.Output, err = runTool("/usr/bin/vmmap", "--summary", strconv.Itoa(step.PID))
	case opPrefWrite:
		result.Exit, result.Output, err = runTool("/usr/bin/defaults", "write", step.Label, "probe", "1")
	case opMount:
		result.Exit, result.Output, err = runTool("/usr/bin/hdiutil", "attach", "-nobrowse", "-noverify", "-noautoopen", "-mountpoint", step.Path, step.Path2)
	case opRead:
		_, err = os.ReadFile(step.Path) //nolint:gosec // G304: the probe's own target.
	case opList:
		_, err = os.ReadDir(step.Path)
	case opStat:
		_, err = os.Lstat(step.Path)
	default:
		err = fmt.Errorf("unknown probe operation %q", step.Op)
	}
	if err != nil {
		result.Err = errnoText(err)
	}
	return result
}

func createFile(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644) //nolint:gosec // G302,G304: the probe's own target.
	if err != nil {
		return err
	}
	if _, err := f.WriteString("probe\n"); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func appendFile(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0) //nolint:gosec // G304: the probe's own target.
	if err != nil {
		return err
	}
	if _, err := f.WriteString("probe\n"); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func listenAndDial(path string) error {
	listener, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	accepted := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			_ = conn.Close()
		}
		accepted <- err
	}()
	if err := dial(path); err != nil {
		return err
	}
	select {
	case err := <-accepted:
		return err
	case <-time.After(5 * time.Second):
		return errors.New("accept timed out")
	}
}

func dial(path string) error {
	conn, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		return err
	}
	return conn.Close()
}

// tcpHost normalizes the loopback TCP probe's dial target. The deny rule
// names the local machine, so the probe must cover the numeric IPv4 and
// IPv6 loopbacks and the hostname that may resolve to either.
func tcpHost(host string) string {
	switch host {
	case "::1", "localhost":
		return host
	default:
		return "127.0.0.1"
	}
}

// dialTCP connects to the loopback target on one port: the probe the
// profile's loopback deny is judged by.
func dialTCP(host string, port int) error {
	target := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		return err
	}
	return conn.Close()
}

// runTool runs one fixed tool with a bound and returns its exit code and a
// bounded slice of its output.
func runTool(name string, args ...string) (int, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: fixed absolute tools from the closed vocabulary.
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if len(text) > 512 {
		text = text[:512]
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), text, nil
	}
	if err != nil {
		return -1, text, err
	}
	return 0, text, nil
}

// lookupScript asks the bootstrap server for each named service and prints
// "name=code" pairs: 0 means the service is reachable from this process.
// Asking is free of side effects, which is why it, and not a real event,
// probes the scripting and mount services.
const lookupScript = `function run(argv) {
  ObjC.bindFunction('task_self_trap', ['unsigned int', []]);
  ObjC.bindFunction('task_get_special_port', ['int', ['unsigned int', 'int', 'unsigned int *']]);
  ObjC.bindFunction('bootstrap_look_up', ['int', ['unsigned int', 'char *', 'unsigned int *']]);
  var bootstrap = Ref('unsigned int');
  var kr = $.task_get_special_port($.task_self_trap(), 4, bootstrap);
  if (kr !== 0) { return 'bootstrap=' + kr; }
  var out = [];
  for (var i = 0; i < argv.length; i++) {
    var port = Ref('unsigned int');
    out.push(argv[i] + '=' + $.bootstrap_look_up(bootstrap[0], argv[i], port));
  }
  return out.join(' ');
}`

func parseLookups(output string) map[string]int {
	lookups := map[string]int{}
	for _, field := range strings.Fields(output) {
		name, code, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		value, err := strconv.Atoi(code)
		if err != nil {
			continue
		}
		lookups[name] = value
	}
	return lookups
}

// probeResultPath is where the probe writes its results: inside the session
// temporary directory, so a profile that denies the writable set leaves no
// results and every positive control fails.
func probeResultPath(sessionTmp string) string {
	return filepath.Join(sessionTmp, "probe-result.json")
}
