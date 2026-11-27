package daemon

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"
)

const (
	managedTestCustomerSAN    = "customer-signer@example.com"
	managedTestCustomerIssuer = "https://customer-issuer.example"
	managedTestVendorSAN      = "vendor-signer@example.com"
	managedTestVendorIssuer   = "https://vendor-issuer.example"
)

// virtualSigstoreRootJSON serializes a VirtualSigstore's trust material to
// the wire bytes the managed projection carries. The in-memory VS stores
// transparency-log IDs as hex-string bytes, which double-encodes through
// NewTrustedRoot+MarshalJSON; decoding the hex to raw bytes first yields a
// JSON document that the production parser accepts and that verifies
// entities signed by the same VS.
func virtualSigstoreRootJSON(t *testing.T, vs *ca.VirtualSigstore) []byte {
	t.Helper()
	fixLogs := func(in map[string]*root.TransparencyLog) map[string]*root.TransparencyLog {
		out := make(map[string]*root.TransparencyLog, len(in))
		for id, tl := range in {
			raw, err := hex.DecodeString(id)
			if err != nil {
				t.Fatalf("decode log id %q: %v", id, err)
			}
			cp := *tl
			cp.ID = raw
			out[id] = &cp
		}
		return out
	}
	tr, err := root.NewTrustedRoot(
		root.TrustedRootMediaType01,
		vs.FulcioCertificateAuthorities(),
		fixLogs(vs.CTLogs()),
		vs.TimestampingAuthorities(),
		fixLogs(vs.RekorLogs()),
	)
	if err != nil {
		t.Fatalf("NewTrustedRoot: %v", err)
	}
	js, err := tr.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	if _, err := root.NewTrustedRootFromJSON(js); err != nil {
		t.Fatalf("round-trip parse: %v", err)
	}
	return js
}

func managedTestProjection(t *testing.T, vs *ca.VirtualSigstore, san, issuer string) *ManagedUpdateTrustProjection {
	t.Helper()
	rootBytes := virtualSigstoreRootJSON(t, vs)
	signers := []UpdateSigner{{SAN: san, Issuer: issuer}}
	return &ManagedUpdateTrustProjection{
		Digest:    computeManagedUpdateTrustDigest(signers, rootBytes),
		Signers:   signers,
		TrustRoot: base64.StdEncoding.EncodeToString(rootBytes),
	}
}

func newManagedTestDaemon(t *testing.T, configPath string, enabled bool, localSigners []UpdateSigner) *Daemon {
	t.Helper()
	d := newRestartTestDaemon(t, nil)
	d.opts.ConfigPath = configPath
	d.config = &Config{
		APIVersion: "v1",
		Kind:       "DaemonConfig",
		Machine:    MachineConfig{ID: "managed-test-machine"},
		Capacity:   CapacityConfig{MaxConcurrentSessions: 2},
		Orchestrator: OrchestratorConfig{
			URL: "https://coordinator.example",
		},
		AutoUpdate: AutoUpdateConfig{
			Channel:             ChannelStable,
			Schedule:            ScheduleManual,
			DrainTimeoutSeconds: 1,
			Signers:             localSigners,
			ManagedTrust:        ManagedUpdateTrustConfig{Enabled: enabled},
		},
	}
	return d
}

