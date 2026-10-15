package pi

import (
	"context"
	"os"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

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
func (p *Provider) executionSecurity() agent.ExecutionSecurityRendering {
	if !p.opts.RequireConfinement || p.opts.skipProcess {
		return agent.ExecutionSecurityRendering{}
	}
	// Manifest has no caller context. Bound the initial two-mode self-test;
	// subsequent calls reuse its current, executable-bound record.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	probe, err := os.Executable()
	if err != nil {
		return agent.ExecutionSecurityRendering{}
	}
	confiner, err := ensurePiConfiner(ctx, p.binary, p.hostConfinementDirs(), []string{probe})
	if err != nil {
		return agent.ExecutionSecurityRendering{}
	}
	attestation, ok := confiner.Attestation()
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
