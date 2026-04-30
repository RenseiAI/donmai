package systemd

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

func TestGenerateUnitFile_User(t *testing.T) {
	out, err := GenerateUnitFile(ScopeUser, "/usr/local/bin/af", InstallOptions{})
	if err != nil {
		t.Fatalf("GenerateUnitFile: %v", err)
	}

	for _, want := range []string{
		"[Unit]",
		"Description=Rensei local daemon — worker pool",
		"After=network-online.target",
		"[Service]",
		"Type=simple",
		"ExecStart=/usr/local/bin/af daemon run",
		"Restart=on-failure",
		"SuccessExitStatus=3",
		"StandardOutput=journal",
		"StandardError=journal",
		"SyslogIdentifier=rensei-daemon",
		"WantedBy=default.target",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected unit file to contain %q, got:\n%s", want, out)
		}
	}

	if strings.Contains(out, "WantedBy=multi-user.target") {
		t.Errorf("user scope must not use multi-user.target")
	}
	if strings.Contains(out, "User=") {
		t.Errorf("user scope must not include a User= directive")
	}
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("unit file must end with a newline")
	}
}

func TestGenerateUnitFile_System(t *testing.T) {
	t.Setenv("SUDO_USER", "alice")

	out, err := GenerateUnitFile(ScopeSystem, "/usr/local/bin/af", InstallOptions{})
	if err != nil {
		t.Fatalf("GenerateUnitFile: %v", err)
	}

	if !strings.Contains(out, "WantedBy=multi-user.target") {
		t.Errorf("system scope must use multi-user.target")
	}
	if !strings.Contains(out, "User=alice") {
		t.Errorf("system scope must include User= directive (got SUDO_USER=alice)")
	}
}

func TestGenerateUnitFile_RegistersDaemonRunNotRenseiDaemon(t *testing.T) {
	out, err := GenerateUnitFile(ScopeUser, "/opt/af", InstallOptions{})
	if err != nil {
		t.Fatalf("GenerateUnitFile: %v", err)
	}

	// Locked REN-1406 decision: ExecStart must be `<host-binary> daemon run`,
	// NOT a separate rensei-daemon binary.
	if !strings.Contains(out, "ExecStart=/opt/af daemon run") {
		t.Errorf("ExecStart must register the host binary's daemon run subcommand. Got:\n%s", out)
	}
	if strings.Contains(out, "ExecStart=/opt/af start") || strings.Contains(out, "rensei-daemon start") {
		t.Errorf("ExecStart must NOT register the legacy rensei-daemon binary or start subcommand. Got:\n%s", out)
	}
}

func TestGenerateUnitFile_ConfigPath(t *testing.T) {
	out, err := GenerateUnitFile(ScopeUser, "/usr/local/bin/af", InstallOptions{
		ConfigPath: "/etc/rensei/daemon.yaml",
	})
	if err != nil {
		t.Fatalf("GenerateUnitFile: %v", err)
	}
	if !strings.Contains(out, "Environment=RENSEI_DAEMON_CONFIG=/etc/rensei/daemon.yaml") {
		t.Errorf("expected Environment= line for config path")
	}
}

func TestGenerateUnitFile_CustomDescription(t *testing.T) {
	out, err := GenerateUnitFile(ScopeUser, "/usr/local/bin/af", InstallOptions{
		Description: "Test description",
	})
	if err != nil {
		t.Fatalf("GenerateUnitFile: %v", err)
	}
	if !strings.Contains(out, "Description=Test description") {
		t.Errorf("expected custom description in unit file")
	}
}

func TestGenerateUnitFile_RequiresBinPath(t *testing.T) {
	if _, err := GenerateUnitFile(ScopeUser, "", InstallOptions{}); err == nil {
		t.Errorf("expected error when binPath is empty")
	}
}

func TestInstall_WritesUnitFileToTempDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", "")

	fr := newFakeRunner()
	unitPath, err := Install(InstallOptions{
		Scope:   ScopeUser,
		BinPath: "/usr/local/bin/af",
		Runner:  fr,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	// Path should be under HOME/.config/systemd/user/.
	expectedDir := filepath.Join(tmp, ".config", "systemd", "user")
	if filepath.Dir(unitPath) != expectedDir {
		t.Errorf("expected unit dir %s, got %s", expectedDir, filepath.Dir(unitPath))
	}
	if filepath.Base(unitPath) != UnitFilename {
		t.Errorf("expected unit filename %s, got %s", UnitFilename, filepath.Base(unitPath))
	}

	content, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read unit file: %v", err)
	}
	if !strings.Contains(string(content), "ExecStart=/usr/local/bin/af daemon run") {
		t.Errorf("unit file must register `daemon run`, got:\n%s", content)
	}

	// systemctl should have been called: daemon-reload then enable --now.
	if len(fr.calls) < 2 {
		t.Errorf("expected at least 2 systemctl calls, got %d: %v", len(fr.calls), fr.calls)
	}
	if !strings.Contains(strings.Join(fr.calls, "\n"), "daemon-reload") {
		t.Errorf("expected systemctl daemon-reload call, got: %v", fr.calls)
	}
	if !strings.Contains(strings.Join(fr.calls, "\n"), "enable --now "+UnitFilename) {
		t.Errorf("expected systemctl enable --now call, got: %v", fr.calls)
	}
}

