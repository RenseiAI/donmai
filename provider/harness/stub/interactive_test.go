package stub

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/stub/stubagent"
)

func newTestProvider(t *testing.T, opts ...Option) *provider {
	t.Helper()
	built, err := New(opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	typed, ok := built.(*provider)
	if !ok {
		t.Fatalf("New returned %T, want *provider", built)
	}
	return typed
}

// TestManifestDeclaresTheInteractiveSpawnMode pins the declaration the runner
// reads. The interactive mode is selected from the LIVE manifest, so a profile
// that goes missing must fail here rather than at spawn time on a real host.
func TestManifestDeclaresTheInteractiveSpawnMode(t *testing.T) {
	t.Parallel()

	manifest := newTestProvider(t).Manifest()
	if !manifest.Caps.SupportsInteractivePTY {
		t.Error("Caps.SupportsInteractivePTY = false; the interactive spawn mode is unreachable without it")
	}
	if manifest.Caps.Transport == agent.TransportPTY {
		t.Error("Transport = pty; PTY is an additional spawn mode here, not the harness's only transport")
	}

	prompt, ok := manifest.PromptProfile(agent.PromptModeHumanControlled)
	if !ok {
		t.Fatal("no prompt profile for the human-controlled mode; every interactive spawn would be denied")
	}
	if prompt.UserDelivery != agent.PromptDeliveryStubPTYSeed {
		t.Errorf("UserDelivery = %q, want %q", prompt.UserDelivery, agent.PromptDeliveryStubPTYSeed)
	}
	if prompt.ContextDelivery != agent.PromptDeliveryStubPTYSeed {
		t.Errorf("ContextDelivery = %q, want %q", prompt.ContextDelivery, agent.PromptDeliveryStubPTYSeed)
	}
	if _, ok := manifest.ToolLifecycleProfile(agent.PromptModeHumanControlled); !ok {
		t.Fatal("no tool/lifecycle profile for the human-controlled mode; every interactive spawn would be denied")
	}
	// The headless profiles must survive the addition, or the original mode
	// silently stops working.
	if _, ok := manifest.ToolLifecycleProfile(agent.PromptModeAutonomous); !ok {
		t.Error("the autonomous tool/lifecycle profile disappeared")
	}
}

func TestStubAgentCommandResolution(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	tests := []struct {
		name     string
		opts     []Option
		specEnv  map[string]string
		procEnv  string
		wantBin  string
		wantArgv []string
	}{
		{
			name:     "default is this executable on its subcommand",
			wantBin:  self,
			wantArgv: []string{StubAgentSubcommand},
		},
		{
			name:     "spec env overrides the default",
			specEnv:  map[string]string{EnvStubAgentBin: "/opt/fake-agent"},
			wantBin:  "/opt/fake-agent",
			wantArgv: nil,
		},
		{
			name:     "process env overrides the default",
			procEnv:  "/opt/from-process",
			wantBin:  "/opt/from-process",
			wantArgv: nil,
		},
		{
			name:     "spec env wins over process env",
			specEnv:  map[string]string{EnvStubAgentBin: "/opt/from-spec"},
			procEnv:  "/opt/from-process",
			wantBin:  "/opt/from-spec",
			wantArgv: nil,
		},
		{
			name:     "explicit option wins over everything",
			opts:     []Option{WithStubAgentCommand("/opt/from-option", "run")},
			specEnv:  map[string]string{EnvStubAgentBin: "/opt/from-spec"},
			procEnv:  "/opt/from-process",
			wantBin:  "/opt/from-option",
			wantArgv: []string{"run"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// t.Setenv forbids parallel subtests, which is correct here: the
			// process environment is shared state.
			if tc.procEnv != "" {
				t.Setenv(EnvStubAgentBin, tc.procEnv)
			} else {
				t.Setenv(EnvStubAgentBin, "")
			}
			bin, argv, err := newTestProvider(t, tc.opts...).stubAgentCommand(agent.Spec{Env: tc.specEnv})
			if err != nil {
				t.Fatalf("stubAgentCommand: %v", err)
			}
			if bin != tc.wantBin {
				t.Errorf("binary = %q, want %q", bin, tc.wantBin)
			}
			if strings.Join(argv, " ") != strings.Join(tc.wantArgv, " ") {
				t.Errorf("argv = %v, want %v", argv, tc.wantArgv)
			}
		})
	}
}

func TestWithScenarioEnv(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		env     map[string]string
		config  map[string]any
		want    map[string]string
		wantErr string
	}{
		{
			name: "no scenario configured leaves the environment alone",
			env:  map[string]string{"KEEP": "me"},
			want: map[string]string{"KEEP": "me"},
		},
		{
			name:   "a JSON string is projected onto the environment",
			config: map[string]any{ScenarioConfigKey: `{"version":1,"name":"typed"}`},
			want:   map[string]string{stubagent.EnvScenario: `{"version":1,"name":"typed"}`},
		},
		{
			name:   "a decoded object is encoded",
			config: map[string]any{ScenarioConfigKey: map[string]any{"version": 1, "name": "object"}},
			want:   map[string]string{stubagent.EnvScenario: `{"name":"object","version":1}`},
		},
		{
			name:   "a scenario file path is projected",
			config: map[string]any{ScenarioFileConfigKey: " /tmp/scenario.json "},
			want:   map[string]string{stubagent.EnvScenarioFile: "/tmp/scenario.json"},
		},
		{
			// The operator who set the variable by hand is the more specific
			// authority; silently overwriting it makes a debugging session lie.
			name:   "an explicit environment entry wins",
			env:    map[string]string{stubagent.EnvScenario: `{"version":1,"name":"by-hand"}`},
			config: map[string]any{ScenarioConfigKey: `{"version":1,"name":"typed"}`},
			want:   map[string]string{stubagent.EnvScenario: `{"version":1,"name":"by-hand"}`},
		},
		{
			name:    "a malformed scenario is refused here, not handed to the child",
			config:  map[string]any{ScenarioConfigKey: `{"version":1,"steps":[{}]}`},
			wantErr: "no action set",
		},
		{
			name:    "a wrong-version scenario is refused",
			config:  map[string]any{ScenarioConfigKey: map[string]any{"version": 3}},
			wantErr: "not the supported version",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := withScenarioEnv(tc.env, tc.config)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("withScenarioEnv err = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("withScenarioEnv: %v", err)
			}
			for key, want := range tc.want {
				if got[key] != want {
					t.Errorf("env[%q] = %q, want %q", key, got[key], want)
				}
			}
			for key := range got {
				if _, expected := tc.want[key]; !expected {
					t.Errorf("env gained an unexpected key %q = %q", key, got[key])
				}
			}
		})
	}
}

