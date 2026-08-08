package agent

// This file declares the NOTICE-DELIVERY AXIS: how — if at all — a message
// authored elsewhere reaches an agent that is ALREADY RUNNING.
//
// # Why this is a declared axis and not an assumption
//
// Every harness in this repo can be handed a prompt at spawn time. Almost none
// of them accept one afterwards through the same door, and the doors they do
// accept differ in kind: a hook the harness calls, a JSON-RPC method on a
// subprocess app-server, an HTTP POST onto a live server session, a steering
// command on an RPC stdio channel, a fresh `--resume` invocation, or nothing at
// all. Typing bytes at a terminal is NOT a substitute for any of them: a PTY
// write reaches the terminal, not the agent, and any inline prompt the agent is
// drawing (an approval, a plan confirmation, a trust dialog) will consume those
// bytes as a menu selection. That is why pty-notice is reserved for harnesses
// with NO agent behind them.
//
// So the mechanism is declared per harness, on the manifest, and a harness that
// has none says so. A silent best-effort drop is not a legitimate degradation:
// the caller that queued the message must be able to learn what happened to it.
//
// # What this axis does NOT promise
//
// A declaration says the HARNESS exposes the channel. It does not say this
// build drives it. Those are separate facts and conflating them is how a
// message gets accepted, acknowledged, and never delivered. Consumers that
// actually push a message must check both — see the runner's interactive
// supervisor, which refuses (and never acknowledges) a channel it cannot drive.
//
// # The floor under every harness
//
// Live push is a LATENCY OPTIMISATION. The durable mailbox with a pull tool the
// agent calls is the floor beneath all of this, which is what makes
// NoticeDelivery == NoticeDeliveryNone a survivable declaration rather than an
// unreachable agent: the message waits to be collected instead of being typed
// at a terminal and hoped for.

// NoticeDelivery names the mechanism by which a message can be delivered into
// an already-running session on a harness.
//
// The zero value is deliberately NOT NoticeDeliveryNone. An empty value means
// UNDECLARED — a manifest that has not answered the question — and is treated
// as a denial by ValidateSpecCapabilities, so a new harness cannot inherit a
// silent "no live delivery" by omission, nor a silent "yes" by accident.
type NoticeDelivery string

// The declared notice-delivery mechanisms.
//
// Each constant names a channel some harness genuinely exposes. Adding a
// constant is not adding a capability: a harness gains one only by declaring it
// on its own manifest, and only after the mechanism has been verified against
// that harness's own CLI or documentation.
const (
	// NoticeDeliveryNone declares that the harness exposes NO way to deliver a
	// message into a running session. This is a legitimate, first-class
	// declaration — the durable mailbox is the delivery path for these
	// harnesses, and the agent collects from it. It is the honest answer for a
	// single-shot CLI wrap (`agy -p`) or a one-request-per-turn HTTP loop.
	NoticeDeliveryNone NoticeDelivery = "none"

	// NoticeDeliveryHook declares a harness-invoked hook: the harness itself
	// calls out at a lifecycle point (Claude Code's Stop hook) and the hook's
	// response is the injection point. The harness exposes the hook; whether a
	// given hook decision actually blocks and re-prompts is a separate,
	// empirically-verified question owned by the lane that implements it.
	NoticeDeliveryHook NoticeDelivery = "hook"

	// NoticeDeliveryMCPRPC declares a JSON-RPC control surface on a subprocess
	// the harness runs for exactly this purpose (Codex's `app-server`, and its
	// `mcp-server` sibling). Delivery is a method call, not a keystroke.
	NoticeDeliveryMCPRPC NoticeDelivery = "mcp-rpc"

	// NoticeDeliveryHTTPSession declares a long-lived local HTTP server owning
	// named sessions, where a message is a POST onto a live session
	// (`opencode serve` + POST /api/session/:id/prompt).
	NoticeDeliveryHTTPSession NoticeDelivery = "http-session"

	// NoticeDeliveryACP declares the Agent Client Protocol over stdio
	// (`gemini --acp`, formerly `--experimental-acp`): the client drives
	// session/prompt against a running agent. Reserved and unused today — the
	// Gemini harness in this repo is the in-box generateContent loop, not the
	// `gemini` CLI, so nothing here may declare ACP until a CLI-wrapping
	// harness exists to expose it.
	NoticeDeliveryACP NoticeDelivery = "acp"

	// NoticeDeliveryRPCSteer declares a line-delimited RPC channel with an
	// explicit steer/follow-up verb (`pi --mode rpc`: steer while a turn is in
	// flight, follow_up while idle).
	NoticeDeliveryRPCSteer NoticeDelivery = "rpc-steer"

	// NoticeDeliveryResumeInject declares that the only way in is to start a
	// NEW invocation that continues the existing conversation
	// (`claude --resume <id>`, `amp threads continue <threadId>`). Delivery is
	// real but it is not delivery into the live process: the running session
	// must be finished, or the resumed one becomes a second writer.
	NoticeDeliveryResumeInject NoticeDelivery = "resume-inject"

	// NoticeDeliveryInBoxLoop declares that the agent loop runs INSIDE this
	// process, so a message is appended to the conversation directly through
	// Handle.Inject with no external channel involved. There is no third-party
	// harness to route around: the loop is ours.
	NoticeDeliveryInBoxLoop NoticeDelivery = "in-box-loop"

	// NoticeDeliveryPTYNotice declares that writing the message into the PTY
	// is the CORRECT primitive — which is true exactly when there is no agent
	// behind the terminal to route around, i.e. the shell harness. For any
	// harness running an agent UI, a PTY write is a keystroke into whatever
	// that UI is currently drawing, so it is not a delivery mechanism and must
	// not be declared here.
	NoticeDeliveryPTYNotice NoticeDelivery = "pty-notice"
)

// noticeDeliveryValues is the closed set of declarable mechanisms.
var noticeDeliveryValues = map[NoticeDelivery]struct{}{
	NoticeDeliveryNone:         {},
	NoticeDeliveryHook:         {},
	NoticeDeliveryMCPRPC:       {},
	NoticeDeliveryHTTPSession:  {},
	NoticeDeliveryACP:          {},
	NoticeDeliveryRPCSteer:     {},
	NoticeDeliveryResumeInject: {},
	NoticeDeliveryInBoxLoop:    {},
	NoticeDeliveryPTYNotice:    {},
}

// Declared reports whether n is one of the known mechanisms. The empty value
// (an unanswered manifest) is NOT declared.
func (n NoticeDelivery) Declared() bool {
	_, ok := noticeDeliveryValues[n]
	return ok
}

// CanDeliver reports whether n names a mechanism that carries messages at all
// — i.e. it is declared AND is not NoticeDeliveryNone. It says nothing about
// whether the local build drives that mechanism; see the file comment.
func (n NoticeDelivery) CanDeliver() bool {
	return n.Declared() && n != NoticeDeliveryNone
}
