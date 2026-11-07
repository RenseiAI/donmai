// Package daemon managed_update_trust.go — coordinator-managed update trust.
//
// An opted-in host lets its already configured coordinator rotate the
// binary-verification trust (signer allowlist + sigstore trust root) over
// the heartbeat channel. The managed pair is a REPLACEMENT for the local
// autoUpdate.signers/trustRootPath, never a union with built-in trust.
//
// Wire: heartbeatResponseBody.managedUpdateTrust carries a
// ManagedUpdateTrustProjection {digest, signers, trustRoot}. trustRoot is
// the public trust-root JSON bytes base64-encoded; no filesystem paths or
// private keys cross the channel. digest is the hex SHA-256 over the
// canonical signer encoding plus the raw root bytes (see
// computeManagedUpdateTrustDigest); repeated delivery of the same digest
// is a no-op.
//
// Validation uses the production parser and the production identity
// policy builder (root.NewTrustedRootFromJSON,
// buildUpdateIdentityPolicies) before any active trust changes. The
// complete policy persists atomically in the daemon's managed config area
// (a single JSON file beside daemon.yaml written via temp + rename with
// directory fsync); the daemon selects the location, callers never choose
// filesystem destinations. The updater sees either the complete old pair
// or the complete new pair — including after restart — because readers
// hold managedTrustMu while swapping/deriving the verifier and the
// restarter reloads the persisted file before verifying.
//
// States (see PR body transition table):
//   unmanaged (opt-out) ignores projections, keeps local behavior;
//   opted-in + valid -> active policy;
//   same digest -> unchanged (no-op);
//   invalid/unavailable/write failure -> updates refused (fail-closed)
//     with the prior persisted policy retained;
//   next valid policy -> active.
package daemon

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sigstore/sigstore-go/pkg/root"
)

// Managed trust bounds. Oversized projections refuse without a swap.
const (
	maxManagedUpdateTrustSigners   = 16
	maxManagedUpdateTrustRootBytes = 512 * 1024
	// Base64 inflates raw bytes by ~4/3; cap the encoded form with margin.
	maxManagedUpdateTrustRootEncoded = 768 * 1024
)

// managedUpdateTrustFileName is the daemon-selected persistence location
// for the managed policy, beside daemon.yaml. Callers never choose it.
const managedUpdateTrustFileName = "managed-update-trust.json"

// ManagedUpdateTrustProjection is the heartbeat wire shape for a managed
// trust rotation. Nil (field absent) means unconfigured: leave local
// policy unchanged. Non-nil with Unavailable means the coordinator has no
// managed opinion right now: opted-in hosts block managed updates until a
// valid projection arrives. Otherwise Digest/Signers/TrustRoot carry the
// complete replacement pair.
type ManagedUpdateTrustProjection struct {
	Digest string `json:"digest"`
	// Signers pins the replacement identities (exact issuer + subject
	// matching via buildUpdateIdentityPolicies, the same builder the
	// local auto-update verifier uses).
	Signers []UpdateSigner `json:"signers"`
	// TrustRoot is the public trust-root JSON bytes, base64-encoded.
	TrustRoot string `json:"trustRoot"`
	// Unavailable marks an explicit no-opinion projection. Distinct from
	// a nil (absent) projection, which leaves local policy unchanged.
	Unavailable bool `json:"unavailable,omitempty"`
}

// managedUpdateTrustPolicy is the persisted + in-memory active pair.
type managedUpdateTrustPolicy struct {
	Digest    string         `json:"digest"`
	Signers   []UpdateSigner `json:"signers"`
	TrustRoot string         `json:"trustRoot"`
	UpdatedAt string         `json:"updatedAt"`
}

// managedUpdateTrustOptedIn reports the explicit local opt-in for the
// already configured coordinator: managedTrust.enabled plus a configured
// coordinator URL. Opted-out hosts ignore remote projections entirely.
func managedUpdateTrustOptedIn(cfg *Config) bool {
	if cfg == nil {
		return false
	}
	if !cfg.AutoUpdate.ManagedTrust.Enabled {
		return false
	}
	return strings.TrimSpace(cfg.Orchestrator.URL) != ""
}

