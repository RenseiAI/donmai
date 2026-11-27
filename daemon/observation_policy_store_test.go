package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

var hostSessionCodex = SessionSpec{Harness: "codex", AuthMode: string(agent.AuthHostSession)}

// restartedDaemon builds a fresh daemon over dir, as a restart would.
func restartedDaemon(t *testing.T, dir string) *Daemon {
	t.Helper()
	var spawns int
	d := quotaClaimDaemonWithOptions(t, Options{ObservationPolicyDir: dir}, &spawns)
	seedQuotaWindows(d, []agent.UsageWindow{quotaClaimWindow("five_hour", 85, "2031-02-03T04:05:06Z")})
	return d
}

func claimReason(d *Daemon, registration string) string {
	if r := observationUnavailableReportForError(d.ObservationClaimAdmission(registration, hostSessionCodex, time.Now())); r != nil {
		return r.Reason
	}
	return ""
}

func pushPolicy(t *testing.T, d *Daemon, registration string, items string) {
	t.Helper()
	apply := d.ApplyObservationMutations(registration)
	if _, failures := apply(context.Background(), []PendingMutation{policyMutation("p", items)}); len(failures) != 0 {
		t.Fatalf("push to %q failed: %v", registration, failures)
	}
}

func TestObservationPolicyPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	before := restartedDaemon(t, dir)
	pushPolicy(t, before, primaryObservationRegistration, codexOn)
	pushPolicy(t, before, "org-b", claudeOn)
	after := restartedDaemon(t, dir)
	if got := claimReason(after, primaryObservationRegistration); got != observationReasonGuardReached {
		t.Fatalf("after restart the primary claim reason = %q, want %s from the persisted guard", got, observationReasonGuardReached)
	}
	if eff, pushed := after.effectiveObservationPolicy("org-b", claudeSlot); !pushed || !eff.Enabled {
		t.Fatalf("after restart org-b claude = %+v pushed %v, want its persisted policy", eff, pushed)
	}
}

func TestObservationPolicyNeverGovernedPassesThrough(t *testing.T) {
	d := restartedDaemon(t, t.TempDir())
	if got := claimReason(d, primaryObservationRegistration); got != "" {
		t.Fatalf("never-governed claim reason = %q, want pass-through", got)
	}
}

func TestObservationPolicyCorruptFileFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, observationPolicyFileName("org-b")), []byte(`{"version":1,"items":[`), 0o600); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}
	d := restartedDaemon(t, dir)
	if got := claimReason(d, "org-b"); got != observationReasonDisabled {
		t.Fatalf("corrupt org-b policy claim reason = %q, want %s", got, observationReasonDisabled)
	}
	if got := claimReason(d, primaryObservationRegistration); got != "" {
		t.Fatalf("org-b's corrupt file gated the primary registration: %q", got)
	}
}

func TestObservationPolicyFreshPushReplacesPersisted(t *testing.T) {
	dir := t.TempDir()
	d := restartedDaemon(t, dir)
	pushPolicy(t, d, primaryObservationRegistration, codexOn)
	pushPolicy(t, d, primaryObservationRegistration, claudeOn)
	after := restartedDaemon(t, dir)
	if got := claimReason(after, primaryObservationRegistration); got != observationReasonDisabled {
		t.Fatalf("codex after the replacing push and a restart = %q, want %s", got, observationReasonDisabled)
	}
}

func TestObservationPolicyWriteFailureLeavesNoPartialFile(t *testing.T) {
	dir := t.TempDir()
	d := restartedDaemon(t, dir)
	pushPolicy(t, d, primaryObservationRegistration, codexOn)
	path := filepath.Join(dir, observationPolicyFileName(primaryObservationRegistration))
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted policy: %v", err)
	}
	original := writeObservationPolicyFile
	t.Cleanup(func() { writeObservationPolicyFile = original })
	writeObservationPolicyFile = func(f *os.File, data []byte) error {
		_, _ = f.Write(data[:len(data)/2])
		return errors.New("disk full")
	}
	apply := d.ApplyObservationMutations(primaryObservationRegistration)
	if _, failures := apply(context.Background(), []PendingMutation{policyMutation("p2", claudeOn)}); len(failures) != 1 {
		t.Fatalf("failures = %v, want the failed write acked as failed", failures)
	}
	entries, _ := os.ReadDir(dir)
	got, _ := os.ReadFile(path)
	if len(entries) != 1 || string(got) != string(want) {
		t.Fatalf("dir holds %d entries and policy %s, want only the previous intact policy %s", len(entries), got, want)
	}
	if eff, _ := d.effectiveObservationPolicy(primaryObservationRegistration, claudeSlot); !eff.Enabled {
		t.Fatal("a push that failed to persist is not enforced in memory")
	}
}
