package afcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/daemon"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/internal/localgithub"
	"github.com/RenseiAI/donmai/internal/localruntimeauth"
	"github.com/RenseiAI/donmai/provider/harness/claude"
	"github.com/RenseiAI/donmai/provider/harness/codex"
	"github.com/RenseiAI/donmai/runner"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

func localFileQueueRoot(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "file" || parsed.Host != "" || parsed.Opaque != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawPath != "" || !filepath.IsAbs(parsed.Path) || filepath.Clean(parsed.Path) != parsed.Path {
		return "", errors.New("local runtime requires a canonical absolute file queue URL")
	}
	return parsed.Path, nil
}

type localRuntimeCompiler struct {
	registry     *runner.Registry
	producer     *runner.LocalAdmissionProducer
	config       runner.LocalAdmissionConfig
	repositories []daemon.LocalGitHubRepository
	security     *daemon.LocalExecutionSecurity
}

func (c *localRuntimeCompiler) Close() error { return c.registry.Shutdown(context.Background()) }

func (c *localRuntimeCompiler) Selectors() *executioncell.ExecutionSelectorRegistry {
	versions := map[string][]string{}
	for _, name := range c.registry.Names() {
		provider, err := c.registry.Resolve(name)
		if err == nil {
			if harness, ok := provider.(agent.HarnessProvider); ok {
				manifest := harness.Manifest()
				versions[string(manifest.Name)] = []string{manifest.ContractABI}
			}
		}
	}
	return &executioncell.ExecutionSelectorRegistry{HarnessVersions: versions, Models: []string{c.config.Model.Author + "/" + c.config.Model.ID}, Endpoints: []string{c.config.EndpointID}, AuthBindings: []string{c.config.AuthBindingID}, Placements: []string{c.config.PlacementID}, Capabilities: []string{}}
}

func (c *localRuntimeCompiler) Compile(ctx context.Context, request daemon.LocalCompileRequest) (daemon.LocalAdmissionEvidence, error) {
	var repository *daemon.LocalGitHubRepository
	for i := range c.repositories {
		if c.repositories[i].RepositoryID == request.Source.RepositoryID && strings.EqualFold(c.repositories[i].OwnerRepo, request.Source.OwnerRepo) {
			repository = &c.repositories[i]
			break
		}
	}
	if repository == nil {
		return daemon.LocalAdmissionEvidence{}, errors.New("local compiler source is outside configured repositories")
	}
	work := runner.QueuedWork{}
	work.WorkType = "development"
	work.Body, work.Title = request.Source.Body, request.Source.Title
	work.IssueIdentifier = request.Source.OwnerRepo + "#" + strconv.Itoa(request.Source.IssueNumber)
	work.ProjectName = repository.OwnerRepo
	work.Repository = "https://github.com/" + repository.OwnerRepo + ".git"
	if err := worktree.ValidateBranchBaseRef(repository.Ref); err != nil {
		return daemon.LocalAdmissionEvidence{}, err
	}
	work.BaseRef = repository.Ref
	work.Branch = "donmai/" + request.SessionID
	stamp, err := daemon.NewLocalExecutionSecurityStamp(c.config.ScopeID, c.security, time.Now())
	if err != nil {
		return daemon.LocalAdmissionEvidence{}, err
	}
	work.ExecutionSecurity = stamp
	output, err := c.producer.Admit(ctx, runner.LocalAdmissionRequest{SessionID: request.SessionID, ReceiptID: request.ReceiptID, PreflightChallengeID: request.ChallengeID, Work: work})
	if err != nil {
		return daemon.LocalAdmissionEvidence{}, err
	}
	return daemon.LocalAdmissionEvidence{Session: output.Session, Intent: output.Intent, Receipt: output.Receipt, EffectiveCell: output.EffectiveCell, RuntimeBinding: output.RuntimeBinding, OperationalPayload: output.OperationalPayload, HostPreflight: output.HostPreflight, ProducerEvidence: output.Evidence}, nil
}

