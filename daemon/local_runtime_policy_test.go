package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/internal/localqueue"
)

func TestLocalAppliedPolicyTighteningPreservesPendingAuthority(t *testing.T) {
	t.Parallel()
	var secrets atomic.Int32
	f := startLocalHTTPFixture(t, func(d *Daemon) {
		d.opts.SpawnerOptions.OnPreSpawn = func(_ SessionSpec, env []string) ([]string, error) { secrets.Add(1); return env, nil }
	})
	admitted := f.admit(t, 77)
	ctx := context.Background()
	original, err := f.runtime.store.Session(ctx, f.runtime.identity.ScopeID, admitted.Session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	revision := f.runtime.store.CurrentRevision()
	cfg := f.d.Config()
	cfg.Capacity.MaxConcurrentSessions = 1
	cfg.LocalRuntime.ExecutionSecurity.Network = agent.NetworkNone
	if err = WriteConfig(f.d.opts.ConfigPath, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(f.d.opts.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	f.d.onYamlChanged(loaded)
	if err = f.runtime.checkCurrentConfiguration(); err != nil {
		t.Fatal(err)
	}
	if err = f.runtime.dispatch(ctx, original.Admission, f.runtime.source); agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityUnmet {
		t.Fatalf("stale minimum refusal=%v", err)
	}
	after, err := f.runtime.store.Session(ctx, f.runtime.identity.ScopeID, admitted.Session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Attempt != nil || after.Admission.DispatchState != localqueue.DispatchPending || f.runtime.store.CurrentRevision() != revision {
		t.Fatal("policy refusal claimed or rewrote pending authority")
	}
	if !bytes.Equal(original.Admission.Envelope.OperationalPayload, after.Admission.Envelope.OperationalPayload) || !bytes.Equal(original.Admission.Envelope.Receipt, after.Admission.Envelope.Receipt) {
		t.Fatal("policy tightening silently restamped admission")
	}
	if secrets.Load() != 0 || f.d.spawner.ActiveCount() != 0 {
		t.Fatal("policy tightening reached credentials or spawned a child")
	}
	entries, err := os.ReadDir(filepath.Join(f.runtime.options.AuthRoot, "attempts"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("policy refusal minted an attempt credential")
	}
}

func TestLocalPolicyMinimumAllowsEqualAndStrongerOnly(t *testing.T) {
	t.Parallel()
	weak := InitialLocalExecutionSecurity().Levels()
	raw, err := json.Marshal(weak)
	if err != nil {
		t.Fatal(err)
	}
	for _, dimension := range agent.ExecutionSecurityDimensions() {
		t.Run(string(dimension), func(t *testing.T) {
			t.Parallel()
			var fields map[string]agent.ExecutionSecurityLevel
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			ladder := agent.ExecutionSecurityLadder(dimension)
			fields[string(dimension)] = ladder[len(ladder)-1]
			encoded, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			var strong agent.ExecutionSecurityLevels
			if err = json.Unmarshal(encoded, &strong); err != nil {
				t.Fatal(err)
			}
			if err = localAdmissionMeetsPolicy(weak, weak); err != nil {
				t.Fatal("equal stamp refused", err)
			}
			if err = localAdmissionMeetsPolicy(strong, weak); err != nil {
				t.Fatal("stronger stamp refused", err)
			}
			if err = localAdmissionMeetsPolicy(weak, strong); agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityUnmet {
				t.Fatal("weaker dimension admitted", err)
			}
		})
	}
}

func TestLocalPolicyPublicationWaitsForOwnedLaunchAndPublicCannotBorrowPermit(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	f := startLocalHTTPFixture(t, func(d *Daemon) {
		d.opts.SpawnerOptions.OnPreSpawn = func(_ SessionSpec, env []string) ([]string, error) { close(entered); <-release; return env, nil }
	})
	t.Cleanup(unblock)
	admitted := f.admit(t, 78)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.d.spawner.SetMaxConcurrentSessions(1); err != nil {
		t.Fatal(err)
	}
	original, err := f.runtime.store.Session(ctx, f.runtime.identity.ScopeID, admitted.Session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	launched := make(chan error, 1)
	go func() { launched <- f.runtime.dispatch(ctx, original.Admission, f.runtime.source) }()
	select {
	case <-entered:
	case err = <-launched:
		t.Fatalf("launch did not reach held credential hook: %v", err)
	case <-ctx.Done():
		t.Fatal("credential hook did not begin")
	}
	projection, err := f.runtime.store.Session(ctx, f.runtime.identity.ScopeID, admitted.Session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	detail, err := f.runtime.detail(projection)
	if err != nil {
		t.Fatal(err)
	}
	var item PollWorkItem
	if err = json.Unmarshal(projection.Admission.Envelope.OperationalPayload, &item); err != nil {
		t.Fatal(err)
	}
	item.SessionID = admitted.Session.SessionID
	spec := PollItemToSessionSpec(item, f.d.Config().EffectiveProjectConfigs())
	spec.OrganizationID = f.runtime.identity.ScopeID
	if _, err = f.d.AcceptWorkWithDetail(spec, detail); err == nil || !strings.Contains(err.Error(), "permit") {
		t.Fatal("public library call borrowed an in-flight launch permit")
	}
	forged := &localLaunchPermit{attempt: *projection.Attempt}
	if _, err = f.d.acceptWorkWithDetail(spec, detail, forged); err == nil || !strings.Contains(err.Error(), "permit") {
		t.Fatal("forged pointer borrowed an exact attempt permit")
	}
	cfg := f.d.Config()
	cfg.LocalRuntime.ExecutionSecurity.Network = agent.NetworkNone
	if err = WriteConfig(f.d.opts.ConfigPath, cfg); err != nil {
		t.Fatal(err)
	}
	applied := make(chan struct{})
	go func() { f.d.onYamlChanged(cfg); close(applied) }()
	select {
	case <-applied:
		t.Fatal("new own policy published while the previous launch/credential lease was active")
	case <-time.After(50 * time.Millisecond):
	}
	unblock()
	select {
	case err = <-launched:
		if err == nil {
			t.Fatal("changed disk policy did not hold the pre-spawn attempt")
		}
	case <-ctx.Done():
		t.Fatal("held launch did not finish")
	}
	select {
	case <-applied:
	case <-ctx.Done():
		t.Fatal("policy update did not finish after launch lease returned")
	}
	if f.d.Config().LocalRuntime.ExecutionSecurity.Network != agent.NetworkNone {
		t.Fatal("new policy was not published")
	}
	if f.d.spawner.ActiveCount() != 0 {
		t.Fatal("held policy change spawned a process")
	}
	f.runtime.mu.Lock()
	permits := len(f.runtime.launchPermits)
	f.runtime.mu.Unlock()
	if permits != 0 {
		t.Fatal("failed launch retained a private permit")
	}
	after, err := f.runtime.store.Session(ctx, f.runtime.identity.ScopeID, admitted.Session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Attempt == nil || after.Terminal != nil || after.Admission.DispatchState != localqueue.DispatchUnknown || !bytes.Equal(original.Admission.Envelope.OperationalPayload, after.Admission.Envelope.OperationalPayload) {
		t.Fatal("held in-flight authority was rewritten or requeued")
	}
}

func TestLocalIntakeHoldsOldSourceDuringAppliedRepositoryReload(t *testing.T) {
	t.Parallel()
	replacement := make(chan struct{})
	var once sync.Once
	f := startLocalHTTPFixture(t, func(d *Daemon) {
		d.opts.LocalRuntime.NewSource = func(repositories []LocalGitHubRepository) (LocalRuntimeSource, error) {
			if len(repositories) > 0 && repositories[0].RepositoryID == 99 {
				once.Do(func() { close(replacement) })
			}
			return &localHTTPTestSource{}, nil
		}
	})
	_, release, err := f.runtime.sourceLease()
	if err != nil {
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	releaseSource := func() { releaseOnce.Do(release) }
	t.Cleanup(releaseSource)
	cfg := f.d.Config()
	cfg.LocalRuntime.Repositories = []LocalGitHubRepository{{RepositoryID: 99, OwnerRepo: "other/project", Label: "donmai"}}
	if err = WriteConfig(f.d.opts.ConfigPath, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(f.d.opts.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	reload := make(chan struct{})
	go func() { f.d.onYamlChanged(loaded); close(reload) }()
	select {
	case <-replacement:
	case <-time.After(5 * time.Second):
		t.Fatal("repository policy did not publish before waiting for old source")
	}
	revision := f.runtime.store.CurrentRevision()
	request := LocalIntakeRequest{RepositoryID: 42, OwnerRepo: "example/project", IssueNumber: 79, IssueURL: "https://github.com/example/project/issues/79", Title: "Old source observation", Body: "Implement the authored change."}
	if _, err = f.runtime.admitIntake(context.Background(), request); err == nil {
		t.Fatal("old source admitted work after current repository authority changed")
	}
	if f.runtime.store.CurrentRevision() != revision {
		t.Fatal("obsolete-source refusal created a durable admission")
	}
	releaseSource()
	select {
	case <-reload:
	case <-time.After(5 * time.Second):
		t.Fatal("source reload did not complete after old observation released")
	}
}
