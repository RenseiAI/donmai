package pi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/internal/testexec"
	"github.com/RenseiAI/donmai/runtime/activity"
)

// appliedEventOf returns the first applied_reasoning_effort system event.
func appliedEventOf(events []agent.Event) (agent.SystemEvent, bool) {
	for _, ev := range events {
		if sys, ok := ev.(agent.SystemEvent); ok && sys.Subtype == agent.SystemSubtypeAppliedReasoningEffort {
			return sys, true
		}
	}
	return agent.SystemEvent{}, false
}

// kindsOf lists event kinds/subtypes for failure messages.
func kindsOfApplied(events []agent.Event) []string {
	var out []string
	for _, ev := range events {
		switch e := ev.(type) {
		case agent.SystemEvent:
			out = append(out, "system:"+e.Subtype)
		case agent.ResultEvent:
			out = append(out, "result")
		case agent.ErrorEvent:
			out = append(out, "error:"+e.Code)
		case agent.InitEvent:
			out = append(out, "init")
		default:
			out = append(out, "other")
		}
	}
	return out
}

// terminalAppliedResult returns the terminal ResultEvent, if any.
func terminalAppliedResult(events []agent.Event) (agent.ResultEvent, bool) {
	for _, ev := range events {
		if res, ok := ev.(agent.ResultEvent); ok {
			return res, true
		}
	}
	return agent.ResultEvent{}, false
}

// postAppliedThroughRealPoster drives one harness-emitted event through the
// real activity poster and returns the captured context pair and content.
func postAppliedThroughRealPoster(t *testing.T, ev agent.Event) (contextPair string, content string) {
	t.Helper()
	var gotContext, gotContent, gotType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/activity") {
			w.WriteHeader(http.StatusOK)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Activity struct {
				Type         string `json:"type"`
				Content      string `json:"content"`
				ContextKey   string `json:"contextKey"`
				ContextValue string `json:"contextValue"`
			} `json:"activity"`
		}
		if err := json.Unmarshal(raw, &body); err == nil && body.Activity.ContextKey != "" {
			gotContext = body.Activity.ContextKey + "=" + body.Activity.ContextValue
			gotContent = body.Activity.Content
			gotType = body.Activity.Type
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	p, err := activity.New(activity.Config{
		SessionID: "s1", WorkerID: "w1", BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })
	p.Send(context.Background(), ev)
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if gotType != "context" {
		t.Fatalf("poster type = %q, want context (payload=%q %q)", gotType, gotContext, gotContent)
	}
	return gotContext, gotContent
}

// appliedFakePi writes a fake `pi` binary that records the exact argv and the
// thinking-level pin it was exec'd with, then answers the headless RPC lane
// like a real child: handshake, get_state, and one complete turn on
// prompt (Spawn) or get_entries (Resume). --version and --list-models are
// answered like the real probe (the probe always reports the requested pair
// present, so a native pin never falls back in these tests).
func appliedFakePi(t *testing.T, dir string) string {
	t.Helper()
	argvPath := filepath.Join(dir, "argv")
	envPath := filepath.Join(dir, "child-env")
	handshake := strings.TrimSpace(handshakeEvent("h1"))
	getState := strings.TrimSpace(getStateResponse("applied-test-session"))
	turn := strings.TrimSpace(
		event(map[string]any{"type": "agent_start"}) +
			event(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_delta", "delta": "done"}}) +
			event(map[string]any{"type": "message_end"}) +
			event(map[string]any{"type": "turn_end", "message": map[string]any{"role": "assistant"}}) +
			event(map[string]any{"type": "agent_settled"}),
	)
	script := "#!/bin/bash\n" +
		"if [ \"$1\" = \"--version\" ]; then echo \"" + PinnedVersion + "\"; exit 0; fi\n" +
		"for a in \"$@\"; do if [ \"$a\" = \"--list-models\" ]; then echo \"provider model\"; echo \"anthropic claude-opus-4-8\"; echo \"zai glm-5.3\"; echo \"openai gpt-5.4\"; exit 0; fi; done\n" +
		"printf '%s\\n' \"$@\" > " + shQuote(argvPath) + "\n" +
		"{ printf 'THINKING=%s\\n' \"$DONMAI_PI_THINKING_LEVEL\"; printf 'MODEL=%s\\n' \"$DONMAI_PI_MODEL\"; } > " + shQuote(envPath) + "\n" +
		"cat <<'HANDSHAKE'\n" + handshake + "\nHANDSHAKE\n" +
		"while IFS= read -r command; do\n" +
		"  case \"$command\" in\n" +
		"    *'\"type\":\"get_state\"'*) echo '" + getState + "' ;;\n" +
		"    *'\"type\":\"prompt\"'*|*'\"type\":\"get_entries\"'*) cat <<'TRANSCRIPT'\n" + turn + "\nTRANSCRIPT\n" +
		"      ;;\n" +
		"    *'\"type\":\"abort\"'*) exit 0 ;;\n" +
		"  esac\n" +
		"done\n"
	bin := filepath.Join(dir, "fake-pi-applied.sh")
	if err := testexec.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func waitForAppliedArgv(t *testing.T, path string) string {
	t.Helper()
	return waitForArgvRecord(t, path, 20*time.Second)
}

// drainApplied reads until the first terminal event or close, like drain in
// handle_test.go but with a longer bound for real-process spawns.
func drainApplied(t *testing.T, h agent.Handle) []agent.Event {
	t.Helper()
	var out []agent.Event
	timeout := time.After(20 * time.Second)
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
			switch ev.(type) {
			case agent.ResultEvent, agent.ErrorEvent:
				return out
			}
		case <-timeout:
			t.Fatalf("timed out draining events; got %d so far: %v", len(out), kindsOfApplied(out))
		}
	}
}

