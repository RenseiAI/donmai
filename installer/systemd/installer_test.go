package systemd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/installer/servicepriority"
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

	// TestGenerateUnitFile_SetupRefusalHoldsFailed pins the fresh-host
	// restart contract: the setup refusal exits with the status the
	// unit names in RestartPreventExitStatus, so the unit waits for its
	// config instead of restarting every RestartSec. Dropping the
	// directive (or changing the refusal's exit) must fail this test.
	for _, want := range []string{
		"[Unit]",
		"Description=Donmai local daemon \u2014 worker pool",
		"After=network-online.target",
		"[Service]",
		"Type=simple",
		"ExecStart=/usr/local/bin/af host run",
		"Restart=on-failure",
		"SuccessExitStatus=3",
		"RestartPreventExitStatus=4",
		"StandardOutput=journal",
		"StandardError=journal",
		"SyslogIdentifier=donmai-daemon",
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

func TestGenerateUnitFile_RegistersHostRunNotSeparateDaemonBinary(t *testing.T) {
	out, err := GenerateUnitFile(ScopeUser, "/opt/af", InstallOptions{})
	if err != nil {
		t.Fatalf("GenerateUnitFile: %v", err)
	}

	// Locked decision: ExecStart must be `<host-binary> host run`,
	// NOT a separate daemon binary.
	if !strings.Contains(out, "ExecStart=/opt/af host run") {
		t.Errorf("ExecStart must register the host binary's host run subcommand. Got:\n%s", out)
	}
	if strings.Contains(out, "ExecStart=/opt/af start") || strings.Contains(out, "donmai-daemon start") {
		t.Errorf("ExecStart must NOT register a separate daemon binary or start subcommand. Got:\n%s", out)
	}
}

// TestGenerateUnitFile_BrandNeutralDefault pins the debrand: the default
// unit carries the OSS unit name, the OSS description, and the matching
// journal tag. The pre-rename strings must not appear anywhere in a default
// render — the log-identifier gap the guard's case-sensitive brand rule
// cannot see is pinned here instead.
func TestGenerateUnitFile_BrandNeutralDefault(t *testing.T) {
	out, err := GenerateUnitFile(ScopeUser, "/usr/local/bin/af", InstallOptions{})
	if err != nil {
		t.Fatalf("GenerateUnitFile: %v", err)
	}
	for _, want := range []string{
		"Description=" + DefaultDescription,
		"SyslogIdentifier=" + DefaultUnitName,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected unit file to contain %q, got:\n%s", want, out)
		}
	}
	if DefaultDescription != "Donmai local daemon \u2014 worker pool" {
		t.Errorf("DefaultDescription = %q, want the brand-neutral default", DefaultDescription)
	}
	if DefaultUnitName != "donmai-daemon" {
		t.Errorf("DefaultUnitName = %q, want donmai-daemon", DefaultUnitName)
	}
	// The pre-rename unit name and description must not appear anywhere in
	// a default render. The literals are assembled so this assertion does
	// not itself reintroduce the strings the guard exists to catch: the
	// brand rule is case-sensitive and blind to the lowercase unit name,
	// which is exactly the gap this test closes.
	legacyUnit := LegacyUnitName()
	legacyTitle := strings.ToTitle(legacyUnit[:6]) + legacyUnit[6:]
	for _, banned := range []string{legacyUnit, legacyTitle} {
		if strings.Contains(out, banned) {
			t.Errorf("default unit must not contain %q, got:\n%s", banned, out)
		}
	}
}

