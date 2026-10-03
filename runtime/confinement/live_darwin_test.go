//go:build darwin

package confinement

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newLiveConfiner(t *testing.T, backend Backend, extra ExtraRules) (*Confiner, string) {
	t.Helper()
	home, stateHome, profileDir := hostDirs(t)
	c, err := New(Options{Backend: backend, ProfileDir: profileDir, Home: home, StateHome: stateHome, ExtraRules: extra, ExecutableDigest: "sha256:test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, stateHome
}

func probeCommand(t *testing.T) []string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return []string{exe}
}

func runSelfTest(t *testing.T, c *Confiner) (SelfTestRecord, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	return c.SelfTest(ctx, SelfTestOptions{ProbeCommand: probeCommand(t), ScratchDir: shortTempDir(t, "dcs")})
}

// TestSeatbelt_SelfTestPassesBothModes is the live backend proof: the probe
// set passes through the headless and the PTY spawn path.
func TestSeatbelt_SelfTestPassesBothModes(t *testing.T) {
	c, _ := newLiveConfiner(t, DefaultBackend(), nil)
	record, err := runSelfTest(t, c)
	if err != nil {
		t.Fatalf("SelfTest: %v\n%s", err, failureIDs(record))
	}
	if !record.Passed || len(record.SessionModes) != 2 {
		t.Fatalf("record passed=%v modes=%v", record.Passed, record.SessionModes)
	}
	t.Logf("self-test passed: %d probes across %v, backend %s", len(record.Probes), record.SessionModes, record.BackendVersion)
}

// probesHeldByTheBackend are the probes the boundary, and nothing else,
// holds: with the backend replaced by nothing they must each turn red.
var probesHeldByTheBackend = []string{
	"outside.home.create",
	"outside.home.write",
	"outside.state_home.write",
	"outside.shared_tmp.create",
	"outside.user_tmp.create",
	"outside.user_cache.create",
	"outside.git_common_dir.write",
	"outside.sibling_session.write",
	"outside.workarea_root.create_direct",
	"ro.write",
	"ro.create",
	"ro.rename_within",
	"ro.remove",
	"ro.chmod",
	"ro.hardlink_from_mutable",
	"ro.symlink_write_from_mutable",
	"ro.mount_over",
	"protected.write",
	"protected.rename_ancestor",
	"protected.rename_state",
	"widen.reenter_backend",
	"widen.job_launcher",
	"widen.open_application",
	"widen.service_lookup.apple_events",
	"widen.service_lookup.launch_services",
	"widen.service_lookup.mount",
	"widen.attach_process",
	"widen.socket_outside",
}

func failedIn(record SelfTestRecord) map[string]bool {
	failed := map[string]bool{}
	for _, probe := range record.Failures() {
		failed[string(probe.Mode)+"/"+probe.ID] = true
	}
	return failed
}

// TestSeatbelt_SelfTestRedWithoutBackend is the discriminating control of
// D1.5: with the backend replaced by nothing, the exact probes the backend
// holds fail, in both session modes.
func TestSeatbelt_SelfTestRedWithoutBackend(t *testing.T) {
	c, _ := newLiveConfiner(t, noopBackend{}, nil)
	record, err := runSelfTest(t, c)
	if reason, _ := ReasonOf(err); reason != ReasonSelfTestFailed {
		t.Fatalf("SelfTest with no backend: err=%v, want self_test_failed", err)
	}
	if record.Passed || len(record.SessionModes) != 0 {
		t.Fatalf("record passed=%v modes=%v, want no attested mode", record.Passed, record.SessionModes)
	}
	failed := failedIn(record)
	for _, mode := range []string{"autonomous", "human_controlled"} {
		for _, id := range probesHeldByTheBackend {
			if !failed[mode+"/"+id] {
				t.Errorf("%s/%s passed with no backend; the probe does not discriminate", mode, id)
			}
		}
	}
	if _, err := c.Prepare(Spec{SessionMode: "autonomous"}); err == nil {
		t.Fatal("Prepare succeeded after a failed self-test")
	} else if reason, _ := ReasonOf(err); reason != ReasonSelfTestFailed {
		t.Fatalf("Prepare after a failed self-test: %v, want self_test_failed", err)
	}
}

// TestSeatbelt_SelfTestRedWithChmod: same-identity permission bits are not
// enforcement (ADR-2026-08-22 D6.7); the permission-change probe on the
// read-only leaf and every outside probe turn red.
func TestSeatbelt_SelfTestRedWithChmod(t *testing.T) {
	c, _ := newLiveConfiner(t, chmodBackend{}, nil)
	record, err := runSelfTest(t, c)
	if reason, _ := ReasonOf(err); reason != ReasonSelfTestFailed {
		t.Fatalf("SelfTest with chmod: err=%v, want self_test_failed", err)
	}
	failed := failedIn(record)
	for _, mode := range []string{"autonomous", "human_controlled"} {
		for _, id := range []string{"ro.chmod", "ro.chmod_leaf", "protected.chmod", "outside.home.create", "widen.reenter_backend"} {
			if !failed[mode+"/"+id] {
				t.Errorf("%s/%s passed under chmod", mode, id)
			}
		}
		if failed[mode+"/ro.write"] {
			t.Errorf("%s/ro.write failed under chmod; the permission bits should have held that one", mode)
		}
	}
}

// liveWorld is a minimal session on disk for direct live tests, under a
// short directory so a socket fits in its session tmp.
func liveWorld(t *testing.T) (specWorld, *Confiner) {
	t.Helper()
	base := shortTempDir(t, "dcl")
	w := specWorld{base: base}
	w.home = filepath.Join(base, "home")
	w.stateHome = filepath.Join(w.home, ".s")
	w.profileDir = filepath.Join(w.stateHome, "p")
	w.ws = filepath.Join(base, "ws")
	w.mut = filepath.Join(w.ws, "mut")
	w.ro = filepath.Join(w.ws, "ro")
	w.state = filepath.Join(w.mut, ".h")
	w.ext = filepath.Join(w.state, "ext")
	w.tmp = filepath.Join(base, "t")
	w.cache = filepath.Join(base, "c")
	for _, dir := range []string{w.profileDir, filepath.Join(w.mut, ".git"), w.ro, w.ext, w.tmp, w.cache, filepath.Join(w.ws, ".workarea")} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}
	c, err := New(Options{Backend: DefaultBackend(), ProfileDir: w.profileDir, Home: w.home, StateHome: w.stateHome})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return w, c
}

