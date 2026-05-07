package afcli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// fakeProviderClient is a hand-rolled mock that satisfies
// providerDaemonClient. Per AGENTS.md, no testify.
type fakeProviderClient struct {
	listResp *afclient.ListProvidersResponse
	listErr  error
	showResp *afclient.ProviderEnvelope
	showErr  error
	gotID    string
	listCalls int
	showCalls int
}

func (f *fakeProviderClient) ListProviders() (*afclient.ListProvidersResponse, error) {
	f.listCalls++
	return f.listResp, f.listErr
}

func (f *fakeProviderClient) GetProvider(id string) (*afclient.ProviderEnvelope, error) {
	f.showCalls++
	f.gotID = id
	return f.showResp, f.showErr
}

func sampleProvider() afclient.Provider {
	return afclient.Provider{
		ID:      "claude",
		Name:    "claude",
		Version: "0.5.5",
		Family:  afclient.FamilyAgentRuntime,
		Scope:   afclient.ScopeGlobal,
		Status:  afclient.StatusReady,
		Source:  afclient.SourceBundled,
		Trust:   afclient.TrustUnsigned,
		Capabilities: map[string]any{
			"supportsSessionResume": true,
		},
	}
}

func newProviderRootForTest(client *fakeProviderClient) *cobra.Command {
	root := &cobra.Command{Use: "afcli-test"}
	root.AddCommand(newProviderCmdWithFactory(func(_ afclient.DaemonConfig) providerDaemonClient {
		return client
	}))
	return root
}

func runRootCmd(t *testing.T, root *cobra.Command, args ...string) (string, error) {
	t.Helper()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stdout)
	root.SetArgs(args)
	err := root.Execute()
	return stdout.String(), err
}

func TestProviderCmd_List_RendersTable(t *testing.T) {
	client := &fakeProviderClient{
		listResp: &afclient.ListProvidersResponse{
			Providers:       []afclient.Provider{sampleProvider()},
			PartialCoverage: true,
			CoveredFamilies: []afclient.ProviderFamily{afclient.FamilyAgentRuntime},
		},
	}
	root := newProviderRootForTest(client)
	out, err := runRootCmd(t, root, "provider", "list")
	if err != nil {
		t.Fatalf("provider list: %v", err)
	}
	if client.listCalls != 1 {
		t.Errorf("ListProviders calls = %d, want 1", client.listCalls)
	}
	if !strings.Contains(out, "claude") {
		t.Errorf("output missing provider name:\n%s", out)
	}
	if !strings.Contains(out, "agent-runtime") {
		t.Errorf("output missing family header:\n%s", out)
	}
}

func TestProviderCmd_List_JSON(t *testing.T) {
	want := afclient.ListProvidersResponse{
		Providers:       []afclient.Provider{sampleProvider()},
		PartialCoverage: true,
		CoveredFamilies: []afclient.ProviderFamily{afclient.FamilyAgentRuntime},
	}
	client := &fakeProviderClient{listResp: &want}
	root := newProviderRootForTest(client)
	out, err := runRootCmd(t, root, "provider", "list", "--json")
	if err != nil {
		t.Fatalf("provider list --json: %v", err)
	}
	var got afclient.ListProvidersResponse
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("parse JSON: %v\n%s", err, out)
	}
	if !got.PartialCoverage {
		t.Errorf("PartialCoverage = false, want true")
	}
	if len(got.Providers) != 1 || got.Providers[0].ID != "claude" {
		t.Errorf("Providers = %+v", got.Providers)
	}
}

func TestProviderCmd_List_PropagatesError(t *testing.T) {
	client := &fakeProviderClient{listErr: errors.New("boom")}
	root := newProviderRootForTest(client)
	_, err := runRootCmd(t, root, "provider", "list")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "list providers") {
		t.Errorf("err missing context: %v", err)
	}
}

func TestProviderCmd_Show_RendersDetail(t *testing.T) {
	client := &fakeProviderClient{
		showResp: &afclient.ProviderEnvelope{Provider: sampleProvider()},
	}
	root := newProviderRootForTest(client)
	out, err := runRootCmd(t, root, "provider", "show", "claude")
	if err != nil {
		t.Fatalf("provider show: %v", err)
	}
	if client.gotID != "claude" {
		t.Errorf("got id %q, want claude", client.gotID)
	}
	if !strings.Contains(out, "claude") {
		t.Errorf("output missing provider id:\n%s", out)
	}
	if !strings.Contains(out, "agent-runtime") {
		t.Errorf("output missing family:\n%s", out)
	}
	if !strings.Contains(out, "supportsSessionResume") {
		t.Errorf("output missing capability key:\n%s", out)
	}
}

func TestProviderCmd_Show_JSON(t *testing.T) {
	client := &fakeProviderClient{
		showResp: &afclient.ProviderEnvelope{Provider: sampleProvider()},
	}
	root := newProviderRootForTest(client)
	out, err := runRootCmd(t, root, "provider", "show", "claude", "--json")
	if err != nil {
		t.Fatalf("provider show --json: %v", err)
	}
	var got afclient.Provider
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("parse JSON: %v\n%s", err, out)
	}
	if got.ID != "claude" {
		t.Errorf("ID = %q, want claude", got.ID)
	}
}

