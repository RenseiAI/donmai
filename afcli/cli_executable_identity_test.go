package afcli

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runner"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

func TestAgentRunProducerForwardsCLIExecutableName(t *testing.T) {
	configured := agentRunOptions(Config{BinaryName: "sample-cli"}, binaryName(Config{BinaryName: "sample-cli"}))
	child := runner.Options{Registry: runner.NewRegistry(), WorktreeManager: &worktree.Manager{}, Poster: &result.Poster{}}
	applyAgentRunCapabilityOptions(&child, configured)
	if child.CLIExecutableName != "sample-cli" {
		t.Fatalf("child CLI executable name = %q", child.CLIExecutableName)
	}

	invalid := agentRunOptions(Config{BinaryName: "sample/cli"}, binaryName(Config{BinaryName: "sample/cli"}))
	applyAgentRunCapabilityOptions(&child, invalid)
	if _, err := runner.New(child); err == nil || err.Error() != "runner: CLI executable name is malformed: must match [A-Za-z0-9][A-Za-z0-9._-]{0,127}" {
		t.Fatalf("actual agent producer did not reach runner validation: %v", err)
	}
}

func TestAgentRunConfiguredCLIExecutableNameReachesActualSpawn(t *testing.T) {
	original := buildRegistryForAgentRun
	t.Cleanup(func() { buildRegistryForAgentRun = original })
	provider := &specDecoratorTestProvider{name: agent.ProviderStub}
	buildRegistryForAgentRun = func(_ *slog.Logger, _ agentRunCtorHints, agentBin string) *runner.Registry {
		if agentBin != "sample-cli" {
			t.Fatalf("registry producer binary = %q", agentBin)
		}
		reg := runner.NewRegistry()
		if err := reg.Register(provider); err != nil {
			t.Fatal(err)
		}
		return reg
	}

	stdout, _ := runSpecDecoratorAgentRunCmd(t, Config{BinaryName: "sample-cli"})
	spawned, err := json.Marshal(provider.spawnSpec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(spawned), "sample-cli linear") || strings.Contains(string(spawned), "donmai linear") {
		t.Fatalf("actual spawn has wrong executable identity (stdout=%q): %s", stdout, spawned)
	}
}

func TestDaemonProviderViewUsesConfiguredCLIExecutableNameInPreparedPrompt(t *testing.T) {
	original := daemonRegistryBuilder
	t.Cleanup(func() { daemonRegistryBuilder = original })
	daemonRegistryBuilder = func(_ *slog.Logger, _ agent.ExtensionDecorator) *runner.Registry {
		reg := runner.NewRegistry()
		if err := reg.Register(daemonDecoratorFakeProvider{}); err != nil {
			t.Fatal(err)
		}
		return reg
	}

	qw := runner.QueuedWork{}
	qw.SessionID = "daemon-cli-identity"
	qw.IssueIdentifier = "EX-4"
	qw.Body = "exercise the daemon prompt producer"
	qw.ResolvedProfile = runner.ResolvedProfile{
		Harness: string(testFakeDecoratorHarnessName), Model: "fake-model",
		Endpoint: &agent.EndpointBinding{
			Company: "fake", Model: "fake-model", Protocol: agent.ProtoStub, Host: agent.HostLocal,
			EndpointID: "fake-endpoint", EndpointOperator: "fake", EndpointRevision: "2026-08-06", ModelAuthor: "fake",
			AuthBindingID: "fake-auth", AuthAuthority: "fake", AuthCommercialMode: string(executioncell.CommercialUsageBilled),
			AuthBindingScope: string(executioncell.ScopeProcess), AuthPortability: string(executioncell.Portable),
			AuthDelivery: string(executioncell.DeliveryEnvironment), Mechanism: agent.AuthAPIKey,
		},
	}
	qw = attachAdmittedExecutionCellForTest(t, qw, fakeDecoratorReceiptCell())
	operational, err := runner.CanonicalOperationalPayload(qw)
	if err != nil {
		t.Fatal(err)
	}
	qw.OperationalPayload = operational
	detail, err := json.Marshal(map[string]any{
		"sessionId": qw.SessionID, "workerId": qw.WorkerID, "admissionReceipt": qw.AdmissionReceipt,
		"effectiveCell": qw.EffectiveCell, "executionRuntimeBinding": qw.ExecutionRuntimeBinding,
		"operationalPayload": qw.OperationalPayload,
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := daemonProviderView(Config{BinaryName: "sample-cli"}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := view.PreflightExecution(detail)
	if err != nil {
		t.Fatalf("PreflightExecution: %v receipt=%s", err, receipt)
	}
	host, err := executioncell.DecodeHostAdaptationReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var explicitPlan agent.PreparedHarness
	if err := json.Unmarshal(host.Plan, &explicitPlan); err != nil {
		t.Fatal(err)
	}
	defaultView, err := daemonProviderView(Config{}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defaultReceipt, err := defaultView.PreflightExecution(detail)
	if err != nil {
		t.Fatal(err)
	}
	defaultHost, err := executioncell.DecodeHostAdaptationReceipt(defaultReceipt)
	if err != nil {
		t.Fatal(err)
	}
	var defaultPlan agent.PreparedHarness
	if err := json.Unmarshal(defaultHost.Plan, &defaultPlan); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"prompt", "promptPlan", "systemPromptAppend"} {
		if explicitPlan.AuthorityFieldDigests[field] == defaultPlan.AuthorityFieldDigests[field] {
			t.Fatalf("daemon BinaryName did not alter prepared %s authority", field)
		}
	}

	if _, err := daemonProviderView(Config{BinaryName: "sample/cli"}, quietLogger()); err == nil || !strings.Contains(err.Error(), "CLI executable name is malformed") {
		t.Fatalf("actual daemon producer bypassed validation: %v", err)
	}
}