func TestInstall_SkipSystemctl(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	fr := newFakeRunner()
	_, err := Install(InstallOptions{
		Scope:         ScopeUser,
		BinPath:       "/usr/local/bin/af",
		SkipSystemctl: true,
		Runner:        fr,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(fr.calls) != 0 {
		t.Errorf("expected no systemctl calls when SkipSystemctl=true, got %v", fr.calls)
	}
}

func TestInstall_PropagatesSystemctlError(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	fr := newFakeRunner()
	fr.set("systemctl --user daemon-reload", "boom", errors.New("daemon-reload failed"))

	_, err := Install(InstallOptions{
		Scope:   ScopeUser,
		BinPath: "/usr/local/bin/af",
		Runner:  fr,
	})
	if err == nil {
		t.Errorf("expected error from failed daemon-reload")
	}
}

func TestUninstall_RemovesUnitFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	// First install (skip systemctl so it works in any environment).
	unitPath, err := Install(InstallOptions{
		Scope:         ScopeUser,
		BinPath:       "/usr/local/bin/af",
		SkipSystemctl: true,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	if _, err := os.Stat(unitPath); err != nil {
		t.Fatalf("unit file should exist after install: %v", err)
	}

	fr := newFakeRunner()
	removedPath, err := Uninstall(UninstallOptions{Scope: ScopeUser, Runner: fr})
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if removedPath != unitPath {
		t.Errorf("expected uninstalled path %s, got %s", unitPath, removedPath)
	}
	if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
		t.Errorf("unit file should be removed; stat err=%v", err)
	}
}

func TestUninstall_NoUnitFile_NoError(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	fr := newFakeRunner()
	_, err := Uninstall(UninstallOptions{Scope: ScopeUser, Runner: fr})
	if err != nil {
		t.Errorf("Uninstall on missing unit must not error, got %v", err)
	}
}

func TestDoctor_NoUnit(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	res, err := Doctor(DoctorOptions{Scope: ScopeUser, SkipSystemctl: true})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if res.UnitExists {
		t.Errorf("expected UnitExists=false on empty home dir")
	}
	if res.Scope != ScopeUser {
		t.Errorf("expected scope user, got %q", res.Scope)
	}
}

func TestDoctor_UnitExists(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	if _, err := Install(InstallOptions{
		Scope:         ScopeUser,
		BinPath:       "/usr/local/bin/af",
		SkipSystemctl: true,
	}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	fr := newFakeRunner()
	fr.set("systemctl --user is-active rensei-daemon", "active", nil)
	fr.set("systemctl --user is-enabled rensei-daemon", "enabled", nil)
	fr.set("systemctl --user status --no-pager rensei-daemon", "● rensei-daemon.service - Rensei", nil)

	res, err := Doctor(DoctorOptions{Scope: ScopeUser, Runner: fr})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if !res.UnitExists {
		t.Errorf("expected UnitExists=true after install")
	}
	if res.IsActive == nil || !*res.IsActive {
		t.Errorf("expected IsActive=true, got %v", res.IsActive)
	}
	if res.IsEnabled == nil || !*res.IsEnabled {
		t.Errorf("expected IsEnabled=true, got %v", res.IsEnabled)
	}
	if !strings.Contains(res.StatusOutput, "rensei-daemon.service") {
		t.Errorf("expected status output to contain unit info, got %q", res.StatusOutput)
	}
}

func TestResolveHostBinPath_ExplicitOverride(t *testing.T) {
	got, err := ResolveHostBinPath("/explicit/path/af")
	if err != nil {
		t.Fatalf("ResolveHostBinPath: %v", err)
	}
	if got != "/explicit/path/af" {
		t.Errorf("expected explicit path, got %s", got)
	}
}

func TestResolveHostBinPath_FallsBackToExecutable(t *testing.T) {
	got, err := ResolveHostBinPath("")
	if err != nil {
		t.Fatalf("ResolveHostBinPath: %v", err)
	}
	if got == "" {
		t.Errorf("expected non-empty bin path")
	}
}

func TestUnitDir_UnknownScope(t *testing.T) {
	if _, err := UnitDir("nonsense"); err == nil {
		t.Errorf("expected error for unknown scope")
	}
}
