package shimwire

// MessageType is the one-byte discriminator for the versioned, closed message
// vocabulary (ADR-2026-08-17 §D3). The selected integer version decides which
// assigned values are legal; selected v1 never accepts a v2 message.
type MessageType uint8

// The v1-frozen registry occupies 0x01–0x0C. Selected v1 treats every later
// value as reserved; selected v2 and v3 assign the values declared below.
const (
	TypeHello     MessageType = 0x01 // shim  -> daemon: range, identity, phase, bounds
	TypeWelcome   MessageType = 0x02 // daemon -> shim: selected version, proposed generation
	TypeAdopted   MessageType = 0x03 // shim  -> daemon: committed generation + replay disposition
	TypeOutput    MessageType = 0x04 // shim  -> daemon: shim-owned host sequence + raw bytes
	TypeGap       MessageType = 0x05 // shim  -> daemon: missing inclusive range + closed reason
	TypeSnapshot  MessageType = 0x06 // shim  -> daemon: state after a gap or on request
	TypeInput     MessageType = 0x07 // daemon -> shim: generation + attributed input bytes
	TypeResize    MessageType = 0x08 // daemon -> shim: generation + authoritative geometry
	TypeStop      MessageType = 0x09 // daemon -> shim: generation + typed reason
	TypeHeartbeat MessageType = 0x0A // both:   liveness + acknowledged sequence
	TypeExit      MessageType = 0x0B // shim  -> daemon: immutable terminal observation
	TypeError     MessageType = 0x0C // either: closed code + display-only detail

	// v2-only. These values remain illegal under selected v1.
	TypeSnapshotRequest MessageType = 0x0D // daemon -> shim: correlated authoritative request
	TypeSnapshotResult  MessageType = 0x0E // shim -> daemon: exact result or closed refusal

	// v3-only. This value remains illegal under selected v1 and v2.
	TypeHostFrame MessageType = 0x0F // shim -> daemon: request id + exact encoded attach frame

	// v4-only. This value remains illegal under selected v1, v2, and v3.
	TypeAttributedInput MessageType = 0x10 // daemon -> shim: generation + relay-stamped userId + input bytes
)

// Known reports whether t is assigned in the frozen v1 vocabulary. It remains
// v1-scoped for source compatibility; AllowedIn is required after selection.
func (t MessageType) Known() bool { return t >= TypeHello && t <= TypeError }

// AllowedIn reports whether t belongs to the selected version's closed
// vocabulary. A v2-capable peer selected at v1 therefore cannot send a v2
// type. Each version's vocabulary is a SUPERSET of the one before it — V4
// keeps HostFrame and SnapshotRequest/Result legal exactly as V3 does — so
// every existing `selected >= V3` (or `>= V2`) call site keeps working
// unchanged the moment negotiation reaches V4; only the brand-new
// TypeAttributedInput is exclusive to it.
func (t MessageType) AllowedIn(version uint32) bool {
	if t >= TypeHello && t <= TypeError {
		return version == V1 || version == V2 || version == V3 || version == V4
	}
	if t == TypeSnapshotRequest || t == TypeSnapshotResult {
		return version == V2 || version == V3 || version == V4
	}
	if t == TypeHostFrame {
		return version == V3 || version == V4
	}
	return version == V4 && t == TypeAttributedInput
}

// Mutating reports whether t carries controller authority and therefore MUST
// carry a controller generation (§D4). Read-only inspection (Hello/Heartbeat
// and every shim-produced observation) may omit it.
//
// Keeping this as one predicate rather than a check at each call site is
// deliberate: the ADR's split-brain risk is "a release path forgets the fence",
// and a per-caller check is exactly where an omission hides.
func (t MessageType) Mutating() bool {
	switch t {
	case TypeInput, TypeResize, TypeStop, TypeSnapshotRequest, TypeAttributedInput:
		return true
	default:
		return false
	}
}

func (t MessageType) String() string {
	switch t {
	case TypeHello:
		return "Hello"
	case TypeWelcome:
		return "Welcome"
	case TypeAdopted:
		return "Adopted"
	case TypeOutput:
		return "Output"
	case TypeGap:
		return "Gap"
	case TypeSnapshot:
		return "Snapshot"
	case TypeInput:
		return "Input"
	case TypeResize:
		return "Resize"
	case TypeStop:
		return "Stop"
	case TypeHeartbeat:
		return "Heartbeat"
	case TypeExit:
		return "Exit"
	case TypeError:
		return "Error"
	case TypeSnapshotRequest:
		return "SnapshotRequest"
	case TypeSnapshotResult:
		return "SnapshotResult"
	case TypeHostFrame:
		return "HostFrame"
	case TypeAttributedInput:
		return "AttributedInput"
	default:
		return "Unknown(0x" + hexByte(byte(t)) + ")"
	}
}

func hexByte(b byte) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[b>>4], digits[b&0x0F]})
}

// Phase is the shim's own lifecycle phase, reported in Hello. A daemon that
// cannot interpret a phase quarantines rather than guessing (§D7) — guessing is
// how an "unknown" state becomes an accidental kill.
type Phase string

// The closed v1 phase registry.
const (
	// PhaseStarting: the shim exists but the harness is not yet running.
	PhaseStarting Phase = "starting"
	// PhaseRunning: harness live, a controller is attached.
	PhaseRunning Phase = "running"
	// PhaseOrphaned: the controller is gone and the bounded orphan deadline is
	// counting down (§D8).
	PhaseOrphaned Phase = "orphaned"
	// PhaseExited: the harness has exited; the shim holds the immutable terminal
	// observation until a daemon durably reports it.
	PhaseExited Phase = "exited"
)

// Known reports whether p is an assigned v1 phase.
func (p Phase) Known() bool {
	switch p {
	case PhaseStarting, PhaseRunning, PhaseOrphaned, PhaseExited:
		return true
	default:
		return false
	}
}

// GapReason is the closed set of reasons a contiguous replay could not be
// served. It exists so a gap is always ATTRIBUTED: "we lost bytes" plus why,
// never a silent renumber.
type GapReason string

// The closed v1 gap-reason registry.
const (
	// GapRingEvicted: the requested position fell out of the bounded ring.
	GapRingEvicted GapReason = "ring_evicted"
	// GapAheadOfStream: the controller asked to resume past what the shim has
	// ever produced — a durable-state disagreement, reported rather than papered
	// over by rewinding the shim.
	GapAheadOfStream GapReason = "ahead_of_stream"
)

// Known reports whether r is an assigned v1 gap reason.
func (r GapReason) Known() bool {
	return r == GapRingEvicted || r == GapAheadOfStream
}

// StopReason is the closed set of typed reasons a controller may stop a shim.
type StopReason string

// The closed v1 stop-reason registry.
const (
	StopOperator     StopReason = "operator"      // an operator asked for it
	StopPolicy       StopReason = "policy"        // no-progress / duration policy
	StopHostShutdown StopReason = "host_shutdown" // the machine is going away
)

// Known reports whether r is an assigned v1 stop reason.
func (r StopReason) Known() bool {
	switch r {
	case StopOperator, StopPolicy, StopHostShutdown:
		return true
	default:
		return false
	}
}
