package stub

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/ptycli"
	"github.com/RenseiAI/donmai/provider/harness/stub/stubagent"
)

// EnvStubAgentBin names an explicit path to the program the interactive spawn
// mode runs under the PTY. It exists for an operator running a stub session
// from a tree whose binary is not the one on $PATH; ordinary use needs no
// override, because the default is this very process (see stubAgentCommand).
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
	binary, argv, err := p.stubAgentCommand(prepared)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}
	prepared.Env, err = withScenarioEnv(prepared.Env, spec.ProviderConfig)
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
		return nil, fmt.Errorf("%w: stub PTY seed delivery: %w", agent.ErrSpawnFailed, err)
	}
	return handle, nil
}

// stubAgentCommand resolves the program to run under the PTY.
//
// The default is os.Executable() re-invoked on StubAgentSubcommand. That is
// deliberate: the fake agent ships INSIDE the binary that spawns it, so there
// is no separate artifact to install, to find on a $PATH, or to accidentally
// leave at a different version than the daemon driving it.
func (p *provider) stubAgentCommand(spec agent.Spec) (string, []string, error) {
	if p.agentBinary != "" {
		return p.agentBinary, append([]string(nil), p.agentArgv...), nil
	}
	if override := specEnv(spec, EnvStubAgentBin); override != "" {
		return override, nil, nil
	}
	if override := strings.TrimSpace(os.Getenv(EnvStubAgentBin)); override != "" {
		return override, nil, nil
	}
	self, err := os.Executable()
	if err != nil {
		return "", nil, fmt.Errorf("resolve own executable for the %s child: %w", StubAgentSubcommand, err)
	}
	return self, []string{StubAgentSubcommand}, nil
}

func specEnv(spec agent.Spec, key string) string {
	if spec.Env == nil {
		return ""
	}
	return strings.TrimSpace(spec.Env[key])
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
	return out, nil
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
