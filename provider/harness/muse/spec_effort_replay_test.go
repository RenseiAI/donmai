package muse

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// strictReasoningEffortVocabulary is the pinned closed vocabulary the
// host accepts on turn/start (msp.schema.json ReasoningEffort enum).
// The replay tests below assert the recorded wire params carry exactly
// this set verbatim at the Spawn boundary — a serialization regression
// that omits or remaps a tier fails the assertion, mirroring how the
// live host would refuse an unknown variant.
var strictReasoningEffortVocabulary = map[string]struct{}{
	"none": {}, "minimal": {}, "low": {}, "medium": {},
	"high": {}, "xhigh": {}, "max": {}, "ultra": {},
}

// TestValidateEffort_AcceptsClosedVocabulary pins the admission rule:
// empty (unset) and every closed-vocabulary tier pass so explicit
// caller intent is preserved verbatim.
func TestValidateEffort_AcceptsClosedVocabulary(t *testing.T) {
	for _, effort := range []agent.EffortLevel{
		"", "minimal", "low", "medium", "high", "xhigh", "max", "none", "ultra",
	} {
		// agent.EffortLevel constants cover low..max; minimal/none/ultra
		// arrive by wire spelling.
		if err := validateEffort(agent.Spec{Effort: effort}); err != nil {
			t.Errorf("effort %q rejected: %v, want accepted", effort, err)
		}
	}
}

// TestValidateEffort_RejectsUnsupportedExplicit pins the loud refusal:
// an explicit but unsupported tier (including the "default" sentinel,
// capitalised variants and unknown names) fails at admission rather
// than silently running the turn at the host default.
//
// RED control: replace the refusal with silent omission (return nil)
// and this test goes RED — proving the refusal, not the omission, is
// what runs.
func TestValidateEffort_RejectsUnsupportedExplicit(t *testing.T) {
	for _, effort := range []agent.EffortLevel{"default", "bogus", "Default", "NONE", "ULTRA", "auto"} {
		err := validateEffort(agent.Spec{Effort: effort})
		if err == nil {
			t.Errorf("effort %q accepted, want explicit rejection", effort)
			continue
		}
		var specErr *SpecEffortError
		if !errors.As(err, &specErr) {
			t.Errorf("effort %q err=%T (%v), want *SpecEffortError", effort, err, err)
		}
	}
	// Whitespace-padded known tiers are not trimmed into acceptance:
	// the wire is case- and space-sensitive, so " high" is unsupported.
	if err := validateEffort(agent.Spec{Effort: " high"}); err == nil {
		t.Error(`effort " high" accepted, want explicit rejection`)
	}
}

// TestSpawn_EffortUltra_PreservedThroughStrictHost replays the
// workflow-derived top-tier spec through the production Spawn boundary
// against the fake host and asserts the wire carries reasoningEffort
// ultra verbatim.
//
// Before the correction resolveEffort omitted ultra (fell through to
// the unset default), so the recorded turn/start params carried no
// reasoningEffort and this assertion failed; after the correction the
// params carry ultra and the live host accepts it. RED-on-revert:
// restore precisely the offending serializer line (drop the ultra arm
// from resolveEffort) and this test goes RED again.
func TestSpawn_EffortUltra_PreservedThroughStrictHost(t *testing.T) {
	p, fh := newTestProvider(t, "single-turn")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{Prompt: "do work", Cwd: t.TempDir(), Autonomous: true, Effort: agent.EffortLevel("ultra")})
	if err != nil {
		t.Fatalf("Spawn with ultra effort: %v", err)
	}
	events := drainEvents(t, h.Events(), 10*time.Second)
	if res, ok := terminalResult(events); !ok || !res.Success {
		t.Fatalf("terminal result missing or failed: %+v", res)
	}

	fh.mu.Lock()
	methods := append([]string(nil), fh.methods...)
	params := append([]map[string]any(nil), fh.params...)
	fh.mu.Unlock()
	found := false
	for i, m := range methods {
		if m != "turn/start" {
			continue
		}
		found = true
		got, _ := params[i]["reasoningEffort"].(string)
		if got != string(ReasoningEffortUltra) {
			t.Errorf("turn/start reasoningEffort=%q, want %q (explicit ultra intent must ride the wire)", got, ReasoningEffortUltra)
		}
		if _, ok := strictReasoningEffortVocabulary[got]; !ok {
			t.Errorf("turn/start reasoningEffort=%q is outside the pinned closed vocabulary", got)
		}
	}
	if !found {
		t.Fatal("host never saw turn/start")
	}
}

