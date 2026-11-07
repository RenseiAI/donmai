package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Consumer under test (production path, no copies):
// HeartbeatService.sendOneSerialized -> callEndpoint ->
// handleHeartbeatResponse -> Daemon.applySystemUpdateDefaults, read via
// Daemon.EffectiveAutoUpdateConfig / EffectiveUpdateChannel /
// EffectiveUpdateCDNBase (daemon/heartbeat.go, daemon/update_defaults.go).

func updateDefaultsTestDaemon(t *testing.T, local AutoUpdateConfig) *Daemon {
	t.Helper()
	d := newRestartTestDaemon(t, nil)
	dir := t.TempDir()
	d.opts.ConfigPath = filepath.Join(dir, "daemon.yaml")
	d.config = &Config{
		APIVersion: "v1", Kind: "DaemonConfig",
		Machine:      MachineConfig{ID: "test-machine"},
		Capacity:     CapacityConfig{MaxConcurrentSessions: 2},
		Orchestrator: OrchestratorConfig{URL: "https://example.test"},
		AutoUpdate:   local,
	}
	// Mark an explicitly supplied non-empty channel as authored so the
	// in-memory construction matches what LoadConfig would record for a
	// file that contains the key. Omitted (empty) stays non-authored.
	d.config.autoUpdateChannelAuthored = strings.TrimSpace(string(local.Channel)) != ""
	if err := WriteConfig(d.opts.ConfigPath, d.config); err != nil {
		t.Fatalf("write config: %v", err)
	}
	// Reload through the production loader so defaulting and authored
	// presence match a normally loaded configuration.
	if loaded, err := LoadConfig(d.opts.ConfigPath); err != nil {
		t.Fatalf("reload config: %v", err)
	} else if loaded != nil {
		d.config = loaded
	}
	return d
}

// disposable platform: operator PUT writes storage, heartbeat reads it.
type updateDefaultsPlatform struct {
	mu      sync.Mutex
	stored  json.RawMessage // nil = older server (omit member)
	omit    bool
	beatErr int // non-zero forces heartbeat HTTP status
}

func (p *updateDefaultsPlatform) put(raw json.RawMessage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stored = raw
	p.omit = false
	p.beatErr = 0
}

