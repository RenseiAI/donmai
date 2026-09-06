package stub

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/stub/stubagent"
)

// TestInteractiveProfileDeclaresRestrictionChannelsSatisfiedByConstruction
// pins the declaration that decides whether an interactive stub session can be
// spawned at all.
//
// The whole point is the split, so both halves are asserted in one table: the
// RESTRICTION channels (a policy over tool use) are satisfied by construction
// because the interactive child registers no tools, and the GRANT channels (a
// request to MOUNT a capability) stay Unsupported and keep denying. A change
// that flipped the grant channels to the same value would make the manifest
// claim a mount that never happens, and this table is what refuses it.
func TestInteractiveProfileDeclaresRestrictionChannelsSatisfiedByConstruction(t *testing.T) {
	t.Parallel()

	profile, ok := newTestProvider(t).Manifest().ToolLifecycleProfile(agent.PromptModeHumanControlled)
	if !ok {
		t.Fatal("no human-controlled tool/lifecycle profile; every interactive spawn would be denied")
	}

	tests := []struct {
		name  string
		got   agent.ToolDeliveryKind
		want  agent.ToolDeliveryKind
		which string
	}{
		{"allowed/disallowed tools", profile.NativeToolPolicyDelivery, agent.ToolDeliveryNoToolSurface, "restriction"},
		{"permission config", profile.PermissionConfigDelivery, agent.ToolDeliveryNoToolSurface, "restriction"},
		{"tool plugins", profile.ToolPluginDelivery, agent.ToolDeliveryUnsupported, "grant"},
		{"mcp servers", profile.MCPDelivery, agent.ToolDeliveryUnsupported, "grant"},
		{"mcp tool names", profile.MCPToolPolicyDelivery, agent.ToolDeliveryUnsupported, "grant"},
		{"tool hooks", profile.ToolHookDelivery, agent.ToolDeliveryUnsupported, "grant"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.got != tc.want {
				t.Errorf("%s channel (%s) = %q, want %q", tc.name, tc.which, tc.got, tc.want)
			}
		})
	}

	// The headless profile is a different claim entirely — its in-process
	// scripted loop DOES answer tool calls through the oracle — and must not
	// be dragged along by an edit to the interactive one.
	headless, ok := newTestProvider(t).Manifest().ToolLifecycleProfile(agent.PromptModeAutonomous)
	if !ok {
		t.Fatal("the autonomous tool/lifecycle profile disappeared")
	}
	if headless.NativeToolPolicyDelivery != agent.ToolDeliveryStubOracle {
		t.Errorf("autonomous NativeToolPolicyDelivery = %q, want %q", headless.NativeToolPolicyDelivery, agent.ToolDeliveryStubOracle)
	}
}

// TestPrepareHarnessAdmitsAPlatformComposedToolPolicy is the regression this
// change exists for, expressed as the caller actually shaped it: every
// interactive launch arrives carrying a composed disallowedTools baseline and
// no toolSurfaceRequired flag (absent means required), which is precisely the
// combination that used to deny the spawn before any process started.
func TestPrepareHarnessAdmitsAPlatformComposedToolPolicy(t *testing.T) {
	t.Parallel()

	manifest := newTestProvider(t).Manifest()
	var receipt *agent.ToolLifecycleReceipt
	spec := agent.Spec{
		Interactive:     &agent.InteractiveSpec{Cols: 80, Rows: 24},
		DisallowedTools: []string{"Bash", "Write", "WebFetch"},
		OnToolLifecycleAdapted: func(r agent.ToolLifecycleReceipt) error {
			copied := r
			receipt = &copied
			return nil
		},
	}
	prepared, err := agent.PrepareHarness(spec, manifest)
	if err != nil {
		t.Fatalf("PrepareHarness: %v", err)
	}
	if receipt == nil {
		t.Fatal("no tool/lifecycle receipt was persisted")
	}
	if receipt.Decision != "ready" {
		t.Fatalf("receipt decision = %q, want ready", receipt.Decision)
	}
	// Admitted, not stripped: the compiler must leave the policy on the Spec
	// the child is spawned from, so the record the child prints is the record
	// that arrived.
	if strings.Join(prepared.DisallowedTools, ",") != "Bash,Write,WebFetch" {
		t.Errorf("prepared DisallowedTools = %v, want the caller's list unchanged", prepared.DisallowedTools)
	}
	var found bool
	for _, entry := range receipt.Entries {
		if entry.Channel != agent.ToolChannelDisallowedTools {
			continue
		}
		found = true
		if entry.Outcome != agent.ToolOutcomeAdmitted || entry.Delivery != agent.ToolDeliveryNoToolSurface {
			t.Errorf("disallowed-tools entry = %+v, want admitted via %q", entry, agent.ToolDeliveryNoToolSurface)
		}
	}
	if !found {
		t.Fatalf("receipt does not name the disallowed-tools channel; entries=%+v", receipt.Entries)
	}
}

