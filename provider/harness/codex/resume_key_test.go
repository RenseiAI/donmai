package codex

import (
	"os"
	"path/filepath"
	"testing"

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
