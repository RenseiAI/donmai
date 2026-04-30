package installer

import (
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

	// ServiceCommand must register `daemon run` against the host binary —
	// the locked REN-1406 decision.
	want := "/usr/local/bin/af daemon run"
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
