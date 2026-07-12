package attachwire

// Sequence namespaces (§4). There are two independent monotonic sequence
// spaces; conflating them is the defect §4 exists to prevent. These named
// types document intent at call sites — both are plain uint64 on the wire.

// HostSeq is a value in the host output sequence namespace (§4): assigned by
// the host to every host-produced frame (Output, applied Resize, Marker, Exit,
// Snapshot), starting at HostSeqStart and strictly increasing by 1 per host
// frame WITHIN ONE host stream epoch (§4.1). It is the space the relay ring
// buffer is keyed on and the space resume_from addresses (§13). The sole
// exception is the post-Exit final-screen Snapshot, which carries header
// seq = 0 (§12.2).
type HostSeq uint64

// InputSeq is a value in the viewer input sequence namespace (§4): assigned by
// each viewer CONNECTION to its own Input frames, starting at InputSeqStart,
// strictly increasing by 1 per input, and spanning carriers (WSS ↔ degraded
// MUST NOT reset it, §14). Acknowledged independently per connection via
// input_ack (§7). An Input frame's HEADER seq is NOT an InputSeq — it is the
// HostSeq the viewer had applied (the reconciliation anchor, §10); the viewer's
// own ordering lives in the payload InputSeq (§5).
type InputSeq uint64

// Epoch is a host stream epoch (§4.1): a monotonic non-negative integer minted
// by the control plane per host-process-start for a session and carried as the
// JWT epoch claim plus the subscribe echo. seq and rel_time are meaningful only
// relative to one epoch; a new epoch is a new room generation (seq restarts at
// HostSeqStart, rel_time restarts against the new process's spawn).
type Epoch uint64

const (
	// HostSeqStart is the first host output sequence value (§4).
	HostSeqStart HostSeq = 1
	// InputSeqStart is the first per-connection input sequence value (§4).
	InputSeqStart InputSeq = 1
	// PostExitSnapshotSeq is the header seq carried by a post-Exit Snapshot —
	// out-of-namespace, keyed by atSeq instead (§2, §12.2).
	PostExitSnapshotSeq = 0
)
