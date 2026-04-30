package daemon

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// WizardOptions configure the interactive setup wizard.
type WizardOptions struct {
	// Existing is an existing config (if any) used as defaults.
	Existing *Config
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
	// prompting. Mirrors the RENSEI_DAEMON_SKIP_WIZARD env var.
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

// ShouldSkipWizard returns true when the wizard should be bypassed:
//   - stdin is not a TTY, OR
//   - RENSEI_DAEMON_SKIP_WIZARD is set.
func ShouldSkipWizard() bool {
	if os.Getenv("RENSEI_DAEMON_SKIP_WIZARD") != "" {
		return true
	}
	// We treat non-TTY stdin as "skip the wizard". We use a coarse fallback
	// — we don't pull in golang.org/x/term to keep the dependency surface
	// minimal — instead we Stat() stdin and check the mode.
	fi, err := os.Stdin.Stat()
	if err != nil {
		return true
	}
	return (fi.Mode() & os.ModeCharDevice) == 0
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

	in := opts.Stdin
	if in == nil {
		in = os.Stdin
	}
	out := opts.Stdout
	if out == nil {
		out = os.Stdout
	}
	r := bufio.NewReader(in)

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

	wln("\nWelcome to Rensei. Let's get your machine working.")

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
	wln("  > 1. Rensei Platform (SaaS)        — register with platform.rensei.dev")
	wln("    2. Self-hosted (OSS only)         — point at your own webhook target")
	wln("    3. Local file queue (single-user) — for solo dev, no network")
	choiceStr, err := promptDefault(r, out, "Choice", "1")
	if err != nil {
		return nil, err
	}
	choice, _ := strconv.Atoi(choiceStr)
	var orchestratorURL string
	authToken := ""
	if opts.Existing != nil {
		authToken = opts.Existing.Orchestrator.AuthToken
	}
	switch choice {
	case 2:
		def := "https://your-rensei-instance.example.com"
		if opts.Existing != nil && opts.Existing.Orchestrator.URL != "" {
			def = opts.Existing.Orchestrator.URL
		}
		orchestratorURL, err = promptDefault(r, out, "Orchestrator URL", def)
		if err != nil {
			return nil, err
		}
	case 3:
		home, _ := os.UserHomeDir()
		queue := filepath.Join(home, ".rensei", "queue")
		orchestratorURL = "file://" + queue
		wf("  Using local file queue at %s\n", queue)
	default:
		orchestratorURL = "https://platform.rensei.dev"
		envTok := os.Getenv("RENSEI_DAEMON_TOKEN")
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
			wln("  Using registration token from RENSEI_DAEMON_TOKEN env var")
		default:
			wln("  Using registration token from existing config")
		}
	}
	if !confirmYes(r, out, "  Continue?", true) {
		return nil, errors.New("setup wizard cancelled by user")
	}

	// [4/5] Project allowlist
	wln("\n[4/5] Project allowlist")
	projects := []ProjectConfig{}
	if opts.Existing != nil {
		projects = append(projects, opts.Existing.Projects...)
	}
	detect := opts.DetectGitRemote
	if detect == nil {
		detect = detectGitRemote
	}
	if remote := detect(); remote != "" {
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
				projects = append(projects, ProjectConfig{ID: id, Repository: repo, CloneStrategy: CloneShallow})
			}
		}
	}
	if confirmYes(r, out, "  Add another project?", false) {
		repoURL, err := promptDefault(r, out, "Repository (e.g. github.com/org/repo)", "")
		if err != nil {
			return nil, err
		}
		if repoURL != "" {
			id := repoURL
			if i := strings.LastIndex(repoURL, "/"); i >= 0 {
				id = repoURL[i+1:]
			}
			projects = append(projects, ProjectConfig{ID: id, Repository: repoURL, CloneStrategy: CloneShallow})
		}
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
		APIVersion: "rensei.dev/v1",
		Kind:       "LocalDaemon",
		Machine:    MachineConfig{ID: machineID, Region: region},
		Capacity: CapacityConfig{
			MaxConcurrentSessions: maxSess,
			MaxVCpuPerSession:     4,
			MaxMemoryMbPerSession: 8192,
			ReservedForSystem: ReservedSystemSpec{
				VCpu:     reservedCores,
				MemoryMb: reservedMem,
			},
		},
		Projects:     projects,
		Orchestrator: OrchestratorConfig{URL: orchestratorURL, AuthToken: authToken},
		AutoUpdate: AutoUpdateConfig{
			Channel:             channel,
			Schedule:            schedule,
			DrainTimeoutSeconds: drain,
		},
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

	if opts.ConfigPath != "" {
		if err := WriteConfig(opts.ConfigPath, cfg); err != nil {
			return nil, fmt.Errorf("write config: %w", err)
		}
		wf("Setup complete. Config written to %s\n", opts.ConfigPath)
	}
	wln("  Status: rensei daemon status")
	wln("  Logs:   rensei daemon logs")
	wln("  Stop:   rensei daemon stop")

	return cfg, nil
}

// BuildDefaultConfigFromExisting returns a default Config (or the existing
// one) and optionally persists it to configPath.
func BuildDefaultConfigFromExisting(existing *Config, configPath string) (*Config, error) {
	cfg := existing
	if cfg == nil {
		cfg = DefaultConfig()
	} else {
		applyDefaults(cfg)
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
