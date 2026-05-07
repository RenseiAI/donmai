package afcli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// fakeRoutingClient is the test double satisfying routingDaemonClient.
type fakeRoutingClient struct {
	configResp    *afclient.RoutingConfigResponse
	configErr     error
	explainResp   *afclient.RoutingExplainResponse
	explainErr    error
	lastSessionID string
}

func (f *fakeRoutingClient) GetRoutingConfig() (*afclient.RoutingConfigResponse, error) {
	return f.configResp, f.configErr
}

func (f *fakeRoutingClient) ExplainRouting(sessionID string) (*afclient.RoutingExplainResponse, error) {
	f.lastSessionID = sessionID
	return f.explainResp, f.explainErr
}

func newFakeRoutingFactory(c routingDaemonClient) routingClientFactory {
	return func(_ afclient.DaemonConfig) routingDaemonClient { return c }
}

// runCommand executes the given subcommand path under the routing tree
// with the given args and captures stdout. Stderr/Usage are silenced.
func runRoutingSubcommand(t *testing.T, factory routingClientFactory, args []string) (string, error) {
	t.Helper()
	cmd := newRoutingCmdWithFactory(factory)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return stdout.String(), err
}

func sampleConfigResp() *afclient.RoutingConfigResponse {
	return &afclient.RoutingConfigResponse{
		Config: afclient.RoutingConfig{
			CapturedAt: time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC),
			Weights:    afclient.RoutingWeights{Cost: 0.7, Latency: 0.3},
			SandboxProviders: []afclient.SandboxProviderState{
				{ProviderID: "local", Alpha: 1, Beta: 1},
			},
			LLMProviders: []afclient.LLMProviderState{
				{ProviderID: "claude", Alpha: 1, Beta: 1},
			},
		},
	}
}

func sampleExplainResp(sessionID string) *afclient.RoutingExplainResponse {
	return &afclient.RoutingExplainResponse{
		SessionID: sessionID,
		Decision: afclient.RoutingDecision{
			SessionID:     sessionID,
			ChosenSandbox: "local",
			ChosenLLM:     "claude",
			Score:         0.18,
			DecidedAt:     time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC),
		},
		Trace: []afclient.RoutingTraceStep{
			{Step: 1, Phase: "score", Dimension: "sandbox", Remaining: []string{"local"}, Note: "winner"},
		},
	}
}

func TestRoutingShow_Default(t *testing.T) {
	t.Parallel()
	fake := &fakeRoutingClient{configResp: sampleConfigResp()}
	out, err := runRoutingSubcommand(t, newFakeRoutingFactory(fake), []string{"show"})
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	for _, want := range []string{"Routing Configuration", "local", "claude"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in show output:\n%s", want, out)
		}
	}
}

func TestRoutingShow_JSON(t *testing.T) {
	t.Parallel()
	fake := &fakeRoutingClient{configResp: sampleConfigResp()}
	out, err := runRoutingSubcommand(t, newFakeRoutingFactory(fake), []string{"show", "--json"})
	if err != nil {
		t.Fatalf("show --json: %v", err)
	}
	if !strings.Contains(out, "\"weights\"") {
		t.Errorf("expected JSON weights field in output:\n%s", out)
	}
	// JSON form must NOT contain the ANSI section header.
	if strings.Contains(out, "Routing Configuration") {
		t.Errorf("JSON form leaked ANSI section header:\n%s", out)
	}
}

func TestRoutingShow_Plain(t *testing.T) {
	t.Parallel()
	fake := &fakeRoutingClient{configResp: sampleConfigResp()}
	out, err := runRoutingSubcommand(t, newFakeRoutingFactory(fake), []string{"show", "--plain"})
	if err != nil {
		t.Fatalf("show --plain: %v", err)
	}
	for _, want := range []string{
		"weights: cost=0.70 latency=0.30",
		"sandbox-providers:",
		"  - id=local alpha=1.00 beta=1.00 selections=0",
		"llm-providers:",
		"  - id=claude alpha=1.00 beta=1.00 selections=0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in plain output:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\033[") {
		t.Errorf("plain output must not contain ANSI escapes:\n%s", out)
	}
}

