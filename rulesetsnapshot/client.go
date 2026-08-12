package rulesetsnapshot

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// maxResponseBytes bounds how much of a snapshot or JWKS response this
// package will read, regardless of what a misbehaving or malicious
// publisher sends. Ten megabytes comfortably covers a real org's compiled
// bundle (compile-time validators on the publishing side already bound
// section sizes) with headroom, and rejecting past it is a fetch failure,
// not a panic or an unbounded allocation.
const maxResponseBytes = 10 << 20

// DefaultDegradedAfter / DefaultRefuseAfter are the OSS-shipped bounds
// (05-sota-research.md §A5's "bounded, exposed staleness"): a cached
// snapshot younger than DefaultDegradedAfter is fully fresh; between the two
// bounds it is still served but flagged degraded=true on every surface that
// reports it; past DefaultRefuseAfter a claim evaluation refuses loudly
// (ExpiredError) rather than trusting data that old. An embedder with a
// tighter or looser outage-tolerance requirement overrides both via Config.
const (
	DefaultDegradedAfter = 5 * time.Minute
	DefaultRefuseAfter   = 30 * time.Minute
)

// Config configures a Client's snapshot source. The zero value is the
// self-hosted default: Endpoint == "" means Configured() reports false and
// every claim-gate caller must treat that identically to "no opinion" — see
// doc.go.
type Config struct {
	// Endpoint is the full URL the daemon GETs for the current snapshot.
	// Supplied entirely by the embedding composer — this package never
	// hardcodes or derives a default. Empty disables the feature.
	Endpoint string
	// AuthHeader, when non-empty, is sent verbatim as the request's
	// Authorization header (e.g. "Bearer <token>"). Never logged, never
	// persisted to disk.
	AuthHeader string
	// TrustedKeys is a static, pinned signingKeyId -> Ed25519 public key
	// map. Checked before JWKSURL; the only resolution path that touches no
	// network.
	TrustedKeys map[string]ed25519.PublicKey
	// JWKSURL, when set, is fetched (RFC 7517 JWKS, cached for
	// jwksKeyCacheTTL) to resolve a signingKeyId TrustedKeys does not cover
	// — e.g. after a key rotation on the publisher side. The embedder
	// resolves any org/workspace path segment itself; this package treats
	// JWKSURL as a fully-formed URL.
	JWKSURL string
	// DegradedAfter / RefuseAfter bound cached-snapshot staleness. Zero
	// values fall back to DefaultDegradedAfter / DefaultRefuseAfter.
	// RefuseAfter must be >= DegradedAfter — see NewClient.
	DegradedAfter time.Duration
	RefuseAfter   time.Duration
	// StatePath overrides the on-disk last-known-good location. Empty uses
	// the brand state directory (statepath.Resolve).
	StatePath string
	// HTTPClient overrides the default (a client with a bounded 10s
	// timeout).
	HTTPClient *http.Client
	// Now overrides time.Now, for tests only.
	Now func() time.Time
}

// Client is the daemon-side ruleset-snapshot cache: fetch, verify, persist,
// serve. Safe for concurrent use. Construct with NewClient.
type Client struct {
	cfg Config

	mu      sync.RWMutex
	current *Snapshot
	lastErr error

	jwksMu        sync.Mutex
	jwks          map[string]ed25519.PublicKey
	jwksFetchedAt time.Time
}

// NewClient constructs a Client and best-effort loads any on-disk
// last-known-good snapshot (re-verified — see loadPersisted). Returns an
// error only for a genuine misconfiguration: an Endpoint with no way to
// ever verify anything (neither TrustedKeys nor JWKSURL) is rejected here,
// at construction, rather than silently failing verification forever on
// every later Refresh — never accepting unverifiable data is the whole
// point of this package, and that includes never accepting it by omission.
func NewClient(cfg Config) (*Client, error) {
	if cfg.Endpoint != "" && len(cfg.TrustedKeys) == 0 && cfg.JWKSURL == "" {
		return nil, fmt.Errorf("rulesetsnapshot: Endpoint is configured but neither TrustedKeys nor JWKSURL is set — a snapshot could never be verified")
	}
	if cfg.DegradedAfter <= 0 {
		cfg.DegradedAfter = DefaultDegradedAfter
	}
	if cfg.RefuseAfter <= 0 {
		cfg.RefuseAfter = DefaultRefuseAfter
	}
	if cfg.RefuseAfter < cfg.DegradedAfter {
		return nil, fmt.Errorf("rulesetsnapshot: RefuseAfter (%s) must be >= DegradedAfter (%s)", cfg.RefuseAfter, cfg.DegradedAfter)
	}
	c := &Client{cfg: cfg}
	c.loadPersisted()
	return c, nil
}

func (c *Client) now() time.Time {
	if c.cfg.Now != nil {
		return c.cfg.Now()
	}
	return time.Now()
}

