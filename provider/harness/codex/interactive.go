package codex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/activityrelay"
	"github.com/RenseiAI/donmai/provider/harness/ptycli"
	"github.com/RenseiAI/donmai/runtime/mcp"
)

// SpawnInteractive opens the codex CLI's own interactive TUI under a PTY via
// ptycli, seeded with spec.Prompt when set. An unnamed session uses bare
// `codex`. A named session uses a bounded per-session app-server:
//
//   - fresh (spec.Interactive.ResumeExisting false — the default; every
//     current producer, custom name or platform-canonical id-shaped name
//     alike): the PTY attaches to the bootstrap server with bare
//     `--remote <socket>` (no resume subcommand) and creates its own
//     thread there; the bootstrap connection observes that thread's
//     thread/started notification and names it post-hoc via
//     thread/name/set, reading back the result before the Handle is
//     returned.
//   - attach-to-existing (spec.Interactive.ResumeExisting true, only ever
//     set by an explicit platform signal): the bootstrap connection first
//     resumes the target by name via thread/resume — proving it exists —
//     then the PTY attaches with `codex resume --remote <socket> <name>`.
//
// The pinned CLI supports the --remote local transport only through Unix
// sockets; named Windows requests fail before any spawn side effect while
// unnamed Windows requests retain the bare-TUI path. See
// interactive_name.go's startNamedInteractiveAppServer for why these are
// two genuinely different native RPC sequences, not one gated by name
// presence alone (that conflation was the root cause of a production
// defect: a thread created before the PTY starts, but never given a turn,
// cannot be reattached by any resume/--remote invocation this CLI
// supports).
//
// It remains independent of the Provider's shared headless app-server state:
// it never touches Provider.client/cmd and resolves the binary itself. The
// optional named-session server is owned and cleaned up with this one PTY
// session, so no headless thread or configuration is shared by accident.
//
// This is a distinct SPAWN MODE from the default headless loop, not a
// different Transport: Manifest().Caps.Transport stays
// agent.TransportSubprocessRPC (the app-server's transport); TransportPTY is
// not codex's declared Transport, only its interactive spawn mode. See
// manifest.go and agent/harness.go's HarnessCaps.SupportsInteractivePTY doc
// comment for why the two are orthogonal, and provider/harness/shell for the
// contrasting harness whose ONLY transport is PTY.
//
// Event semantics are the coarse ptycli contract (program decision D4 — the
// byte-accurate PTY stream is the product): an InitEvent once the PTY child
// is up (SessionID stays empty on this coarse PTY surface) and a single
// terminal ResultEvent when the CLI process exits.
func SpawnInteractive(ctx context.Context, opts Options, spec agent.Spec) (agent.Handle, error) {
	var err error
	spec, err = agent.PrepareHarness(spec, (&Provider{}).Manifest())
	if err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}
	return spawnInteractivePrepared(ctx, opts, spec)
}

// spawnInteractivePrepared receives the one Spec already admitted by
// PrepareHarness. Keeping all PTY/config/process work below this boundary
// prevents interactive mode from minting a second prompt or tool authority.
func spawnInteractivePrepared(ctx context.Context, opts Options, spec agent.Spec) (agent.Handle, error) {
	return spawnInteractivePreparedForGOOS(ctx, opts, spec, runtime.GOOS)
}