// driveManagedProjectionThroughHeartbeat feeds proj through the real
// heartbeat consumer: an httptest platform returns it in the heartbeat
// response and the HeartbeatService invokes the daemon's
// OnManagedUpdateTrust callback.
func driveManagedProjectionThroughHeartbeat(t *testing.T, d *Daemon, proj *ManagedUpdateTrustProjection) {
	t.Helper()
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"acknowledged":       true,
			"managedUpdateTrust": proj,
		})
	}))
	t.Cleanup(srv.Close)
	hb := NewHeartbeatService(HeartbeatOptions{
		WorkerID:             "managed-test-worker",
		Hostname:             "managed-test-host",
		OrchestratorURL:      srv.URL,
		RuntimeJWT:           "managed-test-jwt",
		GetActiveCount:       func() int { return 0 },
		GetMaxCount:          func() int { return 1 },
		GetStatus:            func() RegistrationStatus { return RegistrationIdle },
		OnManagedUpdateTrust: d.applyManagedUpdateTrustProjection,
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := hb.sendOneResult(ctx); err != nil {
		t.Fatalf("heartbeat with managed projection: %v", err)
	}
}

func TestManagedUpdateTrust_HeartbeatRestartCustomerPassesVendorFails(t *testing.T) {
	customerVS, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatalf("customer VS: %v", err)
	}
	vendorVS, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatalf("vendor VS: %v", err)
	}
	proj := managedTestProjection(t, customerVS, managedTestCustomerSAN, managedTestCustomerIssuer)

	dir := t.TempDir()
	configPath := filepath.Join(dir, "daemon.yaml")
	// Local trust pins the vendor root and vendor identity, so the vendor
	// bundle verifies under a local construction: a union mutant (managed
	// OR local) would accept it. Replacement must still reject it.
	localVendor := []UpdateSigner{{SAN: managedTestVendorSAN, Issuer: managedTestVendorIssuer}}
	vendorRootPath := filepath.Join(dir, "vendor-root.json")
	if err := os.WriteFile(vendorRootPath, virtualSigstoreRootJSON(t, vendorVS), 0o600); err != nil {
		t.Fatalf("write vendor root: %v", err)
	}
	d := newManagedTestDaemon(t, configPath, true, localVendor)
	d.config.AutoUpdate.TrustRootPath = vendorRootPath

	driveManagedProjectionThroughHeartbeat(t, d, proj)

	d.managedTrustMu.RLock()
	active := d.managedTrustActive
	blocked := d.managedTrustBlocked
	d.managedTrustMu.RUnlock()
	if blocked {
		t.Fatalf("managed trust blocked after valid projection: %q", d.managedTrustBlockedReason)
	}
	if active == nil || !strings.EqualFold(active.Digest, proj.Digest) {
		t.Fatalf("managed trust not active after heartbeat: active=%+v", active)
	}
	if _, err := os.Stat(managedUpdateTrustPath(configPath)); err != nil {
		t.Fatalf("managed policy not persisted: %v", err)
	}

	// Restart: a fresh Daemon on the same config dir reloads the persisted
	// pair before verifying.
	second := newManagedTestDaemon(t, configPath, true, localVendor)
	second.config.AutoUpdate.TrustRootPath = vendorRootPath
	second.ensureManagedTrustLoaded()
	second.managedTrustMu.RLock()
	restarted := second.managedTrustActive
	restartBlocked := second.managedTrustBlocked
	second.managedTrustMu.RUnlock()
	if restartBlocked || restarted == nil || !strings.EqualFold(restarted.Digest, proj.Digest) {
		t.Fatalf("restart did not reload managed pair: active=%+v blocked=%v", restarted, restartBlocked)
	}

	verifier := second.managedUpdateVerifier(second.Config().AutoUpdate)
	sv, ok := verifier.(*sigstoreBinaryVerifier)
	if !ok {
		t.Fatalf("managed verifier type = %T, want *sigstoreBinaryVerifier (real updater verifier)", verifier)
	}
	if sv.rootSource != "managed" {
		t.Errorf("managed verifier rootSource = %q, want managed", sv.rootSource)
	}

	binary := []byte("managed-update-binary-v1")
	customerEntity, err := customerVS.Sign(managedTestCustomerSAN, managedTestCustomerIssuer, binary)
	if err != nil {
		t.Fatalf("customer sign: %v", err)
	}
	if ok, reason := sv.verifyEntity(customerEntity, hexSHA256(binary)); !ok {
		t.Errorf("customer-signed binary: want valid, got invalid (%s)", reason)
	}
	vendorEntity, err := vendorVS.Sign(managedTestVendorSAN, managedTestVendorIssuer, binary)
	if err != nil {
		t.Fatalf("vendor sign: %v", err)
	}
	// The fixture verifies under the local vendor construction, so the
	// managed rejection below pins replacement rather than a broken
	// fixture: a union of managed and local trust would accept it.
	if lv := newAutoUpdateVerifier(second.Config().AutoUpdate); true {
		localSig, ok := lv.(*sigstoreBinaryVerifier)
		if !ok {
			t.Fatalf("local verifier type = %T, want *sigstoreBinaryVerifier", lv)
		}
		if ok, reason := localSig.verifyEntity(vendorEntity, hexSHA256(binary)); !ok {
			t.Fatalf("vendor bundle must verify under local trust: %s", reason)
		}
	}
	if ok, reason := sv.verifyEntity(vendorEntity, hexSHA256(binary)); ok {
		t.Errorf("vendor-only binary: want invalid (replacement, not union), got valid (%s)", reason)
	}
	// Same customer root, vendor identity: the managed pair pins the
	// customer identity, so this must fail. A union mutant that appends
	// the local (vendor) identities to the managed identities would
	// accept it, so this assertion kills that mutant independently of
	// roots (the root already matches the managed pair).
	crossEntity, err := customerVS.Sign(managedTestVendorSAN, managedTestVendorIssuer, binary)
	if err != nil {
		t.Fatalf("cross sign: %v", err)
	}
	if ok, reason := sv.verifyEntity(crossEntity, hexSHA256(binary)); ok {
		t.Errorf("vendor identity under customer root: want invalid (replacement, not union), got valid (%s)", reason)
	}
	if ok, reason := sv.verifyEntity(customerEntity, hexSHA256([]byte("tampered-binary"))); ok {
		t.Errorf("tampered binary: want invalid, got valid (%s)", reason)
	} else if reason == "" {
		t.Error("tampered binary: want explanation, got empty reason")
	}
}

