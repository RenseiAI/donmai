package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func hostUpgradeTestDaemon(t *testing.T, store interface {
	AcknowledgeExact(ctx context.Context, req interface{}) (interface{}, error)
},
) *Daemon {
	t.Helper()
	_ = store
	d := newRestartTestDaemon(t, nil)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "daemon.yaml")
	d.opts.ConfigPath = cfgPath
	d.config = &Config{
		APIVersion: "v1",
		Kind:       "DaemonConfig",
		Machine:    MachineConfig{ID: "test-machine"},
		Capacity:   CapacityConfig{MaxConcurrentSessions: 2},
		Orchestrator: OrchestratorConfig{
			URL: "https://example.test",
		},
		AutoUpdate: AutoUpdateConfig{
			Channel:             ChannelStable,
			Schedule:            ScheduleManual,
			DrainTimeoutSeconds: 1,
			Signers: []UpdateSigner{
				{SAN: "test@example.com", Issuer: "https://example.test"},
			},
		},
	}
	return d
}

func hostUpgradeMutation(id string, params any) PendingMutation {
	raw, _ := json.Marshal(params)
	return PendingMutation{
		ID:          id,
		Op:          hostUpgradeOp,
		Params:      raw,
		RequestedAt: time.Now().UTC().Format(time.RFC3339),
		RequestedBy: "tester",
	}
}

func waitForHostUpgradeDone(t *testing.T, d *Daemon, id string) hostUpgradeOutcome {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		d.hostUpgradeMu.Lock()
		outcome, ok := d.hostUpgradeDone[id]
		d.hostUpgradeMu.Unlock()
		if ok {
			return outcome
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for host upgrade outcome %q", id)
	return hostUpgradeOutcome{}
}

func TestParseHostUpgradeParams(t *testing.T) {
	t.Parallel()
	now := time.Now()
	future := now.Add(time.Hour).UTC().Format(time.RFC3339)
	past := now.Add(-time.Hour).UTC().Format(time.RFC3339)
	stale := now.Add(-25 * time.Hour).UTC().Format(time.RFC3339)

	trueVal := true
	falseVal := false
	_ = trueVal
	_ = falseVal

	cases := []struct {
		name       string
		params     string
		requested  string
		wantErrSub string
	}{
		{name: "valid consented", params: `{"consented":true}`, wantErrSub: ""},
		{name: "valid with future expiry", params: `{"consented":true,"expiresAt":"` + future + `"}`, wantErrSub: ""},
		{name: "missing params", params: ``, wantErrSub: "requires params"},
		{name: "malformed json", params: `{not-json`, wantErrSub: "decode params"},
		{name: "missing consent", params: `{}`, wantErrSub: "consented"},
		{name: "consent false", params: `{"consented":false}`, wantErrSub: "consented"},
		{name: "expired explicit", params: `{"consented":true,"expiresAt":"` + past + `"}`, wantErrSub: "expired"},
		{name: "malformed expiry", params: `{"consented":true,"expiresAt":"yesterday"}`, wantErrSub: "malformed"},
		{name: "forbidden url", params: `{"consented":true,"url":"https://example.test/x"}`, wantErrSub: "never taken"},
		{name: "forbidden signers", params: `{"consented":true,"signers":[]}`, wantErrSub: "never taken"},
		{name: "forbidden binary", params: `{"consented":true,"binary":"abc"}`, wantErrSub: "never taken"},
		{name: "forbidden sha", params: `{"consented":true,"sha256":"abc"}`, wantErrSub: "never taken"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := PendingMutation{ID: "m1", Op: hostUpgradeOp, Params: json.RawMessage(tt.params), RequestedAt: tt.requested}
			err := parseHostUpgradeParams(m, now)
			if tt.wantErrSub == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErrSub != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErrSub)
				}
				if !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErrSub)
				}
			}
		})
	}
	if err := parseHostUpgradeParams(PendingMutation{ID: "s", Op: hostUpgradeOp, Params: json.RawMessage(`{"consented":true}`), RequestedAt: stale}, now); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale requestedAt should refuse, got %v", err)
	}
}

func TestHostUpgradeHeartbeatWiringValidReachesUpdate(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")
	d := hostUpgradeTestDaemon(t, nil)
	var calls atomic.Int32
	d.runPreparedUpdate = func(context.Context, AutoUpdateConfig, string) (*UpdateResult, error) {
		calls.Add(1)
		return &UpdateResult{Updated: true, Version: "9.9.9", Reason: "update-applied"}, nil
	}

	var mu sync.Mutex
	var requests []heartbeatRequestBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload heartbeatRequestBody
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &payload)
		mu.Lock()
		requests = append(requests, payload)
		beat := len(requests)
		mu.Unlock()
		resp := heartbeatResponseBody{Acknowledged: true}
		if beat == 1 {
			resp.PendingMutations = []PendingMutation{hostUpgradeMutation("up-valid-1", map[string]any{"consented": true})}
		}
		if beat == 2 {
			// Redeliver until the daemon ACKs: the first beat defers the ACK
			// while the bounded attempt runs.
			hasACK := false
			for _, id := range payload.AppliedMutations {
				if id == "up-valid-1" {
					hasACK = true
				}
			}
			for _, f := range payload.MutationFailures {
				if f.ID == "up-valid-1" {
					hasACK = true
				}
			}
			if !hasACK {
				resp.PendingMutations = []PendingMutation{hostUpgradeMutation("up-valid-1", map[string]any{"consented": true})}
			}
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)

	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID: "worker", Hostname: "host", OrchestratorURL: srv.URL, RuntimeJWT: "runtime.jwt",
		GetActiveCount: func() int { return 0 },
		GetMaxCount:    func() int { return 2 },
		GetStatus:      func() RegistrationStatus { return RegistrationIdle },
		// Production callback wiring: the daemon's real applier, not a test helper.
		OnPendingMutations: d.applyPendingMutations,
	})
	hs.sendOne(context.Background())
	outcome := waitForHostUpgradeDone(t, d, "up-valid-1")
	if !outcome.Applied {
		t.Fatalf("valid upgrade outcome = %+v, want applied", outcome)
	}
	hs.sendOne(context.Background())
	hs.sendOne(context.Background())

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, req := range requests {
		for _, id := range req.AppliedMutations {
			if id == "up-valid-1" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("heartbeat requests never ACKed applied up-valid-1: %+v", requests)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("update calls = %d, want exactly 1", got)
	}
}

func TestHostUpgradeRefusesWithoutSwap(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		params map[string]any
		mutate func(d *Daemon)
	}{
		{name: "malformed", params: nil},
		{name: "unconsented", params: map[string]any{"consented": false}},
		{name: "missing consent", params: map[string]any{}},
		{name: "forbidden url", params: map[string]any{"consented": true, "url": "https://example.test/bin"}},
		{name: "expired", params: map[string]any{"consented": true, "expiresAt": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := hostUpgradeTestDaemon(t, nil)
			if tt.mutate != nil {
				tt.mutate(d)
			}
			var calls atomic.Int32
			d.runPreparedUpdate = func(context.Context, AutoUpdateConfig, string) (*UpdateResult, error) {
				calls.Add(1)
				return &UpdateResult{Updated: true, Version: "x", Reason: "update-applied"}, nil
			}
			var raw json.RawMessage
			if tt.params == nil {
				raw = json.RawMessage(`{not-json`)
			} else {
				raw, _ = json.Marshal(tt.params)
			}
			m := PendingMutation{ID: "up-refuse-" + tt.name, Op: hostUpgradeOp, Params: raw, RequestedAt: time.Now().UTC().Format(time.RFC3339)}
			applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{m})
			if len(applied) != 0 {
				t.Fatalf("applied = %v, want none", applied)
			}
			if len(failures) != 1 || failures[0].ID != m.ID {
				t.Fatalf("failures = %v, want one for %q", failures, m.ID)
			}
			if got := calls.Load(); got != 0 {
				t.Fatalf("update calls = %d, want 0 (refuse without swap)", got)
			}
			// Redelivery answers from the recorded outcome without a second attempt.
			applied, failures = d.applyPendingMutations(context.Background(), []PendingMutation{m})
			if len(applied) != 0 || len(failures) != 1 {
				t.Fatalf("redelivery applied/failures = %v/%v, want failure", applied, failures)
			}
			if got := calls.Load(); got != 0 {
				t.Fatalf("redelivery update calls = %d, want 0", got)
			}
		})
	}
}

