package afcli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/donmai/afclient"
)

// fakeKitClient is a hand-rolled mock satisfying kitDaemonClient.
type fakeKitClient struct {
	listResp    *afclient.ListKitsResponse
	listErr     error
	showResp    *afclient.KitManifestEnvelope
	showErr     error
	verifyResp  *afclient.KitSignatureResult
	verifyErr   error
	installResp *afclient.KitInstallResult
	installErr  error
	enableResp  *afclient.Kit
	enableErr   error
	disableResp *afclient.Kit
	disableErr  error
	srcListResp *afclient.ListKitSourcesResponse
	srcListErr  error
	srcEnResp   *afclient.KitSourceToggleResult
	srcEnErr    error
	srcDisResp  *afclient.KitSourceToggleResult
	srcDisErr   error

	gotInstallID, gotInstallVersion string
	gotInstallTrustOverride         string
	gotInstallSource                *afclient.KitInstallSource
	gotInstallCalls                 int
	gotEnableID, gotDisableID       string
	gotSrcEnable, gotSrcDisable     string
}

func (f *fakeKitClient) ListKits() (*afclient.ListKitsResponse, error) {
	return f.listResp, f.listErr
}

func (f *fakeKitClient) GetKit(_ string) (*afclient.KitManifestEnvelope, error) {
	return f.showResp, f.showErr
}

func (f *fakeKitClient) VerifyKitSignature(_ string) (*afclient.KitSignatureResult, error) {
	return f.verifyResp, f.verifyErr
}

func (f *fakeKitClient) InstallKit(id string, req afclient.KitInstallRequest) (*afclient.KitInstallResult, error) {
	f.gotInstallCalls++
	f.gotInstallID = id
	f.gotInstallVersion = req.Version
	f.gotInstallTrustOverride = req.TrustOverride
	if req.Source != nil {
		source := *req.Source
		f.gotInstallSource = &source
	} else {
		f.gotInstallSource = nil
	}
	return f.installResp, f.installErr
}

func (f *fakeKitClient) EnableKit(id string) (*afclient.Kit, error) {
	f.gotEnableID = id
	return f.enableResp, f.enableErr
}

func (f *fakeKitClient) DisableKit(id string) (*afclient.Kit, error) {
	f.gotDisableID = id
	return f.disableResp, f.disableErr
}

func (f *fakeKitClient) ListKitSources() (*afclient.ListKitSourcesResponse, error) {
	return f.srcListResp, f.srcListErr
}

func (f *fakeKitClient) EnableKitSource(name string) (*afclient.KitSourceToggleResult, error) {
	f.gotSrcEnable = name
	return f.srcEnResp, f.srcEnErr
}

func (f *fakeKitClient) DisableKitSource(name string) (*afclient.KitSourceToggleResult, error) {
	f.gotSrcDisable = name
	return f.srcDisResp, f.srcDisErr
}

func newKitRootForTest(client *fakeKitClient) *cobra.Command {
	root := &cobra.Command{Use: "afcli-test"}
	root.AddCommand(newKitCmdWithFactory(func(_ afclient.DaemonConfig) kitDaemonClient {
		return client
	}))
	return root
}

func sampleKit() afclient.Kit {
	return afclient.Kit{
		ID:      "spring/java",
		Name:    "Spring",
		Version: "1.0.0",
		Scope:   afclient.KitScopeProject,
		Status:  afclient.KitStatusActive,
		Source:  afclient.KitSourceLocal,
		Trust:   afclient.KitTrustUnsigned,
	}
}

func TestKitCmd_List_RendersTable(t *testing.T) {
	client := &fakeKitClient{
		listResp: &afclient.ListKitsResponse{Kits: []afclient.Kit{sampleKit()}},
	}
	root := newKitRootForTest(client)
	out, err := runRootCmd(t, root, "kit", "list", "--plain")
	if err != nil {
		t.Fatalf("kit list: %v", err)
	}
	if !strings.Contains(out, "spring/java") {
		t.Errorf("output missing kit id:\n%s", out)
	}
}

