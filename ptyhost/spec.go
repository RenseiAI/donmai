package ptyhost

import (
	"log/slog"
	"os"
	"strings"

	"github.com/RenseiAI/donmai/agent"
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
)

// Default session parameters.
const (
	// DefaultCols / DefaultRows are the fallback PTY geometry (§8 initial
	// geometry; agent.InteractiveSpec 80×24 fallback).
	DefaultCols uint16 = 80
	DefaultRows uint16 = 24

	// DefaultRingBytes bounds the host output-frame ring buffer (payload-byte
	// accounting) when Spec.RingBytes is zero.
	DefaultRingBytes = 8 << 20 // 8 MiB

	// DefaultScrollback is the VT scrollback line cap serialized in the
	// snapshot tail (§12.1 draft default: 200 lines).
	DefaultScrollback = 200

	// maxOutputFrame bounds a single Output frame's payload; a larger read is
	// split across frames (§3.1 Output is length-delimited by the WS frame, but
	// the host caps per-frame size for backpressure sanity).
	maxOutputFrame = 32 * 1024

	// readBufSize is the PTY master read chunk size.
	readBufSize = 32 * 1024
)

// Spec is the input to Spawn. Command is required; every other field has a
// documented default.
type Spec struct {
	// Command is the argv to run; Command[0] is the binary (resolved on PATH by
	// os/exec). A nil or empty Command is an error.
	Command []string

	// Env is applied on top of the parent process environment as KEY=VALUE
	// overrides (last-wins). TERM=xterm-256color and COLORTERM=truecolor are
	// the interactive defaults even when the parent carries different values;
	// explicit entries in Env override those defaults.
	Env []string

	// Cwd is the child working directory. Empty inherits the parent's.
	Cwd string

	// Cols / Rows is the initial PTY geometry, applied to the winsize before the
	// child starts. Zero falls back to DefaultCols / DefaultRows.
	Cols uint16
	Rows uint16

	// RingBytes bounds the host output ring buffer by payload bytes. Zero falls
	// back to DefaultRingBytes.
	RingBytes int

	// Scrollback caps the VT scrollback tail serialized into a snapshot. Zero
	// falls back to DefaultScrollback.
	Scrollback int

	// OutputFlowControl, when non-nil, stops the PTY reader while a subscriber
	// is saturated instead of queueing without bound behind it.
	//
	// Nil is the released behaviour and stays the default: a subscriber that
	// stops draining is absorbed by an unbounded per-subscription queue. That
	// is correct for a short-lived local attach and wrong for a durable owner
	// whose consumer may stall for minutes, which is why the choice is the
	// spawning owner's rather than this package's.
	OutputFlowControl *OutputFlowControl

	// RecordPath is the asciinema-v2 cast destination (§16). Empty disables
	// recording. The cast shares the process-spawn rel_time anchor with the
	// wire, but is NOT a byte-exact copy of it: output events pass through
	// the recorder's own §9 escape-sequence sanitizer before being written to
	// disk (recorder.go), because the cast is a persistent artifact rather
	// than a live, ephemeral stream.
	RecordPath string

	// Epoch is the host stream epoch (§4.1) stamped into every serialized
	// Snapshot (attachwire.Screen.Epoch). The epoch VALUE is external (minted by
	// the control plane and carried by the attach client); the Session merely
	// anchors its host sequence and rel_time to its own process lifetime and
	// stamps this value. Zero is a valid default.
	Epoch uint64

	// NoticeDelivery is the DECLARED notice-delivery mechanism of the harness
	// whose child this PTY is running (agent.HarnessCaps.NoticeDelivery,
	// plumbed through by the spawning driver from the live manifest).
	//
	// It exists to make Session.TryWriteNotice structurally refusable rather
	// than caller-disciplined. Writing a self-submitting line into a PTY is
	// the CORRECT primitive only when nothing is interpreting keystrokes on
	// the other side — i.e. only for agent.NoticeDeliveryPTYNotice, which the
	// shell harness declares because there is no agent behind its terminal.
	// For a harness running its own UI the same bytes are a keystroke into
	// whatever that UI is drawing, and the submit byte picks the highlighted
	// option; the fact that the terminal cannot SEE that state is exactly why
	// the permission to write has to be declared up front instead of guessed
	// at write time.
	//
	// The zero value is "undeclared", which refuses. A session that means to
	// accept notices must say so.
	NoticeDelivery agent.NoticeDelivery

	// Logger receives non-fatal diagnostics (e.g. escape-unsafe snapshot cells
	// replaced with U+FFFD). Nil uses slog.Default().
	Logger *slog.Logger
}

func (s Spec) cols() uint16 {
	if s.Cols == 0 {
		return DefaultCols
	}
	return s.Cols
}

func (s Spec) rows() uint16 {
	if s.Rows == 0 {
		return DefaultRows
	}
	return s.Rows
}

func (s Spec) ringBytes() int {
	if s.RingBytes <= 0 {
		return DefaultRingBytes
	}
	return s.RingBytes
}

func (s Spec) scrollback() int {
	if s.Scrollback <= 0 {
		return DefaultScrollback
	}
	return s.Scrollback
}

func (s Spec) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// composeEnv layers the interactive terminal defaults over the parent
// environment, then applies Spec.Env last. Parent TERM/COLORTERM values describe
// the process that launched donmai, not the child PTY contract; only explicit
// per-request overrides may replace the interactive defaults. The existing
// runtime blocklist also applies to the inherited parent so a previously
// filtered runner environment cannot be undone by this final os.Environ merge.
func composeEnv(parent, overrides []string) []string {
	// Preallocate from one validated slice length. Overrides and the two fixed
	// terminal defaults grow through Go's checked map/slice runtime instead of
	// overflow-prone summed lengths.
	idx := make(map[string]int, len(parent))
	out := make([]string, 0, len(parent))
	blocklist := runtimeenv.NewComposer()
	put := func(kv string, inherited bool) {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if runtimeenv.IsRunnerOnly(key) || (inherited && blocklist.IsBlocked(key)) {
			return
		}
		if at, ok := idx[key]; ok {
			out[at] = kv
			return
		}
		idx[key] = len(out)
		out = append(out, kv)
	}
	for _, kv := range parent {
		put(kv, true)
	}
	put("TERM=xterm-256color", false)
	put("COLORTERM=truecolor", false)
	for _, kv := range overrides {
		put(kv, false)
	}
	return out
}

// shellName returns a best-effort SHELL basename for the recording header.
func shellName() string {
	sh := os.Getenv("SHELL")
	if sh == "" {
		return "/bin/sh"
	}
	return sh
}