func TestHostUpgradeRefusesWhenNoSignedSource(t *testing.T) {
	t.Parallel()
	d := hostUpgradeTestDaemon(t, nil)
	d.config.AutoUpdate.Signers = nil
	var calls atomic.Int32
	d.runPreparedUpdate = func(context.Context, AutoUpdateConfig, string) (*UpdateResult, error) {
		calls.Add(1)
		return &UpdateResult{Updated: true, Version: "x", Reason: "update-applied"}, nil
	}
	m := hostUpgradeMutation("up-no-optin", map[string]any{"consented": true})
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{m})
	if len(applied) != 0 || len(failures) != 1 {
		t.Fatalf("applied/failures = %v/%v, want refusal", applied, failures)
	}
	if !strings.Contains(failures[0].Error, "consent") {
		t.Fatalf("failure %q does not name machine-owner consent", failures[0].Error)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("update calls = %d, want 0", got)
	}
}

func TestHostUpgradeDuplicateDoesNotReswap(t *testing.T) {
	t.Parallel()
	d := hostUpgradeTestDaemon(t, nil)
	var calls atomic.Int32
	d.runPreparedUpdate = func(context.Context, AutoUpdateConfig, string) (*UpdateResult, error) {
		calls.Add(1)
		return &UpdateResult{Updated: true, Version: "2.0.0", Reason: "update-applied"}, nil
	}
	m := hostUpgradeMutation("up-dup", map[string]any{"consented": true})
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{m})
	if len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("first delivery applied/failures = %v/%v, want pending (no ACK yet)", applied, failures)
	}
	waitForHostUpgradeDone(t, d, "up-dup")
	applied, failures = d.applyPendingMutations(context.Background(), []PendingMutation{m})
	if len(applied) != 1 || applied[0] != "up-dup" || len(failures) != 0 {
		t.Fatalf("redelivery applied/failures = %v/%v, want applied", applied, failures)
	}
	applied, failures = d.applyPendingMutations(context.Background(), []PendingMutation{m})
	if len(applied) != 1 || len(failures) != 0 {
		t.Fatalf("second redelivery applied/failures = %v/%v, want applied", applied, failures)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("update calls = %d, want 1", got)
	}
}

func TestHostUpgradeSimultaneousSecondRefuses(t *testing.T) {
	t.Parallel()
	d := hostUpgradeTestDaemon(t, nil)
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	d.runPreparedUpdate = func(ctx context.Context, _ AutoUpdateConfig, _ string) (*UpdateResult, error) {
		calls.Add(1)
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &UpdateResult{Updated: true, Version: "3.0.0", Reason: "update-applied"}, nil
	}
	first := hostUpgradeMutation("up-first", map[string]any{"consented": true})
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{first})
	if len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("first applied/failures = %v/%v, want pending", applied, failures)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first upgrade did not start")
	}
	second := hostUpgradeMutation("up-second", map[string]any{"consented": true})
	applied, failures = d.applyPendingMutations(context.Background(), []PendingMutation{second})
	if len(applied) != 0 || len(failures) != 1 || failures[0].ID != "up-second" {
		t.Fatalf("simultaneous applied/failures = %v/%v, want failure for second", applied, failures)
	}
	close(release)
	waitForHostUpgradeDone(t, d, "up-first")
	if got := calls.Load(); got != 1 {
		t.Fatalf("update calls = %d, want 1 (no second swap)", got)
	}
	// A terminal busy refusal is durable: if its failure ACK is lost, a
	// redelivery after contention clears must replay the same refusal
	// instead of executing a request already reported refused.
	applied, failures = d.applyPendingMutations(context.Background(), []PendingMutation{second})
	if len(applied) != 0 || len(failures) != 1 || failures[0].ID != "up-second" {
		t.Fatalf("post-contention redelivery applied/failures = %v/%v, want replayed failure", applied, failures)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("post-contention update calls = %d, want 1 (no second swap)", got)
	}
}

func TestHostUpgradeManualLifecycleContention(t *testing.T) {
	t.Parallel()
	d := hostUpgradeTestDaemon(t, nil)
	lease, ok := d.tryClaimLifecycle(lifecycleUpdate)
	if !ok {
		t.Fatal("could not claim lifecycle for manual holder")
	}
	var calls atomic.Int32
	d.runPreparedUpdate = func(context.Context, AutoUpdateConfig, string) (*UpdateResult, error) {
		calls.Add(1)
		return &UpdateResult{Updated: true, Version: "x", Reason: "update-applied"}, nil
	}
	m := hostUpgradeMutation("up-manual-race", map[string]any{"consented": true})
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{m})
	if len(applied) != 0 || len(failures) != 1 {
		t.Fatalf("applied/failures = %v/%v, want lifecycle-busy failure", applied, failures)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("update calls = %d, want 0", got)
	}
	// Release the manual holder: a redelivery after contention clears must
	// still replay the recorded refusal, never execute the refused request.
	d.releaseLifecycle(lease)
	applied, failures = d.applyPendingMutations(context.Background(), []PendingMutation{m})
	if len(applied) != 0 || len(failures) != 1 {
		t.Fatalf("post-contention redelivery applied/failures = %v/%v, want replayed failure", applied, failures)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("post-contention update calls = %d, want 0", got)
	}
}

func TestHostUpgradePreflightFailureNoSwap(t *testing.T) {
	t.Parallel()
	d := hostUpgradeTestDaemon(t, nil)
	// Occupy the host with a direct-owned session so restart preflight refuses.
	d.spawner = NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 2,
		WorkerCommand:         []string{"sleep", "30"},
	})
	t.Cleanup(func() { _ = d.spawner.Drain(time.Second) })
	if _, err := d.spawner.AcceptWork(SessionSpec{SessionID: "direct", Repository: "github.com/a/b"}); err != nil {
		t.Fatalf("accept direct work: %v", err)
	}
	var calls atomic.Int32
	d.runPreparedUpdate = func(context.Context, AutoUpdateConfig, string) (*UpdateResult, error) {
		calls.Add(1)
		return &UpdateResult{Updated: true, Version: "x", Reason: "update-applied"}, nil
	}
	m := hostUpgradeMutation("up-preflight", map[string]any{"consented": true})
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{m})
	if len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("preflight delivery applied/failures = %v/%v, want pending", applied, failures)
	}
	outcome := waitForHostUpgradeDone(t, d, "up-preflight")
	if outcome.Applied {
		t.Fatalf("preflight outcome = %+v, want refusal", outcome)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("update calls = %d, want 0 (preflight before swap)", got)
	}
	applied, failures = d.applyPendingMutations(context.Background(), []PendingMutation{m})
	if len(applied) != 0 || len(failures) != 1 {
		t.Fatalf("redelivery applied/failures = %v/%v, want failure", applied, failures)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("redelivery calls = %d, want 0", got)
	}
}

func TestHostUpgradeSignatureFailureNoSwap(t *testing.T) {
	t.Parallel()
	d := hostUpgradeTestDaemon(t, nil)
	var calls atomic.Int32
	d.runPreparedUpdate = func(context.Context, AutoUpdateConfig, string) (*UpdateResult, error) {
		calls.Add(1)
		return &UpdateResult{Updated: false, Version: "1.0.0", Reason: "sig-rejected: bad bundle"}, nil
	}
	m := hostUpgradeMutation("up-sigfail", map[string]any{"consented": true})
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{m})
	if len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("first applied/failures = %v/%v, want pending", applied, failures)
	}
	outcome := waitForHostUpgradeDone(t, d, "up-sigfail")
	if outcome.Applied {
		t.Fatalf("signature-failure outcome = %+v, want failure", outcome)
	}
	applied, failures = d.applyPendingMutations(context.Background(), []PendingMutation{m})
	if len(applied) != 0 || len(failures) != 1 {
		t.Fatalf("redelivery applied/failures = %v/%v, want failure", applied, failures)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("update calls = %d, want 1", got)
	}
	// A duplicate after a signature failure must not run a second swap.
	applied, failures = d.applyPendingMutations(context.Background(), []PendingMutation{m})
	if len(applied) != 0 || len(failures) != 1 {
		t.Fatalf("second redelivery applied/failures = %v/%v, want failure", applied, failures)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls after duplicates = %d, want 1", got)
	}
}