func spawnInteractivePreparedForGOOS(ctx context.Context, opts Options, spec agent.Spec, goos string) (agent.Handle, error) {
	if err := validateNamedInteractiveTransport(spec, goos); err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}
	if err := validateCodexCLIMCPServers(spec.MCPServers); err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, persistInteractiveMCPApplicationDenial(spec, err))
	}
	launch, err := buildInteractiveLaunch(spec)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, persistInteractiveMCPApplicationDenial(spec, err))
	}
	bin, err := resolveCodexBinary(opts.CodexBin)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
	}
	baseURL, gatewayKey, gatewayRouted, routeErr := gatewayBinding(spec)
	if routeErr != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, routeErr)
	}
	if !hasPlatformSessionMCPAuthority(spec.MCPServers) {
		if !gatewayRouted {
			if spec.SessionName != "" {
				home, homeErr := ambientCodexHome()
				if homeErr != nil {
					return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, homeErr)
				}
				server, nameErr := startNamedInteractiveAppServer(ctx, bin, opts, spec, launch, launch.env, home)
				if nameErr != nil {
					return nil, fmt.Errorf("%w: name codex interactive session: %w", agent.ErrSpawnFailed, nameErr)
				}
				spec.Env = launch.env
				return spawnNamedInteractivePTY(ctx, bin, opts, spec, launch, server, server.close)
			}
			spec.Env = launch.env
			handle, err := ptycli.SpawnWithOptions(ctx, bin, launch.argv, spec, (&Provider{}).Manifest(), ptycli.SpawnOptions{
				TerminalCost: interactiveTerminalCost(launch.env["CODEX_HOME"], nil),
			})
			if err != nil {
				return nil, err
			}
			return wrapInteractiveConversationHandle(handle, launch.env["CODEX_HOME"], nil, newInteractiveConversationContextSource(launch.env["CODEX_HOME"], nil)), nil
		}
	}
	// A gateway-routed cell falls through to the isolated boundary below so
	// the provider block lands in a private config and the child cannot see
	// ambient state.
	var config *codexConfigBoundary
	auth := interactiveCodexAuthProjection{}
	if gatewayRouted {
		// The provider block plus the binding-selected key are the cell's
		// only route, so the boundary is a plain private home: no host
		// login is resolved, linked, or required here.
		var berr error
		config, berr = newCodexConfigBoundary(opts.configTempDir, true)
		if berr != nil {
			return nil, fmt.Errorf("%w: isolate codex interactive config: %w", agent.ErrSpawnFailed, berr)
		}
	} else if config, auth, err = newInteractiveCodexConfigBoundary(opts.configTempDir, launch.env); err != nil {
		return nil, fmt.Errorf("%w: isolate codex interactive config: %w", agent.ErrSpawnFailed, err)
	}
	// Seed this interactive session's isolated home from the same
	// host-level warm cache the headless boundary uses (see New in codex.go
	// and plugin_cache.go) — the named bootstrap app-server started below
	// and the PTY itself both run under this same CODEX_HOME, so this one
	// call covers the cold-fetch cost for both.
	config.enablePluginCacheReuse(resolveCodexPluginCacheDir(""))
	if err := pinProtectedMCPFileStore(config.configPath, spec.MCPServers); err != nil {
		return nil, errors.Join(fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err), config.remove())
	}
	if launch.env == nil {
		launch.env = make(map[string]string)
	}
	// CODEX_HOME is runner-owned for this process. Overwrite both an ambient
	// value and a value supplied through Spec.Env: either one may contain a
	// caller-global MCP server carrying an external requester registration.
	// The selected session's MCP server and bearer remain in the process-local
	// --config override built above, while the private config.toml starts from
	// an explicit empty mcp_servers table.
	launch.env["CODEX_HOME"] = config.home
	spec.Env = launch.env
	// A gateway-routed cell skips the environment-auth seeding and the
	// host-session link: the provider block plus the binding-selected key
	// are its only route, so the child must never gain an operator login
	// fallback. The route was resolved once above, before operator-auth
	// resolution; reuse it here rather than re-deriving it after the env
	// was rewritten.
	if gatewayRouted {
		nextEnv, err := config.appendGatewayProviderBlock(baseURL, gatewayKey, launch.env)
		if err != nil {
			return nil, errors.Join(
				fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err),
				config.remove(),
			)
		}
		if nextEnv == nil {
			nextEnv = map[string]string{}
		}
		nextEnv[codexGatewayEnvKey] = gatewayKey
		launch.env = nextEnv
		spec.Env = launch.env
	} else {
		authSeeder := opts.interactiveAuthSeeder
		if authSeeder == nil {
			authSeeder = seedInteractiveCodexEnvironmentAuth
		}
		if err := authSeeder(ctx, bin, config.home, auth); err != nil {
			return nil, errors.Join(
				fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err),
				config.remove(),
			)
		}
	}
	if err := refuseProtectedMCPStoredOAuth(config.home, spec.MCPServers); err != nil {
		return nil, errors.Join(fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err), config.remove())
	}
	// The verified private store is now the sole child authority. Empty values
	// deliberately override inherited parent credentials in both the effective
	// config preflight and the PTY process environment.
	launch.env = clearInteractiveCodexAuthEnvironment(launch.env)
	spec.Env = launch.env
	if err := verifyExclusiveInteractiveMCP(
		ctx,
		opts.interactiveMCPInventoryRunner,
		bin,
		spec,
		launch,
		config.home,
	); err != nil {
		return nil, errors.Join(
			fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err),
			config.remove(),
		)
	}
	// Activity-hook telemetry installs after the MCP inventory readback
	// so the fixture seam above observes the same private config as
	// before. A relay or append failure fails the spawn closed (gateway
	// parity): the launch omits the hooks kill-switch, so continuing with
	// a half-written config would leave the hooks feature on over a
	// boundary validate() rejected — exactly the review-modal hang trust.go
	// exists to prevent.
	var relay *activityrelay.Relay
	// closeRelay removes the relay directory exactly once. It runs on
	// error paths and in the PTY cleanup, which ptycli may invoke from
	// its own goroutine while this function still reads relay below —
	// so it must not mutate relay itself. Relay.Close is idempotent
	// (RemoveAll on a missing path succeeds), and a wrapped handle over
	// an already-closed relay simply sweeps nothing.
	var relayClose sync.Once
	closeRelay := func() {
		relayClose.Do(func() {
			if relay != nil {
				_ = relay.Close()
			}
		})
	}
	if installActivityHooks(spec) {
		newRelayFn := opts.interactiveActivityRelayNew
		if newRelayFn == nil {
			newRelayFn = activityrelay.New
		}
		newRelay, relayErr := newRelayFn()
		if relayErr != nil {
			return nil, errors.Join(
				fmt.Errorf("%w: install activity relay: %w", agent.ErrSpawnFailed, relayErr),
				config.remove(),
			)
		}
		if appendErr := config.appendActivityHooks(newRelay); appendErr != nil {
			_ = newRelay.Close()
			return nil, errors.Join(
				fmt.Errorf("%w: install activity hooks: %w", agent.ErrSpawnFailed, appendErr),
				config.remove(),
			)
		}
		relay = newRelay
	}
	if opts.interactiveBeforePTYSpawn != nil {
		if err := opts.interactiveBeforePTYSpawn(ctx, bin, spec, launch, config.home); err != nil {
			closeRelay()
			return nil, errors.Join(
				fmt.Errorf("%w: interactive pre-PTY fixture: %w", agent.ErrSpawnFailed, err),
				config.remove(),
			)
		}
	}
	if spec.SessionName != "" {
		server, err := startNamedInteractiveAppServer(ctx, bin, opts, spec, launch, launch.env, config.home)
		if err != nil {
			closeRelay()
			return nil, errors.Join(
				fmt.Errorf("%w: name codex interactive session: %w", agent.ErrSpawnFailed, err),
				config.remove(),
			)
		}
		if attachToExistingNamedSession(spec) {
			recordResumeKey(config.home, spec.SessionName)
		}
		cleanup := func() error {
			closeRelay()
			return errors.Join(server.close(), config.remove())
		}
		handle, err := spawnNamedInteractivePTY(ctx, bin, opts, spec, launch, server, cleanup)
		if err != nil {
			closeRelay()
			return nil, err
		}
		if relay != nil {
			return wrapActivityHandle(handle, relay), nil
		}
		return handle, nil
	}
	handle, err := ptycli.SpawnWithOptions(
		ctx,
		bin,
		launch.argv,
		spec,
		(&Provider{}).Manifest(),
		ptycli.SpawnOptions{
			Cleanup: func() error {
				closeRelay()
				return config.remove()
			},
			TerminalCost: interactiveTerminalCost(config.home, nil),
		},
	)
	if err != nil {
		closeRelay()
		return nil, err
	}
	conversation := wrapInteractiveConversationHandle(handle, config.home, nil, newInteractiveConversationContextSource(config.home, nil))
	if relay != nil {
		return wrapActivityHandle(conversation, relay), nil
	}
	return conversation, nil
}

