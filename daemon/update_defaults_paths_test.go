package daemon

import (
	"context"
	"encoding/json"
	"fmt"
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

// Consumers under test (production paths, no copies): the yaml watcher
// (Daemon.onYamlChanged) plus the mutation persist
// (Daemon.persistAndRefreshLocked -> WriteConfig), and every update door
// that resolves the effective auto-update block: the scheduled tick
// (autoUpdateScheduler.attemptOnce -> checkAutoUpdateRelease ->
// updateWithConfig -> runWithConfig -> newUpdaterWithContext), the manual
// door (Daemon.Update) and host.upgrade (Daemon.executeHostUpgrade).

const updateDefaultsReloadYAML = "apiVersion: v1\nkind: DaemonConfig\nmachine:\n  id: test-machine\ncapacity:\n  maxConcurrentSessions: 2\norchestrator:\n  url: https://example.test\nautoUpdate:\n%s  schedule: manual\n"

func writeUpdateDefaultsYAML(t *testing.T, path, channelLine string) *Config {
	t.Helper()
	if err := os.WriteFile(path, []byte(fmt.Sprintf(updateDefaultsReloadYAML, channelLine)), 0o600); err != nil {
		t.Fatalf("write daemon.yaml: %v", err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load daemon.yaml: %v", err)
	}
	if loaded == nil {
		t.Fatal("load daemon.yaml returned nil")
	}
	return loaded
}

func persistUpdateDefaultsConfig(t *testing.T, d *Daemon) string {
	t.Helper()
	d.mu.Lock()
	err := d.persistAndRefreshLocked()
	d.mu.Unlock()
	if err != nil {
		t.Fatalf("persist config: %v", err)
	}
	raw, err := os.ReadFile(d.opts.ConfigPath)
	if err != nil {
		t.Fatalf("read persisted config: %v", err)
	}
	return string(raw)
}

// An operator who live-adds an explicit "channel: stable" pins the host to
// stable over a system beta, and a later unrelated persist keeps the line.
// The default fill is also stable, so the AutoUpdate block alone is
// unchanged by the edit: only the authored bit tells the reload apart.
func TestUpdateDefaults_LiveReloadAddsExplicitStable(t *testing.T) {
	d := newRestartTestDaemon(t, nil)
	d.opts.ConfigPath = filepath.Join(t.TempDir(), "daemon.yaml")
	d.config = writeUpdateDefaultsYAML(t, d.opts.ConfigPath, "")
	d.applySystemUpdateDefaults(json.RawMessage(`{"channel":"beta"}`))
	if ch, o := d.EffectiveUpdateChannel(); ch != ChannelBeta || o != UpdateOriginSystem {
		t.Fatalf("before edit: ch=%q/%q, want beta/system", ch, o)
	}

	d.onYamlChanged(writeUpdateDefaultsYAML(t, d.opts.ConfigPath, "  channel: stable\n"))
	if ch, o := d.EffectiveUpdateChannel(); ch != ChannelStable || o != UpdateOriginLocal {
		t.Fatalf("after live reload of explicit stable: ch=%q/%q, want stable/local", ch, o)
	}
	if got := d.EffectiveAutoUpdateConfig().Channel; got != ChannelStable {
		t.Fatalf("effective update block channel = %q, want stable", got)
	}

	raw := persistUpdateDefaultsConfig(t, d)
	if !strings.Contains(raw, "channel: stable") {
		t.Fatalf("a later persist erased the operator's explicit channel: stable from daemon.yaml:\n%s", raw)
	}
	reloaded, err := LoadConfig(d.opts.ConfigPath)
	if err != nil {
		t.Fatalf("reload persisted config: %v", err)
	}
	if !reloaded.AutoUpdateChannelAuthored() {
		t.Fatal("persisted config lost the authored channel")
	}
}

// Removing an explicit "channel: stable" live hands the channel back to
// the system layer, and a later persist does not resurrect the line.
func TestUpdateDefaults_LiveReloadRemovesExplicitStable(t *testing.T) {
	d := newRestartTestDaemon(t, nil)
	d.opts.ConfigPath = filepath.Join(t.TempDir(), "daemon.yaml")
	d.config = writeUpdateDefaultsYAML(t, d.opts.ConfigPath, "  channel: stable\n")
	d.applySystemUpdateDefaults(json.RawMessage(`{"channel":"beta"}`))
	if ch, o := d.EffectiveUpdateChannel(); ch != ChannelStable || o != UpdateOriginLocal {
		t.Fatalf("before edit: ch=%q/%q, want stable/local", ch, o)
	}

	d.onYamlChanged(writeUpdateDefaultsYAML(t, d.opts.ConfigPath, ""))
	if ch, o := d.EffectiveUpdateChannel(); ch != ChannelBeta || o != UpdateOriginSystem {
		t.Fatalf("after live removal of explicit stable: ch=%q/%q, want beta/system", ch, o)
	}

	raw := persistUpdateDefaultsConfig(t, d)
	if strings.Contains(raw, "channel:") {
		t.Fatalf("a later persist resurrected the removed channel line:\n%s", raw)
	}
}

// channelCDN is a disposable release origin that serves a per-channel
// manifest and records each manifest request in order.
type channelCDN struct {
	srv        *httptest.Server
	binary     []byte
	versions   map[string]string
	onManifest func(channel string)

	mu        sync.Mutex
	manifests []string
}

func newChannelCDN(t *testing.T, versions map[string]string) *channelCDN {
	t.Helper()
	c := &channelCDN{binary: []byte("new-binary"), versions: versions}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/latest.json"):
			channel := strings.Trim(strings.TrimSuffix(r.URL.Path, "/latest.json"), "/")
			c.mu.Lock()
			c.manifests = append(c.manifests, channel)
			hook := c.onManifest
			c.mu.Unlock()
			if hook != nil {
				hook(channel)
			}
			version, ok := c.versions[channel]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(VersionManifest{Version: version, SHA256: hexSHA256(c.binary)})
		case strings.HasSuffix(r.URL.Path, ".sigstore"):
			_, _ = w.Write([]byte("test-bundle"))
		default:
			_, _ = w.Write(c.binary)
		}
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *channelCDN) requested() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.manifests...)
}

