package pi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/confinement"
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
)

// newHandshakeToken returns a random hex secret the harness sets in the child
// env (piHandshakeEnvVar) and the policy extension echoes on every trust-
// boundary round-trip, so the handle can prove a request comes from the exact
// child it spawned.
func newHandshakeToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "pi-token-fallback"
	}
	return hex.EncodeToString(b)
}

// DefaultToolCallTimeoutSeconds is the bound the policy extension applies
// to every bash tool call on the headless RPC lane (extensions/donmai-policy.ts
// resolveBashTimeoutSeconds): a call with no timeout, an invalid timeout, or
// a timeout above the bound runs with the bound instead. pi's bash tool kills
// the command at the timeout and reports it as a tool ERROR, so the agent
// sees the failure and continues — the bound ends the CALL, never the
// session. 300 stays below the runner's 12-minute no-progress window with
// margin for model round trips around the call, and the runner's idle
// watchdog additionally treats an in-flight call as progress. The value is
// emitted once per session as an agent.SystemSubtypeToolCallBounds event,
// directly behind the session's InitEvent (see launchNotices), so the session
// record states the bound it runs at.
const DefaultToolCallTimeoutSeconds = 300

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
	// realBinary is true ONLY when New() resolved binary from PATH/PiBin (the
	// non-skipProcess branch). resolveCatalogProbe (catalog_preflight.go)
	// gates the real `pi --list-models` exec on this rather than on
	// opts.skipProcess directly, because several fixtures in this package
	// construct a *Provider by literal (&Provider{binary: fakeScriptPath},
	// e.g. newFakeInteractivePiProvider in interactive_test.go) rather than
	// through New() — such a Provider has opts.skipProcess == false (the
	// zero value) but binary is a fake/test script, not a real pi. Gating on
	// opts.skipProcess alone would shell out to that fake script for
	// --list-models on any test whose model happens to carry a recognized
	// builtin-provider prefix; the zero value of realBinary keeps every
	// bare-struct-literal Provider probe-free by construction, exactly like
	// skipProcess:true.
	realBinary bool
	// artifact is non-nil only when binary is the exact closed runtime tree
	// compiled into artifact_profile.go. It is never selected from a Spec,
	// environment profile claim, or semantic version.
	artifact *artifactLease
	// trustedExtensions is the immutable ordered same-process extension set
	// supplied by the compiled embedder at provider construction.
	trustedExtensions []TrustedExtensionIdentity
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

	// CatalogProbe overrides the launch-time catalog preflight's
	// `--list-models` query (requirement 2; catalog_preflight.go). Tests
	// inject a scripted catalog here; nil means the real pi binary answers
	// (or, under skipProcess with no override, the preflight is skipped —
	// resolveCatalogProbe).
	CatalogProbe catalogProbeFunc

	// CatalogProbeTimeout caps the launch-time catalog preflight. Defaults
	// to DefaultCatalogProbeTimeout.
	CatalogProbeTimeout time.Duration

	// TrustedExtensions is the complete ordered list of reviewed additional Pi
	// extensions this compiled embedder trusts to share receipt authority. The
	// public default is empty. A session Spec can match this list but cannot
	// extend it.
	TrustedExtensions []TrustedExtensionIdentity

	// RequireConfinement is the host's own requirement that every pi session
	// run inside the executor OS confinement (ADR-2026-10-03 D5.1, the
	// placement-owned trigger). It only tightens: when set, a session the
	// host cannot confine is refused, never run unconfined. New also sets it
	// when the host environment carries DONMAI_PI_CONFINEMENT=required.
	RequireConfinement bool

	// ConfinementReadScope is the host's fileRead level for confined
	// sessions. agent.FileReadWorkarea confines reads to the session's
	// workarea, the runtime and toolchain paths, pi's own install, git's
	// user configuration and ConfinementReadPaths, and implies
	// RequireConfinement; empty or agent.FileReadHost leaves reads open.
	// New refuses any other level, and tightens it to workarea when the
	// host environment carries DONMAI_PI_CONFINEMENT_READ=workarea.
	ConfinementReadScope agent.ExecutionSecurityLevel

	// ConfinementReadPaths are further absolute paths confined sessions may
	// read under a read scope. New appends the entries of
	// DONMAI_PI_CONFINEMENT_READ_PATHS.
	ConfinementReadPaths []string

	// Test seams. skipProcess wires stdin/stdout overrides instead of execing
	// a real child; used by the pipe-stub tests that replay pi RPC shapes.
	skipProcess    bool
	stdinOverride  io.Writer
	stdoutOverride io.Reader
	// beforeInitialPrompt is a private test barrier after the real handshake,
	// before the first prompt command. Observing events after Spawn cannot
	// establish this ordering: the child may already have started its turn.
	// Nil leaves the production launch sequence unchanged.
	beforeInitialPrompt func(*Handle) error
	// beforeChildStart and afterChildStart are deterministic mutation barriers
	// around the two artifact-lease checks. Nil leaves production unchanged.
	beforeChildStart func()
	afterChildStart  func()
	// handshakeToken pins the per-session token in skipProcess tests so a
	// scripted handshake fixture can echo it. Empty ⇒ a random token per Spawn.
	handshakeToken string
	// confinementDirs points the confiner's host directories (profiles,
	// operator home, host state home, self-test scratch) at throwaway paths
	// in tests. Nil resolves them from the host (productionConfinementDirs).
	// It never decides WHETHER a session is confined — the gate does.
	confinementDirs *piConfinementDirs
}

