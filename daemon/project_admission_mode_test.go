package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afclient"
)

// The admission mode is the machine owner's ONE standing consent decision.
// These tests pin the three properties that make it safe to ship:
//
//  1. it never widens by accident — absent, blank, and misspelled all read as
//     the enumerated default;
//  2. under all-routed the spawner stops consulting the per-project list; and
//  3. a change to admission reaches the orchestrator on the next heartbeat,
//     not on the next process start.

func TestEffectiveProjectAdmissionModeFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "absent", raw: "", want: ProjectAdmissionModeEnumerated},
		{name: "blank", raw: "   ", want: ProjectAdmissionModeEnumerated},
		{name: "explicit enumerated", raw: "enumerated", want: ProjectAdmissionModeEnumerated},
		{name: "all routed", raw: "all-routed", want: ProjectAdmissionModeAllRouted},
		{name: "all routed mixed case", raw: "All-Routed", want: ProjectAdmissionModeAllRouted},
		{name: "all routed padded", raw: "  all-routed  ", want: ProjectAdmissionModeAllRouted},
		// A typo must NOT be read as consent. The operator gets the narrow
		// mode and a validation error, never a silently wide machine.
		{name: "underscore typo", raw: "all_routed", want: ProjectAdmissionModeEnumerated},
		{name: "nonsense", raw: "everything", want: ProjectAdmissionModeEnumerated},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &Config{ProjectAdmissionMode: tc.raw}
			if got := cfg.EffectiveProjectAdmissionMode(); got != tc.want {
				t.Fatalf("EffectiveProjectAdmissionMode(%q) = %q, want %q", tc.raw, got, tc.want)
			}
			wantAll := tc.want == ProjectAdmissionModeAllRouted
			if got := cfg.AdmitsAnyRoutedProject(); got != wantAll {
				t.Fatalf("AdmitsAnyRoutedProject(%q) = %v, want %v", tc.raw, got, wantAll)
			}
		})
	}

	if got := (*Config)(nil).EffectiveProjectAdmissionMode(); got != ProjectAdmissionModeEnumerated {
		t.Fatalf("nil config mode = %q, want %q", got, ProjectAdmissionModeEnumerated)
	}
}

func TestValidateConfigRejectsUnknownAdmissionMode(t *testing.T) {
	t.Parallel()

	base := func() *Config {
		return &Config{
			Machine:      MachineConfig{ID: "machine-1"},
			Orchestrator: OrchestratorConfig{URL: "https://example.test"},
		}
	}

	for _, mode := range []string{"", "enumerated", "all-routed", "ALL-ROUTED"} {
		cfg := base()
		cfg.ProjectAdmissionMode = mode
		if err := validateConfig(cfg); err != nil {
			t.Fatalf("validateConfig(mode=%q) = %v, want nil", mode, err)
		}
	}

	cfg := base()
	cfg.ProjectAdmissionMode = "all_routed"
	err := validateConfig(cfg)
	if err == nil {
		t.Fatal("validateConfig(mode=all_routed) = nil, want an error naming the valid modes")
	}
	if !strings.Contains(err.Error(), "all_routed") || !strings.Contains(err.Error(), ProjectAdmissionModeAllRouted) {
		t.Fatalf("error %q must quote the bad value and the valid modes", err)
	}
}

