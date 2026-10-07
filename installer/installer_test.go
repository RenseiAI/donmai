package installer

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/installer/servicepriority"
)

func TestInstall_SkipServiceManager(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("installer only supports darwin/linux; this is %s", runtime.GOOS)
	}

	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	res, err := Install(InstallOptions{
		HostBinPath:        "/usr/local/bin/af",
		Scope:              ScopeUser,
		SkipServiceManager: true,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	if res.OS != runtime.GOOS {
		t.Errorf("expected OS=%s, got %s", runtime.GOOS, res.OS)
	}

	// ServiceCommand must register `host run` against the host binary —
	// the locked service-entrypoint decision (a subcommand of the host
	// binary, never a separate daemon binary). The noun moved from `daemon`
	// to `host` when `daemon` became a deprecated alias.
	want := "/usr/local/bin/af host run"
	if !strings.Contains(res.ServiceCommand, want) {
		t.Errorf("expected ServiceCommand to contain %q, got %q", want, res.ServiceCommand)
	}
	if strings.Contains(res.ServiceCommand, "rensei-daemon") {
		t.Errorf("ServiceCommand must NOT register a separate rensei-daemon binary, got %q", res.ServiceCommand)
	}
	if res.ServicePath == "" {
		t.Errorf("expected non-empty ServicePath")
	}
	if res.Loaded {
		t.Errorf("expected Loaded=false when SkipServiceManager=true")
	}
}

// backgroundMarkers returns the service-definition fragments that mark the
// background process-priority mode on this OS: launchd plist keys on macOS,
// systemd unit directives on Linux.
func backgroundMarkers(t *testing.T) []string {
	t.Helper()
	switch runtime.GOOS {
	case "darwin":
		return []string{
			"<key>ProcessType</key>", "<string>Background</string>",
			"<key>LowPriorityIO</key>", "<key>LowPriorityBackgroundIO</key>",
		}
	case "linux":
		return []string{"Nice=19\n", "CPUSchedulingPolicy=idle\n", "IOSchedulingClass=idle\n"}
	default:
		t.Skipf("installer only supports darwin/linux; this is %s", runtime.GOOS)
		return nil
	}
}

// installIsolated runs Install under a fresh HOME so neither the service
// definition nor the saved process-priority mode touches the real home.
func installIsolated(t *testing.T, priority ProcessPriority) (InstallResult, string) {
	t.Helper()
	res, err := Install(InstallOptions{
		HostBinPath:        "/usr/local/bin/af",
		ProcessPriority:    priority,
		Scope:              ScopeUser,
		SkipServiceManager: true,
	})
	if err != nil {
		t.Fatalf("Install(priority=%q): %v", priority, err)
	}
	content, err := os.ReadFile(res.ServicePath)
	if err != nil {
		t.Fatalf("read service definition %s: %v", res.ServicePath, err)
	}
	return res, string(content)
}

func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got := servicepriority.StatePath(); !strings.HasPrefix(got, home) {
		t.Fatalf("saved-mode path %q is outside the isolated HOME %q", got, home)
	}
	return home
}

func TestInstall_ProcessPriorityPerMode(t *testing.T) {
	markers := backgroundMarkers(t)
	cases := []struct {
		name       string
		priority   ProcessPriority
		wantMode   ProcessPriority
		wantSource ProcessPrioritySource
		wantMarked bool
	}{
		{name: "unset", priority: "", wantMode: ProcessPriorityDefault, wantSource: ProcessPrioritySourceDefault},
		{name: "default", priority: ProcessPriorityDefault, wantMode: ProcessPriorityDefault, wantSource: ProcessPrioritySourceExplicit},
		{name: "background", priority: ProcessPriorityBackground, wantMode: ProcessPriorityBackground, wantSource: ProcessPrioritySourceExplicit, wantMarked: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateHome(t)
			res, content := installIsolated(t, tc.priority)
			if res.ProcessPriority != tc.wantMode || res.ProcessPrioritySource != tc.wantSource {
				t.Errorf("result = (%q, %q), want (%q, %q)", res.ProcessPriority, res.ProcessPrioritySource, tc.wantMode, tc.wantSource)
			}
			// The service entrypoint is the plain host binary in every mode.
			if res.ServiceCommand != "/usr/local/bin/af host run" {
				t.Errorf("ServiceCommand = %q, want the unwrapped host command", res.ServiceCommand)
			}
			for _, marker := range markers {
				if got := strings.Contains(content, marker); got != tc.wantMarked {
					t.Errorf("service definition contains %q = %v, want %v:\n%s", marker, got, tc.wantMarked, content)
				}
			}
		})
	}
}