// managedUpdateTrustPath returns the daemon-selected persistence path for
// the managed policy. Empty when no config path is configured.
func managedUpdateTrustPath(configPath string) string {
	if strings.TrimSpace(configPath) == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(configPath), managedUpdateTrustFileName)
}

// computeManagedUpdateTrustDigest returns the hex SHA-256 over the
// canonical signer encoding plus the raw root bytes. The producer and the
// daemon must compute the same value; a mismatch refuses the projection.
func computeManagedUpdateTrustDigest(signers []UpdateSigner, rootBytes []byte) string {
	signersJSON, _ := json.Marshal(signers)
	h := sha256.New()
	_, _ = h.Write(signersJSON)
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(rootBytes)
	return hex.EncodeToString(h.Sum(nil))
}

// validateManagedUpdateTrustProjection checks bounds, decodes the root,
// parses it with the production parser, builds the production identity
// policy (exact issuer/subject matching), and compares the digest. It
// returns the decoded root bytes on success.
func validateManagedUpdateTrustProjection(proj *ManagedUpdateTrustProjection) ([]byte, error) {
	if proj == nil {
		return nil, fmt.Errorf("managed update trust: missing projection")
	}
	if proj.Unavailable {
		return nil, fmt.Errorf("managed update trust: projection unavailable")
	}
	if len(proj.Signers) == 0 {
		return nil, fmt.Errorf("managed update trust: no signers configured")
	}
	if len(proj.Signers) > maxManagedUpdateTrustSigners {
		return nil, fmt.Errorf("managed update trust: too many signers (%d > %d)", len(proj.Signers), maxManagedUpdateTrustSigners)
	}
	if strings.TrimSpace(proj.TrustRoot) == "" {
		return nil, fmt.Errorf("managed update trust: empty trust root")
	}
	if len(proj.TrustRoot) > maxManagedUpdateTrustRootEncoded {
		return nil, fmt.Errorf("managed update trust: trust root payload oversized")
	}
	rawDigest := strings.TrimSpace(proj.Digest)
	decoded, err := hex.DecodeString(rawDigest)
	if err != nil || len(decoded) != sha256.Size {
		return nil, fmt.Errorf("managed update trust: malformed digest %q", proj.Digest)
	}
	rootBytes, err := base64.StdEncoding.DecodeString(strings.TrimSpace(proj.TrustRoot))
	if err != nil {
		return nil, fmt.Errorf("managed update trust: trust root is not base64: %w", err)
	}
	if len(rootBytes) == 0 {
		return nil, fmt.Errorf("managed update trust: empty trust root")
	}
	if len(rootBytes) > maxManagedUpdateTrustRootBytes {
		return nil, fmt.Errorf("managed update trust: trust root oversized (%d > %d)", len(rootBytes), maxManagedUpdateTrustRootBytes)
	}
	if _, err := buildUpdateIdentityPolicies(proj.Signers); err != nil {
		return nil, fmt.Errorf("managed update trust: invalid signer: %w", err)
	}
	if _, err := root.NewTrustedRootFromJSON(rootBytes); err != nil {
		return nil, fmt.Errorf("managed update trust: invalid trust root: %w", err)
	}
	want := computeManagedUpdateTrustDigest(proj.Signers, rootBytes)
	if !strings.EqualFold(want, rawDigest) {
		return nil, fmt.Errorf("managed update trust: digest mismatch")
	}
	return rootBytes, nil
}

