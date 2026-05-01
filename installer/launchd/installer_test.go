package launchd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRunner records calls and returns canned responses.
type fakeRunner struct {
	responses map[string]struct {
		out []byte
		err error
	}
	calls []string
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{responses: map[string]struct {
		out []byte
		err error
	}{}}
}

func (f *fakeRunner) set(key string, out string, err error) {
	f.responses[key] = struct {
		out []byte
		err error
	}{[]byte(out), err}
}

func (f *fakeRunner) Run(name string, args ...string) ([]byte, error) {
	key := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, key)
	if r, ok := f.responses[key]; ok {
		return r.out, r.err
	}
	return nil, nil
}

func TestGeneratePlist_RegistersDaemonRunNotRenseiDaemon(t *testing.T) {
	out, err := GeneratePlist("/usr/local/bin/af", "/tmp/o.log", "/tmp/e.log")
	if err != nil {
		t.Fatalf("GeneratePlist: %v", err)
	}

	// Locked REN-1406 decision: ProgramArguments must register the host
	// binary's `daemon run` subcommand (NOT a separate rensei-daemon
	// binary, NOT the legacy `start` subcommand).
	if !strings.Contains(out, "<string>/usr/local/bin/af</string>") {
		t.Errorf("expected host binary path in ProgramArguments, got:\n%s", out)
	}
	if !strings.Contains(out, "<string>daemon</string>") || !strings.Contains(out, "<string>run</string>") {
		t.Errorf("expected `daemon run` subcommand in ProgramArguments, got:\n%s", out)
	}
	if strings.Contains(out, "<string>start</string>") {
		t.Errorf("plist must NOT register the legacy `start` subcommand, got:\n%s", out)
	}
	if strings.Contains(out, "rensei-daemon") {
		t.Errorf("plist must NOT register the legacy rensei-daemon binary, got:\n%s", out)
	}
}

func TestGeneratePlist_EncodesKeyBehaviours(t *testing.T) {
	out, err := GeneratePlist("/opt/af", "/var/log/o.log", "/var/log/e.log")
	if err != nil {
		t.Fatalf("GeneratePlist: %v", err)
	}

	for _, want := range []string{
		"<key>Label</key>",
		"<string>" + LaunchdLabel + "</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
		"<key>ThrottleInterval</key>",
		"<integer>30</integer>",
		"<key>StandardOutPath</key>",
		"<string>/var/log/o.log</string>",
		"<key>StandardErrorPath</key>",
		"<string>/var/log/e.log</string>",
		"<key>EnvironmentVariables</key>",
		"<key>HOME</key>",
		"<key>PATH</key>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected plist to contain %q", want)
		}
	}
}

func TestGeneratePlist_EscapesXMLSpecials(t *testing.T) {
	out, err := GeneratePlist("/usr/local/bin/af<weird>&path", "/tmp/o.log", "/tmp/e.log")
	if err != nil {
		t.Fatalf("GeneratePlist: %v", err)
	}
	if !strings.Contains(out, "&lt;weird&gt;&amp;path") {
		t.Errorf("expected XML-special characters to be escaped in plist")
	}
}

func TestGeneratePlist_RequiresHostBinPath(t *testing.T) {
	if _, err := GeneratePlist("", "/tmp/o.log", "/tmp/e.log"); err == nil {
		t.Errorf("expected error when hostBinPath is empty")
	}
}

