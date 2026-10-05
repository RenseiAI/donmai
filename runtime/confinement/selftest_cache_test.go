package confinement

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

var bothModes = []agent.PromptSessionMode{agent.PromptModeAutonomous, agent.PromptModeHumanControlled}

// cacheWorld is a confiner with a probe executable on disk and a cache
// directory under its state home.
type cacheWorld struct {
	w        specWorld
	c        *Confiner
	opts     SelfTestOptions
	launches *atomic.Int32
}

func newCacheWorld(t *testing.T, backend Backend, extra ExtraRules, digest string) cacheWorld {
	t.Helper()
	w := newSpecWorld(t)
	probe := filepath.Join(w.base, "probe")
	if err := os.WriteFile(probe, []byte("probe executable v1"), 0o700); err != nil { //nolint:gosec // G306: a stand-in executable.
		t.Fatal(err)
	}
	launches := &atomic.Int32{}
	count := func(context.Context, []string, []string, string) (int, error) {
		launches.Add(1)
		return 0, nil
	}
	return cacheWorld{
		w: w,
		c: newTestConfiner(t, w, backend, extra, digest),
		opts: SelfTestOptions{
			ProbeCommand: []string{probe},
			ScratchDir:   filepath.Join(w.base, "scratch"),
			CacheDir:     filepath.Join(w.stateHome, "selftest-cache"),
			Launchers:    map[agent.PromptSessionMode]Launcher{agent.PromptModeAutonomous: count, agent.PromptModeHumanControlled: count},
		},
		launches: launches,
	}
}

// seed writes a passing record taken at testedAt into cw's cache slot.
func (cw cacheWorld) seed(t *testing.T, testedAt time.Time) (selfTestCache, SelfTestRecord) {
	t.Helper()
	current, err := cw.c.fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	slot, ok := cw.c.selfTestCache(cw.opts, current)
	if !ok {
		t.Fatal("caching is off")
	}
	record := passingRecord(t, cw.c, bothModes...)
	record.TestedAt = testedAt
	record.Digest = record.computeDigest()
	slot.save(record)
	return slot, record
}

// TestSelfTest_ReusesACachedPassingRecord: a later process on the same host
// reuses a cached passing self-test without spawning a probe, and Prepare
// accepts it; once the record is older than the TTL the self-test runs
// again, and a failing run never replaces the cache.
func TestSelfTest_ReusesACachedPassingRecord(t *testing.T) {
	cw := newCacheWorld(t, noopBackend{}, nil, "sha256:harness")
	slot, seeded := cw.seed(t, time.Now().Add(-time.Hour))

	fresh := newTestConfiner(t, cw.w, noopBackend{}, nil, "sha256:harness")
	record, err := fresh.SelfTest(t.Context(), cw.opts)
	if err != nil {
		t.Fatalf("SelfTest from cache: %v", err)
	}
	if cw.launches.Load() != 0 {
		t.Fatalf("the probe was launched %d times despite a reusable cached record", cw.launches.Load())
	}
	if record.Digest != seeded.Digest || !record.TestedAt.Equal(seeded.TestedAt) {
		t.Fatalf("SelfTest returned %s from %s, want the cached %s from %s", record.Digest, record.TestedAt, seeded.Digest, seeded.TestedAt)
	}
	if _, ok := fresh.Attestation(); !ok {
		t.Fatal("a cached passing record does not attest")
	}

	expired := newTestConfiner(t, cw.w, noopBackend{}, nil, "sha256:harness")
	cw.opts.CacheTTL = time.Minute
	if _, err := expired.SelfTest(t.Context(), cw.opts); err == nil {
		t.Fatal("an expired cache was reused: the self-test did not run")
	}
	if cw.launches.Load() == 0 {
		t.Fatal("the self-test did not launch a probe once the cache expired")
	}
	raw, err := os.ReadFile(slot.path)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	var entry selfTestCacheEntry
	if err := json.Unmarshal(raw, &entry); err != nil || entry.Record.Digest != seeded.Digest {
		t.Fatalf("a failing self-test replaced the cached record (err %v)", err)
	}
}