// loadManagedUpdateTrustPolicy reads the persisted policy and validates it
// with the production parser. A missing file is not an error (no managed
// policy yet).
func loadManagedUpdateTrustPolicy(path string) (*managedUpdateTrustPolicy, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("managed update trust: no durable state path")
	}
	data, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("managed update trust: durable state unreadable: %w", err)
	}
	var policy managedUpdateTrustPolicy
	if err := json.Unmarshal(data, &policy); err != nil {
		return nil, fmt.Errorf("managed update trust: durable state corrupt: %w", err)
	}
	if strings.TrimSpace(policy.Digest) == "" || len(policy.Signers) == 0 || strings.TrimSpace(policy.TrustRoot) == "" {
		return nil, fmt.Errorf("managed update trust: durable state corrupt: incomplete policy")
	}
	rootBytes, err := base64.StdEncoding.DecodeString(strings.TrimSpace(policy.TrustRoot))
	if err != nil {
		return nil, fmt.Errorf("managed update trust: durable state corrupt: trust root is not base64: %w", err)
	}
	if len(rootBytes) == 0 || len(rootBytes) > maxManagedUpdateTrustRootBytes {
		return nil, fmt.Errorf("managed update trust: durable state corrupt: trust root bounds")
	}
	if _, err := buildUpdateIdentityPolicies(policy.Signers); err != nil {
		return nil, fmt.Errorf("managed update trust: durable state corrupt: invalid signer: %w", err)
	}
	if _, err := root.NewTrustedRootFromJSON(rootBytes); err != nil {
		return nil, fmt.Errorf("managed update trust: durable state corrupt: invalid trust root: %w", err)
	}
	want := computeManagedUpdateTrustDigest(policy.Signers, rootBytes)
	if !strings.EqualFold(want, strings.TrimSpace(policy.Digest)) {
		return nil, fmt.Errorf("managed update trust: durable state corrupt: digest mismatch")
	}
	return &policy, nil
}

