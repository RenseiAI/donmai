package stub

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
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
			// The override names a donmai binary, so the subcommand rides
			// along. Without it the child runs a bare `donmai`, prints help
			// and exits 0 — which every layer above reads as a clean session
			// that ran no scenario at all.
			name:     "the environment override still gets the subcommand",
			procEnv:  "/opt/other-build/donmai",
			wantBin:  "/opt/other-build/donmai",
			wantArgv: []string{StubAgentSubcommand},
		},
		{
			name:     "an explicit option wins over the environment and keeps its own argv",
			opts:     []Option{WithStubAgentCommand("/opt/from-option", "run")},
			procEnv:  "/opt/from-process",
			wantBin:  "/opt/from-option",
			wantArgv: []string{"run"},
		},
		{
			name:     "an option with no argv runs the binary bare",
			opts:     []Option{WithStubAgentCommand("/opt/fake-agent")},
			wantBin:  "/opt/fake-agent",
			wantArgv: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// t.Setenv forbids parallel subtests, which is correct here: the
			// process environment is shared state.
			t.Setenv(EnvStubAgentBin, tc.procEnv)
			bin, argv, err := newTestProvider(t, tc.opts...).stubAgentCommand()
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

// TestStubAgentBinaryIsResolvedAtConstruction pins the property that separates
// this from a per-Spec lookup: the environment is read ONCE, by New, exactly as
// codex reads $CODEX_BIN and pi reads $PI_BIN. A provider handed out by a
// shared registry must execute the same program for every caller, so a later
// change to the environment cannot move it.
//
// The Spec half of that property is enforced by the signature rather than by a
// test: stubAgentCommand takes no arguments, so a caller-supplied Spec has no
// channel through which to name the binary this host executes.
func TestStubAgentBinaryIsResolvedAtConstruction(t *testing.T) {
	t.Setenv(EnvStubAgentBin, "/opt/at-construction")
	built := newTestProvider(t)

	t.Setenv(EnvStubAgentBin, "/opt/changed-afterwards")
	bin, argv, err := built.stubAgentCommand()
	if err != nil {
		t.Fatalf("stubAgentCommand: %v", err)
	}
	if bin != "/opt/at-construction" {
		t.Errorf("binary = %q, want the value present at New (%q)", bin, "/opt/at-construction")
	}
	if strings.Join(argv, " ") != StubAgentSubcommand {
		t.Errorf("argv = %v, want [%s]", argv, StubAgentSubcommand)
	}
}

func TestWithScenarioEnv(t *testing.T) {
	t.Parallel()

	// A real file, because withScenarioEnv now reads the one it is about to
	// forward rather than passing the path along on trust.
	scenarioFile := filepath.Join(t.TempDir(), "scenario.json")
	if err := os.WriteFile(scenarioFile, []byte(`{"version":1,"name":"on-disk"}`), 0o600); err != nil {
		t.Fatal(err)
	}

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
			name:   "a scenario file path is projected, trimmed",
			config: map[string]any{ScenarioFileConfigKey: " " + scenarioFile + " "},
			want:   map[string]string{stubagent.EnvScenarioFile: scenarioFile},
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

// TestSpawnInteractiveSeedFailureRetractsTheReadyReceipt is the failure branch
// the deliverSeed seam exists to reach.
//
// The order that makes this matter: PrepareHarness adapts the prompt and
// persists a READY receipt, and only then are the bytes written into the
// terminal. When that write fails the spawn is abandoned — so without a
// retraction the durable record says a prompt was delivered to a session that
// never existed. Mirrors shell's own coverage of the same window.
func TestSpawnInteractiveSeedFailureRetractsTheReadyReceipt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	bin := writeNoopBinary(t)

	tests := []struct {
		name string
		err  error
	}{
		{name: "partial write", err: errors.New("partial PTY seed")},
		{name: "write error", err: errors.New("PTY closed")},
		{name: "cancelled", err: context.Canceled},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan := agent.PromptPlan{
				ContractVersion:  agent.PromptContractVersion,
				BaseInstructions: agent.BaseInstructionPlan{Strategy: agent.BaseInstructionsPreserve},
				UserPrompt:       agent.PromptContent{ID: "interactive-user-task", Text: "seed", Required: true},
			}
			var receipts []agent.PromptDeliveryReceipt

			built := newTestProvider(t, WithStubAgentCommand(bin))
			built.deliverSeed = func(context.Context, agent.Handle, agent.InteractiveSession, string) error {
				return tc.err
			}

			handle, err := built.Spawn(context.Background(), agent.Spec{
				Cwd:         t.TempDir(),
				Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
				PromptPlan:  &plan,
				OnPromptAdapted: func(receipt agent.PromptDeliveryReceipt) error {
					receipts = append(receipts, receipt)
					return nil
				},
			})
			if handle != nil {
				t.Fatal("Spawn returned a usable session after a failed seed")
			}
			if !errors.Is(err, agent.ErrSpawnFailed) || !strings.Contains(err.Error(), tc.err.Error()) {
				t.Fatalf("Spawn error = %v, want ErrSpawnFailed containing %q", err, tc.err)
			}
			if len(receipts) < 2 {
				t.Fatalf("receipts = %+v, want the ready decision followed by an application denial", receipts)
			}
			final := receipts[len(receipts)-1]
			if final.Decision != "denied" {
				t.Fatalf("final receipt decision = %q, want denied", final.Decision)
			}
			for _, entry := range final.Entries {
				if entry.ID != "interactive-user-task" {
					continue
				}
				if entry.Outcome != agent.PromptOutcomeDenied || entry.DenialCode != agent.PromptDenialApplicationFailed {
					t.Fatalf("final user-task receipt = %+v, want an application_failed denial", entry)
				}
			}
		})
	}
}