// TestInstall_ProcessPriorityKeptAcrossReinstall pins the recovery path the
// repo documents: re-running install after an upgrade must not silently reset
// the mode. Only an explicit mode changes it.
func TestInstall_ProcessPriorityKeptAcrossReinstall(t *testing.T) {
	markers := backgroundMarkers(t)
	isolateHome(t)

	assertMode := func(step string, res InstallResult, content string, wantMode ProcessPriority, wantSource ProcessPrioritySource) {
		t.Helper()
		if res.ProcessPriority != wantMode || res.ProcessPrioritySource != wantSource {
			t.Errorf("%s: result = (%q, %q), want (%q, %q)", step, res.ProcessPriority, res.ProcessPrioritySource, wantMode, wantSource)
		}
		for _, marker := range markers {
			if got, want := strings.Contains(content, marker), wantMode == ProcessPriorityBackground; got != want {
				t.Errorf("%s: service definition contains %q = %v, want %v", step, marker, got, want)
			}
		}
	}

	res, content := installIsolated(t, ProcessPriorityBackground)
	assertMode("explicit background", res, content, ProcessPriorityBackground, ProcessPrioritySourceExplicit)

	res, content = installIsolated(t, "")
	assertMode("re-install without a mode", res, content, ProcessPriorityBackground, ProcessPrioritySourceSaved)

	res, content = installIsolated(t, "")
	assertMode("second re-install without a mode", res, content, ProcessPriorityBackground, ProcessPrioritySourceSaved)

	res, content = installIsolated(t, ProcessPriorityDefault)
	assertMode("explicit default", res, content, ProcessPriorityDefault, ProcessPrioritySourceExplicit)

	res, content = installIsolated(t, "")
	assertMode("re-install after explicit default", res, content, ProcessPriorityDefault, ProcessPrioritySourceSaved)
}

func TestInstall_RejectsUnknownProcessPriority(t *testing.T) {
	backgroundMarkers(t) // skips on unsupported OSes
	isolateHome(t)
	_, err := Install(InstallOptions{
		HostBinPath:        "/usr/local/bin/af",
		ProcessPriority:    ProcessPriority("utility"),
		Scope:              ScopeUser,
		SkipServiceManager: true,
	})
	if err == nil {
		t.Fatal("expected an error for an unknown process priority")
	}
	if _, statErr := os.Stat(servicepriority.StatePath()); statErr == nil {
		t.Error("a rejected install saved a process priority")
	}
}

func TestInstall_UnreadableSavedPriorityFailsInsteadOfResetting(t *testing.T) {
	markers := backgroundMarkers(t)
	isolateHome(t)
	if err := os.MkdirAll(filepath.Dir(servicepriority.StatePath()), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(servicepriority.StatePath(), []byte("garbage\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := Install(InstallOptions{HostBinPath: "/usr/local/bin/af", Scope: ScopeUser, SkipServiceManager: true})
	if err == nil || !strings.Contains(err.Error(), "--process-priority") {
		t.Fatalf("Install with corrupt saved mode: err = %v, want an error pointing at --process-priority", err)
	}

	// An explicit mode does not read the saved one, and repairs it.
	res, content := installIsolated(t, ProcessPriorityBackground)
	if res.ProcessPriority != ProcessPriorityBackground {
		t.Errorf("explicit install resolved %q, want background", res.ProcessPriority)
	}
	for _, marker := range markers {
		if !strings.Contains(content, marker) {
			t.Errorf("service definition missing %q", marker)
		}
	}
	if mode, found, loadErr := servicepriority.Load(); loadErr != nil || !found || mode != servicepriority.Background {
		t.Errorf("saved mode after repair = (%q, %v, %v), want background", mode, found, loadErr)
	}
}

func TestInstall_RejectedInstallDoesNotSavePriority(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("only relevant on darwin")
	}
	isolateHome(t)
	_, err := Install(InstallOptions{
		HostBinPath:        "/usr/local/bin/af",
		ProcessPriority:    ProcessPriorityBackground,
		Scope:              ScopeSystem,
		SkipServiceManager: true,
	})
	if err == nil {
		t.Fatal("expected --system to be rejected on darwin")
	}
	if _, statErr := os.Stat(servicepriority.StatePath()); statErr == nil {
		t.Error("an install rejected before writing anything still saved a process priority")
	}
}

func TestInstall_RejectsSystemScopeOnDarwin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("only relevant on darwin")
	}
	_, err := Install(InstallOptions{
		HostBinPath:        "/usr/local/bin/af",
		Scope:              ScopeSystem,
		SkipServiceManager: true,
	})
	if err == nil {
		t.Errorf("expected error when --system scope on darwin")
	}
}

func TestUninstall_NoServiceInstalled(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("installer only supports darwin/linux; this is %s", runtime.GOOS)
	}

	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	res, err := Uninstall(UninstallOptions{
		Scope:              ScopeUser,
		SkipServiceManager: true,
	})
	if err != nil {
		t.Errorf("Uninstall on missing service must not error, got %v", err)
	}
	if res.OS != runtime.GOOS {
		t.Errorf("expected OS=%s, got %s", runtime.GOOS, res.OS)
	}
}

func TestDoctor_NoServiceInstalled(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("installer only supports darwin/linux; this is %s", runtime.GOOS)
	}

	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	res, err := Doctor(DoctorOptions{
		Scope:              ScopeUser,
		SkipServiceManager: true,
	})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if res.OS != runtime.GOOS {
		t.Errorf("expected OS=%s, got %s", runtime.GOOS, res.OS)
	}
	if res.Installed {
		t.Errorf("expected Installed=false on a fresh tmp HOME")
	}
}