func TestRoutingShow_ClientError(t *testing.T) {
	t.Parallel()
	fake := &fakeRoutingClient{configErr: errors.New("boom")}
	_, err := runRoutingSubcommand(t, newFakeRoutingFactory(fake), []string{"show"})
	if err == nil {
		t.Fatalf("show: expected error")
	}
	if !strings.Contains(err.Error(), "get routing config") {
		t.Errorf("err = %v, want wrapped 'get routing config' context", err)
	}
}

func TestRoutingExplain_Default(t *testing.T) {
	t.Parallel()
	fake := &fakeRoutingClient{explainResp: sampleExplainResp("sess-1")}
	out, err := runRoutingSubcommand(t, newFakeRoutingFactory(fake), []string{"explain", "sess-1"})
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if fake.lastSessionID != "sess-1" {
		t.Errorf("client received sessionID %q, want sess-1", fake.lastSessionID)
	}
	for _, want := range []string{"sess-1", "local", "claude", "Step 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in explain output:\n%s", want, out)
		}
	}
}

func TestRoutingExplain_Plain(t *testing.T) {
	t.Parallel()
	fake := &fakeRoutingClient{explainResp: sampleExplainResp("sess-explain-001")}
	out, err := runRoutingSubcommand(t, newFakeRoutingFactory(fake),
		[]string{"explain", "sess-explain-001", "--plain"})
	if err != nil {
		t.Fatalf("explain --plain: %v", err)
	}
	for _, want := range []string{
		"session: sess-explain-001\n",
		"chosen-sandbox: local\n",
		"trace:\n",
		"  step=1 phase=score dim=sandbox remaining=[local]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in plain explain output:\n%s", want, out)
		}
	}
}

func TestRoutingExplain_NotFound(t *testing.T) {
	t.Parallel()
	fake := &fakeRoutingClient{explainErr: afclient.ErrNotFound}
	_, err := runRoutingSubcommand(t, newFakeRoutingFactory(fake), []string{"explain", "missing"})
	if err == nil {
		t.Fatalf("explain: expected error on not-found")
	}
	if !strings.Contains(err.Error(), "routing decision not found") {
		t.Errorf("err = %v, want 'routing decision not found' message", err)
	}
}

func TestRoutingExplain_RequiresSessionID(t *testing.T) {
	t.Parallel()
	fake := &fakeRoutingClient{}
	// Explicitly empty (whitespace) session id — RunE rejects it.
	_, err := runRoutingSubcommand(t, newFakeRoutingFactory(fake), []string{"explain", "   "})
	if err == nil {
		t.Fatalf("expected error for empty session id")
	}
	if !strings.Contains(err.Error(), "session-id must not be empty") {
		t.Errorf("err = %v, want 'session-id must not be empty'", err)
	}
}

func TestRoutingExplain_RequiresOneArg(t *testing.T) {
	t.Parallel()
	fake := &fakeRoutingClient{}
	// No args.
	_, err := runRoutingSubcommand(t, newFakeRoutingFactory(fake), []string{"explain"})
	if err == nil {
		t.Fatalf("expected error for missing positional")
	}
}

// TestRoutingCmd_RegisteredViaRegisterCommands confirms `routing` shows
// up in the binary's RegisterCommands integration. Important: without
// this test, the wiring line in commands.go could be silently dropped.
func TestRoutingCmd_RegisteredViaRegisterCommands(t *testing.T) {
	t.Parallel()
	root := &cobra.Command{Use: "af"}
	RegisterCommands(root, Config{
		ClientFactory: func() afclient.DataSource { return afclient.NewMockClient() },
	})
	if !hasSubcommand(root, "routing") {
		t.Fatalf("routing subcommand missing from RegisterCommands; got %v", subcommandNames(root))
	}
}

// TestRoutingCmd_DefaultFactoryReturnsRealClient is a smoke check that
// the production factory wires up *afclient.DaemonClient.
func TestRoutingCmd_DefaultFactoryReturnsRealClient(t *testing.T) {
	t.Parallel()
	got := defaultRoutingClientFactory(afclient.DefaultDaemonConfig())
	if got == nil {
		t.Fatal("defaultRoutingClientFactory returned nil")
	}
	if _, ok := got.(*afclient.DaemonClient); !ok {
		t.Errorf("default factory returned %T, want *afclient.DaemonClient", got)
	}
}
