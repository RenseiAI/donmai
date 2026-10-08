package confinement

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// selfTestCacheVersion versions the cache file shape and the degraded
// contract: v2 rejects the v1 records that predate degraded marking, so a
// passing record taken before it can never attest on a host the scope
// layer leaves partial.
const selfTestCacheVersion = 2

// selfTestCacheKey is everything a cached record proves the self-test for.
// A record is reused only for the exact key it was taken under.
type selfTestCacheKey struct {
	Backend          BackendName `json:"backend"`
	BackendVersion   string      `json:"backendVersion"`
	ProbeSetVersion  string      `json:"probeSetVersion"`
	ExecutableDigest string      `json:"executableDigest"`
	ProbeDigest      string      `json:"probeDigest"`
	Home             string      `json:"home"`
	StateHome        string      `json:"stateHome"`
	ProfileDir       string      `json:"profileDir"`
}

// selfTestCacheEntry is one cache file.
type selfTestCacheEntry struct {
	Version int              `json:"version"`
	Key     selfTestCacheKey `json:"key"`
	Record  SelfTestRecord   `json:"record"`
}

// selfTestCache is the cache slot for one key.
type selfTestCache struct {
	path string
	key  selfTestCacheKey
	ttl  time.Duration
	now  fingerprint
}

// selfTestCache returns the cache slot for this self-test, or false when
// caching is off: no cache directory, a composer callback, or a probe
// executable that cannot be read and digested.
func (c *Confiner) selfTestCache(opts SelfTestOptions, current fingerprint) (selfTestCache, bool) {
	if opts.CacheDir == "" || !filepath.IsAbs(opts.CacheDir) || c.opts.ExtraRules != nil {
		return selfTestCache{}, false
	}
	probeDigest, err := executableDigest(opts.ProbeCommand[0])
	if err != nil {
		return selfTestCache{}, false
	}
	key := selfTestCacheKey{
		Backend:          current.backend,
		BackendVersion:   current.backendVersion,
		ProbeSetVersion:  current.probeSetVersion,
		ExecutableDigest: current.executableDigest,
		ProbeDigest:      probeDigest,
		Home:             c.opts.Home,
		StateHome:        c.opts.StateHome,
		ProfileDir:       c.opts.ProfileDir,
	}
	raw, err := json.Marshal(key)
	if err != nil {
		return selfTestCache{}, false
	}
	sum := sha256.Sum256(raw)
	ttl := opts.CacheTTL
	if ttl <= 0 {
		ttl = DefaultSelfTestCacheTTL
	}
	return selfTestCache{
		path: filepath.Join(opts.CacheDir, "selftest-"+hex.EncodeToString(sum[:16])+".json"),
		key:  key,
		ttl:  ttl,
		now:  current,
	}, true
}

// load returns the cached record when it is reusable at now: the same
// cache version and key, passing in both session modes, current by the
// staleness fingerprint, intact by its own digest, not degraded (a
// partial-boundary record is never reused as a passing one — the verdict
// is re-checked against this boot's kernel below), and taken within the
// future). Anything else is a miss.
func (s selfTestCache) load(now time.Time) (SelfTestRecord, bool) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return SelfTestRecord{}, false
	}
	var entry selfTestCacheEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return SelfTestRecord{}, false
	}
	record := entry.Record
	switch {
	case entry.Version != selfTestCacheVersion, entry.Key != s.key:
		return SelfTestRecord{}, false
	case !record.Passed, record.Degraded != "", len(record.SessionModes) != 2, !allPass(record.Probes):
		return SelfTestRecord{}, false
	case record.fingerprint() != s.now:
		return SelfTestRecord{}, false
	case record.Digest == "" || record.computeDigest() != record.Digest:
		return SelfTestRecord{}, false
	case record.TestedAt.After(now.Add(time.Minute)), now.Sub(record.TestedAt) >= s.ttl:
		return SelfTestRecord{}, false
	}
	// The record predates knowing the scope floor (or the kernel changed
	// under it): a passing record from a kernel without the scope layer
	// proves a partial boundary only. Re-derive the verdict from this
	// boot's kernel before reuse, so an old cache entry — or a stale one
	// written on another kernel — never attests as a full boundary.
	if ok, _ := ScopesAvailable(); !ok {
		return SelfTestRecord{}, false
	}
	return record, true
}

// save writes a passing record atomically with owner-only permissions. A
// cache that cannot be written only costs the next process a self-test.
func (s selfTestCache) save(record SelfTestRecord) {
	raw, err := json.Marshal(selfTestCacheEntry{Version: selfTestCacheVersion, Key: s.key, Record: record})
	if err != nil {
		return
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".selftest-*")
	if err != nil {
		return
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	_ = os.Rename(tmp.Name(), s.path)
}

// executableDigest returns the sha256 digest of the file argv0 names,
// resolving a bare name on PATH as exec does.
func executableDigest(argv0 string) (string, error) {
	path := argv0
	if !filepath.IsAbs(path) {
		resolved, err := exec.LookPath(path)
		if err != nil {
			return "", err
		}
		path = resolved
	}
	f, err := os.Open(path) //nolint:gosec // G304: the probe executable the caller names.
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}
