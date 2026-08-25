package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

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

func TestQuarantineArmStatePublicationFailureRecoversExactReadyHelper(t *testing.T) {
	// This is the literal pre-publication crash/disk-failure window: daemon
	// mutation accepts first, then the ordinary state.json write fails. Recovery
	// must consume only the exact durable ready record, never a PID/prefix guess.
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	registryDir := filepath.Join(dir, "registry")
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionshim.NewRegistry(registryDir); err != nil {
		t.Fatal(err)
	}
	cfg := config{stateDir: stateDir, registry: registryDir}
	if err := cfg.saveState(state{}); err != nil {
		t.Fatal(err)
	}
	self, err := sessionshim.Self()
	if err != nil {
		t.Fatal(err)
	}
	const orgID = "org-acceptance"
	const sessionID = "session-acceptance"
	const shimID = "0123456789abcdef0123456789abcdef"
	name := acceptanceHelperName(orgID, sessionID, shimID)
	helper := helperState{
		OrgID: orgID, SessionID: sessionID, ShimID: shimID, ProcessEpoch: 1,
		// Reuse this test process's PID with a mismatched start time: cleanup
		// must treat it as gone and must not create the helper stop signal.
		PID: self.PID, ProcessStartedAt: self.StartedAt + 1, HarnessPID: 1,
		RecordPath: filepath.Join(registryDir, name+".json"),
		SocketPath: filepath.Join(registryDir, name+".sock"),
		ReadyPath:  filepath.Join(stateDir, name+".ready.json"),
		StopPath:   filepath.Join(stateDir, name+".stop"),
	}
	ready, err := json.Marshal(helper)
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(helper.ReadyPath, ready, 0o600); err != nil {
		t.Fatal(err)
	}

	var calls []controlRequest
	cfg.controlFn = func(action string, request controlRequest) error {
		switch action {
		case "quarantine-arm", "quarantine-clear":
			calls = append(calls, request)
			return nil
		default:
			return errors.New("unexpected control action")
		}
	}
	cfg.saveStateFn = func(value state) error {
		if value.Helper != nil {
			return errors.New("injected state disk failure")
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		return atomicWrite(cfg.statePath(), raw, 0o600)
	}

	err = cfg.completeQuarantineArm(&state{}, controlRequest{OrgID: orgID, SessionID: sessionID}, helper)
	if err == nil || err.Error() != "injected state disk failure" {
		t.Fatalf("post-mutation state publication = %v, want injected disk failure", err)
	}
	if len(calls) != 1 || calls[0] != (controlRequest{OrgID: orgID, SessionID: sessionID}) {
		t.Fatalf("mutation calls = %+v, want one arm for the adopted lifecycle", calls)
	}

	// GREEN: state.json contains no Helper, yet exact recovery finds only the
	// durable ready correlation and clears that exact incarnation idempotently.
	if err := cfg.quarantineClear(sessionID); err != nil {
		t.Fatalf("recover exact helper after state publication failure: %v", err)
	}
	if len(calls) != 2 || calls[1] != (controlRequest{OrgID: orgID, SessionID: sessionID, ShimID: shimID, ProcessEpoch: 1}) {
		t.Fatalf("recovery calls = %+v, want exact clear correlation", calls)
	}
	if pathExists(helper.ReadyPath) {
		t.Fatal("recovered helper ready record remains")
	}
	if pathExists(helper.StopPath) {
		t.Fatal("cleanup wrote a stop signal for a reused PID")
	}
	loaded, err := cfg.loadState()
	if err != nil || loaded.Helper != nil || loaded.SessionID != sessionID || loaded.OrgID != orgID {
		t.Fatalf("recovered state = %+v, %v", loaded, err)
	}
	if err := cfg.quarantineClear(sessionID); err != nil {
		t.Fatalf("idempotent recovered cleanup: %v", err)
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