func (p *updateDefaultsPlatform) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		stored := p.stored
		omit := p.omit
		beatErr := p.beatErr
		p.mu.Unlock()
		if beatErr != 0 {
			http.Error(w, "boom", beatErr)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/operator/update-defaults") && r.Method == http.MethodPut {
			var raw json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
				http.Error(w, "bad put", http.StatusBadRequest)
				return
			}
			p.mu.Lock()
			p.stored = raw
			p.omit = false
			p.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
			return
		}
		resp := map[string]any{"acknowledged": true}
		if !omit && stored != nil {
			resp["updateDefaults"] = stored
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func driveHeartbeat(t *testing.T, d *Daemon, srv *httptest.Server) error {
	t.Helper()
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")
	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID: "wkr", Hostname: "h", OrchestratorURL: srv.URL, RuntimeJWT: "runtime.jwt",
		GetActiveCount:   func() int { return 0 },
		GetMaxCount:      func() int { return 2 },
		GetStatus:        func() RegistrationStatus { return RegistrationIdle },
		OnUpdateDefaults: d.applySystemUpdateDefaults,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return hs.sendOneResult(ctx)
}

func TestUpdateDefaults_SystemReachesEffectiveAfterHeartbeat(t *testing.T) {
	plat := &updateDefaultsPlatform{}
	srv := httptest.NewServer(plat.handler(t))
	t.Cleanup(srv.Close)
	plat.put(json.RawMessage(`{"channel":"beta","cdnBase":"https://sys.example.test"}`))

	d := updateDefaultsTestDaemon(t, AutoUpdateConfig{Channel: ChannelStable, Schedule: ScheduleManual})
	if err := driveHeartbeat(t, d, srv); err != nil {
		t.Fatalf("beat: %v", err)
	}
	// Local channel stable wins over system beta; system cdnBase applies (local empty).
	ch, origin := d.EffectiveUpdateChannel()
	if ch != ChannelStable || origin != UpdateOriginLocal {
		t.Fatalf("channel = %q/%q, want stable/local", ch, origin)
	}
	base, bOrigin := d.EffectiveUpdateCDNBase()
	if base != "https://sys.example.test" || bOrigin != UpdateOriginSystem {
		t.Fatalf("cdnBase = %q/%q, want system value", base, bOrigin)
	}
	// Clearing the local channel override exposes the system value.
	d.config.AutoUpdate.Channel = ""
	d.config.autoUpdateChannelAuthored = false
	if err := driveHeartbeat(t, d, srv); err != nil {
		t.Fatalf("beat2: %v", err)
	}
	if ch, _ := d.EffectiveUpdateChannel(); ch != ChannelBeta {
		t.Fatalf("after local clear channel = %q, want beta", ch)
	}
}

func TestUpdateDefaults_ChannelIndependentClearAndPreserve(t *testing.T) {
	plat := &updateDefaultsPlatform{}
	srv := httptest.NewServer(plat.handler(t))
	t.Cleanup(srv.Close)
	d := updateDefaultsTestDaemon(t, AutoUpdateConfig{Schedule: ScheduleManual})

	plat.put(json.RawMessage(`{"channel":"beta","cdnBase":"https://sys.example.test"}`))
	if err := driveHeartbeat(t, d, srv); err != nil {
		t.Fatalf("beat: %v", err)
	}
	if ch, _ := d.EffectiveUpdateChannel(); ch != ChannelBeta {
		t.Fatalf("channel = %q, want beta", ch)
	}
	// Explicit system clear of channel only: binary default, cdnBase untouched.
	plat.put(json.RawMessage(`{"channel":null}`))
	if err := driveHeartbeat(t, d, srv); err != nil {
		t.Fatalf("clear beat: %v", err)
	}
	if ch, o := d.EffectiveUpdateChannel(); ch != ChannelStable || o != UpdateOriginDefault {
		t.Fatalf("after clear channel = %q/%q, want stable/default", ch, o)
	}
	if base, _ := d.EffectiveUpdateCDNBase(); base != "https://sys.example.test" {
		t.Fatalf("cdnBase after channel clear = %q, want preserved system", base)
	}
	// Restore system, then absence / malformed / unavailable preserve last-good.
	plat.put(json.RawMessage(`{"channel":"beta"}`))
	if err := driveHeartbeat(t, d, srv); err != nil {
		t.Fatalf("restore: %v", err)
	}
	plat.mu.Lock()
	plat.omit = true
	plat.stored = nil
	plat.mu.Unlock()
	if err := driveHeartbeat(t, d, srv); err != nil {
		t.Fatalf("absent beat: %v", err)
	}
	if ch, _ := d.EffectiveUpdateChannel(); ch != ChannelBeta {
		t.Fatalf("absent cleared channel = %q, want beta preserved", ch)
	}
	plat.mu.Lock()
	plat.omit = false
	plat.stored = json.RawMessage(`{"channel":123}`)
	plat.mu.Unlock()
	if err := driveHeartbeat(t, d, srv); err != nil {
		t.Fatalf("malformed beat: %v", err)
	}
	if ch, _ := d.EffectiveUpdateChannel(); ch != ChannelBeta {
		t.Fatalf("malformed cleared channel = %q, want beta preserved", ch)
	}
	if d.SystemUpdateDiagnostic() == "" {
		t.Fatal("malformed left no typed diagnostic")
	}
	plat.mu.Lock()
	plat.beatErr = http.StatusInternalServerError
	plat.mu.Unlock()
	_ = driveHeartbeat(t, d, srv)
	if ch, _ := d.EffectiveUpdateChannel(); ch != ChannelBeta {
		t.Fatalf("unavailable cleared channel = %q, want beta preserved", ch)
	}
}

func TestUpdateDefaults_LocalBytesAndTrustUntouched(t *testing.T) {
	plat := &updateDefaultsPlatform{}
	srv := httptest.NewServer(plat.handler(t))
	t.Cleanup(srv.Close)
	d := updateDefaultsTestDaemon(t, AutoUpdateConfig{Schedule: ScheduleManual})
	before, err := os.ReadFile(d.opts.ConfigPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	plat.put(json.RawMessage(`{"channel":"beta","cdnBase":"https://sys.example.test","signers":[{"san":"x"}],"trustRoot":"x","unknownField":1}`))
	if err := driveHeartbeat(t, d, srv); err != nil {
		t.Fatalf("beat: %v", err)
	}
	after, err := os.ReadFile(d.opts.ConfigPath)
	if err != nil {
		t.Fatalf("reread config: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("local config bytes changed after system snapshot")
	}
	eff := d.EffectiveAutoUpdateConfig()
	if eff.Channel != ChannelBeta {
		t.Fatalf("effective channel = %q, want beta", eff.Channel)
	}
	if len(eff.Signers) != 0 {
		t.Fatalf("system signers leaked into effective config: %+v", eff.Signers)
	}
	if strings.TrimSpace(eff.TrustRootPath) != "" {
		t.Fatalf("system trust leaked: %q", eff.TrustRootPath)
	}
}

func TestUpdateDefaults_OldServerAndIdempotent(t *testing.T) {
	plat := &updateDefaultsPlatform{omit: true}
	srv := httptest.NewServer(plat.handler(t))
	t.Cleanup(srv.Close)
	d := updateDefaultsTestDaemon(t, AutoUpdateConfig{Channel: ChannelStable, Schedule: ScheduleManual})
	if err := driveHeartbeat(t, d, srv); err != nil {
		t.Fatalf("old-server beat: %v", err)
	}
	if ch, o := d.EffectiveUpdateChannel(); ch != ChannelStable || o != UpdateOriginLocal {
		t.Fatalf("old-server channel = %q/%q, want stable/local", ch, o)
	}
	plat.mu.Lock()
	plat.omit = false
	plat.stored = json.RawMessage(`{"channel":"beta"}`)
	plat.mu.Unlock()
	if err := driveHeartbeat(t, d, srv); err != nil {
		t.Fatalf("beat1: %v", err)
	}
	first := d.SystemUpdateSnapshot()
	if err := driveHeartbeat(t, d, srv); err != nil {
		t.Fatalf("beat2: %v", err)
	}
	second := d.SystemUpdateSnapshot()
	if (first.Channel == nil) != (second.Channel == nil) || (first.Channel != nil && *first.Channel != *second.Channel) {
		t.Fatalf("repeated snapshot not idempotent: %+v vs %+v", first, second)
	}
	// Fresh process without a snapshot uses local/binary configuration.
	fresh := updateDefaultsTestDaemon(t, AutoUpdateConfig{Schedule: ScheduleManual})
	if ch, o := fresh.EffectiveUpdateChannel(); ch != ChannelStable || o != UpdateOriginDefault {
		t.Fatalf("fresh channel = %q/%q, want stable/default", ch, o)
	}
}

func TestUpdateDefaults_OmittedChannelLetsSystemWin(t *testing.T) {
	// B1: a normally loaded configuration that omits autoUpdate.channel
	// fills the compiled default (stable) but must not mask the system
	// layer. Production path: file without the key -> LoadConfig.
	d := newRestartTestDaemon(t, nil)
	dir := t.TempDir()
	d.opts.ConfigPath = filepath.Join(dir, "daemon.yaml")
	raw := []byte("apiVersion: v1\nkind: DaemonConfig\nmachine:\n  id: test-machine\ncapacity:\n  maxConcurrentSessions: 2\norchestrator:\n  url: https://example.test\nautoUpdate:\n  schedule: manual\n")
	if err := os.WriteFile(d.opts.ConfigPath, raw, 0o600); err != nil {
		t.Fatalf("write raw config: %v", err)
	}
	loaded, err := LoadConfig(d.opts.ConfigPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if loaded == nil {
		t.Fatal("load config returned nil")
	}
	if loaded.AutoUpdate.Channel != ChannelStable {
		t.Fatalf("loaded channel = %q, want stable default fill", loaded.AutoUpdate.Channel)
	}
	d.config = loaded
	plat := &updateDefaultsPlatform{}
	srv := httptest.NewServer(plat.handler(t))
	t.Cleanup(srv.Close)
	plat.put(json.RawMessage(`{"channel":"beta"}`))
	if err := driveHeartbeat(t, d, srv); err != nil {
		t.Fatalf("beat: %v", err)
	}
	if ch, o := d.EffectiveUpdateChannel(); ch != ChannelBeta || o != UpdateOriginSystem {
		t.Fatalf("omitted local channel: got %s/%s; want beta/system", ch, o)
	}
	if eff := d.EffectiveAutoUpdateConfig(); eff.Channel != ChannelBeta {
		t.Fatalf("effective config channel = %q, want beta", eff.Channel)
	}
}

func TestUpdateDefaults_MissingHostnamePreserves(t *testing.T) {
	// B2: a system origin with its hostname omitted (port-only authority)
	// must preserve the last-good CDN and record a typed diagnostic.
	plat := &updateDefaultsPlatform{}
	srv := httptest.NewServer(plat.handler(t))
	t.Cleanup(srv.Close)
	d := updateDefaultsTestDaemon(t, AutoUpdateConfig{Schedule: ScheduleManual})
	plat.put(json.RawMessage(`{"cdnBase":"https://sys.example.test"}`))
	if err := driveHeartbeat(t, d, srv); err != nil {
		t.Fatalf("seed beat: %v", err)
	}
	if base, _ := d.EffectiveUpdateCDNBase(); base != "https://sys.example.test" {
		t.Fatalf("seed cdnBase = %q, want system value", base)
	}
	plat.put(json.RawMessage(`{"cdnBase":"https://:443"}`))
	if err := driveHeartbeat(t, d, srv); err != nil {
		t.Fatalf("bad beat: %v", err)
	}
	if base, _ := d.EffectiveUpdateCDNBase(); base != "https://sys.example.test" {
		t.Fatalf("missing hostname overwrote last-good: base=%q", base)
	}
	if diag := d.SystemUpdateDiagnostic(); diag == "" {
		t.Fatal("missing hostname left no typed diagnostic")
	}
}
