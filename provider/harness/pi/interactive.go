package pi

import (
	"context"
	"fmt"
	"strings"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/ptycli"
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
// terminal ResultEvent when the process exits.
func (p *Provider) spawnInteractive(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	// Materialize the embedded policy extension so its provider pin registers in
	// the child. A materialization failure means no pin — fail closed, exactly
	// as the headless lane does.
	layout, err := materializeExtension(spec.Cwd)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
	}

	// The child env carries the same routing pin + config-home isolation the
	// headless lane composes, MINUS the handshake token. ptyhost layers these
	// overrides onto the parent environment and drops blocklisted inherited keys
	// (ptyhost.composeEnv), so host credentials never reach the PTY child while
	// the resolved cell's key (already on spec.Env under PiKeyEnvVar) survives as
	// an explicit override.
	spec.Env = interactiveChildEnv(spec, layout)

	return ptycli.SpawnWithCleanup(ctx, p.binary, interactiveArgs(spec, layout), spec, p.Manifest(), nil)
}

// interactiveArgs builds the argv for pi's own interactive TUI.
//
// Base posture mirrors the headless rpcArgs' session-isolation flags MINUS
// `--mode rpc`: `-e <ext>` loads the embedded policy extension explicitly (so
// its provider pin registers regardless of project trust), `--no-extensions`
// disables all OTHER extension discovery so nothing can shadow the "donmai"
// provider registration, `--approve` trusts the fleet-provisioned worktree for
// this run (so a launched-then-attached session does not park on a trust modal
// before the human attaches), and `--session-dir` keeps session storage inside
// the worktree the runner's lifecycle owns.
//
// The provider pin (`--provider donmai --model <id>` when the cell binds an
// endpoint, plain `--model` otherwise) is the SAME modelPinArgs the headless
// lane uses. spec.Prompt seeds the first message as pi's positional prompt
// argument, kept LAST so it is never consumed as the value of a preceding flag;
// `--append-system-prompt` carries the composed session instructions.
func interactiveArgs(spec agent.Spec, layout sessionLayout) []string {
	args := []string{
		"-e", layout.extension,
		"--no-extensions",
		"--approve",
		"--session-dir", layout.root,
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

// interactiveChildEnv builds the ptycli override env for an interactive spawn.
// It carries the already-projected spec.Env (the resolved cell credentials,
// including PiKeyEnvVar), the four config-home redirect vars headless also sets
// (the exact PI_* home var is unverified against a real binary — see doc.go — so
// all candidates are set and win under ptyhost's last-entry precedence), and the
// non-secret provider-pin vars the embedded extension reads at load.
//
// It deliberately omits piHandshakeEnvVar: interactive PTY mode runs no Go
// handshake round-trip, and the extension skips the handshake (and does not
// block tools awaiting a verdict) exactly when that token is absent — so no UI
// artifact renders in the TUI. This is the ONE difference from headless
// composeChildEnv, and it is the whole point of the interactive posture.
func interactiveChildEnv(spec agent.Spec, layout sessionLayout) map[string]string {
	env := make(map[string]string, len(spec.Env)+len(providerPinEnv(spec.Endpoint, spec.Model))+4)
	for k, v := range spec.Env {
		env[k] = v
	}
	env["PI_HOME"] = layout.root
	env["PI_CONFIG_DIR"] = layout.root
	env["PI_STATE_DIR"] = layout.root
	env["XDG_CONFIG_HOME"] = layout.root
	for _, kv := range providerPinEnv(spec.Endpoint, spec.Model) {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}
	return env
}