// piConfinementEnvVar is the host-level switch that makes every pi session
// on this host request confinement (ADR-2026-10-03 D5.1, placement-owned
// configuration). The only recognized value is "required"; anything else
// leaves Options.RequireConfinement as the caller set it. It can only
// tighten. It is host-owned (runtimeenv.IsHostOwned): the daemon drops a
// work item's copy, so the worker sees only the host's value.
const piConfinementEnvVar = runtimeenv.PiConfinementEnv

// piConfinementReadEnvVar and piConfinementReadPathsEnvVar are the
// host-level read scope and its declared read paths (host-owned, like the
// requirement): the read scope can only tighten.
const (
	piConfinementReadEnvVar      = runtimeenv.PiConfinementReadEnv
	piConfinementReadPathsEnvVar = runtimeenv.PiConfinementReadPathsEnv
)

// New probes the pi binary and enforces the version pin (probe-time, per
// design §2 / opencode §8). A confirmed-below-MinVersion binary fails
// construction with agent.ErrProviderUnavailable; an unverifiable/above
// version proceeds but marks the Provider so every session is labeled.
func New(opts Options) (*Provider, error) {
	if err := validateTrustedExtensionIdentities(opts.TrustedExtensions); err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrProviderUnavailable, err)
	}
	if opts.HandshakeTimeout == 0 {
		opts.HandshakeTimeout = 10 * time.Second
	}
	if opts.VersionProbeTimeout == 0 {
		opts.VersionProbeTimeout = DefaultVersionProbeTimeout
	}
	if opts.VersionProbe == nil {
		opts.VersionProbe = defaultVersionProbe
	}
	if strings.TrimSpace(os.Getenv(piConfinementEnvVar)) == "required" {
		opts.RequireConfinement = true
	}
	if err := applyHostReadScope(&opts); err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrProviderUnavailable, err)
	}
	p := &Provider{opts: opts, trustedExtensions: append([]TrustedExtensionIdentity(nil), opts.TrustedExtensions...)}

	if opts.skipProcess {
		p.binary = "pi"
		return p, nil
	}

	full, err := resolvePiBinary(opts.PiBin)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrProviderUnavailable, err)
	}
	full, err = filepath.Abs(full)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve pi binary absolute path: %v", agent.ErrProviderUnavailable, err)
	}
	full, err = filepath.EvalSymlinks(full)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve pi binary symlinks: %v", agent.ErrProviderUnavailable, err)
	}
	p.binary = full
	p.realBinary = true
	p.artifact, err = measureArtifactProfile(full)
	if err != nil {
		return nil, fmt.Errorf("%w: measure pi artifact profile: %v", agent.ErrProviderUnavailable, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), opts.VersionProbeTimeout)
	defer cancel()
	unverified, perr := checkVersionPin(ctx, opts.VersionProbe, full)
	if perr != nil {
		if p.artifact != nil {
			_ = p.artifact.close()
		}
		return nil, perr // already wraps ErrProviderUnavailable
	}
	p.unverified = unverified
	if p.artifact != nil {
		// Exact compiled bytes are stronger authority than the generic semantic
		// version window. A different binary with the same version remains on
		// the legacy, possibly-unverified path above.
		p.unverified = false
	}
	return p, nil
}

