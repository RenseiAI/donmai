package stub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/ptycli"
	"github.com/RenseiAI/donmai/provider/harness/stub/stubagent"
)

// EnvStubAgentBin names the binary the interactive spawn mode runs under the
// PTY, overriding the default of "this very process". It exists for an
// operator driving a stub session from a build other than the running one.
//
// It is read ONCE, at New(), exactly as codex reads $CODEX_BIN and pi reads
// $PI_BIN — never per Spec. A binary chosen per session is a different
// provider, not a different request, and letting a Spec pick one would give
// every caller of a shared registry-held provider a way to change what the
// host executes.
//
// The value is passed to exec as-is, so a bare name is resolved by LookPath
// and a path is not. That lookup uses the SPAWNING process's $PATH, not the
// child's: ptyhost.Spawn calls exec.Command, which resolves the name at
// construction, and only afterwards assigns the composed cmd.Env — so a $PATH
// placed in Spec.Env has no bearing on which binary is found. Either way the
// stub-agent subcommand is appended, because the override names a DONMAI
// BINARY, not an arbitrary program. A test that must run something else uses
// WithStubAgentCommand, which supplies its own argv.
const EnvStubAgentBin = "DONMAI_STUB_AGENT_BIN"

// StubAgentSubcommand is the argv this binary answers the fake agent on. The
// child is THIS binary re-invoked, not a separate artifact, so an integration
// environment cannot end up running a stub agent from one build against a
// daemon from another. The literal lives with the child's own environment
// contract so the spawner and the CLI registration cannot drift.
const StubAgentSubcommand = stubagent.CommandName

// Config keys for the interactive spawn mode. They mirror the environment
// variables the child reads, so a caller can supply a scenario through the
// typed Spec.ProviderConfig without also having to know how Spec.Env is
// composed on its way to the process.
const (
	// ScenarioConfigKey carries a scenario as either a JSON string or a
	// decoded object.
	ScenarioConfigKey = "stub.scenario"
	// ScenarioFileConfigKey carries a path to a scenario file.
	ScenarioFileConfigKey = "stub.scenarioFile"
)

// interactiveStopGrace bounds the Stop issued when PTY seed delivery fails.
// It matches the window shell uses for the same failure.
const interactiveStopGrace = 10 * time.Second

// MaxScenarioFileBytes caps the scenario file the parent will read while
// validating a spawn. A scenario is a short script — the largest this repo
// ships is a few hundred bytes — so the ceiling is generous by orders of
// magnitude and exists only to keep a validation step from becoming a way to
// make Spawn read something enormous.
const MaxScenarioFileBytes = 1 << 20 // 1 MiB

// spawnInteractive runs the deterministic fake agent under a PTY through the
// SHARED interactive driver (provider/harness/ptycli) — the same call the
// claude, codex, pi and shell harnesses make.
//
// Routing through that driver rather than re-implementing a PTY spawn here is
// the whole point of this mode: it is what makes a stub session
// indistinguishable from a real one to everything upstream of the child —
// ptyhost's process group and geometry, session-shim adoption when the
// controller asked this process to be a shim, the ring/recorder/attach
// surface, and the SIGTERM -> grace -> SIGKILL stop escalation. A stub that
// spawned its own PTY would prove the stub works and nothing else.
func (p *provider) spawnInteractive(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	manifest := p.Manifest()
	prepared, err := agent.PrepareHarness(spec, manifest)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}
	binary, argv, err := p.stubAgentCommand()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}
	prepared.Env, err = withScenarioEnv(prepared.Env, spec.ProviderConfig)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}
	// PREPARED, not spec: what the child records must be what the adaptation
	// compiler actually admitted for this session, not what the caller asked
	// for before it ran.
	prepared.Env, err = withToolPolicyEnv(prepared.Env, prepared)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}

	handle, err := ptycli.Spawn(ctx, binary, argv, prepared, manifest)
	if err != nil {
		return nil, err
	}
	deliverSeed := p.deliverSeed
	if deliverSeed == nil {
		deliverSeed = ptycli.DeliverSeed
	}
	if err := deliverSeed(ctx, handle, handle.InteractiveSession(), prepared.Prompt); err != nil {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), interactiveStopGrace)
		_ = handle.Stop(stopCtx)
		stopCancel()
		// PreparePrompt already persisted a READY receipt for these bytes.
		// The write failed and the session is being abandoned, so leaving that
		// record standing would claim a prompt reached an agent that never
		// existed. Retract it before returning the error.
		if receiptErr := ptycli.DenyPromptReceiptAfterSeedFailure(prepared); receiptErr != nil {
			return nil, fmt.Errorf(
				"%w: stub PTY seed delivery: %w; persist denial receipt: %w",
				agent.ErrSpawnFailed, err, receiptErr,
			)
		}
		return nil, fmt.Errorf("%w: stub PTY seed delivery: %w", agent.ErrSpawnFailed, err)
	}
	return handle, nil
}

