package confinement

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
// staleness fingerprint, intact by its own digest, and taken within the TTL
// (and not in the future). A degraded record is reusable as degraded on a
// host whose scope layer is still missing, and only there: the scope verdict
// keys reuse, so a kernel upgrade (or downgrade) misses and re-tests instead
// of attesting from the other side's record. Anything else is a miss.
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
	case !record.Passed, len(record.SessionModes) != 2, !allPass(record.Probes):
		return SelfTestRecord{}, false
	case record.fingerprint() != s.now:
		return SelfTestRecord{}, false
	case record.Digest == "" || record.computeDigest() != record.Digest:
		return SelfTestRecord{}, false
	case record.TestedAt.After(now.Add(time.Minute)), now.Sub(record.TestedAt) >= s.ttl:
		return SelfTestRecord{}, false
	}
	// The scope verdict keys reuse: a full record from a kernel with the
	// scope layer never reuses where the layer is missing, and a degraded
	// record never reuses where the layer holds. Either side misses and
	// re-tests, so a kernel upgrade earns a full record and a downgrade
	// re-marks degraded instead of attesting from the other side.
	scopeOK, _ := ScopesAvailable()
	if scopeOK && record.Degraded != "" {
		return SelfTestRecord{}, false
	}
	if !scopeOK && record.Degraded == "" {
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

// latestSelfTestFileName is the single latest-outcome file in the proven
// directory: every completed self-test (and every backend Check failure)
// overwrites it, so a failure after a pass supersedes the pass and the
// status path never attests from a stale pass.
const latestSelfTestFileName = "latest.json"

// provenDir is the proven directory for one confiner: under its host state
// home, so tests write under their throwaway state home. In production the
// confiner state home is the host state home, so this resolves to the same
// path the status reader uses.
func (c *Confiner) provenDir() string {
	if c != nil && c.opts.StateHome != "" {
		return filepath.Join(c.opts.StateHome, "confinement-selftest")
	}
	return latestSelfTestCacheDir()
}

// recordLatest records the latest outcome for one confiner: every completed
// self-test (and every backend Check failure) overwrites the single
// latest-outcome file under the confiner's proven directory, so a failure
// after a pass supersedes the pass.
func (c *Confiner) recordLatest(record SelfTestRecord) {
	recordLatestSelfTest(c.provenDir(), record)
}

// recordProven keeps a passing, whole record under the confiner's proven
// directory (see recordProvenSelfTest for the contract, including pruning).
func (c *Confiner) recordProven(record SelfTestRecord) {
	if !record.Passed || record.Degraded != "" || len(record.SessionModes) == 0 {
		return
	}
	writeProvenFile(c.provenDir(), record)
}

// recordProvenSelfTest keeps a proven self-test record where the status
// path reads it back: one file per proven record, written atomically with
// owner-only permissions. Only passing, whole records are kept — a
// degraded or failing record proves no boundary to attest. A host where
// the state directory cannot be written keeps no record: the next status
// reader reports the boundary unproven instead of a boundary no seat
// proved. Secret-free by construction: the record carries no credential
// and no raw path (see Record). After writing, every other proven file is
// pruned, so the directory keeps only the latest outcome and never grows.
func recordProvenSelfTest(record SelfTestRecord) {
	if !record.Passed || record.Degraded != "" || len(record.SessionModes) == 0 {
		return
	}
	dir := latestSelfTestCacheDir()
	if dir == "" {
		return
	}
	writeProvenFile(dir, record)
}

// recordLatestSelfTest overwrites the single latest-outcome file with the
// record of the just-completed self-test (or Check failure), passing or
// not. A later pass overwrites a failure and a later failure overwrites a
// pass: the status path reads the newest outcome only, so a failure after a
// pass reports unproven instead of attesting from the stale pass. A record
// without a digest keeps no file.
func recordLatestSelfTest(dir string, record SelfTestRecord) {
	if dir == "" || record.Digest == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	raw, err := json.Marshal(provenSelfTestFile{Version: provenSelfTestFileVersion, Record: record})
	if err != nil {
		return
	}
	writeAtomic(dir, latestSelfTestFileName, raw)
}

// writeAtomic writes raw to dir/name atomically with owner-only
// permissions: a temp file plus rename, so readers never see a half write.
func writeAtomic(dir, name string, raw []byte) {
	tmp, err := os.CreateTemp(dir, ".tmp-*")
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
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp.Name(), filepath.Join(dir, name))
}

// writeProvenFile writes one proven file and prunes every other proven
// file, so the directory keeps only the latest outcome: the scan cap below
// is then only a backstop, and the directory never grows one file per
// self-test.
func writeProvenFile(dir string, record SelfTestRecord) {
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
	name := "proven-" + provenSelfTestName(record.Digest) + ".json"
	writeAtomic(dir, name, raw)
	pruneProvenFiles(dir, name)
}