func TestManagedUpdateTrust_WrongIdentityAndUntrustedRootFail(t *testing.T) {
	customerVS, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatalf("customer VS: %v", err)
	}
	otherVS, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatalf("other VS: %v", err)
	}
	proj := managedTestProjection(t, customerVS, managedTestCustomerSAN, managedTestCustomerIssuer)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "daemon.yaml")
	d := newManagedTestDaemon(t, configPath, true, []UpdateSigner{{SAN: managedTestCustomerSAN, Issuer: managedTestCustomerIssuer}})
	driveManagedProjectionThroughHeartbeat(t, d, proj)
	verifier := d.managedUpdateVerifier(d.Config().AutoUpdate)
	sv, ok := verifier.(*sigstoreBinaryVerifier)
	if !ok {
		t.Fatalf("managed verifier type = %T, want *sigstoreBinaryVerifier", verifier)
	}
	binary := []byte("managed-binary-identity")
	// Wrong customer identity under the trusted root.
	wrongEntity, err := customerVS.Sign("someone-else@example.com", managedTestCustomerIssuer, binary)
	if err != nil {
		t.Fatalf("wrong-identity sign: %v", err)
	}
	if ok, _ := sv.verifyEntity(wrongEntity, hexSHA256(binary)); ok {
		t.Error("wrong customer identity: want invalid, got valid (identity check bypassed)")
	}
	// Right identity under an untrusted root.
	untrustedEntity, err := otherVS.Sign(managedTestCustomerSAN, managedTestCustomerIssuer, binary)
	if err != nil {
		t.Fatalf("untrusted-root sign: %v", err)
	}
	if ok, _ := sv.verifyEntity(untrustedEntity, hexSHA256(binary)); ok {
		t.Error("right identity under untrusted root: want invalid, got valid (root check bypassed)")
	}
	// Right identity under the trusted root still passes.
	goodEntity, err := customerVS.Sign(managedTestCustomerSAN, managedTestCustomerIssuer, binary)
	if err != nil {
		t.Fatalf("good sign: %v", err)
	}
	if ok, reason := sv.verifyEntity(goodEntity, hexSHA256(binary)); !ok {
		t.Errorf("right identity under trusted root: want valid, got invalid (%s)", reason)
	}
}

func TestManagedUpdateTrust_OptedOutIgnoresRemotePolicy(t *testing.T) {
	customerVS, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatalf("customer VS: %v", err)
	}
	proj := managedTestProjection(t, customerVS, managedTestCustomerSAN, managedTestCustomerIssuer)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "daemon.yaml")
	local := []UpdateSigner{{SAN: "local@example.com", Issuer: "https://issuer.example"}}
	d := newManagedTestDaemon(t, configPath, false, local)
	driveManagedProjectionThroughHeartbeat(t, d, proj)
	d.managedTrustMu.RLock()
	active := d.managedTrustActive
	blocked := d.managedTrustBlocked
	d.managedTrustMu.RUnlock()
	if active != nil || blocked {
		t.Fatalf("opted-out host must ignore remote policy: active=%+v blocked=%v", active, blocked)
	}
	if _, err := os.Stat(managedUpdateTrustPath(configPath)); !os.IsNotExist(err) {
		t.Fatalf("opted-out host must not persist remote policy")
	}
	verifier := d.managedUpdateVerifier(d.Config().AutoUpdate)
	if _, isManaged := verifier.(*sigstoreBinaryVerifier); isManaged {
		// Local signers + embedded root also build a sigstore verifier, so
		// distinguish by rootSource: opted-out must use the local path,
		// never the managed pair.
		if sv := verifier.(*sigstoreBinaryVerifier); sv.rootSource == "managed" {
			t.Fatal("opted-out verifier uses managed pair, want local behavior")
		}
	}
}

