package daemon

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
)

func copyLocalWorkerFixture(t *testing.T, source, destination string) {
	t.Helper()
	input, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close() }()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(output, input)
	closeErr := output.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if err = syscall.Chmod(destination, 0o500); err != nil {
		t.Fatal(err)
	}
}

func testLocalWorkerReplacementRefusesBeforeClaim(t *testing.T, replacement string) {
	t.Helper()
	worker := filepath.Join(t.TempDir(), "worker")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	copyLocalWorkerFixture(t, self, worker)
	var hooks atomic.Int32
	f := startLocalHTTPFixture(t, func(d *Daemon) {
		d.opts.SpawnerOptions.WorkerCommand = []string{worker, "-test.run=^TestLocalNoResultWorkerHelper$", "--"}
		d.opts.SpawnerOptions.OnPreSpawn = func(_ SessionSpec, env []string) ([]string, error) { hooks.Add(1); return env, nil }
	})
	admitted := f.admit(t, 31)
	before := f.runtime.store.CurrentRevision()
	// Replace only the owned executable copy. Never rewrite the running test
	// executable or any installed binary. Periodic intake remains disabled;
	// open one actual spawner slot so capacity cannot mask a coupling failure.
	if err = f.d.spawner.SetMaxConcurrentSessions(1); err != nil {
		t.Fatal(err)
	}
	if err = syscall.Chmod(worker, 0o600); err != nil {
		t.Fatal(err)
	}
	copyLocalWorkerFixture(t, replacement, worker)
	projection, err := f.runtime.store.Session(context.Background(), f.runtime.identity.ScopeID, admitted.Session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.runtime.dispatch(context.Background(), projection.Admission, f.runtime.source); err == nil || !strings.Contains(err.Error(), "same executable artifact") {
		t.Fatalf("uncoupled worker refusal=%v", err)
	}
	after, err := f.runtime.store.Session(context.Background(), f.runtime.identity.ScopeID, admitted.Session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Attempt != nil || f.runtime.store.CurrentRevision() != before || hooks.Load() != 0 || f.d.spawner.ActiveCount() != 0 {
		t.Fatal("incompatible worker reached claim, credential hook, or spawn")
	}
	entries, err := os.ReadDir(filepath.Join(f.runtime.options.AuthRoot, "attempts"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("incompatible worker caused an attempt key")
	}
}

func TestLocalWorkerArtifactChangeRefusesBeforeClaim(t *testing.T) {
	t.Parallel()
	replacement := filepath.Join(t.TempDir(), "incompatible")
	if err := os.WriteFile(replacement, []byte("#!/bin/sh\nexit 91\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	testLocalWorkerReplacementRefusesBeforeClaim(t, replacement)
}