// Name implements agent.Provider.
func (p *Provider) Name() agent.ProviderName { return agent.ProviderPi }

// Spawn starts a new pi session. The Handle's Events channel emits exactly one
// InitEvent (from the get_state response), then session events, then exactly
// one terminal event, then closes. Fail-closed: the prompt is NEVER sent unless
// the policy extension materialized AND its handshake verified (design §2
// step 3).
func (p *Provider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	// Admit + endpoint-project the spec BEFORE the interactive/headless split so
	// a gateway-backed binding (model override, DONMAI_PI_KEY mirror, provider-
	// pin env) reaches BOTH spawn modes from birth. This bakes in the lesson the
	// claude interactive-endpoint fix had to RETROFIT (sibling of #323, whose
	// interactive spawn forked off before applyEndpoint ran) rather than
	// repeating it: pi's interactive path consumes the binding from the start.
	spec, err := p.prepare(ctx, spec)
	if err != nil {
		return nil, err
	}
	// Interactive PTY spawn mode: capability-gated on the LIVE manifest (mirrors
	// claude.go / codex.go), so an edit that flips SupportsInteractivePTY back to
	// false silently falls through to the headless RPC lane instead of crashing.
	if spec.Interactive != nil && p.Manifest().Caps.SupportsInteractivePTY {
		return p.spawnInteractive(ctx, spec)
	}
	return p.launch(ctx, spec, launchPrompt, "")
}

// prepare admits the spec against the pi manifest and projects the resolved
// endpoint binding onto it. It runs in Spawn/Resume, ahead of any spawn-mode
// split, so launch and spawnInteractive both receive one admitted,
// endpoint-projected spec — the interactive lane never sees a raw, unprojected
// binding.
func (p *Provider) prepare(ctx context.Context, spec agent.Spec) (agent.Spec, error) {
	var err error
	spec, err = agent.PrepareHarness(spec, p.Manifest())
	if err != nil {
		return spec, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}
	spec, err = applyEndpoint(spec)
	if err != nil {
		return spec, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
	}
	// Requirement 2: fail fast, before any child spawns, when the resolved
	// pin routes NATIVELY through one of pi's built-in providers (see
	// nativeProviderPin) but that exact (provider, model) pair is not in
	// pi's own catalog — instead of finding out on the first turn's 400.
	//
	// An aggregator endpoint is first given the chance to route through pi's
	// own aggregator provider (promoteAggregatorPin); that promotion already
	// ran the same catalog query, so the preflight below is skipped for it.
	spec, promoted := p.promoteAggregatorPin(ctx, spec)
	if promoted {
		return spec, nil
	}
	if provider, bareModel, useNative := nativeProviderPin(spec.Model, spec.Endpoint); useNative {
		if probe := p.resolveCatalogProbe(); probe != nil {
			credEnvVar := builtinProviderCredentialEnv[provider]
			if err := p.preflightCatalogCheck(ctx, probe, provider, bareModel, credEnvVar, spec.Env[credEnvVar]); err != nil {
				var miss *catalogMissError
				if !errors.As(err, &miss) {
					return spec, err
				}
				fallback, ferr := fallbackToInjectedProvider(spec, miss)
				if ferr != nil {
					return spec, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, ferr)
				}
				slog.Warn("pi catalog preflight miss; spawning through the injected provider",
					"provider", miss.provider, "model", miss.model,
					"fallbackModel", fallback.Model, "baseURL", fallback.Endpoint.BaseURL, "api", piAPIForProtocol(fallback.Endpoint.Protocol))
				spec = fallback
			}
		}
	}
	// A model the injected provider would serve over a protocol that cannot
	// omit the output limit is refused here, before either spawn mode starts
	// a child, when no limit is configured — a clear configuration error
	// instead of an upstream rejection on the first request.
	if err := requireOutputLimit(spec); err != nil {
		return spec, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}
	return spec, nil
}

