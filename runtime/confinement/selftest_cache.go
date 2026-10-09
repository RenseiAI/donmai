package confinement

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/runtime/statehome"
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

// latestSelfTestCacheDir is the host state directory workers record
// proven self-tests under: every seat self-test that passes keeps its
// record there (see recordProvenSelfTest), so later processes on this host
// read the posture back without re-probing. It mirrors the seat layer's
// cache directory (productionConfinementDirs in the harness adapter): both
// resolve under the same host state home, so the status path reads what
// the seats wrote.
func latestSelfTestCacheDir() string {
	if dir := statehome.StateDir("confinement-selftest"); dir != "" {
		return dir
	}
	return ""
}

// recordProvenSelfTest keeps a proven self-test record where the status
// path reads it back: one file per proven record, written atomically with
// owner-only permissions. Only passing, whole records are kept — a
// degraded or failing record proves no boundary to attest. A host where
// the state directory cannot be written keeps no record: the next status
// reader reports the boundary unproven instead of a boundary no seat
// proved. Secret-free by construction: the record carries no credential
// and no raw path (see Record).
func recordProvenSelfTest(record SelfTestRecord) {
	if !record.Passed || record.Degraded != "" || len(record.SessionModes) == 0 {
		return
	}
	dir := latestSelfTestCacheDir()
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	raw, err := json.Marshal(provenSelfTestFile{Version: provenSelfTestFileVersion, Record: record})
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".proven-*")
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
	_ = os.Rename(tmp.Name(), filepath.Join(dir, "proven-"+provenSelfTestName(record.Digest)+".json"))
}

// provenSelfTestName maps a record digest to its file name: the hex part
// of the sha256 digest, so the name carries no path, no probe value and
// no executable identity beyond the digest itself. An empty digest (a
// record that never computed one) keeps no file.
func provenSelfTestName(digest string) string {
	name, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || name == "" {
		return "unproven"
	}
	return name
}

// provenSelfTestFileVersion versions the proven-record file shape the
// status path reads back. A record the shape cannot decode is not proof.
const provenSelfTestFileVersion = 1

// provenSelfTestFile is one proven self-test record on disk.
type provenSelfTestFile struct {
	Version int            `json:"version"`
	Record  SelfTestRecord `json:"record"`
}

// LatestProvenRecord reports the latest proven seat self-test on this
// host, if one was recorded: the newest passing, whole record under the
// shared proven directory whose digest still covers it. Anything else —
// no file, an undecodable shape, a version the binary does not read, a
// degraded or failing record, a digest that no longer covers it — is no
// evidence, so the status path reports the boundary unproven instead of
// attesting from it.
func LatestProvenRecord() (SelfTestRecord, bool) {
	dir := latestSelfTestCacheDir()
	if dir == "" {
		return SelfTestRecord{}, false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return SelfTestRecord{}, false
	}
	var latest SelfTestRecord
	found := false
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "proven-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name())) //nolint:gosec // G304: entry names come from the proven-record directory listing and are filtered to the proven-*.json shape above.
		if err != nil {
			continue
		}
		var file provenSelfTestFile
		if err := json.Unmarshal(raw, &file); err != nil {
			continue
		}
		record := file.Record
		if file.Version != provenSelfTestFileVersion {
			continue
		}
		if !record.Passed || record.Degraded != "" || len(record.SessionModes) == 0 {
			continue
		}
		if record.Digest == "" || record.computeDigest() != record.Digest {
			continue
		}
		if !found || record.TestedAt.After(latest.TestedAt) {
			latest, found = record, true
		}
	}
	return latest, found
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
