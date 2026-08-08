package conformance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/attachwire"
)

// This file holds the fake adapters for the TERMINAL notice rail — the one
// `shell` really uses and the runner's interactive supervisor really drives
// (runner/interactive_inject.go). The Handle.Inject fakes live in
// fakeharness_test.go; these are separate because the rail is separate: an
// interactive PTY handle answers agent.ErrUnsupported to Inject, and delivery
// happens through agent.InteractiveNotifier.TryWriteNotice instead.
//
// As in fakeharness_test.go, one configuration is conformant and the rest are
// broken in exactly one way each, so every branch of the PTY rail has a
// subject that must not pass.

// ptyMode is how a fake terminal surface behaves when handed a notice.
type ptyMode int

const (
	// ptyDeliver is the only conformant behavior: the terminal echoes the
	// typed line AND the session acts on it, producing a second occurrence.
	ptyDeliver ptyMode = iota
	// ptyEchoOnly displays the typed line and never acts on it — the lie the
	// occurrence rule exists to catch. Bytes left this process; nothing
	// received them.
	ptyEchoOnly
	// ptyRefuse always answers (false, nil): the surface is permanently
	// unwilling, which a caller must never round up to delivered.
	ptyRefuse
	// ptyWriteErr fails the write outright.
	ptyWriteErr
	// ptyNotNotifier exposes an interactive session that cannot accept
	// notices at all (the runner's noticeDeadSurfaceNotNotifier shape).
	ptyNotNotifier
	// ptyNotInteractive returns a plain Handle for a manifest that declares
	// pty-notice: there is no terminal to write into.
	ptyNotInteractive
)

type fakePTYConfig struct {
	mode ptyMode
	// notice overrides the declared mechanism (defaults to pty-notice).
	notice agent.NoticeDelivery
	// spawnErr fails Spawn.
	spawnErr error
}

// fakePTYHarness is a provider whose manifest declares the terminal rail.
type fakePTYHarness struct{ cfg fakePTYConfig }

var _ agent.HarnessProvider = (*fakePTYHarness)(nil)

func newFakePTY(cfg fakePTYConfig) *fakePTYHarness {
	if cfg.notice == "" {
		cfg.notice = agent.NoticeDeliveryPTYNotice
	}
	return &fakePTYHarness{cfg: cfg}
}

func (*fakePTYHarness) Name() agent.ProviderName { return agent.ProviderName("fake-pty") }

// Capabilities deliberately reports SupportsMessageInjection=false, exactly as
// shell does: the terminal rail is not the Handle.Inject rail, and a suite
// that reads this field to judge the terminal channel is reading the wrong
// fact.
func (*fakePTYHarness) Capabilities() agent.Capabilities {
	return agent.Capabilities{HumanLabel: "Fake PTY Harness"}
}

func (f *fakePTYHarness) Manifest() agent.HarnessManifest {
	prompt, tools := headlessProfiles()
	return agent.HarnessManifest{
		Name:        agent.HarnessName("fake-pty"),
		HumanLabel:  "Fake PTY Harness",
		Family:      agent.FamilyHarness,
		ContractABI: "harness/v2",
		Caps: agent.HarnessCaps{
			SupportsInteractivePTY: true,
			NoticeDelivery:         f.cfg.notice,
			Transport:              agent.TransportPTY,
		},
		PromptDelivery: prompt,
		ToolLifecycle:  tools,
	}
}

func (f *fakePTYHarness) Spawn(_ context.Context, spec agent.Spec) (agent.Handle, error) {
	if f.cfg.spawnErr != nil {
		return nil, f.cfg.spawnErr
	}
	h := &fakePTYHandle{
		events: make(chan agent.Event, 4),
		sess:   &fakeTerminal{cfg: f.cfg},
	}
	h.events <- agent.InitEvent{SessionID: "fake-pty-session"}
	// The seed prompt is typed at the terminal, exactly as ptycli.DeliverSeed
	// does, so the screen carries it before any notice arrives.
	h.sess.appendLine(spec.Prompt)
	if f.cfg.mode == ptyNotInteractive {
		return &plainHandle{inner: h}, nil
	}
	return h, nil
}

func (*fakePTYHarness) Resume(context.Context, string, agent.Spec) (agent.Handle, error) {
	return nil, agent.ErrUnsupported
}

func (*fakePTYHarness) Shutdown(context.Context) error { return nil }

// fakePTYHandle is the interactive-capable handle.
type fakePTYHandle struct {
	events   chan agent.Event
	sess     *fakeTerminal
	stopOnce sync.Once
}

var _ agent.InteractiveCapable = (*fakePTYHandle)(nil)

func (*fakePTYHandle) SessionID() string { return "fake-pty-session" }

func (h *fakePTYHandle) Events() <-chan agent.Event { return h.events }

