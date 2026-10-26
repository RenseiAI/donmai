package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/prompt"
)

// launchContextTestHandle is an interactive handle with a session sidecar
// path, as a production claude handle reports it.
type launchContextTestHandle struct {
	*testInteractiveHandle
	sidecar string
}

func (h *launchContextTestHandle) ContextUsageSidecarPath() string       { return h.sidecar }
func (h *launchContextTestHandle) ContextUsageRollout() (string, string) { return "", "" }

// stageInteractiveHandle builds a live handle with a sidecar in dir.
func stageInteractiveHandle(dir string) (*launchContextTestHandle, *recordingInteractiveSession) {
	sidecar := filepath.Join(dir, "statusline-sidecar.json")
	session := liveRecordingInteractiveSession()
	base := &fakeHandle{events: make(chan agent.Event, 1)}
	close(base.events)
	return &launchContextTestHandle{
		testInteractiveHandle: &testInteractiveHandle{Handle: base, session: session},
		sidecar:               sidecar,
	}, session
}

// TestDispatchInteractive_StagesLaunchContext drives the production
// interactive entry point: memory plus the seed prompt land beside the
// sidecar for the compact hook, while an empty context stages nothing.
func TestDispatchInteractive_StagesLaunchContext(t *testing.T) {
	r := minimalRunner(t)
	qw := QueuedWork{QueuedWork: prompt.QueuedWork{
		SessionID:     "launch-ctx-1",
		Mode:          prompt.InteractiveRunMode,
		MemoryBlock:   "recall: the deploy finished",
		InitialPrompt: "cut the release",
	}}
	dir := t.TempDir()
	handle, session := stageInteractiveHandle(dir)
	if err := os.WriteFile(handle.sidecar, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	res := &Result{SessionID: qw.SessionID}
	done := make(chan struct{})
	var got *Result
	go func() {
		defer close(done)
		got, _ = r.dispatchInteractive(context.Background(), handle, t.TempDir(), qw, res, noopSink{}, nil, nil, agent.NoticeDeliveryHook)
	}()
	// The file lands before the supervisor loop starts, so ending the
	// session first still leaves it staged when the dispatch is correct.
	close(session.done)
	<-done
	if got == nil || got.Status != "completed" {
		t.Fatalf("dispatch status = %+v, want completed", got)
	}
	body, err := os.ReadFile(filepath.Join(dir, agent.ClaudeLaunchContextFileName))
	if err != nil {
		t.Fatalf("launch context was not staged beside the sidecar: %v", err)
	}
	if !strings.Contains(string(body), "recall: the deploy finished") || !strings.Contains(string(body), "cut the release") {
		t.Fatalf("launch context = %q, want memory and seed prompt", body)
	}

	// An empty context stages nothing, through the same production helper
	// the dispatch calls.
	empty, _ := stageInteractiveHandle(t.TempDir())
	writeLaunchContextFile(empty, "", "", nil)
	if _, err := os.Stat(filepath.Join(filepath.Dir(empty.sidecar), agent.ClaudeLaunchContextFileName)); !os.IsNotExist(err) {
		t.Fatalf("empty context staged a file (stat err = %v)", err)
	}
}
