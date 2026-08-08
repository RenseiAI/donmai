package agent

// This file declares the PULL half of the notice-delivery axis.
//
// notice_delivery.go names WHICH mechanism a harness exposes
// (HarnessCaps.NoticeDelivery). This file is the seam a spawned session uses to
// hand that mechanism to the runner, for the mechanisms that are pull-shaped:
// the runner OFFERS a message and the harness collects it on its own schedule,
// at a lifecycle point only the harness controls.
//
// # Why pull needs its own seam
//
// InteractiveNotifier is push-shaped and synchronous: the bytes either reach
// the PTY or they do not, and the answer is known before TryWriteNotice
// returns. That is the correct shape for exactly one mechanism —
// NoticeDeliveryPTYNotice, where the terminal IS the recipient's input surface.
//
// Every application-level channel is different in kind. Claude Code's Stop hook
// is the archetype: the harness calls out when the current turn ends, and the
// message is delivered by ANSWERING that call. The runner cannot make the turn
// end, so it cannot make delivery happen; it can only make the message
// available and then find out whether it was taken. "Placed" and "delivered"
// are therefore two different facts separated by an unbounded amount of time,
// and a seam that cannot express the gap between them will report the first as
// the second.
//
// # The one rule this interface exists to enforce
//
// Consumed reports CONSUMPTION, never placement. A message is consumed when the
// recipient's own durable record shows it entered the conversation — not when
// the runner wrote it somewhere the recipient might read, and not when the
// harness's callout returned. Everything upstream (deliveredToLiveTurn, the
// producer's ack) hangs off that distinction: an implementation that returns
// true on placement converts every silently-dropped message into a delivered
// one, which is the exact failure this axis was introduced to stop.

// NoticeChannel is a live, PULL-shaped notice-delivery channel belonging to one
// spawned session. Implementations must be safe for concurrent use.
//
// The runner drives it as a three-state loop against ONE outstanding message at
// a time: Offer once, Consumed until it answers true, and Retract if the
// message must be withdrawn before it is taken.
type NoticeChannel interface {
	// Offer makes text available as the single outstanding notice, replacing
	// any previous offer that was never consumed.
	//
	// Offer says nothing about delivery. Returning nil means only that the
	// message is now where the harness will look; whether the harness ever
	// looks is what Consumed answers.
	Offer(deliveryID, text string) error

	// Consumed reports whether the outstanding offer has entered the
	// recipient's conversation, as evidenced by the recipient's OWN record of
	// it — not by anything the runner or the channel plumbing did.
	//
	// It must answer false, not an error, while the message is merely
	// outstanding: waiting is the normal state of a pull channel. An error
	// means the evidence could not be read at all.
	//
	// It must never answer true twice for two different offers on the strength
	// of one piece of evidence: an implementation matching on message content
	// has to advance past evidence it has already credited, or a repeated
	// message acks itself against its predecessor's record.
	Consumed() (bool, error)

	// Retract withdraws the outstanding offer so it can never be taken later.
	//
	// retracted == true means the message was still outstanding and is now
	// gone: it certainly was not, and now certainly cannot be, delivered.
	// retracted == false means the harness had already claimed it — the
	// message is out of the runner's hands, and whether it landed is Consumed's
	// answer, not Retract's.
	Retract() (retracted bool, err error)
}

// NoticeChannelCapable is the OPTIONAL capability a Handle implements when its
// session exposes a NoticeChannel. It mirrors InteractiveCapable: callers
// type-assert, and absence of the interface (or a nil channel) means this
// session has no pull channel to drive.
//
// A harness declaring a pull-shaped HarnessCaps.NoticeDelivery is NOT required
// to satisfy this — a declaration describes the harness, a channel describes
// one running session, and a session whose channel could not be established
// must be able to say so. Callers must treat a declared-but-absent channel as
// undeliverable-and-reported, never as a silent drop: the durable mailbox is
// the floor under every harness, and a message left unacked is a message the
// producer can still re-offer.
type NoticeChannelCapable interface {
	Handle

	// NoticeChannel returns the live pull channel, or nil when this session
	// has none.
	NoticeChannel() NoticeChannel
}
