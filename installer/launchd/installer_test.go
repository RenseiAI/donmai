package launchd

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/daemon"
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

func TestGeneratePlist_RegistersHostRunNotSeparateDaemonBinary(t *testing.T) {
	out, err := GeneratePlist("/usr/local/bin/af", "/tmp/o.log", "/tmp/e.log")
	if err != nil {
		t.Fatalf("GeneratePlist: %v", err)
	}

	// Locked decision: ProgramArguments must register a subcommand of the
	// host binary itself (NOT a separate daemon binary, NOT the legacy
	// `start` subcommand). The noun moved from `daemon run` to `host run`
	// when `daemon` became a deprecated alias of `host`.
	if !strings.Contains(out, "<string>/usr/local/bin/af</string>") {
		t.Errorf("expected host binary path in ProgramArguments, got:\n%s", out)
	}
	if !strings.Contains(out, "<string>host</string>") || !strings.Contains(out, "<string>run</string>") {
		t.Errorf("expected `host run` subcommand in ProgramArguments, got:\n%s", out)
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

// TestGeneratePlist_KeepAliveOnlyOnFailure pins the SuccessfulExit=false
// shape of the KeepAlive dict. The May-2026 incident saw the boolean form
// (`<key>KeepAlive</key><true/>`) respawn the daemon within 30s of every
// `rensei host stop`, because launchd treats the boolean true as "always
// restart on exit". The dict form below restarts only on a non-zero exit
// (crash recovery preserved) while leaving operator-initiated stops to
// stick. Asserts the dict markup specifically so a regression to the
// boolean form is caught by the test, not by an angry operator.
func TestGeneratePlist_KeepAliveOnlyOnFailure(t *testing.T) {
	out, err := GeneratePlist("/opt/af", "/tmp/o.log", "/tmp/e.log")
	if err != nil {
		t.Fatalf("GeneratePlist: %v", err)
	}

	// The KeepAlive value must be a dict carrying SuccessfulExit=false plus
	// the explicit Crashed=true crash-respawn half (durability keys), not
	// the bare <true/> boolean form. We match the rendered substring
	// including whitespace between the open tag and the dict body so a
	// future refactor that splits these across lines still passes.
	keepAliveDictRe := regexp.MustCompile(
		`<key>KeepAlive</key>\s*<dict>\s*<key>SuccessfulExit</key>\s*<false/>\s*<key>Crashed</key>\s*<true/>\s*</dict>`,
	)
	if !keepAliveDictRe.MatchString(out) {
		t.Errorf("expected KeepAlive dict with SuccessfulExit=false and Crashed=true, got:\n%s", out)
	}

	// Belt-and-suspenders: the bare boolean form (`<key>KeepAlive</key>\s*<true/>`)
	// must not be present anywhere in the plist.
	keepAliveTrueRe := regexp.MustCompile(`<key>KeepAlive</key>\s*<true/>`)
	if keepAliveTrueRe.MatchString(out) {
		t.Errorf("regressed to boolean KeepAlive=true (respawns on clean stop):\n%s", out)
	}
}

// TestGeneratePlist_DurabilityKeys pins the four durability keys that stop
// launchd from killing hosted sessions with the daemon job. Each row is a
// rendered-fragment guard: ExitTimeOut must cover the graceful drain (or
// launchd SIGKILLs the whole job process group mid-drain on every
// restart/upgrade/plist reload), AbandonProcessGroup orphans session
// children instead of killing them with the job, KeepAlive/Crashed makes
// crash respawn explicit, and LegacyTimers keeps heartbeat/token-refresh
// timers from being coalesced-deferred on battery.
func TestGeneratePlist_DurabilityKeys(t *testing.T) {
	out, err := GeneratePlist("/opt/af", "/tmp/o.log", "/tmp/e.log")
	if err != nil {
		t.Fatalf("GeneratePlist: %v", err)
	}

	tests := []struct {
		name string
		re   string
	}{
		{"ExitTimeOut covers drain plus margin", `<key>ExitTimeOut</key>\s*<integer>` + strconv.Itoa(ExitTimeOutSeconds) + `</integer>`},
		{"AbandonProcessGroup true", `<key>AbandonProcessGroup</key>\s*<true/>`},
		{"KeepAlive Crashed true", `<key>Crashed</key>\s*<true/>`},
		{"LegacyTimers true", `<key>LegacyTimers</key>\s*<true/>`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !regexp.MustCompile(tt.re).MatchString(out) {
				t.Errorf("plist missing durability fragment %q, got:\n%s", tt.re, out)
			}
		})
	}
}

// TestExitTimeOutCoversDrainDefault statically asserts the launchd exit
// window is at least the daemon's config-default graceful drain (plus
// margin), pinned against the real default rather than a mirrored literal —
// if the drain default grows past the plist's exit window, launchd would
// SIGKILL the job mid-drain again and this test fails first.
func TestExitTimeOutCoversDrainDefault(t *testing.T) {
	drain := daemon.DefaultConfig().AutoUpdate.DrainTimeoutSeconds
	if drain <= 0 {
		t.Fatalf("daemon config-default DrainTimeoutSeconds = %d, want > 0", drain)
	}
	if ExitTimeOutSeconds < drain+30 {
		t.Errorf("ExitTimeOutSeconds = %d, want >= config-default drain %d + 30s escalation margin",
			ExitTimeOutSeconds, drain)
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
		"service already bootstrapped: dev.donmai.daemon",
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

// retryRunner is a CommandRunner stub that returns different responses on
// successive `launchctl bootstrap` calls. Used by the exit-5 retry test
// where the first bootstrap must fail with the async-tear-down race
// signature and the second must succeed.
type retryRunner struct {
	bootstrapResponses []struct {
		out string
		err error
	}
	bootstrapCalls int
	calls          []string
}

func (r *retryRunner) Run(name string, args ...string) ([]byte, error) {
	key := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, key)
	if name == "launchctl" && len(args) > 0 && args[0] == "bootstrap" {
		i := r.bootstrapCalls
		r.bootstrapCalls++
		if i >= len(r.bootstrapResponses) {
			return nil, nil
		}
		resp := r.bootstrapResponses[i]
		return []byte(resp.out), resp.err
	}
	// All other commands (bootout) are no-ops.
	return nil, nil
}

// TestInstall_RetriesOnAsyncTeardownRace pins the bootstrap-retry path
// the May-2026 incident exposed: `rensei host install` after a binary
// upgrade saw `Bootstrap failed: 5: Input/output error` because the
// pre-bootout returned before launchd had finished tearing down the
// prior service. Without retry, operators saw a hard error and had to
// manually run `rensei host uninstall` then re-install. With retry,
// one 500ms settle + retry covers the race transparently.
func TestInstall_RetriesOnAsyncTeardownRace(t *testing.T) {
	tmp := t.TempDir()
	plistPath := filepath.Join(tmp, "test.plist")

	rr := &retryRunner{
		bootstrapResponses: []struct {
			out string
			err error
		}{
			{out: "Bootstrap failed: 5: Input/output error", err: errors.New("exit status 5")},
			{out: "", err: nil}, // retry succeeds
		},
	}

	res, err := Install(InstallOptions{
		HostBinPath:  "/usr/local/bin/af",
		PlistPath:    plistPath,
		LogPath:      filepath.Join(tmp, "o.log"),
		ErrorLogPath: filepath.Join(tmp, "e.log"),
		Runner:       rr,
	})
	if err != nil {
		t.Fatalf("Install must transparently retry on exit-5; got: %v", err)
	}
	if !res.Loaded {
		t.Errorf("expected Loaded=true after successful retry")
	}
	if rr.bootstrapCalls != 2 {
		t.Errorf("expected 2 bootstrap calls (1 fail + 1 retry), got %d", rr.bootstrapCalls)
	}
}

// TestInstall_RetriesOnceThenSurfacesError pins that the retry is
// bounded — a persistent exit-5 surfaces with an actionable hint
// (pointing at the manual `launchctl bootout` recovery), not a silent
// retry storm.
func TestInstall_RetriesOnceThenSurfacesError(t *testing.T) {
	tmp := t.TempDir()
	plistPath := filepath.Join(tmp, "test.plist")

	rr := &retryRunner{
		bootstrapResponses: []struct {
			out string
			err error
		}{
			{out: "Bootstrap failed: 5: Input/output error", err: errors.New("exit status 5")},
			{out: "Bootstrap failed: 5: Input/output error", err: errors.New("exit status 5")},
		},
	}

	_, err := Install(InstallOptions{
		HostBinPath:  "/usr/local/bin/af",
		PlistPath:    plistPath,
		LogPath:      filepath.Join(tmp, "o.log"),
		ErrorLogPath: filepath.Join(tmp, "e.log"),
		Runner:       rr,
	})
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if !strings.Contains(err.Error(), "launchctl bootout") {
		t.Errorf("expected actionable hint pointing at manual bootout; got: %v", err)
	}
	if rr.bootstrapCalls != 2 {
		t.Errorf("expected exactly 2 bootstrap calls (no infinite retry), got %d", rr.bootstrapCalls)
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
// the daemon.
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
