package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
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