func TestSpawnerAllRoutedAdmitsUnlistedProject(t *testing.T) {
	t.Parallel()

	const unlisted = "proj_never_enabled_here"

	t.Run("enumerated refuses", func(t *testing.T) {
		t.Parallel()
		s := &WorkerSpawner{
			opts:                   SpawnerOptions{EnabledProjectIDs: []string{"proj_other"}},
			extraEnabledProjectIDs: map[string]struct{}{},
		}
		if s.isProjectAllowedLocked(unlisted) {
			t.Fatal("enumerated mode admitted a project that is not in enabledProjectIds")
		}
	})

	t.Run("all-routed admits", func(t *testing.T) {
		t.Parallel()
		s := &WorkerSpawner{
			opts: SpawnerOptions{
				EnabledProjectIDs:    []string{"proj_other"},
				ProjectAdmissionMode: ProjectAdmissionModeAllRouted,
			},
			extraEnabledProjectIDs: map[string]struct{}{},
		}
		if !s.isProjectAllowedLocked(unlisted) {
			t.Fatal("all-routed mode refused a routed project; the whole point is that it admits without a per-project entry")
		}
	})

	t.Run("all-routed still refuses a blank id", func(t *testing.T) {
		t.Parallel()
		s := &WorkerSpawner{
			opts:                   SpawnerOptions{ProjectAdmissionMode: ProjectAdmissionModeAllRouted},
			extraEnabledProjectIDs: map[string]struct{}{},
		}
		// A blank project id is a malformed spec, not a routing decision:
		// admitting it would let repository-only dispatch skip resolution.
		if s.isProjectAllowedLocked("") {
			t.Fatal("all-routed admitted an empty project id")
		}
	})
}

func TestAcceptWorkHonoursAllRoutedMode(t *testing.T) {
	t.Parallel()

	const routed = "proj_routed_but_not_listed"

	spawner := NewWorkerSpawner(SpawnerOptions{
		EnabledProjectIDs:    []string{},
		ProjectAdmissionMode: ProjectAdmissionModeAllRouted,
	})
	t.Cleanup(func() { _ = spawner.Drain(5 * time.Second) })

	handle, err := spawner.AcceptWork(SessionSpec{SessionID: "sess-all-routed", ProjectID: routed})
	if err != nil {
		t.Fatalf("AcceptWork under all-routed = %v, want admission", err)
	}
	if handle == nil {
		t.Fatal("AcceptWork returned a nil handle under all-routed")
	}
}

func TestAcceptWorkEnumeratedStillRefusesUnlisted(t *testing.T) {
	t.Parallel()

	spawner := NewWorkerSpawner(SpawnerOptions{EnabledProjectIDs: []string{"proj_allowed"}})
	t.Cleanup(func() { _ = spawner.Drain(5 * time.Second) })

	_, err := spawner.AcceptWork(SessionSpec{SessionID: "sess-denied", ProjectID: "proj_denied"})
	if err == nil {
		t.Fatal("AcceptWork admitted an unlisted project under the enumerated default")
	}
	if !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("error %q should say the project is not allowed", err)
	}
}

func TestSetProjectAdmissionModeAppliesWithoutRestart(t *testing.T) {
	t.Parallel()

	spawner := NewWorkerSpawner(SpawnerOptions{EnabledProjectIDs: []string{"proj_allowed"}})
	t.Cleanup(func() { _ = spawner.Drain(5 * time.Second) })

	if got := spawner.ProjectAdmissionMode(); got != ProjectAdmissionModeEnumerated {
		t.Fatalf("initial mode = %q, want %q", got, ProjectAdmissionModeEnumerated)
	}
	if _, err := spawner.AcceptWork(SessionSpec{SessionID: "s1", ProjectID: "proj_new"}); err == nil {
		t.Fatal("enumerated spawner admitted proj_new before the mode changed")
	}

	// This is the yaml-watcher path: the operator edits daemon.yaml, the
	// watcher reloads, and the running daemon must honour the new consent
	// immediately. Requiring a restart here is the founder-reported defect.
	spawner.SetProjectAdmissionMode(ProjectAdmissionModeAllRouted)

	if got := spawner.ProjectAdmissionMode(); got != ProjectAdmissionModeAllRouted {
		t.Fatalf("mode after set = %q, want %q", got, ProjectAdmissionModeAllRouted)
	}
	if _, err := spawner.AcceptWork(SessionSpec{SessionID: "s2", ProjectID: "proj_new"}); err != nil {
		t.Fatalf("AcceptWork after mode flip = %v, want admission without a restart", err)
	}
}