// Inject mirrors ptycli.Handle: in interactive mode the terminal is the input
// surface, so the Inject rail is genuinely unavailable here.
func (*fakePTYHandle) Inject(context.Context, string) error {
	return fmt.Errorf("fake-pty: Inject: %w (the terminal is the input surface)", agent.ErrUnsupported)
}

func (h *fakePTYHandle) Stop(context.Context) error {
	h.stopOnce.Do(func() {
		h.events <- agent.ResultEvent{Success: true}
		close(h.events)
	})
	return nil
}

func (h *fakePTYHandle) InteractiveSession() agent.InteractiveSession {
	if h.sess.cfg.mode == ptyNotNotifier {
		return notifierlessTerminal{h.sess}
	}
	return h.sess
}

// plainHandle strips the interactive seam off a manifest that declares
// pty-notice — a harness with no terminal to write into. It forwards the
// agent.Handle method set explicitly rather than embedding, because embedding
// would promote InteractiveSession() straight back and the fake would no
// longer be broken in the way it exists to be broken in.
type plainHandle struct{ inner *fakePTYHandle }

var _ agent.Handle = (*plainHandle)(nil)

func (h *plainHandle) SessionID() string                          { return h.inner.SessionID() }
func (h *plainHandle) Events() <-chan agent.Event                 { return h.inner.Events() }
func (h *plainHandle) Inject(ctx context.Context, s string) error { return h.inner.Inject(ctx, s) }
func (h *plainHandle) Stop(ctx context.Context) error             { return h.inner.Stop(ctx) }

// notifierlessTerminal is a live PTY surface that cannot accept notices.
type notifierlessTerminal struct{ agent.InteractiveSession }

// fakeTerminal is the live surface. It models exactly what the occurrence
// rule depends on: a terminal echoes what is typed at it, and a session that
// acts on the line produces additional output.
type fakeTerminal struct {
	cfg fakePTYConfig

	mu    sync.Mutex
	lines []string
	done  chan struct{}
}

var (
	_ agent.InteractiveSession  = (*fakeTerminal)(nil)
	_ agent.InteractiveNotifier = (*fakeTerminal)(nil)
)

func (t *fakeTerminal) appendLine(s string) {
	if strings.TrimSpace(s) == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = append(t.lines, s)
}

func (t *fakeTerminal) TryWriteNotice(p []byte) (bool, error) {
	switch t.cfg.mode {
	case ptyRefuse:
		return false, nil
	case ptyWriteErr:
		return false, errors.New("fake-pty: pty master write failed")
	}
	line := strings.TrimRight(string(p), "\r\n")
	// The terminal echoes the typed line. This alone is NOT delivery.
	t.appendLine("$ " + line)
	if t.cfg.mode == ptyDeliver {
		// …and the session acted on it, producing its own output.
		t.appendLine(strings.TrimPrefix(line, "echo "))
	}
	return true, nil
}

func (t *fakeTerminal) WriteInput(p []byte) (int, error) {
	t.appendLine(strings.TrimRight(string(p), "\r\n"))
	return len(p), nil
}

func (*fakeTerminal) Resize(uint32, uint32, uint32, uint32) error { return nil }

// Snapshot renders the accumulated terminal text as scrollback lines, which is
// where a real host puts everything that has scrolled above the visible rows.
func (t *fakeTerminal) Snapshot() (attachwire.Screen, attachwire.HostSeq, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	screen := attachwire.Screen{Cols: 200, Rows: uint64(len(t.lines))}
	for _, line := range t.lines {
		cells := make([]attachwire.Cell, 0, len(line))
		for _, r := range line {
			cells = append(cells, attachwire.Cell{RuneBytes: []byte(string(r))})
		}
		screen.Scrollback = append(screen.Scrollback, cells)
	}
	return screen, attachwire.HostSeq(len(t.lines)), nil
}

func (*fakeTerminal) EmitSnapshot() (attachwire.Frame, bool, error) {
	return attachwire.Frame{}, false, errors.New("fake-pty: EmitSnapshot unused")
}

func (*fakeTerminal) EmitMarker(string) error { return nil }

func (*fakeTerminal) Subscribe(attachwire.HostSeq) (agent.InteractiveSubscription, error) {
	return nil, errors.New("fake-pty: Subscribe unused")
}

func (t *fakeTerminal) Done() <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done == nil {
		t.done = make(chan struct{})
	}
	return t.done
}

func (*fakeTerminal) Exit() (attachwire.ExitPayload, bool) { return attachwire.ExitPayload{}, false }

// ptySubject is the baseline subject for the terminal rail. BaseSpec carries
// Spec.Interactive because a harness that declares pty-notice has no headless
// mode to fall back to.
func ptySubject(cfg fakePTYConfig) Subject {
	return Subject{
		Provider:     newFakePTY(cfg),
		BaseSpec:     agent.Spec{Interactive: &agent.InteractiveSpec{Cols: 200, Rows: 50}},
		EchoPrompt:   func(nonce string) string { return "echo " + nonce },
		ProbeTimeout: 5 * time.Second,
	}
}
