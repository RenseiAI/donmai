package confinement

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

func TestReasons_AreTheClosedEnum(t *testing.T) {
	want := []string{
		"backend_absent", "self_test_failed", "self_test_stale", "nested_sandbox",
		"namespace_unavailable", "writable_set_unrepresentable", "rule_unrenderable", "mode_unsupported",
	}
	var got []string
	for _, reason := range Reasons() {
		got = append(got, string(reason))
		if parsed, err := ParseReason(string(reason)); err != nil || parsed != reason {
			t.Errorf("ParseReason(%q) = %q, %v", reason, parsed, err)
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Reasons() = %v, want %v", got, want)
	}
	for _, unknown := range []string{"", "Backend_Absent", "unsupported", "self_test_failed "} {
		if _, err := ParseReason(unknown); err == nil {
			t.Errorf("ParseReason(%q) accepted an unknown reason", unknown)
		}
	}
}

func TestError_ReportsUnderTheRenderingRefusal(t *testing.T) {
	err := fmt.Errorf("spawn: %w", refuse(ReasonNestedSandbox, "detail"))
	reason, ok := ReasonOf(err)
	if !ok || reason != ReasonNestedSandbox {
		t.Fatalf("ReasonOf = %q, %v", reason, ok)
	}
	var typed *Error
	if !errors.As(err, &typed) || typed.Code() != agent.ExecutionSecurityUnrenderable {
		t.Fatalf("code = %v, want execution_security_unrenderable", typed)
	}
	if _, ok := ReasonOf(errors.New("plain")); ok {
		t.Fatal("ReasonOf accepted a plain error")
	}
}

func TestDefaultBackend_ByOS(t *testing.T) {
	backend := DefaultBackend()
	if runtime.GOOS == "darwin" {
		if backend == nil || backend.Name() != BackendMacOSSeatbelt {
			t.Fatalf("DefaultBackend on darwin = %v", backend)
		}
		return
	}
	if backend != nil {
		t.Fatalf("DefaultBackend on %s = %v, want none", runtime.GOOS, backend)
	}
}

func newTestConfiner(t *testing.T, w specWorld, backend Backend, extra ExtraRules, digest string) *Confiner {
	t.Helper()
	c, err := New(Options{Backend: backend, ProfileDir: w.profileDir, Home: w.home, StateHome: w.stateHome, ExtraRules: extra, ExecutableDigest: digest})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func passingRecord(t *testing.T, c *Confiner, modes ...agent.PromptSessionMode) SelfTestRecord {
	t.Helper()
	current, err := c.fingerprint()
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	record := SelfTestRecord{
		Backend: current.backend, BackendVersion: current.backendVersion, ProbeSetVersion: ProbeSetVersion,
		ExecutableDigest: current.executableDigest, SessionModes: modes, Passed: len(modes) == 2,
		Probes: []ProbeOutcome{{ID: "p", Pass: true}}, TestedAt: time.Unix(0, 0).UTC(),
	}
	record.Digest = record.computeDigest()
	c.store(record)
	return record
}

func TestPrepare_RefusesWhenUnavailable(t *testing.T) {
	both := []agent.PromptSessionMode{agent.PromptModeAutonomous, agent.PromptModeHumanControlled}
	tests := []struct {
		name  string
		setup func(t *testing.T, w specWorld) (*Confiner, Spec)
		want  Reason
	}{
		{"no backend", func(t *testing.T, w specWorld) (*Confiner, Spec) {
			return newTestConfiner(t, w, nil, nil, ""), w.spec()
		}, ReasonBackendAbsent},
		{"unknown session mode", func(t *testing.T, w specWorld) (*Confiner, Spec) {
			spec := w.spec()
			spec.SessionMode = "batch"
			return newTestConfiner(t, w, renderingBackend{}, nil, ""), spec
		}, ReasonModeUnsupported},
		{"no self-test yet", func(t *testing.T, w specWorld) (*Confiner, Spec) {
			return newTestConfiner(t, w, renderingBackend{}, nil, ""), w.spec()
		}, ReasonSelfTestFailed},
		{"self-test passed for the other mode only", func(t *testing.T, w specWorld) (*Confiner, Spec) {
			c := newTestConfiner(t, w, renderingBackend{}, nil, "")
			passingRecord(t, c, agent.PromptModeHumanControlled)
			if attestation, ok := c.Attestation(); !ok || len(attestation.SessionModes) != 1 || attestation.SessionModes[0] != agent.PromptModeHumanControlled {
				t.Fatalf("Attestation = %+v, %v; want exactly the mode that passed", attestation, ok)
			}
			return c, w.spec()
		}, ReasonSelfTestFailed},
		{"self-test taken under another executable", func(t *testing.T, w specWorld) (*Confiner, Spec) {
			before := newTestConfiner(t, w, renderingBackend{}, nil, "sha256:old")
			record := passingRecord(t, before, both...)
			c := newTestConfiner(t, w, renderingBackend{}, nil, "sha256:new")
			c.store(record)
			return c, w.spec()
		}, ReasonSelfTestStale},
		{"self-test taken under another OS build", func(t *testing.T, w specWorld) (*Confiner, Spec) {
			before := newTestConfiner(t, w, renderingBackend{version: "v+build-A"}, nil, "")
			record := passingRecord(t, before, both...)
			c := newTestConfiner(t, w, renderingBackend{version: "v+build-B"}, nil, "")
			c.store(record)
			return c, w.spec()
		}, ReasonSelfTestStale},
		{"unrepresentable writable set", func(t *testing.T, w specWorld) (*Confiner, Spec) {
			c := newTestConfiner(t, w, renderingBackend{}, nil, "")
			passingRecord(t, c, both...)
			spec := w.spec()
			spec.SessionTmp = w.home
			return c, spec
		}, ReasonWritableSetUnrepresentable},
		{"composer rule the backend cannot render", func(t *testing.T, w specWorld) (*Confiner, Spec) {
			c := newTestConfiner(t, w, renderingBackend{}, func(RuleContext) []Rule { return []Rule{{Kind: "allow_write", Path: "/x"}} }, "")
			passingRecord(t, c, both...)
			return c, w.spec()
		}, ReasonRuleUnrenderable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newSpecWorld(t)
			c, spec := tt.setup(t, w)
			plan, err := c.Prepare(spec)
			if plan != nil {
				t.Fatal("Prepare returned a plan for an unavailable confinement")
			}
			if reason, _ := ReasonOf(err); reason != tt.want {
				t.Fatalf("Prepare: err=%v, want %s", err, tt.want)
			}
			if _, ok := c.Attestation(); ok && tt.want == ReasonSelfTestStale {
				t.Fatal("a stale self-test still attests")
			}
		})
	}
}

func TestPrepare_PlanCarriesTheBindingsAndAPathFreeRecord(t *testing.T) {
	w := newSpecWorld(t)
	var seen RuleContext
	c := newTestConfiner(t, w, renderingBackend{}, func(ctx RuleContext) []Rule {
		seen = ctx
		return []Rule{{Kind: RuleDenyServiceLookup, Service: "com.example.agent"}}
	}, "sha256:exe")
	record := passingRecord(t, c, agent.PromptModeAutonomous, agent.PromptModeHumanControlled)
	attestation, ok := c.Attestation()
	if !ok || attestation.SelfTestDigest != record.Digest || len(attestation.SessionModes) != 2 || attestation.ProbeSetVersion != ProbeSetVersion {
		t.Fatalf("Attestation = %+v, %v", attestation, ok)
	}
	plan, err := c.Prepare(w.spec())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if seen.SessionID != "s1" || seen.HarnessID != "h1" || seen.SessionMode != agent.PromptModeAutonomous || seen.Backend != BackendMacOSSeatbelt || seen.WorkareaRoot != w.ws {
		t.Fatalf("composer context = %+v", seen)
	}
	argv, err := plan.Command([]string{"sh", "-c", "true"})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if argv[0] != "/wrapped" || !strings.HasPrefix(argv[1], "/") || argv[2] != "-c" {
		t.Fatalf("Command = %v, want the wrapped absolute binary", argv)
	}
	if _, err := plan.Command(nil); err == nil {
		t.Fatal("Command accepted an empty argv")
	}
	got := plan.Record()
	if got.Backend != BackendMacOSSeatbelt || got.SelfTestDigest != record.Digest || got.SessionMode != agent.PromptModeAutonomous {
		t.Fatalf("record = %+v", got)
	}
	if strings.Join(classNames(got.WritableClasses), ",") != "mutable_leaf,harness_state,session_tmp,session_cache" {
		t.Fatalf("writable classes = %v", got.WritableClasses)
	}
	if strings.Join(got.ReadOnlyLeaves, ",") != "ro" || !strings.HasPrefix(got.ComposerRuleDigest, "sha256:") ||
		!strings.HasPrefix(got.RenderedSpecDigest, "sha256:") || !strings.HasPrefix(got.RecordID, "cfr_") ||
		!strings.HasPrefix(got.Digest(), "sha256:") {
		t.Fatalf("record = %+v", got)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "/") {
		t.Fatalf("the record carries a path: %s", raw)
	}
	env := strings.Join(plan.Environment(), "\n")
	if !strings.Contains(env, "TMPDIR="+w.tmp) || !strings.Contains(env, "GOCACHE="+w.cache) {
		t.Fatalf("environment = %s", env)
	}
	if err := plan.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func classNames(classes []WritableClass) []string {
	names := make([]string, len(classes))
	for i, class := range classes {
		names[i] = string(class)
	}
	return names
}

func TestSelfTestRecord_DigestCoversTheOutcomes(t *testing.T) {
	record := SelfTestRecord{Backend: BackendMacOSSeatbelt, BackendVersion: "v", ProbeSetVersion: ProbeSetVersion, Probes: []ProbeOutcome{{ID: "a", Pass: true}}}
	first := record.computeDigest()
	record.Digest = first
	if again := record.computeDigest(); again != first {
		t.Fatal("the digest depends on the digest field")
	}
	record.Probes[0].Pass = false
	if changed := record.computeDigest(); changed == first {
		t.Fatal("the digest does not cover the probe outcomes")
	}
	if failures := record.Failures(); len(failures) != 1 || failures[0].ID != "a" {
		t.Fatalf("Failures = %v", failures)
	}
}

func TestSelfTest_RefusesWithoutABackend(t *testing.T) {
	w := newSpecWorld(t)
	c := newTestConfiner(t, w, nil, nil, "")
	_, err := c.SelfTest(t.Context(), SelfTestOptions{ProbeCommand: []string{"/bin/true"}, ScratchDir: w.base})
	if reason, _ := ReasonOf(err); reason != ReasonBackendAbsent {
		t.Fatalf("SelfTest: err=%v, want backend_absent", err)
	}
	if _, err := c.Prepare(w.spec()); err == nil {
		t.Fatal("Prepare succeeded with no backend")
	}
}

func TestSelfTest_ValidatesItsOptions(t *testing.T) {
	w := newSpecWorld(t)
	c := newTestConfiner(t, w, renderingBackend{}, nil, "")
	if _, err := c.SelfTest(t.Context(), SelfTestOptions{ScratchDir: w.base}); err == nil {
		t.Error("SelfTest ran without a probe command")
	}
	if _, err := c.SelfTest(t.Context(), SelfTestOptions{ProbeCommand: []string{"/bin/true"}, ScratchDir: "rel"}); err == nil {
		t.Error("SelfTest ran with a relative scratch directory")
	}
	if _, err := New(Options{ProfileDir: "rel"}); err == nil {
		t.Error("New accepted a relative profile directory")
	}
}

func TestOverlayEnv_LastWins(t *testing.T) {
	got := overlayEnv([]string{"A=1", "B=2"}, []string{"B=3", "C=4", "A=5"})
	if strings.Join(got, ",") != "A=5,B=3,C=4" {
		t.Fatalf("overlayEnv = %v", got)
	}
}

func TestSafeName(t *testing.T) {
	if got := safeName("a/b c.d-e_f"); got != "a_b_c_d-e_f" {
		t.Fatalf("safeName = %q", got)
	}
	if got := safeName(""); got != "x" {
		t.Fatalf("safeName(empty) = %q", got)
	}
	if got := safeName(strings.Repeat("a", 200)); len(got) != 48 {
		t.Fatalf("safeName length = %d", len(got))
	}
}