func TestAdmissionHashCoversModeAndEnabledIDs(t *testing.T) {
	t.Parallel()

	entries := []ProjectAllowlistEntry{{ID: "proj_a", Repository: "https://example.test/a.git"}}

	// The defect this pins: a project enabled with NO repository resource left
	// the entries list untouched, so the entries-only hash never changed and
	// the orchestrator never learned about it.
	withoutEnabled := admissionHash(ProjectAdmissionReport{Entries: entries})
	withEnabled := admissionHash(ProjectAdmissionReport{
		EnabledProjectIDs: []string{"proj_repoless"},
		Entries:           entries,
	})
	if withoutEnabled == withEnabled {
		t.Fatal("enabling a project with no repository did not change the admission hash — the orchestrator can never learn about it")
	}

	withAllRouted := admissionHash(ProjectAdmissionReport{
		Mode:    ProjectAdmissionModeAllRouted,
		Entries: entries,
	})
	if withAllRouted == withoutEnabled {
		t.Fatal("flipping the admission mode did not change the admission hash")
	}

	// Order and whitespace must not churn the hash, or every beat would ship a
	// full payload.
	a := admissionHash(ProjectAdmissionReport{EnabledProjectIDs: []string{"proj_b", " proj_a "}})
	b := admissionHash(ProjectAdmissionReport{EnabledProjectIDs: []string{"proj_a", "proj_b"}})
	if a != b {
		t.Fatalf("admission hash is order/whitespace sensitive: %q vs %q", a, b)
	}

	// Empty stays the established "daemon did not report" signal.
	if got := admissionHash(ProjectAdmissionReport{}); got != "" {
		t.Fatalf("empty report hash = %q, want empty string", got)
	}
	if got := admissionHash(ProjectAdmissionReport{Mode: ProjectAdmissionModeEnumerated}); got != "" {
		t.Fatalf("default-mode empty report hash = %q, want empty string", got)
	}
	// ...but an all-routed daemon with nothing else to say IS reporting.
	if got := admissionHash(ProjectAdmissionReport{Mode: ProjectAdmissionModeAllRouted}); got == "" {
		t.Fatal("an all-routed daemon with no projects reported nothing; the platform then cannot tell it admits everything")
	}

	// Domain separation: an id must not be able to impersonate a repository.
	split := admissionHash(ProjectAdmissionReport{EnabledProjectIDs: []string{"x"}, Entries: nil})
	joined := admissionHash(ProjectAdmissionReport{EnabledProjectIDs: nil, Entries: []ProjectAllowlistEntry{{ID: "x", Repository: ""}}})
	if split == joined {
		t.Fatal("enabled ids and allowlist entries collide in the hash")
	}
}

func TestProjectAdmissionModeConstantsMatchDaemon(t *testing.T) {
	t.Parallel()

	// afclient cannot import daemon (daemon imports afclient), so the two
	// constant pairs are duplicated. If they ever drift, the CLI writes a mode
	// string the daemon reads as "enumerated" and consent silently narrows.
	if afclient.ProjectAdmissionModeEnumerated != ProjectAdmissionModeEnumerated {
		t.Fatalf("enumerated constant drift: afclient=%q daemon=%q",
			afclient.ProjectAdmissionModeEnumerated, ProjectAdmissionModeEnumerated)
	}
	if afclient.ProjectAdmissionModeAllRouted != ProjectAdmissionModeAllRouted {
		t.Fatalf("all-routed constant drift: afclient=%q daemon=%q",
			afclient.ProjectAdmissionModeAllRouted, ProjectAdmissionModeAllRouted)
	}
	if afclient.NormalizeProjectAdmissionMode("All-Routed") != normalizeProjectAdmissionMode("All-Routed") {
		t.Fatal("afclient and daemon normalize the same mode string differently")
	}
	if afclient.NormalizeProjectAdmissionMode("all_routed") != normalizeProjectAdmissionMode("all_routed") {
		t.Fatal("afclient and daemon disagree on a misspelled mode")
	}
}

