package daemon

import (
	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/kgextract"
	"github.com/RenseiAI/donmai/worker"
)

// baseSubstrateCapabilities are the capability tags every daemon can back: a
// worker on this machine runs local and workarea sessions.
var baseSubstrateCapabilities = []string{"local", "workarea"}

// sandboxCapability is advertised only by a daemon whose execution-security
// attestation proves an isolation boundary around the harness. A host must
// not advertise a sandbox it does not have
// (ADR-2026-09-27-execution-security-levels.md D3.5), and the tag is never a
// level: the per-dimension levels ride executionSecurityEnforcement.
const sandboxCapability = "sandbox"

// laneCapabilities are the capability tags for the non-agent work lanes EVERY
// PollService executes. They are appended unconditionally because the poll
// service always wires their executors (see NewPollService), so advertising
// them can never make this worker claim work it would drop.
//
// The order matters only for wire readability; MergeCapabilities dedupes.
var laneCapabilities = []string{kgextract.WorkTypeKGExtraction}

// receiptPreflightNackReasonCapability is a worker producer contract, not a
// claim lane. handlePollWorkItem owns the NACK sender unconditionally after a
// local accept-work refusal, so every registered daemon built with this code
// can truthfully advertise its closed typed reason projection.
const receiptPreflightNackReasonCapability = "donmai.receipt-preflight-nack:reason-v1"

// ExecutionPreflightRegistrationCapability is advertised only by a daemon
// that has both a registrar and an exact-replay receipt store wired.
const ExecutionPreflightRegistrationCapability = "execution-preflight-registration/v1"

var producerCapabilities = []string{receiptPreflightNackReasonCapability}

// effectiveRegistrationCapabilities computes the flat capability-tag list this
// daemon advertises at registration.
//
//   - a nil embedder list means "no opinion": the base substrate set is used,
//     preserving the historical wire for a daemon that sets nothing.
//   - an embedder-supplied list is taken verbatim (it may add its own tags, or
//     narrow the substrate set), and
//   - the lane and producer-contract tags are appended either way, deduped and
//     order-preserving.
//
// Appending is safe precisely because the lanes and producer contracts are
// wired in the daemon rather than by each embedder: a capability tag reaching
// the coordinator always has a matching implementation behind it on this host.
//
// The sandbox tag is the one substrate tag the daemon decides itself: it is
// removed from any list, the embedder's included, unless enforcement attests
// an isolation boundary, and added when it does.
func effectiveRegistrationCapabilities(embedder []string, enforcement agent.ExecutionSecurityEnforcement) []string {
	base := embedder
	if base == nil {
		base = baseSubstrateCapabilities
	}
	substrate := make([]string, 0, len(base)+1)
	for _, tag := range base {
		if tag != sandboxCapability {
			substrate = append(substrate, tag)
		}
	}
	if enforcement.AttestsSandbox() {
		substrate = append(substrate, sandboxCapability)
	}
	return worker.MergeCapabilities(substrate, append(laneCapabilities, producerCapabilities...)...)
}

// registrationExecutionSecurityEnforcement is the attestation published at
// registration. A daemon configured with no attestation applies no
// containment around the harness, so it attests index 0 on every substrate
// dimension — explicitly, so the control plane reads a statement rather than
// an absence.
func registrationExecutionSecurityEnforcement(configured *agent.ExecutionSecurityEnforcement) (agent.ExecutionSecurityEnforcement, error) {
	if configured == nil {
		return agent.UncontainedHostEnforcement(), nil
	}
	if err := configured.Validate(); err != nil {
		return agent.ExecutionSecurityEnforcement{}, err
	}
	out := *configured
	// Normalize absent substrate dimensions to their explicit index-0 value
	// so the published attestation names every dimension it covers.
	for _, dimension := range agent.ExecutionSecurityDimensions() {
		if dimension == agent.ExecutionSecurityToolApproval {
			continue
		}
		setEnforcementLevel(&out, dimension, out.Level(dimension))
	}
	return out, nil
}

func setEnforcementLevel(e *agent.ExecutionSecurityEnforcement, dimension agent.ExecutionSecurityDimension, level agent.ExecutionSecurityLevel) {
	switch dimension {
	case agent.ExecutionSecurityFileRead:
		e.FileRead = level
	case agent.ExecutionSecurityFileWrite:
		e.FileWrite = level
	case agent.ExecutionSecurityNetwork:
		e.Network = level
	case agent.ExecutionSecurityCredentials:
		e.Credentials = level
	case agent.ExecutionSecurityIsolation:
		e.Isolation = level
	}
}

func mergePreflightRegistrationCapability(capabilities []string) []string {
	return worker.MergeCapabilities(capabilities, ExecutionPreflightRegistrationCapability)
}

func preflightRegistrationCapabilities(capabilities []string, registrar ExecutionPreflightRegistrar, store ExecutionPreflightStore, provider ProviderRegistry) []string {
	if registrar == nil {
		return capabilities
	}
	if _, replayable := store.(ExecutionPreflightReplayStore); !replayable {
		return capabilities
	}
	if _, compiles := provider.(ExecutionPreflightProvider); !compiles {
		return capabilities
	}
	if _, validatesReplay := provider.(ExecutionPreflightReplayValidator); !validatesReplay {
		return capabilities
	}
	return mergePreflightRegistrationCapability(capabilities)
}