// resolveStubAgentCommand applies WithStubAgentCommand -> $DONMAI_STUB_AGENT_BIN,
// the same option-then-environment order codex and pi use for their binaries.
// It runs at New() so the choice belongs to the provider, and returns empty to
// mean "no override" — the default is deferred to spawn time (see
// stubAgentCommand).
func resolveStubAgentCommand(binary string, argv []string) (string, []string) {
	if binary != "" {
		return binary, argv
	}
	if override := strings.TrimSpace(os.Getenv(EnvStubAgentBin)); override != "" {
		// The override names a donmai binary, so it still needs the
		// subcommand. Without it the child runs a bare `donmai`, prints help
		// and exits 0 — which every layer above reads as a clean session that
		// ran no scenario at all.
		return override, []string{StubAgentSubcommand}
	}
	return "", nil
}

// stubAgentCommand resolves the program to run under the PTY.
//
// With no override the answer is os.Executable() re-invoked on
// StubAgentSubcommand. That is deliberate: the fake agent ships INSIDE the
// binary that spawns it, so there is no separate artifact to install, to find,
// or to accidentally leave at a different version than the daemon driving it.
//
// This one step is resolved per spawn rather than at New() because New() is
// infallible and is called for EVERY registry build, including hosts that only
// ever use the headless mode. A failure to identify our own executable must
// deny the one interactive spawn that needs it, not remove the stub provider
// from the registry entirely.
func (p *provider) stubAgentCommand() (string, []string, error) {
	if p.agentBinary != "" {
		return p.agentBinary, append([]string(nil), p.agentArgv...), nil
	}
	self, err := os.Executable()
	if err != nil {
		return "", nil, fmt.Errorf("resolve own executable for the %s child: %w", StubAgentSubcommand, err)
	}
	return self, []string{StubAgentSubcommand}, nil
}

// withScenarioEnv projects the typed ProviderConfig scenario knobs onto the
// environment the child reads. An explicit Spec.Env entry wins: the operator
// who set the variable by hand is the more specific authority, and silently
// overwriting it would make a debugging session lie.
func withScenarioEnv(env map[string]string, config map[string]any) (map[string]string, error) {
	inline, err := scenarioConfigJSON(config)
	if err != nil {
		return nil, err
	}
	file, _ := config[ScenarioFileConfigKey].(string)

	// Copy rather than mutate: the caller's Spec.Env map is shared with the
	// Spec it was built from, and a harness that quietly writes into it would
	// leak this session's scenario into the next spawn from the same Spec.
	out := make(map[string]string, len(env)+2)
	for key, value := range env {
		out[key] = value
	}
	set := func(key, value string) {
		if value == "" || out[key] != "" {
			return
		}
		out[key] = value
	}
	set(stubagent.EnvScenario, inline)
	set(stubagent.EnvScenarioFile, strings.TrimSpace(file))

	if err := validateScenarioFile(out); err != nil {
		return nil, err
	}
	return out, nil
}

// withToolPolicyEnv projects the tool-permission policy this session received
// onto the child's environment, so the child can record it in its own
// transcript (stubagent.EnvToolPolicy).
//
// This is the observable half of the manifest's claim. The stub's interactive
// tool/lifecycle profile declares the native tool-policy channel satisfied by
// construction (agent.ToolDeliveryNoToolSurface) — the child registers no
// tools, so a deny-list has nothing left to forbid and an allow-list nothing
// left to withhold. That claim is only auditable if the policy that arrived is
// written down somewhere a caller can read, and the session transcript is the
// one artifact that reaches every layer above this one.
//
// A session that received no policy sets nothing, so the PRESENCE of the
// variable — and of the child's line — is itself the evidence that a policy
// arrived. As everywhere else in this file, an explicit Spec.Env entry wins:
// the operator who set the variable by hand is the more specific authority.
func withToolPolicyEnv(env map[string]string, spec agent.Spec) (map[string]string, error) {
	policy := stubagent.ToolPolicy{AllowedTools: spec.AllowedTools, DisallowedTools: spec.DisallowedTools}
	if policy.Empty() || env[stubagent.EnvToolPolicy] != "" {
		return env, nil
	}
	encoded, err := stubagent.EncodeToolPolicy(policy)
	if err != nil {
		return nil, err
	}
	// Copy rather than mutate, for the same reason withScenarioEnv does: the
	// map may still be shared with the Spec it was built from.
	//
	// The capacity hint is a bare len(env), not len(env)+1: a hint is only a
	// hint (the map grows on its own for the one extra key), and arithmetic on
	// a caller-supplied length inside make() is an allocation-size-overflow
	// shape a static analyser flags — correctly in general, even though no
	// Spec.Env can approach the bound here.
	out := make(map[string]string, len(env))
	for key, value := range env {
		out[key] = value
	}
	out[stubagent.EnvToolPolicy] = encoded
	return out, nil
}