func TestConfigRoundTripsAdmissionMode(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")

	cfg := &Config{
		APIVersion:              "donmai.dev/v1",
		Kind:                    "LocalDaemon",
		ProjectAdmissionVersion: ProjectAdmissionVersionV2,
		ProjectAdmissionMode:    ProjectAdmissionModeAllRouted,
		Machine:                 MachineConfig{ID: "machine-1"},
		Orchestrator:            OrchestratorConfig{URL: "https://example.test"},
	}
	if err := WriteConfig(path, cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	raw, err := os.ReadFile(path) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), "projectAdmissionMode: all-routed") {
		t.Fatalf("written config does not carry the mode:\n%s", raw)
	}

	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !loaded.AdmitsAnyRoutedProject() {
		t.Fatal("round-tripped config lost its all-routed consent")
	}

	// A config that never opted in must not gain the key on write — otherwise
	// every save rewrites every operator's file.
	plain := filepath.Join(dir, "plain.yaml")
	if err := WriteConfig(plain, &Config{
		APIVersion:   "donmai.dev/v1",
		Kind:         "LocalDaemon",
		Machine:      MachineConfig{ID: "machine-1"},
		Orchestrator: OrchestratorConfig{URL: "https://example.test"},
	}); err != nil {
		t.Fatalf("WriteConfig(plain): %v", err)
	}
	plainRaw, err := os.ReadFile(plain) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatalf("ReadFile(plain): %v", err)
	}
	if strings.Contains(string(plainRaw), "projectAdmissionMode") {
		t.Fatalf("a config with no admission opinion gained the key:\n%s", plainRaw)
	}
}

func TestRegisterRequestCarriesAdmissionMode(t *testing.T) {
	t.Parallel()

	body, err := json.Marshal(RegisterRequest{ProjectAdmissionMode: ProjectAdmissionModeAllRouted})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), `"projectAdmissionMode":"all-routed"`) {
		t.Fatalf("RegisterRequest did not serialize the admission mode: %s", body)
	}

	// Absent must stay absent so an older platform sees no new key at all.
	body, err = json.Marshal(RegisterRequest{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "projectAdmissionMode") {
		t.Fatalf("empty RegisterRequest emitted an admission-mode key: %s", body)
	}
}