func TestHostUpgradeHeartbeatProgressDuringDelayedUpdate(t *testing.T) {
	t.Parallel()
	d := hostUpgradeTestDaemon(t, nil)
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	d.runPreparedUpdate = func(ctx context.Context, _ AutoUpdateConfig, _ string) (*UpdateResult, error) {
		calls.Add(1)
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &UpdateResult{Updated: true, Version: "4.0.0", Reason: "update-applied"}, nil
	}
	m := hostUpgradeMutation("up-slow", map[string]any{"consented": true})
	if applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{m}); len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("slow first applied/failures = %v/%v, want pending", applied, failures)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("slow upgrade did not start")
	}
	// The next beat must still progress while the update is delayed. A
	// same-ID redelivery observes the in-flight attempt and defers its ACK
	// (pending): it must not start a second swap and must not ACK early.
	// This drives the production same-ID in-flight branch, not a helper.
	type beatResult struct {
		applied  []string
		failures []HeartbeatMutationFailure
	}
	done := make(chan beatResult, 1)
	go func() {
		applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{m})
		done <- beatResult{applied: applied, failures: failures}
	}()
	var redelivery beatResult
	select {
	case redelivery = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("heartbeat blocked behind delayed update")
	}
	if len(redelivery.applied) != 0 || len(redelivery.failures) != 0 {
		t.Fatalf("in-flight duplicate applied/failures = %v/%v, want pending (no ACK yet)", redelivery.applied, redelivery.failures)
	}
	// The in-flight duplicate must not have started a second attempt and must
	// not have ACKed before the durable outcome.
	d.hostUpgradeMu.Lock()
	_, inFlight := d.hostUpgradeInFlight["up-slow"]
	_, completed := d.hostUpgradeDone["up-slow"]
	d.hostUpgradeMu.Unlock()
	if !inFlight || completed {
		t.Fatalf("inFlight=%v completed=%v, want in-flight without early completion", inFlight, completed)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("update calls during in-flight duplicate = %d, want 1 (no second swap)", got)
	}
	close(release)
	waitForHostUpgradeDone(t, d, "up-slow")
	if got := calls.Load(); got != 1 {
		t.Fatalf("update calls after completion = %d, want 1", got)
	}
}

func TestHostUpgradeShutdownCancelsAttempt(t *testing.T) {
	t.Parallel()
	d := hostUpgradeTestDaemon(t, nil)
	entered := make(chan struct{})
	d.runPreparedUpdate = func(ctx context.Context, _ AutoUpdateConfig, _ string) (*UpdateResult, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	m := hostUpgradeMutation("up-shutdown", map[string]any{"consented": true})
	if applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{m}); len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("applied/failures = %v/%v, want pending", applied, failures)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("upgrade did not start")
	}
	// Drive the production shutdown path: Stop must cancel the in-flight
	// attempt before waiting on the lifecycle lease. Invoking the cancel
	// helper directly would bypass this integration.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopCancel()
	if err := d.Stop(stopCtx); err != nil {
		t.Fatalf("Stop during in-flight upgrade: %v", err)
	}
	outcome := waitForHostUpgradeDone(t, d, "up-shutdown")
	if outcome.Applied {
		t.Fatalf("cancelled outcome = %+v, want failure", outcome)
	}
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{m})
	if len(applied) != 0 || len(failures) != 1 {
		t.Fatalf("post-cancel redelivery = %v/%v, want failure", applied, failures)
	}
}

func TestHostUpgradePostRestartOutcomeFromDurableFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "daemon.yaml")
	first := newRestartTestDaemon(t, nil)
	first.opts.ConfigPath = cfgPath
	first.config = &Config{
		AutoUpdate: AutoUpdateConfig{
			Channel: ChannelStable, Schedule: ScheduleManual,
			Signers: []UpdateSigner{{SAN: "test@example.com", Issuer: "https://example.test"}},
		},
	}
	first.runPreparedUpdate = func(context.Context, AutoUpdateConfig, string) (*UpdateResult, error) {
		return &UpdateResult{Updated: true, Version: "5.0.0", Reason: "update-applied"}, nil
	}
	m := hostUpgradeMutation("up-restart", map[string]any{"consented": true})
	if applied, failures := first.applyPendingMutations(context.Background(), []PendingMutation{m}); len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("first applied/failures = %v/%v, want pending", applied, failures)
	}
	waitForHostUpgradeDone(t, first, "up-restart")

	// Simulate a process restart: a fresh daemon with the same config dir must
	// answer the redelivery from the durable outcome without a second swap.
	second := newRestartTestDaemon(t, nil)
	second.opts.ConfigPath = cfgPath
	second.config = &Config{
		AutoUpdate: AutoUpdateConfig{
			Channel: ChannelStable, Schedule: ScheduleManual,
			Signers: []UpdateSigner{{SAN: "test@example.com", Issuer: "https://example.test"}},
		},
	}
	var calls atomic.Int32
	second.runPreparedUpdate = func(context.Context, AutoUpdateConfig, string) (*UpdateResult, error) {
		calls.Add(1)
		return &UpdateResult{Updated: true, Version: "5.0.0", Reason: "update-applied"}, nil
	}
	applied, failures := second.applyPendingMutations(context.Background(), []PendingMutation{m})
	if len(applied) != 1 || applied[0] != "up-restart" || len(failures) != 0 {
		t.Fatalf("post-restart applied/failures = %v/%v, want applied", applied, failures)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("post-restart update calls = %d, want 0 (no second swap)", got)
	}
}

func TestHostUpgradeOlderServerWithNoOperation(t *testing.T) {
	t.Parallel()
	d := hostUpgradeTestDaemon(t, nil)
	var calls atomic.Int32
	d.runPreparedUpdate = func(context.Context, AutoUpdateConfig, string) (*UpdateResult, error) {
		calls.Add(1)
		return &UpdateResult{Updated: true, Version: "x", Reason: "update-applied"}, nil
	}
	applied, failures := d.applyPendingMutations(context.Background(), nil)
	if len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("empty batch applied/failures = %v/%v, want none", applied, failures)
	}
	applied, failures = d.applyPendingMutations(context.Background(), []PendingMutation{
		{ID: "other-1", Op: "pool.deleted", Params: json.RawMessage(`{"poolId":"pool-gone"}`)},
	})
	if len(applied) != 1 || len(failures) != 0 {
		t.Fatalf("unrelated op applied/failures = %v/%v", applied, failures)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("update calls = %d, want 0 for server with no upgrade op", got)
	}
	d.hostUpgradeMu.Lock()
	inFlight := len(d.hostUpgradeInFlight)
	done := len(d.hostUpgradeDone)
	d.hostUpgradeMu.Unlock()
	if inFlight != 0 || done != 0 {
		t.Fatalf("upgrade state inFlight=%d done=%d, want 0/0", inFlight, done)
	}
}

func TestHostUpgradeNeverTakesReleaseURL(t *testing.T) {
	t.Parallel()
	d := hostUpgradeTestDaemon(t, nil)
	var seenConfig AutoUpdateConfig
	d.runPreparedUpdate = func(_ context.Context, cfg AutoUpdateConfig, _ string) (*UpdateResult, error) {
		seenConfig = cfg
		return &UpdateResult{Updated: true, Version: "6.0.0", Reason: "update-applied"}, nil
	}
	evil := map[string]any{
		"consented": true,
		"url":       "https://evil.test/bin",
		"signers":   []any{},
		"sha256":    "abc",
	}
	m := hostUpgradeMutation("up-evil", evil)
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{m})
	if len(applied) != 0 || len(failures) != 1 {
		t.Fatalf("evil applied/failures = %v/%v, want refusal", applied, failures)
	}
	d.hostUpgradeMu.Lock()
	_, completed := d.hostUpgradeDone["up-evil"]
	d.hostUpgradeMu.Unlock()
	if !completed {
		t.Fatal("evil request outcome not recorded")
	}
	if seenConfig.Channel != "" || len(seenConfig.Signers) != 0 {
		t.Fatalf("remote request leaked into update config: %+v", seenConfig)
	}
}

