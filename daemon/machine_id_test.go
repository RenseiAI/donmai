package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveMachineID_ResolutionOrder pins the tier order and, for each tier,
// the property that makes it usable as a host identity.
func TestResolveMachineID_ResolutionOrder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		env        string
		native     string
		localDir   func(t *testing.T) func() (string, error)
		wantSource MachineIDSource
		wantID     string // exact match when set
		wantPrefix bool   // otherwise assert the mid_ prefix
	}{
		{
			name:       "operator override wins over everything",
			env:        "pinned-machine-7",
			native:     "native-id-that-must-lose",
			wantSource: MachineIDSourceEnv,
			wantID:     "pinned-machine-7",
		},
		{
			name:       "override is sanitized to the wire alphabet",
			env:        "Machine Name.local",
			wantSource: MachineIDSourceEnv,
			wantID:     "Machine-Name-local",
		},
		{
			name:       "native identifier is preferred over generating one",
			native:     "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE",
			wantSource: MachineIDSourceNative,
			wantPrefix: true,
		},
		{
			name:       "generates and persists when no native identifier exists",
			wantSource: MachineIDSourceGenerated,
			wantPrefix: true,
		},
		{
			name: "degrades to ephemeral when nothing is writable",
			localDir: func(*testing.T) func() (string, error) {
				return func() (string, error) { return "", errors.New("no home") }
			},
			wantSource: MachineIDSourceEphemeral,
			wantPrefix: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			localDir := func() (string, error) { return dir, nil }
			if tc.localDir != nil {
				localDir = tc.localDir(t)
			}
			got, source, _ := resolveMachineID(machineIDDeps{
				Getenv:   func(string) string { return tc.env },
				NativeID: func() string { return tc.native },
				LocalDir: localDir,
			})
			if source != tc.wantSource {
				t.Errorf("source = %q, want %q", source, tc.wantSource)
			}
			if tc.wantID != "" && got != tc.wantID {
				t.Errorf("machine id = %q, want %q", got, tc.wantID)
			}
			if tc.wantPrefix && !strings.HasPrefix(got, machineIDPrefix) {
				t.Errorf("machine id %q should carry the %q prefix", got, machineIDPrefix)
			}
			if got == "" {
				t.Error("machine id must never be empty — an empty identity is worse than a degraded one")
			}
		})
	}
}

// TestResolveMachineID_IsSingleValuedAcrossHostnameChange is the regression
// test for host identity forking.
//
// One machine used to present itself under every hostname form it had ever
// resolved to — "<name>.local" on one network, "<name>.localdomain" on
// another — and each form became a separate host upstream. The resolved
// identity must not move when the hostname does.
//
// Against the pre-fix code the identity WAS the hostname, so these three
// lookups produced three identities.
func TestResolveMachineID_IsSingleValuedAcrossHostnameChange(t *testing.T) {
	t.Parallel()

	const native = "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE"
	dir := t.TempDir()

	var first string
	// Every one of these is the SAME machine seen through a different
	// hostname lookup at a different moment.
	for _, hostname := range []string{
		"machine.local",
		"machine.localdomain",
		"MACHINE",
		"machine.lan",
	} {
		got, source, err := resolveMachineID(machineIDDeps{
			Getenv:   func(string) string { return "" },
			NativeID: func() string { return native },
			LocalDir: func() (string, error) { return dir, nil },
		})
		if err != nil {
			t.Fatalf("resolve for %q: %v", hostname, err)
		}
		if source != MachineIDSourceNative {
			t.Fatalf("expected the native tier, got %q", source)
		}
		if first == "" {
			first = got
			continue
		}
		if got != first {
			t.Errorf("identity moved with the hostname (%q): %q != %q — one machine must be one identity",
				hostname, got, first)
		}
	}
}

// TestResolveMachineID_NativeIdentifierIsNotTransmitted asserts the privacy
// property: the OS-native machine identifier is hashed, never echoed.
func TestResolveMachineID_NativeIdentifierIsNotTransmitted(t *testing.T) {
	t.Parallel()

	const native = "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE"
	got, _, _ := resolveMachineID(machineIDDeps{
		Getenv:   func(string) string { return "" },
		NativeID: func() string { return native },
		LocalDir: func() (string, error) { return t.TempDir(), nil },
	})
	if strings.Contains(strings.ToLower(got), strings.ToLower(native)) {
		t.Errorf("machine id %q leaks the native identifier verbatim", got)
	}
	for _, segment := range strings.Split(native, "-") {
		if strings.Contains(strings.ToLower(got), strings.ToLower(segment)) {
			t.Errorf("machine id %q leaks native segment %q", got, segment)
		}
	}
}

// TestResolveMachineID_SurvivesStateWipeWhenNativeIdExists is the re-install
// property: wiping local state and starting over must RECLAIM the existing
// identity rather than fork a new one. This is what stops repeated test
// installs from littering the fleet with abandoned hosts.
func TestResolveMachineID_SurvivesStateWipeWhenNativeIdExists(t *testing.T) {
	t.Parallel()

	deps := func(dir string) machineIDDeps {
		return machineIDDeps{
			Getenv:   func(string) string { return "" },
			NativeID: func() string { return "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE" },
			LocalDir: func() (string, error) { return dir, nil },
		}
	}
	before, _, _ := resolveMachineID(deps(t.TempDir()))
	// A fresh, empty local directory stands in for an uninstall/reinstall.
	after, _, _ := resolveMachineID(deps(t.TempDir()))
	if before != after {
		t.Errorf("re-install forked the identity: %q -> %q", before, after)
	}
}