// Resume re-execs `pi --mode rpc` against the persisted session and replays
// entries from the caller's cursor (design §4). Capability-gated on
// SupportsSessionResume.
//
// NOTE (untested): the get_entries cursor replay path is implemented against
// the real command shape ({type:"get_entries", since:<entryId>} — no session
// param; the session is selected via the --session CLI flag) but not verified
// against a real model turn; the donmai-smokes step20 resume item is its
// acceptance gate.
//
// Production status: runner/steering.go's attemptSteering now has a real
// stop-and-resume fallback — when a live Handle.Inject call returns
// agent.ErrUnsupported and the harness declares SupportsSessionResume, the
// runner stops the turn and calls Provider.Resume with the queued steer
// content riding the resumed Spec.Prompt. That closes the gap for codex and
// opencode.go's create-with-session lane, both of whose Inject
// implementations return agent.ErrUnsupported outright.
//
// pi does NOT take that branch today: pi's own Handle.Inject (handle.go)
// classifies a post-terminal call as a closed session ("pi: session
// closed"), not agent.ErrUnsupported — the state attemptSteering actually
// observes at its post-terminal call site (F.1.1 §4 step 11 fires after
// step 10's terminal wait, so the pump has already settled and h.closed has
// fired). That error hits attemptSteering's default branch (hard fail →
// backstop), same as before this fallback existed. So this package's Resume
// still gets its only non-test call site from agent/conformance/checks.go
// unless a future change teaches pi's post-terminal Inject to return
// agent.ErrUnsupported instead of a bespoke closed-session error.
// get_entries is now routed as an observable SystemEvent (event_mapping.go)
// rather than silently dropped, but is not decoded into replayed history —
// see this package's real_binary_test.go TestRealBinary_Resume_StructuralReplay.
func (p *Provider) Resume(ctx context.Context, sessionID string, spec agent.Spec) (agent.Handle, error) {
	if sessionID == "" {
		return nil, agent.ErrSessionNotFound
	}
	spec, err := p.prepare(ctx, spec)
	if err != nil {
		return nil, err
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
	// spec is already admitted + endpoint-projected by prepare() (called in
	// Spawn/Resume before the spawn-mode split).
	//
	// Decide confinement FIRST, before the parent writes anything into the
	// session's state: a confined session's state was writable by the seat
	// for its whole previous run, so every parent-side write below goes
	// through the strict writer, which refuses a planted symbolic link
	// instead of following it out of the set. Process-less
	// (protocol-scripted) launches spawn nothing and are never confined.
	var confiner *confinement.Confiner
	if !p.opts.skipProcess {
		var err error
		confiner, err = p.confinerForSession(ctx, spec)
		if err != nil {
			return nil, err
		}
	}
	// Materialize the policy extension BEFORE spawning. A materialization
	// failure means no boundary — fail closed. The state lands in the
	// session state root inside the working folder (confinement.go).
	layout, err := materializeExtensionForSpec(spec, confiner != nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
	}
	// Materialize + digest-verify spec.AdditionalExtensions (ADR-2026-08-12
	// D1). This runs on EVERY call to launch — both Spawn and Resume funnel
	// through here — so a resumed session re-verifies every injected artifact
	// rather than trusting a prior verification (D2(c)): the directory has
	// been agent-writable for the whole intervening period. A required
	// delivery that fails to materialize or verify denies spawn closed,
	// before any credential reaches the child (D1.2 — no warn-and-strip).
	extraExtensionPaths, err := materializeAdditionalExtensions(layout, spec.AdditionalExtensions)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
	}
	// The boundary extension always loads first and is never displaced,
	// reordered, or disabled by a delivery (D1).
	extensionPaths := append([]string{layout.extension}, extraExtensionPaths...)
	actualExtensions := make([]agentExtensionIdentity, len(spec.AdditionalExtensions))
	for i, delivery := range spec.AdditionalExtensions {
		actualExtensions[i] = agentExtensionIdentity{id: delivery.ID, digest: delivery.Digest, path: extraExtensionPaths[i]}
	}
	token := p.opts.handshakeToken
	if token == "" {
		token = newHandshakeToken()
	}
	// Deliver session credentials through files, never the child env: pi
	// renames itself into a short process name at startup, and a same-user
	// process listing then renders the child ENVIRONMENT as if it were its
	// command line. The exec environment is allowlisted (child_env.go); the
	// credential file carries the injected-provider key for the extension
	// plus the provider-native mirrors, and — in its environment section —
	// every other session binding the allowlist keeps out of the exec
	// environment; the native auth.json covers the --provider <name> route.
	// The environment is partitioned ONCE so the file and the exec
	// environment are two halves of the same composition. Any write failure
	// denies spawn closed, before any child starts. The files are removed at
	// session end (Stop) and, on spawn-failure paths below, here.
	childEnvParts := sessionChildEnv(spec)
	credentialPath, err := writeSessionCredentialFile(layout, sessionCredentialEntries(spec), childEnvParts.deferred)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
	}
	removeCredentials := func() {
		removeSessionCredentialFile(layout)
		removeNativeProviderAuthFile(layout)
	}
	if _, err := writeNativeProviderAuthFile(layout, spec); err != nil {
		removeCredentials()
		return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
	}
	// Prepare the confinement plan for a session that requested it. The
	// plan wraps the child argv below and binds the session tmp and caches
	// over their variables; nil means the session did not request
	// confinement and spawns exactly as before. Any failure refuses the
	// spawn — the session never runs half-confined. Every failure path
	// from here to the handle releases the rendered profile.
	var plan *confinement.Plan
	if confiner != nil {
		plan, err = p.confineSession(spec, layout, confiner)
		if err != nil {
			removeCredentials()
			return nil, err
		}
	}
	releasePlan := func() {
		if plan != nil {
			_ = plan.Release()
		}
	}
	// Compose once. Receipt admission inspects this exact final environment,
	// and spawnChild assigns the same immutable slice to exec.Cmd.Env. The
	// confinement bindings are appended last so they win. The credential
	// file's PATH rides the env (sessionCredentialEnv); no credential VALUE
	// does — the exec half of the partition holds allowlisted names only.
	childEnv := confinePiEnv(sessionCredentialEnv(headlessChildEnv(childEnvParts.exec, spec, layout, token), credentialPath), plan)
	var receipt *receiptAdmission
	if spec.Autonomous {
		startup := measureReceiptStartupContext(spec.Cwd, childEnv)
		receipt, err = newReceiptAdmission(p.artifact, layout, actualExtensions, p.trustedExtensions, startup)
		if err != nil {
			releasePlan()
			removeCredentials()
			return nil, fmt.Errorf("%w: measure pi receipt extension closure: %v", agent.ErrSpawnFailed, err)
		}
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
		c, in, out, serr := p.spawnChild(spec, layout, extensionPaths, childEnv, mode, sessionID, p.artifact, receipt, plan)
		if serr != nil {
			releasePlan()
			removeCredentials()
			return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, serr)
		}
		cmd, stdin, stdout = c, in, out
		if p.opts.afterChildStart != nil {
			p.opts.afterChildStart()
		}
		if p.artifact != nil {
			if err := revalidateLaunchTrust(p.artifact, receipt, childEnv); err != nil {
				stopStartedChild(cmd)
				releasePlan()
				removeCredentials()
				return nil, fmt.Errorf("%w: pi artifact changed after spawn: %v", agent.ErrSpawnFailed, err)
			}
		}
	}

	client := newRPCClient(stdin, stdout)
	h := newHandle(client, cmd, spec, token, receipt)
	h.setConfinement(plan)
	// The session credential files live in the session state root. Stop
	// removes them so a finished session leaves no secret on disk; the
	// worktree lifecycle that removes the state root is the backstop.
	h.onStop = func() {
		removeCredentials()
	}
	// Set before the pump starts (the go statement orders the write before
	// every read on the pump goroutine); dispatch emits them directly behind
	// the session's InitEvent. See launchNotices.
	h.launchNotices = p.launchNotices(spec)
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

	// Resolve the session id (agent_start carries none in the real protocol),
	// then bring up the turn. The model + reasoning effort are pinned on the
	// CLI at startup (rpcArgs: --provider donmai --model <id>[:<thinking>]), NOT
	// via a runtime set_model command — set_model and prompt race (verified
	// against the real binary: the prompt response can arrive before set_model
	// applies), so a runtime pin could let the first turn run on the default
	// model. The CLI pin is deterministic: get_state reports donmai/<id> before
	// any prompt is processed.
	_ = client.WriteCommand(map[string]any{"type": "get_state", "id": "donmai-get-state"})

	switch mode {
	case launchResume:
		// get_entries operates on the session already loaded (via --session);
		// `since` is the caller's last-seen ENTRY id cursor (no session param).
		if err := client.WriteCommand(map[string]any{"type": "get_entries", "since": sessionID}); err != nil {
			_ = h.Stop(context.Background())
			return nil, fmt.Errorf("%w: pi resume get_entries: %v", agent.ErrSpawnFailed, err)
		}
	default:
		if p.opts.beforeInitialPrompt != nil {
			if err := p.opts.beforeInitialPrompt(h); err != nil {
				_ = h.Stop(context.Background())
				return nil, fmt.Errorf("%w: pi initial prompt barrier: %v", agent.ErrSpawnFailed, err)
			}
		}
		if err := client.WriteCommand(map[string]any{"type": "prompt", "message": spec.Prompt}); err != nil {
			_ = h.Stop(context.Background())
			return nil, fmt.Errorf("%w: pi prompt: %v", agent.ErrSpawnFailed, err)
		}
	}

	if spec.OnProcessSpawned != nil && cmd != nil && cmd.Process != nil {
		spec.OnProcessSpawned(cmd.Process.Pid)
	}
	return h, nil
}

