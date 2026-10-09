package sessionshim

import "github.com/RenseiAI/donmai/shimwire"

// Workload is the closed set of session workloads a shim can own.
//
// An interactive shim owns a PTY-hosted session: a harness process group, a
// VT/snapshot stream, a bounded replay ring, and flow control. A headless
// shim owns a runner's process group directly: no PTY, no output stream, no
// ring. The value rides the discovery record so a daemon that predates the
// headless profile reads the record as malformed and quarantines it
// (record_malformed) rather than treating it as a PTY session it can adopt.
type Workload string

// The closed workload registry.
const (
	// WorkloadInteractive is a PTY-hosted session. It is the default: an
	// interactive record omits the field, so released readers keep byte-exact
	// behavior.
	WorkloadInteractive Workload = "interactive"
	// WorkloadHeadless is a runner-owned session with no PTY and no host
	// output stream. Its sole terminal observation is the runner's own exit.
	WorkloadHeadless Workload = "headless"
)

// Known reports whether w is an assigned workload.
func (w Workload) Known() bool {
	return w == WorkloadInteractive || w == WorkloadHeadless
}

// HeadlessExitCause is the closed set of reasons a headless run ended. It is
// the wire registry itself (shimwire.HeadlessExitCause), not a copy: the
// tombstone and the HeadlessExit frame carry the same members, so one closed
// registry decides both and they can never disagree.
type HeadlessExitCause = shimwire.HeadlessExitCause

// The closed headless-exit-cause registry, exactly the contract's set.
const (
	// HeadlessExitCompleted: the runner reached its own end, successful or not.
	HeadlessExitCompleted = shimwire.HeadlessExitCompleted
	// HeadlessExitOrphaned: the orphan deadline ended the run.
	HeadlessExitOrphaned = shimwire.HeadlessExitOrphaned
	// HeadlessExitStoppedForResume: the run stopped for a later resume; it
	// writes no session terminal.
	HeadlessExitStoppedForResume = shimwire.HeadlessExitStoppedForResume
	// HeadlessExitShimFailure: written only by an adopting controller's
	// janitor, never sent on the wire.
	HeadlessExitShimFailure = shimwire.HeadlessExitShimFailure
)

// HeadlessOutboxState is the closed set of terminal-status outbox states a
// headless tombstone can report: the wire registry (shimwire.HeadlessExitState)
// for the same reason as HeadlessExitCause.
type HeadlessOutboxState = shimwire.HeadlessExitState

// The closed headless-outbox-state registry.
const (
	// HeadlessOutboxDelivered: the terminal record reached its receiver.
	HeadlessOutboxDelivered = shimwire.HeadlessExitDelivered
	// HeadlessOutboxPending: the terminal record is persisted but not yet
	// delivered.
	HeadlessOutboxPending = shimwire.HeadlessExitPending
	// HeadlessOutboxNone: no terminal record exists. Valid only beside
	// HeadlessExitStoppedForResume, which writes no session terminal.
	HeadlessOutboxNone = shimwire.HeadlessExitNone
)

// HeadlessExit is the one immutable terminal observation of a headless
// lineage. It carries the runner's own exit beside the outbox record that
// holds the session terminal, so a replacement daemon can replay a pending
// record or accept a delivered one without re-running the seat.
type HeadlessExit struct {
	// Cause names why the run ended.
	Cause HeadlessExitCause `json:"cause"`
	// OutboxKey names the session's terminal-status outbox record.
	OutboxKey string `json:"outboxKey,omitempty"`
	// OutboxState is that record's delivery state.
	OutboxState HeadlessOutboxState `json:"outboxState,omitempty"`
	// GroupReaped is true only after the harness process group was proved gone.
	GroupReaped bool `json:"groupReaped"`
}

// Validate enforces the headless-exit contract.
func (e HeadlessExit) Validate() error {
	if !e.Cause.Known() {
		return invalidRecordf("unknown headless exit cause %q", string(e.Cause))
	}
	// An empty outbox state names no outbox record: the tombstone carries
	// the runner's exit before any terminal-status record exists (the
	// persist-before-send half lands separately). An explicit none keeps
	// the wire meaning — valid only beside stopped_for_resume, which
	// writes no session terminal.
	if e.OutboxState == "" {
		if e.OutboxKey != "" {
			return invalidRecordf("headless outbox key without an outbox state")
		}
		return nil
	}
	if !e.OutboxState.Known() {
		return invalidRecordf("unknown headless outbox state %q", string(e.OutboxState))
	}
	if e.OutboxState == HeadlessOutboxNone && e.Cause != HeadlessExitStoppedForResume {
		return invalidRecordf("headless outbox state none requires cause stopped_for_resume")
	}
	return nil
}
