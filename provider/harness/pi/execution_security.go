package pi

import (
	"context"
	"os"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/confinement"
)

// piSecurityAttestation returns the current per-harness attestation the
// rendering binds its mode scope to. A variable so tests can pin a
// single-mode attestation: both session modes pass the self-test on a real
// confined host, so no live test can tell the mode binding from its
// absence. Production always runs the executable-bound self-test.
var piSecurityAttestation = func(ctx context.Context, p *Provider, probe string) (confinement.Attestation, bool) {
	confiner, err := ensurePiConfiner(ctx, p.binary, p.hostConfinementDirs(), []string{probe})
	if err != nil {
		return confinement.Attestation{}, false
	}
	return confiner.Attestation()
}

// manifestAttestation is the single validated attestation one Manifest()
// evaluation shares between its execution-security rendering and its
// workarea caps. It returns false without calling piSecurityAttestation
// when this provider cannot attest (no host requirement or processless),
// so an unattested host declares neither stronger levels nor workarea
// protocols. Callers decide the mode and digest gating.
func (p *Provider) manifestAttestation() (confinement.Attestation, bool) {
	if !p.opts.RequireConfinement || p.opts.skipProcess {
		return confinement.Attestation{}, false
	}
	// Manifest has no caller context. Bound the initial two-mode self-test;
	// subsequent calls reuse its current, executable-bound record.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	probe, err := os.Executable()
	if err != nil {
		return confinement.Attestation{}, false
	}
	return piSecurityAttestation(ctx, p, probe)
}

// executionSecurityFromAttestation renders the attested execution-security
// levels for one already-obtained attestation. It performs no I/O: the
// single piSecurityAttestation call lives in manifestAttestation, shared
// with the workarea caps. A single-mode attestation still renders its one
// mode here; the workarea caps below require both exact modes.
func executionSecurityFromAttestation(attestation confinement.Attestation, ok bool) agent.ExecutionSecurityRendering {
	if !ok || len(attestation.SessionModes) == 0 || attestation.SelfTestDigest == "" {
		return agent.ExecutionSecurityRendering{}
	}
	return agent.ExecutionSecurityRendering{Levels: []agent.RenderedExecutionSecurityLevel{
		{
			Dimension: agent.ExecutionSecurityFileWrite, Level: agent.FileWriteWorkarea,
			Modes: attestation.SessionModes, Layers: []agent.EnforcingLayer{agent.LayerExecutorOSSandbox},
			EvidenceDigest: attestation.SelfTestDigest,
		},
		{
			Dimension: agent.ExecutionSecurityIsolation, Level: agent.IsolationOSSandbox,
			Modes: attestation.SessionModes, Layers: []agent.EnforcingLayer{agent.LayerExecutorOSSandbox},
			EvidenceDigest: attestation.SelfTestDigest,
		},
	}}
}

// workareaCapsFromAttestation derives the workarea declaration for one
// already-obtained attestation. Only a both-mode, nonempty-digest proof
// attests session-root-v1 with isolated-read-only-v1 enforcement: workarea
// registration has no per-mode split, so a single-mode proof leaves both
// fields empty while its execution-security rendering above still binds
// the one attested mode. Failed, degraded (Attestation refuses those),
// empty-digest, and processless proofs likewise leave both fields empty.
func workareaCapsFromAttestation(attestation confinement.Attestation, ok bool) (protocols []string, enforcement string) {
	if !ok || attestation.SelfTestDigest == "" || !hasBothSessionModes(attestation.SessionModes) {
		return nil, ""
	}
	return []string{"session-root-v1"}, "isolated-read-only-v1"
}

// hasBothSessionModes reports whether the attested modes cover both exact
// spawn paths the workarea registration binds: headless autonomous and
// interactive human-controlled. No subset attests a read-only leaf.
func hasBothSessionModes(modes []agent.PromptSessionMode) bool {
	hasAutonomous, hasHumanControlled := false, false
	for _, mode := range modes {
		switch mode {
		case agent.PromptModeAutonomous:
			hasAutonomous = true
		case agent.PromptModeHumanControlled:
			hasHumanControlled = true
		}
	}
	return hasAutonomous && hasHumanControlled
}

// executionSecurity is this host's attested rendering for pi. Only a host
// requiring confinement may declare the stronger write and isolation levels:
// every launch on that provider must apply the boundary or refuse. The
// per-harness self-test binds the current executable and backend, and its
// tested modes and digest travel with the declaration. A missing or stale
// attestation declares no stronger levels, so admission returns the typed
// unrenderable refusal before any session state or child is created.
//
// pi ships no permission system; the handshake-verified policy
// extension carries the deny entries on every built-in tool call, but its
// tool patterns match a text prefix of the raw command, which chaining
// defeats, so they are best effort and no list level is declared. Path
// containment on the file tools does not confine the bash tool.