// TestWithScenarioEnvDoesNotMutateTheCallersMap pins the copy. Spec.Env is
// shared with the Spec it was built from, so writing through it would leak one
// session's scenario into the next spawn from the same Spec.
func TestWithScenarioEnvDoesNotMutateTheCallersMap(t *testing.T) {
	t.Parallel()

	caller := map[string]string{"KEEP": "me"}
	if _, err := withScenarioEnv(caller, map[string]any{ScenarioConfigKey: `{"version":1}`}); err != nil {
		t.Fatalf("withScenarioEnv: %v", err)
	}
	if _, leaked := caller[stubagent.EnvScenario]; leaked {
		t.Errorf("the caller's map gained %q: %v", stubagent.EnvScenario, caller)
	}
}

// TestSpawnInteractiveRefusesAMalformedScenario pins the fail-closed edge: a
// child that exits because its scenario was garbage is indistinguishable, at
// the session layer, from a scenario that asked to exit.
func TestSpawnInteractiveRefusesAMalformedScenario(t *testing.T) {
	t.Parallel()

	built := newTestProvider(t, WithStubAgentCommand("/nonexistent/never-spawned"))
	_, err := built.Spawn(context.Background(), agent.Spec{
		Interactive:    &agent.InteractiveSpec{Cols: 80, Rows: 24},
		ProviderConfig: map[string]any{ScenarioConfigKey: `{"version":1,"steps":[{"print":"a","exit":0}]}`},
	})
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("Spawn err = %v, want one wrapping agent.ErrSpawnFailed", err)
	}
	if !strings.Contains(err.Error(), "exactly one is allowed") {
		t.Errorf("Spawn err = %v, want it to name the scenario defect", err)
	}
}

// TestResumeRefusesAnInteractiveSpec pins the refusal rather than the silent
// mode switch: a dead PTY child has nothing to resume, and handing back a
// scripted in-process handle for a session the caller asked to continue as a
// terminal is worse than saying no.
func TestResumeRefusesAnInteractiveSpec(t *testing.T) {
	t.Parallel()

	_, err := newTestProvider(t).Resume(context.Background(), "sess-1", agent.Spec{
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if !errors.Is(err, agent.ErrUnsupported) {
		t.Fatalf("Resume err = %v, want one wrapping agent.ErrUnsupported", err)
	}
}

// TestSpawnWithoutInteractiveStaysHeadless is the RED-side guard for the mode
// switch: adding the PTY mode must not have changed what a headless Spawn
// does. A headless Spawn spawns no process, so pointing the interactive child
// at a path that does not exist is harmless — unless the switch is wrong.
func TestSpawnWithoutInteractiveStaysHeadless(t *testing.T) {
	t.Parallel()

	built := newTestProvider(t, WithStubAgentCommand("/nonexistent/never-spawned"))
	handle, err := built.Spawn(context.Background(), agent.Spec{})
	if err != nil {
		t.Fatalf("headless Spawn: %v", err)
	}
	defer func() { _ = handle.Stop(context.Background()) }()

	if _, isPTY := handle.(agent.InteractiveCapable); isPTY {
		t.Error("a headless Spawn returned an interactive handle")
	}
	if handle.SessionID() == "" {
		t.Error("headless Spawn returned a handle with no session id")
	}
}
