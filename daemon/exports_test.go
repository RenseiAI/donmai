// Package daemon_test exercises the Wave C exported symbols from outside the
// daemon package. Each test proves reachability (the symbol is exported and
// callable) and behaviour parity with the pre-export state (no logic change).
//
// Exported so embedders can drive multi-identity poll loops; these tests are
// the embedder's smoke test — if they compile and pass, the public surface is
// correct.
package daemon_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/daemon"
)

// TestAllowlistEntriesFromConfig_Exported proves AllowlistEntriesFromConfig is
// callable from outside the package and produces the same output as the
// pre-export implementation (sort-by-id, skip incomplete entries).
func TestAllowlistEntriesFromConfig_Exported(t *testing.T) {
	t.Parallel()

	t.Run("nil input returns nil", func(t *testing.T) {
		t.Parallel()
		if got := daemon.AllowlistEntriesFromConfig(nil); got != nil {
			t.Errorf("AllowlistEntriesFromConfig(nil) = %v, want nil", got)
		}
	})

	t.Run("sorts by id for stable hash", func(t *testing.T) {
		t.Parallel()
		in := []daemon.ProjectConfig{
			{ID: "zebra", Repository: "https://github.com/x/zebra"},
			{ID: "alpha", Repository: "https://github.com/x/alpha"},
		}
		got := daemon.AllowlistEntriesFromConfig(in)
		want := []daemon.ProjectAllowlistEntry{
			{ID: "alpha", Repository: "https://github.com/x/alpha"},
			{ID: "zebra", Repository: "https://github.com/x/zebra"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("AllowlistEntriesFromConfig sort: got %v, want %v", got, want)
		}
	})

	t.Run("skips entries missing id or repository", func(t *testing.T) {
		t.Parallel()
		in := []daemon.ProjectConfig{
			{ID: "ok", Repository: "https://github.com/x/ok"},
			{ID: "", Repository: "https://github.com/x/no-id"},
			{ID: "no-repo", Repository: ""},
		}
		got := daemon.AllowlistEntriesFromConfig(in)
		want := []daemon.ProjectAllowlistEntry{
			{ID: "ok", Repository: "https://github.com/x/ok"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("AllowlistEntriesFromConfig skip: got %v, want %v", got, want)
		}
	})
}

// TestPollItemToSessionSpec_Exported proves PollItemToSessionSpec is callable
// from outside the package and maps a PollWorkItem to a SessionSpec correctly.
func TestPollItemToSessionSpec_Exported(t *testing.T) {
	t.Parallel()

	projects := []daemon.ProjectConfig{
		{ID: "proj-abc", Repository: "https://github.com/x/repo"},
	}

	t.Run("resolves allowlisted repository by id", func(t *testing.T) {
		t.Parallel()
		item := daemon.PollWorkItem{
			SessionID:   "sess-ext-1",
			ProjectName: "proj-abc",
		}
		spec := daemon.PollItemToSessionSpec(item, projects)
		if spec.SessionID != "sess-ext-1" {
			t.Errorf("SessionID = %q, want %q", spec.SessionID, "sess-ext-1")
		}
		if spec.Repository != "https://github.com/x/repo" {
			t.Errorf("Repository = %q, want resolved URL", spec.Repository)
		}
		if spec.ProjectName != "proj-abc" {
			t.Errorf("ProjectName = %q, want %q", spec.ProjectName, "proj-abc")
		}
	})

	t.Run("falls back to wire value on no allowlist match", func(t *testing.T) {
		t.Parallel()
		item := daemon.PollWorkItem{
			SessionID:   "sess-ext-2",
			ProjectName: "unknown-proj",
			Repository:  "https://github.com/x/unknown",
		}
		spec := daemon.PollItemToSessionSpec(item, projects)
		if spec.Repository != "https://github.com/x/unknown" {
			t.Errorf("Repository = %q, want original wire value", spec.Repository)
		}
		if spec.ProjectName != "" {
			t.Errorf("ProjectName = %q, want empty on no allowlist match", spec.ProjectName)
		}
	})
}

// TestPollItemToSessionDetail_Exported proves PollItemToSessionDetail is
// callable from outside the package and populates SessionDetail correctly.
func TestPollItemToSessionDetail_Exported(t *testing.T) {
	t.Parallel()

	projects := []daemon.ProjectConfig{
		{ID: "proj-xyz", Repository: "https://github.com/x/xyz"},
	}

	t.Run("resolves repository and injects worker fields", func(t *testing.T) {
		t.Parallel()
		item := daemon.PollWorkItem{
			SessionID:   "sess-ext-3",
			ProjectName: "proj-xyz",
		}
		detail := daemon.PollItemToSessionDetail(
			item, projects,
			"https://platform.example",
			"tok-abc",
			"wkr-ext",
		)
		if detail == nil {
			t.Fatal("PollItemToSessionDetail returned nil")
		}
		if detail.SessionID != "sess-ext-3" {
			t.Errorf("SessionID = %q, want %q", detail.SessionID, "sess-ext-3")
		}
		if detail.Repository != "https://github.com/x/xyz" {
			t.Errorf("Repository = %q, want resolved URL", detail.Repository)
		}
		if detail.ProjectName != "proj-xyz" {
			t.Errorf("ProjectName = %q, want allowlist id", detail.ProjectName)
		}
		if detail.WorkerID != "wkr-ext" {
			t.Errorf("WorkerID = %q, want %q", detail.WorkerID, "wkr-ext")
		}
		if detail.AuthToken != "tok-abc" {
			t.Errorf("AuthToken = %q, want %q", detail.AuthToken, "tok-abc")
		}
		if detail.PlatformURL != "https://platform.example" {
			t.Errorf("PlatformURL = %q, want %q", detail.PlatformURL, "https://platform.example")
		}
	})

	t.Run("falls back to wire value on no allowlist match", func(t *testing.T) {
		t.Parallel()
		item := daemon.PollWorkItem{
			SessionID:  "sess-ext-4",
			Repository: "https://github.com/x/nope",
		}
		detail := daemon.PollItemToSessionDetail(item, nil, "", "", "")
		if detail == nil {
			t.Fatal("PollItemToSessionDetail returned nil")
		}
		if detail.Repository != "https://github.com/x/nope" {
			t.Errorf("Repository = %q, want original wire value", detail.Repository)
		}
	})
}

// TestDaemon_ActiveSessionCount_Exported proves ActiveSessionCount is callable
// from outside the package. Before Start(), the spawner is nil so the count is 0.
func TestDaemon_ActiveSessionCount_Exported(t *testing.T) {
	t.Parallel()
	d := daemon.New(daemon.Options{
		ConfigPath: "/dev/null",
		HTTPPort:   0,
	})
	if got := d.ActiveSessionCount(); got != 0 {
		t.Errorf("ActiveSessionCount before Start = %d, want 0", got)
	}
}

// TestDaemon_ActiveInteractiveSessionCount_Exported proves
// ActiveInteractiveSessionCount is callable from outside the package. Before
// Start(), the spawner is nil so the count is 0.
func TestDaemon_ActiveInteractiveSessionCount_Exported(t *testing.T) {
	t.Parallel()
	d := daemon.New(daemon.Options{
		ConfigPath: "/dev/null",
		HTTPPort:   0,
	})
	if got := d.ActiveInteractiveSessionCount(); got != 0 {
		t.Errorf("ActiveInteractiveSessionCount before Start = %d, want 0", got)
	}
}

// TestDaemon_ActiveSessionCounts_Exported proves the coherent callback can be
// wired directly by an external embedder. Before Start(), both counts are 0.
func TestDaemon_ActiveSessionCounts_Exported(t *testing.T) {
	t.Parallel()
	d := daemon.New(daemon.Options{
		ConfigPath: "/dev/null",
		HTTPPort:   0,
	})
	callback := d.ActiveSessionCounts
	active, interactive := callback()
	if active != 0 || interactive != 0 {
		t.Errorf("ActiveSessionCounts before Start = (%d, %d), want (0, 0)", active, interactive)
	}
}

// TestDaemon_MaxConcurrentSessions_Exported proves MaxConcurrentSessions is
// callable from outside the package. Before Start() (config not loaded),
// it returns 0.
func TestDaemon_MaxConcurrentSessions_Exported(t *testing.T) {
	t.Parallel()
	d := daemon.New(daemon.Options{
		ConfigPath: "/dev/null",
		HTTPPort:   0,
	})
	if got := d.MaxConcurrentSessions(); got != 0 {
		t.Errorf("MaxConcurrentSessions before Start = %d, want 0 (config nil)", got)
	}
}

// TestDaemon_RegistrationStatus_Exported proves RegistrationStatus is callable
// from outside the package. Before Start(), the daemon is in StateStopped, which
// maps to RegistrationIdle.
func TestDaemon_RegistrationStatus_Exported(t *testing.T) {
	t.Parallel()
	d := daemon.New(daemon.Options{
		ConfigPath: "/dev/null",
		HTTPPort:   0,
	})
	got := d.RegistrationStatus()
	if got != daemon.RegistrationIdle {
		t.Errorf("RegistrationStatus before Start = %q, want %q", got, daemon.RegistrationIdle)
	}
}

func TestDaemon_PollClaimGate_Exported(t *testing.T) {
	d := daemon.New(daemon.Options{SessionShim: daemon.SessionShimConfig{
		EnableAdoption: true, RequireCredentialAttestation: true,
		ControllerID: "external-claim-gate", AttestationCapabilities: daemon.RequiredSessionShimHostCapabilities(),
		GetCarrierProofV2Readiness: func() (daemon.SessionShimCarrierProofV2Readiness, error) {
			return daemon.SessionShimCarrierProofV2Readiness{}, nil
		},
	}})

	var pollHits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		pollHits.Add(1)
		_ = json.NewEncoder(w).Encode(daemon.PollResponse{})
	}))
	t.Cleanup(server.Close)

	poller := daemon.NewPollService(daemon.PollOptions{
		WorkerID: "satellite-worker", RuntimeJWT: "runtime", OrchestratorURL: server.URL,
		ClaimSuspended: d.PollClaimGate(),
		OnWork:         func(daemon.PollWorkItem) error { return nil },
		HTTPClient:     server.Client(),
	})
	poller.Start()
	deadline := time.Now().Add(time.Second)
	for !poller.ClaimsSuspended() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	poller.Stop()
	if got := pollHits.Load(); got != 0 {
		t.Fatalf("satellite poll reached claim endpoint while proof-v2 readiness was withdrawn: hits=%d", got)
	}
	if !poller.ClaimsSuspended() {
		t.Fatal("satellite poll did not latch the exported daemon claim gate")
	}
}