// persistManagedUpdateTrustPolicy atomically records the complete policy.
// The write is temp + rename with file and directory fsync so a crash
// leaves the previous durable policy intact.
func persistManagedUpdateTrustPolicy(path string, policy managedUpdateTrustPolicy) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("managed update trust: no durable state path")
	}
	data, err := json.Marshal(policy)
	if err != nil {
		return fmt.Errorf("managed update trust: encode failed: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("managed update trust: durable state not writable: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "managed-update-trust-*.tmp")
	if err != nil {
		return fmt.Errorf("managed update trust: durable state not writable: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("managed update trust: durable state write failed: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("managed update trust: durable state write failed: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("managed update trust: durable state write failed: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("managed update trust: durable state write failed: %w", err)
	}
	dirFile, err := os.Open(dir) //nolint:gosec
	if err != nil {
		return fmt.Errorf("managed update trust: durable state directory not syncable: %w", err)
	}
	syncErr := dirFile.Sync()
	closeErr := dirFile.Close()
	if syncErr != nil {
		return fmt.Errorf("managed update trust: durable state directory sync failed: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("managed update trust: durable state directory sync failed: %w", closeErr)
	}
	return nil
}

// ensureManagedTrustLoaded lazily loads the persisted policy once per
// process. Corrupt/unreadable state fails closed (blocks updates) rather
// than falling back to local or built-in trust.
func (d *Daemon) ensureManagedTrustLoaded() {
	d.managedTrustMu.Lock()
	defer d.managedTrustMu.Unlock()
	if d.managedTrustLoaded {
		return
	}
	d.managedTrustLoaded = true
	policy, err := loadManagedUpdateTrustPolicy(managedUpdateTrustPath(d.opts.ConfigPath))
	if err != nil {
		d.managedTrustActive = nil
		d.managedTrustBlocked = true
		d.managedTrustBlockedReason = err.Error()
		return
	}
	d.managedTrustActive = policy
	d.managedTrustBlocked = false
	d.managedTrustBlockedReason = ""
}

// applyManagedUpdateTrustProjection is the heartbeat-goroutine entry point
// for the managedUpdateTrust response field. Nil means unconfigured: leave
// local policy unchanged. Opted-out hosts ignore every projection.
func (d *Daemon) applyManagedUpdateTrustProjection(proj *ManagedUpdateTrustProjection) {
	if proj == nil {
		return
	}
	cfg := d.Config()
	if !managedUpdateTrustOptedIn(cfg) {
		return
	}
	d.ensureManagedTrustLoaded()
	d.managedTrustMu.Lock()
	defer d.managedTrustMu.Unlock()
	if d.managedTrustActive != nil && strings.EqualFold(strings.TrimSpace(d.managedTrustActive.Digest), strings.TrimSpace(proj.Digest)) {
		// Repeated digest is a no-op: duplicate delivery is harmless and
		// must not rewrite or re-block.
		return
	}
	rootBytes, err := validateManagedUpdateTrustProjection(proj)
	if err != nil {
		// Malformed/unavailable: block new managed updates until a valid
		// projection arrives, preserve the prior persisted policy, never
		// enable built-in fallback. The file is untouched; only the
		// in-memory gate flips to fail-closed.
		d.managedTrustBlocked = true
		d.managedTrustBlockedReason = err.Error()
		return
	}
	policy := managedUpdateTrustPolicy{
		Digest:    strings.TrimSpace(proj.Digest),
		Signers:   append([]UpdateSigner(nil), proj.Signers...),
		TrustRoot: strings.TrimSpace(proj.TrustRoot),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	_ = rootBytes
	if err := persistManagedUpdateTrustPolicy(managedUpdateTrustPath(d.opts.ConfigPath), policy); err != nil {
		d.managedTrustBlocked = true
		d.managedTrustBlockedReason = err.Error()
		return
	}
	cp := policy
	d.managedTrustActive = &cp
	d.managedTrustBlocked = false
	d.managedTrustBlockedReason = ""
}

// managedUpdateVerifier returns the BinaryVerifier for the effective trust:
// the managed pair when opted-in and active, fail-closed when blocked,
// local config otherwise. All daemon update entry points must use this so
// they share one active pair. The managed pair is a replacement, never a
// union with local or built-in trust.
func (d *Daemon) managedUpdateVerifier(cfg AutoUpdateConfig) BinaryVerifier {
	var local Config
	local.Orchestrator.URL = d.orchestratorURLForManagedOptIn()
	local.AutoUpdate = cfg
	// Prefer the live daemon config for the opt-in check so a test that
	// mutates d.config without rebuilding cfg still gates correctly; fall
	// back to the passed cfg's ManagedTrust flag when the daemon has no
	// config yet (e.g. early startup helpers).
	effectiveCfg := d.Config()
	if effectiveCfg == nil {
		effectiveCfg = &local
	}
	if !managedUpdateTrustOptedIn(effectiveCfg) {
		return newAutoUpdateVerifier(cfg)
	}
	d.ensureManagedTrustLoaded()
	d.managedTrustMu.RLock()
	active := d.managedTrustActive
	blocked := d.managedTrustBlocked
	reason := d.managedTrustBlockedReason
	d.managedTrustMu.RUnlock()
	if blocked {
		if strings.TrimSpace(reason) == "" {
			reason = "managed update trust is unavailable; refusing swap"
		}
		return failClosedVerifier{reason: reason + "; refusing swap"}
	}
	if active == nil {
		// Unconfigured: leave local policy unchanged.
		return newAutoUpdateVerifier(cfg)
	}
	rootBytes, err := base64.StdEncoding.DecodeString(strings.TrimSpace(active.TrustRoot))
	if err != nil {
		return failClosedVerifier{reason: "managed update trust: persisted root undecodable; refusing swap"}
	}
	material, err := root.NewTrustedRootFromJSON(rootBytes)
	if err != nil {
		return failClosedVerifier{reason: fmt.Sprintf("managed update trust: persisted root invalid: %v; refusing swap", err)}
	}
	identities, err := buildUpdateIdentityPolicies(active.Signers)
	if err != nil {
		return failClosedVerifier{reason: fmt.Sprintf("managed update trust: persisted signer invalid: %v; refusing swap", err)}
	}
	return &sigstoreBinaryVerifier{material: material, identities: identities, rootSource: "managed"}
}

// orchestratorURLForManagedOptIn reads the current orchestrator URL without
// holding managedTrustMu. Used only as a startup fallback in
// managedUpdateVerifier when the daemon config is not yet loaded.
func (d *Daemon) orchestratorURLForManagedOptIn() string {
	cfg := d.Config()
	if cfg == nil {
		return ""
	}
	return cfg.Orchestrator.URL
}

// newManagedSigstoreVerifierForTest builds a verifier from explicit managed
// bytes for tests that exercise the managed pair without a Daemon. It uses
// the production parser and identity builder.
func newManagedSigstoreVerifierForTest(signers []UpdateSigner, rootBytes []byte) (*sigstoreBinaryVerifier, error) {
	material, err := root.NewTrustedRootFromJSON(rootBytes)
	if err != nil {
		return nil, err
	}
	identities, err := buildUpdateIdentityPolicies(signers)
	if err != nil {
		return nil, err
	}
	return &sigstoreBinaryVerifier{material: material, identities: identities, rootSource: "managed"}, nil
}