func (c *localRuntimeCompiler) Revalidate(ctx context.Context, evidence daemon.LocalAdmissionEvidence) error {
	if err := daemon.ValidateLocalExecutionSecurityEvidence(c.config.ScopeID, evidence.OperationalPayload, evidence.HostPreflight); err != nil {
		return err
	}
	var work runner.QueuedWork
	if err := json.Unmarshal(evidence.OperationalPayload, &work); err != nil {
		return err
	}
	binding, err := executioncell.DecodeRuntimeBinding(evidence.RuntimeBinding)
	if err != nil || binding.PreflightRegistration == nil {
		return errors.New("local runtime binding has no registered preflight challenge")
	}
	allowed := false
	for _, repository := range c.repositories {
		if work.Repository == "https://github.com/"+repository.OwnerRepo+".git" && work.Ref == "" && work.BaseRef == repository.Ref {
			allowed = true
			break
		}
	}
	if !allowed {
		return errors.New("local repository/ref consent changed after admission")
	}
	work.SessionID = ""
	work.OrganizationID = ""
	work.WorkerID = ""
	work.ResolvedProfile = runner.ResolvedProfile{}
	// The producer owns the resolved model projection; authored payload content
	// remains unchanged and must reproduce the exact immutable payload below.
	output, err := c.producer.Admit(ctx, runner.LocalAdmissionRequest{SessionID: evidence.Session.SessionID, ReceiptID: evidence.Session.AdmissionReceiptID, PreflightChallengeID: binding.PreflightRegistration.ChallengeID, Work: work})
	if err != nil {
		return err
	}
	if !bytes.Equal(output.Evidence, evidence.ProducerEvidence) || !bytes.Equal(output.OperationalPayload, evidence.OperationalPayload) {
		return errors.New("local runtime/configuration observations changed after admission")
	}
	return nil
}

func newLocalCompiler(identity daemon.LocalRuntimeIdentity, settings *daemon.LocalRuntimeConfig) (*localRuntimeCompiler, error) {
	if settings == nil {
		return nil, errors.New("localRuntime profile is required for a file queue")
	}
	if err := settings.ExecutionSecurity.Validate(); err != nil {
		return nil, err
	}
	registry, err := runner.NewRegistryWithOptions(runner.RegistryOptions{RuntimeTransportMode: runner.RuntimeTransportLocalV2})
	if err != nil {
		return nil, err
	}
	fail := func(cause error) (*localRuntimeCompiler, error) {
		_ = registry.Shutdown(context.Background())
		return nil, cause
	}
	var provider agent.Provider
	var binary string
	var check func(context.Context) error
	switch settings.Harness {
	case "codex":
		binary, err = exec.LookPath("codex")
		if err != nil {
			return fail(errors.New("local Codex runtime is not installed"))
		}
		provider, err = codex.New(codex.Options{CodexBin: binary, HostSessionAuth: true})
		check = func(ctx context.Context) error { return codex.CheckHostSessionLogin(ctx, binary) }
	case "claude-code":
		binary, err = exec.LookPath("claude")
		if err != nil {
			return fail(errors.New("local Claude runtime is not installed"))
		}
		provider, err = claude.New(claude.Options{Binary: binary})
		selectedModel := settings.Model
		check = func(ctx context.Context) error {
			if err := claude.CheckModelBinaryVersion(ctx, binary, selectedModel); err != nil {
				return err
			}
			return claude.CheckHostSessionLogin(ctx, binary)
		}
	default:
		return fail(errors.New("local runtime supports only Claude Code or Codex"))
	}
	if err != nil {
		return fail(err)
	}
	if err = registry.Register(provider); err != nil {
		return fail(err)
	}
	profile := struct {
		Harness, Model, Author, Catalog string
		Security                        agent.ExecutionSecurityLevels
	}{settings.Harness, settings.Model, settings.ModelAuthor, settings.ModelCatalogRevision, settings.ExecutionSecurity.Levels()}
	revision, err := executioncell.DigestContractValue(profile)
	if err != nil {
		return fail(err)
	}
	config := runner.LocalAdmissionConfig{ScopeID: identity.ScopeID, WorkerID: identity.WorkerID, PlacementID: identity.HostID, ConfigurationRevision: revision, Harness: agent.HarnessName(settings.Harness), Model: executioncell.ModelRef{ID: settings.Model, Author: settings.ModelAuthor}, EndpointID: "local-" + settings.ModelAuthor, AuthBindingID: "local-host-login", BinaryPath: binary, CheckHostAuth: check}
	if settings.ModelCatalogRevision != "" {
		config.ModelCatalogEntry = &runner.LocalModelCatalogEntry{Model: config.Model, Host: agent.HostOAuthCLI, Revision: settings.ModelCatalogRevision}
	}
	producer, err := runner.NewLocalAdmissionProducer(registry, config)
	if err != nil {
		return fail(err)
	}
	policy := *settings.ExecutionSecurity
	return &localRuntimeCompiler{registry: registry, producer: producer, config: config, repositories: append([]daemon.LocalGitHubRepository(nil), settings.Repositories...), security: &policy}, nil
}