func TestKitCmd_List_JSON(t *testing.T) {
	want := afclient.ListKitsResponse{Kits: []afclient.Kit{sampleKit()}}
	client := &fakeKitClient{listResp: &want}
	root := newKitRootForTest(client)
	out, err := runRootCmd(t, root, "kit", "list", "--json")
	if err != nil {
		t.Fatalf("kit list --json: %v", err)
	}
	var got afclient.ListKitsResponse
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("parse JSON: %v\n%s", err, out)
	}
	if len(got.Kits) != 1 || got.Kits[0].ID != "spring/java" {
		t.Errorf("decoded Kits: %+v", got.Kits)
	}
}

func TestKitCmd_List_Error(t *testing.T) {
	client := &fakeKitClient{listErr: errors.New("boom")}
	root := newKitRootForTest(client)
	if _, err := runRootCmd(t, root, "kit", "list"); err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestKitCmd_Show_RendersDetail(t *testing.T) {
	client := &fakeKitClient{
		showResp: &afclient.KitManifestEnvelope{Kit: afclient.KitManifest{Kit: sampleKit()}},
	}
	root := newKitRootForTest(client)
	out, err := runRootCmd(t, root, "kit", "show", "spring/java", "--plain")
	if err != nil {
		t.Fatalf("kit show: %v", err)
	}
	if !strings.Contains(out, "spring/java") {
		t.Errorf("output missing kit id:\n%s", out)
	}
}

func TestKitCmd_Show_JSON(t *testing.T) {
	client := &fakeKitClient{
		showResp: &afclient.KitManifestEnvelope{Kit: afclient.KitManifest{Kit: sampleKit()}},
	}
	root := newKitRootForTest(client)
	out, err := runRootCmd(t, root, "kit", "show", "spring/java", "--json")
	if err != nil {
		t.Fatalf("kit show --json: %v", err)
	}
	var got afclient.KitManifestEnvelope
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("parse JSON: %v\n%s", err, out)
	}
	if got.Kit.ID != "spring/java" {
		t.Errorf("Kit.ID: want spring/java, got %q", got.Kit.ID)
	}
}

func TestKitCmd_Show_NotFound(t *testing.T) {
	client := &fakeKitClient{showErr: fmt.Errorf("get: %w", afclient.ErrNotFound)}
	root := newKitRootForTest(client)
	_, err := runRootCmd(t, root, "kit", "show", "nope")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "kit not found: nope") {
		t.Errorf("error: %v, want \"kit not found: nope\"", err)
	}
}

func TestKitCmd_Show_OtherErrorPropagates(t *testing.T) {
	client := &fakeKitClient{showErr: errors.New("upstream timeout")}
	root := newKitRootForTest(client)
	_, err := runRootCmd(t, root, "kit", "show", "spring/java")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "show kit") {
		t.Errorf("error missing context: %v", err)
	}
}

func TestKitCmd_Verify_RendersResult(t *testing.T) {
	client := &fakeKitClient{
		verifyResp: &afclient.KitSignatureResult{
			KitID: "spring/java",
			Trust: afclient.KitTrustUnsigned,
			OK:    true,
		},
	}
	root := newKitRootForTest(client)
	out, err := runRootCmd(t, root, "kit", "verify", "spring/java", "--plain")
	if err != nil {
		t.Fatalf("kit verify: %v", err)
	}
	if !strings.Contains(out, "spring/java") || !strings.Contains(out, "[unsigned]") {
		t.Errorf("output missing trust info:\n%s", out)
	}
}

func TestKitCmd_Verify_NotFound(t *testing.T) {
	client := &fakeKitClient{verifyErr: fmt.Errorf("v: %w", afclient.ErrNotFound)}
	root := newKitRootForTest(client)
	_, err := runRootCmd(t, root, "kit", "verify", "nope")
	if err == nil || !strings.Contains(err.Error(), "kit not found: nope") {
		t.Errorf("error: %v, want kit not found", err)
	}
}