// launchNotices returns the session-scoped SystemEvents a headless RPC launch
// states on its event stream, in order. The handle emits them directly
// behind the session's single InitEvent — never ahead of it, which the event
// contract forbids (agent/conformance CheckSingleInit) — and so before any
// turn output. They are emitted only once the handshake has verified and
// get_state (or agent_start) has resolved the session; a launch that fails
// closed at the handshake gate emits none.
func (p *Provider) launchNotices(spec agent.Spec) []agent.Event {
	var notices []agent.Event
	// The boundary is live by the time these are seen. Label unverified
	// versions.
	if p.unverified {
		notices = append(notices, agent.SystemEvent{
			Subtype: unverifiedVersionSubtype,
			Message: fmt.Sprintf("pi binary version could not be confirmed within [%s, %s]; session proceeds labeled unverified", MinVersion, VerifiedAgainst),
		})
	}
	// Typed pre-spawn denial for Spec fields this provider cannot honor: named
	// on the event stream, before any turn output, rather than the silent drop
	// agent.Spec's own doc comment concedes ("unsupported fields are silently
	// ignored"). Today's only entry is CodeIntelEnforcement; see
	// codeIntelEnforcementNote.
	if note := codeIntelEnforcementNote(spec); note != nil {
		notices = append(notices, agent.SystemEvent{
			Subtype: codeIntelEnforcementUnsupportedSubtype,
			Message: note.Reason,
		})
	}
	// Bound a single tool call so it cannot end the session: the policy
	// extension clamps every bash call to DefaultToolCallTimeoutSeconds (or
	// below) and the runner's idle watchdog treats an in-flight call as
	// progress. Stated once per session so the record carries the bound the
	// session runs at. The interactive PTY lane carries no such event: its
	// handle wraps the shared ptycli driver with no spawn-time event seam, and
	// its local tool gate answers a narrower channel — see the residual note
	// on the RPC tool_call hook.
	notices = append(notices, agent.ToolCallBoundsEvent(DefaultToolCallTimeoutSeconds))
	return notices
}