// TestSelfTestCache_KeyCoversWhatTheRecordProves: a different backend
// version, harness executable, probe executable or host directory selects
// a different slot, and a composer callback turns caching off.
func TestSelfTestCache_KeyCoversWhatTheRecordProves(t *testing.T) {
	base := newCacheWorld(t, noopBackend{}, nil, "sha256:harness")
	slotOf := func(cw cacheWorld) (string, bool) {
		current, err := cw.c.fingerprint()
		if err != nil {
			t.Fatal(err)
		}
		slot, ok := cw.c.selfTestCache(cw.opts, current)
		return slot.path, ok
	}
	basePath, ok := slotOf(base)
	if !ok {
		t.Fatal("caching is off for a plain confiner")
	}
	otherProbe := base
	otherProbe.opts.ProbeCommand = []string{filepath.Join(base.w.base, "probe2")}
	if err := os.WriteFile(otherProbe.opts.ProbeCommand[0], []byte("probe executable v2"), 0o700); err != nil { //nolint:gosec // G306: a stand-in executable.
		t.Fatal(err)
	}
	otherHarness := base
	otherHarness.c = newTestConfiner(t, base.w, noopBackend{}, nil, "sha256:other-harness")
	otherBackend := base
	otherBackend.c = newTestConfiner(t, base.w, renderingBackend{version: "rendering-v2"}, nil, "sha256:harness")
	otherHome := base
	otherHome.c = newTestConfiner(t, base.w, noopBackend{}, nil, "sha256:harness")
	otherHome.c.opts.Home = filepath.Join(base.w.base, "other-home")
	for name, cw := range map[string]cacheWorld{
		"probe executable":   otherProbe,
		"harness executable": otherHarness,
		"backend version":    otherBackend,
		"operator home":      otherHome,
	} {
		path, ok := slotOf(cw)
		if !ok || path == basePath {
			t.Errorf("%s: slot %q (caching %v), want a slot other than %q", name, path, ok, basePath)
		}
	}
	composed := base
	composed.c = newTestConfiner(t, base.w, noopBackend{}, func(RuleContext) []Rule { return nil }, "sha256:harness")
	if _, ok := slotOf(composed); ok {
		t.Error("caching is on for a confiner with a composer callback")
	}
	unreadable := base
	unreadable.opts.ProbeCommand = []string{filepath.Join(base.w.base, "absent")}
	if _, ok := slotOf(unreadable); ok {
		t.Error("caching is on for a probe executable that cannot be digested")
	}
}

// TestSelfTestCache_LoadRefusesWhatItCannotVouchFor: a cached record is a
// miss unless it is the same key, passing in both modes, current by the
// staleness fingerprint, intact by its digest and within the TTL.
func TestSelfTestCache_LoadRefusesWhatItCannotVouchFor(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name   string
		mutate func(*selfTestCacheEntry)
		hit    bool
	}{
		{"reusable", func(*selfTestCacheEntry) {}, true},
		{"other cache version", func(e *selfTestCacheEntry) { e.Version = 99 }, false},
		{"other key", func(e *selfTestCacheEntry) { e.Key.ProbeDigest = "sha256:other" }, false},
		{"failing record", func(e *selfTestCacheEntry) { e.Record.Passed = false; e.Record.Digest = e.Record.computeDigest() }, false},
		{"one session mode", func(e *selfTestCacheEntry) {
			e.Record.SessionModes = bothModes[:1]
			e.Record.Digest = e.Record.computeDigest()
		}, false},
		{"a failed probe", func(e *selfTestCacheEntry) {
			e.Record.Probes = append(e.Record.Probes, ProbeOutcome{ID: "q"})
			e.Record.Digest = e.Record.computeDigest()
		}, false},
		{"stale probe set", func(e *selfTestCacheEntry) {
			e.Record.ProbeSetVersion = "executor-confinement-probes-v3"
			e.Record.Digest = e.Record.computeDigest()
		}, false},
		{"tampered", func(e *selfTestCacheEntry) { e.Record.TestedAt = now }, false},
		{"expired", func(e *selfTestCacheEntry) {
			e.Record.TestedAt = now.Add(-25 * time.Hour)
			e.Record.Digest = e.Record.computeDigest()
		}, false},
		{"from the future", func(e *selfTestCacheEntry) {
			e.Record.TestedAt = now.Add(time.Hour)
			e.Record.Digest = e.Record.computeDigest()
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cw := newCacheWorld(t, noopBackend{}, nil, "sha256:harness")
			slot, _ := cw.seed(t, now.Add(-time.Hour))
			raw, err := os.ReadFile(slot.path)
			if err != nil {
				t.Fatal(err)
			}
			var entry selfTestCacheEntry
			if err := json.Unmarshal(raw, &entry); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&entry)
			raw, err = json.Marshal(entry)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(slot.path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, hit := slot.load(now); hit != tc.hit {
				t.Fatalf("load hit = %v, want %v", hit, tc.hit)
			}
		})
	}
}