func TestGenerateUnitFile_ConfigPath(t *testing.T) {
	out, err := GenerateUnitFile(ScopeUser, "/usr/local/bin/af", InstallOptions{
		ConfigPath: "/etc/donmai/daemon.yaml",
	})
	if err != nil {
		t.Fatalf("GenerateUnitFile: %v", err)
	}
	if !strings.Contains(out, "Environment=DONMAI_DAEMON_CONFIG=/etc/donmai/daemon.yaml") {
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

// priorityDirectiveKeys are every unit key the process-priority modes may set.
var priorityDirectiveKeys = []string{"Nice=", "CPUSchedulingPolicy=", "IOSchedulingClass=", "IOSchedulingPriority="}

func TestGenerateUnitFile_ProcessPriorityPerMode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cases := []struct {
		name     string
		priority servicepriority.Mode
		want     []string // directives that must appear, in [Service]
	}{
		{name: "unset reads as default", priority: ""},
		{name: "default", priority: servicepriority.Default},
		{
			name:     "background",
			priority: servicepriority.Background,
			want:     []string{"Nice=19", "CPUSchedulingPolicy=idle", "IOSchedulingClass=idle"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := GenerateUnitFile(ScopeUser, "/usr/local/bin/af", InstallOptions{ProcessPriority: tc.priority})
			if err != nil {
				t.Fatalf("GenerateUnitFile: %v", err)
			}
			serviceStart := strings.Index(out, "[Service]")
			installStart := strings.Index(out, "[Install]")
			if serviceStart < 0 || installStart < serviceStart {
				t.Fatalf("unit has no [Service] before [Install]:\n%s", out)
			}
			service := out[serviceStart:installStart]

			for _, directive := range tc.want {
				if !strings.Contains(service, "\n"+directive+"\n") {
					t.Errorf("[Service] missing %q:\n%s", directive, service)
				}
			}
			for _, key := range priorityDirectiveKeys {
				if len(tc.want) == 0 && strings.Contains(out, key) {
					t.Errorf("mode %q must not emit %s, got:\n%s", tc.priority, key, out)
				}
			}
			// The service keeps its entrypoint in every mode.
			if !strings.Contains(out, "ExecStart=/usr/local/bin/af host run\n") {
				t.Errorf("ExecStart changed under mode %q:\n%s", tc.priority, out)
			}
		})
	}
}

// TestGenerateUnitFile_DefaultPriorityIsByteIdentical pins that selecting the
// default mode, explicitly or not, renders exactly the unit an install without
// the feature always rendered.
func TestGenerateUnitFile_DefaultPriorityIsByteIdentical(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	base, err := GenerateUnitFile(ScopeUser, "/usr/local/bin/af", InstallOptions{})
	if err != nil {
		t.Fatalf("GenerateUnitFile: %v", err)
	}
	got, err := GenerateUnitFile(ScopeUser, "/usr/local/bin/af", InstallOptions{ProcessPriority: servicepriority.Default})
	if err != nil {
		t.Fatalf("GenerateUnitFile(default): %v", err)
	}
	if got != base {
		t.Errorf("default mode changed the unit:\n--- got ---\n%s\n--- want ---\n%s", got, base)
	}
}

func TestGenerateUnitFile_RejectsUnknownProcessPriority(t *testing.T) {
	for _, mode := range []servicepriority.Mode{"utility", "faster"} {
		if _, err := GenerateUnitFile(ScopeUser, "/usr/local/bin/af", InstallOptions{ProcessPriority: mode}); err == nil {
			t.Errorf("mode %q: expected an error", mode)
		}
	}
}

func TestInstall_WritesBackgroundProcessPriority(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	unitPath, err := Install(InstallOptions{
		Scope:           ScopeUser,
		BinPath:         "/usr/local/bin/af",
		ProcessPriority: servicepriority.Background,
		SkipSystemctl:   true,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	content, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read unit: %v", err)
	}
	for _, want := range []string{"Nice=19\n", "CPUSchedulingPolicy=idle\n", "IOSchedulingClass=idle\n"} {
		if !strings.Contains(string(content), want) {
			t.Errorf("installed unit missing %q:\n%s", want, content)
		}
	}
}

func TestGenerateUnitFile_RequiresBinPath(t *testing.T) {
	if _, err := GenerateUnitFile(ScopeUser, "", InstallOptions{}); err == nil {
		t.Errorf("expected error when binPath is empty")
	}
}

// TestGenerateUnitFile_PathIncludesUserLocalBin asserts the v0.5.1
// fix: the unit file's Environment=PATH must prepend ~/.local/bin so
// user-local installs of provider CLIs like `claude` are visible to
// the daemon.
func TestGenerateUnitFile_PathIncludesUserLocalBin(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	out, err := GenerateUnitFile(ScopeUser, "/usr/local/bin/af", InstallOptions{})
	if err != nil {
		t.Fatalf("GenerateUnitFile: %v", err)
	}

	want := filepath.Join(tmp, ".local", "bin")
	wantLinePrefix := "Environment=PATH=" + want + ":"
	if !strings.Contains(out, wantLinePrefix) {
		t.Errorf("unit file Environment=PATH must start with %q. Got:\n%s", wantLinePrefix, out)
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
	if !strings.Contains(string(content), "ExecStart=/usr/local/bin/af host run") {
		t.Errorf("unit file must register `host run`, got:\n%s", content)
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
	fr.set("systemctl --user is-active donmai-daemon", "active", nil)
	fr.set("systemctl --user is-enabled donmai-daemon", "enabled", nil)
	fr.set("systemctl --user status --no-pager donmai-daemon", "\u25cf donmai-daemon.service - Donmai", nil)

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
	if !strings.Contains(res.StatusOutput, "donmai-daemon.service") {
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

// TestGenerateUnitFile_KeepsTheControlGroupKill pins the generated unit's
// stop posture to the corpus: the unit sets no KillMode, so stopping it keeps
// systemd's default control-group kill and a direct-owned seat in the unit's
// cgroup ends with its daemon. Shim-owned seats survive a restart because each
// starts in its own transient scope outside this cgroup, not because the unit
// stops killing its children; `KillMode=process` on the daemon unit is
// explicitly not used. Delegate is absent too: the daemon creates no cgroups
// beneath its unit, so delegating the subtree to it buys nothing.
func TestGenerateUnitFile_KeepsTheControlGroupKill(t *testing.T) {
	for _, scope := range []Scope{ScopeUser, ScopeSystem} {
		out, err := GenerateUnitFile(scope, "/usr/local/bin/af", InstallOptions{})
		if err != nil {
			t.Fatalf("GenerateUnitFile(%s): %v", scope, err)
		}
		for _, banned := range []string{"KillMode=", "Delegate="} {
			if strings.Contains(out, banned) {
				t.Errorf("scope %s unit sets %q; the unit keeps the default control-group kill and the seat scope is the survival boundary:\n%s", scope, banned, out)
			}
		}
	}
}

// TestGenerateUnitFile_EmbedderUnitNameOverride pins the downstream seam:
// an embedder keeps its own unit name and journal tag through installer
// config, and the render carries no OSS default strings.
func TestGenerateUnitFile_EmbedderUnitNameOverride(t *testing.T) {
	out, err := GenerateUnitFile(ScopeUser, "/usr/local/bin/af", InstallOptions{UnitName: "acme-daemon"})
	if err != nil {
		t.Fatalf("GenerateUnitFile: %v", err)
	}
	for _, want := range []string{"SyslogIdentifier=acme-daemon"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected unit file to contain %q, got:\n%s", want, out)
		}
	}
	for _, banned := range []string{"SyslogIdentifier=" + DefaultUnitName, "SyslogIdentifier=" + LegacyUnitName()} {
		if strings.Contains(out, banned) {
			t.Errorf("override render must not contain %q, got:\n%s", banned, out)
		}
	}
	if got := ResolveUnitFilename("acme-daemon"); got != "acme-daemon.service" {
		t.Errorf("ResolveUnitFilename = %q, want acme-daemon.service", got)
	}
}

// TestGenerateUnitFile_EmbedderSyslogIdentifierOverride pins the journal-tag
// half of the seam: an explicit tag wins over the unit-name default.
func TestGenerateUnitFile_EmbedderSyslogIdentifierOverride(t *testing.T) {
	out, err := GenerateUnitFile(ScopeUser, "/usr/local/bin/af", InstallOptions{
		UnitName:         "acme-daemon",
		SyslogIdentifier: "acme-journal",
	})
	if err != nil {
		t.Fatalf("GenerateUnitFile: %v", err)
	}
	if !strings.Contains(out, "SyslogIdentifier=acme-journal") {
		t.Errorf("expected explicit journal tag, got:\n%s", out)
	}
}

// TestResolveUnitName pins the override normalization: empty means the OSS
// default, a suffixed name agrees with its bare form, and surrounding
// whitespace never leaks into a filename.
func TestResolveUnitName(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		want  string
		wantF string
	}{
		{name: "empty means default", in: "", want: DefaultUnitName, wantF: DefaultUnitName + ".service"},
		{name: "bare name", in: "acme-daemon", want: "acme-daemon", wantF: "acme-daemon.service"},
		{name: "suffixed agrees with bare", in: "acme-daemon.service", want: "acme-daemon", wantF: "acme-daemon.service"},
		{name: "whitespace trimmed", in: "  acme-daemon  ", want: "acme-daemon", wantF: "acme-daemon.service"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveUnitName(tc.in); got != tc.want {
				t.Errorf("ResolveUnitName(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if got := ResolveUnitFilename(tc.in); got != tc.wantF {
				t.Errorf("ResolveUnitFilename(%q) = %q, want %q", tc.in, got, tc.wantF)
			}
		})
	}
}

// TestInstall_FreshInstallWritesCurrentUnit pins the fresh-install path: with
// no legacy unit on disk the install writes the OSS unit, enables it, and
// issues no legacy stop/disable.
func TestInstall_FreshInstallWritesCurrentUnit(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	fr := newFakeRunner()
	unitPath, err := Install(InstallOptions{
		Scope:   ScopeUser,
		BinPath: "/usr/local/bin/af",
		Runner:  fr,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if filepath.Base(unitPath) != DefaultUnitName+".service" {
		t.Errorf("fresh install wrote %q, want %q", filepath.Base(unitPath), DefaultUnitName+".service")
	}
	content, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read unit: %v", err)
	}
	if !strings.Contains(string(content), "SyslogIdentifier="+DefaultUnitName) {
		t.Errorf("fresh unit must carry the current journal tag, got:\n%s", content)
	}
	joined := strings.Join(fr.calls, "\n")
	if !strings.Contains(joined, "enable --now "+DefaultUnitName+".service") {
		t.Errorf("fresh install must enable the current unit, calls:\n%s", joined)
	}
	for _, banned := range []string{"stop " + LegacyUnitName(), "disable " + LegacyUnitFilename()} {
		if strings.Contains(joined, banned) {
			t.Errorf("fresh install must not touch the legacy unit (%q), calls:\n%s", banned, joined)
		}
	}
}

// TestInstall_MigratesLegacyUnit pins the upgrade path: a pre-rename unit on
// disk is stopped, disabled, and removed, and the current unit is installed
// and enabled. The daemon config on disk is preserved byte-for-byte — state
// lives outside the unit, so the migration moves no state.
func TestInstall_MigratesLegacyUnit(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	unitDir := filepath.Join(tmp, ".config", "systemd", "user")
	// #nosec G301 -- test seed mirrors the 0755 unit dirs production creates.
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	legacyPath := filepath.Join(unitDir, LegacyUnitFilename())
	legacyContent := "[Unit]\nDescription=legacy\n[Service]\nExecStart=/usr/local/bin/af host run\n[Install]\nWantedBy=default.target\n"
	if err := os.WriteFile(legacyPath, []byte(legacyContent), 0o644); err != nil { //nolint:gosec // test seed mirrors the world-readable unit file
		t.Fatalf("seed legacy unit: %v", err)
	}

	fr := newFakeRunner()
	unitPath, err := Install(InstallOptions{
		Scope:   ScopeUser,
		BinPath: "/usr/local/bin/af",
		Runner:  fr,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if filepath.Base(unitPath) != DefaultUnitName+".service" {
		t.Errorf("migrated install wrote %q, want %q", filepath.Base(unitPath), DefaultUnitName+".service")
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Errorf("legacy unit must be removed; stat err=%v", err)
	}
	joined := strings.Join(fr.calls, "\n")
	for _, want := range []string{
		"stop " + LegacyUnitName(),
		"disable " + LegacyUnitFilename(),
		"enable --now " + DefaultUnitName + ".service",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("migration must run %q, calls:\n%s", want, joined)
		}
	}
	// The stop order matters: the legacy unit stops before the new unit is
	// enabled, so two units never fight over the same daemon.
	stopAt := strings.Index(joined, "stop "+LegacyUnitName())
	enableAt := strings.Index(joined, "enable --now "+DefaultUnitName+".service")
	if stopAt < 0 || enableAt < 0 || stopAt > enableAt {
		t.Errorf("legacy stop must precede new enable, calls:\n%s", joined)
	}
}

// TestInstall_MigrationPreservesConfig pins the state half of the upgrade:
// the daemon config bytes on disk survive the migration untouched — the new
// unit starts the same binary against the same config.
func TestInstall_MigrationPreservesConfig(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	cfgPath := filepath.Join(tmp, "daemon.yaml")
	cfgBytes := []byte("apiVersion: donmai.dev/v1\nkind: LocalDaemon\nmachine:\n  id: operator-host\ncapacity:\n  maxConcurrentSessions: 2\norchestrator:\n  url: http://127.0.0.1:1\n")
	if err := os.WriteFile(cfgPath, cfgBytes, 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	unitDir := filepath.Join(tmp, ".config", "systemd", "user")
	// #nosec G301 -- test seed mirrors the 0755 unit dirs production creates.
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(unitDir, LegacyUnitFilename()), []byte("[Unit]\n"), 0o644); err != nil { //nolint:gosec // test seed mirrors the world-readable unit file
		t.Fatalf("seed legacy unit: %v", err)
	}

	if _, err := Install(InstallOptions{
		Scope:      ScopeUser,
		BinPath:    "/usr/local/bin/af",
		ConfigPath: cfgPath,
		Runner:     newFakeRunner(),
	}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(after) != string(cfgBytes) {
		t.Errorf("migration rewrote the daemon config:\n--- before ---\n%s\n--- after ---\n%s", cfgBytes, after)
	}
}

// TestInstall_MigrationSkipsSystemctlRemovesFileOnly pins the hermetic half:
// with SkipSystemctl the migration still removes the legacy file but issues
// no service-manager calls.
func TestInstall_MigrationSkipsSystemctlRemovesFileOnly(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	unitDir := filepath.Join(tmp, ".config", "systemd", "user")
	// #nosec G301 -- test seed mirrors the 0755 unit dirs production creates.
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	legacyPath := filepath.Join(unitDir, LegacyUnitFilename())
	if err := os.WriteFile(legacyPath, []byte("[Unit]\n"), 0o644); err != nil { //nolint:gosec // test seed mirrors the world-readable unit file
		t.Fatalf("seed legacy unit: %v", err)
	}

	fr := newFakeRunner()
	if _, err := Install(InstallOptions{
		Scope:         ScopeUser,
		BinPath:       "/usr/local/bin/af",
		Runner:        fr,
		SkipSystemctl: true,
	}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Errorf("legacy unit must be removed; stat err=%v", err)
	}
	if len(fr.calls) != 0 {
		t.Errorf("SkipSystemctl migration must issue no systemctl calls, got %v", fr.calls)
	}
}

// TestInstall_ReinstallIsStable pins idempotence: installing twice over the
// same options leaves the unit bytes unchanged.
func TestInstall_ReinstallIsStable(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	fr := newFakeRunner()
	first, err := Install(InstallOptions{Scope: ScopeUser, BinPath: "/usr/local/bin/af", Runner: fr})
	if err != nil {
		t.Fatalf("first Install: %v", err)
	}
	before, err := os.ReadFile(first)
	if err != nil {
		t.Fatalf("read first unit: %v", err)
	}
	second, err := Install(InstallOptions{Scope: ScopeUser, BinPath: "/usr/local/bin/af", Runner: newFakeRunner()})
	if err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if second != first {
		t.Errorf("reinstall path changed: %q vs %q", second, first)
	}
	after, err := os.ReadFile(second)
	if err != nil {
		t.Fatalf("read second unit: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("reinstall rewrote the unit:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}

// TestUninstall_RemovesBothUnits pins the uninstall half of the rename: with
// both the current and the legacy unit on disk, one uninstall removes both.
func TestUninstall_RemovesBothUnits(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	unitDir := filepath.Join(tmp, ".config", "systemd", "user")
	// #nosec G301 -- test seed mirrors the 0755 unit dirs production creates.
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	currentPath := filepath.Join(unitDir, DefaultUnitName+".service")
	legacyPath := filepath.Join(unitDir, LegacyUnitFilename())
	for _, p := range []string{currentPath, legacyPath} {
		if err := os.WriteFile(p, []byte("[Unit]\n"), 0o644); err != nil { //nolint:gosec // test seed mirrors the world-readable unit file
			t.Fatalf("seed %s: %v", p, err)
		}
	}

	fr := newFakeRunner()
	removed, err := Uninstall(UninstallOptions{Scope: ScopeUser, Runner: fr})
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if removed != currentPath {
		t.Errorf("Uninstall returned %q, want the current unit %q", removed, currentPath)
	}
	for _, p := range []string{currentPath, legacyPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s must be removed; stat err=%v", p, err)
		}
	}
	joined := strings.Join(fr.calls, "\n")
	for _, want := range []string{"stop " + LegacyUnitName(), "stop " + DefaultUnitName} {
		if !strings.Contains(joined, want) {
			t.Errorf("uninstall must %q, calls:\n%s", want, joined)
		}
	}
}

// TestDoctor_ReportsPendingMigration pins the doctor half: with only the
// legacy unit on disk, doctor names the pending migration instead of
// reporting a healthy install.
func TestDoctor_ReportsPendingMigration(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	unitDir := filepath.Join(tmp, ".config", "systemd", "user")
	// #nosec G301 -- test seed mirrors the 0755 unit dirs production creates.
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	legacyPath := filepath.Join(unitDir, LegacyUnitFilename())
	if err := os.WriteFile(legacyPath, []byte("[Unit]\n"), 0o644); err != nil { //nolint:gosec // test seed mirrors the world-readable unit file
		t.Fatalf("seed legacy unit: %v", err)
	}

	res, err := Doctor(DoctorOptions{Scope: ScopeUser, SkipSystemctl: true})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if res.UnitExists {
		t.Errorf("UnitExists must be false with only the legacy unit on disk")
	}
	if res.LegacyUnit != legacyPath {
		t.Errorf("LegacyUnit = %q, want %q", res.LegacyUnit, legacyPath)
	}
}

// TestDoctor_ReportsLegacyActive pins the running-seat half: with both units
// on disk and the legacy unit active, doctor reports both states so the
// operator can see the migration has not run yet.
func TestDoctor_ReportsLegacyActive(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	if _, err := Install(InstallOptions{Scope: ScopeUser, BinPath: "/usr/local/bin/af", SkipSystemctl: true}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	unitDir := filepath.Join(tmp, ".config", "systemd", "user")
	if err := os.WriteFile(filepath.Join(unitDir, LegacyUnitFilename()), []byte("[Unit]\n"), 0o644); err != nil { //nolint:gosec // test seed mirrors the world-readable unit file
		t.Fatalf("seed legacy unit: %v", err)
	}

	fr := newFakeRunner()
	fr.set("systemctl --user is-active "+DefaultUnitName, "", errors.New("inactive"))
	fr.set("systemctl --user is-enabled "+DefaultUnitName, "enabled", nil)
	fr.set("systemctl --user status --no-pager "+DefaultUnitName, "inactive", nil)
	fr.set("systemctl --user is-active "+LegacyUnitName(), "active", nil)

	res, err := Doctor(DoctorOptions{Scope: ScopeUser, Runner: fr})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if !res.UnitExists {
		t.Errorf("UnitExists must be true with the current unit on disk")
	}
	if res.LegacyUnit == "" {
		t.Errorf("LegacyUnit must name the pending migration")
	}
	if res.LegacyActive == nil || !*res.LegacyActive {
		t.Errorf("LegacyActive must be true while the legacy unit runs, got %v", res.LegacyActive)
	}
	if res.IsActive == nil || *res.IsActive {
		t.Errorf("IsActive must be false while the current unit is down, got %v", res.IsActive)
	}
}