// confinerForSession returns the shared, self-tested confiner for a session
// that requested confinement, and nil for one that did not. The probe is this
// process: the main entrypoint answers the confinement probe (see
// cmd/donmai/main.go and the TestMain hook in confinement_test_main_test.go),
// so the self-test drives the probe through the exact production spawn
// binding. A request that cannot be met — no working directory, no backend,
// a failed self-test — refuses the spawn.
func (p *Provider) confinerForSession(ctx context.Context, spec agent.Spec) (*confinement.Confiner, error) {
	if !piConfinementEnabled(spec, p.opts.RequireConfinement) {
		return nil, nil
	}
	if strings.TrimSpace(spec.Cwd) == "" {
		return nil, fmt.Errorf("%w: pi confinement requested for a session with no working directory", agent.ErrSpawnFailed)
	}
	probe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("%w: pi confinement probe executable: %v", agent.ErrSpawnFailed, err)
	}
	return ensurePiConfiner(ctx, p.binary, p.hostConfinementDirs(), []string{probe})
}

// hostConfinementDirs returns the confiner's host directories: the test
// seam's when set, else the host's own.
func (p *Provider) hostConfinementDirs() piConfinementDirs {
	if p.opts.confinementDirs != nil {
		return *p.opts.confinementDirs
	}
	return productionConfinementDirs()
}

// confineSession prepares a confined session's plan under the host's read
// scope.
func (p *Provider) confineSession(spec agent.Spec, layout sessionLayout, confiner *confinement.Confiner) (*confinement.Plan, error) {
	reads, err := p.sessionReadScope(p.hostConfinementDirs().home)
	if err != nil {
		return nil, err
	}
	return confinePiSession(spec, layout, confiner, reads)
}