// interactiveConversationContextSource is the session-scoped context-usage
// source for a conversation handle: the codex leg is the rollout file,
// never a sidecar. home is the session's own home; thread resolves the
// thread the session actually ran as (nil on paths where the thread is
// never known — the rollout side stays quiet). The watcher re-resolves
// every tick, so an empty thread simply polls nothing yet.
type interactiveConversationContextSource struct {
	home    string
	thread  *atomic.Value
	resolve func() string
}

// newInteractiveConversationContextSource builds the fixed-thread source
// for the unnamed paths: the thread is unknown at spawn (the TUI creates
// it after the PTY is up), so the source reports the session home with an
// empty thread until the first rollout file appears.
func newInteractiveConversationContextSource(home string, resolve func() string) *interactiveConversationContextSource {
	return &interactiveConversationContextSource{home: home, resolve: resolve}
}

// ContextUsageSidecarPath reports no sidecar: the codex leg is the
// rollout file, never a sidecar.
func (s *interactiveConversationContextSource) ContextUsageSidecarPath() string {
	return ""
}

// ContextUsageRollout returns the session's own home and the thread the
// session runs as (or "" while naming has not completed).
func (s *interactiveConversationContextSource) ContextUsageRollout() (string, string) {
	if s == nil {
		return "", ""
	}
	if s.thread != nil {
		if id, _ := s.thread.Load().(string); id != "" {
			return s.home, id
		}
		return s.home, ""
	}
	if s.resolve != nil {
		if id := s.resolve(); id != "" {
			return s.home, id
		}
	}
	if s.home == "" {
		return "", ""
	}
	return s.home, ""
}

// interactiveTerminalCost returns the TerminalCost closure for one
// interactive PTY spawn: at child exit it reads the thread's session file
// under home and attaches the last cumulative token total as the terminal
// cost. home is the exact CODEX_HOME the child ran under; resolveThreadID
// reports the thread the session actually ran as, or "" when it is not
// known (an attach path records the resume key up front, the fresh path
// only after naming). Either unknown keeps the bare result — absent records
// are "unknown", never "free" — and a re-adopted session after a daemon
// restart reads the same cumulative file, so it reports the same total.
func interactiveTerminalCost(home string, resolveThreadID func() string) func() *agent.CostData {
	return func() *agent.CostData {
		if home == "" || resolveThreadID == nil {
			return nil
		}
		return rolloutUsageCost(home, resolveThreadID())
	}
}

// Event channel sizing for interactiveConversationHandle. The coarse
// events — InitEvent and the terminal ResultEvent — always have a reserved
// slot, so their blocking sends complete even when nobody drains Events.
// Conversation activity only ever fills the remaining slots, and is
// dropped (never queued) once they are taken.
const (
	interactiveConversationCoarseSlots = 2
	interactiveConversationEventSlots  = 64
)