// validateScenarioFile reads and parses the scenario file the child is about
// to be pointed at.
//
// Validating the inline form and forwarding the file form unchecked would be
// the worse half of both: the inline path refuses a malformed scenario at
// spawn precisely so a child that exits on garbage is never mistaken for a
// child that was ASKED to exit — and a missing or malformed file produces
// exactly that same indistinguishable exit. The file is read here, in the
// parent, because for every environment this harness serves the parent and the
// child share a filesystem.
//
// The inline form wins when both are set, and is already validated, so this
// only runs when the file is what the child will actually read.
func validateScenarioFile(env map[string]string) error {
	if env[stubagent.EnvScenario] != "" {
		return nil
	}
	path := env[stubagent.EnvScenarioFile]
	if path == "" {
		return nil
	}
	// Stat before reading, and insist on a REGULAR file. os.ReadFile on a
	// FIFO blocks until a writer appears, which for a named pipe nobody opens
	// is forever — and this runs inside Spawn, so an unbounded read here is an
	// unbounded Spawn. A device or directory is refused for the same reason:
	// validation must not be a way to make the parent read something whose
	// length it cannot know in advance.
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s %s: %w", ScenarioFileConfigKey, path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf(
			"%s %s: not a regular file (%s); a scenario must be readable without blocking on a writer",
			ScenarioFileConfigKey, path, info.Mode().Type(),
		)
	}
	if info.Size() > MaxScenarioFileBytes {
		return fmt.Errorf(
			"%s %s: %d bytes exceeds the %d-byte scenario ceiling",
			ScenarioFileConfigKey, path, info.Size(), MaxScenarioFileBytes,
		)
	}

	// Read through a bounded reader rather than trusting the size just
	// stat'ed: the file can grow between the two calls, and the ceiling is
	// only a ceiling if the read enforces it too.
	handle, err := os.Open(path) //nolint:gosec // the path is the caller's own scenario file
	if err != nil {
		return fmt.Errorf("%s %s: %w", ScenarioFileConfigKey, path, err)
	}
	defer func() { _ = handle.Close() }()

	data, err := io.ReadAll(io.LimitReader(handle, MaxScenarioFileBytes+1))
	if err != nil {
		return fmt.Errorf("%s %s: %w", ScenarioFileConfigKey, path, err)
	}
	if len(data) > MaxScenarioFileBytes {
		return fmt.Errorf(
			"%s %s: exceeds the %d-byte scenario ceiling",
			ScenarioFileConfigKey, path, MaxScenarioFileBytes,
		)
	}
	if _, err := stubagent.Parse(data); err != nil {
		return fmt.Errorf("%s %s: %w", ScenarioFileConfigKey, path, err)
	}
	return nil
}

// scenarioConfigJSON accepts the scenario either as a JSON string or as an
// already-decoded object, and validates it HERE rather than letting a
// malformed scenario reach the child. A child that exits on a bad scenario is
// indistinguishable at the session layer from a scenario that asked to exit,
// so the spawn refuses instead.
func scenarioConfigJSON(config map[string]any) (string, error) {
	raw, ok := config[ScenarioConfigKey]
	if !ok || raw == nil {
		return "", nil
	}
	var encoded []byte
	if text, isString := raw.(string); isString {
		if strings.TrimSpace(text) == "" {
			return "", nil
		}
		encoded = []byte(text)
	} else {
		marshalled, err := json.Marshal(raw)
		if err != nil {
			return "", fmt.Errorf("encode %s: %w", ScenarioConfigKey, err)
		}
		encoded = marshalled
	}
	if _, err := stubagent.Parse(encoded); err != nil {
		return "", fmt.Errorf("%s: %w", ScenarioConfigKey, err)
	}
	return string(encoded), nil
}
