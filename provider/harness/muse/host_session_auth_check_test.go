package muse

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// writeFakeMuseCLI writes a per-test stand-in host binary and returns
// its absolute path. The script answers `--version` with the given
// output line (or exits non-zero when failVersion is set).
func writeFakeMuseCLI(t *testing.T, name, versionLine string, failVersion bool) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake muse CLI uses /bin/sh; skip on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	var script string
	if failVersion {
		script = "#!/bin/sh\nexit 3\n"
	} else {
		script = "#!/bin/sh\n" +
			"if [ \"$1\" = \"--version\" ]; then printf '%s\\n' '" + versionLine + "'; exit 0; fi\n" +
			"exit 3\n"
	}
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // test fixture script
		t.Fatalf("write fake muse CLI: %v", err)
	}
	return path
}

func TestCheckHostSessionLogin_SupportedVersion(t *testing.T) {
	t.Parallel()
	bin := writeFakeMuseCLI(t, "muse", "Muse Code 1.4.9 (1.4.9-R6297.1)", false)
	if err := CheckHostSessionLogin(context.Background(), bin); err != nil {
		t.Fatalf("supported CLI refused: %v", err)
	}
}

func TestCheckHostSessionLogin_MinimumVersion(t *testing.T) {
	t.Parallel()
	bin := writeFakeMuseCLI(t, "muse", "Muse Code 1.4.6 (1.4.6-R6000.1)", false)
	if err := CheckHostSessionLogin(context.Background(), bin); err != nil {
		t.Fatalf("minimum CLI refused: %v", err)
	}
}

func TestCheckHostSessionLogin_TooOld(t *testing.T) {
	t.Parallel()
	bin := writeFakeMuseCLI(t, "muse", "Muse Code 1.4.2 (1.4.2-R5000.1)", false)
	if err := CheckHostSessionLogin(context.Background(), bin); !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Fatalf("too-old CLI accepted: %v", err)
	}
}

func TestCheckHostSessionLogin_NonZeroExit(t *testing.T) {
	t.Parallel()
	bin := writeFakeMuseCLI(t, "muse", "", true)
	if err := CheckHostSessionLogin(context.Background(), bin); !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Fatalf("failing CLI accepted: %v", err)
	}
}

// TestCheckHostSessionLogin_ValidVersionThenNonZeroExit pins the
// fail-closed non-zero-exit guard: a CLI that prints a supported
// version and THEN exits non-zero is still refused. Deleting the
// `waitErr != nil` refusal would accept it, because the version
// line alone parses fine.
func TestCheckHostSessionLogin_ValidVersionThenNonZeroExit(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("fake muse CLI uses /bin/sh; skip on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "muse")
	script := "#!/bin/sh\nprintf 'Muse Code 1.4.9 (1.4.9-R6297.1)\\n'\nexit 2\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // test fixture script
		t.Fatalf("write fake muse CLI: %v", err)
	}
	if err := CheckHostSessionLogin(context.Background(), path); !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Fatalf("valid-version-then-failing CLI accepted: %v", err)
	}
}

func TestCheckHostSessionLogin_UnparseableVersion(t *testing.T) {
	t.Parallel()
	bin := writeFakeMuseCLI(t, "muse", "muse: no version here", false)
	if err := CheckHostSessionLogin(context.Background(), bin); !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Fatalf("unparseable CLI accepted: %v", err)
	}
}

func TestCheckHostSessionLogin_RequiresAbsoluteBinary(t *testing.T) {
	t.Parallel()
	if err := CheckHostSessionLogin(context.Background(), "muse"); !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Fatalf("relative binary accepted: %v", err)
	}
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if err := CheckHostSessionLogin(cancelled, "/usr/bin/muse"); !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Fatalf("cancelled context accepted: %v", err)
	}
}

// TestCheckHostSessionLogin_RelativeBinaryOnPATHRefused pins the
// absolute-path guard against a relative name that DOES resolve: a
// fake `muse` on PATH must still be refused, because the caller's
// PATH and the probe's minimal PATH=/usr/bin:/bin could disagree
// about which binary answers. Without the guard this test's fake
// would run and its version would be accepted.
func TestCheckHostSessionLogin_RelativeBinaryOnPATHRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake muse CLI uses /bin/sh; skip on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "muse")
	script := "#!/bin/sh\nprintf 'Muse Code 1.4.9 (1.4.9-R6297.1)\\n'\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // test fixture script
		t.Fatalf("write fake muse CLI: %v", err)
	}
	t.Setenv("PATH", dir)
	if err := CheckHostSessionLogin(context.Background(), "muse"); !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Fatalf("resolving relative binary accepted: %v", err)
	}
}

func TestCheckHostSessionLogin_MissingBinary(t *testing.T) {
	t.Parallel()
	bin := filepath.Join(t.TempDir(), "muse-missing")
	if err := CheckHostSessionLogin(context.Background(), bin); !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Fatalf("missing binary accepted: %v", err)
	}
}

