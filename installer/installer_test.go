package installer

import (
	"bytes"
	"log/slog"
	"runtime"
	"strings"
	"testing"
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

func TestInstall_DarwinProcessPriorityServiceCommand(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("only relevant on darwin")
	}

	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	res, err := Install(InstallOptions{
		HostBinPath:        "/usr/local/bin/af",
		ProcessPriority:    ProcessPriorityUtility,
		Scope:              ScopeUser,
		SkipServiceManager: true,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	want := "/usr/sbin/taskpolicy -c utility /usr/local/bin/af host run"
	if res.ServiceCommand != want {
		t.Fatalf("ServiceCommand = %q, want %q", res.ServiceCommand, want)
	}
}

func TestInstallLinux_IgnoresProcessPriorityWithWarning(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	res, err := installLinux(InstallOptions{
		HostBinPath:        "/usr/local/bin/af",
		ProcessPriority:    ProcessPriorityBackground,
		Scope:              ScopeUser,
		SkipServiceManager: true,
	})
	if err != nil {
		t.Fatalf("installLinux: %v", err)
	}
	if res.ServiceCommand != "/usr/local/bin/af host run" {
		t.Fatalf("ServiceCommand = %q, want unchanged Linux host run command", res.ServiceCommand)
	}
	if !strings.Contains(logs.String(), "unsupported on non-macOS installers") || !strings.Contains(logs.String(), "background") {
		t.Fatalf("expected unsupported process-priority warning, got:\n%s", logs.String())
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