// wrapInteractiveConversationHandle adds the rollout conversation tail to
// a spawned PTY handle. resolveThreadID reports the thread the session
// actually ran as (nil on paths where the thread is never known — the
// tailer then stays quiet). The tailer reads only the admitted session's
// own rollout files — the thread's file plus its descendant subagent
// files — located through the same thread identity the terminal-cost read
// uses, and maps each new user/assistant message, tool call/result, and
// turn marker to the same agent events the headless lane emits, so the
// live conversation reaches the runner's activity sink in order.
//
// Ordering: rollout records the child wrote on its way out can only be
// read after InteractiveSession().Done closes, so the final flush is
// enqueued strictly after Done. ActivityFlushed closes once that flush is
// enqueued, and the terminal ResultEvent is forwarded only after it.
//
// Teardown: every helper goroutine returns at session end whether or not
// the caller drains Events — the tailer stops on Done and never blocks,
// and the coarse sends always fit the reserved slots — after which Events
// closes.
func wrapInteractiveConversationHandle(handle *ptycli.Handle, home string, resolveThreadID func() string, ctxSource *interactiveConversationContextSource) *interactiveConversationHandle {
	w := &interactiveConversationHandle{
		Handle:          handle,
		events:          make(chan agent.Event, interactiveConversationCoarseSlots+interactiveConversationEventSlots),
		activityFlushed: make(chan struct{}),
		finished:        make(chan struct{}),
		contextSource:   ctxSource,
	}
	session := handle.InteractiveSession()
	// emit forwards one conversation event best-effort. Only the tailer
	// goroutine calls it, and it never takes a slot reserved for the
	// coarse events: with at most interactiveConversationCoarseSlots
	// coarse sends over the handle's life, the coarse senders below can
	// never block on a channel conversation events filled. When the
	// consumer is slow the event is dropped rather than blocking the
	// tailer behind the terminal.
	emit := func(ev agent.Event) {
		if len(w.events) >= cap(w.events)-interactiveConversationCoarseSlots {
			return
		}
		select {
		case w.events <- ev:
		default:
		}
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for event := range handle.Events() {
			if _, terminal := event.(agent.ResultEvent); terminal {
				// ptycli sends the result strictly after Done; the
				// tailer's final flush, which also starts at Done,
				// goes first.
				<-w.activityFlushed
			}
			w.events <- event
		}
	}()
	// Rollout conversation tail: while the session runs, its rollout
	// files are tailed and each new user/assistant message, tool
	// call/result, and turn marker is forwarded as the same agent event
	// the headless lane emits for it. Best-effort and rate-limited (see
	// interactive_conversation.go). It stops when the PTY child has
	// exited and drained, after one final flush of trailing writes. The
	// thread id resolves late on the fresh-named path (only after the
	// PTY's own thread is named), so the tailer polls the resolver and
	// starts once the identity is known.
	go func() {
		defer wg.Done()
		defer close(w.activityFlushed)
		tailer := newInteractiveConversationTailer(home, resolveThreadID)
		tailer.run(session.Done(), emit)
	}()
	go func() {
		wg.Wait()
		close(w.events)
		close(w.finished)
	}()
	return w
}

// interactiveConversationHandle adds the rollout conversation tail to the
// shared PTY handle.
//
// Embedding *ptycli.Handle rather than reimplementing it keeps
// agent.Handle and agent.InteractiveCapable behaviour byte-identical to
// every other interactive harness; the only thing this type adds is the
// session's own live conversation on Events. The terminal ResultEvent is
// the PTY lifecycle's own (carrying the harness-accounted cost the
// TerminalCost hook attaches); turn completion rides a SystemEvent turn
// marker, never a second terminal.
type interactiveConversationHandle struct {
	*ptycli.Handle
	events chan agent.Event
	// contextSource carries the session's context-usage signal for the
	// runner's session-scoped watcher (nil on paths where the thread is
	// never known — the watcher then stays quiet). It is separate from
	// the conversation tailer's resolver so the watcher re-resolves
	// every tick while the tailer polls for its own start.
	contextSource *interactiveConversationContextSource
	// activityFlushed closes once the conversation tailer has enqueued
	// its final flush (or given up on a full channel) and returned.
	activityFlushed chan struct{}
	// finished closes after events is closed, i.e. once every helper
	// goroutine has returned.
	finished chan struct{}
}

var (
	_ agent.InteractiveCapable            = (*interactiveConversationHandle)(nil)
	_ agent.InteractiveActivityFlusher    = (*interactiveConversationHandle)(nil)
	_ agent.InteractiveTerminalCostSource = (*interactiveConversationHandle)(nil)
	_ agent.ContextUsageSourcer           = (*interactiveConversationHandle)(nil)
)

// Events overrides ptycli.Handle.Events with the coarse Init/Result
// contract plus the live conversation tailed from the session's own
// rollout files. The terminal event is forwarded only after the flush, so
// a consumer reading to the ResultEvent (or waiting on ActivityFlushed
// after Done) sees every delivered conversation event before the session
// totals.
func (h *interactiveConversationHandle) Events() <-chan agent.Event { return h.events }

// ActivityFlushed implements agent.InteractiveActivityFlusher: it closes
// once the session's final conversation activity has been enqueued on
// Events.
func (h *interactiveConversationHandle) ActivityFlushed() <-chan struct{} {
	return h.activityFlushed
}

// HasTerminalCost forwards the inner PTY handle's terminal-cost
// declaration so the runner's post-exit wait keeps the long grace for a
// promised harness-accounted cost. Without it the wrapper would hide the
// declaration and the runner would stop waiting after the short grace.
func (h *interactiveConversationHandle) HasTerminalCost() bool {
	if h == nil || h.Handle == nil {
		return false
	}
	src, ok := agent.Handle(h.Handle).(agent.InteractiveTerminalCostSource)
	return ok && src.HasTerminalCost()
}

// ContextUsageSidecarPath reports no sidecar: the codex leg is the
// rollout file, never a sidecar.
func (h *interactiveConversationHandle) ContextUsageSidecarPath() string { return "" }

// ContextUsageRollout returns the session's own home and thread through
// the handle's session-scoped source (empty thread until the rollout
// file appears; empty home on paths where the thread is never known).
func (h *interactiveConversationHandle) ContextUsageRollout() (string, string) {
	if h == nil || h.contextSource == nil {
		return "", ""
	}
	return h.contextSource.ContextUsageRollout()
}

