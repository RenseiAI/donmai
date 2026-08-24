package daemon

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

func TestSessionShimAcceptanceControlRequiresConfiguredBearer(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "control-token")
	const token = "fixture-control-token-32-bytes-xxxx"
	if err := os.WriteFile(tokenPath, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(sessionShimAcceptanceTokenPathEnvironment(), tokenPath)

	d := New(Options{SkipRegistration: true})
	s := NewServer(d)
	httpd := httptest.NewServer(s.httpd.Handler)
	t.Cleanup(httpd.Close)

	request := func(token string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, httpd.URL+sessionShimAcceptanceRoute+"check", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	if got := request(""); got != http.StatusNotFound {
		t.Fatalf("missing bearer status = %d, want non-disclosing 404", got)
	}
	if got := request("wrong"); got != http.StatusNotFound {
		t.Fatalf("wrong bearer status = %d, want non-disclosing 404", got)
	}
	if got := request(token); got != http.StatusNoContent {
		t.Fatalf("configured bearer status = %d, want 204", got)
	}
}

func TestSessionShimAcceptanceQuarantineRequiresLiveUnexpectedRecord(t *testing.T) {
	registryDir := t.TempDir()
	d := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{RegistryDir: registryDir}})
	id := sessionshim.Identity{OrgID: "org-acceptance", SessionID: "session-acceptance"}
	d.shims.mu.Lock()
	d.shims.adopted[id] = adoptedShim{shimID: "shim-owned"}
	d.shims.mu.Unlock()

	process, err := sessionshim.Self()
	if err != nil {
		t.Fatal(err)
	}
	registry, err := sessionshim.NewRegistry(registryDir)
	if err != nil {
		t.Fatal(err)
	}
	record := sessionshim.Record{
		SchemaVersion: sessionshim.RecordSchemaVersion,
		OrgID:         id.OrgID, SessionID: id.SessionID,
		ShimID: "shim-incompatible", ProcessEpoch: 7,
		PID: process.PID, ProcessStartedAt: process.StartedAt,
		SocketPath:  filepath.Join(registryDir, "incompatible.sock"),
		ProtocolMin: shimwire.ProtocolMax + 1, ProtocolMax: shimwire.ProtocolMax + 1,
		Phase: shimwire.PhaseRunning, CreatedAtUnixNano: time.Now().UnixNano(),
	}
	if err := registry.Put(record); err != nil {
		t.Fatal(err)
	}

	if err := d.armSessionShimAcceptanceQuarantine(id); err != nil {
		t.Fatalf("arm acceptance quarantine: %v", err)
	}
	got := d.QuarantinedSessions()
	if len(got) != 1 || got[0].Identity() != id || got[0].ShimID != record.ShimID ||
		got[0].ProcessEpoch != record.ProcessEpoch || got[0].Reason != sessionshim.QuarantineProtocolMismatch ||
		!got[0].ConsumesCapacity {
		t.Fatalf("acceptance quarantine = %+v, want exact capacity-charged incompatible record", got)
	}
}

func TestSessionShimAcceptanceFenceRefusalIsExactAndOneShot(t *testing.T) {
	d := New(Options{SkipRegistration: true})
	id := sessionshim.Identity{OrgID: "org-acceptance", SessionID: "session-acceptance"}
	d.shims.mu.Lock()
	d.shims.adopted[id] = adoptedShim{shimID: "shim-owned"}
	d.shims.mu.Unlock()
	if err := d.armSessionShimAcceptanceFenceRefusal(id); err != nil {
		t.Fatal(err)
	}
	preparation := &restartPreparation{
		scopeIDs: []string{id.OrgID},
		covered:  map[string][]sessionshim.FencedSession{id.OrgID: {{OrgID: id.OrgID, SessionID: id.SessionID}}},
	}
	if err := d.consumeSessionShimAcceptanceFenceRefusal(preparation); !errors.Is(err, errSessionShimAcceptanceFenceRefused) {
		t.Fatalf("first fence acknowledgement = %v, want exact refusal", err)
	}
	if err := d.consumeSessionShimAcceptanceFenceRefusal(preparation); err != nil {
		t.Fatalf("second fence acknowledgement remained faulted: %v", err)
	}
	if err := d.clearSessionShimAcceptanceFenceRefusal(id); err != nil {
		t.Fatalf("clear observed refusal: %v", err)
	}
}