// TestSpawn_EffortNone_PreservedThroughStrictHost is the no-reasoning
// companion: an explicit none must ride the wire as none (ask for no
// reasoning), never be omitted into the host default.
func TestSpawn_EffortNone_PreservedThroughStrictHost(t *testing.T) {
	p, fh := newTestProvider(t, "single-turn")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{Prompt: "do work", Cwd: t.TempDir(), Autonomous: true, Effort: agent.EffortLevel("none")})
	if err != nil {
		t.Fatalf("Spawn with none effort: %v", err)
	}
	events := drainEvents(t, h.Events(), 10*time.Second)
	if res, ok := terminalResult(events); !ok || !res.Success {
		t.Fatalf("terminal result missing or failed: %+v", res)
	}

	fh.mu.Lock()
	methods := append([]string(nil), fh.methods...)
	params := append([]map[string]any(nil), fh.params...)
	fh.mu.Unlock()
	for i, m := range methods {
		if m != "turn/start" {
			continue
		}
		if got, _ := params[i]["reasoningEffort"].(string); got != string(ReasoningEffortNone) {
			t.Errorf("turn/start reasoningEffort=%q, want %q", got, ReasoningEffortNone)
		}
	}
}

// TestSpawn_ControlUnset_OmittedAndResumeValid pins the control lane:
// an unset effort omits reasoningEffort (host default applies) and the
// equivalent Resume path stays valid.
func TestSpawn_ControlUnset_OmittedAndResumeValid(t *testing.T) {
	p, fh := newTestProvider(t, "single-turn")
	fh.sessionID = "22222222-2222-7222-8222-222222222222"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{Prompt: "do work", Cwd: t.TempDir(), Autonomous: true})
	if err != nil {
		t.Fatalf("Spawn control: %v", err)
	}
	events := drainEvents(t, h.Events(), 10*time.Second)
	if res, ok := terminalResult(events); !ok || !res.Success {
		t.Fatalf("control terminal result missing or failed: %+v", res)
	}

	fh.mu.Lock()
	methods := append([]string(nil), fh.methods...)
	params := append([]map[string]any(nil), fh.params...)
	fh.mu.Unlock()
	for i, m := range methods {
		if m != "turn/start" {
			continue
		}
		if _, present := params[i]["reasoningEffort"]; present {
			t.Errorf("control turn/start reasoningEffort=%v, want omitted (host default)", params[i]["reasoningEffort"])
		}
	}

	// The equivalent Resume carries no effort either and must not call
	// session/start.
	p2, fh2 := newTestProvider(t, "single-turn")
	fh2.sessionID = "22222222-2222-7222-8222-222222222222"
	h2, err := p2.Resume(ctx, "22222222-2222-7222-8222-222222222222", agent.Spec{Prompt: "continue", Cwd: t.TempDir(), Autonomous: true})
	if err != nil {
		t.Fatalf("Resume control: %v", err)
	}
	events2 := drainEvents(t, h2.Events(), 10*time.Second)
	if res, ok := terminalResult(events2); !ok || !res.Success {
		t.Fatalf("resume terminal result missing or failed: %+v", res)
	}
	fh2.mu.Lock()
	methods2 := append([]string(nil), fh2.methods...)
	fh2.mu.Unlock()
	for _, m := range methods2 {
		if m == "session/start" {
			t.Errorf("resume path called session/start; methods=%v", methods2)
		}
	}
}