func (c *Client) httpClient() *http.Client {
	if c.cfg.HTTPClient != nil {
		return c.cfg.HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// Configured reports whether a snapshot source was ever set up
// (Config.Endpoint != ""). The "no behaviour change when unconfigured"
// invariant is enforced by callers checking this before treating a
// Client's absence of an opinion as anything but a no-op — see
// daemon.FailStaticClaimGateProvider.
func (c *Client) Configured() bool { return c.cfg.Endpoint != "" }

// RefuseAfter returns the configured (or defaulted) hard TTL bound.
func (c *Client) RefuseAfter() time.Duration { return c.cfg.RefuseAfter }

// DegradedAfter returns the configured (or defaulted) soft staleness bound.
func (c *Client) DegradedAfter() time.Duration { return c.cfg.DegradedAfter }

// LastError returns the most recent Refresh (or persisted-load) failure,
// if any — a typed health warning surfacing a rejected snapshot, distinct
// from Current()'s ok=false (which only means "nothing has ever
// verified").
func (c *Client) LastError() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastErr
}

// Current returns the most recently verified Snapshot and its freshly
// computed Status. ok is false only when nothing has EVER verified — not
// even a persisted copy. Age/Degraded in Status are always computed
// relative to now, never frozen at fetch time (Consul `Age` semantics).
func (c *Client) Current() (Snapshot, Status, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.current == nil {
		return Snapshot{}, Status{}, false
	}
	age := c.now().Sub(c.current.CompiledAt)
	if age < 0 {
		age = 0
	}
	status := Status{
		Rev:        c.current.RulesetRev,
		Age:        age,
		Degraded:   age > c.cfg.DegradedAfter,
		CompiledAt: c.current.CompiledAt,
	}
	return *c.current, status, true
}

// Refresh fetches, verifies, and — on success — adopts and durably persists
// the publisher's current snapshot. It NEVER adopts a snapshot that fails
// verification: a transport error, a non-200 status, a content-hash
// mismatch, or a bad/unresolvable signature all leave the previously cached
// Snapshot (if any) exactly as it was, and are surfaced as a returned error
// AND recorded in LastError() — a rejected snapshot is a loud, typed health
// event, never a silent no-op.
func (c *Client) Refresh(ctx context.Context) error {
	if !c.Configured() {
		return ErrNotConfigured
	}
	raw, err := c.fetch(ctx)
	if err != nil {
		c.recordErr(err)
		return err
	}
	snap, err := c.parseAndVerify(ctx, raw)
	if err != nil {
		c.recordErr(err)
		return err
	}

	c.mu.Lock()
	c.current = &snap
	c.lastErr = nil
	c.mu.Unlock()

	// Persistence is a durability nicety on top of an already-successful,
	// already-adopted verification — a disk write failure must not undo the
	// in-memory success (a daemon on a read-only filesystem still gets the
	// fail-static benefit for the life of this process).
	if err := c.persist(raw); err != nil {
		c.recordErr(fmt.Errorf("verified snapshot adopted in memory but persist failed: %w", err))
	}
	return nil
}

func (c *Client) recordErr(err error) {
	c.mu.Lock()
	c.lastErr = err
	c.mu.Unlock()
}

func (c *Client) fetch(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.Endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("rulesetsnapshot: build request: %w", err)
	}
	if c.cfg.AuthHeader != "" {
		req.Header.Set("Authorization", c.cfg.AuthHeader)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("rulesetsnapshot: fetch snapshot: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("rulesetsnapshot: read snapshot response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rulesetsnapshot: snapshot endpoint returned status %d", resp.StatusCode)
	}
	return body, nil
}

// parseAndVerify decodes raw and verifies it end to end: well-formed JSON,
// a recognized algorithm, a matching content hash, and a signature that
// verifies against a resolvable trusted key. Shared by Refresh (network
// bytes) and loadPersisted (disk bytes, via context.Background()) so both
// paths enforce the identical invariant — a Snapshot value only ever exists
// once fully verified.
func (c *Client) parseAndVerify(ctx context.Context, raw []byte) (Snapshot, error) {
	var wire wireSnapshot
	if err := json.Unmarshal(raw, &wire); err != nil {
		return Snapshot{}, fmt.Errorf("%w: decode response: %v", ErrVerificationFailed, err)
	}
	if wire.Algorithm != "ed25519" {
		return Snapshot{}, fmt.Errorf("%w: unsupported algorithm %q", ErrVerificationFailed, wire.Algorithm)
	}
	if strings.TrimSpace(wire.SigningKeyID) == "" {
		return Snapshot{}, fmt.Errorf("%w: missing signingKeyId", ErrVerificationFailed)
	}

	computedHash, err := contentHash(wire.Sections)
	if err != nil {
		return Snapshot{}, err
	}
	if computedHash != wire.ContentHash {
		return Snapshot{}, fmt.Errorf("%w: content hash mismatch (computed %s, claimed %s)", ErrVerificationFailed, computedHash, wire.ContentHash)
	}

	pub, err := c.resolvePublicKey(ctx, wire.SigningKeyID)
	if err != nil {
		return Snapshot{}, err
	}
	if err := verifySignature(pub, wire.ContentHash, wire.Signature); err != nil {
		return Snapshot{}, err
	}

	compiledAt, err := time.Parse(time.RFC3339Nano, wire.CompiledAt)
	if err != nil {
		return Snapshot{}, fmt.Errorf("%w: parse compiledAt: %v", ErrVerificationFailed, err)
	}
	sections, err := decodeTypedSections(wire.Sections)
	if err != nil {
		return Snapshot{}, fmt.Errorf("%w: %v", ErrVerificationFailed, err)
	}

	return Snapshot{
		OrgID:        wire.OrgID,
		Revision:     wire.Revision,
		RulesetRev:   wire.RulesetRev,
		ContentHash:  wire.ContentHash,
		SigningKeyID: wire.SigningKeyID,
		CompiledAt:   compiledAt,
		Sections:     sections,
	}, nil
}

// Start runs Refresh once immediately and then on interval until ctx is
// canceled. It is fire-and-forget: Refresh errors are already captured by
// LastError()/Current()'s degraded accounting, so Start does not return
// them. A no-op (returns immediately, no goroutine) when the Client is not
// Configured. Callers that want tighter control (their own scheduler,
// jittered backoff, a health-check hook) can ignore Start and call
// Refresh directly instead.
func (c *Client) Start(ctx context.Context, interval time.Duration) {
	if !c.Configured() || interval <= 0 {
		return
	}
	go func() {
		_ = c.Refresh(ctx)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = c.Refresh(ctx)
			}
		}
	}()
}