// nativeHighSpec is a native-provider high configuration: the pin routes
// through pi's built-in provider, and the translated level rides the --model
// suffix.
func nativeHighSpec(cwd string) agent.Spec {
	return agent.Spec{Prompt: "do work", Cwd: cwd, Autonomous: true, Model: "anthropic/claude-opus-4-8", Effort: agent.EffortHigh}
}

// injectedHighSpec is an injected-provider high configuration: the pin routes
// through the adapter-registered provider, and the same translated level
// rides both the --model suffix and the extension pin.
func injectedHighSpec(cwd string) agent.Spec {
	return agent.Spec{
		Prompt: "do work", Cwd: cwd, Autonomous: true,
		Model: "injected-model", Effort: agent.EffortHigh,
		Endpoint: &agent.EndpointBinding{
			Company: agent.CompanyOpenAI, Host: agent.HostDirect,
			Protocol: agent.ProtoOpenAIChat, BaseURL: "https://proxy.example.com/v1",
			Model: "injected-model",
		},
	}
}

// TestAppliedEffort_SpawnHighCapturesTranslatedPin pins AC-1's Spawn half for
// both providers: a real headless Spawn execs the translated pin (final argv
// carries --model <id>:high and the env carries the same level), and the
// emitted event carries high on the new activity key through the real poster,
// ordered ahead of terminal completion.
func TestAppliedEffort_SpawnHighCapturesTranslatedPin(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec func(cwd string) agent.Spec
		want string
	}{
		{"native high", nativeHighSpec, "claude-opus-4-8:high"},
		{"injected high", injectedHighSpec, "injected-model:high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cwd := t.TempDir()
			bin := appliedFakePi(t, dir)
			p, err := New(Options{
				PiBin: bin, HandshakeTimeout: 10 * time.Second, handshakeToken: testHandshakeToken,
				VersionProbe: func(context.Context, string) (string, error) { return PinnedVersion, nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			h, err := p.Spawn(ctx, tc.spec(cwd))
			if err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			t.Cleanup(func() { _ = h.Stop(context.Background()) })

			argv := waitForAppliedArgv(t, filepath.Join(dir, "argv"))
			if !strings.Contains(argv, tc.want) {
				t.Fatalf("final argv lacks translated pin %q:\n%s", tc.want, argv)
			}
			envRaw, err := os.ReadFile(filepath.Join(dir, "child-env"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(envRaw), "THINKING=high") {
				t.Fatalf("extension pin lacks translated level high:\n%s", envRaw)
			}

			events := drainApplied(t, h)
			applied, ok := appliedEventOf(events)
			if !ok {
				t.Fatalf("no applied_reasoning_effort event; kinds=%v", kindsOfApplied(events))
			}
			if applied.Message != "high" {
				t.Errorf("applied Message = %q, want high", applied.Message)
			}
			if applied.Subtype == agent.SystemSubtypeReasoningEffort {
				t.Errorf("applied event uses the legacy requested key %q, want the new activity key", applied.Subtype)
			}
			appliedIdx, terminalIdx := -1, -1
			for i, ev := range events {
				if sys, ok := ev.(agent.SystemEvent); ok && sys.Subtype == agent.SystemSubtypeAppliedReasoningEffort && appliedIdx == -1 {
					appliedIdx = i
				}
				if _, ok := ev.(agent.ResultEvent); ok && terminalIdx == -1 {
					terminalIdx = i
				}
			}
			if appliedIdx == -1 || terminalIdx == -1 || appliedIdx > terminalIdx {
				t.Errorf("ordering applied=%d terminal=%d, want applied ahead of terminal; kinds=%v", appliedIdx, terminalIdx, kindsOfApplied(events))
			}
			if res, ok := terminalAppliedResult(events); !ok || !res.Success {
				t.Fatalf("terminal missing or failed: %+v", res)
			}
			gotCtx, gotContent := postAppliedThroughRealPoster(t, applied)
			if gotCtx != "appliedReasoningEffort=high" {
				t.Errorf("poster context = %q, want appliedReasoningEffort=high", gotCtx)
			}
			if gotContent != "applied reasoning effort: high (as applied)" {
				t.Errorf("poster content = %q, want the fixed applied sentence", gotContent)
			}
		})
	}
}

// TestAppliedEffort_ResumeHighCapturesTranslatedPin pins AC-1's Resume half:
// a resumed headless turn uses the same emission path as a new turn.
func TestAppliedEffort_ResumeHighCapturesTranslatedPin(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec func(cwd string) agent.Spec
		want string
	}{
		{"native high", nativeHighSpec, "claude-opus-4-8:high"},
		{"injected high", injectedHighSpec, "injected-model:high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cwd := t.TempDir()
			bin := appliedFakePi(t, dir)
			p, err := New(Options{
				PiBin: bin, HandshakeTimeout: 10 * time.Second, handshakeToken: testHandshakeToken,
				VersionProbe: func(context.Context, string) (string, error) { return PinnedVersion, nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			h, err := p.Resume(ctx, "resume-cursor-applied", tc.spec(cwd))
			if err != nil {
				t.Fatalf("Resume: %v", err)
			}
			t.Cleanup(func() { _ = h.Stop(context.Background()) })

			argv := waitForAppliedArgv(t, filepath.Join(dir, "argv"))
			if !strings.Contains(argv, "--session") || !strings.Contains(argv, "resume-cursor-applied") {
				t.Fatalf("resumed argv lacks --session cursor:\n%s", argv)
			}
			if !strings.Contains(argv, tc.want) {
				t.Fatalf("resumed argv lacks translated pin %q:\n%s", tc.want, argv)
			}
			events := drainApplied(t, h)
			applied, ok := appliedEventOf(events)
			if !ok {
				t.Fatalf("no applied_reasoning_effort on resume; kinds=%v", kindsOfApplied(events))
			}
			if applied.Message != "high" {
				t.Errorf("resumed applied Message = %q, want high", applied.Message)
			}
			gotCtx, _ := postAppliedThroughRealPoster(t, applied)
			if gotCtx != "appliedReasoningEffort=high" {
				t.Errorf("resume poster context = %q, want appliedReasoningEffort=high", gotCtx)
			}
			if res, ok := terminalAppliedResult(events); !ok || !res.Success {
				t.Fatalf("resumed terminal missing or failed: %+v", res)
			}
		})
	}
}

// TestAppliedEffort_MissingOrUnsupportedNeverReportsHigh pins AC-2: a missing
// model pin, an omitted effort, or an unsupported input reports
// not-configured (empty) rather than a guessed high. The RED pin builds the
// report directly from Spec.Effort: for a missing pin that raw value
// coincides with high while the translated report must stay empty.
func TestAppliedEffort_MissingOrUnsupportedNeverReportsHigh(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*agent.Spec)
	}{
		{"missing model pin", func(s *agent.Spec) { s.Model = "" }},
		{"omitted effort", func(s *agent.Spec) { s.Effort = "" }},
		{"unsupported ultra", func(s *agent.Spec) { s.Effort = "ultra" }},
		{"unsupported turbo", func(s *agent.Spec) { s.Effort = "turbo" }},
		{"sentinel default", func(s *agent.Spec) { s.Effort = "default" }},
		{"sentinel none", func(s *agent.Spec) { s.Effort = "none" }},
		{"unknown", func(s *agent.Spec) { s.Effort = "bogus-level" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := nativeHighSpec(t.TempDir())
			tc.mutate(&spec)
			if got := appliedEffortFromModelPin(spec); got != "" {
				t.Fatalf("translated applied = %q, want empty (not-configured)", got)
			}
			// RED: reporting raw Spec.Effort independent of the actual pin
			// claims high (or the raw input) just because the profile
			// contained it. The translated report must not coincide with it
			// when a pin is missing or unsupported.
			if direct := agent.AppliedReasoningEffortEvent(spec.Effort); spec.Effort != "" && direct.Message == string(spec.Effort) && appliedEffortFromModelPin(spec) == spec.Effort && spec.Effort == agent.EffortHigh {
				t.Errorf("direct-from-spec report coincides with translated report; translation bypass not pinned")
			}
			ev := appliedEffortEvent(spec)
			sys, ok := ev.(agent.SystemEvent)
			if !ok || sys.Subtype != agent.SystemSubtypeAppliedReasoningEffort {
				t.Fatalf("event = %#v, want the new activity key", ev)
			}
			if sys.Message != "" {
				t.Errorf("applied Message = %q, want empty", sys.Message)
			}
			gotCtx, gotContent := postAppliedThroughRealPoster(t, ev)
			if gotCtx != "appliedReasoningEffort=not-configured" {
				t.Errorf("poster context = %q, want appliedReasoningEffort=not-configured", gotCtx)
			}
			if !strings.Contains(gotContent, "not configured") {
				t.Errorf("poster content = %q, want the not-configured sentence", gotContent)
			}
		})
	}
}

// TestAppliedEffort_XHighMaxStayCoherent pins AC-2's second half: xhigh and
// max ride the pin unchanged (the extension's extended-level map lists each
// under its own name), and the applied report matches the pin exactly.
func TestAppliedEffort_XHighMaxStayCoherent(t *testing.T) {
	for _, level := range []agent.EffortLevel{agent.EffortXHigh, agent.EffortMax} {
		t.Run(string(level), func(t *testing.T) {
			spec := nativeHighSpec(t.TempDir())
			spec.Effort = level
			if got := appliedEffortFromModelPin(spec); got != level {
				t.Fatalf("translated applied = %q, want %q", got, level)
			}
			pin := providerPinEnv(spec)
			var thinking string
			for _, e := range pin {
				if v, ok := strings.CutPrefix(e, piThinkingLevelEnvVar+"="); ok {
					thinking = v
				}
			}
			if thinking != string(level) {
				t.Fatalf("extension pin thinking level = %q, want %q (must stay coherent with the model suffix)", thinking, level)
			}
			ev := appliedEffortEvent(spec)
			gotCtx, gotContent := postAppliedThroughRealPoster(t, ev)
			if gotCtx != "appliedReasoningEffort="+string(level) {
				t.Errorf("poster context = %q, want appliedReasoningEffort=%s", gotCtx, level)
			}
			if gotContent != "applied reasoning effort: "+string(level)+" (as applied)" {
				t.Errorf("poster content = %q, want the fixed applied sentence for %s", gotContent, level)
			}
		})
	}
}

// TestAppliedEffort_FailedLaunchEmitsNothing pins AC-2's failed-launch half:
// a launch that fails closed (tampered extension) returns an error and emits
// no successful high. The real fake answers the handshake with the wrong SHA.
func TestAppliedEffort_FailedLaunchEmitsNothing(t *testing.T) {
	dir := t.TempDir()
	badHandshake := strings.TrimSpace(uiRequest("h1", map[string]any{
		"donmai": handshakeKind, "token": testHandshakeToken, "sha": strings.Repeat("a", 64),
	}))
	script := "#!/bin/bash\n" +
		"if [ \"$1\" = \"--version\" ]; then echo \"" + PinnedVersion + "\"; exit 0; fi\n" +
		"for a in \"$@\"; do if [ \"$a\" = \"--list-models\" ]; then echo \"provider model\"; echo \"anthropic claude-opus-4-8\"; exit 0; fi; done\n" +
		"cat <<'HANDSHAKE'\n" + badHandshake + "\nHANDSHAKE\n" +
		"sleep 30\n"
	bin := filepath.Join(dir, "fake-pi-bad-handshake.sh")
	if err := testexec.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := New(Options{
		PiBin: bin, HandshakeTimeout: 5 * time.Second, handshakeToken: testHandshakeToken,
		VersionProbe: func(context.Context, string) (string, error) { return PinnedVersion, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, nativeHighSpec(t.TempDir()))
	if err == nil {
		_ = h.Stop(context.Background())
		t.Fatal("Spawn succeeded against a tampered extension, want fail-closed error")
	}
	if h != nil {
		t.Errorf("failed launch returned a handle; want nil so no event can be delivered")
	}
}

// TestAppliedEffort_LegacyAndResultUnchanged pins AC-3's first half: the
// legacy requested-effort event keeps its own key, and a successful headless
// launch still delivers its terminal result behind the notices.
func TestAppliedEffort_LegacyAndResultUnchanged(t *testing.T) {
	if agent.SystemSubtypeAppliedReasoningEffort == agent.SystemSubtypeReasoningEffort {
		t.Fatalf("applied key must differ from the legacy requested key %q", agent.SystemSubtypeReasoningEffort)
	}
	legacy := agent.ReasoningEffortEvent(agent.EffortHigh)
	gotCtx, _ := postAppliedThroughRealPoster(t, legacy)
	if gotCtx != "reasoningEffort=high" {
		t.Fatalf("legacy poster context = %q, want reasoningEffort=high (unchanged)", gotCtx)
	}
	dir := t.TempDir()
	bin := appliedFakePi(t, dir)
	p, err := New(Options{
		PiBin: bin, HandshakeTimeout: 10 * time.Second, handshakeToken: testHandshakeToken,
		VersionProbe: func(context.Context, string) (string, error) { return PinnedVersion, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, nativeHighSpec(t.TempDir()))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	events := drainApplied(t, h)
	if res, ok := terminalAppliedResult(events); !ok || !res.Success {
		t.Fatalf("headless result delivery changed: %+v", res)
	}
	var sawBounds bool
	for _, ev := range events {
		if sys, ok := ev.(agent.SystemEvent); ok && sys.Subtype == agent.SystemSubtypeToolCallBounds {
			sawBounds = true
		}
	}
	if !sawBounds {
		t.Errorf("tool-call-bounds notice missing; legacy launch notices must stay unchanged")
	}
}

// TestAppliedEffort_CancelledLaunchEmitsNothing pins AC-3's second half: a
// cancelled launch returns an error and delivers no applied event. RED:
// emitting under the old context key or bypassing cancellation.
func TestAppliedEffort_CancelledLaunchEmitsNothing(t *testing.T) {
	dir := t.TempDir()
	bin := appliedFakePi(t, dir)
	p, err := New(Options{
		PiBin: bin, HandshakeTimeout: 10 * time.Second, handshakeToken: testHandshakeToken,
		VersionProbe: func(context.Context, string) (string, error) { return PinnedVersion, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h, err := p.Spawn(ctx, nativeHighSpec(t.TempDir()))
	if err == nil {
		_ = h.Stop(context.Background())
		t.Fatal("Spawn succeeded on a cancelled context, want error")
	}
	if h != nil {
		t.Errorf("cancelled launch returned a handle; event delivery must not outlive cancellation")
	}
}

// TestAppliedEffort_CallSites pins the call-site enumeration: modelPinArgs
// feeds both the headless argv (rpcArgs) and the interactive argv
// (interactiveArgs), but only the headless launchNotices path emits the
// applied event — interactive callers remain unchanged and are never credited
// as covered by headless tests.
func TestAppliedEffort_CallSites(t *testing.T) {
	t.Parallel()
	spec := nativeHighSpec("/tmp/work")
	headless := rpcArgs(sessionLayout{root: "/tmp/session"}, []string{"/tmp/session/policy.ts"}, launchPrompt, "", spec)
	var headlessModel string
	for i, a := range headless {
		if a == "--model" && i+1 < len(headless) {
			headlessModel = headless[i+1]
		}
	}
	if headlessModel != "claude-opus-4-8:high" {
		t.Fatalf("headless rpcArgs --model = %q, want the translated pin claude-opus-4-8:high", headlessModel)
	}
	interactive := interactiveArgs(spec, sessionLayout{root: "/tmp/session"}, []string{"/tmp/session/policy.ts"})
	var interactiveModel string
	for i, a := range interactive {
		if a == "--model" && i+1 < len(interactive) {
			interactiveModel = interactive[i+1]
		}
	}
	if interactiveModel != "claude-opus-4-8:high" {
		t.Fatalf("interactive argv --model = %q, want the same shared pin (call site enumerated, behavior unchanged)", interactiveModel)
	}
	notices := (&Provider{}).launchNotices(spec)
	var sawApplied bool
	for _, n := range notices {
		if sys, ok := n.(agent.SystemEvent); ok && sys.Subtype == agent.SystemSubtypeAppliedReasoningEffort {
			sawApplied = true
			if sys.Message != "high" {
				t.Errorf("launchNotices applied Message = %q, want high", sys.Message)
			}
		}
	}
	if !sawApplied {
		t.Fatalf("launchNotices carries no applied event; headless call site must emit it")
	}
	// The interactive lane has no event seam: its handle never carries
	// launchNotices, so the shared pin above cannot be credited as applied
	// coverage for it. This test fails (RED) if launchNotices is ever wired
	// into the interactive spawn path.
}