type localProviderView struct {
	settings func() *daemon.LocalRuntimeConfig
}

func (v *localProviderView) compiler() (*localRuntimeCompiler, error) {
	return newLocalCompiler(daemon.LocalRuntimeIdentity{ScopeID: "local-inspection", HostID: "local-host", WorkerID: "local-worker"}, v.settings())
}

func (v *localProviderView) Names() []string {
	compiler, err := v.compiler()
	if err != nil {
		return nil
	}
	defer func() { _ = compiler.Close() }()
	return runner.NewProviderView(compiler.registry).Names()
}

func (v *localProviderView) Capabilities(name string) (map[string]any, bool) {
	compiler, err := v.compiler()
	if err != nil {
		return nil, false
	}
	defer func() { _ = compiler.Close() }()
	return runner.NewProviderView(compiler.registry).Capabilities(name)
}

func (v *localProviderView) PreflightExecution(raw json.RawMessage) (json.RawMessage, error) {
	compiler, err := v.compiler()
	if err != nil {
		return nil, err
	}
	defer func() { _ = compiler.Close() }()
	return runner.NewProviderView(compiler.registry).PreflightExecution(raw)
}

func newLocalRuntimeComposition(configPath string, current func() *daemon.Config, baseEnv map[string]string) (*daemon.LocalRuntimeOptions, daemon.ProviderRegistry, error) {
	initial, err := daemon.LoadConfig(configPath)
	if err != nil {
		return nil, nil, err
	}
	if initial == nil || !strings.HasPrefix(initial.Orchestrator.URL, "file:") {
		return nil, nil, nil
	}
	root, err := localFileQueueRoot(initial.Orchestrator.URL)
	if err != nil {
		return nil, nil, err
	}
	if initial.LocalRuntime == nil || initial.LocalRuntime.Harness == "" || initial.LocalRuntime.Model == "" || len(initial.LocalRuntime.Repositories) == 0 {
		// A fresh-host seed reaches composition before any operator ran
		// setup. Return the queue root with no profile so the
		// caller refuses with the operator action (`host setup`)
		// instead of an internal wiring complaint.
		return &daemon.LocalRuntimeOptions{QueueRoot: root, AuthRoot: root + ".auth"}, nil, nil
	}
	settings := func() *daemon.LocalRuntimeConfig {
		if current != nil {
			if live := current(); live != nil {
				return live.LocalRuntime
			}
		}
		return initial.LocalRuntime
	}
	token, err := localGitHubSourceToken(baseEnv)
	if err != nil {
		return nil, nil, err
	}
	options := &daemon.LocalRuntimeOptions{QueueRoot: root, AuthRoot: root + ".auth", Repositories: append([]daemon.LocalGitHubRepository(nil), initial.LocalRuntime.Repositories...)}
	options.NewCompiler = func(identity daemon.LocalRuntimeIdentity) (daemon.LocalRuntimeCompiler, error) {
		return newLocalCompiler(identity, settings())
	}
	options.NewSource = func(repositories []daemon.LocalGitHubRepository) (daemon.LocalRuntimeSource, error) {
		return localgithub.New(localgithub.Options{Repositories: repositories, Token: token, PublicationRoot: root + ".github-publications"})
	}
	return options, &localProviderView{settings: settings}, nil
}

func localAgentRegistry(logger *slog.Logger, hints agentRunCtorHints, bin string, mode runner.RuntimeTransportMode, selectedHarness string) (*runner.Registry, error) {
	registry, err := runner.NewRegistryWithOptions(runner.RegistryOptions{RuntimeTransportMode: mode})
	if err != nil {
		return nil, err
	}
	selectedProvider := ""
	switch selectedHarness {
	case "":
	case string(agent.HarnessCodex):
		selectedProvider = string(agent.ProviderCodex)
	case string(agent.HarnessClaudeCode):
		selectedProvider = string(agent.ProviderClaude)
	default:
		return nil, errors.New("local runtime has no supported admitted harness")
	}
	for _, ctor := range agentRunProviderCtors(hints) {
		if selectedProvider != "" && ctor.name != selectedProvider {
			continue
		}
		provider, err := ctor.new()
		if err != nil {
			logger.Warn("local provider unavailable", "provider", ctor.name)
			continue
		}
		if err = registry.Register(provider); err != nil {
			return nil, err
		}
	}
	if len(registry.Names()) == 0 {
		return nil, fmt.Errorf("%s local runtime has no available provider", bin)
	}
	return registry, nil
}

