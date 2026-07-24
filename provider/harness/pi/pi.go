package pi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// newNonce returns a random hex nonce used to correlate the handshake and
// subsequent adjudication round-trips.
func newNonce() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "pi-nonce-fallback"
	}
	return hex.EncodeToString(b)
}

// Compile-time assertion: pi satisfies the base Provider contract.
var _ agent.Provider = (*Provider)(nil)

// Provider is the agent.Provider for the pi harness. Unlike codex (one
// long-lived app-server, N threads) pi is one child per session, so the
// Provider itself holds no subprocess — it probes the binary + version pin at
// construction and spawns a fresh `pi --mode rpc` child per Spawn/Resume.
type Provider struct {
	opts Options

	// binary is the resolved pi binary path (empty in skipProcess tests).
	binary string
	// unverified is set when the probed version fell outside
	// [MinVersion, VerifiedAgainst] (DEC-2: label, don't block). Each session
	// emits one SystemEvent{unverified_harness_version} when true.
	unverified bool
}

// Options configures Provider construction. The empty value runs `pi` from
// PATH.
type Options struct {
	// PiBin is the pi binary path. Defaults to $PI_BIN, then "pi" via $PATH.
	PiBin string

	// VersionProbe overrides the "--version" probe (tests).
	VersionProbe versionProbeFunc

	// HandshakeTimeout caps how long Spawn waits for the policy-extension
	// handshake before failing closed. Defaults to 10s (design §2 step 3).
	HandshakeTimeout time.Duration

	// VersionProbeTimeout caps the construction-time version probe.
	VersionProbeTimeout time.Duration

	// Test seams. skipProcess wires stdin/stdout overrides instead of execing
	// a real child; used by the pipe-stub tests that replay pi RPC shapes.
	skipProcess    bool
	stdinOverride  io.Writer
	stdoutOverride io.Reader
}

// New probes the pi binary and enforces the version pin (probe-time, per
// design §2 / opencode §8). A confirmed-below-MinVersion binary fails
// construction with agent.ErrProviderUnavailable; an unverifiable/above
// version proceeds but marks the Provider so every session is labeled.
func New(opts Options) (*Provider, error) {
	if opts.HandshakeTimeout == 0 {
		opts.HandshakeTimeout = 10 * time.Second
	}
	if opts.VersionProbeTimeout == 0 {
		opts.VersionProbeTimeout = DefaultVersionProbeTimeout
	}
	if opts.VersionProbe == nil {
		opts.VersionProbe = defaultVersionProbe
	}
	p := &Provider{opts: opts}

	if opts.skipProcess {
		p.binary = "pi"
		return p, nil
	}

	full, err := resolvePiBinary(opts.PiBin)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrProviderUnavailable, err)
	}
	p.binary = full

	ctx, cancel := context.WithTimeout(context.Background(), opts.VersionProbeTimeout)
	defer cancel()
	unverified, perr := checkVersionPin(ctx, opts.VersionProbe, full)
	if perr != nil {
		return nil, perr // already wraps ErrProviderUnavailable
	}
	p.unverified = unverified
	return p, nil
}

// Name implements agent.Provider.
func (p *Provider) Name() agent.ProviderName { return agent.ProviderPi }

// Spawn starts a new pi session. The Handle's Events channel emits exactly one
// InitEvent (from agent_start), then session events, then exactly one terminal
// event, then closes. Fail-closed: the prompt is NEVER sent unless the policy
// extension materialized AND its handshake verified (design §2 step 3).
func (p *Provider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	return p.launch(ctx, spec, launchPrompt, "")
}

// Resume re-execs `pi --mode rpc` against the persisted session file and
// replays entries from the caller's cursor (design §4). Capability-gated on
// SupportsSessionResume.
//
// NOTE (untested): the get_entries cursor replay + dedup path is implemented
// but not verified against a real binary; the donmai-smokes step20 resume item
// is its acceptance gate.
func (p *Provider) Resume(ctx context.Context, sessionID string, spec agent.Spec) (agent.Handle, error) {
	if sessionID == "" {
		return nil, agent.ErrSessionNotFound
	}
	return p.launch(ctx, spec, launchResume, sessionID)
}

// launchMode selects the post-handshake bring-up.
type launchMode int

const (
	launchPrompt launchMode = iota
	launchResume
)