func TestCheckHostSessionLogin_MinimalEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, name := range []string{"META_API_KEY", "MUSE_BIN", "DONMAI_DAEMON_JWT", "HTTP_PROXY"} {
		t.Setenv(name, "synthetic-must-not-inherit")
	}
	script := "[ \"$1\" = \"--version\" ] || exit 21\n" +
		"[ -z \"${META_API_KEY+x}\" ] || exit 22\n" +
		"[ -z \"${DONMAI_DAEMON_JWT+x}\" ] || exit 23\n" +
		"[ -z \"${HTTP_PROXY+x}\" ] || exit 24\n" +
		"[ \"$MUSE_NO_AUTO_UPDATE\" = \"1\" ] || exit 25\n" +
		"printf 'Muse Code 1.4.9 (1.4.9-R6297.1)\\n'\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "muse")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o700); err != nil { //nolint:gosec // test fixture script
		t.Fatalf("write fake muse CLI: %v", err)
	}
	if err := CheckHostSessionLogin(context.Background(), path); err != nil {
		t.Fatalf("minimal-env probe refused: %v", err)
	}
}

func TestCheckHostSessionLogin_AgreesWithConstructor(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"Muse Code 1.4.9 (1.4.9-R6297.1)", "Muse Code 1.4.2 (1.4.2-R5000.1)"} {
		bin := writeFakeMuseCLI(t, "muse", version, false)
		checkErr := CheckHostSessionLogin(context.Background(), bin)
		_, newErr := New(Options{MuseBin: bin})
		if (checkErr == nil) != (newErr == nil) {
			t.Fatalf("version %q: check err=%v, New err=%v, want agreement", version, checkErr, newErr)
		}
	}
}

// TestResolveMuseBinary_Chain pins the resolver order: an explicit
// option wins, then $MUSE_BIN, then PATH, then the installer's
// default location. PATH and HOME are scrubbed so the host's own
// install cannot leak into the test.
func TestResolveMuseBinary_Chain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake muse CLI uses /bin/sh; skip on windows")
	}
	explicit := writeFakeMuseCLI(t, "explicit-muse", "Muse Code 1.4.9", false)
	envBin := writeFakeMuseCLI(t, "env-muse", "Muse Code 1.4.9", false)
	pathDir := t.TempDir()
	pathBin := filepath.Join(pathDir, "muse")
	if err := os.WriteFile(pathBin, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { //nolint:gosec // test fixture script
		t.Fatalf("write PATH muse: %v", err)
	}
	home := t.TempDir()
	localBin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(localBin, 0o750); err != nil {
		t.Fatalf("make local bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localBin, "muse"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { //nolint:gosec // test fixture script
		t.Fatalf("write home muse: %v", err)
	}

	// Scrub the host environment: neither the host's PATH nor its
	// real install may answer the resolver.
	t.Setenv("MUSE_BIN", envBin)
	t.Setenv("PATH", pathDir)
	t.Setenv("HOME", home)
	// Explicit option wins over everything.
	if got, err := ResolveMuseBinary(explicit); err != nil || got != explicit {
		t.Fatalf("explicit = %q, %v, want %q", got, err, explicit)
	}
	// $MUSE_BIN wins over PATH.
	if got, err := ResolveMuseBinary(""); err != nil || got != envBin {
		t.Fatalf("$MUSE_BIN = %q, %v, want %q", got, err, envBin)
	}
	// PATH wins over the installer default.
	t.Setenv("MUSE_BIN", "")
	if got, err := ResolveMuseBinary(""); err != nil || got != pathBin {
		t.Fatalf("PATH = %q, %v, want %q", got, err, pathBin)
	}
	// The installer default is the fallback when PATH lacks muse.
	t.Setenv("PATH", t.TempDir())
	wantHome := filepath.Join(home, ".local", "bin", "muse")
	if got, err := ResolveMuseBinary(""); err != nil || got != wantHome {
		t.Fatalf("home fallback = %q, %v, want %q", got, err, wantHome)
	}
	// An explicit unknown name never redirects to the home fallback.
	if _, err := ResolveMuseBinary("this-binary-does-not-exist-muse-12345"); err == nil {
		t.Fatal("unknown explicit name resolved, want an error")
	}
	// Nothing anywhere is an error the constructor maps to unavailable.
	t.Setenv("HOME", t.TempDir())
	if _, err := ResolveMuseBinary(""); err == nil {
		t.Fatal("missing binary resolved, want an error")
	}
}

func TestResolveMuseBinary_ConstructorSharesResolver(t *testing.T) {
	bin := writeFakeMuseCLI(t, "muse", "Muse Code 1.4.9 (1.4.9-R6297.1)", false)
	t.Setenv("MUSE_BIN", bin)
	t.Setenv("PATH", t.TempDir())
	got, err := ResolveMuseBinary("")
	if err != nil || got != bin {
		t.Fatalf("ResolveMuseBinary = %q, %v, want %q", got, err, bin)
	}
	if _, err := New(Options{}); err != nil {
		t.Fatalf("New with the resolved binary refused: %v", err)
	}
	if err := CheckHostSessionLogin(context.Background(), got); err != nil {
		t.Fatalf("check with the resolved binary refused: %v", err)
	}
}

func TestCheckHostSessionLogin_StderrDiagnosticsRefused(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("fake muse CLI uses /bin/sh; skip on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "muse")
	script := "#!/bin/sh\nprintf 'Muse Code 1.4.9 (1.4.9-R6297.1)\\n'\nprintf 'warning: noisy\\n' >&2\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // test fixture script
		t.Fatalf("write fake muse CLI: %v", err)
	}
	if err := CheckHostSessionLogin(context.Background(), path); !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Fatalf("noisy CLI accepted: %v", err)
	}
}

