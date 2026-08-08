package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runtime/heartbeat"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

// minimalRunner returns a Runner wired to a no-op platform mock, an
// in-memory worktree manager, and the stub provider. Used by tests
// that exercise individual loop helpers (steering decisions, backstop)
// without spinning up a full Run.
func minimalRunner(t *testing.T) *Runner {
	t.Helper()
	srv := mockPlatformServer(t)
	t.Cleanup(srv.Close)

	wtParent := t.TempDir()
	wtm, err := worktree.NewManager(worktree.Options{ParentDir: wtParent})
	if err != nil {
		t.Fatalf("worktree.NewManager: %v", err)
	}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: srv.URL,
		WorkerID:    "test-worker",
		AuthToken:   "token",
		HTTPClient:  srv.Client(),
		BaseDelay:   1, // 1ns — effectively no sleep between retries
	})
	if err != nil {
		t.Fatalf("result.NewPoster: %v", err)
	}
	reg := NewRegistry()
	r, err := New(Options{
		Registry:        reg,
		WorktreeManager: wtm,
		Poster:          poster,
		HTTPClient:      srv.Client(),
		// MaxSessionDuration negative disables timeout; some tests
		// run uninterruptable behaviors (hang-then-timeout) and need
		// caller-controlled cancellation.
		MaxSessionDuration: -1,
		SkipBackstop:       true,
		SkipSteering:       true,
		SkipPostSession:    true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

// mockPlatformServer returns an httptest.Server that accepts every
// /api/sessions/<id>/{completion,status,lock-refresh} call and
// responds 200 OK with `{"refreshed":true}` (for lock-refresh). The
// returned URL is suitable for both result.Poster and heartbeat.Pulser.
func mockPlatformServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// All endpoints accept POST and return JSON.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"refreshed":true,"ok":true}`))
	}))
	return srv
}

// recordingPlatformServer is a mockPlatformServer that additionally captures
// the sessionClass field observed on any /lock-refresh request body. It
// accepts every session endpoint (200 OK) so result.Poster + heartbeat.Pulser
// + activity/step-heartbeat posters all succeed. lastSessionClass reads the
// most recent stamp (empty until a lock-refresh with a sessionClass lands).
type recordingPlatformServer struct {
	*httptest.Server
	mu           sync.Mutex
	sessionClass string
	refreshes    int
	// pendingInjects are piggybacked onto successive successful
	// lock-refresh responses (one per refresh, in order) exactly the way the
	// platform delivers a runtime inject. Empty by default, so every existing
	// caller sees the unchanged {"refreshed":true,"ok":true} body.
	pendingInjects []heartbeat.InjectPayload
	// acks / deadLetters are the two terminal facts the worker reports back
	// about an inject. Recorded SEPARATELY on purpose: "delivered" and "never
	// delivered, here is why" are different answers, and a rail that cannot
	// tell them apart is how a message gets destroyed with a success reported.
	acks        []string
	deadLetters []struct {
		DeliveryID string `json:"deliveryId"`
		Reason     string `json:"reason"`
	}
}

// injectReports returns every ack and dead-letter the worker echoed.
func (s *recordingPlatformServer) injectReports() (acks []string, dead []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acks = append(acks, s.acks...)
	for _, d := range s.deadLetters {
		dead = append(dead, d.DeliveryID+":"+d.Reason)
	}
	return acks, dead
}

// queueInject arms the double to piggyback p onto the next lock-refresh.
func (s *recordingPlatformServer) queueInject(p heartbeat.InjectPayload) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pendingInjects = append(s.pendingInjects, p)
}

// takeInject pops the next queued inject, or nil when none is armed.
// Called with s.mu held.
func (s *recordingPlatformServer) takeInjectLocked() *heartbeat.InjectPayload {
	if len(s.pendingInjects) == 0 {
		return nil
	}
	next := s.pendingInjects[0]
	s.pendingInjects = s.pendingInjects[1:]
	return &next
}

// lastSessionClass returns the sessionClass most recently observed on a
// lock-refresh body, and the count of lock-refresh calls seen.
func (s *recordingPlatformServer) lastSessionClass() (string, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionClass, s.refreshes
}

func newRecordingPlatformServer(t *testing.T) *recordingPlatformServer {
	t.Helper()
	rec := &recordingPlatformServer{}
	rec.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/lock-refresh") {
			var body struct {
				SessionClass        string `json:"sessionClass"`
				AckedInject         string `json:"ackedInject"`
				DeadLetteredInjects []struct {
					DeliveryID string `json:"deliveryId"`
					Reason     string `json:"reason"`
				} `json:"deadLetteredInjects"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			rec.mu.Lock()
			rec.refreshes++
			if body.SessionClass != "" {
				rec.sessionClass = body.SessionClass
			}
			if body.AckedInject != "" {
				rec.acks = append(rec.acks, body.AckedInject)
			}
			rec.deadLetters = append(rec.deadLetters, body.DeadLetteredInjects...)
			inject := rec.takeInjectLocked()
			rec.mu.Unlock()

			if inject != nil {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(struct {
					Refreshed bool                     `json:"refreshed"`
					OK        bool                     `json:"ok"`
					Inject    *heartbeat.InjectPayload `json:"inject"`
				}{Refreshed: true, OK: true, Inject: inject})
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"refreshed":true,"ok":true}`))
	}))
	t.Cleanup(rec.Server.Close)
	return rec
}

// gitInit initialises a fresh git repo at dir with a single committed
// file so subsequent backstop / push operations have a base.
func gitInit(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
		{"config", "commit.gpgsign", "false"},
	} {
		//nolint:gosec // G204: test fixture, args are hard-coded literals.
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// Seed an initial commit on main so future commits aren't on a
	// detached-HEAD or empty branch.
	writeFile(t, dir, "README.md", "# test repo\n")
	for _, args := range [][]string{
		{"add", "README.md"},
		{"commit", "-m", "initial"},
	} {
		//nolint:gosec // G204: test fixture, args are hard-coded literals.
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// writeFile writes content to dir/relPath, creating parents as needed.
func writeFile(t *testing.T, dir, relPath, content string) {
	t.Helper()
	full := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// checkout creates+switches to the named branch.
func checkout(t *testing.T, dir, branch string) {
	t.Helper()
	//nolint:gosec // G204: test fixture, branch comes from test caller.
	cmd := exec.Command("git", "checkout", "-b", branch)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout -b %s: %v\n%s", branch, err, out)
	}
}

// queuedWorkBase returns a minimal but dispatchable QueuedWork for
// tests that don't care about the prompt / repository fields.
func queuedWorkBase(identifier string) prompt.QueuedWork {
	return prompt.QueuedWork{
		SessionID:       "test-session-" + identifier,
		IssueID:         "issue-uuid-" + identifier,
		IssueIdentifier: identifier,
		WorkType:        "development",
		ProjectName:     "TestProject",
		OrganizationID:  "org_test",
		Body:            "This is a test issue body.",
		Title:           "Test issue " + identifier,
	}
}

// agentResultWithPR returns an agent.Result with a PR URL for table
// tests that need a "completed" envelope.
func agentResultWithPR(prURL string) agent.Result {
	return agent.Result{
		Status:         "completed",
		PullRequestURL: prURL,
	}
}

// agentResultWithFailure returns an agent.Result classified as failed
// with the supplied FailureMode.
func agentResultWithFailure(mode string) agent.Result {
	return agent.Result{
		Status:      "failed",
		FailureMode: mode,
	}
}

// withCtx returns a cancellable context with a 30s deadline. Used so
// tests don't hang forever on a misbehaving fixture.
func withCtx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithCancel(context.Background())
}