func TestKitCmd_Install_PassesVersion(t *testing.T) {
	client := &fakeKitClient{
		installResp: &afclient.KitInstallResult{Kit: sampleKit(), Message: "installed"},
	}
	root := newKitRootForTest(client)
	out, err := runRootCmd(t, root, "kit", "install", "spring/java", "--version", "2.0", "--plain")
	if err != nil {
		t.Fatalf("kit install: %v", err)
	}
	if client.gotInstallID != "spring/java" {
		t.Errorf("install id: want spring/java, got %q", client.gotInstallID)
	}
	if client.gotInstallVersion != "2.0" {
		t.Errorf("install version: want 2.0, got %q", client.gotInstallVersion)
	}
	if !strings.Contains(out, "installed") {
		t.Errorf("output missing 'installed':\n%s", out)
	}
}

func TestKitCmd_Install_PassesGitSourceFlags(t *testing.T) {
	client := &fakeKitClient{
		installResp: &afclient.KitInstallResult{Kit: sampleKit(), Message: "installed"},
	}
	root := newKitRootForTest(client)
	out, err := runRootCmd(t, root,
		"kit", "install", "spring/java",
		"--version", "2.0",
		"--source-kind", "git",
		"--source-url", "https://example.invalid/kits.git",
		"--source-ref", "refs/tags/v2.0.0",
		"--allow-unsigned", "--plain",
	)
	if err != nil {
		t.Fatalf("kit install with Git source: %v", err)
	}
	wantSource := &afclient.KitInstallSource{
		Kind: "git", URL: "https://example.invalid/kits.git", Ref: "refs/tags/v2.0.0",
	}
	if client.gotInstallCalls != 1 || client.gotInstallID != "spring/java" || client.gotInstallVersion != "2.0" {
		t.Errorf("install request identity/version: calls=%d id=%q version=%q", client.gotInstallCalls, client.gotInstallID, client.gotInstallVersion)
	}
	if client.gotInstallSource == nil || *client.gotInstallSource != *wantSource {
		t.Errorf("install source: got %+v, want %+v", client.gotInstallSource, wantSource)
	}
	if client.gotInstallTrustOverride != afclient.TrustOverrideAllowedThisOnce {
		t.Errorf("trustOverride: want %q, got %q", afclient.TrustOverrideAllowedThisOnce, client.gotInstallTrustOverride)
	}
	if !strings.Contains(out, "WARNING") || !strings.Contains(out, "installed") {
		t.Errorf("install output missing bypass warning or result:\n%s", out)
	}
}

func TestKitCmd_Install_RejectsInvalidSourceFlagsBeforeRequest(t *testing.T) {
	const sourceURL = "https://example.invalid/kits.git"
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "URL requires an explicit kind",
			args: []string{"--source-url", sourceURL},
			want: "--source-kind is required",
		},
		{
			name: "ref requires an explicit kind",
			args: []string{"--source-ref", "main"},
			want: "--source-kind is required",
		},
		{
			name: "ref requires a URL",
			args: []string{"--source-kind", "git", "--source-ref", "main"},
			want: "--source-url is required",
		},
		{
			name: "git kind requires a URL",
			args: []string{"--source-kind", "git"},
			want: "--source-url is required",
		},
		{
			name: "unsupported federation kind is rejected locally",
			args: []string{"--source-kind", "tessl", "--source-url", sourceURL},
			want: "supported source kind: git",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeKitClient{}
			root := newKitRootForTest(client)
			args := append([]string{"kit", "install", "spring/java"}, tt.args...)
			_, err := runRootCmd(t, root, args...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
			if client.gotInstallCalls != 0 {
				t.Fatalf("invalid source options reached InstallKit %d times", client.gotInstallCalls)
			}
		})
	}
}

