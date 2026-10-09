package localqueue

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/result"
)

// TestForcedReplayReturnsOriginalReceipt pins the standalone receiver half of
// the outbox contract: a runner killed after persist but before send has its
// exact bytes replayed by the daemon, and that forced second replay must
// return the ORIGINAL receipt with the revision unchanged — never a second
// terminal transaction. The replay goes through a store reopen (the daemon
// restart) to prove the receipt is durable, not in-memory.
func TestForcedReplayReturnsOriginalReceipt(t *testing.T) {
	root := filepath.Join(t.TempDir(), "queue")
	store, err := Open(root, testBinding())
	if err != nil {
		t.Fatal(err)
	}
	envelope := testEnvelope(t)
	if _, err := store.Admit(context.Background(), store.CurrentRevision(), envelope); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.Claim(context.Background(), store.CurrentRevision(), envelope.Session)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BeginLaunch(context.Background(), store.CurrentRevision(), attempt); err != nil {
		t.Fatal(err)
	}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: "http://127.0.0.1:1", WorkerID: attempt.WorkerID, AuthToken: "fixture-only",
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := poster.PrepareTerminalStatusBody(context.Background(), attempt.SessionID, agent.Result{
		Status: "failed", FailureMode: "silent-exit", Summary: "worker produced no result",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.CommitTerminal(context.Background(), attempt, body)
	if err != nil {
		t.Fatal(err)
	}
	revisionAfterFirst := store.CurrentRevision()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// The daemon restarts, finds no terminal workarea lease for this seat,
	// and replays the exact persisted bytes against the standalone receiver.
	reopened, err := Open(root, testBinding())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	second, err := reopened.CommitTerminal(context.Background(), attempt, bytes.Clone(body))
	if err != nil {
		t.Fatalf("forced replay of exact bytes: %v", err)
	}
	if second.Revision != first.Revision || !bytes.Equal(second.Body, first.Body) ||
		second.BodySHA256 != first.BodySHA256 || second.Status != first.Status {
		t.Fatalf("replay receipt differs:\nfirst=%+v\nsecond=%+v", first, second)
	}
	if reopened.CurrentRevision() != revisionAfterFirst {
		t.Fatal("forced replay advanced the durable revision")
	}
	// A CHANGED body for the same attempt is still a conflict, not a second
	// receipt: the runner record and any other record never both exist.
	changed, err := poster.PrepareTerminalStatusBody(context.Background(), attempt.SessionID, agent.Result{
		Status: "failed", FailureMode: "silent-exit", Summary: "a different summary",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.CommitTerminal(context.Background(), attempt, changed); err == nil {
		t.Fatal("changed terminal body was accepted alongside the original receipt")
	}
}
