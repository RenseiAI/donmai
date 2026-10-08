package pi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/ptycli"
	"github.com/RenseiAI/donmai/runtime/confinement"
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
)

// spawnInteractive opens pi's OWN interactive TUI — bare `pi`, with NEITHER
// `--mode rpc` NOR `--mode json` (those select the headless RPC / single-shot
// lanes rpcArgs drives) — under a PTY via the shared ptycli driver, seeded with
// spec.Prompt when set.
//
// This is a distinct SPAWN MODE from the default headless RPC loop, not a
// different Transport: Manifest().Caps.Transport stays
// agent.TransportSubprocessRPC (the headless loop's transport); PTY is used only
// as a per-Spawn-call mode selected by Spec.Interactive != nil. See manifest.go
// and agent/harness.go's HarnessCaps.SupportsInteractivePTY doc comment.
//
// The spec is already admitted + endpoint-projected (Spawn ran prepare() before
// the interactive/headless split), so applyEndpoint has already honored
// Endpoint.Model over Spec.Model and mirrored the resolved cell key onto
// PiKeyEnvVar in spec.Env. This method therefore consumes the binding from
// birth: the provider pin argv and the DONMAI_PI_* pin env are minted from the
// already-projected spec, exactly as the headless lane mints them.
//
// Extension posture (program decision D4 / the A1 "slimmed extension"): the SAME
// embedded policy extension headless uses is materialized and loaded — never a
// second file. Its provider registration from env (DONMAI_PI_BASE_URL/API/MODEL
// + PiKeyEnvVar) is what points pi at the resolved cell endpoint, and it runs
// unconditionally at load with no RPC. What this spawn mode deliberately does
// NOT do is set the per-session handshake token (piHandshakeEnvVar): the
// extension's handshake + Go-adjudication round-trip are RPC-mode-only and are
// skipped when that env is absent (extensions/donmai-policy.ts). In PTY mode
// the human at the attached terminal plus pi's own native approval UI is the
// tool authority — the truthful pi/interactive tool-lifecycle profile declares
// that injected-boundary gap rather than inheriting the headless profile's
// evidence (D6).
//
// Event semantics are the coarse ptycli contract (D4 — the byte-accurate PTY
// stream is the product): an InitEvent once the PTY child is up and a single
// terminal ResultEvent when the process exits, plus best-effort activity
// tailed from this session's own transcript in between (see
// interactive_state_loss.go and interactive_transcript.go).
func (p *Provider) spawnInteractive(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	// Decide confinement before the parent writes anything into the
	// session's state, exactly as the headless lane does (launch): a
	// confined session's state is written through the strict writer.
	confiner, err := p.confinerForSession(ctx, spec)
	if err != nil {
		return nil, err
	}
	// Materialize the embedded policy extension so its provider pin registers in
	// the child. A materialization failure means no pin — fail closed, exactly
	// as the headless lane does. The state lands in the session state root
	// inside the working folder (confinement.go).
	layout, err := materializeExtensionForSpec(spec, confiner != nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
	}
	// Materialize + digest-verify spec.AdditionalExtensions the SAME way the
	// headless lane does (ADR-2026-08-12 D1 is host-agnostic across spawn
	// modes): a required delivery that cannot be materialized or verified
	// denies spawn closed, before the child ever starts. The exact interactive
	// profile admits this list via pi_additional_extension_registration; this
	// call is the concrete application step whose real bare-PTY registration
	// and invocation behavior is pinned in extension_delivery_test.go.
	extraPaths, err := materializeAdditionalExtensions(layout, spec.AdditionalExtensions)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
	}
	extensionPaths := append([]string{layout.extension}, extraPaths...)

	// Deliver session credentials through files, never the child env — the
	// same posture as the headless lane (pi.go launch): the credential file
	// carries the injected-provider key for the extension, the native
	// auth.json covers the --provider <name> route, and only the file's PATH
	// rides the child env. Any write failure denies spawn closed, before
	// the child starts; the files are removed by the PTY cleanup below.
	credentialPath, err := writeSessionCredentialFile(layout, sessionCredentialEntries(spec))
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

	// The child env carries the same routing pin + config-home isolation the
	// headless lane composes, MINUS the handshake token, MINUS credential
	// values. ptyhost layers these overrides onto the parent environment and
	// drops blocklisted inherited keys (ptyhost.composeEnv), so host
	// credentials never reach the PTY child; the resolved cell's credentials
	// ride the files above, and only the credential file's path survives as
	// an explicit override.
	spec.Env = interactiveChildEnv(spec, layout)
	if strings.TrimSpace(credentialPath) != "" {
		if spec.Env == nil {
			spec.Env = map[string]string{}
		}
		spec.Env[credentialFileEnvVar] = credentialPath
	}

	// Confine the interactive child the same way the headless lane confines
	// its own: the wrapped argv runs under the session profile, and the
	// plan's tmp/cache bindings ride the override env. A nil plan means the
	// session did not request confinement. The PTY child inherits only the
	// slave side of its terminal as its standard descriptors — ptyhost never
	// passes ExtraFiles — so no out-of-set descriptor reaches it either.
	var plan *confinement.Plan
	if confiner != nil {
		plan, err = p.confineSession(spec, layout, confiner)
		if err != nil {
			return nil, err
		}
	}
	// Snapshot the shared state dir's existing transcripts BEFORE the child
	// starts, so the transcript tail reads only what this session writes.
	tailer := newInteractiveTranscriptTailer(layout.root)

	wrapped, err := confinePiArgv(plan, append([]string{p.binary}, interactiveArgs(spec, layout, extensionPaths)...))
	if err != nil {
		if plan != nil {
			_ = plan.Release()
		}
		removeCredentials()
		return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
	}
	for _, kv := range confinePiEnv(nil, plan) {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			if spec.Env == nil {
				spec.Env = map[string]string{}
			}
			spec.Env[kv[:i]] = kv[i+1:]
		}
	}

	handle, err := ptycli.SpawnWithCleanup(ctx, wrapped[0], wrapped[1:], spec, p.Manifest(), releaseInteractiveSession(plan, removeCredentials))
	if err != nil {
		if plan != nil {
			_ = plan.Release()
		}
		removeCredentials()
		return nil, err
	}
	return newInteractiveStateLossHandle(handle, layout.root, tailer), nil
}