// pruneProvenFiles deletes every proven-*.json in dir except keep: only the
// latest outcome is evidence, so the directory never grows one file per
// self-test.
func pruneProvenFiles(dir, keep string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "proven-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		if name == keep {
			continue
		}
		_ = os.Remove(filepath.Join(dir, name))
	}
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

// provenRecordTTL bounds how long a proven self-test record counts as
// evidence: the same window the in-memory self-test cache reuses a
// passing record. A proven file older than this is stale on this host,
// so the status path reports unproven instead of attesting from it.
const provenRecordTTL = DefaultSelfTestCacheTTL

// maxProvenRecordsPerRead caps how many proven files one status read
// scans, and maxProvenFileBytes caps how large a single proven file may
// be: the shared proven directory is host state every seat writes, so an
// unbounded scan would let one bloated or flooded directory stall every
// status and doctor read.
const (
	maxProvenRecordsPerRead = 128
	maxProvenFileBytes      = 1 << 20
)

// provenSelfTestFile is one proven self-test record on disk.
type provenSelfTestFile struct {
	Version int            `json:"version"`
	Record  SelfTestRecord `json:"record"`
}

// LatestProvenRecord reports the latest proven seat self-test on this
// host, if one was recorded: the newest passing, whole record under the
// shared proven directory whose digest still covers it and whose age is
// within the proven window. The single latest-outcome file (latest.json,
// written on every completed self-test and every Check failure) supersedes
// the per-record files: when it is present, only it is evidence — a failing,
// degraded, stale or future-stamped latest reports no evidence even where an
// older passing proven file remains. Anything else — no file, an undecodable
// shape, a version the binary does not read, a degraded or failing
// record, a digest that no longer covers it, a record older than the
// proven window or stamped in the future — is no evidence, so the status
// path reports the boundary unproven instead of attesting from it. The
// per-record scan is bounded and newest-first: entries are ordered by
// modification time (newest first) before the cap, so the cap never drops
// the newest evidence for an older file. At most maxProvenRecordsPerRead
// files and maxProvenFileBytes per file, so a flooded or bloated directory
// cannot stall the status path.
func LatestProvenRecord() (SelfTestRecord, bool) {
	dir := latestSelfTestCacheDir()
	if dir == "" {
		return SelfTestRecord{}, false
	}
	return LatestProvenRecordIn(dir)
}

// LatestProvenRecordIn reports the latest proven record under dir: the
// production reader (LatestProvenRecord) over the shared proven directory,
// and tests over an isolated directory. See LatestProvenRecord for the
// contract.
func LatestProvenRecordIn(dir string) (SelfTestRecord, bool) {
	if dir == "" {
		return SelfTestRecord{}, false
	}
	now := time.Now()
	if raw, err := os.ReadFile(filepath.Join(dir, latestSelfTestFileName)); err == nil { //nolint:gosec // G304: the latest file name is a constant in the proven-record directory.
		var file provenSelfTestFile
		if err := json.Unmarshal(raw, &file); err == nil && file.Version == provenSelfTestFileVersion {
			record := file.Record
			if record.Passed && record.Degraded == "" && len(record.SessionModes) != 0 &&
				record.Digest != "" && record.computeDigest() == record.Digest &&
				!record.TestedAt.After(now.Add(time.Minute)) && now.Sub(record.TestedAt) < provenRecordTTL {
				return record, true
			}
			// A present, decodable latest outcome that is not a passing,
			// whole, fresh record supersedes any older proven file: the
			// newest outcome on this host is a failure (or a stale or
			// future-stamped pass), so there is no evidence to attest from.
			return SelfTestRecord{}, false
		}
	}
	return latestProvenScan(dir, now)
}

// latestProvenScan scans the per-record proven files and returns the newest
// passing, whole, fresh record, or false. Entries are ordered newest-first
// by modification time before the read cap, so the cap never drops the
// newest evidence for an older file.
func latestProvenScan(dir string, now time.Time) (SelfTestRecord, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return SelfTestRecord{}, false
	}
	type cand struct {
		name string
		mod  time.Time
		size int64
	}
	var cands []cand
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "proven-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		cands = append(cands, cand{name: entry.Name(), mod: info.ModTime(), size: info.Size()})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].mod.After(cands[j].mod) })
	var latest SelfTestRecord
	found := false
	scanned := 0
	for _, c := range cands {
		if scanned >= maxProvenRecordsPerRead {
			break
		}
		scanned++
		if c.size <= 0 || c.size > maxProvenFileBytes {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, c.name)) //nolint:gosec // G304: entry names come from the proven-record directory listing and are filtered to the proven-*.json shape above.
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
		if record.TestedAt.After(now.Add(time.Minute)) || now.Sub(record.TestedAt) >= provenRecordTTL {
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
