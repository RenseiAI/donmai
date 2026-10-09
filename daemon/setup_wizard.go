package daemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/RenseiAI/donmai/internal/statepath"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

// LocalRuntimeSetupProfile is a native host-login route discovered by the
// embedding CLI. Its catalog revision is empty for a built-in model pairing.
type LocalRuntimeSetupProfile struct {
	Label, Harness, Model, ModelAuthor, ModelCatalogRevision string
}

// LocalRuntimeSetupResolver supplies observed host and GitHub facts. Setup
// never derives immutable repository identity or model catalog authority from
// unverified text.
type LocalRuntimeSetupResolver interface {
	Profiles(context.Context) ([]LocalRuntimeSetupProfile, error)
	Repository(context.Context, string) (LocalGitHubRepository, error)
}

// LocalRuntimeSetupBranchVerifier is the additive local new-branch setup seam.
// Implementations verify repository.Ref as an actual branch under the observed
// immutable repository identity; a tag/SHA or missing branch cannot substitute.
// It does not alter the existing resolver's metadata/model methods.
type LocalRuntimeSetupBranchVerifier interface {
	VerifyBaseBranch(context.Context, LocalGitHubRepository) error
}

// WizardOptions configure the interactive setup wizard.
type WizardOptions struct {
	Context              context.Context
	LocalRuntimeResolver LocalRuntimeSetupResolver
	// Existing is an existing config (if any) used as defaults.
	Existing *Config
	// BinaryName is the invoking command name used in completion guidance.
	// Empty defaults to the standalone OSS binary name, donmai.
	BinaryName string
	// ConfigPath is where to write the resulting config. Empty means do not
	// persist.
	ConfigPath string
	// Stdin is the TTY input. Defaults to os.Stdin.
	Stdin io.Reader
	// Stdout is where prompts are printed. Defaults to os.Stdout.
	Stdout io.Writer
	// IsTTY overrides the auto-detected TTY status. When false (and not
	// explicitly set true), the wizard returns the default config without
	// prompting.
	IsTTY *bool
	// SkipWizard, when true, returns DefaultConfig (or Existing) without
	// prompting. Mirrors the DONMAI_DAEMON_SKIP_WIZARD env var.
	SkipWizard bool
	// CPUCount overrides runtime.NumCPU() (test injection).
	CPUCount int
	// MemoryMB overrides total-memory detection (test injection). 0 means
	// "use a sensible default".
	MemoryMB int
	// DetectGitRemote returns the cwd's git remote URL or "" if none. Tests
	// inject a stub.
	DetectGitRemote func() string
}

// ServiceModeStdin reports whether the process runs under a service manager
// with no terminal attached: stdin is /dev/null (a character device that is
// not a terminal) or is otherwise not a terminal at all. A mode-bit test
// alone reads /dev/null as a terminal because /dev/null IS a character
// device, so a service-mode start whose stdin is /dev/null ran the
// interactive wizard and failed on its first required answer instead of
// taking the non-interactive path.
func ServiceModeStdin() bool {
	return !term.IsTerminal(int(os.Stdin.Fd()))
}

// ShouldSkipWizard returns true when the wizard should be bypassed:
//   - stdin is not a terminal (including the service-mode /dev/null
//     case — see ServiceModeStdin), OR
//   - DONMAI_DAEMON_SKIP_WIZARD is set.
func ShouldSkipWizard() bool {
	if os.Getenv("DONMAI_DAEMON_SKIP_WIZARD") != "" {
		return true
	}
	return ServiceModeStdin()
}