// releaseInteractiveSession turns the plan release plus the session
// credential-file removal into the ptycli per-session cleanup: it runs
// exactly once on spawn failure, child exit, context cancellation, or Stop,
// whichever happens first. The rendered profile must stay on disk until the
// confined child has exited; the profile is read when the wrapped command
// starts, so releasing once the session is over is safe, and a process
// already running under the profile stays confined. A nil plan (no backend)
// needs no profile cleanup, but the credential files are still removed.
func releaseInteractiveSession(plan *confinement.Plan, removeCredentials func()) func() error {
	return func() error {
		if removeCredentials != nil {
			removeCredentials()
		}
		if plan == nil {
			return nil
		}
		return plan.Release()
	}
}

// interactiveArgs builds the argv for pi's own interactive TUI.
//
// Base posture mirrors the headless rpcArgs' session-isolation flags MINUS
// `--mode rpc`: one `-e <path>` per entry in extensionPaths (the embedded
// policy extension first, then spec.AdditionalExtensions in order —
// ADR-2026-08-12 D1) loads each explicitly (so the provider pin and any
// additional extension's tools register regardless of project trust),
// `--no-extensions` disables all OTHER extension discovery so nothing can
// shadow them, `--approve` trusts the fleet-provisioned worktree for this run
// (so a launched-then-attached session does not park on a trust modal before
// the human attaches), and `--session-dir` keeps session storage inside the
// worktree the runner's lifecycle owns.
//
// The provider pin (`--provider donmai --model <id>` when the cell binds an
// endpoint, plain `--model` otherwise) is the SAME modelPinArgs the headless
// lane uses. spec.Prompt seeds the first message as pi's positional prompt
// argument, kept LAST so it is never consumed as the value of a preceding flag;
// `--append-system-prompt` carries the composed session instructions.
func interactiveArgs(spec agent.Spec, layout sessionLayout, extensionPaths []string) []string {
	var args []string
	for _, path := range extensionPaths {
		args = append(args, "-e", path)
	}
	args = append(args,
		"--no-extensions",
		"--approve",
		"--session-dir", layout.root,
	)
	if spec.SessionName != "" {
		args = append(args, "--name", spec.SessionName)
	}
	args = append(args, modelPinArgs(spec)...)
	if spec.SystemPromptAppend != "" {
		args = append(args, "--append-system-prompt", spec.SystemPromptAppend)
	}
	// Positional prompt LAST — every flag-shaped argument precedes it.
	if spec.Prompt != "" {
		args = append(args, spec.Prompt)
	}
	return args
}

// piStateDirEnvVar carries the relocated per-session state root onto the
// interactive child so the embedded extension's local state-dir guard
// (extensions/donmai-policy.ts stateDirRoots) covers the live session
// state exactly as the Go engine's stateDirDeletionReasonForRoots does.
// The headless lane needs no equivalent: its guard runs in-process in
// policy.go against engine.stateRoot, not via the child env.
const piStateDirEnvVar = "DONMAI_PI_STATE_DIR"

// piAllowedToolsEnvVar / piDisallowedToolsEnvVar carry a JSON-encoded
// Spec.AllowedTools / Spec.DisallowedTools array onto the interactive PTY
// child so the SAME embedded policy extension can answer the allowed/
// disallowed-tools channel LOCALLY (agent.ToolDeliveryPiInteractiveLocalToolPolicy
// — manifest.go, agent/tool_adaptation.go). This is deliberately an
// INTERACTIVE-ONLY mechanism: composeChildEnv (headless) never sets these —
// the headless lane already answers the same two Spec fields through the
// full RPC-backed policy.go engine (NativeToolPolicyDelivery:
// ToolDeliveryPiInjectedBoundary), and running a second, narrower local gate
// alongside it would risk the two disagreeing over the same fields.
const (
	piAllowedToolsEnvVar    = "DONMAI_PI_ALLOWED_TOOLS"
	piDisallowedToolsEnvVar = "DONMAI_PI_DISALLOWED_TOOLS"
)