// TestSpawnInteractiveValidatesTheScenarioFile closes the gap between the two
// scenario forms. The inline form is refused at spawn precisely so a child that
// exits on garbage is never mistaken for one that was ASKED to exit; a missing
// or malformed file produces that same indistinguishable exit, so it is checked
// on the same path.
func TestSpawnInteractiveValidatesTheScenarioFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	if err := os.WriteFile(good, []byte(`{"version":1,"name":"from-file"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"version":1,"steps":[{}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	oversize := filepath.Join(dir, "oversize.json")
	if err := os.WriteFile(oversize, make([]byte, MaxScenarioFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	// A FIFO is the case that matters most: os.ReadFile on one blocks until a
	// writer appears, and nothing here will ever open it for writing. Without
	// the regular-file check this entry hangs the test — which is exactly what
	// it would do to a Spawn.
	fifo := filepath.Join(dir, "scenario.fifo")
	mkfifo(t, fifo)

	tests := []struct {
		name    string
		config  map[string]any
		env     map[string]string
		wantErr string
	}{
		{
			name:   "a valid file passes through",
			config: map[string]any{ScenarioFileConfigKey: good},
		},
		{
			name:    "a missing file is refused",
			config:  map[string]any{ScenarioFileConfigKey: filepath.Join(dir, "absent.json")},
			wantErr: "no such file",
		},
		{
			name:    "a malformed file is refused",
			config:  map[string]any{ScenarioFileConfigKey: bad},
			wantErr: "no action set",
		},
		{
			name:    "a directory is refused",
			config:  map[string]any{ScenarioFileConfigKey: dir},
			wantErr: "not a regular file",
		},
		{
			name:    "a FIFO is refused rather than blocking the spawn",
			config:  map[string]any{ScenarioFileConfigKey: fifo},
			wantErr: "not a regular file",
		},
		{
			name:    "a file over the ceiling is refused",
			config:  map[string]any{ScenarioFileConfigKey: oversize},
			wantErr: "exceeds the",
		},
		{
			name:    "a file named through the environment is checked too",
			env:     map[string]string{stubagent.EnvScenarioFile: bad},
			wantErr: "no action set",
		},
		{
			// The child reads the inline form and never opens the file, so
			// checking the file here would refuse a spawn that would have
			// worked.
			name:   "an inline scenario makes the file irrelevant",
			config: map[string]any{ScenarioConfigKey: `{"version":1,"name":"inline"}`, ScenarioFileConfigKey: bad},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := withScenarioEnv(tc.env, tc.config)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("withScenarioEnv err = %v, want one containing %q", err, tc.wantErr)
				}
				// A refusal that does not name the file it refused sends the
				// operator looking through every path in the Spec.
				if path := scenarioPathOf(tc.config, tc.env); path != "" && !strings.Contains(err.Error(), path) {
					t.Errorf("withScenarioEnv err = %v, want it to name %q", err, path)
				}
				return
			}
			if err != nil {
				t.Fatalf("withScenarioEnv: %v", err)
			}
		})
	}
}

// writeNoopBinary materializes a do-nothing executable to stand in for the
// interactive child. The seed tests never want it to produce output — the
// assertion is about what happens when the write INTO it fails.
func writeNoopBinary(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	bin := filepath.Join(t.TempDir(), "noop-agent")
	if err := os.WriteFile(bin, []byte("#!/bin/bash\nsleep 30\n"), 0o600); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	if err := os.Chmod(bin, 0o755); err != nil { //nolint:gosec // test fixture needs the exec bit
		t.Fatal(err)
	}
	return bin
}

// scenarioPathOf returns the file path a case pointed the validator at, so the
// assertions can insist every refusal names it.
func scenarioPathOf(config map[string]any, env map[string]string) string {
	if path, ok := config[ScenarioFileConfigKey].(string); ok {
		return strings.TrimSpace(path)
	}
	return env[stubagent.EnvScenarioFile]
}

// mkfifo creates a named pipe, skipping on platforms or filesystems that
// cannot. The refusal it exercises is real on every unix the daemon runs on.
func mkfifo(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("named pipes are created differently on windows")
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo %s: %v", path, err)
	}
}
