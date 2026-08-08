package conformance

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// This file holds the fake adapters the suite is tested against. One is
// conformant; the rest are deliberately non-conformant in exactly one way
// each, so every check has a subject it must reject. Without them a green
// suite would only prove the suite runs, not that it can fail anything.

// injectMode is how a fake adapter behaves when a notice is injected.
type injectMode int

const (
	// injectDeliver echoes the injected text back into the event stream —
	// the only behavior that proves delivery.
	injectDeliver injectMode = iota
	// injectDrop accepts the notice, returns nil, and delivers nothing. This
	// is the lie the live-notice tier exists to catch: a caller is told the
	// message was accepted and the agent never sees it.
	injectDrop
	// injectUnsupported refuses the notice the manifest declared.
	injectUnsupported
	// injectSlowUnsupported refuses too, but only AFTER the session has
	// finished draining. It is the shape the probe used to lose: the
	// injecting goroutine's error was read with a non-blocking select the
	// instant the drain returned, so a late answer was discarded.
	injectSlowUnsupported
	// injectHang never answers at all.
	injectHang
)

// injectSlowDelay outlasts fakeConfig.noticeGrace, so a fake using
// injectSlowUnsupported reliably returns after the drain has ended.
const injectSlowDelay = 900 * time.Millisecond

// injectHangDelay is effectively forever for a test: long enough that the
// suite's own grace expires first, short enough that the goroutine is not
// parked for the life of the binary.
const injectHangDelay = 30 * time.Second

// fakeConfig configures a fake adapter. The zero value is conformant.
type fakeConfig struct {
	notice agent.NoticeDelivery
	// undeclaredNotice leaves NoticeDelivery empty — the manifest that never
	// answered the axis at all, which newFake's default would otherwise fill in.
	undeclaredNotice bool
	supportInject    bool
	supportResume    bool
	interactivePTY   bool

	spawnErr  error
	resumeErr error
	stopErr   error

	// script overrides the default event sequence. It receives the spawn
	// prompt so the default can echo it.
	script func(sessionID, prompt string) []agent.Event

	inject   injectMode
	holdOpen bool // never close the events channel

	// promptDelivery and toolLifecycle populate the adaptation manifest.
	// Both default to a well-formed autonomous-mode pair; the omit flags
	// produce the half-authored manifest a contributed harness ships.
	promptDelivery    []agent.PromptDeliveryProfile
	toolLifecycle     []agent.ToolLifecycleProfile
	omitPromptProfile bool
	omitToolProfile   bool

	// noticeGrace bounds how long a live-notice session waits for an inject
	// that never arrives. Zero means a short default.
	noticeGrace time.Duration
}

func newFake(cfg fakeConfig) *fakeHarness {
	// The default is in-box-loop, not hook: in-box-loop is carried by
	// Handle.Inject, which is the rail these fakes implement. Defaulting to a
	// channel nothing drives would make every unrelated fake exercise the
	// undriven path instead of the one under test.
	if cfg.notice == "" && !cfg.undeclaredNotice {
		cfg.notice = agent.NoticeDeliveryInBoxLoop
	}
	if cfg.noticeGrace == 0 {
		cfg.noticeGrace = 500 * time.Millisecond
	}
	if cfg.script == nil {
		cfg.script = defaultScript
	}
	prompt, tools := headlessProfiles()
	if cfg.promptDelivery == nil && !cfg.omitPromptProfile {
		cfg.promptDelivery = prompt
	}
	if cfg.toolLifecycle == nil && !cfg.omitToolProfile {
		cfg.toolLifecycle = tools
	}
	return &fakeHarness{cfg: cfg}
}

// defaultScript is a conformant sequence: one Init first, an assistant
// message that echoes the prompt, one terminal Result last.
func defaultScript(sessionID, prompt string) []agent.Event {
	return []agent.Event{
		agent.InitEvent{SessionID: sessionID},
		agent.AssistantTextEvent{Text: "acknowledged: " + prompt},
		agent.ResultEvent{Success: true, Message: "done"},
	}
}

type fakeHarness struct {
	cfg fakeConfig

	mu     sync.Mutex
	spawns int
}

var _ agent.HarnessProvider = (*fakeHarness)(nil)

func (f *fakeHarness) Name() agent.ProviderName { return agent.ProviderName("fake") }

func (f *fakeHarness) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		SupportsMessageInjection: f.cfg.supportInject,
		SupportsSessionResume:    f.cfg.supportResume,
		HumanLabel:               "Fake Harness",
	}
}

func (f *fakeHarness) Manifest() agent.HarnessManifest {
	return agent.HarnessManifest{
		Name:        agent.HarnessName("fake"),
		HumanLabel:  "Fake Harness",
		Family:      agent.FamilyHarness,
		ContractABI: "harness/v2",
		Caps: agent.HarnessCaps{
			SupportsMessageInjection: f.cfg.supportInject,
			SupportsSessionResume:    f.cfg.supportResume,
			SupportsInteractivePTY:   f.cfg.interactivePTY,
			NoticeDelivery:           f.cfg.notice,
			Drives:                   []agent.WireProtocol{agent.ProtoStub},
			DrivesHosts:              []agent.ServingHost{agent.HostLocal},
			Transport:                agent.TransportDirectAPI,
		},
		PromptDelivery: f.cfg.promptDelivery,
		ToolLifecycle:  f.cfg.toolLifecycle,
	}
}

