package runner

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/workarea"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

// TestReworkCounterObjectArrayRedactedWhole pins B1: an object or array
// under an exact counter key is redacted whole, not traversed. Deleting the
// numeric-only exemption must make these fail (outside bytes survive).
func TestReworkCounterObjectArrayRedactedWhole(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "counter-object", body: "{\"input_tokens\":{\"a\":\"b\"},\"msg\":\"clean\"}\n"},
		{name: "counter-array", body: "{\"input_tokens\":[1,2],\"msg\":\"clean\"}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clone, _, base := cloneForRescue(t)
			writeFile(t, clone, ".agent/transcript.jsonl", tc.body)
			r := minimalRunner(t)
			r.rescueDir = t.TempDir()
			res := &Result{}
			res.Status = "failed"
			res.WorktreePath = clone
			res.rescueTargets = []rescueTarget{{path: clone, base: base}}
			res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: ".agent/transcript.jsonl"}}
			qw := QueuedWork{QueuedWork: queuedWorkBase("REWORK-" + tc.name)}
			if !r.preserveUnpublishedWork(qw, res) {
				t.Fatalf("preserve = false (warnings %v)", res.PostSessionWarnings)
			}
			files := findEvidenceFiles(t, r.rescueDir)
			if len(files) != 1 {
				t.Fatalf("evidence files = %v; want one", files)
			}
			got := readEvidence(t, files[0])
			var m map[string]interface{}
			if err := json.Unmarshal([]byte(strings.TrimSuffix(got, "\n")), &m); err != nil {
				t.Fatalf("archived record invalid JSON %q: %v", got, err)
			}
			if m["input_tokens"] != "[REDACTED]" {
				t.Fatalf("%s: input_tokens = %v; want wholesale [REDACTED] in %q", tc.name, m["input_tokens"], got)
			}
		})
	}
}

// TestReworkNumericEquivalentRedacted pins B2: a transcript number written
// in scientific notation matches the same numeric session secret.
// Comparing by spelling alone must leave the equivalent behind.
func TestReworkNumericEquivalentRedacted(t *testing.T) {
	secret := "987654321"
	clone, _, base := cloneForRescue(t)
	writeFile(t, clone, ".agent/transcript.jsonl", "{\"pin\":9.87654321e8,\"msg\":\"clean\"}\n")
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	res := &Result{}
	res.Status = "failed"
	res.WorktreePath = clone
	res.rescueTargets = []rescueTarget{{path: clone, base: base}}
	res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: ".agent/transcript.jsonl"}}
	qw := QueuedWork{QueuedWork: queuedWorkBase("REWORK-NUMEQ")}
	qw.Env = map[string]string{"MY_PIN": secret}
	if !r.preserveUnpublishedWork(qw, res) {
		t.Fatalf("preserve = false (warnings %v)", res.PostSessionWarnings)
	}
	files := findEvidenceFiles(t, r.rescueDir)
	got := readEvidence(t, files[0])
	if strings.Contains(got, "987654321") || strings.Contains(got, "9.87654321e8") {
		t.Fatalf("archive retains numeric equivalent:\n%q", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("archive missing [REDACTED]:\n%q", got)
	}
}

// TestReworkNinthEmbeddedRedacted pins B3: embedded-JSON redaction has no
// 8-span cap. Nine embedded structured secrets (same JSON value with
// different spacing, so only the structural rail catches them) must all be
// redacted; stopping after eight must leave the ninth behind. A
// non-sensitive key is used so the shared output rail does not mask the
// candidates before the JSON-aware rail sees them.
func TestReworkNinthEmbeddedRedacted(t *testing.T) {
	secret := `{"mykey": "probe-structured-ninth-xyz"}`
	var parts []string
	for i := 0; i < 9; i++ {
		parts = append(parts, `{"mykey":"probe-structured-ninth-xyz"}`)
	}
	inner := strings.Join(parts, " ")
	outer, err := json.Marshal(map[string]string{"msg": "prefix " + inner + " suffix"})
	if err != nil {
		t.Fatal(err)
	}
	body := string(outer) + "\n"
	clone, _, base := cloneForRescue(t)
	writeFile(t, clone, ".agent/transcript.jsonl", body)
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	res := &Result{}
	res.Status = "failed"
	res.WorktreePath = clone
	res.rescueTargets = []rescueTarget{{path: clone, base: base}}
	res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: ".agent/transcript.jsonl"}}
	qw := QueuedWork{QueuedWork: queuedWorkBase("REWORK-NINTH")}
	qw.Env = map[string]string{"MY_JSON_CRED": secret}
	if !r.preserveUnpublishedWork(qw, res) {
		t.Fatalf("preserve = false (warnings %v)", res.PostSessionWarnings)
	}
	files := findEvidenceFiles(t, r.rescueDir)
	got := readEvidence(t, files[0])
	if strings.Contains(got, "probe-structured-ninth-xyz") {
		t.Fatalf("archive retains ninth-embedded structured secret:\n%q", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("archive missing [REDACTED]:\n%q", got)
	}
}

func acquireManagerLeaseForRetain(t *testing.T, manager *worktree.Manager) (*workarea.TerminalLease, string) {
	t.Helper()
	sessionID := "11111111-1111-4111-8111-111111111111"
	resultID := "tr_11111111111111111111111111111111"
	path, err := manager.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: sessionID, Strategy: worktree.StrategyEmpty,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := manager.AcquireTerminalLease(context.Background(), workarea.AcquireSpec{
		SessionID: sessionID, TerminalResultID: resultID, Policy: workarea.DefaultLeasePolicy(),
		ReleaseRequested: true, ReleaseDisposition: "destroy",
	})
	if err != nil {
		t.Fatal(err)
	}
	return lease, path
}

func statPath(path string) (os.FileInfo, error) {
	return os.Stat(path)
}

// TestReworkRetainSurvivesRestartExpiry pins B4 at the durable layer: a
// retained lease keeps its workarea across a manager restart and an expiry
// reap that would destroy a destroy-disposition lease.
func TestReworkRetainSurvivesRestartExpiry(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Millisecond)
	manager, err := worktree.NewManager(worktree.Options{ParentDir: dir, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	lease, path := acquireManagerLeaseForRetain(t, manager)
	if _, err := manager.RetainTerminalLease(context.Background(), lease.LeaseID, lease.SessionID, lease.TerminalResultID, lease.WorkareaID); err != nil {
		t.Fatalf("RetainTerminalLease: %v", err)
	}
	stored, err := manager.TerminalLease(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ReleaseDisposition != "retain" {
		t.Fatalf("disposition = %q; want retain", stored.ReleaseDisposition)
	}
	now = now.Add(workarea.DefaultLeaseDuration + time.Millisecond)
	recovered, err := worktree.NewManager(worktree.Options{ParentDir: dir, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recovered.ReapExpiredTerminalLeases(context.Background(), 10, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := statPath(path); err != nil {
		t.Fatalf("retained workarea missing after restart+expiry: %v", err)
	}
	released, err := recovered.TerminalLease(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if released.State != workarea.LeaseReleased {
		t.Fatalf("lease state = %q; want released after retain reap", released.State)
	}
	if released.ReleaseDisposition != "retain" {
		t.Fatalf("released disposition = %q; want retain", released.ReleaseDisposition)
	}
}