// TestSpawn_UnsupportedEffort_FailsExplicitly pins AC-2: an explicit
// but unsupported tier fails at the Spawn boundary with a typed
// refusal — never a silent omission and never a host 400.
func TestSpawn_UnsupportedEffort_FailsExplicitly(t *testing.T) {
	p, _ := newTestProvider(t, "single-turn")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, effort := range []agent.EffortLevel{"default", "bogus"} {
		_, err := p.Spawn(ctx, agent.Spec{Prompt: "do work", Cwd: t.TempDir(), Autonomous: true, Effort: effort})
		if err == nil {
			t.Errorf("Spawn with effort %q succeeded, want explicit refusal", effort)
			continue
		}
		var specErr *SpecEffortError
		if !errors.As(err, &specErr) {
			t.Errorf("Spawn with effort %q err=%v, want *SpecEffortError in the chain", effort, err)
		}
		if !errors.Is(err, agent.ErrSpawnFailed) {
			t.Errorf("Spawn with effort %q err=%v, want ErrSpawnFailed in the chain", effort, err)
		}
	}
}

// TestResume_UnsupportedEffort_FailsExplicitly pins the Resume door of
// AC-2: an explicit but unsupported tier is refused on Resume with the
// same typed refusal as Spawn, before any host process starts. Resume
// shares the spawnHandle admission path; without that shared check a
// refactor splitting Resume admission could silently re-admit bogus
// tiers on Resume.
//
// RED control: drop the validateEffort call in spawnHandle and this
// test goes RED (Resume proceeds to bring-up instead of refusing).
func TestResume_UnsupportedEffort_FailsExplicitly(t *testing.T) {
	p, _ := newTestProvider(t, "single-turn")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const resumeID = "22222222-2222-7222-8222-222222222222"
	for _, effort := range []agent.EffortLevel{"bogus", "default"} {
		_, err := p.Resume(ctx, resumeID, agent.Spec{Prompt: "continue", Cwd: t.TempDir(), Autonomous: true, Effort: effort})
		if err == nil {
			t.Errorf("Resume with effort %q succeeded, want explicit refusal", effort)
			continue
		}
		var specErr *SpecEffortError
		if !errors.As(err, &specErr) {
			t.Errorf("Resume with effort %q err=%v, want *SpecEffortError in the chain", effort, err)
		}
		if !errors.Is(err, agent.ErrSpawnFailed) {
			t.Errorf("Resume with effort %q err=%v, want ErrSpawnFailed in the chain", effort, err)
		}
	}
}

// TestSpawn_Interactive_UnsupportedEffort_FailsExplicitly pins the
// interactive spawn door's admission guard: an explicit but unsupported
// tier is refused by the production Spawn entry point with an
// interactive spec, before the login check and before any PTY child
// starts — with the typed refusal, never a silent run at the host
// default.
//
// RED control: drop the validateEffort call in spawnInteractive and
// this test goes RED (the spawn proceeds past admission into login /
// PTY bring-up instead of refusing with *SpecEffortError).
func TestSpawn_Interactive_UnsupportedEffort_FailsExplicitly(t *testing.T) {
	p := newFakeInteractiveProvider(t, `echo "tui up"`)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, effort := range []agent.EffortLevel{"bogus", "default"} {
		_, err := p.Spawn(ctx, agent.Spec{
			Prompt:      "hello",
			Cwd:         t.TempDir(),
			Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
			Effort:      effort,
		})
		if err == nil {
			t.Errorf("interactive Spawn with effort %q succeeded, want explicit refusal", effort)
			continue
		}
		var specErr *SpecEffortError
		if !errors.As(err, &specErr) {
			t.Errorf("interactive Spawn with effort %q err=%v, want *SpecEffortError in the chain", effort, err)
		}
		if !errors.Is(err, agent.ErrSpawnFailed) {
			t.Errorf("interactive Spawn with effort %q err=%v, want ErrSpawnFailed in the chain", effort, err)
		}
	}
}