func TestHostUpgradeExactExpiryBoundary(t *testing.T) {
	t.Parallel()
	base := time.Now().UTC().Truncate(time.Second)
	exact := base.Format(time.RFC3339)
	m := PendingMutation{ID: "b", Op: hostUpgradeOp, Params: json.RawMessage(`{"consented":true,"expiresAt":"` + exact + `"}`)}
	if err := parseHostUpgradeParams(m, base); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expiresAt == now should refuse expired, got %v", err)
	}
	later := base.Add(time.Second).Format(time.RFC3339)
	m2 := PendingMutation{ID: "b2", Op: hostUpgradeOp, Params: json.RawMessage(`{"consented":true,"expiresAt":"` + later + `"}`)}
	if err := parseHostUpgradeParams(m2, base); err != nil {
		t.Fatalf("expiresAt == now+1s should be valid, got %v", err)
	}
}

func TestHostUpgradeRefusesWithoutDurablePath(t *testing.T) {
	t.Parallel()
	d := hostUpgradeTestDaemon(t, nil)
	d.opts.ConfigPath = ""
	var calls atomic.Int32
	d.runPreparedUpdate = func(context.Context, AutoUpdateConfig, string) (*UpdateResult, error) {
		calls.Add(1)
		return &UpdateResult{Updated: true, Version: "x", Reason: "update-applied"}, nil
	}
	m := hostUpgradeMutation("up-no-path", map[string]any{"consented": true})
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{m})
	if len(applied) != 0 || len(failures) != 1 {
		t.Fatalf("applied/failures = %v/%v, want failure without durable path", applied, failures)
	}
	if !strings.Contains(failures[0].Error, "durable") {
		t.Fatalf("failure %q should name durable state", failures[0].Error)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("update calls = %d, want 0", got)
	}
	applied, failures = d.applyPendingMutations(context.Background(), []PendingMutation{m})
	if len(applied) != 0 || len(failures) != 1 {
		t.Fatalf("redelivery applied/failures = %v/%v, want failure", applied, failures)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("redelivery calls = %d, want 0", got)
	}
}

func TestHostUpgradeUnwritableStateRefusesWithoutSwap(t *testing.T) {
	t.Parallel()
	d := hostUpgradeTestDaemon(t, nil)
	d.opts.ConfigPath = filepath.Join(t.TempDir(), "no-such-dir", "daemon.yaml")
	var calls atomic.Int32
	d.runPreparedUpdate = func(context.Context, AutoUpdateConfig, string) (*UpdateResult, error) {
		calls.Add(1)
		return &UpdateResult{Updated: true, Version: "x", Reason: "update-applied"}, nil
	}
	m := hostUpgradeMutation("up-unwritable", map[string]any{"consented": true})
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{m})
	if len(applied) != 0 || len(failures) != 1 {
		t.Fatalf("applied/failures = %v/%v, want failure for unwritable state", applied, failures)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("update calls = %d, want 0 (refuse without swap)", got)
	}
	applied, failures = d.applyPendingMutations(context.Background(), []PendingMutation{m})
	if len(applied) != 0 || len(failures) != 1 {
		t.Fatalf("redelivery applied/failures = %v/%v, want failure", applied, failures)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("redelivery calls = %d, want 0", got)
	}
}

func TestHostUpgradeCorruptDurableRefusesWithoutSwap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "daemon.yaml")
	statePath := hostUpgradeStatePath(cfgPath)
	// Write a corrupt durable file before any upgrade.
	if err := os.WriteFile(statePath, []byte("{corrupt-json"), 0o600); err != nil {
		t.Fatalf("write corrupt state: %v", err)
	}
	d := newRestartTestDaemon(t, nil)
	d.opts.ConfigPath = cfgPath
	d.config = &Config{
		AutoUpdate: AutoUpdateConfig{
			Channel: ChannelStable, Schedule: ScheduleManual,
			Signers: []UpdateSigner{{SAN: "test@example.com", Issuer: "https://example.test"}},
		},
	}
	var calls atomic.Int32
	d.runPreparedUpdate = func(context.Context, AutoUpdateConfig, string) (*UpdateResult, error) {
		calls.Add(1)
		return &UpdateResult{Updated: true, Version: "x", Reason: "update-applied"}, nil
	}
	m := hostUpgradeMutation("up-corrupt", map[string]any{"consented": true})
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{m})
	if len(applied) != 0 || len(failures) != 1 {
		t.Fatalf("corrupt applied/failures = %v/%v, want failure", applied, failures)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("corrupt update calls = %d, want 0 (no second swap)", got)
	}
	// Restart with the same corrupt file must still refuse without a swap.
	second := newRestartTestDaemon(t, nil)
	second.opts.ConfigPath = cfgPath
	second.config = &Config{
		AutoUpdate: AutoUpdateConfig{
			Channel: ChannelStable, Schedule: ScheduleManual,
			Signers: []UpdateSigner{{SAN: "test@example.com", Issuer: "https://example.test"}},
		},
	}
	second.runPreparedUpdate = func(context.Context, AutoUpdateConfig, string) (*UpdateResult, error) {
		calls.Add(1)
		return &UpdateResult{Updated: true, Version: "x", Reason: "update-applied"}, nil
	}
	applied, failures = second.applyPendingMutations(context.Background(), []PendingMutation{m})
	if len(applied) != 0 || len(failures) != 1 {
		t.Fatalf("post-corrupt applied/failures = %v/%v, want failure", applied, failures)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("post-corrupt calls = %d, want 0", got)
	}
}

func TestHostUpgradeRefusesWithoutUpdateSource(t *testing.T) {
	t.Parallel()
	// Production path with no test seam and no configured CDN base: the real
	// updater would short-circuit to already-up-to-date without checking any
	// release. Admission must refuse without a swap instead of ACKing applied.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "daemon.yaml")
	d := newRestartTestDaemon(t, nil)
	d.opts.ConfigPath = cfgPath
	d.config = &Config{
		AutoUpdate: AutoUpdateConfig{
			Channel: ChannelStable, Schedule: ScheduleManual,
			Signers: []UpdateSigner{{SAN: "test@example.com", Issuer: "https://example.test"}},
		},
	}
	d.runPreparedUpdate = nil
	m := hostUpgradeMutation("up-no-source", map[string]any{"consented": true})
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{m})
	if len(applied) != 0 || len(failures) != 1 {
		t.Fatalf("no-source applied/failures = %v/%v, want refusal", applied, failures)
	}
	if !strings.Contains(failures[0].Error, "source") {
		t.Fatalf("failure %q should name the missing update source", failures[0].Error)
	}
	if got := d.State(); got != StateRunning {
		t.Fatalf("state = %q, want running (no draining for refused request)", got)
	}
	applied, failures = d.applyPendingMutations(context.Background(), []PendingMutation{m})
	if len(applied) != 0 || len(failures) != 1 {
		t.Fatalf("redelivery applied/failures = %v/%v, want replayed failure", applied, failures)
	}
}

func TestHostUpgradePersistFailureDoesNotAckApplied(t *testing.T) {
	t.Parallel()
	d := hostUpgradeTestDaemon(t, nil)
	stateDir := filepath.Dir(d.opts.ConfigPath)
	var calls atomic.Int32
	d.runPreparedUpdate = func(context.Context, AutoUpdateConfig, string) (*UpdateResult, error) {
		calls.Add(1)
		// Remove the durable directory so the final outcome write fails.
		// The admission intent was already persisted before this runs.
		_ = os.RemoveAll(stateDir)
		return &UpdateResult{Updated: true, Version: "9.9.9", Reason: "update-applied"}, nil
	}
	m := hostUpgradeMutation("lost-outcome", map[string]any{"consented": true})
	if applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{m}); len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("first applied/failures = %v/%v, want pending", applied, failures)
	}
	outcome := waitForHostUpgradeDone(t, d, "lost-outcome")
	if outcome.Applied {
		t.Fatalf("persist-failure outcome = %+v, want failure (no durable receipt)", outcome)
	}
	d.hostUpgradeMu.Lock()
	broken := d.hostUpgradeDurableBroken
	d.hostUpgradeMu.Unlock()
	if !broken {
		t.Fatal("durability should be marked broken after final persist failure")
	}
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{m})
	if len(applied) != 0 || len(failures) != 1 {
		t.Fatalf("redelivery applied/failures = %v/%v, want failure (never applied without receipt)", applied, failures)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("update calls = %d, want 1", got)
	}
}