func validateLocalAgentOrigin(origin string) error {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" || parsed.Path != "" || parsed.RawPath != "" {
		return errors.New("local worker requires an explicit canonical loopback receiver origin")
	}
	host, portText, err := net.SplitHostPort(parsed.Host)
	port, portErr := strconv.Atoi(portText)
	ip := net.ParseIP(host)
	if err != nil || portErr != nil || ip == nil || !ip.IsLoopback() || port < 1 || port > 65535 {
		return errors.New("local worker requires an explicit canonical loopback receiver origin")
	}
	if origin != (&url.URL{Scheme: "http", Host: net.JoinHostPort(ip.String(), strconv.Itoa(port))}).String() {
		return errors.New("local worker requires an explicit canonical loopback receiver origin")
	}
	return nil
}

func validateLocalAgentDetail(origin string, detail *daemon.SessionDetail) error {
	if err := validateLocalAgentOrigin(origin); err != nil {
		return err
	}
	if detail == nil || detail.PlatformURL != origin+"/api/daemon/local" || detail.AuthToken != "" || detail.McpAuthToken != "" {
		return errors.New("local worker detail has foreign transport or controller credentials")
	}
	return daemon.ValidateLocalExecutionSecurityEvidence(detail.OrganizationID, detail.OperationalPayload, detail.HostAdaptationReceipt)
}

func localCallbackClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("local callback redirect refused") }}
}

// This transport trusts protected metadata owned by the selected local user;
// it does not claim cryptographic authentication of an arbitrary local process.
// No bearer is forwarded to a different origin or through a redirect.
type localOperatorTransport struct {
	configPath string
	origin     string
	base       http.RoundTripper
}

func (t *localOperatorTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method == http.MethodGet || request.Method == http.MethodHead {
		return t.base.RoundTrip(request)
	}
	if request.URL.Scheme+"://"+request.URL.Host != t.origin {
		return nil, errors.New("local control credential origin mismatch")
	}
	cfg, err := daemon.LoadConfig(t.configPath)
	if err != nil {
		return nil, errors.New("selected local daemon configuration is unavailable")
	}
	if cfg == nil {
		return nil, errors.New("selected local daemon configuration is absent")
	}
	root, err := localFileQueueRoot(cfg.Orchestrator.URL)
	if err != nil {
		return nil, err
	}
	auth, err := localruntimeauth.OpenExisting(root+".auth", root)
	if err != nil {
		return nil, errors.New("selected local operator authority is unavailable")
	}
	defer func() { _ = auth.Close() }()
	endpoint, err := auth.Endpoint()
	if err != nil || endpoint.Origin != t.origin {
		return nil, errors.New("selected local daemon endpoint binding mismatch")
	}
	identity := auth.Identity()
	if endpoint.ScopeID != identity.ScopeID || endpoint.HostID != identity.HostID || endpoint.WorkerID != identity.WorkerID {
		return nil, errors.New("selected local endpoint identity mismatch")
	}
	credential, err := auth.OperatorCredential()
	if err != nil {
		return nil, errors.New("selected local operator credential is unavailable")
	}
	cloned := request.Clone(request.Context())
	cloned.Header = request.Header.Clone()
	cloned.Header.Set("Authorization", "Bearer "+credential.BearerToken())
	return t.base.RoundTrip(cloned)
}

func localOperatorClient(cfg afclient.DaemonConfig, configPath string) (*http.Client, error) {
	settings, err := daemon.LoadConfig(configPath)
	if err != nil {
		return nil, err
	}
	if settings == nil || !strings.HasPrefix(settings.Orchestrator.URL, "file:") {
		return nil, nil
	}
	return &http.Client{Timeout: 10 * time.Second, Transport: &localOperatorTransport{configPath: configPath, origin: cfg.BaseURL(), base: http.DefaultTransport}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("local control redirect refused") }}, nil
}
