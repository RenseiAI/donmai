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
	return &receiptAdmission{artifact: artifact, extensions: []extensionFileLease{extension}}, binaryPath, extensionPath
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

func TestDispatchPreExecutionRefusalClosedClassifier(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*testing.T, *receiptAdmission, *rawEvent, string, string)
		wantFatal  bool
		wantResult bool
	}{
		{name: "invalid arguments", wantResult: true},
		{name: "unknown tool", mutate: func(_ *testing.T, _ *receiptAdmission, ev *rawEvent, _, _ string) {
			ev.Fields["toolName"] = "not-a-tool"
			ev.Fields["preExecutionRefusal"].(map[string]any)["toolName"] = "not-a-tool"
			ev.Fields["preExecutionRefusal"].(map[string]any)["origin"] = "unknown_tool"
		}, wantResult: true},
		{name: "missing profile", mutate: func(_ *testing.T, admission *receiptAdmission, _ *rawEvent, _, _ string) {
			*admission = receiptAdmission{}
		}, wantFatal: true},
		{name: "wrong schema", mutate: func(_ *testing.T, _ *receiptAdmission, ev *rawEvent, _, _ string) {
			ev.Fields["preExecutionRefusal"].(map[string]any)["schemaVersion"] = float64(2)
		}, wantFatal: true},
		{name: "wrong origin", mutate: func(_ *testing.T, _ *receiptAdmission, ev *rawEvent, _, _ string) {
			ev.Fields["preExecutionRefusal"].(map[string]any)["origin"] = "execution_error"
		}, wantFatal: true},
		{name: "extra receipt member", mutate: func(_ *testing.T, _ *receiptAdmission, ev *rawEvent, _, _ string) {
			ev.Fields["preExecutionRefusal"].(map[string]any)["extra"] = true
		}, wantFatal: true},
		{name: "outer id mismatch", mutate: func(_ *testing.T, _ *receiptAdmission, ev *rawEvent, _, _ string) { ev.Fields["toolCallId"] = "other" }, wantFatal: true},
		{name: "inner name mismatch", mutate: func(_ *testing.T, _ *receiptAdmission, ev *rawEvent, _, _ string) {
			ev.Fields["preExecutionRefusal"].(map[string]any)["toolName"] = "bash"
		}, wantFatal: true},
		{name: "not error", mutate: func(_ *testing.T, _ *receiptAdmission, ev *rawEvent, _, _ string) { ev.Fields["isError"] = false }, wantFatal: true},
		{name: "forged details", mutate: func(_ *testing.T, _ *receiptAdmission, ev *rawEvent, _, _ string) {
			delete(ev.Fields, "preExecutionRefusal")
			ev.Fields["result"].(map[string]any)["details"] = map[string]any{"preExecutionRefusal": map[string]any{"schemaVersion": 1}}
		}, wantFatal: true},
		{name: "artifact drift", mutate: func(t *testing.T, _ *receiptAdmission, _ *rawEvent, binary, _ string) {
			f, err := os.OpenFile(binary, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = f.WriteString("drift")
			_ = f.Close()
		}, wantFatal: true},
		{name: "extension drift", mutate: func(t *testing.T, _ *receiptAdmission, _ *rawEvent, _, extension string) {
			f, err := os.OpenFile(extension, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = f.WriteString("drift")
			_ = f.Close()
		}, wantFatal: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			admission, binary, extension := testReceiptAdmission(t)
			ev := refusalEnd("invalid_arguments")
			if test.mutate != nil {
				test.mutate(t, admission, &ev, binary, extension)
			}
			h := newHandle(nil, nil, agent.Spec{Autonomous: true}, "", admission)
			fatal := h.dispatch(ev)
			if fatal != test.wantFatal {
				t.Fatalf("dispatch fatal=%v, want %v", fatal, test.wantFatal)
			}
			got := <-h.Events()
			_, isResult := got.(agent.ToolResultEvent)
			if isResult != test.wantResult {
				t.Fatalf("event=%T, want ToolResultEvent=%v", got, test.wantResult)
			}
			if result, ok := got.(agent.ToolResultEvent); ok && (!result.IsError || result.ToolUseID != "call-1") {
				t.Fatalf("recoverable result lost error/identity: %+v", result)
			}
			if h.wasAdjudicated("call-1") {
				t.Fatal("pre-execution refusal was falsely marked adjudicated")
			}
		})
	}
}

func TestDispatchPreExecutionRefusalInteractiveStaysFatal(t *testing.T) {
	admission, _, _ := testReceiptAdmission(t)
	h := newHandle(nil, nil, agent.Spec{Autonomous: false}, "", admission)
	if !h.dispatch(refusalEnd("invalid_arguments")) {
		t.Fatal("non-autonomous receipt bypassed the fatal policy monitor")
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
	admission, err := newReceiptAdmission(base.artifact, layout, actual, trusted)
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
			got, err := newReceiptAdmission(base.artifact, layout, actual, test.trusted)
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
	if _, err := newReceiptAdmission(base.artifact, layout, actual, trusted); err == nil {
		t.Fatal("materialized extension byte drift retained receipt admission")
	}
}

func TestExecutedThenErroredToolRemainsFatalWithoutSDKRefusal(t *testing.T) {
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
	if !h.dispatch(ev) {
		t.Fatal("post-effect tool error without SDK refusal was not fatal")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("test did not establish the prior filesystem effect: %v", err)
	}
	if h.wasAdjudicated("call-1") {
		t.Fatal("post-effect error was falsely marked adjudicated")
	}
}