// interactiveToolPolicyEnv JSON-encodes the stamped tool-designator lists for
// the extension's local matcher (extensions/donmai-policy.ts). A list is
// omitted entirely when empty, so a session that stamped neither field
// carries no local gate at all — mirroring
// agent/tool_adaptation.go legacyToolRequirements, which only ever projects
// ToolChannelAllowedTools/DisallowedTools requirements when the Spec field is
// non-empty.
func interactiveToolPolicyEnv(spec agent.Spec) []string {
	var out []string
	if len(spec.AllowedTools) > 0 {
		if b, err := json.Marshal(spec.AllowedTools); err == nil {
			out = append(out, piAllowedToolsEnvVar+"="+string(b))
		}
	}
	if len(spec.DisallowedTools) > 0 {
		if b, err := json.Marshal(spec.DisallowedTools); err == nil {
			out = append(out, piDisallowedToolsEnvVar+"="+string(b))
		}
	}
	return out
}

// interactiveChildEnv builds the ptycli override env for an interactive spawn.
// It carries the already-projected spec.Env MINUS session credential values
// (stripped first: the resolved cell credentials ride the session credential
// file, never the child env), MINUS any inherited-credential shadow (a
// supervisor-declared name the PTY host would otherwise inherit from the
// parent into the renamed child — see shadowCredentialDeclaredParent below),
// the two documented config/session-home redirect vars headless also sets
// (piCodingAgentDirEnvVar/piCodingAgentSessionDirEnvVar — ADR-2026-08-12
// D4.1), the offline-posture defaults (D4.3 — the interactive lane is
// explicitly in scope, not just headless), the non-secret provider-pin vars
// the embedded extension reads at load, and the stamped
// allowed/disallowed-tools lists the SAME extension matches locally
// (interactiveToolPolicyEnv above). The credential file's PATH is composed by
// the caller (spawnInteractive), not here.
//
// The shadow entries are the interactive counterpart of the headless lane's
// stripCredentialNamedEnv backstop (spec_translation.go composeChildEnv):
// the PTY host layers these overrides onto the parent environment and
// inherits any name the override map does not mention — including a
// supervisor-declared credential the parent carries — so a credential-named
// entry must be present here to keep the parent's value out, exactly as the
// headless lane drops it after the merge. An empty value reads as absent to
// the child (no entry in sessionCredentialEntries is ever empty), never as
// a usable key.
//
// It deliberately omits piHandshakeEnvVar: interactive PTY mode runs no Go
// handshake round-trip, and the extension skips the handshake (and does not
// block tools awaiting a verdict) exactly when that token is absent — so no UI
// artifact renders in the TUI. That and the credential strip are the
// differences from headless composeChildEnv, and they are the whole point of
// the interactive posture.
func interactiveChildEnv(spec agent.Spec, layout sessionLayout) map[string]string {
	// No capacity hint: a Go map grows on demand, so pre-sizing it buys nothing
	// here, and summing len()s as an allocation size is exactly the shape a
	// static scanner (go/allocation-size-overflow) flags as a potential overflow.
	spec = stripSessionCredentialEnv(spec)
	env := make(map[string]string)
	// Runner-only attach controls address the supervisor, never the workload:
	// refuse them from the snapshot layer exactly as composeChildEnv (via the
	// shared Composer) does on the headless lane. Without this, a snapshot
	// key riding Spec.Env would reach the PTY child headless refuses it from.
	for k, v := range runtimeenv.FilterRunnerOnlyMap(spec.Env) {
		env[k] = v
	}
	env[piCodingAgentDirEnvVar] = layout.agentHome
	env[piCodingAgentSessionDirEnvVar] = layout.root
	env[piStateDirEnvVar] = layout.root
	for _, kv := range offlinePostureEnv(spec) {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}
	for _, kv := range providerPinEnv(spec) {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}
	for _, kv := range interactiveToolPolicyEnv(spec) {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}
	// Shadow every credential name the session does not itself carry: the PTY
	// host inherits unmentioned names from the parent, so without an explicit
	// (empty) override a supervisor-declared credential the parent carries
	// would reach the renamed child — the exact hole the headless lane's
	// post-merge strip closes. Names the session DOES carry were already
	// fanned out to the credential file above; shadowing them too would
	// change nothing (both are empty/absent reads to the child) but would
	// hide a merge regression, so only the absent ones are shadowed. The
	// undeclared secret-valued names are shadowed unconditionally: they ride
	// neither the file nor the env on this harness, so there is no
	// session-carried value to preserve.
	for _, name := range sessionCredentialNames() {
		if _, present := env[name]; !present {
			env[name] = ""
		}
	}
	for _, name := range undeclaredSecretValueNames {
		if _, present := env[name]; !present {
			env[name] = ""
		}
	}
	return env
}