// spawnNamedInteractivePTY points launch's argv at server (remoteInteractiveArgs
// already knows, from whether buildInteractiveLaunchEnv prepended "resume",
// which of the two named shapes this is), spawns the PTY, and — for the
// fresh (non-attach) path only — waits for and names the thread the PTY
// itself creates. cleanup is whatever the caller owns for server (plus any
// isolated-config-boundary removal); it always runs on PTY teardown, exactly
// as before this refactor.
//
// For the attach-to-existing path, startNamedInteractiveAppServer has
// already proven the target exists (via thread/resume) before this function
// is ever reached, so no further post-spawn step is needed here: the argv
// already carries `resume --remote <socket> <name>`, the #480 construction,
// unchanged.
func spawnNamedInteractivePTY(
	ctx context.Context,
	bin string,
	opts Options,
	spec agent.Spec,
	launch interactiveLaunch,
	server *namedInteractiveAppServer,
	cleanup func() error,
) (*interactiveConversationHandle, error) {
	launch.argv = remoteInteractiveArgs(launch.argv, server.remoteURL)
	home := spec.Env["CODEX_HOME"]
	var currentThreadID atomic.Value
	currentThreadID.Store("")
	resolveThreadID := func() string { id, _ := currentThreadID.Load().(string); return id }
	handle, err := ptycli.SpawnWithOptions(ctx, bin, launch.argv, spec, (&Provider{}).Manifest(), ptycli.SpawnOptions{
		Cleanup:      cleanup,
		TerminalCost: interactiveTerminalCost(home, resolveThreadID),
	})
	if err != nil {
		return nil, err
	}
	ctxSource := &interactiveConversationContextSource{home: home, thread: &currentThreadID}
	if attachToExistingNamedSession(spec) {
		currentThreadID.Store(spec.SessionName)
		return wrapInteractiveConversationHandle(handle, home, resolveThreadID, ctxSource), nil
	}
	nameTimeout := opts.HandshakeTimeout
	if nameTimeout <= 0 {
		nameTimeout = 30 * time.Second
	}
	threadID, err := finishNamingLiveInteractiveThread(ctx, spec, server, nameTimeout)
	if err != nil {
		// Stop below can take up to the shim finalize grace window (see
		// ptycli.Handle.awaitShimTerminal); log the reason now so a slow
		// teardown is not silent about why it started. The bootstrap
		// app-server's own bounded, redacted stderr excerpt rides along here
		// (and into the returned error below) because this is exactly the
		// failure shape a dead-mid-startup app-server produces: naming times
		// out or errors while the process itself already carries the real
		// cause in what it printed before dying.
		excerpt := server.stderr.Excerpt()
		slog.Warn("codex: fresh interactive session naming failed; stopping the PTY",
			"session", spec.SessionName, "err", err, "stderrExcerpt", excerpt)
		stopErr := handle.Stop(ctx)
		wrapped := fmt.Errorf("%w: name codex interactive session: %w", agent.ErrSpawnFailed, err)
		if excerpt != "" {
			wrapped = fmt.Errorf("%w (app-server stderr: %s)", wrapped, excerpt)
		}
		return nil, errors.Join(wrapped, stopErr)
	}
	// finishNamingLiveInteractiveThread waits through Codex's rollout-flush
	// race, so the resume key is recorded only after the native state exists.
	recordResumeKey(spec.Env["CODEX_HOME"], threadID)
	currentThreadID.Store(threadID)
	return wrapInteractiveConversationHandle(handle, home, resolveThreadID, ctxSource), nil
}

// validateNamedInteractiveTransport keeps the optional naming layer honest on
// platforms where the pinned Codex CLI has no supported local transport for
// sharing one app-server between the name-set RPC and the interactive TUI.
// Unnamed sessions never enter that layer and retain the historical bare-TUI
// path unchanged.
func validateNamedInteractiveTransport(spec agent.Spec, goos string) error {
	if spec.SessionName == "" || goos != "windows" {
		return nil
	}
	return fmt.Errorf(
		"named Codex interactive sessions require Unix-socket app-server attach; Windows supports unnamed interactive sessions only: %w",
		agent.ErrUnsupported,
	)
}

// hasPlatformSessionMCPAuthority identifies the reserved gateway entry the
// runner prepends for a platform-launched session. Standalone interactive use
// retains its historical ambient Codex configuration; only the session shape
// carrying the runner-owned authority needs the exclusive boundary.
func hasPlatformSessionMCPAuthority(servers []agent.MCPServerConfig) bool {
	if len(servers) == 0 {
		return false
	}
	gateway := servers[0]
	return strings.EqualFold(strings.TrimSpace(gateway.Type), "http") &&
		strings.HasSuffix(strings.TrimSpace(gateway.Name), "-platform") &&
		strings.Contains(gateway.URL, "/api/mcp/")
}

// newInteractiveCodexConfigBoundary creates the same exclusive user-config
// boundary used by the headless app-server lane. If the host login is
// file-backed, its auth.json inode is projected into the private home; no
// config.toml, project table, MCP server, or header mapping is copied. Hosts
// authenticated through an environment variable are seeded through Codex's
// own login command into an ephemeral private auth.json. An OS-keyring
// credential cannot be safely projected to a different CODEX_HOME and is
// refused before the PTY starts.
func newInteractiveCodexConfigBoundary(tempDir string, specEnv map[string]string) (*codexConfigBoundary, interactiveCodexAuthProjection, error) {
	auth, err := resolveInteractiveCodexAuth(specEnv)
	if err != nil {
		return nil, interactiveCodexAuthProjection{}, err
	}

	boundaryParent := tempDir
	if auth.kind == interactiveCodexAuthFile && boundaryParent == "" {
		// A hard link cannot cross filesystems. Keeping the private directory
		// beside the host credential makes the projection portable to hosts
		// whose system temp directory is a separate mount.
		boundaryParent = filepath.Dir(auth.hostAuthFile)
	}
	boundary, err := newCodexConfigBoundaryWithAuthMode(boundaryParent, auth.storeMode)
	if err != nil {
		return nil, interactiveCodexAuthProjection{}, err
	}
	if auth.kind == interactiveCodexAuthFile {
		if err := boundary.linkHostSessionAuth(auth.hostAuthFile); err != nil {
			return nil, interactiveCodexAuthProjection{}, errors.Join(err, boundary.remove())
		}
	}
	return boundary, auth, nil
}