func (c *channelCDN) setOnManifest(hook func(channel string)) {
	c.mu.Lock()
	c.onManifest = hook
	c.mu.Unlock()
}

// systemChannelSchedulerDaemon builds an opted-in scheduled host whose
// daemon.yaml omits the channel (default-filled stable, not authored) and
// whose heartbeat system snapshot names beta.
func systemChannelSchedulerDaemon(t *testing.T, cdn *channelCDN) (*Daemon, string, *atomic.Int32) {
	t.Helper()
	d := newRestartTestDaemon(t, nil)
	d.opts.Version = "0.1.0"
	d.config = &Config{AutoUpdate: AutoUpdateConfig{
		Channel: ChannelStable, Schedule: ScheduleNightly, Scheduled: true, DrainTimeoutSeconds: 1,
		CDNBase: cdn.srv.URL,
		Signers: []UpdateSigner{
			{SAN: "releases@example.com", Issuer: "https://issuer.example"},
		},
	}}
	current := filepath.Join(t.TempDir(), "donmai-daemon")
	if err := os.WriteFile(current, []byte("old-binary"), 0o600); err != nil {
		t.Fatalf("write current binary: %v", err)
	}
	d.autoUpdateVerifier = passVerifier{}
	d.autoUpdateBinaryPath = current
	d.autoUpdateRunBound = 5 * time.Second
	restarts := &atomic.Int32{}
	d.autoUpdateRestartHook = func() { restarts.Add(1) }
	d.applySystemUpdateDefaults(json.RawMessage(`{"channel":"beta"}`))
	if ch, o := d.EffectiveUpdateChannel(); ch != ChannelBeta || o != UpdateOriginSystem {
		t.Fatalf("fixture: ch=%q/%q, want beta/system", ch, o)
	}
	return d, current, restarts
}

