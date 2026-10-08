package pi

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/ptycli"
	"github.com/RenseiAI/donmai/runtime/confinement"
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
// second file. Its provider registration (DONMAI_PI_BASE_URL/API/MODEL from env,
// the key from the session credential file) is what points pi at the resolved
// cell endpoint, and it runs
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
	// same posture as the headless lane (pi.go launch): the exec environment
	// is allowlisted (child_env.go), the credential file carries the
	// injected-provider key for the extension plus — in its environment
	// section — every other session binding the allowlist keeps out of the
	// exec environment, the native auth.json covers the --provider <name>
	// route, and only the file's PATH rides the child env. The environment
	// is partitioned ONCE so the file and the exec environment are two
	// halves of the same composition. Any write failure denies spawn closed,
	// before the child starts; the files are removed by the PTY cleanup below.
	childEnvParts := interactiveChildEnvParts(spec)
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

	// The child env carries the same routing pin + config-home isolation the
	// headless lane composes, MINUS the handshake token, over the same
	// allowlisted exec environment. It is the child's COMPLETE environment:
	// the PTY host is asked for an exact environment below (ExactEnv), so it
	// inherits nothing from the parent past the allowlist and no credential
	// name needs an empty shadow to mask an inherited value. The resolved
	// cell's credentials ride the files above; only the credential file's
	// path is added here.
	spec.Env = interactiveExecEnv(childEnvParts.exec, spec, layout)
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
			removeCredentials()
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

	handle, err := ptycli.SpawnWithOptions(ctx, wrapped[0], wrapped[1:], spec, p.Manifest(), ptycli.SpawnOptions{
		Cleanup: releaseInteractiveSession(plan, removeCredentials),
		// spec.Env is the child's complete environment: the PTY host must not
		// layer it over its own parent environment, or every parent name the
		// allowlist kept out would ride back in around it.
		ExactEnv: true,
	})
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

// interactiveChildEnvParts partitions the interactive session environment
// (child_env.go partitionChildEnv). The parent's terminal descriptors are
// dropped first: they describe the process that launched the harness, and
// the PTY host supplies the child's own interactive defaults, which only an
// explicit Spec.Env entry may replace.
func interactiveChildEnvParts(spec agent.Spec) childEnvPartition {
	return partitionChildEnv(withoutTerminalEnv(os.Environ()), spec)
}

// interactiveChildEnv builds the complete environment for an interactive
// spawn from this process's own environment and spec. Tests drive it;
// spawnInteractive builds the same map from the partition it also writes
// to the credential file.
func interactiveChildEnv(spec agent.Spec, layout sessionLayout) map[string]string {
	return interactiveExecEnv(interactiveChildEnvParts(spec).exec, spec, layout)
}

// interactiveExecEnv builds the interactive child's complete exec
// environment: the allowlisted half of the session partition (child_env.go
// — no credential value and no unlisted binding, so nothing a same-user
// listing of the renamed child could render is secret), the two documented
// config/session-home redirect vars headless also sets
// (piCodingAgentDirEnvVar/piCodingAgentSessionDirEnvVar — ADR-2026-08-12
// D4.1), the offline-posture defaults (D4.3 — the interactive lane is
// explicitly in scope, not just headless), the non-secret provider-pin vars
// the embedded extension reads at load, and the stamped
// allowed/disallowed-tools lists the SAME extension matches locally
// (interactiveToolPolicyEnv above). The credential file's PATH is added by
// the caller (spawnInteractive), not here.
//
// The PTY host spawns the child with exactly this map (ptyhost
// Spec.ExactEnv) plus its terminal defaults — it inherits nothing from the
// parent — so a name absent here is absent in the child. No credential name
// needs an empty shadow to mask an inherited value.
//
// It deliberately omits piHandshakeEnvVar: interactive PTY mode runs no Go
// handshake round-trip, and the extension skips the handshake (and does not
// block tools awaiting a verdict) exactly when that token is absent — so no UI
// artifact renders in the TUI. That is the one difference from headless
// composeChildEnv, and it is the whole point of the interactive posture.
func interactiveExecEnv(allowed []string, spec agent.Spec, layout sessionLayout) map[string]string {
	// No capacity hint: a Go map grows on demand, so pre-sizing it buys nothing
	// here, and summing len()s as an allocation size is exactly the shape a
	// static scanner (go/allocation-size-overflow) flags as a potential overflow.
	env := make(map[string]string)
	for _, kv := range allowed {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			env[kv[:i]] = kv[i+1:]
		}
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
	return env
}
