package daemon

import (
	"github.com/RenseiAI/donmai/kgextract"
	"github.com/RenseiAI/donmai/worker"
)

// baseSubstrateCapabilities are the capability tags a daemon has always
// advertised at registration: a worker on this machine can run local, sandbox,
// and workarea sessions.
var baseSubstrateCapabilities = []string{"local", "sandbox", "workarea"}

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
func effectiveRegistrationCapabilities(embedder []string) []string {
	base := embedder
	if base == nil {
		base = baseSubstrateCapabilities
	}
	return worker.MergeCapabilities(base, append(laneCapabilities, producerCapabilities...)...)
}
