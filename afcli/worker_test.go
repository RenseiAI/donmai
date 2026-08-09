package afcli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/codesurvival"
	"github.com/RenseiAI/donmai/kgextract"
	"github.com/RenseiAI/donmai/worker"
)

// TestWorkerParentHelp verifies the worker parent command exposes the
// start subcommand via --help.
func TestWorkerParentHelp(t *testing.T) {
	t.Parallel()

	cmd := newWorkerCmd(func() afclient.DataSource { return afclient.NewMockClient() }, Config{})
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(buf.String(), "start") {
		t.Errorf("worker --help missing 'start' subcommand; got:\n%s", buf.String())
	}
}

// TestWorkerIsDeprecated is the D3 acceptance for `worker`: the parent
// command carries a Cobra Deprecated marker naming `host` as the replacement
// and a concrete removal version, never an unfalsifiable "next release"
// promise — see legacyWorkerFleetRemovalVersion's comment (fleet.go) for why
// that promise failed the previous alias generation.
func TestWorkerIsDeprecated(t *testing.T) {
	t.Parallel()

	cmd := newWorkerCmd(func() afclient.DataSource { return afclient.NewMockClient() }, Config{BinaryName: "donmai"})

	if cmd.Deprecated == "" {
		t.Fatal("worker must carry a Deprecated string")
	}
	if !strings.Contains(cmd.Deprecated, legacyWorkerFleetRemovalVersion) {
		t.Errorf("Deprecated %q must name the removal version %q", cmd.Deprecated, legacyWorkerFleetRemovalVersion)
	}
	if !strings.Contains(cmd.Deprecated, "host") {
		t.Errorf("Deprecated %q must point at `host`", cmd.Deprecated)
	}
	for _, banned := range []string{"next release", "a future release", "soon"} {
		if strings.Contains(strings.ToLower(cmd.Deprecated), banned) {
			t.Errorf("Deprecated %q must not say %q — name a version", cmd.Deprecated, banned)
		}
	}
	if !strings.HasPrefix(legacyWorkerFleetRemovalVersion, "v") {
		t.Errorf("removal version %q must be a concrete vX.Y.Z tag", legacyWorkerFleetRemovalVersion)
	}
}

// TestWorkerStartDefaults verifies the flag defaults on `worker start`
// match the documented contract.
func TestWorkerStartDefaults(t *testing.T) {
	t.Parallel()

	cmd := newWorkerStartCmd()

	cases := []struct {
		name string
		want string
	}{
		{"max-agents", "1"},
		{"poll-interval", "5s"},
		{"heartbeat-interval", "30s"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := cmd.Flags().Lookup(tc.name)
			if f == nil {
				t.Fatalf("flag %q not registered", tc.name)
			}
			if f.DefValue != tc.want {
				t.Errorf("flag %q default = %q, want %q", tc.name, f.DefValue, tc.want)
			}
		})
	}
}