// runConfined runs argv inside the plan through the headless spawn path and
// returns its exit code and output.
func runConfined(t *testing.T, plan *Plan, dir string, env []string, argv ...string) (int, string) {
	t.Helper()
	wrapped, err := plan.Command(argv)
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, wrapped[0], wrapped[1:]...) //nolint:gosec // G204: the confined argv under test.
	cmd.Dir = dir
	cmd.Env = overlayEnv(os.Environ(), append(plan.Environment(), env...))
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), string(out)
	}
	if err != nil {
		t.Fatalf("run %v: %v", argv, err)
	}
	return 0, string(out)
}

// runProbeSteps runs probe steps inside the plan and returns their results.
func runProbeSteps(t *testing.T, plan *Plan, w specWorld, steps ...probeStep) map[string]stepResult {
	t.Helper()
	planPath := filepath.Join(w.base, "plan-"+randomSuffix()+".json")
	raw, err := json.Marshal(probePlan{ResultPath: probeResultPath(w.tmp), Steps: steps})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out := runConfined(t, plan, w.mut, []string{ProbeEnv + "=" + planPath}, probeCommand(t)...); code != 0 {
		t.Fatalf("probe exit %d: %s", code, out)
	}
	raw, err = os.ReadFile(probeResultPath(w.tmp))
	if err != nil {
		t.Fatalf("probe results: %v", err)
	}
	var results []stepResult
	if err := json.Unmarshal(raw, &results); err != nil {
		t.Fatal(err)
	}
	byID := map[string]stepResult{}
	for _, result := range results {
		byID[result.ID] = result
	}
	return byID
}

