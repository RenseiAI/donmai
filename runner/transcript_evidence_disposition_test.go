package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/stub"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

// This file pins who decides the workarea's fate when a failed session's
// transcript tail cannot be archived.
//
// Without a terminal lease, the runner's own teardown is the only deleter,
// and the archive outcome gates it in the same goroutine: a tail failure keeps
// the workarea, with no window for a concurrent delete.
//
// With a terminal lease, the session was completed when the lease was
// acquired and failed afterwards, while preparing the lease. The lease's
// disposition was durably fixed at acquisition and is executed by the lease
// reaper. An earlier revision rewrote that disposition to "retain" after the
// fact. That rewrite raced a reaper already releasing with the old
// disposition, and it was refused outright when the lease store's own failure
// was the reason the session failed. The archive no longer touches the lease.
// A tail failure there is reported as following the lease, never claimed as
// retained.

func evidenceWarnings(res *Result) []string {
	var out []string
	for _, w := range res.PostSessionWarnings {
		if strings.Contains(w, "transcript evidence unavailable") {
			out = append(out, w)
		}
	}
	return out
}

// TestTranscriptEvidence_UnleasedFailureKeepsWorkareaViaRun drives Runner.Run
// for a failed session without a lease whose tail read fails: the workarea
// survives the run and the warning says so.
func TestTranscriptEvidence_UnleasedFailureKeepsWorkareaViaRun(t *testing.T) {
	old := transcriptEvidenceReadTail
	transcriptEvidenceReadTail = func(*os.File, os.FileInfo, int64) ([]byte, error) {
		return nil, context.DeadlineExceeded
	}
	t.Cleanup(func() { transcriptEvidenceReadTail = old })
	res, _ := runTranscriptCrashSession(t, "{\"final\":true}\n", ".agent/transcript.jsonl")
	if res.Status != "failed" {
		t.Fatalf("Status = %q; want failed", res.Status)
	}
	if _, err := os.Stat(res.WorktreePath); err != nil {
		t.Fatalf("worktree stat err = %v; want the workarea kept after a tail failure", err)
	}
	got := evidenceWarnings(res)
	if len(got) != 1 || got[0] != "transcript evidence unavailable for file 0; workarea retained" {
		t.Fatalf("evidence warnings = %q; want exactly the retained warning", got)
	}
}

// TestTranscriptEvidence_LeasedFailureFollowsLeaseViaRun drives Runner.Run
// through the late lease failure the reviewers used: the lease is acquired,
// then receiver registration fails (leaving the lease store unready), and
// the session flips to failed. A readable tail is still archived; an
// unreadable one is reported without claiming the workarea was retained.
func TestTranscriptEvidence_LeasedFailureFollowsLeaseViaRun(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	for _, tc := range []struct {
		name     string
		plant    bool
		wantWarn []string
		wantTail bool
	}{
		{name: "readable-tail-archived", plant: true, wantTail: true},
		{name: "unreadable-tail-follows-lease", plant: false, wantWarn: []string{"transcript evidence unavailable for file 0; workarea follows its terminal lease"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const declared = "evidence/leased-failure-5b1d.jsonl"
			wtParent := t.TempDir()
			srv := mockPlatformServer(t)
			t.Cleanup(srv.Close)
			manager, err := worktree.NewManager(worktree.Options{ParentDir: wtParent})
			if err != nil {
				t.Fatalf("worktree.NewManager: %v", err)
			}
			// Leases still acquire; only receiver registration fails.
			receivers := filepath.Join(wtParent, ".terminal-receivers")
			if err := os.RemoveAll(receivers); err != nil {
				t.Fatalf("remove receiver dir: %v", err)
			}
			if err := os.WriteFile(receivers, []byte("blocked\n"), 0o600); err != nil {
				t.Fatalf("block receiver dir: %v", err)
			}
			poster, err := result.NewPoster(result.Options{
				PlatformURL: srv.URL, WorkerID: "worker-1", AuthToken: "tok",
				HTTPClient: srv.Client(), BaseDelay: 1,
			})
			if err != nil {
				t.Fatalf("result.NewPoster: %v", err)
			}
			baseStub, err := stub.New()
			if err != nil {
				t.Fatalf("stub.New: %v", err)
			}
			harness, ok := baseStub.(agent.HarnessProvider)
			if !ok {
				t.Fatal("stub provider is not a HarnessProvider")
			}
			provider := &lateLeasePlantingProvider{HarnessProvider: harness, handle: newOrderedEvidenceHandle(declared, "WORK_RESULT:passed", nil)}
			if tc.plant {
				provider.rel = declared
			}
			reg := NewRegistry()
			if err := reg.Register(provider); err != nil {
				t.Fatalf("Register: %v", err)
			}
			r, err := New(Options{
				Registry: reg, WorktreeManager: manager, Poster: poster,
				HTTPClient: srv.Client(), SkipBackstop: true, SkipSteering: true, SkipPostSession: true,
				MaxSessionDuration: -1,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			r.rescueDir = t.TempDir()
			qw := QueuedWork{
				QueuedWork: queuedWorkBase("LEASED-FAILURE"), WorkerID: "worker-1", AuthToken: "tok",
				PlatformURL: srv.URL, ResolvedProfile: ResolvedProfile{Provider: agent.ProviderStub},
				TerminalWorkareaLease: terminalLeaseRequest(),
			}
			qw.SessionID = "5b1d0c2e-7a44-4c1b-9e3f-2d6a8f0b1c55"
			qw.Repository = makeBareRepo(t)
			qw.WorkType = "research"
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			res, err := r.Run(ctx, qw)
			if res == nil || res.Status != "failed" || res.FailureMode != FailureTerminalWorkareaLease {
				t.Fatalf("Run = %+v err = %v; want failed with %q", res, err, FailureTerminalWorkareaLease)
			}
			if !strings.Contains(res.Error, "register terminal status receiver") {
				t.Fatalf("Error = %q; want the post-acquisition receiver failure", res.Error)
			}
			if got := evidenceWarnings(res); strings.Join(got, "|") != strings.Join(tc.wantWarn, "|") {
				t.Fatalf("evidence warnings = %q; want %q", got, tc.wantWarn)
			}
			tails := findEvidenceFiles(t, r.rescueDir)
			if tc.wantTail != (len(tails) == 1) {
				t.Fatalf("archived tails = %v; want tail archived = %v", tails, tc.wantTail)
			}
		})
	}
}

// TestTranscriptEvidence_AncestorSymlinkRefused pins the path rule for
// relative declarations: a directory symlink inside the agent-controlled
// workarea must not steer the archiver to a file outside it.
func TestTranscriptEvidence_AncestorSymlinkRefused(t *testing.T) {
	const outsideContent = "outside-file-content-7c21"
	clone, _, base := cloneForRescue(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "transcript.jsonl"), []byte("{\"msg\":\""+outsideContent+"\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(clone, ".agent")); err != nil {
		t.Fatal(err)
	}
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	res := &Result{}
	res.Status = "failed"
	res.WorktreePath = clone
	res.rescueTargets = []rescueTarget{{path: clone, base: base}}
	res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: ".agent/transcript.jsonl"}}
	if r.preserveUnpublishedWork(QueuedWork{QueuedWork: queuedWorkBase("ANCESTOR")}, res) {
		t.Fatal("preserve = true; want the symlinked directory refused and the workarea kept")
	}
	for _, f := range findEvidenceFiles(t, r.rescueDir) {
		if strings.Contains(readEvidence(t, f), outsideContent) {
			t.Fatalf("archive %s carries content read through a directory symlink", f)
		}
	}
}