func TestInstall_WritesPlistToTempPath(t *testing.T) {
	tmp := t.TempDir()
	plistPath := filepath.Join(tmp, "test.plist")
	logPath := filepath.Join(tmp, "logs", "daemon.log")
	errLogPath := filepath.Join(tmp, "logs", "daemon-error.log")

	fr := newFakeRunner()
	res, err := Install(InstallOptions{
		HostBinPath:  "/usr/local/bin/af",
		PlistPath:    plistPath,
		LogPath:      logPath,
		ErrorLogPath: errLogPath,
		Runner:       fr,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.PlistPath != plistPath {
		t.Errorf("expected plist path %s, got %s", plistPath, res.PlistPath)
	}
	if res.HostBinPath != "/usr/local/bin/af" {
		t.Errorf("expected host bin /usr/local/bin/af, got %s", res.HostBinPath)
	}
	if !res.Loaded {
		t.Errorf("expected Loaded=true after successful bootstrap")
	}

	content, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("read plist: %v", err)
	}
	if !strings.Contains(string(content), "<string>/usr/local/bin/af</string>") {
		t.Errorf("plist must contain host binary path; got:\n%s", content)
	}

	// launchctl bootstrap should have been called.
	joined := strings.Join(fr.calls, "\n")
	if !strings.Contains(joined, "launchctl bootstrap") {
		t.Errorf("expected launchctl bootstrap call, got: %v", fr.calls)
	}
}

func TestInstall_SkipLaunchctl(t *testing.T) {
	tmp := t.TempDir()
	plistPath := filepath.Join(tmp, "test.plist")

	fr := newFakeRunner()
	res, err := Install(InstallOptions{
		HostBinPath:   "/usr/local/bin/af",
		PlistPath:     plistPath,
		LogPath:       filepath.Join(tmp, "o.log"),
		ErrorLogPath:  filepath.Join(tmp, "e.log"),
		SkipLaunchctl: true,
		Runner:        fr,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.Loaded {
		t.Errorf("expected Loaded=false when SkipLaunchctl=true")
	}
	if len(fr.calls) != 0 {
		t.Errorf("expected no launchctl calls, got %v", fr.calls)
	}
}

func TestInstall_AlreadyLoadedNotError(t *testing.T) {
	tmp := t.TempDir()
	plistPath := filepath.Join(tmp, "test.plist")

	fr := newFakeRunner()
	// Simulate launchctl returning "already loaded".
	fr.set("launchctl bootstrap gui/"+itoa(os.Getuid())+" "+plistPath,
		"service already bootstrapped: dev.rensei.daemon",
		errors.New("exit status 17"))

	res, err := Install(InstallOptions{
		HostBinPath:  "/usr/local/bin/af",
		PlistPath:    plistPath,
		LogPath:      filepath.Join(tmp, "o.log"),
		ErrorLogPath: filepath.Join(tmp, "e.log"),
		Runner:       fr,
	})
	if err != nil {
		t.Fatalf("Install must treat 'already' as benign success, got: %v", err)
	}
	if !res.Loaded {
		t.Errorf("expected Loaded=true on benign 'already' response")
	}
}

func TestInstall_PropagatesBootstrapError(t *testing.T) {
	tmp := t.TempDir()
	plistPath := filepath.Join(tmp, "test.plist")

	fr := newFakeRunner()
	fr.set("launchctl bootstrap gui/"+itoa(os.Getuid())+" "+plistPath,
		"some other failure",
		errors.New("exit status 1"))

	if _, err := Install(InstallOptions{
		HostBinPath:  "/usr/local/bin/af",
		PlistPath:    plistPath,
		LogPath:      filepath.Join(tmp, "o.log"),
		ErrorLogPath: filepath.Join(tmp, "e.log"),
		Runner:       fr,
	}); err == nil {
		t.Errorf("expected error when bootstrap fails for non-already reasons")
	}
}

func TestUninstall_RemovesPlist(t *testing.T) {
	tmp := t.TempDir()
	plistPath := filepath.Join(tmp, "test.plist")
	if err := os.WriteFile(plistPath, []byte("<plist/>"), 0o600); err != nil {
		t.Fatalf("seed plist: %v", err)
	}

	fr := newFakeRunner()
	removed, err := Uninstall(UninstallOptions{
		PlistPath: plistPath,
		Runner:    fr,
	})
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !removed {
		t.Errorf("expected removed=true")
	}
	if _, err := os.Stat(plistPath); !os.IsNotExist(err) {
		t.Errorf("expected plist removed; stat err=%v", err)
	}
	if !strings.Contains(strings.Join(fr.calls, "\n"), "launchctl bootout") {
		t.Errorf("expected launchctl bootout call, got %v", fr.calls)
	}
}

func TestUninstall_PlistNotPresent(t *testing.T) {
	tmp := t.TempDir()
	plistPath := filepath.Join(tmp, "missing.plist")

	removed, err := Uninstall(UninstallOptions{PlistPath: plistPath, SkipLaunchctl: true})
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if removed {
		t.Errorf("expected removed=false on missing plist")
	}
}

func TestDoctor_PlistMissing(t *testing.T) {
	tmp := t.TempDir()
	plistPath := filepath.Join(tmp, "missing.plist")

	res, err := Doctor(DoctorOptions{
		PlistPath:     plistPath,
		LaunchctlList: func() (string, error) { return "", nil },
	})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if res.Healthy {
		t.Errorf("expected Healthy=false when plist missing")
	}
	if got := findCheck(res, "plist-exists"); got == nil || got.Passed {
		t.Errorf("expected plist-exists check failed")
	}
}

func TestDoctor_AllChecksPass(t *testing.T) {
	tmp := t.TempDir()
	plistPath := filepath.Join(tmp, "test.plist")
	if err := os.WriteFile(plistPath, []byte("<plist/>"), 0o600); err != nil {
		t.Fatalf("seed plist: %v", err)
	}

	res, err := Doctor(DoctorOptions{
		PlistPath: plistPath,
		LaunchctlList: func() (string, error) {
			return `{
	"PID" = 12345;
	"Label" = "` + LaunchdLabel + `";
}`, nil
		},
	})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if !res.Healthy {
		t.Errorf("expected Healthy=true; checks=%+v", res.Checks)
	}
	for _, c := range res.Checks {
		if !c.Passed {
			t.Errorf("expected check %q to pass; detail=%s", c.Name, c.Detail)
		}
	}
}

func TestDoctor_LoadedButNotRunning(t *testing.T) {
	tmp := t.TempDir()
	plistPath := filepath.Join(tmp, "test.plist")
	if err := os.WriteFile(plistPath, []byte("<plist/>"), 0o600); err != nil {
		t.Fatalf("seed plist: %v", err)
	}

	res, err := Doctor(DoctorOptions{
		PlistPath: plistPath,
		LaunchctlList: func() (string, error) {
			return `{
	"PID" = 0;
	"Label" = "` + LaunchdLabel + `";
}`, nil
		},
	})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if res.Healthy {
		t.Errorf("expected Healthy=false when PID=0")
	}
	if got := findCheck(res, "daemon-running"); got == nil || got.Passed {
		t.Errorf("expected daemon-running check failed")
	}
}

func TestResolveHostBinPath_ExplicitOverride(t *testing.T) {
	got, err := ResolveHostBinPath("/explicit/af")
	if err != nil {
		t.Fatalf("ResolveHostBinPath: %v", err)
	}
	if got != "/explicit/af" {
		t.Errorf("expected explicit path, got %s", got)
	}
}

// TestGeneratePlist_PathIncludesUserLocalBin asserts the v0.5.1 fix:
// the plist's EnvironmentVariables.PATH must prepend ~/.local/bin so
// user-local installs of provider CLIs like `claude` are visible to
// the daemon. (REN-1462.)
func TestGeneratePlist_PathIncludesUserLocalBin(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	out, err := GeneratePlist("/usr/local/bin/af", "/tmp/o.log", "/tmp/e.log")
	if err != nil {
		t.Fatalf("GeneratePlist: %v", err)
	}

	wantSegment := filepath.Join(tmp, ".local", "bin")
	if !strings.Contains(out, wantSegment) {
		t.Errorf("plist PATH missing %q\n--- plist ---\n%s", wantSegment, out)
	}
	// Order matters: ~/.local/bin must come first so a user-scope
	// install wins over a stale system-scope copy.
	pathLineIdx := strings.Index(out, "<key>PATH</key>")
	if pathLineIdx < 0 {
		t.Fatalf("plist missing <key>PATH</key>\n%s", out)
	}
	rest := out[pathLineIdx:]
	stringStart := strings.Index(rest, "<string>")
	stringEnd := strings.Index(rest, "</string>")
	if stringStart < 0 || stringEnd < 0 || stringStart >= stringEnd {
		t.Fatalf("plist PATH value missing or malformed:\n%s", rest[:200])
	}
	pathVal := rest[stringStart+len("<string>") : stringEnd]
	parts := strings.Split(pathVal, ":")
	if len(parts) == 0 || parts[0] != wantSegment {
		t.Errorf("first PATH segment = %q, want %q (full=%q)", parts[0], wantSegment, pathVal)
	}
}

func TestPlistPath_HomeDependence(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	got, err := PlistPath()
	if err != nil {
		t.Fatalf("PlistPath: %v", err)
	}
	want := filepath.Join(tmp, "Library", "LaunchAgents", LaunchdLabel+".plist")
	if got != want {
		t.Errorf("expected %s, got %s", want, got)
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

func findCheck(res DoctorResult, name string) *DoctorCheck {
	for i := range res.Checks {
		if res.Checks[i].Name == name {
			return &res.Checks[i]
		}
	}
	return nil
}

// itoa is a small local helper to avoid importing strconv in tests just for
// formatting Getuid().
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
