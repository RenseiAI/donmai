package confinement

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// stubScopeProbe pins the Landlock ABI the scope gate sees for one test, so
// both branches run on any host; production always probes the running
// kernel. It lives in the portable test file (not the Linux-only backend
// test) so the scope-cache test stubs it on every host.
func stubScopeProbe(t *testing.T, abi int) {
	t.Helper()
	setScopesProbeForTest(t, func() int { return abi })
}

// writeScopeCacheEntry rewrites the cache file at path through mutate, so a
// test can age a record into a shape the current binary must refuse.

func writeScopeCacheEntry(t *testing.T, path string, mutate func(*selfTestCacheEntry)) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var entry selfTestCacheEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatal(err)
	}
	mutate(&entry)
	raw, err = json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestScopeVerdict_PartialBelowTheFloor pins the host-independent reading
// of the scope probe: below the scope floor the layer reports unenforced
// with the typed reason, at or above it enforced with none. It drives
// scopeVerdict directly, so it runs on every host; deleting the floor
// comparison (always enforced) turns it red.
func TestScopeVerdict_PartialBelowTheFloor(t *testing.T) {
	for _, abi := range []int{0, 1, 2, 5} {
		if ok, why := scopeVerdict(abi); ok || why == "" {
			t.Fatalf("scopeVerdict(%d) = (%v, %q), want unenforced with a reason", abi, ok, why)
		} else if !strings.Contains(why, "scope layer is unenforced") {
			t.Fatalf("scopeVerdict(%d) reason = %q, want the typed scope-layer reason", abi, why)
		}
	}
	if ok, why := scopeVerdict(landlockScopeABI); !ok || why != "" {
		t.Fatalf("scopeVerdict(%d) = (%v, %q), want enforced", landlockScopeABI, ok, why)
	}
	// The ambient probe stays stubbed enforced so the check below is
	// hermetic on every kernel: on a sub-floor host the real probe
	// reports missing, which is the degraded path this package pins
	// elsewhere, not the enforced branch checked here.
	stubScopeProbe(t, landlockScopeABI)
	if ok, _ := ScopesAvailable(); !ok {
		t.Fatal("ScopesAvailable reports a missing scope layer on a host with no scope concept; want enforced")
	}
}

// TestScopeVerdict_DegradedNeverAttests pins the production attestation
// entry points against the degraded marking: a passing record marked
// through applyScopeVerdict with a sub-floor verdict keeps its session modes
// so seats still start, but attests nothing — through the record or the
// confiner — while Prepare starts a seat from it and Degraded carries the
// reason. Deleting any of the guards (record attestation, confiner
// attestation) turns this test red; restoring the old Prepare refusal turns
// the start assertion red.
func TestScopeVerdict_DegradedNeverAttests(t *testing.T) {
	w := newSpecWorld(t)
	c := newTestConfiner(t, w, renderingBackend{}, nil, "sha256:test")
	record := passingRecord(t, c, agent.PromptModeAutonomous, agent.PromptModeHumanControlled)
	if _, ok := c.Attestation(); !ok {
		t.Fatal("a passing record does not attest before the scope verdict is applied")
	}
	ok, why := scopeVerdict(landlockScopeABI - 1)
	if ok || why == "" {
		t.Fatal("the sub-floor verdict is not a missing layer with a reason")
	}
	record.applyScopeVerdict(ok, why)
	if record.Degraded == "" {
		t.Fatal("a passing record under a sub-floor verdict carries no degraded reason")
	}
	if len(record.SessionModes) != 2 {
		t.Fatalf("a degraded record keeps modes %v, want both so seats still start", record.SessionModes)
	}
	record.Digest = record.computeDigest()
	// The record entry point refuses on the degraded marking alone: even
	// with its modes kept, it attests nothing as a full boundary.
	if attestation := record.Attestation(); len(attestation.SessionModes) != 0 {
		t.Fatalf("a degraded record attests modes %v, want none", attestation.SessionModes)
	}
	// The confiner entry point refuses on the degraded marking alone.
	c.store(record)
	if attestation, ok := c.Attestation(); ok {
		t.Fatalf("a degraded record attests through the confiner: %+v", attestation)
	}
	if _, ok := c.Degraded(); !ok {
		t.Fatal("Degraded reports nothing for a degraded record")
	}
	// Prepare still starts seats from a passing degraded record: the probes
	// passed and only the scope layer is missing, which the status surface
	// reports as degraded.
	if _, err := c.Prepare(w.spec()); err != nil {
		t.Fatalf("Prepare from a passing degraded record: err=%v, want a seat", err)
	}
	stripped := record
	stripped.Degraded = ""
	if stripped.computeDigest() == record.Digest {
		t.Fatal("the digest does not cover the degraded marking")
	}
}

