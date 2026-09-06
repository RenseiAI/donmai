package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/ptyhost"
	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

func TestRecordResumeKeyAfterRolloutFlush(t *testing.T) {
	registry, err := sessionshim.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := sessionshim.Identity{OrgID: "org-resume", SessionID: "session-resume"}
	if err := registry.Put(sessionshim.Record{
		SchemaVersion: sessionshim.RecordSchemaVersion, OrgID: id.OrgID, SessionID: id.SessionID,
		ShimID: "shim", ProcessEpoch: 1, PID: os.Getpid(), ProcessStartedAt: 1,
		SocketPath: registry.SocketPath(id), ProtocolMin: shimwire.ProtocolMin, ProtocolMax: shimwire.ProtocolMax,
		Phase: shimwire.PhaseRunning, CreatedAtUnixNano: 1,
	}); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	rollout := filepath.Join(home, codexSessionStateSubdir, "2026", "09", "06", "rollout-thread-live.jsonl")
	if err := os.MkdirAll(filepath.Dir(rollout), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rollout, []byte(`{"type":"session_meta"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(sessionshim.EnvCodexResumeRegistry, registry.Dir())
	t.Setenv(sessionshim.EnvCodexResumeOrg, id.OrgID)
	t.Setenv(sessionshim.EnvCodexResumeSession, id.SessionID)
	recordResumeKey(home, "thread-live")
	record, err := registry.Get(id)
	if err != nil || record.ResumeKey == nil || record.ResumeKey.CodexHome != home || record.ResumeKey.ThreadID != "thread-live" {
		t.Fatalf("resume key = %+v, err=%v", record.ResumeKey, err)
	}
}

// TestQuarantinedLiveHarnessKeepsHomeRolloutAndResumeKey is the acceptance RED
// for the incident this change exists for: two long-running interactive
// sessions lost their isolated Codex home on teardown and could not be resumed.
//
// It composes the real seams the incident actually traversed, in order, around
// a genuinely live harness:
//
//	the harness names its thread and flushes a rollout  →  recordResumeKey
//	the controller drops inside the orphan window       →  the shim republishes
//	the next adoption pass REFUSES the survivor         →  quarantine, no kill
//	the session finally ends                            →  tombstone + cleanup
//
// and asserts the only property that matters at the end of it: the recorded
// resume locator still resolves to session state that is still on disk. Each
// half of the fix is separately load-bearing here — reverting the retention
// guard in remove() deletes the home the tombstone points at, and reverting
// either the republish carry or the tombstone carry loses the pointer to a
// home that survived.
func TestQuarantinedLiveHarnessKeepsHomeRolloutAndResumeKey(t *testing.T) {
	dir := shortTempDirForShim(t)
	registry, err := sessionshim.NewRegistry(filepath.Join(dir, "registry"))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	boundary, err := newCodexConfigBoundary(filepath.Join(dir, "homes"), false)
	if err != nil {
		t.Fatalf("new boundary: %v", err)
	}
	rollout := filepath.Join(boundary.home, codexSessionStateSubdir, "2026", "09", "06",
		"rollout-2026-09-06T12-00-00-"+testThreadUUID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(rollout), 0o700); err != nil {
		t.Fatalf("create session state: %v", err)
	}
	if err := os.WriteFile(rollout, []byte(`{"type":"session_meta"}`), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}

	id := sessionshim.Identity{OrgID: "org-quarantine", SessionID: "sess-quarantine"}
	shim, err := sessionshim.Start(sessionshim.Options{
		Identity: id,
		Registry: registry,
		// A real interactive harness: it blocks on the PTY and stays alive for
		// the whole pass, which is the entire point of "quarantine does not
		// kill".
		Spec:         ptyhost.Spec{Command: []string{"/bin/sh", "-c", `while IFS= read -r line; do printf 'ack:%s\n' "$line"; done`}},
		WorkareaPath: filepath.Join(dir, "workarea"),
		Orphan: sessionshim.OrphanPolicy{
			Deadline:          time.Hour, // never fires on its own during this test
			TerminationGrace:  500 * time.Millisecond,
			PropagationMargin: 0,
		},
		ProcessEpoch: 1,
	})
	if errors.Is(err, sessionshim.ErrShimUnsupported) {
		t.Skip("session shim adoption is unsupported on this platform")
	}
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = shim.Close() })

	// The harness's own publication path, through the same runner-local env the
	// launch contract supplies.
	t.Setenv(sessionshim.EnvCodexResumeRegistry, registry.Dir())
	t.Setenv(sessionshim.EnvCodexResumeOrg, id.OrgID)
	t.Setenv(sessionshim.EnvCodexResumeSession, id.SessionID)
	recordResumeKey(boundary.home, testThreadUUID)
	want := sessionshim.ResumeKey{CodexHome: boundary.home, ThreadID: testThreadUUID}
	assertLiveResumeKey(t, registry, id, want, "after the rollout flush")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// A controller adopts and then goes away. Losing it arms the orphan clock,
	// and arming republishes the discovery record from the shim's own in-memory
	// state — the write that used to erase the key.
	adopted, err := sessionshim.Adopt(ctx, sessionshim.AdoptOptions{Registry: registry, ControllerID: "controller-1"})
	if errors.Is(err, sessionshim.ErrShimUnsupported) {
		t.Skip("session shim adoption is unsupported on this platform")
	}
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if len(adopted.Adopted) != 1 {
		t.Fatalf("adopted %d controllers, want 1", len(adopted.Adopted))
	}
	adopted.Close()
	assertLiveResumeKey(t, registry, id, want, "after controller loss republished the record")

	// The next daemon refuses this survivor. Quarantine withdraws authority and
	// counts the slot; it must not kill the harness and must not touch its home.
	refused, err := sessionshim.Adopt(ctx, sessionshim.AdoptOptions{
		Registry:     registry,
		ControllerID: "controller-2",
		ProtocolMin:  shimwire.ProtocolMax + 1,
		ProtocolMax:  shimwire.ProtocolMax + 2,
	})
	if err != nil {
		t.Fatalf("refusing Adopt: %v", err)
	}
	if len(refused.Quarantined) != 1 || len(refused.Adopted) != 0 {
		t.Fatalf("refusing pass quarantined %d / adopted %d, want 1 / 0",
			len(refused.Quarantined), len(refused.Adopted))
	}
	// The exact refusal reason is the adoption classifier's business; what this
	// test pins is that the refusal is a QUARANTINE — a survivor that still
	// occupies capacity because its harness is still running.
	if got := refused.Quarantined[0].Reason; got == "" {
		t.Fatal("quarantined survivor carries no reason")
	}
	if got := refused.OccupiedSlots(); got != 1 {
		t.Fatalf("occupied slots after quarantine = %d, want 1", got)
	}
	if got := shim.Phase(); got == shimwire.PhaseExited {
		t.Fatal("quarantine killed the harness")
	}
	if _, err := os.Stat(rollout); err != nil {
		t.Fatalf("quarantine disturbed the rollout: %v", err)
	}
	assertLiveResumeKey(t, registry, id, want, "while quarantined with a live harness")

	// The session finally ends: the shim reaps its harness and leaves a
	// tombstone, and the runner's PTY teardown runs the isolated-home cleanup.
	if err := shim.Terminate(ctx); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if err := boundary.remove(); err != nil {
		t.Fatalf("boundary remove: %v", err)
	}

	tombstone, err := registry.GetTombstone(id)
	if err != nil {
		t.Fatalf("GetTombstone: %v", err)
	}
	if tombstone.ResumeKey == nil || *tombstone.ResumeKey != want {
		t.Fatalf("terminal resume key = %+v, want %+v", tombstone.ResumeKey, want)
	}
	// The whole point: the locator on the terminal record still names state a
	// resume can actually be pointed at.
	if _, err := os.Stat(filepath.Join(tombstone.ResumeKey.CodexHome,
		codexSessionStateSubdir, "2026", "09", "06",
		"rollout-2026-09-06T12-00-00-"+testThreadUUID+".jsonl")); err != nil {
		t.Fatalf("the resume key points at session state teardown deleted: %v", err)
	}
}

func assertLiveResumeKey(t *testing.T, registry *sessionshim.Registry, id sessionshim.Identity, want sessionshim.ResumeKey, when string) {
	t.Helper()
	record, err := registry.Get(id)
	if err != nil {
		t.Fatalf("live record %s: %v", when, err)
	}
	if record.ResumeKey == nil || *record.ResumeKey != want {
		t.Fatalf("live resume key %s = %+v, want %+v", when, record.ResumeKey, want)
	}
}

// shortTempDirForShim keeps the registry's Unix socket path inside the
// platform's sun_path limit, which the default per-test temp root on darwin
// does not leave room for.
func shortTempDirForShim(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "dcx")
	if err != nil {
		return t.TempDir()
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	for _, sub := range []string{"registry", "homes", "workarea"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			t.Fatalf("create %s: %v", sub, err)
		}
	}
	return dir
}