func runSchedulerTick(t *testing.T, d *Daemon) {
	t.Helper()
	if d.runPreparedUpdate != nil {
		t.Fatal("runPreparedUpdate must stay nil to exercise the production updater branch")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s := &autoUpdateScheduler{daemon: d, clock: systemUpdateClock{}}
	s.ctx = ctx
	s.attemptOnce()
}

func assertOnlyChannel(t *testing.T, got []string, want string) {
	t.Helper()
	if len(got) == 0 {
		t.Fatalf("manifest requests = %v, want at least one %q", got, want)
	}
	for _, ch := range got {
		if ch != want {
			t.Fatalf("manifest requests = %v, want only %q (one effective snapshot per tick)", got, want)
		}
	}
}

// A scheduled host follows the heartbeat channel: a newer beta with a
// current stable is found by the pre-check and applied by the Update.
func TestUpdateDefaults_ScheduledTickFollowsSystemChannel(t *testing.T) {
	cdn := newChannelCDN(t, map[string]string{"stable": "0.1.0", "beta": "9.9.9"})
	d, current, restarts := systemChannelSchedulerDaemon(t, cdn)
	runSchedulerTick(t, d)
	assertOnlyChannel(t, cdn.requested(), "beta")
	if got := restarts.Load(); got != 1 {
		t.Fatalf("restart requests = %d, want 1 (newer system-channel release never applied)", got)
	}
	swapped, err := os.ReadFile(current) //nolint:gosec
	if err != nil {
		t.Fatalf("read swapped binary: %v", err)
	}
	if string(swapped) != "new-binary" {
		t.Fatalf("swapped binary = %q, want the beta release", swapped)
	}
}

// A newer stable with a current beta must not drain a host whose effective
// channel is beta: the pre-check and the Update agree, so the host never
// leaves running and is never stranded draining.
func TestUpdateDefaults_ScheduledTickNeverStrandsDraining(t *testing.T) {
	cdn := newChannelCDN(t, map[string]string{"stable": "9.9.9", "beta": "0.1.0"})
	d, _, restarts := systemChannelSchedulerDaemon(t, cdn)
	runSchedulerTick(t, d)
	assertOnlyChannel(t, cdn.requested(), "beta")
	if got := d.State(); got != StateRunning {
		t.Fatalf("state = %q after an up-to-date effective channel, want running (host stranded)", got)
	}
	if got := restarts.Load(); got != 0 {
		t.Fatalf("restart requests = %d, want 0", got)
	}
}

// A heartbeat that changes the system channel between the pre-check and
// the Update must not split the tick: the Update reuses the pre-checked
// snapshot instead of re-resolving the live layers.
func TestUpdateDefaults_ScheduledTickIgnoresMidTickHeartbeat(t *testing.T) {
	cdn := newChannelCDN(t, map[string]string{"stable": "0.1.0", "beta": "9.9.9"})
	d, _, restarts := systemChannelSchedulerDaemon(t, cdn)
	var once sync.Once
	cdn.setOnManifest(func(string) {
		once.Do(func() { d.applySystemUpdateDefaults(json.RawMessage(`{"channel":"stable"}`)) })
	})
	runSchedulerTick(t, d)
	assertOnlyChannel(t, cdn.requested(), "beta")
	if got := restarts.Load(); got != 1 {
		t.Fatalf("restart requests = %d, want 1 (state=%q): the Update re-resolved after a mid-tick heartbeat", got, d.State())
	}
}

// systemOriginDaemon has neither a local channel nor a local origin, so
// both effective values come from the heartbeat system snapshot.
func systemOriginDaemon(t *testing.T, cdn *channelCDN) *Daemon {
	t.Helper()
	d := newRestartTestDaemon(t, nil)
	d.opts.Version = "0.1.0"
	d.config = &Config{AutoUpdate: AutoUpdateConfig{
		Channel: ChannelStable, Schedule: ScheduleManual, DrainTimeoutSeconds: 1,
	}}
	current := filepath.Join(t.TempDir(), "donmai-daemon")
	if err := os.WriteFile(current, []byte("old-binary"), 0o600); err != nil {
		t.Fatalf("write current binary: %v", err)
	}
	d.autoUpdateVerifier = passVerifier{}
	d.autoUpdateBinaryPath = current
	raw, err := json.Marshal(map[string]string{"channel": "beta", "cdnBase": cdn.srv.URL})
	if err != nil {
		t.Fatalf("marshal system defaults: %v", err)
	}
	d.applySystemUpdateDefaults(raw)
	return d
}

// The manual door requests the system channel from the system origin.
func TestUpdateDefaults_ManualUpdateUsesSystemChannelAndOrigin(t *testing.T) {
	cdn := newChannelCDN(t, map[string]string{"beta": "0.1.0"})
	d := systemOriginDaemon(t, cdn)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := d.Update(ctx); err != nil {
		t.Fatalf("manual update: %v", err)
	}
	assertOnlyChannel(t, cdn.requested(), "beta")
}

// host.upgrade requests the system channel from the system origin.
func TestUpdateDefaults_HostUpgradeUsesSystemChannelAndOrigin(t *testing.T) {
	cdn := newChannelCDN(t, map[string]string{"beta": "0.1.0"})
	d := systemOriginDaemon(t, cdn)
	if !d.hostUpgradeSourceConfigured() {
		t.Fatal("host.upgrade source not configured although the system snapshot names an origin")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := d.executeHostUpgrade(ctx); err != nil {
		t.Fatalf("host.upgrade: %v", err)
	}
	assertOnlyChannel(t, cdn.requested(), "beta")
}