func TestWithToolPolicyEnv(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		env  map[string]string
		spec agent.Spec
		want string
	}{
		{
			name: "no policy sets nothing, so the child prints no line",
			spec: agent.Spec{},
			want: "",
		},
		{
			name: "both lists are recorded",
			spec: agent.Spec{AllowedTools: []string{"Read"}, DisallowedTools: []string{"Bash"}},
			want: `{"allowedTools":["Read"],"disallowedTools":["Bash"]}`,
		},
		{
			name: "a deny-list alone omits the empty allow-list",
			spec: agent.Spec{DisallowedTools: []string{"Bash", "Write"}},
			want: `{"disallowedTools":["Bash","Write"]}`,
		},
		{
			name: "an explicit environment entry wins",
			env:  map[string]string{stubagent.EnvToolPolicy: `{"allowedTools":["ByHand"]}`},
			spec: agent.Spec{DisallowedTools: []string{"Bash"}},
			want: `{"allowedTools":["ByHand"]}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := withToolPolicyEnv(tc.env, tc.spec)
			if err != nil {
				t.Fatalf("withToolPolicyEnv: %v", err)
			}
			if got[stubagent.EnvToolPolicy] != tc.want {
				t.Fatalf("env[%s] = %q, want %q", stubagent.EnvToolPolicy, got[stubagent.EnvToolPolicy], tc.want)
			}
			if tc.want == "" {
				return
			}
			// Whatever was written must decode back through the child's own
			// loader, or the record is unreadable exactly where it is needed.
			policy, err := stubagent.LoadToolPolicy(func(string) string { return got[stubagent.EnvToolPolicy] })
			if err != nil {
				t.Fatalf("the child's loader rejected what the parent wrote: %v", err)
			}
			if encoded, err := stubagent.EncodeToolPolicy(policy); err != nil || encoded != tc.want {
				t.Fatalf("round trip = %q (err %v), want %q", encoded, err, tc.want)
			}
		})
	}
}

// TestWithToolPolicyEnvDoesNotMutateTheCallersMap pins the same copy
// withScenarioEnv makes, for the same reason: Spec.Env is shared with the Spec
// it was built from, so writing through it leaks one session's record into the
// next spawn from that Spec.
func TestWithToolPolicyEnvDoesNotMutateTheCallersMap(t *testing.T) {
	t.Parallel()

	caller := map[string]string{"KEEP": "me"}
	if _, err := withToolPolicyEnv(caller, agent.Spec{DisallowedTools: []string{"Bash"}}); err != nil {
		t.Fatalf("withToolPolicyEnv: %v", err)
	}
	if _, leaked := caller[stubagent.EnvToolPolicy]; leaked {
		t.Errorf("the caller's map gained %q: %v", stubagent.EnvToolPolicy, caller)
	}
}

// TestSpawnInteractiveCarriesTheToolPolicyToTheChildProcess closes the gap
// between "the parent composed a variable" and "the child was actually
// started with it". Everything in between — PrepareHarness, ptycli.Spawn,
// ptyhost's own environment composition — is real here; only the program is
// substituted, for a script that writes its view of the variable to disk.
func TestSpawnInteractiveCarriesTheToolPolicyToTheChildProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	dir := t.TempDir()
	record := filepath.Join(dir, "policy.json")
	bin := writeEnvRecorderBinary(t, stubagent.EnvToolPolicy, record)

	built := newTestProvider(t, WithStubAgentCommand(bin))
	handle, err := built.Spawn(context.Background(), agent.Spec{
		Cwd:             dir,
		Interactive:     &agent.InteractiveSpec{Cols: 80, Rows: 24},
		DisallowedTools: []string{"Bash", "Write"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = handle.Stop(context.Background()) }()

	data := waitForFile(t, record)
	var policy stubagent.ToolPolicy
	if err := json.Unmarshal(data, &policy); err != nil {
		t.Fatalf("the child saw %q, which is not a policy: %v", data, err)
	}
	if strings.Join(policy.DisallowedTools, ",") != "Bash,Write" {
		t.Fatalf("child saw DisallowedTools %v, want [Bash Write]", policy.DisallowedTools)
	}
	if notice := policy.Notice(); !strings.Contains(notice, "Bash,Write") ||
		!strings.Contains(notice, "honoured by construction") {
		t.Fatalf("notice = %q, want it to name the deny-list and the reason", notice)
	}
}

// writeEnvRecorderBinary returns a script that writes one environment
// variable's value to path and then idles, so the spawn under test stays alive
// long enough to be stopped by the test rather than racing its own teardown.
func writeEnvRecorderBinary(t *testing.T, variable, path string) string {
	t.Helper()
	bin := writeNoopBinary(t) // skips when bash is unavailable
	script := "#!/bin/bash\nprintf '%s' \"${" + variable + "}\" > '" + path + "'\nsleep 30\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec // test fixture needs the exec bit
		t.Fatal(err)
	}
	return bin
}

// waitForFile polls until the child has written its record. The child is a
// real process under a real PTY, so the write is genuinely concurrent with the
// assertion; polling a short deadline is what keeps this from being a sleep.
func waitForFile(t *testing.T, path string) []byte {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, err := os.ReadFile(path) //nolint:gosec // test-owned temp path
		if err == nil && len(data) > 0 {
			return data
		}
		if time.Now().After(deadline) {
			t.Fatalf("the child never recorded %s (last error: %v)", path, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