type interactiveLaunch struct {
	argv []string
	env  map[string]string
}

// interactiveArgs builds the argv for codex's own interactive TUI. The codex
// CLI accepts a positional prompt to seed the first message of an
// interactive session — `codex "fix the failing tests"` launches the TUI
// with that initial prompt already queued, distinct from
// `codex exec "..."` (headless, one-shot, prints the final message to
// stdout and exits) which this package never uses for the interactive
// spawn mode. An empty prompt starts the TUI bare, with no seeded message.
func interactiveArgs(spec agent.Spec) []string {
	launch, _ := buildInteractiveLaunch(spec)
	return launch.argv
}

// buildInteractiveLaunch projects requested MCP servers into one process-local
// Codex CLI override. For a platform-launched session,
// spawnInteractivePrepared points the child at a private CODEX_HOME whose
// config starts with `mcp_servers = {}`, so the effective MCP set is
// exclusively the servers in this override. The host's persistent user and
// project configuration never participates in that session. Standalone
// interactive use retains its historical ambient configuration.
//
// It also seeds the startup trust codex would otherwise raise a modal review
// for — see trust.go, which owns that decision and the rule for what may be
// pre-answered at all. Without it the TUI parks on "Do you trust the contents
// of this directory?" before reading the seeded prompt, and an unattended
// session never gets past it.
//
// It also projects Spec.Model, when the platform resolved one
// (QueuedWork.ResolvedProfile.Model → Spec.Model via translateSpec), onto the
// same `--config key=value` mechanism this launch already uses for every
// other session-scoped knob (approval_policy, sandbox_mode,
// developer_instructions, mcp_servers, projects.*, features.hooks) —
// `model` is codex's own top-level config.toml key, mirrored here as a
// process-local override rather than a write to the operator's persistent
// config, exactly like every other seed in this file. This closes the same
// class of defect trust.go and approvals_seed.go closed for their own knobs:
// before this mapping existed the interactive TUI silently ran under
// whatever model codex's own config.toml/CLI default resolved to, even when
// the platform had already resolved a specific one for headless dispatch of
// the identical Spec (spec_translation.go's threadStartParams/
// turnStartParams, which always set "model" via resolveModel).
//
// Unlike AllowedTools/PermissionConfig (which get a typed pre-spawn denial
// receipt when a harness structurally cannot deliver them — see
// persistInteractiveMCPApplicationDenial below), Model IS mechanically
// deliverable here: codex has no upfront way to validate a model id, so
// there is nothing to deny in advance. The honest contract is pass-through:
// an id codex rejects surfaces as codex's own nonzero exit, which
// ptycli.buildResult turns into a failed agent.ResultEvent (see
// runner.TestInteractive_ExitDetailIsNotASummary) — never a silent retry
// under a different model and never swallowed as a false "completed".
// Empty Spec.Model emits no override at all, leaving codex on its own
// default — this deliberately does NOT call resolveModel (spec_translation.go),
// which defaults to DefaultCodexModel / CODEX_MODEL(_TIER) env fallbacks for
// the headless JSON-RPC lane; those fallbacks are a separate legacy
// compatibility shim, not part of the platform-selection contract this
// mapping restores.
func buildInteractiveLaunch(spec agent.Spec) (interactiveLaunch, error) {
	return buildInteractiveLaunchEnv(spec, os.Getenv)
}

