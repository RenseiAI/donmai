//go:build darwin

package confinement

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// sandboxExec is the macOS profile launcher. It is always run by absolute
// path: a lookup on PATH could find a planted no-op.
const sandboxExec = "/usr/bin/sandbox-exec"

// resolverSocket is the macOS resolver's socket. Name resolution fails
// without it, so a harness that needs the network declares it.
const resolverSocket = "/private/var/run/mDNSResponder"

// DefaultBackend returns the confinement backend for the running OS: the
// macOS profile backend here.
func DefaultBackend() Backend { return &seatbeltBackend{exe: sandboxExec} }

// ResolverSockets returns the OS resolver sockets a harness that needs name
// resolution declares in Spec.Sockets.
func ResolverSockets() []string { return []string{resolverSocket} }

type seatbeltBackend struct {
	exe string
}

func (s *seatbeltBackend) Name() BackendName { return BackendMacOSSeatbelt }

func (s *seatbeltBackend) Version() (string, error) {
	build, err := osBuild()
	if err != nil {
		return "", err
	}
	return seatbeltProfileVersion + "+darwin-" + build, nil
}

// Check refuses with nested_sandbox when this process is already inside a
// profile — whether or not the outer profile would let a second one apply,
// the harness must never run under an outer profile while the inner level is
// reported (D4.2) — and with backend_absent when the launcher is missing or
// cannot apply a trivial profile.
func (s *seatbeltBackend) Check() error {
	info, err := os.Stat(s.exe)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return refuse(ReasonBackendAbsent, "the macOS profile launcher is missing")
	}
	sandboxed, err := alreadySandboxed()
	if err != nil {
		return refuse(ReasonBackendAbsent, "cannot tell whether this process is already confined: %v", err)
	}
	if sandboxed {
		return refuse(ReasonNestedSandbox, "this process is already inside a sandbox profile")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, s.exe, "-p", "(version 1)(allow default)", "/usr/bin/true").CombinedOutput() //nolint:gosec // G204: fixed absolute launcher and arguments.
	if err == nil {
		return nil
	}
	if bytes.Contains(out, []byte("sandbox_apply")) {
		return refuse(ReasonNestedSandbox, "this process is already inside a sandbox profile")
	}
	return refuse(ReasonBackendAbsent, "the macOS profile launcher failed: %s", strings.TrimSpace(string(out)))
}

// sandboxCheckScript asks the kernel whether the process running it is under
// a profile. A child inherits its parent's profile, so the answer for the
// child is the answer for this process.
const sandboxCheckScript = `ObjC.bindFunction("sandbox_check", ["int", ["int", "void *", "int"]]);
ObjC.bindFunction("getpid", ["int", []]);
"sandboxed=" + $.sandbox_check($.getpid(), null, 0)`

var (
	sandboxedOnce  sync.Once
	sandboxedValue bool
	sandboxedErr   error
)

// alreadySandboxed reports whether this process is inside a profile. The
// answer cannot change for the life of the process — it never applies a
// profile to itself — so it is asked once.
func alreadySandboxed() (bool, error) {
	sandboxedOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "/usr/bin/osascript", "-l", "JavaScript", "-e", sandboxCheckScript).Output() //nolint:gosec // G204: fixed tool and script.
		if err != nil {
			sandboxedErr = fmt.Errorf("sandbox check: %w", err)
			return
		}
		switch strings.TrimSpace(string(out)) {
		case "sandboxed=0":
			sandboxedValue = false
		case "sandboxed=1":
			sandboxedValue = true
		default:
			sandboxedErr = fmt.Errorf("sandbox check: unexpected answer %q", strings.TrimSpace(string(out)))
		}
	})
	return sandboxedValue, sandboxedErr
}

// Canonical resolves symbolic links and folds the data-volume firmlink, so a
// path is spelled the way the kernel reports it to the profile.
func (s *seatbeltBackend) Canonical(path string) (string, error) {
	return canonicalDarwin(path)
}

func canonicalDarwin(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	const dataVolume = "/System/Volumes/Data"
	if strings.HasPrefix(resolved, dataVolume+"/") {
		resolved = strings.TrimPrefix(resolved, dataVolume)
	}
	return resolved, nil
}

func (s *seatbeltBackend) Apply(req ApplyRequest) (Applied, error) {
	shared, err := sharedLocations()
	if err != nil {
		return Applied{}, refuse(ReasonBackendAbsent, "shared locations: %v", err)
	}
	text, err := renderSeatbelt(req.Resolved, shared, req.Rules, s.Canonical)
	if err != nil {
		return Applied{}, err
	}
	path, err := writeProfile(req.ProfileDir, profileName(req.Resolved), []byte(text))
	if err != nil {
		return Applied{}, err
	}
	exe := s.exe
	return Applied{
		Rendered: []byte(text),
		Wrap: func(argv []string) []string {
			return append([]string{exe, "-f", path, "--"}, argv...)
		},
		Release: func() error {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("confinement: remove profile: %w", err)
			}
			return nil
		},
	}, nil
}

var (
	osBuildOnce  sync.Once
	osBuildValue string
	osBuildErr   error
)

func osBuild() (string, error) {
	osBuildOnce.Do(func() {
		osBuildValue, osBuildErr = unix.Sysctl("kern.osversion")
		if osBuildErr == nil && osBuildValue == "" {
			osBuildErr = errors.New("empty OS build")
		}
	})
	return osBuildValue, osBuildErr
}

var (
	sharedOnce  sync.Once
	sharedValue []string
	sharedErr   error
)

// sharedLocations are the shared temporary and cache locations the profile
// names (D2.1): /tmp, /var/tmp and the per-user temporary and cache
// directories the OS assigns.
func sharedLocations() ([]string, error) {
	sharedOnce.Do(func() {
		locations := []string{"/private/tmp", "/private/var/tmp"}
		for _, name := range []string{"DARWIN_USER_TEMP_DIR", "DARWIN_USER_CACHE_DIR"} {
			out, err := exec.Command("/usr/bin/getconf", name).Output() //nolint:gosec // G204: fixed absolute tool, fixed names.
			if err != nil {
				sharedErr = fmt.Errorf("getconf %s: %w", name, err)
				return
			}
			dir, err := canonicalDarwin(strings.TrimSpace(string(out)))
			if err != nil {
				sharedErr = fmt.Errorf("resolve %s: %w", name, err)
				return
			}
			locations = append(locations, dir)
		}
		sharedValue = locations
	})
	return append([]string(nil), sharedValue...), sharedErr
}

// profileName is a profile file name for one rendering: the harness and
// session ids reduced to a safe alphabet plus a random suffix, so two
// renderings never share a file.
func profileName(r *Resolved) string {
	return safeName(r.HarnessID) + "-" + safeName(r.SessionID) + "-" + randomSuffix() + ".sb"
}

// writeProfile writes a profile atomically with owner-only permissions.
func writeProfile(dir, name string, text []byte) (string, error) {
	tmp, err := os.CreateTemp(dir, ".profile-*")
	if err != nil {
		return "", fmt.Errorf("confinement: write profile: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(text); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("confinement: write profile: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("confinement: write profile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("confinement: write profile: %w", err)
	}
	path := filepath.Join(dir, name)
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("confinement: write profile: %w", err)
	}
	return path, nil
}
