package afcli_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afcli"
)

// externalHandleMap is the downstream handle hook used through the public
// afcli surface only. It proves an external module can name and supply the
// contract without importing any internal package.
type externalHandleMap map[string]string

func (m externalHandleMap) Handle(sessionID string) string { return m[sessionID] }

// The public alias must name the same contract the command reads.
var _ afcli.HostWatchHandleEnricher = externalHandleMap{}

// TestHostWatchExternalHandleEnricherEndToEnd drives the real `watch`
// command through the public afcli surface only: the handle travels on a
// context built with afcli.WithHostWatchHandleEnricher and the piped plain
// output names the session by its handle. Deleting the afcli export breaks
// this file's compilation.
func TestHostWatchExternalHandleEnricherEndToEnd(t *testing.T) {
	if os.Getenv("DONMAI_TEST_EXTERNAL_WATCH_CHILD") == "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()
		ctx = afcli.WithHostWatchHandleEnricher(ctx, externalHandleMap{
			"watch-fixture-session-1234": "fixture-handle",
		})
		cmd := afcli.NewHostWatchCmd()
		cmd.SetArgs([]string{"--all", "--daemon-url", os.Getenv("DONMAI_TEST_EXTERNAL_WATCH_URL"), "--plain"})
		cmd.SetOut(os.Stdout)
		cmd.SetErr(os.Stderr)
		if err := cmd.ExecuteContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("watch should run until bounded context cancellation, got %v", err)
		}
		return
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/daemon/sessions":
			_, _ = w.Write([]byte(`[{"sessionId":"watch-fixture-session-1234","pid":42,"state":"running","projectName":"private-watch-project","repository":"https://github.com/example/watch-fixture-repo.git","harness":"claude-code","model":"fixture-model","workType":"development","issueIdentifier":"FIX-77"}]`))
		case "/api/daemon/status":
			_, _ = w.Write([]byte(`{"status":"running","version":"9.9.9-fixture","maxSessions":2,"uptimeSeconds":90}`))
		case "/api/daemon/stats":
			_, _ = w.Write([]byte(`{"queueDepth":1}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	root := t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	output, err := os.CreateTemp(root, "watch-external-output-")
	if err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(os.DevNull)
	if err != nil {
		_ = output.Close()
		t.Fatal(err)
	}
	env := []string{
		"HOME=" + root, "TMPDIR=" + root, "PATH=" + os.Getenv("PATH"),
		"TERM=dumb", "NO_COLOR=1", "GORACE=atexit_sleep_ms=0",
		"DONMAI_TEST_EXTERNAL_WATCH_CHILD=1",
		"DONMAI_TEST_EXTERNAL_WATCH_URL=" + server.URL,
	}
	process, err := os.StartProcess(binary, []string{binary, "-test.run=^TestHostWatchExternalHandleEnricherEndToEnd$"}, &os.ProcAttr{
		Dir: root, Env: env, Files: []*os.File{input, output, output},
	})
	_ = input.Close()
	_ = output.Close()
	if err != nil {
		t.Fatalf("start owned watch child: %v", err)
	}
	state, err := process.Wait()
	raw, readErr := os.ReadFile(output.Name())
	if readErr != nil {
		t.Fatal(readErr)
	}
	if err != nil || !state.Success() {
		t.Fatalf("owned watch child failed: state=%v err=%v output=%s", state, err, raw)
	}
	text := string(raw)
	if strings.Contains(text, "\x1b") {
		t.Errorf("piped plain output carries terminal escapes: %q", text)
	}
	if !strings.Contains(text, "fixture-handle") {
		t.Errorf("externally enriched watch lost fixture-handle: %q", text)
	}
	if strings.Contains(text, "FIX-77") {
		t.Errorf("externally enriched watch still shows the issue identifier where the handle belongs: %q", text)
	}
}