func (p *Provider) launch(ctx context.Context, spec agent.Spec, mode launchMode, sessionID string) (agent.Handle, error) {
	spec, err := applyEndpoint(spec)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
	}

	// Materialize the policy extension + provider pin BEFORE spawning. A
	// materialization failure means no boundary — fail closed.
	layout, err := materializeExtension(spec.Cwd, spec.Endpoint, spec.Model)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
	}

	var (
		cmd    *exec.Cmd
		stdin  io.Writer
		stdout io.Reader
	)
	if p.opts.skipProcess {
		stdin = p.opts.stdinOverride
		stdout = p.opts.stdoutOverride
	} else {
		c, in, out, serr := p.spawnChild(spec, layout)
		if serr != nil {
			return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, serr)
		}
		cmd, stdin, stdout = c, in, out
	}

	client := newRPCClient(stdin, stdout)
	h := newHandle(client, cmd, spec, newNonce())
	go h.run()

	// Fail-closed handshake gate.
	select {
	case herr := <-h.handshakeResult:
		if herr != nil {
			_ = h.Stop(context.Background())
			return nil, fmt.Errorf("%w: policy extension failed to load: %v", agent.ErrSpawnFailed, herr)
		}
	case <-time.After(p.opts.HandshakeTimeout):
		_ = h.Stop(context.Background())
		return nil, fmt.Errorf("%w: policy extension failed to load (no handshake within %s)", agent.ErrSpawnFailed, p.opts.HandshakeTimeout)
	case <-ctx.Done():
		_ = h.Stop(context.Background())
		return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, ctx.Err())
	}

	// Handshake verified — the boundary is live. Label unverified versions.
	if p.unverified {
		h.emit(agent.SystemEvent{
			Subtype: unverifiedVersionSubtype,
			Message: fmt.Sprintf("pi binary version could not be confirmed within [%s, %s]; session proceeds labeled unverified", MinVersion, VerifiedAgainst),
		})
	}

	// Pin the resolved model + reasoning effort, then bring up the turn.
	if spec.Model != "" {
		_ = client.WriteCommand(map[string]any{"command": "set_model", "model": "donmai/" + spec.Model})
	}
	if lvl := thinkingLevelForEffort(spec.Effort); lvl != "" {
		_ = client.WriteCommand(map[string]any{"command": "set_thinking_level", "level": lvl})
	}

	switch mode {
	case launchResume:
		if err := client.WriteCommand(map[string]any{"command": "get_entries", "session": sessionID, "since": sessionID}); err != nil {
			_ = h.Stop(context.Background())
			return nil, fmt.Errorf("%w: pi resume get_entries: %v", agent.ErrSpawnFailed, err)
		}
	default:
		if err := client.WriteCommand(map[string]any{"command": "prompt", "text": spec.Prompt}); err != nil {
			_ = h.Stop(context.Background())
			return nil, fmt.Errorf("%w: pi prompt: %v", agent.ErrSpawnFailed, err)
		}
	}

	if spec.OnProcessSpawned != nil && cmd != nil && cmd.Process != nil {
		spec.OnProcessSpawned(cmd.Process.Pid)
	}
	return h, nil
}

// spawnChild execs `pi --mode rpc` with cmd.Dir = spec.Cwd, an
// allowlist-composed env, and its own process group.
func (p *Provider) spawnChild(spec agent.Spec, layout sessionLayout) (*exec.Cmd, io.WriteCloser, io.ReadCloser, error) {
	// nolint:gosec // G204: binary resolved from Options/env, args are constant.
	cmd := exec.Command(p.binary, rpcArgs()...)
	cmd.Dir = spec.Cwd
	cmd.Env = composeChildEnv(spec, layout)
	configureProcessGroup(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("pi stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("pi stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("pi stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, fmt.Errorf("pi spawn: %w", err)
	}
	go drainStderr(stderr)
	return cmd, stdin, stdout, nil
}

// Shutdown implements agent.Provider. pi is one-child-per-session, so the
// Provider owns no long-lived process — Shutdown is a no-op (each Handle owns
// and reaps its own child via Stop). Idempotent.
func (p *Provider) Shutdown(_ context.Context) error { return nil }

// resolvePiBinary applies PiBin → $PI_BIN → "pi" and resolves via LookPath.
func resolvePiBinary(bin string) (string, error) {
	if bin == "" {
		bin = os.Getenv("PI_BIN")
	}
	if bin == "" {
		bin = "pi"
	}
	full, err := exec.LookPath(bin)
	if err != nil {
		return "", fmt.Errorf("pi binary %q not on PATH (install: npm i -g @earendil-works/pi-coding-agent@%s): %w", bin, PinnedVersion, err)
	}
	return full, nil
}

func drainStderr(r io.ReadCloser) {
	defer func() { _ = r.Close() }()
	buf := make([]byte, 4096)
	for {
		if _, err := r.Read(buf); err != nil {
			return
		}
	}
}