func TestHostUpgradeCrashPendingDoesNotReswap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "daemon.yaml")
	first := newRestartTestDaemon(t, nil)
	first.opts.ConfigPath = cfgPath
	first.config = &Config{
		AutoUpdate: AutoUpdateConfig{
			Channel: ChannelStable, Schedule: ScheduleManual,
			Signers: []UpdateSigner{{SAN: "test@example.com", Issuer: "https://example.test"}},
		},
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var firstCalls atomic.Int32
	first.runPreparedUpdate = func(ctx context.Context, _ AutoUpdateConfig, _ string) (*UpdateResult, error) {
		firstCalls.Add(1)
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &UpdateResult{Updated: true, Version: "7.0.0", Reason: "update-applied"}, nil
	}
	m := hostUpgradeMutation("up-crash", map[string]any{"consented": true})
	if applied, failures := first.applyPendingMutations(context.Background(), []PendingMutation{m}); len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("first applied/failures = %v/%v, want pending", applied, failures)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first upgrade did not start")
	}
	// Durable intent must exist before the swap completes: a crash here
	// leaves request identity on disk.
	statePath := hostUpgradeStatePath(cfgPath)
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read durable intent: %v", err)
	}
	var stored map[string]hostUpgradeOutcome
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("decode durable intent: %v", err)
	}
	if intent, ok := stored["up-crash"]; !ok || !intent.Pending {
		t.Fatalf("durable intent = %+v, want pending record for up-crash", stored)
	}
	// Simulate a process exit between intent and final outcome: a fresh
	// daemon with the same state dir must not run a second swap on
	// redelivery. It answers the interrupted failure instead.
	second := newRestartTestDaemon(t, nil)
	second.opts.ConfigPath = cfgPath
	second.config = &Config{
		AutoUpdate: AutoUpdateConfig{
			Channel: ChannelStable, Schedule: ScheduleManual,
			Signers: []UpdateSigner{{SAN: "test@example.com", Issuer: "https://example.test"}},
		},
	}
	var secondCalls atomic.Int32
	second.runPreparedUpdate = func(context.Context, AutoUpdateConfig, string) (*UpdateResult, error) {
		secondCalls.Add(1)
		return &UpdateResult{Updated: true, Version: "7.0.1", Reason: "update-applied"}, nil
	}
	applied, failures := second.applyPendingMutations(context.Background(), []PendingMutation{m})
	if len(applied) != 0 || len(failures) != 1 {
		t.Fatalf("post-crash redelivery applied/failures = %v/%v, want interrupted failure", applied, failures)
	}
	if !strings.Contains(failures[0].Error, "interrupted") {
		t.Fatalf("failure %q should name the interruption", failures[0].Error)
	}
	if got := secondCalls.Load(); got != 0 {
		t.Fatalf("post-crash update calls = %d, want 0 (no second swap)", got)
	}
	close(release)
	outcome := waitForHostUpgradeDone(t, first, "up-crash")
	if !outcome.Applied {
		t.Fatalf("first outcome = %+v, want applied", outcome)
	}
	if got := firstCalls.Load(); got != 1 {
		t.Fatalf("first calls = %d, want 1", got)
	}
}

func TestHostUpgradeExpiryAfterPreflightRefusesWithoutSwap(t *testing.T) {
	t.Parallel()
	d := hostUpgradeTestDaemon(t, nil)
	// Hold a spawn reservation so restart preflight waits past consent
	// expiry. This drives the real preflight wait, not a helper.
	d.spawner.mu.Lock()
	d.spawner.spawnReservations["block-expiry"] = struct{}{}
	d.spawner.mu.Unlock()
	t.Cleanup(func() {
		d.spawner.mu.Lock()
		delete(d.spawner.spawnReservations, "block-expiry")
		d.spawner.mu.Unlock()
	})
	var calls atomic.Int32
	d.runPreparedUpdate = func(context.Context, AutoUpdateConfig, string) (*UpdateResult, error) {
		calls.Add(1)
		return &UpdateResult{Updated: true, Version: "8.0.0", Reason: "update-applied"}, nil
	}
	// RFC3339Nano keeps the sub-second margin: second-precision formatting
	// can truncate almost a whole second off the admission margin. A 2s
	// deadline with Nano keeps ~2s of margin while the 3s wait still lands
	// safely past it.
	expiresAt := time.Now().Add(2 * time.Second).UTC().Format(time.RFC3339Nano)
	m := hostUpgradeMutation("up-expiry-preflight", map[string]any{"consented": true, "expiresAt": expiresAt})
	if applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{m}); len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("first applied/failures = %v/%v, want pending (valid at admission)", applied, failures)
	}
	// Wait past expiry while preflight is still blocked, then let preflight
	// proceed. Execution must revalidate and refuse without a swap.
	time.Sleep(3 * time.Second)
	d.spawner.mu.Lock()
	delete(d.spawner.spawnReservations, "block-expiry")
	d.spawner.mu.Unlock()
	outcome := waitForHostUpgradeDone(t, d, "up-expiry-preflight")
	if outcome.Applied {
		t.Fatalf("expired-after-preflight outcome = %+v, want refusal", outcome)
	}
	if !strings.Contains(strings.ToLower(outcome.Error), "expired") {
		t.Fatalf("failure %q should name expiry", outcome.Error)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("update calls = %d, want 0 (no swap after expiry)", got)
	}
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{m})
	if len(applied) != 0 || len(failures) != 1 {
		t.Fatalf("redelivery applied/failures = %v/%v, want failure", applied, failures)
	}
}

func TestHostUpgradeProductionSourceWithConfiguredCDN(t *testing.T) {
	t.Parallel()
	// Production path with no test seam: an operator-configured CDN base
	// serving the current version must report applied (durable
	// no-swap-needed) instead of refusing for a missing source.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "daemon.yaml")
	d := newRestartTestDaemon(t, nil)
	d.opts.ConfigPath = cfgPath
	d.opts.Version = "9.9.9-test"
	current := d.EffectiveVersion()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"version": current, "sha256": "abc", "releasedAt": time.Now().UTC().Format(time.RFC3339),
		})
	}))
	t.Cleanup(srv.Close)
	d.config = &Config{
		AutoUpdate: AutoUpdateConfig{
			Channel: ChannelStable, Schedule: ScheduleManual,
			Signers: []UpdateSigner{{SAN: "test@example.com", Issuer: "https://example.test"}},
			CDNBase: srv.URL,
		},
	}
	d.runPreparedUpdate = nil
	m := hostUpgradeMutation("up-prod-source", map[string]any{"consented": true})
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{m})
	if len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("first applied/failures = %v/%v, want pending", applied, failures)
	}
	outcome := waitForHostUpgradeDone(t, d, "up-prod-source")
	if !outcome.Applied {
		t.Fatalf("production-source outcome = %+v, want applied (already-up-to-date via configured CDN)", outcome)
	}
	if hits.Load() == 0 {
		t.Fatal("configured CDN was never contacted")
	}
	applied, failures = d.applyPendingMutations(context.Background(), []PendingMutation{m})
	if len(applied) != 1 || applied[0] != "up-prod-source" || len(failures) != 0 {
		t.Fatalf("redelivery applied/failures = %v/%v, want applied", applied, failures)
	}
}

func TestHostUpgradeNullDurableRefusesWithoutSwap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "daemon.yaml")
	if err := os.WriteFile(hostUpgradeStatePath(cfgPath), []byte("null"), 0o600); err != nil {
		t.Fatalf("write null state: %v", err)
	}
	d := newRestartTestDaemon(t, nil)
	d.opts.ConfigPath = cfgPath
	d.config = &Config{
		AutoUpdate: AutoUpdateConfig{
			Channel: ChannelStable, Schedule: ScheduleManual,
			Signers: []UpdateSigner{{SAN: "test@example.com", Issuer: "https://example.test"}},
		},
	}
	var calls atomic.Int32
	d.runPreparedUpdate = func(context.Context, AutoUpdateConfig, string) (*UpdateResult, error) {
		calls.Add(1)
		return &UpdateResult{Updated: true, Version: "x", Reason: "update-applied"}, nil
	}
	m := hostUpgradeMutation("up-null", map[string]any{"consented": true})
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{m})
	if len(applied) != 0 || len(failures) != 1 {
		t.Fatalf("null-durable applied/failures = %v/%v, want failure", applied, failures)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("null-durable update calls = %d, want 0", got)
	}
}

