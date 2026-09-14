package pi

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func testReceiptAdmission(t *testing.T) (*receiptAdmission, string, string) {
	t.Helper()
	rootPath := filepath.Join(t.TempDir(), "artifact")
	if err := os.Mkdir(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	binaryPath := filepath.Join(rootPath, "pi")
	sidecarPath := filepath.Join(rootPath, artifactSidecarName)
	if err := os.WriteFile(binaryPath, []byte("test binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sidecarPath, []byte("test sidecar"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	rootInfo, _ := root.Stat(".")
	binaryInfo, binaryDigest, err := readMeasuredRootFile(root, artifactProfileFile{Mode: "0600", Path: "pi", SHA256: mustTestSHA(t, binaryPath), Size: 11}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sideInfo, sideDigest, err := readMeasuredRootFile(root, artifactProfileFile{Mode: "0600", Path: artifactSidecarName, SHA256: mustTestSHA(t, sidecarPath), Size: 12}, nil)
	if err != nil {
		t.Fatal(err)
	}
	artifact := &artifactLease{
		root: root, rootPath: rootPath, rootInfo: rootInfo,
		sidecar: artifactFileLease{entry: artifactProfileFile{Mode: "0600", Path: artifactSidecarName, SHA256: sideDigest, Size: 12}, info: sideInfo},
		files:   []artifactFileLease{{entry: artifactProfileFile{Mode: "0600", Path: "pi", SHA256: binaryDigest, Size: 11}, info: binaryInfo}},
	}
	extensionPath := filepath.Join(t.TempDir(), "donmai-policy.ts")
	if err := os.WriteFile(extensionPath, extensionSource(), 0o600); err != nil {
		t.Fatal(err)
	}
	extension, err := measureExtensionFile("donmai-policy", extensionPath, extensionSHA())
	if err != nil {
		t.Fatal(err)
	}
	startup := measureReceiptStartupContext(t.TempDir(), []string{"PATH=/usr/bin"})
	if startup == nil {
		t.Fatal("safe test startup context was not admitted")
	}
	return &receiptAdmission{artifact: artifact, extensions: []extensionFileLease{extension}, startup: startup}, binaryPath, extensionPath
}

func mustTestSHA(t *testing.T, path string) string {
	t.Helper()
	digest, err := sha256File(path)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func refusalEnd(origin string) rawEvent {
	fields := map[string]any{
		"type":       "tool_execution_end",
		"toolCallId": "call-1",
		"toolName":   "write",
		"isError":    true,
		"result": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "rejected before execution"}},
			"details": map[string]any{},
		},
		"preExecutionRefusal": map[string]any{
			"schemaVersion": float64(1),
			"origin":        origin,
			"toolCallId":    "call-1",
			"toolName":      "write",
		},
	}
	return rawEvent{Type: "tool_execution_end", Fields: fields}
}

// bufferedEvents reads everything already queued on the handle without
// blocking. dispatch is synchronous and emit is non-blocking, so by the time
// dispatch returns every event it produced is queued.
func bufferedEvents(h *Handle) []agent.Event {
	var out []agent.Event
	for {
		select {
		case ev := <-h.Events():
			out = append(out, ev)
		default:
			return out
		}
	}
}

// TestDispatchPreExecutionRefusalClosedClassifier pins the classifier that
// decides whether a tool_execution_end carries a VERIFIED SDK pre-execution
// refusal. Its discrimination is unchanged — only the two accepted shapes are
// accepted, and every tampered, forged, drifted or wrong-mode variant is
// rejected. What a rejection MEANS changed: an unverified claim is a call the
// boundary cannot prove, so it is recorded as a miss and surfaced non-fatally
// rather than ending the session (doc.go "The fail-safe fence"). The refusal's
// own recoverable ToolResultEvent reaches the caller either way.
func TestDispatchPreExecutionRefusalClosedClassifier(t *testing.T) {
	tests := []struct {
		name string
		// mutate tampers with the admission, the event, or the measured files.
		mutate func(*testing.T, *receiptAdmission, *rawEvent, string, string)
		// wantMiss is true when the claim must be rejected and the call
		// recorded as unproven.
		wantMiss bool
		// wantCallID is the id the recorded miss must name.
		wantCallID string
	}{
		{name: "invalid arguments"},
		{name: "unknown tool", mutate: func(_ *testing.T, _ *receiptAdmission, ev *rawEvent, _, _ string) {
			ev.Fields["toolName"] = "not-a-tool"
			ev.Fields["preExecutionRefusal"].(map[string]any)["toolName"] = "not-a-tool"
			ev.Fields["preExecutionRefusal"].(map[string]any)["origin"] = "unknown_tool"
		}},
		{name: "missing profile", mutate: func(_ *testing.T, admission *receiptAdmission, _ *rawEvent, _, _ string) {
			*admission = receiptAdmission{}
		}, wantMiss: true, wantCallID: "call-1"},
		{name: "wrong schema", mutate: func(_ *testing.T, _ *receiptAdmission, ev *rawEvent, _, _ string) {
			ev.Fields["preExecutionRefusal"].(map[string]any)["schemaVersion"] = float64(2)
		}, wantMiss: true, wantCallID: "call-1"},
		{name: "wrong origin", mutate: func(_ *testing.T, _ *receiptAdmission, ev *rawEvent, _, _ string) {
			ev.Fields["preExecutionRefusal"].(map[string]any)["origin"] = "execution_error"
		}, wantMiss: true, wantCallID: "call-1"},
		{name: "extra receipt member", mutate: func(_ *testing.T, _ *receiptAdmission, ev *rawEvent, _, _ string) {
			ev.Fields["preExecutionRefusal"].(map[string]any)["extra"] = true
		}, wantMiss: true, wantCallID: "call-1"},
		{name: "outer id mismatch", mutate: func(_ *testing.T, _ *receiptAdmission, ev *rawEvent, _, _ string) {
			ev.Fields["toolCallId"] = "other"
		}, wantMiss: true, wantCallID: "other"},
		{name: "inner name mismatch", mutate: func(_ *testing.T, _ *receiptAdmission, ev *rawEvent, _, _ string) {
			ev.Fields["preExecutionRefusal"].(map[string]any)["toolName"] = "bash"
		}, wantMiss: true, wantCallID: "call-1"},
		{name: "not error", mutate: func(_ *testing.T, _ *receiptAdmission, ev *rawEvent, _, _ string) {
			ev.Fields["isError"] = false
		}, wantMiss: true, wantCallID: "call-1"},
		{name: "forged details", mutate: func(_ *testing.T, _ *receiptAdmission, ev *rawEvent, _, _ string) {
			delete(ev.Fields, "preExecutionRefusal")
			ev.Fields["result"].(map[string]any)["details"] = map[string]any{"preExecutionRefusal": map[string]any{"schemaVersion": 1}}
		}, wantMiss: true, wantCallID: "call-1"},
		{name: "artifact drift", mutate: func(t *testing.T, _ *receiptAdmission, _ *rawEvent, binary, _ string) {
			f, err := os.OpenFile(binary, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = f.WriteString("drift")
			_ = f.Close()
		}, wantMiss: true, wantCallID: "call-1"},
		{name: "extension drift", mutate: func(t *testing.T, _ *receiptAdmission, _ *rawEvent, _, extension string) {
			f, err := os.OpenFile(extension, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = f.WriteString("drift")
			_ = f.Close()
		}, wantMiss: true, wantCallID: "call-1"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			admission, binary, extension := testReceiptAdmission(t)
			ev := refusalEnd("invalid_arguments")
			if test.mutate != nil {
				test.mutate(t, admission, &ev, binary, extension)
			}
			h := newHandle(nil, nil, agent.Spec{Autonomous: true}, "", admission)
			if fatal := h.dispatch(ev); fatal {
				t.Fatalf("an unproven refusal claim ended the session; only a denial the runtime executed anyway may")
			}

			events := bufferedEvents(h)
			var misses []agent.ErrorEvent
			var results []agent.ToolResultEvent
			for _, e := range events {
				switch value := e.(type) {
				case agent.ErrorEvent:
					if value.Code != adjudicationMissingCode {
						t.Fatalf("unexpected error event: %+v", value)
					}
					misses = append(misses, value)
				case agent.ToolResultEvent:
					results = append(results, value)
				}
			}
			// The refusal's own recoverable result still reaches the caller,
			// carrying the event's identity and error flag verbatim.
			wantErr, _ := ev.Fields["isError"].(bool)
			if len(results) != 1 || results[0].IsError != wantErr || results[0].ToolUseID != toolCallID(ev.Fields) {
				t.Fatalf("recoverable result lost error/identity: %+v", results)
			}
			wantMisses := 0
			if test.wantMiss {
				wantMisses = 1
			}
			if len(misses) != wantMisses {
				t.Fatalf("%s events = %d, want %d: %+v", adjudicationMissingCode, len(misses), wantMisses, misses)
			}
			recorded := h.adjudicationMisses()
			if len(recorded) != wantMisses {
				t.Fatalf("recorded misses = %+v, want %d", recorded, wantMisses)
			}
			if test.wantMiss && recorded[0].callID != test.wantCallID {
				t.Errorf("recorded miss call id = %q, want %q", recorded[0].callID, test.wantCallID)
			}
			if h.wasAdjudicated("call-1") {
				t.Fatal("pre-execution refusal was falsely recorded as an adjudication outcome")
			}
		})
	}
}

// TestDispatchPreExecutionRefusalInteractiveStaysUnproven keeps the mode half
// of the classifier: the receipt is only honoured for an autonomous session,
// so the same claim on an interactive one is rejected — and, like every other
// rejection, recorded rather than fatal.
func TestDispatchPreExecutionRefusalInteractiveStaysUnproven(t *testing.T) {
	admission, _, _ := testReceiptAdmission(t)
	h := newHandle(nil, nil, agent.Spec{Autonomous: false}, "", admission)
	if h.dispatch(refusalEnd("invalid_arguments")) {
		t.Fatal("an unproven refusal claim ended the session")
	}
	misses := h.adjudicationMisses()
	if len(misses) != 1 || misses[0].callID != "call-1" {
		t.Fatalf("non-autonomous receipt claim was not recorded as unproven: %+v", misses)
	}
}

func TestReceiptAdmissionRequiresExactOrderedMaterializedExtensionClosure(t *testing.T) {
	base, _, _ := testReceiptAdmission(t)
	layout := newSessionLayout(t.TempDir())
	if err := os.MkdirAll(layout.root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.extension, extensionSource(), 0o600); err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(t.TempDir(), "one.ts"), filepath.Join(t.TempDir(), "two.ts")}
	for i, path := range paths {
		if err := os.WriteFile(path, []byte{byte('1' + i)}, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	actual := []agentExtensionIdentity{
		{id: "one", digest: mustTestSHA(t, paths[0]), path: paths[0]},
		{id: "two", digest: mustTestSHA(t, paths[1]), path: paths[1]},
	}
	trusted := []TrustedExtensionIdentity{{ID: "one", Digest: actual[0].digest}, {ID: "two", Digest: actual[1].digest}}
	admission, err := newReceiptAdmission(base.artifact, layout, actual, trusted, base.startup)
	if err != nil || admission == nil {
		t.Fatalf("exact extension closure admission = %v, %v", admission, err)
	}
	if len(admission.extensions) != 3 {
		t.Fatalf("measured extension closure length=%d, want policy + two additions", len(admission.extensions))
	}

	for _, test := range []struct {
		name    string
		trusted []TrustedExtensionIdentity
	}{
		{name: "missing", trusted: trusted[:1]},
		{name: "reordered", trusted: []TrustedExtensionIdentity{trusted[1], trusted[0]}},
		{name: "changed digest", trusted: []TrustedExtensionIdentity{{ID: "one", Digest: actual[1].digest}, trusted[1]}},
		{name: "untrusted id", trusted: []TrustedExtensionIdentity{{ID: "other", Digest: actual[0].digest}, trusted[1]}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := newReceiptAdmission(base.artifact, layout, actual, test.trusted, base.startup)
			if err != nil {
				t.Fatalf("mismatch should retain legacy session behavior, got error: %v", err)
			}
			if got != nil {
				t.Fatal("mismatched extension closure gained receipt admission")
			}
		})
	}

	if err := os.WriteFile(paths[0], []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newReceiptAdmission(base.artifact, layout, actual, trusted, base.startup); err == nil {
		t.Fatal("materialized extension byte drift retained receipt admission")
	}
}

// TestExecutedThenErroredToolWithoutSDKRefusalIsRecordedUnproven pins that a
// tool which ran, had effects, and then errored is never EXCUSED by a forged
// origin buried in its result details: the claim is rejected and the call is
// recorded as unproven. It is not fatal, because "no record" alone cannot
// demonstrate a bypass — the boundary reached no ruling here to violate. The
// fatal case is a recorded denial the runtime executed anyway
// (policy_fence_test.go, "denied call executed successfully anyway").
func TestExecutedThenErroredToolWithoutSDKRefusalIsRecordedUnproven(t *testing.T) {
	admission, _, _ := testReceiptAdmission(t)
	marker := filepath.Join(t.TempDir(), "effect.txt")
	if err := os.WriteFile(marker, []byte("effect happened"), 0o600); err != nil {
		t.Fatal(err)
	}
	ev := refusalEnd("invalid_arguments")
	delete(ev.Fields, "preExecutionRefusal")
	ev.Fields["result"] = map[string]any{
		"content": []any{map[string]any{"type": "text", "text": "tool threw after effect"}},
		"details": map[string]any{"forgedOrigin": "invalid_arguments"},
	}
	h := newHandle(nil, nil, agent.Spec{Autonomous: true}, "", admission)
	if h.dispatch(ev) {
		t.Fatal("a post-effect tool error with no recorded ruling ended the session")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("test did not establish the prior filesystem effect: %v", err)
	}
	misses := h.adjudicationMisses()
	if len(misses) != 1 || misses[0].callID != "call-1" {
		t.Fatalf("forged-origin post-effect error was not recorded as unproven: %+v", misses)
	}
	if h.wasAdjudicated("call-1") {
		t.Fatal("post-effect error was falsely recorded as an adjudication outcome")
	}
}