func (f *fakeHarness) Spawn(_ context.Context, spec agent.Spec) (agent.Handle, error) {
	if f.cfg.spawnErr != nil {
		return nil, f.cfg.spawnErr
	}
	f.mu.Lock()
	f.spawns++
	id := "fake-session-" + strconv.Itoa(f.spawns)
	f.mu.Unlock()
	return f.start(id, spec, spec.RequiresLiveNotice), nil
}

func (f *fakeHarness) Resume(_ context.Context, sessionID string, spec agent.Spec) (agent.Handle, error) {
	if f.cfg.resumeErr != nil {
		return nil, f.cfg.resumeErr
	}
	if !f.cfg.supportResume {
		return nil, agent.ErrUnsupported
	}
	if sessionID == "" {
		return nil, fmt.Errorf("fake: Resume needs a session id")
	}
	return f.start(sessionID, spec, false), nil
}

func (f *fakeHarness) Shutdown(context.Context) error { return nil }

func (f *fakeHarness) start(sessionID string, spec agent.Spec, awaitNotice bool) *fakeHandle {
	h := &fakeHandle{
		cfg:         f.cfg,
		sessionID:   sessionID,
		events:      make(chan agent.Event, 64),
		deliver:     make(chan string, 4),
		injectSeen:  make(chan struct{}, 4),
		stopped:     make(chan struct{}),
		script:      f.cfg.script(sessionID, spec.Prompt),
		awaitNotice: awaitNotice,
	}
	go h.run()
	return h
}

type fakeHandle struct {
	cfg         fakeConfig
	sessionID   string
	events      chan agent.Event
	deliver     chan string
	injectSeen  chan struct{}
	stopped     chan struct{}
	script      []agent.Event
	awaitNotice bool

	stopOnce sync.Once
}

func (h *fakeHandle) SessionID() string { return h.sessionID }

func (h *fakeHandle) Events() <-chan agent.Event { return h.events }

func (h *fakeHandle) Inject(_ context.Context, text string) error {
	switch h.cfg.inject {
	case injectDeliver:
		select {
		case h.deliver <- text:
		default:
		}
		return nil
	case injectDrop:
		// Accepted, acknowledged, never delivered.
		select {
		case h.injectSeen <- struct{}{}:
		default:
		}
		return nil
	case injectUnsupported:
		select {
		case h.injectSeen <- struct{}{}:
		default:
		}
		return agent.ErrUnsupported
	case injectSlowUnsupported:
		time.Sleep(injectSlowDelay)
		select {
		case h.injectSeen <- struct{}{}:
		default:
		}
		return agent.ErrUnsupported
	case injectHang:
		time.Sleep(injectHangDelay)
		return nil
	}
	return nil
}

func (h *fakeHandle) Stop(context.Context) error {
	h.stopOnce.Do(func() { close(h.stopped) })
	return h.cfg.stopErr
}

func (h *fakeHandle) run() {
	if !h.cfg.holdOpen {
		defer close(h.events)
	}
	for i, ev := range h.script {
		h.events <- ev
		// A live-notice session pauses once, right after announcing itself,
		// so an injected message has a running session to arrive at.
		if i == 0 && h.awaitNotice {
			h.awaitInject()
		}
	}
}

func (h *fakeHandle) awaitInject() {
	select {
	case text := <-h.deliver:
		h.events <- agent.AssistantTextEvent{Text: "received: " + text}
	case <-h.injectSeen:
	case <-h.stopped:
	case <-time.After(h.cfg.noticeGrace):
	}
}

// perTokenScript emits assistant text one small fragment at a time — the
// shape CheckCompleteAssistantTexts must reject.
func perTokenScript(sessionID, prompt string) []agent.Event {
	events := []agent.Event{agent.InitEvent{SessionID: sessionID}}
	for _, token := range []string{"ack", "now", "ledg", "ing", " the", " pro", "mpt", " for", " you", " now"} {
		events = append(events, agent.AssistantTextEvent{Text: token})
	}
	_ = prompt
	return append(events, agent.ResultEvent{Success: true})
}

// twoTerminalScript is the shape a shipped adapter really produced: a
// successful run followed by a spurious error event.
func twoTerminalScript(sessionID, prompt string) []agent.Event {
	return []agent.Event{
		agent.InitEvent{SessionID: sessionID},
		agent.AssistantTextEvent{Text: "acknowledged: " + prompt},
		agent.ResultEvent{Success: true},
		agent.ErrorEvent{Code: "spawn_no_result", Message: "no result captured"},
	}
}

// noInitScript starts straight into assistant output.
func noInitScript(_, prompt string) []agent.Event {
	return []agent.Event{
		agent.AssistantTextEvent{Text: "acknowledged: " + prompt},
		agent.ResultEvent{Success: true},
	}
}

var errFakeSpawn = errors.New("fake: binary not found")

// echoPrompt is the subject glue the fakes honor: the default script echoes
// whatever prompt it is handed.
func echoPrompt(nonce string) string {
	return "reply with this token verbatim and then wait: " + nonce
}

// conformantSubject is the baseline subject every check should pass against.
func conformantSubject(cfg fakeConfig) Subject {
	return Subject{
		Provider:     newFake(cfg),
		EchoPrompt:   echoPrompt,
		ProbeTimeout: 5 * time.Second,
	}
}