func TestHostUpgradeConcurrentLedgerNoLostUpdate(t *testing.T) {
	t.Parallel()
	d := hostUpgradeTestDaemon(t, nil)
	const n = 20
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			id := fmt.Sprintf("up-concurrent-ledger-%02d", i)
			d.finishHostUpgrade(id, hostUpgradeOutcome{
				Applied: false, Error: "busy", CompletedAt: time.Now().UTC().Format(time.RFC3339),
			})
		}(i)
	}
	close(start)
	wg.Wait()
	d.hostUpgradeMu.Lock()
	count := len(d.hostUpgradeDone)
	d.hostUpgradeMu.Unlock()
	if count != n {
		t.Fatalf("in-memory outcomes = %d, want %d (lost update under concurrency)", count, n)
	}
	reloaded, err := loadHostUpgradeOutcomes(hostUpgradeStatePath(d.opts.ConfigPath))
	if err != nil {
		t.Fatalf("reload durable: %v", err)
	}
	if len(reloaded) != n {
		t.Fatalf("durable outcomes = %d, want %d (lost update overwrote ledger)", len(reloaded), n)
	}
}

func TestHostUpgradeExpiryDuringDownloadRefusesWithoutSwap(t *testing.T) {
	t.Parallel()
	d := hostUpgradeTestDaemon(t, nil)
	var calls atomic.Int32
	d.runPreparedUpdate = func(context.Context, AutoUpdateConfig, string) (*UpdateResult, error) {
		calls.Add(1)
		// Ignore context on purpose: a slow download that runs past consent
		// must still refuse via the post-execution expiry revalidation.
		time.Sleep(3 * time.Second)
		return &UpdateResult{Updated: true, Version: "8.8.8", Reason: "update-applied"}, nil
	}
	// RFC3339Nano keeps the sub-second margin: second-precision formatting
	// can truncate almost a whole second off a 1s margin and flake
	// admission. A 2s deadline with Nano keeps ~2s of margin while still
	// expiring before the 3s seam returns.
	expiresAt := time.Now().Add(2 * time.Second).UTC().Format(time.RFC3339Nano)
	m := hostUpgradeMutation("up-expiry-download", map[string]any{"consented": true, "expiresAt": expiresAt})
	if applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{m}); len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("first applied/failures = %v/%v, want pending (valid at admission)", applied, failures)
	}
	outcome := waitForHostUpgradeDone(t, d, "up-expiry-download")
	if outcome.Applied {
		t.Fatalf("expired-during-download outcome = %+v, want refusal", outcome)
	}
	if !strings.Contains(strings.ToLower(outcome.Error), "expired") {
		t.Fatalf("failure %q should name expiry", outcome.Error)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("update calls = %d, want 1 (attempt ran, outcome refused)", got)
	}
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{m})
	if len(applied) != 0 || len(failures) != 1 {
		t.Fatalf("redelivery applied/failures = %v/%v, want failure", applied, failures)
	}
}

func TestUpdaterRunUpdateExpiredContextDoesNotSwap(t *testing.T) {
	t.Parallel()
	// Consent bounds the swap itself: when the bounded context expires
	// during verification, RunUpdate must abort before the irreversible
	// rename instead of swapping and letting the caller report a refusal.
	newBinary := []byte("new-binary-content")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/latest.json"):
			_ = json.NewEncoder(w).Encode(VersionManifest{Version: "9.9.9", SHA256: hexSHA256(newBinary)})
		case strings.HasSuffix(r.URL.Path, ".sigstore"):
			_, _ = w.Write([]byte("test-bundle"))
		default:
			_, _ = w.Write(newBinary)
		}
	}))
	t.Cleanup(srv.Close)
	current := filepath.Join(t.TempDir(), "donmai-daemon")
	if err := os.WriteFile(current, []byte("old-binary-content"), 0o755); err != nil { //nolint:gosec // test scratch binary needs the exec bit, mirroring production chmod 0755
		t.Fatalf("write current binary: %v", err)
	}
	// A verifier that waits past the consent deadline, then reports valid:
	// without a post-verification cancellation check the rename would
	// still run. The expired context must stop it. The deadline is after
	// the fast local download but before verification returns, isolating
	// the post-verification window (an already-expired context would fail
	// earlier at download and never reach the rename).
	deadline := time.Now().Add(300 * time.Millisecond)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	u := NewUpdater(UpdaterOptions{
		CurrentVersion:    "0.1.0",
		CurrentBinaryPath: current,
		Config:            AutoUpdateConfig{Channel: ChannelStable},
		CDNBase:           srv.URL,
		Verifier:          hostUpgradeSleepVerifier{delay: time.Second},
		SkipExit:          true,
	})
	res, err := u.RunUpdate(ctx)
	if err == nil {
		t.Fatalf("RunUpdate with expired context should fail, got %+v", res)
	}
	if res != nil && res.Updated {
		t.Fatalf("expired RunUpdate must not report Updated, got %+v", res)
	}
	swapped, err := os.ReadFile(current)
	if err != nil {
		t.Fatalf("read current binary: %v", err)
	}
	if string(swapped) != "old-binary-content" {
		t.Fatalf("binary swapped after expiry: %q", swapped)
	}
}

func TestHostUpgradeNullLedgerEntryRefusesWithoutSwap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "daemon.yaml")
	// A ledger entry that is JSON null decodes to a zero-valued outcome
	// without an error. It must fail closed instead of letting a new
	// request execute.
	if err := os.WriteFile(hostUpgradeStatePath(cfgPath), []byte(`{"other":null}`), 0o600); err != nil {
		t.Fatalf("write null-entry state: %v", err)
	}
	d := newRestartTestDaemon(t, nil)
	d.opts.ConfigPath = cfgPath
	d.config = &Config{
		AutoUpdate: AutoUpdateConfig{
			Channel: ChannelStable, Schedule: ScheduleManual,
			Signers: []UpdateSigner{{SAN: "test@example.com", Issuer: "https://example.test"}},
		},
	}
	var calls atomic.Int32
	d.runPreparedUpdate = func(context.Context, AutoUpdateConfig, string) (*UpdateResult, error) {
		calls.Add(1)
		return &UpdateResult{Updated: true, Version: "x", Reason: "update-applied"}, nil
	}
	m := hostUpgradeMutation("up-null-entry", map[string]any{"consented": true})
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{m})
	if len(applied) != 0 || len(failures) != 1 {
		t.Fatalf("null-entry applied/failures = %v/%v, want failure", applied, failures)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("null-entry update calls = %d, want 0 (no swap on corrupt ledger)", got)
	}
}

func TestHostUpgradeRetentionBound(t *testing.T) {
	t.Parallel()
	d := hostUpgradeTestDaemon(t, nil)
	d.runPreparedUpdate = func(context.Context, AutoUpdateConfig, string) (*UpdateResult, error) {
		return &UpdateResult{Updated: true, Version: "x", Reason: "update-applied"}, nil
	}
	const total = hostUpgradeMaxRetainedOutcomes + 32
	for i := 0; i < total; i++ {
		m := hostUpgradeMutation(fmt.Sprintf("up-flood-%04d", i), map[string]any{"consented": false})
		applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{m})
		if len(applied) != 0 || len(failures) != 1 {
			t.Fatalf("flood %d applied/failures = %v/%v, want refusal", i, applied, failures)
		}
	}
	d.hostUpgradeMu.Lock()
	retained := len(d.hostUpgradeDone)
	d.hostUpgradeMu.Unlock()
	if retained > hostUpgradeMaxRetainedOutcomes {
		t.Fatalf("retained outcomes = %d, want at most %d", retained, hostUpgradeMaxRetainedOutcomes)
	}
	reloaded, err := loadHostUpgradeOutcomes(hostUpgradeStatePath(d.opts.ConfigPath))
	if err != nil {
		t.Fatalf("reload durable: %v", err)
	}
	if len(reloaded) > hostUpgradeMaxRetainedOutcomes {
		t.Fatalf("durable outcomes = %d, want at most %d", len(reloaded), hostUpgradeMaxRetainedOutcomes)
	}
}

