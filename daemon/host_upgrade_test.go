package daemon

import (
	"context"
	"encoding/json"
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
}

func TestHostUpgradeManualLifecycleContention(t *testing.T) {
	t.Parallel()
	d := hostUpgradeTestDaemon(t, nil)
	lease, ok := d.tryClaimLifecycle(lifecycleUpdate)
	if !ok {
		t.Fatal("could not claim lifecycle for manual holder")
	}
	defer d.releaseLifecycle(lease)
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
	d.runPreparedUpdate = func(ctx context.Context, _ AutoUpdateConfig, _ string) (*UpdateResult, error) {
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
	// The next beat must still progress while the update is delayed: a config
	// mutation applies without waiting on the upgrade lease.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = d.applyPendingMutations(context.Background(), []PendingMutation{
			{ID: "up-slow-dup", Op: hostUpgradeOp, Params: hostUpgradeMutation("up-slow", map[string]any{"consented": true}).Params},
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("heartbeat blocked behind delayed update")
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
	close(release)
	waitForHostUpgradeDone(t, d, "up-slow")
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
	d.cancelHostUpgrades()
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
