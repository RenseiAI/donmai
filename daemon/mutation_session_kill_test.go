package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

func newSessionKillTestDaemon(t *testing.T, command []string) (*Daemon, *WorkerSpawner) {
	t.Helper()
	spawner := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 2,
		WorkerCommand:         command,
	})
	d := &Daemon{spawner: spawner}
	t.Cleanup(func() { _ = spawner.Drain(time.Second) })
	return d, spawner
}

func sessionKillMutation(t *testing.T, id, sessionID string) PendingMutation {
	t.Helper()
	params, err := json.Marshal(map[string]string{"sessionId": sessionID, "reason": "operator"})
	if err != nil {
		t.Fatal(err)
	}
	return PendingMutation{ID: id, Op: "session.kill", Params: params}
}

func TestApplySessionMutationsKillDuplicateAndWrongOwner(t *testing.T) {
	d, spawner := newSessionKillTestDaemon(t, []string{"sleep", "30"})
	if _, err := spawner.AcceptWork(SessionSpec{SessionID: "owned", Repository: "github.com/a/b"}); err != nil {
		t.Fatal(err)
	}

	applied, failures := d.ApplySessionMutations(context.Background(), []PendingMutation{
		{ID: "config-op", Op: "project.add", Params: json.RawMessage(`{}`)},
		sessionKillMutation(t, "kill-1", "owned"),
	})
	if len(applied) != 1 || applied[0] != "kill-1" || len(failures) != 0 {
		t.Fatalf("first apply = applied %v failures %v", applied, failures)
	}

	applied, failures = d.ApplySessionMutations(context.Background(), []PendingMutation{
		sessionKillMutation(t, "kill-duplicate", "owned"),
		sessionKillMutation(t, "wrong-host", "never-owned"),
	})
	if len(applied) != 1 || applied[0] != "kill-duplicate" {
		t.Fatalf("duplicate applied = %v", applied)
	}
	if len(failures) != 1 || failures[0].ID != "wrong-host" || !strings.Contains(failures[0].Error, "not owned") {
		t.Fatalf("wrong-owner failures = %v", failures)
	}
}

func TestApplyPendingMutationsSessionKillPrimary(t *testing.T) {
	d, spawner := newSessionKillTestDaemon(t, []string{"sleep", "30"})
	if _, err := spawner.AcceptWork(SessionSpec{SessionID: "primary-owned", Repository: "github.com/a/b"}); err != nil {
		t.Fatal(err)
	}
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{
		sessionKillMutation(t, "primary-kill", "primary-owned"),
	})
	if len(applied) != 1 || applied[0] != "primary-kill" || len(failures) != 0 {
		t.Fatalf("primary apply = applied %v failures %v", applied, failures)
	}
}

func TestApplySessionMutationsAlreadyDeadAndSignalFailure(t *testing.T) {
	t.Run("already dead", func(t *testing.T) {
		d, spawner := newSessionKillTestDaemon(t, []string{"/bin/sh", "-c", "exit 0"})
		ended := sessionEnds(spawner)
		if _, err := spawner.AcceptWork(SessionSpec{SessionID: "dead", Repository: "github.com/a/b"}); err != nil {
			t.Fatal(err)
		}
		waitSessionEnd(t, ended)
		applied, failures := d.ApplySessionMutations(context.Background(), []PendingMutation{sessionKillMutation(t, "dead-kill", "dead")})
		if len(applied) != 1 || applied[0] != "dead-kill" || len(failures) != 0 {
			t.Fatalf("already-dead apply = applied %v failures %v", applied, failures)
		}
	})

	t.Run("signal failure", func(t *testing.T) {
		d, spawner := newSessionKillTestDaemon(t, []string{"sleep", "30"})
		if _, err := spawner.AcceptWork(SessionSpec{SessionID: "live", Repository: "github.com/a/b"}); err != nil {
			t.Fatal(err)
		}
		spawner.killProcessGroup = func(*exec.Cmd) error { return errors.New("signal denied") }
		applied, failures := d.ApplySessionMutations(context.Background(), []PendingMutation{sessionKillMutation(t, "failed-kill", "live")})
		if len(applied) != 0 || len(failures) != 1 || !strings.Contains(failures[0].Error, "signal denied") {
			t.Fatalf("signal-failure apply = applied %v failures %v", applied, failures)
		}
		if spawner.ActiveCount() != 1 {
			t.Fatal("signal failure removed the live session")
		}
	})
}

func TestSessionKillMutationACKsOnNextHeartbeat(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")
	d, spawner := newSessionKillTestDaemon(t, []string{"sleep", "30"})
	if _, err := spawner.AcceptWork(SessionSpec{SessionID: "ack-owned", Repository: "github.com/a/b"}); err != nil {
		t.Fatal(err)
	}

	var (
		mu       sync.Mutex
		requests []heartbeatRequestBody
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload heartbeatRequestBody
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &payload)
		mu.Lock()
		requests = append(requests, payload)
		beat := len(requests)
		mu.Unlock()
		response := heartbeatResponseBody{Acknowledged: true}
		if beat == 1 {
			response.PendingMutations = []PendingMutation{sessionKillMutation(t, "ack-kill", "ack-owned")}
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(srv.Close)

	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID: "worker", Hostname: "host", OrchestratorURL: srv.URL, RuntimeJWT: "runtime.jwt",
		GetActiveCount:     func() int { return spawner.ActiveCount() },
		GetMaxCount:        func() int { return 2 },
		GetStatus:          func() RegistrationStatus { return RegistrationIdle },
		OnPendingMutations: d.ApplySessionMutations,
	})
	hs.sendOne(context.Background())
	hs.sendOne(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 || len(requests[1].AppliedMutations) != 1 || requests[1].AppliedMutations[0] != "ack-kill" {
		t.Fatalf("heartbeat requests = %+v", requests)
	}
	if len(requests[1].MutationFailures) != 0 {
		t.Fatalf("unexpected failure ACKs: %v", requests[1].MutationFailures)
	}
}