// buildInteractiveLaunchEnv is buildInteractiveLaunch with the environment
// lookup injected, so the hooks policy can be table-tested without mutating
// process state (t.Setenv forbids the t.Parallel this package uses throughout).
func buildInteractiveLaunchEnv(spec agent.Spec, getenv func(string) string) (interactiveLaunch, error) {
	env := cloneInteractiveEnv(spec.Env)
	var args []string
	hooks, err := codexHooksPolicy(getenv)
	if err != nil {
		return interactiveLaunch{}, err
	}
	if spec.RepositoryAuthority != nil && hooks != codexHooksOff {
		return interactiveLaunch{}, codexMCPApplicationError("repository authority enforcement requires workspace hooks disabled because hooks execute outside the sandbox")
	}
	approvals, err := codexApprovalsPolicy(getenv)
	if err != nil {
		return interactiveLaunch{}, err
	}
	trustLevel := codexTrustLevelTrusted
	if hasPlatformSessionMCPAuthority(spec.MCPServers) {
		trustLevel = codexTrustLevelUntrusted
		// An unattended platform session cannot answer Codex's self-update
		// modal. The fleet upgrades the binary out of band.
		args = append(args, "--config", "check_for_update_on_startup=false")
	}
	trustArgs, err := interactiveTrustArgsWithLevel(spec.Cwd, hooks, trustLevel, installActivityHooks(spec), os.Getwd)
	if err != nil {
		return interactiveLaunch{}, err
	}
	args = append(args, trustArgs...)
	args = append(args, interactiveApprovalArgs(approvals)...)
	if spec.RepositoryAuthority != nil {
		// Last-writer-wins over an operator's interactive approval seed: the
		// selected repository remains the sole writable root even when the
		// attended posture would otherwise choose danger-full-access.
		args = append(args, "--config", "sandbox_mode="+tomlBasicString("workspace-write"))
		for _, mutablePath := range spec.RepositoryAuthority.MutablePaths {
			if mutablePath != "" && mutablePath != spec.Cwd {
				args = append(args, "--add-dir", mutablePath)
			}
		}
	}
	if model := strings.TrimSpace(spec.Model); model != "" {
		args = append(args, "--config", "model="+tomlBasicString(model))
	}
	// A gateway-routed cell restates its provider block through CLI
	// overrides (see gatewayProviderConfigOverride): the private config
	// file carries the primary route, and these ride the CLI layer above
	// every file layer, so repo-checked-in provider config cannot redirect
	// the bearer. Non-routed sessions render nothing and keep their argv.
	if baseURL, _, routed, routeErr := gatewayBinding(spec); routeErr == nil && routed {
		args = append(args, gatewayProviderConfigOverride(baseURL)...)
	}
	// Reasoning effort rides the same process-local override as model:
	// model_reasoning_effort is codex's own config.toml key, and the headless
	// lane already sends the identical value as turn/start reasoningEffort
	// (spec_translation.go). Empty emits nothing — no level is chosen on the
	// session's behalf.
	if effort := strings.TrimSpace(string(spec.Effort)); effort != "" {
		args = append(args, "--config", "model_reasoning_effort="+tomlBasicString(effort))
	}
	if spec.SystemPromptAppend != "" {
		args = append(args, "--config", "developer_instructions="+tomlBasicString(spec.SystemPromptAppend))
	}
	if len(spec.MCPServers) > 0 {
		override, nextEnv, err := codexCLIMCPOverride(spec.MCPServers, env, mcpToolsApprovalMode(approvals))
		if err != nil {
			return interactiveLaunch{}, err
		}
		env = nextEnv
		args = append(args, "--config", override, "--strict-config")
	}
	// Mere SessionName presence is NOT an attach signal: every current
	// producer (custom name or the platform's canonical id-shaped name
	// alike) sets a fresh session's name this way, and #480 unconditionally
	// running `resume <name>` here for a session that had never taken a
	// turn was the production defect this gate closes. resume is invoked
	// only when the caller explicitly signals attach-to-existing (see
	// agent.InteractiveSpec.ResumeExisting and interactive_name.go's
	// startNamedInteractiveAppServer doc comment for why the two shapes are
	// not interchangeable). A fresh named session's thread is instead
	// created by the PTY itself and named post-hoc — see
	// spawnNamedInteractivePTY.
	//
	// The resume path carries the session name as a second positional:
	// option parsing ends before it so both positionals follow the `--`.
	// Without this a seed starting with `-` parses as a flag and a seed
	// equal to a subcommand word (for example `resume`, `exec`, `config`)
	// starts that subcommand instead of a session. A resume with no seed
	// keeps its shape (no trailing `--`); a fresh session's seed is still
	// appended LAST, as today.
	if spec.SessionName != "" && attachToExistingNamedSession(spec) {
		args = append([]string{"resume"}, args...)
		if spec.Prompt != "" {
			args = append(args, "--", spec.SessionName, spec.Prompt)
		} else {
			args = append(args, spec.SessionName)
		}
	} else if spec.Prompt != "" {
		args = append(args, "--", spec.Prompt)
	}
	return interactiveLaunch{argv: args, env: env}, nil
}

func cloneInteractiveEnv(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// codexCLIMCPOverride renders the requested MCP servers as one process-local
// `mcp_servers` override. toolsApprovalMode, when non-empty, is written into
// every requested server's table as `default_tools_approval_mode` so the
// session does not stop on codex's one-review-per-tool-name approval; see
// approvals_seed.go. It is scoped to the servers named here, so a server the
// operator's ambient configuration defines keeps whatever posture it had.
func codexCLIMCPOverride(servers []agent.MCPServerConfig, env map[string]string, toolsApprovalMode string) (string, map[string]string, error) {
	if err := validateCodexCLIMCPServers(servers); err != nil {
		return "", nil, err
	}
	ordered := append([]agent.MCPServerConfig(nil), servers...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })

	var body strings.Builder
	body.WriteString("mcp_servers={")
	for i, server := range ordered {
		if i > 0 {
			body.WriteByte(',')
		}
		body.WriteString(tomlBasicString(server.Name))
		body.WriteString("={")
		if toolsApprovalMode != "" {
			body.WriteString("\"default_tools_approval_mode\"=")
			body.WriteString(tomlBasicString(toolsApprovalMode))
			body.WriteByte(',')
		}
		switch strings.ToLower(strings.TrimSpace(server.Type)) {
		case "", "stdio":
			body.WriteString("\"command\"=")
			body.WriteString(tomlBasicString(server.Command))
			body.WriteString(",\"args\"=")
			body.WriteString(tomlStringArray(server.Args))
			if len(server.Env) > 0 {
				keys := sortedStringKeys(server.Env)
				for _, key := range keys {
					if !validProcessEnvKey(key) {
						return "", nil, codexMCPApplicationError("stdio MCP environment contains an invalid variable name")
					}
					var err error
					env, err = setInteractiveEnv(env, key, server.Env[key])
					if err != nil {
						return "", nil, err
					}
				}
				body.WriteString(",\"env_vars\"=")
				body.WriteString(tomlStringArray(keys))
			}
		case "http":
			body.WriteString("\"url\"=")
			body.WriteString(tomlBasicString(server.URL))
			helper, hasHelper := agent.ProtectedRuntimeMCPHeadersHelper(server)
			if hasHelper {
				for name := range server.Headers {
					if strings.EqualFold(name, "Authorization") {
						return "", nil, codexMCPApplicationError("MCP header helper conflicts with explicit Authorization")
					}
				}
				body.WriteString(",\"http_headers_helper\"=")
				body.WriteString(tomlBasicString(helper))
			}
			if len(server.Headers) > 0 {
				headers := sortedStringKeys(server.Headers)
				body.WriteString(",\"env_http_headers\"={")
				for j, header := range headers {
					if j > 0 {
						body.WriteByte(',')
					}
					envName := codexHTTPHeaderEnvName(server.Name, header)
					var err error
					env, err = setInteractiveEnv(env, envName, server.Headers[header])
					if err != nil {
						return "", nil, err
					}
					body.WriteString(tomlBasicString(header))
					body.WriteByte('=')
					body.WriteString(tomlBasicString(envName))
				}
				body.WriteByte('}')
			}
		default:
			return "", nil, codexMCPApplicationError("MCP server type must be stdio or http")
		}
		body.WriteByte('}')
	}
	body.WriteByte('}')
	return body.String(), env, nil
}