// TestSeatbelt_ProfileParsesUnderSandboxExec: every rule the renderer can
// emit, composer rules of each kind included, is accepted by the kernel's
// profile compiler.
func TestSeatbelt_ProfileParsesUnderSandboxExec(t *testing.T) {
	w, _ := liveWorld(t)
	c, err := New(Options{Backend: DefaultBackend(), ProfileDir: w.profileDir, Home: w.home, StateHome: w.stateHome, ExtraRules: func(RuleContext) []Rule {
		return []Rule{
			{Kind: RuleDenyRead, Path: filepath.Join(w.home, ".secret"), Scope: ScopeSubtree},
			{Kind: RuleDenyRead, Path: filepath.Join(w.home, ".netrc"), Scope: ScopeLiteral},
			{Kind: RuleDenyWrite, Path: filepath.Join(w.mut, "vendor"), Scope: ScopeSubtree},
			{Kind: RuleDenyServiceLookup, Service: "com.example.composer.agent"},
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	spec := w.spec()
	spec.Sockets = ResolverSockets()
	plan, err := c.prepare(spec, "")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer func() { _ = plan.Release() }()
	if code, out := runConfined(t, plan, w.mut, nil, "/usr/bin/true"); code != 0 {
		t.Fatalf("sandbox-exec rejected the rendered profile: exit %d: %s", code, out)
	}
}

// TestSeatbelt_NestedSandboxRefused: from inside an outer profile the backend
// refuses with nested_sandbox instead of running under the outer profile and
// reporting the inner level (D4.2).
func TestSeatbelt_NestedSandboxRefused(t *testing.T) {
	cmd := exec.Command(sandboxExec, append([]string{"-p", "(version 1)(allow default)"}, probeCommand(t)...)...) //nolint:gosec // G204: fixed launcher, the test binary.
	cmd.Env = append(os.Environ(), nestedCheckEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("nested check: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "reason=nested_sandbox") {
		t.Fatalf("nested check output %q, want reason=nested_sandbox", out)
	}
	if err := DefaultBackend().Check(); err != nil {
		t.Fatalf("Check outside any profile: %v", err)
	}
}

// TestSeatbelt_ComposerRulesWinLast: a composer deny inside the writable set
// holds against the writable allow, its ancestors are pinned, and a service
// deny closes the service.
func TestSeatbelt_ComposerRulesWinLast(t *testing.T) {
	w, _ := liveWorld(t)
	vendor := filepath.Join(w.mut, "vendor")
	if err := os.MkdirAll(filepath.Join(vendor, "pkg"), 0o750); err != nil {
		t.Fatal(err)
	}
	c, err := New(Options{Backend: DefaultBackend(), ProfileDir: w.profileDir, Home: w.home, StateHome: w.stateHome, ExtraRules: func(RuleContext) []Rule {
		return []Rule{
			{Kind: RuleDenyWrite, Path: filepath.Join(vendor, "pkg"), Scope: ScopeSubtree},
			{Kind: RuleDenyServiceLookup, Service: lookupControlService},
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := c.prepare(w.spec(), "")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer func() { _ = plan.Release() }()
	results := runProbeSteps(t, plan, w,
		probeStep{ID: "denied", Op: opCreate, Path: filepath.Join(vendor, "pkg", "x")},
		probeStep{ID: "beside", Op: opCreate, Path: filepath.Join(vendor, "y")},
		probeStep{ID: "rename-ancestor", Op: opRename, Path: vendor, Path2: filepath.Join(w.mut, "vendor-moved")},
		probeStep{ID: "lookup", Op: opLookup, Services: []string{lookupControlService}},
	)
	if results["denied"].Err == "" || exists(filepath.Join(vendor, "pkg", "x")) {
		t.Error("a write under the composer deny got through")
	}
	if results["beside"].Err != "" {
		t.Errorf("a write beside the composer deny was refused: %s", results["beside"].Err)
	}
	if results["rename-ancestor"].Err == "" || !exists(vendor) {
		t.Error("renaming an ancestor carried the composer-denied path out from under its rule")
	}
	if code, ok := results["lookup"].Lookups[lookupControlService]; !ok || code == 0 {
		t.Errorf("the composer service deny did not close %s: %+v", lookupControlService, results["lookup"])
	}
}

// TestSeatbelt_ComposerUnrenderableFailsSelfTest: a composer that returns a
// rule the backend cannot render fails at start, not at the first session
// (D4.3).
func TestSeatbelt_ComposerUnrenderableFailsSelfTest(t *testing.T) {
	c, _ := newLiveConfiner(t, DefaultBackend(), func(RuleContext) []Rule {
		return []Rule{{Kind: RuleDenyWrite, Path: "relative/path", Scope: ScopeSubtree}}
	})
	record, err := runSelfTest(t, c)
	if reason, _ := ReasonOf(err); reason != ReasonRuleUnrenderable {
		t.Fatalf("SelfTest: err=%v, want rule_unrenderable", err)
	}
	if record.Passed || len(record.SessionModes) != 0 {
		t.Fatalf("record passed=%v modes=%v", record.Passed, record.SessionModes)
	}
	if _, ok := c.Attestation(); ok {
		t.Fatal("a self-test that never ran a probe attests")
	}
}

// TestSeatbelt_DeclaredSocketsOnly: a local socket outside the writable set
// is reachable only when the adapter declares it.
func TestSeatbelt_DeclaredSocketsOnly(t *testing.T) {
	resolver := ResolverSockets()[0]
	for _, tt := range []struct {
		name     string
		declared []string
		wantOpen bool
	}{
		{"undeclared", nil, false},
		{"declared", []string{resolver}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w, c := liveWorld(t)
			spec := w.spec()
			spec.Sockets = tt.declared
			plan, err := c.prepare(spec, "")
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			defer func() { _ = plan.Release() }()
			result := runProbeSteps(t, plan, w, probeStep{ID: "dial", Op: opDial, Path: resolver})["dial"]
			if open := result.Err == ""; open != tt.wantOpen {
				t.Fatalf("dial %s: err=%q, want open=%v", tt.name, result.Err, tt.wantOpen)
			}
		})
	}
}

// TestSeatbelt_ToolsWorkInsideTheSet: ordinary tools work on the writable
// set with TMPDIR redirected, and the same shell cannot write the home.
func TestSeatbelt_ToolsWorkInsideTheSet(t *testing.T) {
	w, c := liveWorld(t)
	plan, err := c.prepare(w.spec(), "")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer func() { _ = plan.Release() }()
	repo := filepath.Join(w.mut, "repo")
	script := strings.Join([]string{
		`set -e`,
		// macOS mktemp prefers the OS-assigned per-user directory to TMPDIR
		// unless given a template: refused there, it works with one.
		`if mktemp >/dev/null 2>&1; then echo "mktemp wrote the per-user temporary directory"; exit 3; fi`,
		`f=$(mktemp "$TMPDIR/x.XXXXXX")`,
		`cat > "$f" <<HEREDOC`,
		`a here-document needs a temporary file`,
		`HEREDOC`,
		`grep -q here-document "$f"`,
		`mkdir -p "` + repo + `" && cd "` + repo + `"`,
		`git init -q .`,
		`echo hello > a.txt`,
		`git add a.txt`,
		`git -c user.name=probe -c user.email=probe@example.invalid commit -q -m first`,
		`git log --oneline | grep -q first`,
		`echo ok`,
	}, "\n")
	if code, out := runConfined(t, plan, w.mut, []string{"HOME=" + w.home, "GIT_CONFIG_NOSYSTEM=1"}, "/bin/sh", "-c", script); code != 0 || !strings.Contains(out, "ok") {
		t.Fatalf("tools inside the set: exit %d: %s", code, out)
	}
	if code, _ := runConfined(t, plan, w.mut, nil, "/usr/bin/touch", filepath.Join(w.home, "planted")); code == 0 || exists(filepath.Join(w.home, "planted")) {
		t.Fatal("the confined shell wrote the operator home")
	}
}