func managedTestValidProjection(t *testing.T, vs *ca.VirtualSigstore) *ManagedUpdateTrustProjection {
	t.Helper()
	return managedTestProjection(t, vs, managedTestCustomerSAN, managedTestCustomerIssuer)
}

func TestManagedUpdateTrust_InvalidDigestRootOversizedUnavailableBlock(t *testing.T) {
	customerVS, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatalf("customer VS: %v", err)
	}
	valid := managedTestValidProjection(t, customerVS)
	validRootBytes, err := base64.StdEncoding.DecodeString(valid.TrustRoot)
	if err != nil {
		t.Fatalf("decode valid root: %v", err)
	}
	_ = validRootBytes

	cases := []struct {
		name   string
		mutate func(*ManagedUpdateTrustProjection)
	}{
		{"invalid root not base64", func(p *ManagedUpdateTrustProjection) { p.TrustRoot = "!!!not-base64!!!" }},
		{"invalid root not JSON", func(p *ManagedUpdateTrustProjection) {
			p.TrustRoot = base64.StdEncoding.EncodeToString([]byte("{not-json"))
			p.Digest = computeManagedUpdateTrustDigest(p.Signers, []byte("{not-json"))
		}},
		{"wrong digest", func(p *ManagedUpdateTrustProjection) { p.Digest = strings.Repeat("0", 64) }},
		{"malformed digest", func(p *ManagedUpdateTrustProjection) { p.Digest = "zz" }},
		{"no signers", func(p *ManagedUpdateTrustProjection) {
			p.Signers = nil
			p.Digest = computeManagedUpdateTrustDigest(nil, validRootBytes)
		}},
		{"oversized payload", func(p *ManagedUpdateTrustProjection) {
			big := make([]byte, maxManagedUpdateTrustRootBytes+1)
			p.TrustRoot = base64.StdEncoding.EncodeToString(big)
			p.Digest = strings.Repeat("a", 64)
		}},
		{"unavailable projection", func(p *ManagedUpdateTrustProjection) {
			p.Unavailable = true
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "daemon.yaml")
			d := newManagedTestDaemon(t, configPath, true, []UpdateSigner{{SAN: managedTestCustomerSAN, Issuer: managedTestCustomerIssuer}})
			// Seed a valid policy first so we can prove the bad projection
			// preserves the prior pair instead of partially replacing it.
			d.applyManagedUpdateTrustProjection(valid)
			before, err := os.ReadFile(managedUpdateTrustPath(configPath))
			if err != nil {
				t.Fatalf("seed valid: %v", err)
			}
			bad := *valid
			bad.Signers = append([]UpdateSigner(nil), valid.Signers...)
			tc.mutate(&bad)
			d.applyManagedUpdateTrustProjection(&bad)
			d.managedTrustMu.RLock()
			blocked := d.managedTrustBlocked
			d.managedTrustMu.RUnlock()
			if !blocked {
				t.Fatalf("invalid projection %q must block updates", tc.name)
			}
			after, err := os.ReadFile(managedUpdateTrustPath(configPath))
			if err != nil {
				t.Fatalf("read after invalid: %v", err)
			}
			if string(before) != string(after) {
				t.Fatalf("invalid projection %q partially replaced persisted pair", tc.name)
			}
			verifier := d.managedUpdateVerifier(d.Config().AutoUpdate)
			if _, ok := verifier.(failClosedVerifier); !ok {
				t.Fatalf("blocked verifier type = %T, want failClosedVerifier", verifier)
			}
			if ok, _ := verifier.Verify(context.Background(), strings.Repeat("a", 64), "anything"); ok {
				t.Fatalf("blocked verifier must refuse every swap")
			}
			// Valid retry recovers.
			d.applyManagedUpdateTrustProjection(valid)
			d.managedTrustMu.RLock()
			stillBlocked := d.managedTrustBlocked
			active := d.managedTrustActive
			d.managedTrustMu.RUnlock()
			if stillBlocked || active == nil || !strings.EqualFold(active.Digest, valid.Digest) {
				t.Fatalf("valid retry must recover: blocked=%v active=%+v", stillBlocked, active)
			}
		})
	}
}