// piConfinementEnabled reports whether the session requested OS confinement
// (ADR-2026-10-03 D5.1): the host's own configuration requires it for pi
// (hostRequires — Options.RequireConfinement), or the session declares a
// repository authority, whose read-only leaves need the executor boundary.
// Confinement applies exactly when requested, never opportunistically, so
// the answer does not depend on whether this host has a backend: a request
// on a host without one is refused, not dropped (ensurePiConfiner).
//
// The manifest declares the multi-repository workarea protocol and the
// read-only enforcement (manifest.go), so admission admits an
// authority-bearing spec and this authority branch confines it; the host
// requirement remains the trigger for sessions without one.
func piConfinementEnabled(spec agent.Spec, hostRequires bool) bool {
	if hostRequires {
		return true
	}
	return spec.RepositoryAuthority != nil && strings.TrimSpace(spec.RepositoryAuthority.WorkareaRoot) != ""
}

// newHeadlessChildCommand builds the headless harness child: argv runs
// with cmd.Dir and the composed environment in its own process group. It
// deliberately never sets ExtraFiles — the confined child inherits only
// its three standard-descriptor pipes (created by the caller via
// StdinPipe/StdoutPipe/StderrPipe), never a descriptor open on an
// out-of-set file, which the OS boundary would not judge.
func newHeadlessChildCommand(argv []string, dir string, env []string) *exec.Cmd {
	// nolint:gosec // G204: argv is the provider-built harness command.
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	configureProcessGroup(cmd)
	return cmd
}

// spawnChild execs `pi --mode rpc …` with cmd.Dir = spec.Cwd, an allowlist-
// composed env (incl. the per-session handshake token + provider-pin vars), and
// its own process group. extensionPaths is the boundary extension followed by
// every materialized+verified spec.AdditionalExtensions entry, in order
// (ADR-2026-08-12 D1). A non-nil plan wraps the argv in the session's
// confinement; the child then inherits exactly three descriptors — the
// stdin/stdout/stderr pipes created below — and never a descriptor open on
// an out-of-set file (ExtraFiles is never set: newHeadlessChildCommand).
// spawnChild never releases the plan: on any error the caller does.
func (p *Provider) spawnChild(spec agent.Spec, layout sessionLayout, extensionPaths []string, childEnv []string, mode launchMode, sessionID string, artifact *artifactLease, receipt *receiptAdmission, plan *confinement.Plan) (*exec.Cmd, io.WriteCloser, io.ReadCloser, error) {
	argv, err := confinePiArgv(plan, append([]string{p.binary}, rpcArgs(layout, extensionPaths, mode, sessionID, spec)...))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("pi confine command: %w", err)
	}
	// nolint:gosec // G204: binary resolved from Options/env; args are a fixed
	// set plus paths/ids/model this package controls.
	cmd := newHeadlessChildCommand(argv, spec.Cwd, childEnv)

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
	if artifact != nil {
		if p.opts.beforeChildStart != nil {
			p.opts.beforeChildStart()
		}
		if err := revalidateLaunchTrust(artifact, receipt, childEnv); err != nil {
			return nil, nil, nil, fmt.Errorf("pi artifact changed before spawn: %w", err)
		}
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, fmt.Errorf("pi spawn: %w", err)
	}
	go drainStderr(stderr)
	return cmd, stdin, stdout, nil
}

func revalidateLaunchTrust(artifact *artifactLease, receipt *receiptAdmission, childEnv []string) error {
	if receipt != nil {
		if !receipt.startup.matchesEnv(childEnv) {
			return fmt.Errorf("pi receipt child environment changed")
		}
		return receipt.revalidate()
	}
	return artifact.revalidate()
}

// Shutdown implements agent.Provider. pi is one-child-per-session, so the
// Provider owns no long-lived process — Shutdown is a no-op (each Handle owns
// and reaps its own child via Stop). Idempotent.
func (p *Provider) Shutdown(_ context.Context) error {
	if p.artifact != nil {
		return p.artifact.close()
	}
	return nil
}

func stopStartedChild(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	signalProcessGroup(cmd, syscall.SIGKILL)
	_, _ = cmd.Process.Wait()
}

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