func TestHostUpgradeDirectoryNotSyncableRefusesWithoutSwap(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("directory permission probe requires a non-root user")
	}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "daemon.yaml")
	stateDir := filepath.Dir(hostUpgradeStatePath(cfgPath))
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	if err := os.Chmod(stateDir, 0o300); err != nil { //nolint:gosec // intentional write-only probe: missing read perm fails dir-sync while rename still succeeds
		t.Fatalf("chmod state dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(stateDir, 0o750) //nolint:gosec // G302: restore the 0750 test-dir mode so TempDir cleanup can traverse it
	})
	d := newRestartTestDaemon(t, nil)
	d.opts.ConfigPath = cfgPath
	d.config = &Config{
		AutoUpdate: AutoUpdateConfig{
			Channel: ChannelStable, Schedule: ScheduleManual,
			Signers: []UpdateSigner{{SAN: "test@example.com", Issuer: "https://example.test"}},
		},
	}
	var calls atomic.Int32
	d.runPreparedUpdate = func(context.Context, AutoUpdateConfig, string) (*UpdateResult, error) {
		calls.Add(1)
		return &UpdateResult{Updated: true, Version: "x", Reason: "update-applied"}, nil
	}
	m := hostUpgradeMutation("up-dir-sync", map[string]any{"consented": true})
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{m})
	if len(applied) != 0 || len(failures) != 1 {
		t.Fatalf("dir-sync applied/failures = %v/%v, want failure", applied, failures)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("dir-sync update calls = %d, want 0 (no swap without durable dir)", got)
	}
}

type hostUpgradeSleepVerifier struct{ delay time.Duration }

func (v hostUpgradeSleepVerifier) Verify(_ context.Context, _, _ string) (bool, string) {
	time.Sleep(v.delay)
	return true, "test-sleep-verifier"
}

func TestHostUpgradeRetentionPreservesAppliedAcrossFlood(t *testing.T) {
	t.Parallel()
	d := hostUpgradeTestDaemon(t, nil)
	var calls atomic.Int32
	d.runPreparedUpdate = func(context.Context, AutoUpdateConfig, string) (*UpdateResult, error) {
		calls.Add(1)
		return &UpdateResult{Updated: true, Version: "2.0.0", Reason: "update-applied"}, nil
	}
	m := hostUpgradeMutation("up-keep-success", map[string]any{"consented": true})
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{m})
	if len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("first delivery applied/failures = %v/%v, want pending", applied, failures)
	}
	waitForHostUpgradeDone(t, d, "up-keep-success")
	if got := calls.Load(); got != 1 {
		t.Fatalf("update calls = %d, want 1", got)
	}
	// Age the success past the second-precision CompletedAt so a
	// count-based pruner would treat it as the oldest entry: this mirrors
	// the probe, where the completed receipt grows older before the flood.
	time.Sleep(1100 * time.Millisecond)
	for i := 0; i < hostUpgradeMaxRetainedOutcomes; i++ {
		flood := hostUpgradeMutation(fmt.Sprintf("up-flood-%04d", i), map[string]any{"consented": false})
		applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{flood})
		if len(applied) != 0 || len(failures) != 1 {
			t.Fatalf("flood %d applied/failures = %v/%v, want refusal", i, applied, failures)
		}
	}
	applied, failures = d.applyPendingMutations(context.Background(), []PendingMutation{m})
	if len(applied) != 1 || applied[0] != "up-keep-success" || len(failures) != 0 {
		t.Fatalf("redelivery applied/failures = %v/%v, want applied", applied, failures)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("update calls after flood redelivery = %d, want 1 (no second swap)", got)
	}
	reloaded, err := loadHostUpgradeOutcomes(hostUpgradeStatePath(d.opts.ConfigPath))
	if err != nil {
		t.Fatalf("reload durable: %v", err)
	}
	outcome, ok := reloaded["up-keep-success"]
	if !ok || !outcome.Applied {
		t.Fatalf("durable receipt for original = %+v present=%v, want applied", outcome, ok)
	}
}

func TestHostUpgradeRetentionPreservesBusyRefusalAcrossFlood(t *testing.T) {
	t.Parallel()
	d := hostUpgradeTestDaemon(t, nil)
	lease, ok := d.tryClaimLifecycle(lifecycleUpdate)
	if !ok {
		t.Fatal("could not claim lifecycle for manual holder")
	}
	var calls atomic.Int32
	d.runPreparedUpdate = func(context.Context, AutoUpdateConfig, string) (*UpdateResult, error) {
		calls.Add(1)
		return &UpdateResult{Updated: true, Version: "x", Reason: "update-applied"}, nil
	}
	m := hostUpgradeMutation("up-busy-keep", map[string]any{"consented": true})
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{m})
	if len(applied) != 0 || len(failures) != 1 {
		t.Fatalf("busy applied/failures = %v/%v, want refusal", applied, failures)
	}
	// Age the busy receipt past the second-precision CompletedAt so a
	// count-based pruner would treat it as the oldest entry.
	time.Sleep(1100 * time.Millisecond)
	for i := 0; i < hostUpgradeMaxRetainedOutcomes; i++ {
		flood := hostUpgradeMutation(fmt.Sprintf("up-bflood-%04d", i), map[string]any{"consented": false})
		applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{flood})
		if len(applied) != 0 || len(failures) != 1 {
			t.Fatalf("flood %d applied/failures = %v/%v, want refusal", i, applied, failures)
		}
	}
	// Release contention before redelivery: a terminal busy refusal must
	// replay even when the host is free again. Under a count-based pruner
	// the evicted receipt would re-validate to a fresh execution instead.
	d.releaseLifecycle(lease)
	applied, failures = d.applyPendingMutations(context.Background(), []PendingMutation{m})
	if len(applied) != 0 || len(failures) != 1 || failures[0].ID != "up-busy-keep" {
		t.Fatalf("redelivery applied/failures = %v/%v, want replayed busy refusal", applied, failures)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("update calls = %d, want 0 (refused request must never execute)", got)
	}
}

func TestHostUpgradeTwoRequestsSameHeartbeat(t *testing.T) {
	t.Parallel()
	d := hostUpgradeTestDaemon(t, nil)
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	var once sync.Once
	d.runPreparedUpdate = func(ctx context.Context, _ AutoUpdateConfig, _ string) (*UpdateResult, error) {
		calls.Add(1)
		once.Do(func() { close(entered) })
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &UpdateResult{Updated: true, Version: "3.0.0", Reason: "update-applied"}, nil
	}
	first := hostUpgradeMutation("up-same-a", map[string]any{"consented": true})
	second := hostUpgradeMutation("up-same-b", map[string]any{"consented": true})
	// One heartbeat carrying two requests: the first is admitted pending and
	// the second must refuse immediately from the in-flight admission guard,
	// before either background attempt claims the lifecycle lease.
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{first, second})
	if len(applied) != 0 {
		t.Fatalf("same-beat applied = %v, want none yet", applied)
	}
	if len(failures) != 1 || failures[0].ID != "up-same-b" {
		t.Fatalf("same-beat failures = %v, want immediate refusal for second", failures)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first upgrade did not start")
	}
	close(release)
	waitForHostUpgradeDone(t, d, "up-same-a")
	applied, failures = d.applyPendingMutations(context.Background(), []PendingMutation{second})
	if len(applied) != 0 || len(failures) != 1 || failures[0].ID != "up-same-b" {
		t.Fatalf("post-contention redelivery applied/failures = %v/%v, want replayed refusal", applied, failures)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("update calls = %d, want 1 (no second swap)", got)
	}
}

