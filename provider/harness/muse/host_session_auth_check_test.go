package muse

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

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
	if err := CheckHostSessionLogin(nil, "/usr/bin/muse"); !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Fatalf("nil context accepted: %v", err)
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
	if err := os.MkdirAll(localBin, 0o755); err != nil {
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