// RunSetupWizard runs the interactive first-run wizard (or returns the
// non-interactive default when stdin is not a TTY).
func RunSetupWizard(opts WizardOptions) (*Config, error) {
	skip := opts.SkipWizard
	if !skip {
		if opts.IsTTY != nil {
			skip = !*opts.IsTTY
		} else {
			skip = ShouldSkipWizard()
		}
	}
	if skip {
		return BuildDefaultConfigFromExisting(opts.Existing, opts.ConfigPath)
	}
	binaryName := strings.TrimSpace(opts.BinaryName)
	if binaryName == "" {
		binaryName = "donmai"
	}

	in := opts.Stdin
	if in == nil {
		in = os.Stdin
	}
	out := opts.Stdout
	if out == nil {
		out = os.Stdout
	}
	r := bufio.NewReader(in)
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}

	cpu := opts.CPUCount
	if cpu == 0 {
		cpu = runtime.NumCPU()
	}
	mem := opts.MemoryMB
	if mem == 0 {
		mem = 16 * 1024 // 16 GB default for the heuristics; not life-critical.
	}

	wln := func(msg string) { _, _ = fmt.Fprintln(out, msg) }
	wf := func(format string, args ...any) { _, _ = fmt.Fprintf(out, format, args...) }

	wln("\nLet's get your machine working.")

	// [1/5] Machine identity
	wln("\n[1/5] Machine identity")
	defaultID := DeriveDefaultMachineID()
	defaultRegion := "home-network"
	if opts.Existing != nil {
		if opts.Existing.Machine.ID != "" {
			defaultID = opts.Existing.Machine.ID
		}
		if opts.Existing.Machine.Region != "" {
			defaultRegion = opts.Existing.Machine.Region
		}
	}
	machineID, err := promptDefault(r, out, "Machine ID (auto-generated)", defaultID)
	if err != nil {
		return nil, err
	}
	region, err := promptDefault(r, out, "Region (helps the scheduler with latency)", defaultRegion)
	if err != nil {
		return nil, err
	}
	if !confirmYes(r, out, "  Continue?", true) {
		return nil, errors.New("setup wizard cancelled by user")
	}

	// [2/5] Capacity
	wln("\n[2/5] Capacity")
	wf("  Detected: %d cores, %d MB RAM\n", cpu, mem)
	defaultReservedCores := minOf(4, cpu/4)
	defaultReservedMem := minOf(16384, mem/4)
	defaultMaxSess := defaultMaxSessions(cpu)
	if opts.Existing != nil {
		if opts.Existing.Capacity.ReservedForSystem.VCpu > 0 {
			defaultReservedCores = opts.Existing.Capacity.ReservedForSystem.VCpu
		}
		if opts.Existing.Capacity.ReservedForSystem.MemoryMb > 0 {
			defaultReservedMem = opts.Existing.Capacity.ReservedForSystem.MemoryMb
		}
		if opts.Existing.Capacity.MaxConcurrentSessions > 0 {
			defaultMaxSess = opts.Existing.Capacity.MaxConcurrentSessions
		}
	}
	reservedCores, err := promptInt(r, out, "Reserve cores for system", defaultReservedCores)
	if err != nil {
		return nil, err
	}
	reservedMem, err := promptInt(r, out, "Reserve memory MB for system", defaultReservedMem)
	if err != nil {
		return nil, err
	}
	maxSess, err := promptInt(r, out, "Max concurrent sessions", defaultMaxSess)
	if err != nil {
		return nil, err
	}
	if !confirmYes(r, out, "  Continue?", true) {
		return nil, errors.New("setup wizard cancelled by user")
	}

	// [3/5] Orchestrator
	wln("\n[3/5] Orchestrator")
	wln("  Where do work assignments come from?")
	wln("  > 1. Remote orchestrator           — register with a platform you configure")
	wln("    2. Local file queue (single-user) — for solo dev, no network")
	choiceDefault := "1"
	if opts.Existing != nil && strings.HasPrefix(opts.Existing.Orchestrator.URL, "file:") {
		choiceDefault = "2"
	}
	choiceStr, err := promptDefault(r, out, "Choice", choiceDefault)
	if err != nil {
		return nil, err
	}
	choice, err := strconv.Atoi(choiceStr)
	if err != nil || (choice != 1 && choice != 2) {
		return nil, errors.New("orchestrator choice must be 1 or 2")
	}
	var orchestratorURL string
	authToken := ""
	if opts.Existing != nil {
		authToken = opts.Existing.Orchestrator.AuthToken
	}
	switch choice {
	case 2:
		queue := statepath.Resolve("queue", "/tmp/.donmai/queue")
		orchestratorURL = "file://" + queue
		if opts.Existing != nil && strings.HasPrefix(opts.Existing.Orchestrator.URL, "file:") {
			if !canonicalLocalQueueURL(opts.Existing.Orchestrator.URL) {
				return nil, errors.New("existing local file queue URL is not canonical")
			}
			orchestratorURL = opts.Existing.Orchestrator.URL
			queue = strings.TrimPrefix(orchestratorURL, "file://")
		}
		wf("  Using local file queue at %s\n", queue)
		authToken = ""
	case 1:
		// No vendor default — the operator supplies the orchestrator URL.
		def := ""
		if opts.Existing != nil && opts.Existing.Orchestrator.URL != "" && !strings.HasPrefix(opts.Existing.Orchestrator.URL, "file:") {
			def = opts.Existing.Orchestrator.URL
		}
		orchestratorURL, err = promptDefault(r, out, "Orchestrator URL", def)
		if err != nil {
			return nil, err
		}
		if orchestratorURL == "" || strings.HasPrefix(orchestratorURL, "file:") {
			return nil, errors.New("remote orchestrator URL must be nonempty and must not use file mode")
		}
		envTok := os.Getenv("DONMAI_DAEMON_TOKEN")
		switch {
		case envTok == "" && authToken == "":
			tok, err := promptDefault(r, out, "Registration token (rsp_live_...)", "")
			if err != nil {
				return nil, err
			}
			if tok != "" {
				authToken = tok
			}
		case envTok != "":
			authToken = envTok
			wln("  Using registration token from DONMAI_DAEMON_TOKEN env var")
		default:
			wln("  Using registration token from existing config")
		}
	}
	if !confirmYes(r, out, "  Continue?", true) {
		return nil, errors.New("setup wizard cancelled by user")
	}

	var localRuntime *LocalRuntimeConfig
	if choice == 2 {
		localRuntime, err = configureLocalRuntime(ctx, r, out, opts)
		if err != nil {
			return nil, fmt.Errorf("configure local runtime: %w", err)
		}
	}

	// [4/5] Project allowlist
	wln("\n[4/5] Project allowlist")
	projects := []ProjectConfig{}
	if opts.Existing != nil {
		projects = append(projects, opts.Existing.EffectiveProjectConfigs()...)
	}
	detect := opts.DetectGitRemote
	if detect == nil {
		detect = detectGitRemote
	}
	if remote := detect(); remote != "" && choice != 2 {
		repo := remoteToRepository(remote)
		alreadyAdded := false
		for _, p := range projects {
			if p.Repository == repo {
				alreadyAdded = true
				break
			}
		}
		if !alreadyAdded {
			if confirmYes(r, out, fmt.Sprintf("  Detected: %s  [add?]", repo), true) {
				id := repo
				if i := strings.LastIndex(repo, "/"); i >= 0 {
					id = repo[i+1:]
				}
				projects = append(projects, ProjectConfig{ID: id, Repository: repo})
			}
		}
	}
	if choice != 2 && confirmYes(r, out, "  Add another project?", false) {
		repoURL, err := promptDefault(r, out, "Repository (e.g. github.com/org/repo)", "")
		if err != nil {
			return nil, err
		}
		if repoURL != "" {
			id := repoURL
			if i := strings.LastIndex(repoURL, "/"); i >= 0 {
				id = repoURL[i+1:]
			}
			projects = append(projects, ProjectConfig{ID: id, Repository: repoURL})
		}
	}
	// Standing consent, asked ONCE, at the only moment the machine owner is
	// already thinking about what this machine is for. The alternative — the
	// enumerated default — means re-answering this same question for every
	// project, forever, on every machine.
	//
	// An existing config keeps whatever it already chose: a re-run of the
	// wizard must never silently widen a machine the owner deliberately
	// narrowed.
	admissionDefaultAllRouted := true
	if opts.Existing != nil {
		admissionDefaultAllRouted = opts.Existing.AdmitsAnyRoutedProject()
	}
	admissionMode := ProjectAdmissionModeEnumerated
	if choice != 2 {
		wln("\n  Project admission")
		wln("    all-routed  any project your organization routes to this machine runs here")
		wln("    enumerated  only projects you allow on this machine, one by one")
		if confirmYes(r, out, "  Accept any project routed to this machine?", admissionDefaultAllRouted) {
			admissionMode = ProjectAdmissionModeAllRouted
		}
	} else {
		wln("  Local intake is limited to the configured GitHub repository and issue label.")
	}
	if !confirmYes(r, out, "  Continue?", true) {
		return nil, errors.New("setup wizard cancelled by user")
	}

	// [5/5] Auto-update
	wln("\n[5/5] Auto-update")
	defChannel := string(ChannelStable)
	defSchedule := string(ScheduleNightly)
	defDrain := 600
	if opts.Existing != nil {
		if opts.Existing.AutoUpdate.Channel != "" {
			defChannel = string(opts.Existing.AutoUpdate.Channel)
		}
		if opts.Existing.AutoUpdate.Schedule != "" {
			defSchedule = string(opts.Existing.AutoUpdate.Schedule)
		}
		if opts.Existing.AutoUpdate.DrainTimeoutSeconds > 0 {
			defDrain = opts.Existing.AutoUpdate.DrainTimeoutSeconds
		}
	}
	channelStr, err := promptDefault(r, out, "Channel (stable/beta/main)", defChannel)
	if err != nil {
		return nil, err
	}
	scheduleStr, err := promptDefault(r, out, "Schedule (nightly/on-release/manual)", defSchedule)
	if err != nil {
		return nil, err
	}
	drain, err := promptInt(r, out, "Drain timeout seconds", defDrain)
	if err != nil {
		return nil, err
	}

	channel := UpdateChannel(channelStr)
	switch channel {
	case ChannelStable, ChannelBeta, ChannelMain:
	default:
		channel = ChannelStable
	}
	schedule := UpdateSchedule(scheduleStr)
	switch schedule {
	case ScheduleNightly, ScheduleOnRelease, ScheduleManual:
	default:
		schedule = ScheduleNightly
	}

	cfg := &Config{
		APIVersion:              "donmai.dev/v1",
		Kind:                    "LocalDaemon",
		ProjectAdmissionVersion: ProjectAdmissionVersionV2,
		ProjectAdmissionMode:    admissionMode,
		EnabledProjectIDs:       projectIDsFromRepositories(projects),
		Repositories:            normalizeRepositories(nil, projects),
		Machine:                 MachineConfig{ID: machineID, Region: region},
		Capacity: CapacityConfig{
			MaxConcurrentSessions: maxSess,
			MaxVCpuPerSession:     4,
			MaxMemoryMbPerSession: 8192,
			ReservedForSystem: ReservedSystemSpec{
				VCpu:     reservedCores,
				MemoryMb: reservedMem,
			},
		},
		Orchestrator: OrchestratorConfig{URL: orchestratorURL, AuthToken: authToken},
		AutoUpdate: AutoUpdateConfig{
			Channel:             channel,
			Schedule:            schedule,
			DrainTimeoutSeconds: drain,
		},
	}
	if choice == 2 {
		cfg.APIVersion = LocalRuntimeConfigAPIVersion
		cfg.LocalRuntime = localRuntime
		if err := ValidateLocalExecutionSecurity(cfg); err != nil {
			return nil, err
		}
	}
	if opts.Existing != nil {
		// Preserve per-session caps from existing config when present.
		if opts.Existing.Capacity.MaxVCpuPerSession > 0 {
			cfg.Capacity.MaxVCpuPerSession = opts.Existing.Capacity.MaxVCpuPerSession
		}
		if opts.Existing.Capacity.MaxMemoryMbPerSession > 0 {
			cfg.Capacity.MaxMemoryMbPerSession = opts.Existing.Capacity.MaxMemoryMbPerSession
		}
	}
	applyDefaults(cfg)
	if err := validateConfig(cfg); err != nil {
		return nil, fmt.Errorf("validate setup config: %w", err)
	}

	if opts.ConfigPath != "" {
		if err := WriteConfig(opts.ConfigPath, cfg); err != nil {
			return nil, fmt.Errorf("write config: %w", err)
		}
		wf("Setup complete. Config written to %s\n", opts.ConfigPath)
	}
	wf("  Status: %s host status\n", binaryName)
	wf("  Logs:   %s host logs\n", binaryName)
	wf("  Stop:   %s host stop\n", binaryName)

	return cfg, nil
}

func canonicalLocalQueueURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "file" && parsed.Host == "" && parsed.Opaque == "" && parsed.User == nil && parsed.RawQuery == "" && !parsed.ForceQuery && parsed.Fragment == "" && parsed.RawPath == "" && filepath.IsAbs(parsed.Path) && filepath.Clean(parsed.Path) == parsed.Path
}

func validLocalIssueSelection(label, ref string) bool {
	return label != "" && len(label) <= 128 && strings.TrimSpace(label) == label && ref != "" && len(ref) <= 256 && strings.TrimSpace(ref) == ref && !strings.Contains(ref, "..") && !strings.ContainsAny(ref, " \t\r\n")
}

func configureLocalRuntime(ctx context.Context, r *bufio.Reader, out io.Writer, opts WizardOptions) (*LocalRuntimeConfig, error) {
	if opts.LocalRuntimeResolver == nil {
		return nil, errors.New("local setup requires a native profile and GitHub repository resolver")
	}
	var previous *LocalRuntimeConfig
	if opts.Existing != nil {
		switch opts.Existing.APIVersion {
		case "donmai.dev/v1":
			// This is an explicit operator transition from a known v1 config.
		case LocalRuntimeConfigAPIVersion:
			if err := ValidateLocalExecutionSecurity(opts.Existing); err != nil {
				return nil, err
			}
			previous = opts.Existing.LocalRuntime
			if previous.Harness != "codex" && previous.Harness != "claude-code" {
				return nil, errors.New("existing local harness is invalid")
			}
			if strings.TrimSpace(previous.Model) == "" || strings.TrimSpace(previous.ModelAuthor) == "" || len(previous.Repositories) == 0 || len(previous.Repositories) > 128 {
				return nil, errors.New("existing local profile or repository is invalid")
			}
			if err := validateLocalRepositories(previous.Repositories); err != nil {
				return nil, err
			}
		default:
			return nil, errors.New("unknown existing config schema; local setup refuses to guess a migration")
		}
	}
	profiles, err := opts.LocalRuntimeResolver.Profiles(ctx)
	if err != nil {
		return nil, fmt.Errorf("discover native profiles: %w", err)
	}
	if len(profiles) == 0 {
		return nil, errors.New("no installed and authenticated native harness with a built-in host-login model")
	}
	defaultChoice := 1
	_, _ = fmt.Fprintln(out, "\n  Installed and authenticated native profiles:")
	for i, profile := range profiles {
		if profile.Label == "" || (profile.Harness != "codex" && profile.Harness != "claude-code") || profile.Model == "" || profile.ModelAuthor == "" || profile.ModelCatalogRevision != "" {
			return nil, errors.New("native profile resolver returned an invalid built-in pairing")
		}
		_, _ = fmt.Fprintf(out, "    %d. %s (%s, %s/%s)\n", i+1, profile.Label, profile.Harness, profile.ModelAuthor, profile.Model)
		if previous != nil && previous.Harness == profile.Harness && previous.Model == profile.Model && previous.ModelAuthor == profile.ModelAuthor && previous.ModelCatalogRevision == profile.ModelCatalogRevision {
			defaultChoice = i + 1
		}
	}
	if previous != nil && (profiles[defaultChoice-1].Harness != previous.Harness || profiles[defaultChoice-1].Model != previous.Model || profiles[defaultChoice-1].ModelAuthor != previous.ModelAuthor || profiles[defaultChoice-1].ModelCatalogRevision != previous.ModelCatalogRevision) {
		return nil, errors.New("existing native profile is no longer installed, authenticated, or in the built-in catalog")
	}
	choice, err := promptInt(r, out, "Native profile number", defaultChoice)
	if err != nil {
		return nil, err
	}
	if choice < 1 || choice > len(profiles) {
		return nil, errors.New("native profile number is out of range")
	}
	selected := profiles[choice-1]

	defaultRepo := ""
	if previous != nil {
		defaultRepo = previous.Repositories[0].OwnerRepo
	} else {
		detect := opts.DetectGitRemote
		if detect == nil {
			detect = detectGitRemote
		}
		if remote := remoteToRepository(detect()); strings.HasPrefix(remote, "github.com/") {
			defaultRepo = strings.TrimPrefix(remote, "github.com/")
		}
	}
	ownerRepo, err := promptDefault(r, out, "GitHub owner/repository", defaultRepo)
	if err != nil {
		return nil, err
	}
	if ownerRepo == "" || strings.ContainsAny(ownerRepo, " \t\r\n") {
		return nil, errors.New("GitHub owner/repository is required")
	}
	repository, err := opts.LocalRuntimeResolver.Repository(ctx, ownerRepo)
	if err != nil {
		return nil, fmt.Errorf("verify GitHub repository: %w", err)
	}
	if repository.RepositoryID <= 0 || repository.OwnerRepo == "" || repository.Ref == "" {
		return nil, errors.New("GitHub repository metadata lacks immutable ID, canonical name, or default ref")
	}
	if previous != nil && previous.Repositories[0].RepositoryID != repository.RepositoryID {
		return nil, errors.New("existing GitHub repository ID changed")
	}
	labelDefault := "donmai"
	refDefault := repository.Ref
	if previous != nil {
		labelDefault = previous.Repositories[0].Label
		if previous.Repositories[0].Ref != "" {
			refDefault = previous.Repositories[0].Ref
		}
	}
	label, err := promptDefault(r, out, "Issue label to watch", labelDefault)
	if err != nil {
		return nil, err
	}
	ref, err := promptDefault(r, out, "Base branch for new PRs", refDefault)
	if err != nil {
		return nil, err
	}
	if !validLocalIssueSelection(label, ref) {
		return nil, errors.New("GitHub issue label or ref is invalid")
	}
	repository.Label, repository.Ref = label, ref
	verifier, ok := opts.LocalRuntimeResolver.(LocalRuntimeSetupBranchVerifier)
	if !ok {
		return nil, errors.New("local setup requires verified GitHub base-branch support")
	}
	if err = worktree.ValidateBranchBaseRef(ref); err != nil {
		return nil, err
	}
	if err = verifier.VerifyBaseBranch(ctx, repository); err != nil {
		return nil, fmt.Errorf("verify GitHub base branch: %w", err)
	}
	repositories := []LocalGitHubRepository{repository}
	if previous != nil {
		for _, configured := range previous.Repositories[1:] {
			if !validLocalIssueSelection(configured.Label, configured.Ref) {
				return nil, errors.New("configured GitHub issue label or ref is invalid")
			}
			observed, err := opts.LocalRuntimeResolver.Repository(ctx, configured.OwnerRepo)
			if err != nil {
				return nil, fmt.Errorf("verify configured GitHub repository: %w", err)
			}
			if observed.RepositoryID != configured.RepositoryID || observed.RepositoryID <= 0 || observed.OwnerRepo == "" || observed.Ref == "" {
				return nil, errors.New("configured GitHub repository identity changed or metadata is incomplete")
			}
			configured.OwnerRepo = observed.OwnerRepo
			if err = worktree.ValidateBranchBaseRef(configured.Ref); err != nil {
				return nil, err
			}
			if err = verifier.VerifyBaseBranch(ctx, configured); err != nil {
				return nil, fmt.Errorf("verify configured GitHub base branch: %w", err)
			}
			repositories = append(repositories, configured)
		}
	}
	if err := validateLocalRepositories(repositories); err != nil {
		return nil, err
	}
	policy := InitialLocalExecutionSecurity()
	if previous != nil {
		policy = previous.ExecutionSecurity
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	_, _ = fmt.Fprintln(out, "\n  Local execution security (explicit policy):")
	_, _ = fmt.Fprintf(out, "    toolApproval: %s\n    fileRead: %s\n    fileWrite: %s\n    network: %s\n    credentials: %s\n    isolation: %s\n", policy.ToolApproval, policy.FileRead, policy.FileWrite, policy.Network, policy.Credentials, policy.Isolation)
	_, _ = fmt.Fprintln(out, "  host-user is uncontained. ambient-host-login lets the harness use logins available to this OS user.")
	for _, configured := range repositories {
		_, _ = fmt.Fprintf(out, "  GitHub intake: %s (repository ID %d), label %q, ref %q\n", configured.OwnerRepo, configured.RepositoryID, configured.Label, configured.Ref)
	}
	return &LocalRuntimeConfig{ExecutionSecurity: policy, Harness: selected.Harness, Model: selected.Model, ModelAuthor: selected.ModelAuthor, ModelCatalogRevision: selected.ModelCatalogRevision, Repositories: repositories}, nil
}

// FreshHostQueueURL is the file-queue URL a fresh host seeds when the
// operator never authored a config: the local file-queue root under the
// host state home. Exported so `host install` seeds the same URL the
// non-interactive first-run path does.
func FreshHostQueueURL() string {
	return "file://" + statepath.Resolve("queue", "/tmp/.donmai/queue")
}

// FreshHostConfig returns the seed config a fresh host starts from: the
// local file queue plus the explicit execution-security seed the startup
// migration would apply. No harness or repository profile — `host setup`
// adds those. The seed loads under validateConfig and starts without the
// interactive wizard; composition refuses with the operator action until
// setup completes.
func FreshHostConfig() *Config {
	cfg := DefaultConfig()
	cfg.APIVersion = LocalRuntimeConfigAPIVersion
	cfg.Orchestrator.URL = FreshHostQueueURL()
	cfg.LocalRuntime = &LocalRuntimeConfig{ExecutionSecurity: InitialLocalExecutionSecurity()}
	applyDefaults(cfg)
	return cfg
}

// BuildDefaultConfigFromExisting returns a default Config (or the existing
// one) and optionally persists it to configPath.
//
// A fresh host whose operator never authored a config gets a config the
// daemon can actually load: DefaultConfig carries an empty orchestrator URL
// (no vendor default — the operator configures it explicitly), which
// validateConfig refuses. Seeding the local file-queue URL plus the
// explicit execution-security seed the startup migration would apply keeps
// a service-mode start on a fresh host out of the crash loop: with no
// harness or repository profile the file queue still refuses at
// composition time, but with a typed error the operator can act on, not a
// reopened prompt.
func BuildDefaultConfigFromExisting(existing *Config, configPath string) (*Config, error) {
	cfg := existing
	if cfg == nil {
		// No operator config and no explicit orchestrator URL: seed the
		// fresh-host file queue (with its explicit execution policy)
		// instead of an empty URL no authored config could ever load.
		// An explicitly configured URL (env or otherwise) keeps the
		// plain default so stub and platform paths behave as before.
		if DefaultConfig().Orchestrator.URL == "" {
			cfg = FreshHostConfig()
		} else {
			cfg = DefaultConfig()
		}
	} else {
		applyDefaults(cfg)
	}
	if strings.HasPrefix(cfg.Orchestrator.URL, "file:") {
		if err := ValidateLocalExecutionSecurity(cfg); err != nil {
			return nil, err
		}
	}
	if configPath != "" {
		if err := WriteConfig(configPath, cfg); err != nil {
			return nil, fmt.Errorf("write default config: %w", err)
		}
	}
	return cfg, nil
}

// ── prompt helpers ───────────────────────────────────────────────────────

func promptDefault(r *bufio.Reader, w io.Writer, question, def string) (string, error) {
	_, _ = fmt.Fprintf(w, "  %s [%s]: ", question, def)
	line, err := readLine(r)
	if err != nil {
		return "", err
	}
	if line == "" {
		return def, nil
	}
	return line, nil
}

func promptInt(r *bufio.Reader, w io.Writer, question string, def int) (int, error) {
	s, err := promptDefault(r, w, question, strconv.Itoa(def))
	if err != nil {
		return 0, err
	}
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def, nil
	}
	return n, nil
}

func confirmYes(r *bufio.Reader, w io.Writer, question string, defaultYes bool) bool {
	hint := "[Y/n]"
	if !defaultYes {
		hint = "[y/N]"
	}
	_, _ = fmt.Fprintf(w, "%s %s: ", question, hint)
	line, err := readLine(r)
	if err != nil {
		return defaultYes
	}
	if line == "" {
		return defaultYes
	}
	return strings.HasPrefix(strings.ToLower(line), "y")
}

func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func detectGitRemote() string {
	out, err := exec.Command("git", "remote", "get-url", "origin").Output() //nolint:gosec
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// remoteToRepository normalises a git remote URL to "host/org/repo" form.
func remoteToRepository(remote string) string {
	out := remote
	out = strings.TrimPrefix(out, "git@")
	out = strings.TrimPrefix(out, "https://")
	out = strings.TrimPrefix(out, "http://")
	// First colon → slash (ssh hosts).
	if i := strings.Index(out, ":"); i >= 0 {
		out = out[:i] + "/" + out[i+1:]
	}
	out = strings.TrimSuffix(out, ".git")
	return out
}

func minOf(a, b int) int {
	if a < b {
		return a
	}
	return b
}