func setInteractiveEnv(env map[string]string, key, value string) (map[string]string, error) {
	if existing, ok := env[key]; ok {
		if existing != value {
			return nil, codexMCPApplicationError("MCP environment conflicts with the inherited process variable " + key)
		}
		return env, nil
	}
	if env == nil {
		env = make(map[string]string)
	}
	env[key] = value
	return env, nil
}

func validProcessEnvKey(key string) bool {
	return key != "" && !strings.ContainsAny(key, "=\x00")
}

func codexHTTPHeaderEnvName(server, header string) string {
	sum := sha256.Sum256([]byte(server + "\x00" + header))
	return "DONMAI_MCP_HEADER_" + strings.ToUpper(hex.EncodeToString(sum[:]))
}

func validateCodexCLIMCPServers(servers []agent.MCPServerConfig) error {
	seen := make(map[string]struct{}, len(servers))
	for _, server := range servers {
		name := strings.TrimSpace(server.Name)
		if _, exists := seen[name]; exists {
			return codexMCPApplicationError("requested MCP server names must be unique")
		}
		seen[name] = struct{}{}
	}
	if _, err := mcp.BuildConfigFile(servers); err != nil {
		return codexMCPApplicationError("requested MCP server structure is invalid")
	}
	return nil
}

func sortedStringKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// tomlBasicString renders value as a TOML basic string: double-quoted, with
// `"` and `\` escaped, `\b \t \n \f \r` for those control characters,
// and `\uXXXX` for every other C0 control character (0x00-0x1f) and for DEL
// (0x7f). All other runes pass through literally, so non-ASCII text stays
// readable in argv and logs.
//
// A generic JSON encoder cannot serve here: it emits `\a` and `\v` (valid
// JSON, invalid TOML) and leaves DEL raw (invalid TOML), and the CLI this
// launch targets falls back to the raw literal when a `--config` value
// fails to parse as TOML — so the recipient would see the quoted literal
// (outer quotes, every newline as the two characters `\n`) instead of the
// intended text. Encoding every control character in TOML's own vocabulary
// keeps launch context byte-exact for any input text.
//
// Invalid UTF-8 bytes are replaced with U+FFFD, matching the prior encoder's
// behavior of never emitting raw invalid bytes.
func tomlBasicString(value string) string {
	var body strings.Builder
	body.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"':
			body.WriteString(`\"`)
		case '\\':
			body.WriteString(`\\`)
		case '\b':
			body.WriteString(`\b`)
		case '\t':
			body.WriteString(`\t`)
		case '\n':
			body.WriteString(`\n`)
		case '\f':
			body.WriteString(`\f`)
		case '\r':
			body.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				// Every other C0 control and DEL fits in two hex digits;
				// emit the fixed `\u00XX` form by hand so this helper
				// reports no error and never allocates a formatter.
				const hexDigits = "0123456789ABCDEF"
				var esc [6]byte
				esc[0], esc[1], esc[2], esc[3] = '\\', 'u', '0', '0'
				esc[4] = hexDigits[(r>>4)&0xF]
				esc[5] = hexDigits[r&0xF]
				body.Write(esc[:])
				continue
			}
			body.WriteRune(r)
		}
	}
	body.WriteByte('"')
	return body.String()
}

func tomlStringArray(values []string) string {
	var body strings.Builder
	body.WriteByte('[')
	for i, value := range values {
		if i > 0 {
			body.WriteByte(',')
		}
		body.WriteString(tomlBasicString(value))
	}
	body.WriteByte(']')
	return body.String()
}

func persistInteractiveMCPApplicationDenial(spec agent.Spec, applicationErr error) error {
	profile, ok := agent.SelectedToolLifecycleProfile(spec, (&Provider{}).Manifest())
	if !ok {
		profile = agent.ToolLifecycleProfile{ID: "codex/interactive/tool-lifecycle-v1", EvidenceTier: "unit_verified"}
	}
	receipt := agent.ToolLifecycleReceipt{
		ContractVersion:    agent.ToolLifecycleContractVersion,
		ProfileID:          profile.ID,
		Decision:           "denied",
		EvidenceTier:       profile.EvidenceTier,
		ProductionEligible: profile.ProductionEligible,
		Entries: []agent.ToolLifecycleEntry{{
			ID:         "mcp-servers",
			Channel:    agent.ToolChannelMCPServer,
			Required:   true,
			Outcome:    agent.ToolOutcomeDenied,
			DenialCode: agent.ToolDenialApplicationFailed,
		}},
	}
	if spec.OnToolLifecycleAdapted != nil {
		if err := spec.OnToolLifecycleAdapted(receipt); err != nil {
			return codexMCPApplicationError("persist denied Codex CLI MCP receipt: " + err.Error())
		}
	}
	return applicationErr
}