func TestKitCmd_Install_HelpExplainsSupportedGitSource(t *testing.T) {
	root := newKitRootForTest(&fakeKitClient{})
	installHelp, err := runRootCmd(t, root, "kit", "install", "--help")
	if err != nil {
		t.Fatalf("kit install --help: %v", err)
	}
	for _, want := range []string{"--source-kind", "--source-url", "--source-ref", "branch name or fully-qualified ref", "A source is required", "HTTP 501", "rejected by the CLI"} {
		if !strings.Contains(installHelp, want) {
			t.Errorf("install help missing %q:\n%s", want, installHelp)
		}
	}
	if strings.Contains(installHelp, "commit") || strings.Contains(installHelp, "Tessl") {
		t.Errorf("install help promises unsupported ref/kind semantics:\n%s", installHelp)
	}
	if strings.Contains(installHelp, "Wave 9 caveat") || strings.Contains(installHelp, "only locally-installed kits are supported") {
		t.Errorf("install help retains the stale local-only caveat:\n%s", installHelp)
	}

	kitHelp, err := runRootCmd(t, newKitRootForTest(&fakeKitClient{}), "kit", "--help")
	if err != nil {
		t.Fatalf("kit --help: %v", err)
	}
	if !strings.Contains(kitHelp, "Install a kit from a Git source") || !strings.Contains(kitHelp, "rejects source kinds other than") || strings.Contains(kitHelp, "Only the `local` source has a working backend") {
		t.Errorf("kit help does not describe the Git install source accurately:\n%s", kitHelp)
	}
}

func TestKitCmd_Install_NotFound(t *testing.T) {
	client := &fakeKitClient{installErr: fmt.Errorf("i: %w", afclient.ErrNotFound)}
	root := newKitRootForTest(client)
	_, err := runRootCmd(t, root, "kit", "install", "nope")
	if err == nil || !strings.Contains(err.Error(), "kit not found: nope") {
		t.Errorf("error: %v, want kit not found", err)
	}
}

// TestKitCmd_Install_DefaultSendsNoTrustOverride pins that a plain
// install does NOT bypass the trust gate.
func TestKitCmd_Install_DefaultSendsNoTrustOverride(t *testing.T) {
	client := &fakeKitClient{
		installResp: &afclient.KitInstallResult{Kit: sampleKit(), Message: "installed"},
	}
	root := newKitRootForTest(client)
	if _, err := runRootCmd(t, root, "kit", "install", "spring/java", "--plain"); err != nil {
		t.Fatalf("kit install: %v", err)
	}
	if client.gotInstallTrustOverride != "" {
		t.Errorf("trustOverride: want empty without --allow-unsigned, got %q", client.gotInstallTrustOverride)
	}
	if client.gotInstallCalls != 1 || client.gotInstallSource != nil {
		t.Errorf("no-source install: calls=%d source=%+v, want one call with nil Source", client.gotInstallCalls, client.gotInstallSource)
	}
}

// TestKitCmd_Install_AllowUnsignedSendsOverrideAndWarns pins the
// explicit per-install override path: --allow-unsigned maps to the
// trustOverride wire field and prints a warning before installing.
func TestKitCmd_Install_AllowUnsignedSendsOverrideAndWarns(t *testing.T) {
	client := &fakeKitClient{
		installResp: &afclient.KitInstallResult{Kit: sampleKit(), Message: "installed"},
	}
	root := newKitRootForTest(client)
	out, err := runRootCmd(t, root, "kit", "install", "spring/java", "--allow-unsigned", "--plain")
	if err != nil {
		t.Fatalf("kit install --allow-unsigned: %v", err)
	}
	if client.gotInstallTrustOverride != afclient.TrustOverrideAllowedThisOnce {
		t.Errorf("trustOverride: want %q, got %q", afclient.TrustOverrideAllowedThisOnce, client.gotInstallTrustOverride)
	}
	if !strings.Contains(out, "WARNING") || !strings.Contains(out, "bypasses signature verification") {
		t.Errorf("output missing bypass warning:\n%s", out)
	}
}

// TestKitCmd_Install_TrustGateRejectedShowsGuidance pins the failure
// UX: a daemon 403 (trust-gate rejection) surfaces remediation steps —
// allowlist the signer, one-time override, or switch the trust mode.
func TestKitCmd_Install_TrustGateRejectedShowsGuidance(t *testing.T) {
	client := &fakeKitClient{installErr: fmt.Errorf("i: %w", afclient.ErrUnauthorized)}
	root := newKitRootForTest(client)
	_, err := runRootCmd(t, root, "kit", "install", "spring/java")
	if err == nil {
		t.Fatal("kit install: want error for trust-gate rejection, got nil")
	}
	for _, want := range []string{
		"trust gate",
		"trust.issuerSet",
		"--allow-unsigned",
		"trust.mode: permissive",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%s", want, err.Error())
		}
	}
}