func TestPruneHostUpgradeLedgerKeepsActionableReceipts(t *testing.T) {
	t.Parallel()
	now := time.Now()
	actionable := now.Add(time.Hour).UTC().Format(time.RFC3339Nano)
	expired := now.Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	completed := now.UTC().Format(time.RFC3339Nano)
	outcomes := map[string]hostUpgradeOutcome{
		"applied": {Applied: true, CompletedAt: completed, ConsentUntil: actionable},
		"busy":    {Applied: false, Error: "host.upgrade already in progress", CompletedAt: completed, ConsentUntil: actionable},
		"old":     {Applied: false, Error: "host.upgrade refused: stale", CompletedAt: completed, Evictable: true, ConsentUntil: expired},
	}
	for i := 0; i < hostUpgradeMaxRetainedOutcomes; i++ {
		outcomes[fmt.Sprintf("refuse-%04d", i)] = hostUpgradeOutcome{
			Applied: false, Error: "host.upgrade requires consented:true",
			CompletedAt: completed, Evictable: true,
		}
	}
	pruneHostUpgradeLedgerAt(outcomes, now)
	if _, ok := outcomes["applied"]; !ok {
		t.Fatal("applied receipt evicted while consent actionable")
	}
	if _, ok := outcomes["busy"]; !ok {
		t.Fatal("busy receipt evicted while consent actionable")
	}
	if len(outcomes) > hostUpgradeMaxRetainedOutcomes+2 {
		t.Fatalf("ledger size = %d, want bounded", len(outcomes))
	}
	// Expired non-evictable receipts may be pruned: redelivery refuses via expiry anyway.
	expiredOutcomes := map[string]hostUpgradeOutcome{
		"applied-expired": {Applied: true, CompletedAt: completed, ConsentUntil: expired},
	}
	for i := 0; i < hostUpgradeMaxRetainedOutcomes; i++ {
		expiredOutcomes[fmt.Sprintf("refuse-%04d", i)] = hostUpgradeOutcome{
			Applied: false, Error: "host.upgrade requires consented:true",
			CompletedAt: completed, Evictable: true,
		}
	}
	pruneHostUpgradeLedgerAt(expiredOutcomes, now)
	if _, ok := expiredOutcomes["applied-expired"]; ok && len(expiredOutcomes) > hostUpgradeMaxRetainedOutcomes {
		t.Fatal("expired receipt should be evictable to enforce the bound")
	}
}

func TestHostUpgradeOptInFloodStaysBounded(t *testing.T) {
	t.Parallel()
	d := hostUpgradeTestDaemon(t, nil)
	d.config.AutoUpdate.Signers = nil
	var calls atomic.Int32
	d.runPreparedUpdate = func(context.Context, AutoUpdateConfig, string) (*UpdateResult, error) {
		calls.Add(1)
		return &UpdateResult{Updated: true, Version: "x", Reason: "update-applied"}, nil
	}
	timestampLess := func(id string) PendingMutation {
		raw, _ := json.Marshal(map[string]any{"consented": true})
		return PendingMutation{ID: id, Op: hostUpgradeOp, Params: raw, RequestedBy: "tester"}
	}
	const total = hostUpgradeMaxRetainedOutcomes + 32
	for i := 0; i < total; i++ {
		m := timestampLess(fmt.Sprintf("up-optin-flood-%04d", i))
		applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{m})
		if len(applied) != 0 || len(failures) != 1 {
			t.Fatalf("flood %d applied/failures = %v/%v, want refusal", i, applied, failures)
		}
		if !strings.Contains(failures[0].Error, "consent") {
			t.Fatalf("flood %d failure %q does not name machine-owner consent", i, failures[0].Error)
		}
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("update calls = %d, want 0 (opt-in refusals must never execute)", got)
	}
	d.hostUpgradeMu.Lock()
	retained := len(d.hostUpgradeDone)
	_, evicted := d.hostUpgradeDone["up-optin-flood-0000"]
	d.hostUpgradeMu.Unlock()
	if retained > hostUpgradeMaxRetainedOutcomes {
		t.Fatalf("retained outcomes = %d, want at most %d", retained, hostUpgradeMaxRetainedOutcomes)
	}
	if evicted {
		t.Fatal("oldest opt-in refusal should have been evicted to enforce the bound")
	}
	reloaded, err := loadHostUpgradeOutcomes(hostUpgradeStatePath(d.opts.ConfigPath))
	if err != nil {
		t.Fatalf("reload durable: %v", err)
	}
	if len(reloaded) > hostUpgradeMaxRetainedOutcomes {
		t.Fatalf("durable outcomes = %d, want at most %d", len(reloaded), hostUpgradeMaxRetainedOutcomes)
	}
	for id, outcome := range reloaded {
		if !outcome.Evictable {
			t.Fatalf("retained opt-in refusal %q is not evictable", id)
		}
	}
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{timestampLess("up-optin-flood-0000")})
	if len(applied) != 0 || len(failures) != 1 || !strings.Contains(failures[0].Error, "consent") {
		t.Fatalf("evicted redelivery applied/failures = %v/%v, want consent refusal", applied, failures)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("update calls after evicted redelivery = %d, want 0", got)
	}
}

func TestHostUpgradeNoSourceFloodStaysBounded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "daemon.yaml")
	d := newRestartTestDaemon(t, nil)
	d.opts.ConfigPath = cfgPath
	d.config = &Config{
		AutoUpdate: AutoUpdateConfig{
			Channel: ChannelStable, Schedule: ScheduleManual,
			Signers: []UpdateSigner{{SAN: "test@example.com", Issuer: "https://example.test"}},
		},
	}
	d.runPreparedUpdate = nil
	timestampLess := func(id string) PendingMutation {
		raw, _ := json.Marshal(map[string]any{"consented": true})
		return PendingMutation{ID: id, Op: hostUpgradeOp, Params: raw, RequestedBy: "tester"}
	}
	const total = hostUpgradeMaxRetainedOutcomes + 32
	for i := 0; i < total; i++ {
		m := timestampLess(fmt.Sprintf("up-nosrc-flood-%04d", i))
		applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{m})
		if len(applied) != 0 || len(failures) != 1 {
			t.Fatalf("flood %d applied/failures = %v/%v, want refusal", i, applied, failures)
		}
		if !strings.Contains(failures[0].Error, "source") {
			t.Fatalf("flood %d failure %q does not name the missing update source", i, failures[0].Error)
		}
	}
	d.hostUpgradeMu.Lock()
	retained := len(d.hostUpgradeDone)
	_, evicted := d.hostUpgradeDone["up-nosrc-flood-0000"]
	d.hostUpgradeMu.Unlock()
	if retained > hostUpgradeMaxRetainedOutcomes {
		t.Fatalf("retained outcomes = %d, want at most %d", retained, hostUpgradeMaxRetainedOutcomes)
	}
	if evicted {
		t.Fatal("oldest no-source refusal should have been evicted to enforce the bound")
	}
	reloaded, err := loadHostUpgradeOutcomes(hostUpgradeStatePath(cfgPath))
	if err != nil {
		t.Fatalf("reload durable: %v", err)
	}
	if len(reloaded) > hostUpgradeMaxRetainedOutcomes {
		t.Fatalf("durable outcomes = %d, want at most %d", len(reloaded), hostUpgradeMaxRetainedOutcomes)
	}
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{timestampLess("up-nosrc-flood-0000")})
	if len(applied) != 0 || len(failures) != 1 || !strings.Contains(failures[0].Error, "source") {
		t.Fatalf("evicted redelivery applied/failures = %v/%v, want source refusal", applied, failures)
	}
}

func TestHostUpgradeDurableBrokenRefusesNewID(t *testing.T) {
	t.Parallel()
	d := hostUpgradeTestDaemon(t, nil)
	var calls atomic.Int32
	d.runPreparedUpdate = func(context.Context, AutoUpdateConfig, string) (*UpdateResult, error) {
		calls.Add(1)
		return &UpdateResult{Updated: true, Version: "x", Reason: "update-applied"}, nil
	}
	stateDir := filepath.Dir(d.opts.ConfigPath)
	first := hostUpgradeMutation("up-broken-first", map[string]any{"consented": true})
	if applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{first}); len(applied) != 0 || len(failures) != 0 {
		t.Fatalf("first applied/failures = %v/%v, want pending", applied, failures)
	}
	_ = os.RemoveAll(stateDir)
	waitForHostUpgradeDone(t, d, "up-broken-first")
	d.hostUpgradeMu.Lock()
	broken := d.hostUpgradeDurableBroken
	d.hostUpgradeMu.Unlock()
	if !broken {
		t.Fatal("durability should be marked broken after persist failure")
	}
	if err := os.MkdirAll(stateDir, 0o750); err != nil { //nolint:gosec // G301: test-dir mode matches repo helper dirs
		t.Fatalf("restore state dir: %v", err)
	}
	second := hostUpgradeMutation("up-broken-new", map[string]any{"consented": true})
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{second})
	if len(applied) != 0 || len(failures) != 1 {
		t.Fatalf("sticky-broken applied/failures = %v/%v, want refusal for new id", applied, failures)
	}
	if !strings.Contains(strings.ToLower(failures[0].Error), "durable") {
		t.Fatalf("sticky-broken failure %q does not name durable state", failures[0].Error)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("update calls = %d, want 1 (new id must not execute after breakage)", got)
	}
}