// TestResolveWorkerToken covers flag > env > error precedence.
func TestResolveWorkerToken(t *testing.T) {
	tests := []struct {
		name    string
		flag    string
		env     string
		want    string
		wantErr bool
	}{
		{"flag_wins", "flag-token", "env-token", "flag-token", false},
		{"env_fallback", "", "env-token", "env-token", false},
		{"both_empty_errors", "", "", "", true},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AF_PROVISIONING_TOKEN", tc.env)
			got, err := resolveWorkerToken(tc.flag)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if !strings.Contains(err.Error(), "provisioning token required") {
					t.Errorf("error missing expected phrase: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveWorkerBaseURL covers flag > env > default precedence.
func TestResolveWorkerBaseURL(t *testing.T) {
	tests := []struct {
		name string
		flag string
		env  string
		want string
	}{
		{"flag_wins", "https://flag.example", "https://env.example", "https://flag.example"},
		{"env_fallback", "", "https://env.example", "https://env.example"},
		{"default_fallback", "", "", defaultWorkerBaseURL},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AF_BASE_URL", tc.env)
			if got := resolveWorkerBaseURL(tc.flag); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWorkerStartMissingToken verifies that invoking `worker start` with
// no token and no env var returns a helpful error without spawning any
// process.
func TestWorkerStartMissingToken(t *testing.T) {
	t.Setenv("AF_PROVISIONING_TOKEN", "")

	err := runWorkerStart(&workerStartFlags{
		pollInterval:      5 * time.Second,
		heartbeatInterval: 30 * time.Second,
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "provisioning token required") {
		t.Errorf("error missing expected phrase: %v", err)
	}
}

// TestConfigureWorkerLogging is a smoke test — it must not panic for
// any flag combination.
func TestConfigureWorkerLogging(t *testing.T) {
	cases := []struct {
		name         string
		debug, quiet bool
	}{
		{"default", false, false},
		{"debug", true, false},
		{"quiet", false, true},
		{"debug_and_quiet", true, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(_ *testing.T) {
			configureWorkerLogging(tc.debug, tc.quiet)
		})
	}
}

// TestWorkerStartCapabilities verifies `donmai worker start` always advertises
// BOTH batch capabilities it runs in-process — the code-survival scan and the
// kg-extraction lane — on top of whatever the operator passed, deduped. A lane
// this process executes but does not advertise never receives work; a lane it
// advertises but does not execute has its work claimed and dropped.
func TestWorkerStartCapabilities(t *testing.T) {
	kgLane := kgextract.NewLane(kgextract.Options{})

	cases := []struct {
		name     string
		operator []string
		want     []string
	}{
		{"empty_operator_gets_both_lanes", nil, []string{codesurvival.WorkTypeCodeSurvivalScan, kgLane.Capability}},
		{"operator_preserved_lanes_appended", []string{"gpu"}, []string{"gpu", codesurvival.WorkTypeCodeSurvivalScan, kgLane.Capability}},
		{
			"dedupe_when_operator_already_has_them",
			[]string{codesurvival.WorkTypeCodeSurvivalScan, kgLane.Capability},
			[]string{codesurvival.WorkTypeCodeSurvivalScan, kgLane.Capability},
		},
		{"drops_empty_tags", []string{"", "gpu", ""}, []string{"gpu", codesurvival.WorkTypeCodeSurvivalScan, kgLane.Capability}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := worker.MergeCapabilities(tc.operator, codesurvival.WorkTypeCodeSurvivalScan, kgLane.Capability)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("capabilities(%v) = %v, want %v", tc.operator, got, tc.want)
			}
		})
	}
}

// TestBatchHandlerMux_RoutesByWorkType pins the fan-out `donmai worker start`
// wires: each batch lane reaches its OWN executor, and an item for a work-type
// this binary does not know is skipped instead of being handed to the wrong
// decoder (or crashing the poll loop).
func TestBatchHandlerMux_RoutesByWorkType(t *testing.T) {
	cases := []struct {
		name         string
		workType     string
		wantSurvival bool
		wantKG       bool
	}{
		{"survival_item", codesurvival.WorkTypeCodeSurvivalScan, true, false},
		{"kg_item", kgextract.WorkTypeKGExtraction, false, true},
		{"unknown_item", "some-future-work-type", false, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var survivalHits, kgHits int
			mux := batchHandlerMux(
				func(context.Context, worker.BatchWorkItem) error { survivalHits++; return nil },
				func(context.Context, worker.BatchWorkItem) error { kgHits++; return nil },
			)
			if err := mux(context.Background(), worker.BatchWorkItem{WorkType: tc.workType}); err != nil {
				t.Fatalf("mux(%q) = %v, want nil", tc.workType, err)
			}
			if got := survivalHits > 0; got != tc.wantSurvival {
				t.Errorf("survival handler called = %v, want %v", got, tc.wantSurvival)
			}
			if got := kgHits > 0; got != tc.wantKG {
				t.Errorf("kg handler called = %v, want %v", got, tc.wantKG)
			}
		})
	}
}