func TestManagedUpdateTrust_DuplicateDeliveryHarmlessAndUnconfiguredLeavesLocal(t *testing.T) {
	customerVS, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatalf("customer VS: %v", err)
	}
	valid := managedTestValidProjection(t, customerVS)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "daemon.yaml")
	d := newManagedTestDaemon(t, configPath, true, []UpdateSigner{{SAN: managedTestCustomerSAN, Issuer: managedTestCustomerIssuer}})
	d.applyManagedUpdateTrustProjection(valid)
	before, err := os.ReadFile(managedUpdateTrustPath(configPath))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Duplicate delivery is a no-op.
	d.applyManagedUpdateTrustProjection(valid)
	after, err := os.ReadFile(managedUpdateTrustPath(configPath))
	if err != nil {
		t.Fatalf("read after duplicate: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("duplicate digest rewrote persisted pair")
	}
	d.managedTrustMu.RLock()
	blocked := d.managedTrustBlocked
	d.managedTrustMu.RUnlock()
	if blocked {
		t.Fatal("duplicate digest must not block")
	}
	// Unconfigured (nil) leaves local policy unchanged.
	d.applyManagedUpdateTrustProjection(nil)
	d.managedTrustMu.RLock()
	active := d.managedTrustActive
	d.managedTrustMu.RUnlock()
	if active == nil || !strings.EqualFold(active.Digest, valid.Digest) {
		t.Fatalf("nil projection must leave active policy unchanged")
	}
}

func TestManagedUpdateTrust_PersistenceFailureBlocksAndRecovers(t *testing.T) {
	customerVS, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatalf("customer VS: %v", err)
	}
	valid := managedTestValidProjection(t, customerVS)
	dir := t.TempDir()
	// Make the managed path a directory so persistence fails.
	configPath := filepath.Join(dir, "daemon.yaml")
	if err := os.MkdirAll(managedUpdateTrustPath(configPath), 0o700); err != nil {
		t.Fatalf("mkdir blocking path: %v", err)
	}
	d := newManagedTestDaemon(t, configPath, true, []UpdateSigner{{SAN: managedTestCustomerSAN, Issuer: managedTestCustomerIssuer}})
	d.applyManagedUpdateTrustProjection(valid)
	d.managedTrustMu.RLock()
	blocked := d.managedTrustBlocked
	reason := d.managedTrustBlockedReason
	d.managedTrustMu.RUnlock()
	if !blocked {
		t.Fatal("persistence failure must block updates")
	}
	if strings.TrimSpace(reason) == "" {
		t.Fatal("persistence failure must carry a reason")
	}
	verifier := d.managedUpdateVerifier(d.Config().AutoUpdate)
	if _, ok := verifier.(failClosedVerifier); !ok {
		t.Fatalf("persistence-failure verifier = %T, want failClosedVerifier", verifier)
	}
	// Repair the path and retry: valid retry recovers.
	if err := os.Remove(managedUpdateTrustPath(configPath)); err != nil {
		t.Fatalf("remove blocking dir: %v", err)
	}
	d.managedTrustMu.Lock()
	d.managedTrustLoaded = false
	d.managedTrustBlocked = false
	d.managedTrustBlockedReason = ""
	d.managedTrustActive = nil
	d.managedTrustMu.Unlock()
	d.applyManagedUpdateTrustProjection(valid)
	d.managedTrustMu.RLock()
	stillBlocked := d.managedTrustBlocked
	active := d.managedTrustActive
	d.managedTrustMu.RUnlock()
	if stillBlocked || active == nil {
		t.Fatalf("valid retry after repair must recover: blocked=%v active=%+v", stillBlocked, active)
	}
}