// heartbeatFakeMuseCLI writes a fake whose background loop keeps
// appending ticks to a heartbeat file for as long as it lives. The
// path derives from the script's own directory, because the probe
// runs `<binary> --version` under a minimal env that carries no
// test state. Requiring ticks before the verdict proves the helper
// was alive during the probe; requiring silence after proves the
// probe's group kill reaped it.
func heartbeatFakeMuseCLI(t *testing.T, body string) (bin, heartbeat string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake muse CLI uses /bin/sh; skip on windows")
	}
	dir := t.TempDir()
	bin = filepath.Join(dir, "muse")
	heartbeat = filepath.Join(dir, "heartbeat.log")
	script := "#!/bin/sh\nHB='" + heartbeat + "'\n" + body
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { //nolint:gosec // test fixture script
		t.Fatalf("write fake muse CLI: %v", err)
	}
	return bin, heartbeat
}

func heartbeatTicks(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // test temp path
	if err != nil {
		t.Fatalf("read heartbeat: %v", err)
	}
	return len(raw)
}

// assertHeartbeatStopped requires the helper to have been alive
// during the probe (ticks > 0) and silent afterwards: two samples
// past the kill-delivery window must agree.
func assertHeartbeatStopped(t *testing.T, path string) {
	t.Helper()
	time.Sleep(time.Second)
	before := heartbeatTicks(t, path)
	if before == 0 {
		t.Fatal("helper never ticked during the probe; the leak assertion is vacuous")
	}
	time.Sleep(800 * time.Millisecond)
	if after := heartbeatTicks(t, path); after != before {
		t.Fatalf("helper ticked after the probe returned (%d -> %d bytes): the probe leaked its process group", before, after)
	}
}

// TestCheckHostSessionLogin_HangingCLIRefusedWithinTimeout pins the
// 5 s bound with the production default: a CLI that hangs (and
// ignores TERM) is refused at about the timeout, and its forked
// helper dies with the probe's process group instead of leaking.
// Raising the timeout lets the check stay blocked past the
// advertised bound; deleting the group kill leaves the helper
// ticking after the return.
func TestCheckHostSessionLogin_HangingCLIRefusedWithinTimeout(t *testing.T) {
	t.Parallel()
	// One synchronous tick before forking the helper: under load the first
	// asynchronous tick can land after the probe's group kill, and the file
	// would not exist yet (same race as the detached-child case).
	bin, heartbeat := heartbeatFakeMuseCLI(t,
		"echo tick >> \"$HB\"\n"+
			"( while true; do echo tick >> \"$HB\"; sleep 0.2; done ) &\n"+
			"trap '' TERM\n"+
			"sleep 300\n")
	done := make(chan error, 1)
	go func() { done <- CheckHostSessionLogin(context.Background(), bin) }()
	select {
	case err := <-done:
		if !errors.Is(err, agent.ErrProviderUnavailable) {
			t.Fatalf("hanging CLI accepted: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("hanging CLI blocked past the 5 s probe bound")
	}
	assertHeartbeatStopped(t, heartbeat)
}

// TestCheckHostSessionLogin_DetachedChildKeepingStdoutOpenRefused
// pins the WaitDelay path: the leader prints a valid version and
// exits at once, but its child keeps the stdout pipe open. The
// check still refuses (fast, not at the probe timeout), and the
// group kill reaps the pipe-holder.
func TestCheckHostSessionLogin_DetachedChildKeepingStdoutOpenRefused(t *testing.T) {
	t.Parallel()
	bin, heartbeat := heartbeatFakeMuseCLI(t,
		"if [ \"$1\" = \"--version\" ]; then\n"+
			"printf 'Muse Code 1.4.9 (1.4.9-R6297.1)\\n'\n"+
			// The first tick is synchronous: the probe can kill the group
			// before the detached helper is scheduled, and a heartbeat file
			// that never appeared is a race, not a leak.
			"echo tick >> \"$HB\"\n"+
			"( while true; do echo tick >> \"$HB\"; sleep 0.2; done ) &\n"+
			"exit 0\n"+
			"fi\n"+
			"exit 3\n")
	start := time.Now()
	err := CheckHostSessionLogin(context.Background(), bin)
	elapsed := time.Since(start)
	if !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Fatalf("detached-child CLI accepted: %v", err)
	}
	if elapsed >= hostSessionLoginTimeout {
		t.Fatalf("detached-child refusal took %v, want well under the %v probe bound (the WaitDelay path)", elapsed, hostSessionLoginTimeout)
	}
	assertHeartbeatStopped(t, heartbeat)
}