func TestKitCmd_EnableDisable(t *testing.T) {
	k := sampleKit()
	enabledKit := k
	enabledKit.Status = afclient.KitStatusActive
	disabledKit := k
	disabledKit.Status = afclient.KitStatusDisabled

	client := &fakeKitClient{
		enableResp:  &enabledKit,
		disableResp: &disabledKit,
	}
	root := newKitRootForTest(client)

	out, err := runRootCmd(t, root, "kit", "enable", "spring/java", "--plain")
	if err != nil {
		t.Fatalf("kit enable: %v", err)
	}
	if !strings.Contains(out, "kit spring/java enabled") {
		t.Errorf("enable output: %s", out)
	}
	if client.gotEnableID != "spring/java" {
		t.Errorf("enable id: want spring/java, got %q", client.gotEnableID)
	}

	out, err = runRootCmd(t, root, "kit", "disable", "spring/java", "--plain")
	if err != nil {
		t.Fatalf("kit disable: %v", err)
	}
	if !strings.Contains(out, "kit spring/java disabled") {
		t.Errorf("disable output: %s", out)
	}
	if client.gotDisableID != "spring/java" {
		t.Errorf("disable id: want spring/java, got %q", client.gotDisableID)
	}
}

func TestKitCmd_Enable_NotFound(t *testing.T) {
	client := &fakeKitClient{enableErr: fmt.Errorf("e: %w", afclient.ErrNotFound)}
	root := newKitRootForTest(client)
	_, err := runRootCmd(t, root, "kit", "enable", "nope")
	if err == nil || !strings.Contains(err.Error(), "kit not found: nope") {
		t.Errorf("error: %v, want kit not found", err)
	}
}

func TestKitCmd_Disable_OtherError(t *testing.T) {
	client := &fakeKitClient{disableErr: errors.New("network bork")}
	root := newKitRootForTest(client)
	_, err := runRootCmd(t, root, "kit", "disable", "x")
	if err == nil || !strings.Contains(err.Error(), "disable kit") {
		t.Errorf("error: %v, want wrapped", err)
	}
}

func TestKitCmd_Sources_List(t *testing.T) {
	client := &fakeKitClient{
		srcListResp: &afclient.ListKitSourcesResponse{
			Sources: []afclient.KitRegistrySource{{Name: "local", Kind: "local", Enabled: true, Priority: 1}},
		},
	}
	root := newKitRootForTest(client)
	out, err := runRootCmd(t, root, "kit", "sources", "list", "--plain")
	if err != nil {
		t.Fatalf("sources list: %v", err)
	}
	if !strings.Contains(out, "local") {
		t.Errorf("output: %s", out)
	}
}

func TestKitCmd_Sources_ListJSON(t *testing.T) {
	want := &afclient.ListKitSourcesResponse{
		Sources: []afclient.KitRegistrySource{{Name: "tessl", Kind: "tessl", Enabled: false, Priority: 4}},
	}
	client := &fakeKitClient{srcListResp: want}
	root := newKitRootForTest(client)
	out, err := runRootCmd(t, root, "kit", "sources", "list", "--json")
	if err != nil {
		t.Fatalf("sources list --json: %v", err)
	}
	var got afclient.ListKitSourcesResponse
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("parse JSON: %v\n%s", err, out)
	}
	if len(got.Sources) != 1 || got.Sources[0].Name != "tessl" {
		t.Errorf("decoded: %+v", got.Sources)
	}
}