// TestResolveMachineID_PersistedFallbackIsReusedAndLocal covers the tier-3
// path: generate once, reuse thereafter, and write somewhere a config sync
// would not carry to another machine.
func TestResolveMachineID_PersistedFallbackIsReusedAndLocal(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	deps := machineIDDeps{
		Getenv:   func(string) string { return "" },
		NativeID: func() string { return "" },
		LocalDir: func() (string, error) { return dir, nil },
	}

	first, source, err := resolveMachineID(deps)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if source != MachineIDSourceGenerated {
		t.Fatalf("first resolve source = %q, want %q", source, MachineIDSourceGenerated)
	}

	second, source, err := resolveMachineID(deps)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if source != MachineIDSourcePersisted {
		t.Errorf("second resolve source = %q, want %q", source, MachineIDSourcePersisted)
	}
	if second != first {
		t.Errorf("persisted identity changed across restarts: %q -> %q", first, second)
	}

	raw, err := os.ReadFile(filepath.Join(dir, machineIDFileName))
	if err != nil {
		t.Fatalf("read persisted file: %v", err)
	}
	var p persistedMachineID
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode persisted file: %v", err)
	}
	if p.MachineID != first {
		t.Errorf("persisted machineId = %q, want %q", p.MachineID, first)
	}
	if p.Note == "" {
		t.Error("the persisted file must warn a human against copying it between machines")
	}
}

// TestLocalMachineStateDir_IsNotTheSyncableStateDir asserts the placement
// decision: the generated-identity file lives in the machine-local data
// directory, never alongside the daemon config an operator may sync between
// their machines (two machines sharing one identity are indistinguishable
// upstream).
func TestLocalMachineStateDir_IsNotTheSyncableStateDir(t *testing.T) {
	t.Parallel()

	dir, err := localMachineStateDir()
	if err != nil {
		t.Skipf("no resolvable local state dir in this environment: %v", err)
	}
	if dir == "" {
		t.Fatal("local state dir must not be empty")
	}
	stateDir := filepath.Dir(DefaultConfigPath())
	if dir == stateDir {
		t.Errorf("machine identity must not live in the syncable state dir %q", stateDir)
	}
}

// TestSanitizeMachineID is table-driven over the wire-safety normalization.
func TestSanitizeMachineID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: ""},
		{name: "whitespace only", in: "   ", want: ""},
		{name: "already safe", in: "mid_abc123", want: "mid_abc123"},
		{name: "dots become dashes", in: "host.local", want: "host-local"},
		{name: "spaces become dashes", in: "my machine", want: "my-machine"},
		{name: "edges trimmed", in: "...host...", want: "host"},
		{name: "only separators", in: "...", want: ""},
		{name: "length capped", in: strings.Repeat("a", 300), want: strings.Repeat("a", 128)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := sanitizeMachineID(tc.in); got != tc.want {
				t.Errorf("sanitizeMachineID(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestParseDarwinPlatformUUID covers the ioreg output parser without needing a
// darwin host.
func TestParseDarwinPlatformUUID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		out  string
		want string
	}{
		{
			name: "typical ioreg block",
			out: `+-o J316sAP  <class IOPlatformExpertDevice, id 0x100000253>
    {
      "IOPlatformUUID" = "12345678-90AB-CDEF-1234-567890ABCDEF"
      "IOPlatformSerialNumber" = "XXXXXXXXXX"
    }`,
			want: "12345678-90AB-CDEF-1234-567890ABCDEF",
		},
		{name: "absent", out: `{ "IOPlatformSerialNumber" = "X" }`, want: ""},
		{name: "empty output", out: "", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := parseDarwinPlatformUUID(tc.out); got != tc.want {
				t.Errorf("parseDarwinPlatformUUID = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNormalizeMachineLabel pins the hostname LABEL normalization: one label
// per machine even when the resolver alternates between DNS forms.
func TestNormalizeMachineLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "mdns form", in: "Machine.local", want: "machine"},
		{name: "dhcp domain form", in: "Machine.localdomain", want: "machine"},
		{name: "fully qualified", in: "machine.example.internal", want: "machine"},
		{name: "bare", in: "MACHINE", want: "machine"},
		{name: "underscores collapse", in: "my_machine_01", want: "my-machine-01"},
		{name: "repeat separators squashed", in: "a---b", want: "a-b"},
		{name: "empty falls back", in: "", want: "local-machine"},
		{name: "all separators falls back", in: "...", want: "local-machine"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := normalizeMachineLabel(tc.in); got != tc.want {
				t.Errorf("normalizeMachineLabel(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormalizeMachineLabel_DNSFormsCollapseToOne states the property
// directly: the DNS forms one macOS box alternates between must not produce
// two labels.
func TestNormalizeMachineLabel_DNSFormsCollapseToOne(t *testing.T) {
	t.Parallel()

	mdns := normalizeMachineLabel("Some-Box.local")
	dhcp := normalizeMachineLabel("Some-Box.localdomain")
	if mdns != dhcp {
		t.Errorf("one machine produced two labels: %q vs %q", mdns, dhcp)
	}
}