// TestScopeVerdict_EnforcedLeavesAPassingRecordWhole pins the other branch:
// an enforced verdict leaves a passing record (and a failing one, which
// the verdict must not rescue) exactly as it was, so the floor only ever
// takes attestation away, never grants it.
func TestScopeVerdict_EnforcedLeavesAPassingRecordWhole(t *testing.T) {
	w := newSpecWorld(t)
	c := newTestConfiner(t, w, renderingBackend{}, nil, "sha256:test")
	record := passingRecord(t, c, agent.PromptModeAutonomous, agent.PromptModeHumanControlled)
	before := record.Digest
	ok, why := scopeVerdict(landlockScopeABI)
	if !ok || why != "" {
		t.Fatal("the floor verdict is not enforced with no reason")
	}
	record.applyScopeVerdict(ok, why)
	if record.Degraded != "" || len(record.SessionModes) != 2 || record.computeDigest() != before {
		t.Fatalf("an enforced verdict changed a passing record: %+v", record)
	}
	failing := record
	failing.Passed = false
	failing.applyScopeVerdict(ok, why)
	if failing.Degraded != "" {
		t.Fatal("an enforced verdict marked a failing record degraded")
	}
}

// TestSelfTest_MarksDegradedBelowTheFloor runs the production self-test with
// the scope probe stubbed below the floor and asserts the production record
// carries the degraded marking (modes kept so seats still start, no
// full-boundary attestation). Deleting the production applyScopeVerdict call
// in SelfTest turns this test red: the record would come back whole.
func TestSelfTest_MarksDegradedBelowTheFloor(t *testing.T) {
	backend := DefaultBackend()
	if backend == nil {
		t.Skip("no confinement backend on this host; nothing to prove")
	}
	stubScopeProbe(t, landlockScopeABI-1)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(base, "home")
	stateHome := filepath.Join(home, ".state")
	profileDir := filepath.Join(stateHome, "profiles")
	scratchBase, err := os.MkdirTemp("", "dcs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = removeAllForce(scratchBase) })
	scratchBase, err = filepath.EvalSymlinks(scratchBase)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{profileDir, home, stateHome} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	c, err := New(Options{Backend: backend, ProfileDir: profileDir, Home: home, StateHome: stateHome, ExecutableDigest: "sha256:test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	record, err := c.SelfTest(ctx, SelfTestOptions{ProbeCommand: []string{exe}, ScratchDir: scratchBase})
	if err != nil {
		t.Fatalf("SelfTest below the floor: %v", err)
	}
	if !record.Passed {
		t.Fatalf("SelfTest below the floor did not pass: %+v", record.Failures())
	}
	if record.Degraded == "" {
		t.Fatal("a passing self-test below the floor carries no degraded reason; want the production scope marking")
	}
	if len(record.SessionModes) != 2 {
		t.Fatalf("a degraded record keeps modes %v, want both so seats still start", record.SessionModes)
	}
	if attestation, ok := c.Attestation(); ok {
		t.Fatalf("a degraded self-test attests through the confiner: %+v", attestation)
	}
	if _, ok := c.Degraded(); !ok {
		t.Fatal("Degraded reports nothing for a degraded self-test")
	}
}

// TestScopeVerdict_CacheReusesDegradedAsDegraded pins the scope-keyed cache
// reuse: a degraded record hits on a host whose scope layer is still missing
// (so a sub-floor host does not re-probe on every spawn) and misses where
// the layer holds (so a kernel upgrade re-tests to a full record). Deleting
// either scope arm in load turns this test red.
func TestScopeVerdict_CacheReusesDegradedAsDegraded(t *testing.T) {
	now := time.Now()
	cw := newCacheWorld(t, noopBackend{}, nil, "sha256:harness")
	slot, _ := cw.seed(t, now.Add(-time.Hour))
	writeScopeCacheEntry(t, slot.path, func(entry *selfTestCacheEntry) {
		entry.Record.Degraded = "the scope layer is unenforced"
		entry.Record.Digest = entry.Record.computeDigest()
	})
	stubScopeProbe(t, landlockScopeABI-1)
	if loaded, hit := slot.load(now); !hit {
		t.Fatal("a degraded record is a miss with the scope layer still missing; want a degraded hit so a sub-floor host does not re-probe")
	} else if loaded.Degraded == "" {
		t.Fatalf("a degraded cache hit loads without the reason: %+v", loaded)
	}
	// Where the layer holds, the same degraded entry misses so the kernel
	// upgrade re-tests to a full record instead of reusing the partial one.
	// Re-stub enforced: the helper restores the ambient probe at cleanup,
	// so scoping the enforced read in a subtest keeps the outer sub-floor
	// stub for the rest of the test.
	t.Run("enforced", func(t *testing.T) {
		stubScopeProbe(t, landlockScopeABI)
		if loaded, hit := slot.load(now); hit {
			t.Fatalf("a degraded record loads with the scope layer enforced: %+v", loaded)
		}
	})
}

// TestScopeVerdict_CacheNeverReusesAScopeGap pins the cache side on both
// axes: a full-pass record written before degraded marking existed (cache
// version 1, no Degraded content, intact digest) is a miss at the new
// binary, so it can never load as a passing attestation; and the scope
// recheck in load refuses reuse where the scope layer reports missing,
// which the stubbed sub-floor probe below proves for an otherwise current
// record. Deleting the version bump and the recheck in turn (each
// mutation alone) turns this test red.
func TestScopeVerdict_CacheNeverReusesAScopeGap(t *testing.T) {
	now := time.Now()
	cw := newCacheWorld(t, noopBackend{}, nil, "sha256:harness")
	slot, seeded := cw.seed(t, now.Add(-time.Hour))
	if _, ok := cw.c.Attestation(); !ok {
		t.Fatal("the seeded passing record does not attest before the cache checks")
	}
	// A current-version record is reusable while the scope layer reports
	// enforced; the seeded entry proves the slot works.
	if _, hit := slot.load(now); !hit {
		t.Fatalf("a current passing record is a miss with the scope layer enforced (seeded %s)", seeded.Digest)
	}
	// A pre-marking record: cache version 1, full pass, no Degraded
	// content, intact digest. The new binary must refuse it even while the
	// scope layer reports enforced, so the version bump alone pins it.
	// The version is the absolute pre-marking value, never relative to
	// the current constant: a future version bump must not re-point this
	// case at a version the binary still accepts.
	writeScopeCacheEntry(t, slot.path, func(entry *selfTestCacheEntry) {
		entry.Version = 1
		entry.Record.Digest = entry.Record.computeDigest()
	})
	if loaded, hit := slot.load(now); hit {
		t.Fatalf("a pre-marking cache record loads as passing: %+v", loaded)
	}
	// A current-version record on a host whose scope probe reports the
	// layer missing: the load-time recheck refuses it, so no stale entry
	// attests as a full boundary.
	stubScopeProbe(t, landlockScopeABI-1)
	writeScopeCacheEntry(t, slot.path, func(entry *selfTestCacheEntry) {
		entry.Version = selfTestCacheVersion
		entry.Record.Digest = entry.Record.computeDigest()
	})
	if loaded, hit := slot.load(now); hit {
		t.Fatalf("a cached passing record loads as passing with the scope layer missing: %+v", loaded)
	}
}