func TestKitCmd_Sources_EnableDisable(t *testing.T) {
	enableSrc := afclient.KitRegistrySource{Name: "tessl", Kind: "tessl", Enabled: true}
	disableSrc := afclient.KitRegistrySource{Name: "tessl", Kind: "tessl", Enabled: false}
	client := &fakeKitClient{
		srcEnResp:  &afclient.KitSourceToggleResult{Source: enableSrc, Message: "ok"},
		srcDisResp: &afclient.KitSourceToggleResult{Source: disableSrc, Message: "ok"},
	}
	root := newKitRootForTest(client)

	out, err := runRootCmd(t, root, "kit", "sources", "enable", "tessl", "--plain")
	if err != nil {
		t.Fatalf("sources enable: %v", err)
	}
	if !strings.Contains(out, "source tessl enabled") {
		t.Errorf("enable output: %s", out)
	}
	if client.gotSrcEnable != "tessl" {
		t.Errorf("enable name: want tessl, got %q", client.gotSrcEnable)
	}

	out, err = runRootCmd(t, root, "kit", "sources", "disable", "tessl", "--plain")
	if err != nil {
		t.Fatalf("sources disable: %v", err)
	}
	if !strings.Contains(out, "source tessl disabled") {
		t.Errorf("disable output: %s", out)
	}
}

func TestKitCmd_Sources_NotFound(t *testing.T) {
	client := &fakeKitClient{srcEnErr: fmt.Errorf("e: %w", afclient.ErrNotFound)}
	root := newKitRootForTest(client)
	_, err := runRootCmd(t, root, "kit", "sources", "enable", "nope")
	if err == nil || !strings.Contains(err.Error(), "kit source not found: nope") {
		t.Errorf("error: %v", err)
	}
}

func TestKitCmd_Sources_OtherError(t *testing.T) {
	client := &fakeKitClient{srcDisErr: errors.New("disk full")}
	root := newKitRootForTest(client)
	_, err := runRootCmd(t, root, "kit", "sources", "disable", "tessl")
	if err == nil || !strings.Contains(err.Error(), "disable kit source") {
		t.Errorf("error: %v", err)
	}
}

func TestKitCmd_Show_RequiresExactlyOneArg(t *testing.T) {
	client := &fakeKitClient{}
	root := newKitRootForTest(client)
	if _, err := runRootCmd(t, root, "kit", "show"); err == nil {
		t.Error("expected error with no args")
	}
	if _, err := runRootCmd(t, root, "kit", "show", "a", "b"); err == nil {
		t.Error("expected error with two args")
	}
}

// TestKitCmd_RegisteredViaRegisterCommands pins the integration hook:
// RegisterCommands must add `kit` to the root tree so the rensei
// binary picks it up automatically per the af↔rensei boundary rule
// in AGENTS.md.
func TestKitCmd_RegisteredViaRegisterCommands(t *testing.T) {
	root := &cobra.Command{Use: "test-root"}
	RegisterCommands(root, Config{
		ClientFactory: func() afclient.DataSource { return afclient.NewMockClient() },
	})
	for _, c := range root.Commands() {
		if c.Use == "kit" {
			return
		}
	}
	t.Errorf("RegisterCommands did not register `kit`; subcommands: %v", commandNames(root))
}

// TestResolveKitDaemonConfig pins the env-override path used by all
// kit subcommands.
func TestResolveKitDaemonConfig(t *testing.T) {
	t.Setenv(providerEnvDaemonURL, "")
	cfg := resolveKitDaemonConfig()
	if cfg.Host != "127.0.0.1" || cfg.Port != 7734 {
		t.Errorf("default cfg = %+v, want 127.0.0.1:7734", cfg)
	}
	t.Setenv(providerEnvDaemonURL, "http://10.0.0.5:9999")
	cfg = resolveKitDaemonConfig()
	if cfg.Host != "10.0.0.5" || cfg.Port != 9999 {
		t.Errorf("override cfg = %+v, want 10.0.0.5:9999", cfg)
	}
}

// TestDefaultKitClientFactory pins that the production factory returns
// a non-nil DaemonClient.
func TestDefaultKitClientFactory(t *testing.T) {
	c := defaultKitClientFactory(afclient.DefaultDaemonConfig())
	if c == nil {
		t.Fatal("default factory returned nil")
	}
}