func TestProviderCmd_Show_NotFoundFriendlyMessage(t *testing.T) {
	client := &fakeProviderClient{
		showErr: fmt.Errorf("get: %w", afclient.ErrNotFound),
	}
	root := newProviderRootForTest(client)
	_, err := runRootCmd(t, root, "provider", "show", "nope")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "provider not found: nope") {
		t.Errorf("err = %v, want \"provider not found: nope\"", err)
	}
}

func TestProviderCmd_Show_OtherErrorPropagates(t *testing.T) {
	client := &fakeProviderClient{
		showErr: errors.New("server died"),
	}
	root := newProviderRootForTest(client)
	_, err := runRootCmd(t, root, "provider", "show", "claude")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "get provider") {
		t.Errorf("err missing context: %v", err)
	}
}

func TestProviderCmd_Show_RequiresExactlyOneArg(t *testing.T) {
	client := &fakeProviderClient{}
	root := newProviderRootForTest(client)
	if _, err := runRootCmd(t, root, "provider", "show"); err == nil {
		t.Errorf("expected error with no args, got nil")
	}
	if _, err := runRootCmd(t, root, "provider", "show", "a", "b"); err == nil {
		t.Errorf("expected error with two args, got nil")
	}
	if client.showCalls != 0 {
		t.Errorf("GetProvider should not be called on bad args; calls = %d", client.showCalls)
	}
}

// TestProviderCmd_RegisteredViaRegisterCommands pins the integration
// hook: RegisterCommands must add `provider` to the root tree so the
// rensei binary picks it up automatically per the af↔rensei boundary
// rule in AGENTS.md.
func TestProviderCmd_RegisteredViaRegisterCommands(t *testing.T) {
	root := &cobra.Command{Use: "test-root"}
	RegisterCommands(root, Config{
		ClientFactory: func() afclient.DataSource { return afclient.NewMockClient() },
	})
	for _, c := range root.Commands() {
		if c.Use == "provider" {
			return
		}
	}
	t.Errorf("RegisterCommands did not register `provider`; subcommands: %v", commandNames(root))
}

func commandNames(root *cobra.Command) []string {
	out := make([]string, 0, len(root.Commands()))
	for _, c := range root.Commands() {
		out = append(out, c.Use)
	}
	return out
}

// TestSplitHTTPHostPort covers the small URL parser used to honour
// RENSEI_DAEMON_URL overrides.
func TestSplitHTTPHostPort(t *testing.T) {
	tests := []struct {
		raw      string
		wantHost string
		wantPort int
		wantOK   bool
	}{
		{"http://127.0.0.1:7734", "127.0.0.1", 7734, true},
		{"https://example.com:8080", "example.com", 8080, true},
		{"http://example.com:8080/", "example.com", 8080, true},
		{"127.0.0.1:7734", "127.0.0.1", 7734, true},
		{"http://no-port-here", "", 0, false},
		{"http://", "", 0, false},
		{"", "", 0, false},
		{"http://host:abc", "", 0, false},
		{"http://host:0", "", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			h, p, ok := splitHTTPHostPort(tc.raw)
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v", ok, tc.wantOK)
			}
			if h != tc.wantHost {
				t.Errorf("host = %q, want %q", h, tc.wantHost)
			}
			if p != tc.wantPort {
				t.Errorf("port = %d, want %d", p, tc.wantPort)
			}
		})
	}
}

// TestResolveProviderDaemonConfig verifies the env-override path picks
// up RENSEI_DAEMON_URL and falls back to defaults when unset.
func TestResolveProviderDaemonConfig(t *testing.T) {
	t.Setenv(providerEnvDaemonURL, "")
	cfg := resolveProviderDaemonConfig()
	if cfg.Host != "127.0.0.1" || cfg.Port != 7734 {
		t.Errorf("default cfg = %+v, want 127.0.0.1:7734", cfg)
	}

	t.Setenv(providerEnvDaemonURL, "http://10.0.0.5:9999")
	cfg = resolveProviderDaemonConfig()
	if cfg.Host != "10.0.0.5" || cfg.Port != 9999 {
		t.Errorf("override cfg = %+v, want 10.0.0.5:9999", cfg)
	}

	// Malformed override falls back to defaults — defensive, so a
	// stale env value doesn't break commands.
	t.Setenv(providerEnvDaemonURL, "junk")
	cfg = resolveProviderDaemonConfig()
	if cfg.Host != "127.0.0.1" || cfg.Port != 7734 {
		t.Errorf("malformed override cfg = %+v, want defaults", cfg)
	}
}

// TestDefaultProviderClientFactory pins that the production factory
// returns a non-nil real *afclient.DaemonClient. We don't assert on
// the type identity (afclient is a separate package and the *T cast
// is internal); just that we get something usable.
func TestDefaultProviderClientFactory(t *testing.T) {
	c := defaultProviderClientFactory(afclient.DefaultDaemonConfig())
	if c == nil {
		t.Fatal("default factory returned nil")
	}
}