// TestHeartbeatPropagatesAdmissionWithoutRestart is the regression test for the
// founder-reported symptom "enable the project, then restart the daemon before
// the platform believes you".
//
// The daemon used to report only the REPOSITORY projection of its admission
// state on the heartbeat. Enabling a project that has no repository resource
// therefore left the reported entries — and their hash — byte-identical, so the
// orchestrator kept its stale copy. The enabled set travelled only on
// registration, which happens once, at process start. Hence the restart.
func TestHeartbeatPropagatesAdmissionWithoutRestart(t *testing.T) {
	t.Parallel()

	var (
		mu       sync.Mutex
		payloads []HeartbeatPayload
		report   = ProjectAdmissionReport{
			Mode:              ProjectAdmissionModeEnumerated,
			EnabledProjectIDs: []string{"proj_a"},
			Entries:           []ProjectAllowlistEntry{{ID: "proj_a", Repository: "https://example.test/a.git"}},
		}
	)

	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID: "w1", Hostname: "h", IntervalSeconds: 1,
		GetActiveCount: func() int { return 0 },
		GetMaxCount:    func() int { return 1 },
		GetStatus:      func() RegistrationStatus { return RegistrationIdle },
		GetProjectAdmission: func() ProjectAdmissionReport {
			mu.Lock()
			defer mu.Unlock()
			out := report
			out.EnabledProjectIDs = append([]string(nil), report.EnabledProjectIDs...)
			out.Entries = append([]ProjectAllowlistEntry(nil), report.Entries...)
			return out
		},
		OnHeartbeat: func(p HeartbeatPayload) {
			mu.Lock()
			payloads = append(payloads, p)
			mu.Unlock()
		},
	})

	hs.sendOne(context.Background())
	hs.sendOne(context.Background())

	mu.Lock()
	first, second := payloads[0], payloads[1]
	mu.Unlock()

	if first.ProjectAdmissionMode != ProjectAdmissionModeEnumerated {
		t.Fatalf("first beat mode = %q, want %q", first.ProjectAdmissionMode, ProjectAdmissionModeEnumerated)
	}
	if len(first.EnabledProjectIDs) != 1 || first.EnabledProjectIDs[0] != "proj_a" {
		t.Fatalf("first beat enabled ids = %v, want [proj_a]", first.EnabledProjectIDs)
	}
	if second.EnabledProjectIDs != nil || second.ProjectAdmissionMode != "" {
		t.Fatalf("unchanged beat re-sent the full admission report: %+v", second)
	}

	// Enable a project that has NO repository resource. Entries do not move.
	mu.Lock()
	report.EnabledProjectIDs = []string{"proj_a", "proj_repoless"}
	mu.Unlock()
	hs.sendOne(context.Background())

	mu.Lock()
	third := payloads[2]
	mu.Unlock()

	if third.AllowlistHash == first.AllowlistHash {
		t.Fatal("enabling a repository-less project did not move the hash — the orchestrator would never learn about it, and only a daemon restart would fix it")
	}
	if len(third.EnabledProjectIDs) != 2 {
		t.Fatalf("beat after enable carried ids %v, want both projects", third.EnabledProjectIDs)
	}
	if len(third.Allowlist) != 1 {
		t.Fatalf("beat after enable carried entries %v, want the unchanged single repository entry", third.Allowlist)
	}

	// Flipping consent mode must also reach the wire on the next beat.
	mu.Lock()
	report.Mode = ProjectAdmissionModeAllRouted
	mu.Unlock()
	hs.sendOne(context.Background())

	mu.Lock()
	fourth := payloads[3]
	mu.Unlock()

	if fourth.AllowlistHash == third.AllowlistHash {
		t.Fatal("flipping to all-routed did not move the hash")
	}
	if fourth.ProjectAdmissionMode != ProjectAdmissionModeAllRouted {
		t.Fatalf("beat after mode flip reported %q, want %q", fourth.ProjectAdmissionMode, ProjectAdmissionModeAllRouted)
	}
}

// TestHeartbeatLegacyAllowlistCallbackStillWorks pins that an embedder wired to
// the older entries-only callback keeps working unchanged.
func TestHeartbeatLegacyAllowlistCallbackStillWorks(t *testing.T) {
	t.Parallel()

	var (
		mu       sync.Mutex
		payloads []HeartbeatPayload
	)

	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID: "w1", Hostname: "h", IntervalSeconds: 1,
		GetActiveCount: func() int { return 0 },
		GetMaxCount:    func() int { return 1 },
		GetStatus:      func() RegistrationStatus { return RegistrationIdle },
		GetAllowlist: func() []ProjectAllowlistEntry {
			return []ProjectAllowlistEntry{{ID: "proj_a", Repository: "https://example.test/a.git"}}
		},
		OnHeartbeat: func(p HeartbeatPayload) {
			mu.Lock()
			payloads = append(payloads, p)
			mu.Unlock()
		},
	})

	hs.sendOne(context.Background())

	mu.Lock()
	first := payloads[0]
	mu.Unlock()

	if first.AllowlistHash == "" {
		t.Fatal("legacy callback produced no allowlist hash")
	}
	if len(first.Allowlist) != 1 {
		t.Fatalf("legacy callback allowlist = %v, want one entry", first.Allowlist)
	}
	if first.ProjectAdmissionMode != ProjectAdmissionModeEnumerated {
		t.Fatalf("legacy callback reported mode %q, want the enumerated default", first.ProjectAdmissionMode)
	}
	if first.EnabledProjectIDs != nil {
		t.Fatalf("legacy callback invented enabled ids: %v", first.EnabledProjectIDs)
	}
}
