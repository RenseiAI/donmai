package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/sessionshim"
)

func TestCheckIsNonMutatingAndRequiresExactPaths(t *testing.T) {
	dir := t.TempDir()
	candidate := filepath.Join(dir, "candidate")
	writeTestExecutable(t, candidate, "#!/bin/sh\n# DONMAI_SESSION_SHIM_ACCEPTANCE_TOKEN_FILE /api/daemon/session-shim/acceptance/ restart_fence_refused\nexit 0\n")
	stateDir := filepath.Join(dir, "state")
	cfg := config{
		stateDir:  stateDir,
		registry:  filepath.Join(dir, "registry"),
		daemonURL: "http://127.0.0.1:7734",
		candidate: candidate,
		tokenFile: filepath.Join(stateDir, "control-token"),
	}
	if err := cfg.checkForOS("linux"); err != nil {
		t.Fatalf("check: %v", err)
	}
	if pathExists(stateDir) {
		t.Fatal("non-mutating check created the acceptance state directory")
	}
	bad := cfg
	bad.tokenFile = filepath.Join(dir, "outside-token")
	if err := bad.checkForOS("linux"); err == nil {
		t.Fatal("check accepted a token path outside the exact state directory")
	}

	wrongCandidate := filepath.Join(dir, "wrong-candidate")
	writeTestExecutable(t, wrongCandidate, "#!/bin/sh\n# /api/daemon/session-shim/acceptance/\nexit 0\n")
	bad = cfg
	bad.candidate = wrongCandidate
	if err := bad.checkForOS("linux"); err == nil {
		t.Fatal("check accepted a candidate without the typed fence-refusal behavior")
	}
}

func TestControlSendsBearerAndExactCorrelation(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte("01234567890123456789012345678901\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != controlPrefix+"fence-refuse-arm" || r.Header.Get("Authorization") != "Bearer 01234567890123456789012345678901" {
			http.NotFound(w, r)
			return
		}
		var body controlRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.OrgID != "org_1" || body.SessionID != "sess_1" {
			http.Error(w, "wrong correlation", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	cfg := config{daemonURL: server.URL, tokenFile: tokenPath}
	if err := cfg.control("fence-refuse-arm", controlRequest{OrgID: "org_1", SessionID: "sess_1"}); err != nil {
		t.Fatal(err)
	}
}

func TestPreparePublishesPrivateTokenThroughServiceManager(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	manager := "systemctl"
	if runtime.GOOS == "darwin" {
		manager = "launchctl"
	}
	managerPath := filepath.Join(binDir, manager)
	writeTestExecutable(t, managerPath, "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	candidate := filepath.Join(dir, "candidate")
	writeTestExecutable(t, candidate, "#!/bin/sh\n# DONMAI_SESSION_SHIM_ACCEPTANCE_TOKEN_FILE /api/daemon/session-shim/acceptance/ restart_fence_refused\nexit 0\n")
	stateDir := filepath.Join(dir, "state")
	cfg := config{
		stateDir: stateDir, registry: filepath.Join(dir, "registry"),
		daemonURL: "http://127.0.0.1:7734", candidate: candidate,
		tokenFile: filepath.Join(stateDir, "control-token"),
	}
	if err := cfg.prepareForOS("linux"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(cfg.tokenFile)
	if err != nil || info.Mode().Perm() != 0o600 || info.Size() < 32 {
		t.Fatalf("prepared token info = %+v, %v", info, err)
	}
	loaded, err := cfg.loadState()
	if err != nil || loaded.SchemaVersion != stateSchema {
		t.Fatalf("prepared state = %+v, %v", loaded, err)
	}
}

func writeTestExecutable(t *testing.T, path, content string) {
	t.Helper()
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	name := filepath.Base(path)
	if err := root.WriteFile(name, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := root.Chmod(name, 0o700); err != nil {
		t.Fatal(err)
	}
}

// TestPublishHelperTombstoneProvesTheReapItClaims is the discriminating test
// for the helper's terminal half. The helper is a real mini-shim lineage: it
// owns a real child process group, and on the way out it reaps that group and
// publishes a REAL tombstone through the registry.
//
// GroupReaped is the field that matters, because §D10 lets a group-reaped
// tombstone release a claim. It must be the answer to a live liveness probe,
// not a constant — so the negative case pins a tombstone published while the
// recorded harness identity is demonstrably still running.
func TestPublishHelperTombstoneProvesTheReapItClaims(t *testing.T) {
	record := sessionshim.Record{
		SchemaVersion: sessionshim.RecordSchemaVersion,
		OrgID:         "org_helper", SessionID: "sess_helper",
		ShimID: "shim-helper", ProcessEpoch: 3,
	}
	for _, tc := range []struct {
		name string
		// liveIdentity substitutes a process identity that is still running for
		// the reaped child's, without changing anything else.
		liveIdentity bool
		wantReaped   bool
	}{
		{name: "reaped group is proven", wantReaped: true},
		{name: "live identity is never claimed as reaped", liveIdentity: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			registry, err := sessionshim.NewRegistry(dir)
			if err != nil {
				t.Fatal(err)
			}
			harness := exec.Command("sleep", helperHarnessLifetime)
			harness.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := harness.Start(); err != nil {
				t.Fatal(err)
			}
			identity, err := sessionshim.ProcessIdentityFor(harness.Process.Pid)
			if err != nil {
				_ = harness.Process.Kill()
				t.Fatal(err)
			}
			if tc.liveIdentity {
				// This test process: recorded as the harness, still running when
				// the tombstone is written.
				if identity, err = sessionshim.Self(); err != nil {
					t.Fatal(err)
				}
			}

			if err := publishHelperTombstone(registry, record, harness, identity); err != nil {
				t.Fatalf("publish: %v", err)
			}

			got, err := registry.GetTombstoneIncarnation(record.Identity(), record.ShimID, record.ProcessEpoch)
			if err != nil {
				t.Fatalf("the helper published no durable tombstone: %v", err)
			}
			if got.GroupReaped != tc.wantReaped {
				t.Fatalf("groupReaped = %t, want %t — a tombstone that claims a reap it did not "+
					"verify is worse than no tombstone, because a proven one releases a claim",
					got.GroupReaped, tc.wantReaped)
			}
			if got.HarnessPID != identity.PID || got.HarnessStartedAt != identity.StartedAt {
				t.Fatalf("tombstone harness identity = pid %d start %d, want the pinned %+v",
					got.HarnessPID, got.HarnessStartedAt, identity)
			}
			if got.ObservedAtUnixNano <= 0 || got.SchemaVersion != sessionshim.RecordSchemaVersion {
				t.Fatalf("tombstone is not a valid terminal observation: %+v", got)
			}
			if !tc.liveIdentity {
				// The reaped child died by the helper's own SIGTERM, so the §12.2
				// outcome is signal death — measured from the wait status, never
				// assumed.
				if got.Signal != "SIGTERM" || got.ExitCode != attachwire.ExitCodeForSignal(int(syscall.SIGTERM)) {
					t.Fatalf("tombstone outcome = code %d signal %q, want the reaping signal's exact outcome",
						got.ExitCode, got.Signal)
				}
			}
			if got.LastSeq != 0 {
				t.Fatalf("tombstone lastSeq = %d, want 0 — this lineage allocates no host output sequence", got.LastSeq)
			}
		})
	}
}

// The helper withdraws its liveness claim the way every real lineage does:
// PutTombstone publishes the proof and THEN removes the exact discovery
// record. A helper that deleted its record without leaving evidence is what
// made the acceptance clear unable to report anything.
func TestPublishHelperTombstoneWithdrawsTheDiscoveryRecord(t *testing.T) {
	dir := t.TempDir()
	registry, err := sessionshim.NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	record := sessionshim.Record{
		SchemaVersion: sessionshim.RecordSchemaVersion,
		OrgID:         "org_helper", SessionID: "sess_helper",
		ShimID: "shim-helper", ProcessEpoch: 3,
		PID: os.Getpid(), ProcessStartedAt: 1,
		SocketPath:  filepath.Join(dir, "helper.sock"),
		ProtocolMin: 99, ProtocolMax: 99,
		Phase:             "running",
		CreatedAtUnixNano: 1,
	}
	if err := registry.Put(record); err != nil {
		t.Fatal(err)
	}
	harness := exec.Command("sleep", helperHarnessLifetime)
	harness.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := harness.Start(); err != nil {
		t.Fatal(err)
	}
	identity, err := sessionshim.ProcessIdentityFor(harness.Process.Pid)
	if err != nil {
		_ = harness.Process.Kill()
		t.Fatal(err)
	}
	if err := publishHelperTombstone(registry, record, harness, identity); err != nil {
		t.Fatalf("publish: %v", err)
	}
	present, err := registry.HasIncarnation(record.Identity(), record.ShimID, record.ProcessEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if present {
		t.Fatal("the helper left its discovery record live after publishing a terminal tombstone")
	}
	if _, err := registry.GetTombstoneIncarnation(record.Identity(), record.ShimID, record.ProcessEpoch); err != nil {
		t.Fatalf("the tombstone did not survive the record withdrawal: %v", err)
	}
}
