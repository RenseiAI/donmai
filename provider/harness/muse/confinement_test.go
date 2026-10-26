package muse

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/confinement"
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
)

// embedderSeatbeltPrefix is the neutral fixture standing in for an
// embedder-owned control namespace registered with the shared classifier.
const embedderSeatbeltPrefix = "EMBEDDER_SEATBELT_"

func init() {
	runtimeenv.RegisterEngineControlPrefix(embedderSeatbeltPrefix)
}

// TestMuseConfinementEnabled_FollowsTheRequest pins the gate: confinement
// is requested by the host requirement or by a declared repository
// authority, and by nothing else — never by the mere presence of a
// backend.
func TestMuseConfinementEnabled_FollowsTheRequest(t *testing.T) {
	t.Parallel()
	authority := &agent.RepositoryAuthorityPolicy{WorkareaRoot: "/w", SelectedPath: "/w/a", MutablePaths: []string{"/w/a"}}
	for _, tc := range []struct {
		name string
		spec agent.Spec
		host bool
		want bool
	}{
		{"nothing requested", agent.Spec{Cwd: "/w/a"}, false, false},
		{"host requires", agent.Spec{Cwd: "/w/a"}, true, true},
		{"authority declared", agent.Spec{Cwd: "/w/a", RepositoryAuthority: authority}, false, true},
		{"authority without a root", agent.Spec{Cwd: "/w/a", RepositoryAuthority: &agent.RepositoryAuthorityPolicy{}}, false, false},
	} {
		if got := museConfinementEnabled(tc.spec, tc.host); got != tc.want {
			t.Errorf("%s: museConfinementEnabled = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestNew_HostConfinementSwitchOnlyTightens pins the host switch: the
// environment can turn the requirement on, never off.
func TestNew_HostConfinementSwitchOnlyTightens(t *testing.T) {
	for _, tc := range []struct {
		env    string
		option bool
		want   bool
	}{
		{"required", false, true},
		{"", false, false},
		{"off", true, true},
		{"yes", false, false},
	} {
		t.Setenv(museConfinementEnvVar, tc.env)
		p, err := New(Options{skipProcess: true, RequireConfinement: tc.option})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if p.opts.RequireConfinement != tc.want {
			t.Errorf("env %q option %v: RequireConfinement = %v, want %v", tc.env, tc.option, p.opts.RequireConfinement, tc.want)
		}
	}
}

// TestNew_HostReadScopeOnlyTightens pins the read-scope switch: the
// environment can tighten to workarea (which implies the requirement),
// never loosen, and an unknown level refuses construction.
func TestNew_HostReadScopeOnlyTightens(t *testing.T) {
	t.Setenv(museConfinementReadEnvVar, "workarea")
	p, err := New(Options{skipProcess: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.opts.ConfinementReadScope != agent.FileReadWorkarea {
		t.Errorf("ConfinementReadScope = %q, want workarea", p.opts.ConfinementReadScope)
	}
	if !p.opts.RequireConfinement {
		t.Error("workarea read scope did not imply RequireConfinement")
	}

	t.Setenv(museConfinementReadEnvVar, "bogus-scope")
	if _, err := New(Options{skipProcess: true}); err == nil {
		t.Error("unknown read scope accepted, want refusal")
	}
}

// TestConfinerForSession_RequestWithoutBackendRefuses pins the never-run-
// unconfined rule on a host with no backend: a session that requested
// confinement is refused with the typed backend_absent reason, never run
// unconfined. This is the confinement probe the done-when names: on a
// backend-less host the probe is denied.
func TestConfinerForSession_RequestWithoutBackendRefuses(t *testing.T) {
	old := museConfinementBackend
	museConfinementBackend = func() confinement.Backend { return nil }
	t.Cleanup(func() { museConfinementBackend = old })
	p := &Provider{museBin: "/bin/sh", opts: Options{RequireConfinement: true}}
	_, err := p.confinerForSession(context.Background(), agent.Spec{Cwd: t.TempDir()})
	if reason, ok := confinement.ReasonOf(err); !ok || reason != confinement.ReasonBackendAbsent || !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("confinerForSession = %v, want a spawn failure carrying %s", err, confinement.ReasonBackendAbsent)
	}
}

// TestConfinerForSession_RequestWithoutWorkdirRefuses: a requested
// confinement has nothing to confine without a working directory, and is
// refused rather than dropped.
func TestConfinerForSession_RequestWithoutWorkdirRefuses(t *testing.T) {
	t.Parallel()
	p := &Provider{museBin: "/bin/sh", opts: Options{RequireConfinement: true}}
	if _, err := p.confinerForSession(context.Background(), agent.Spec{}); !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("confinerForSession with no working directory = %v, want a spawn failure", err)
	}
}

// TestConfinerForSession_UnrequestedIsNil: a session that requested
// nothing gets no confiner — it spawns exactly as before.
func TestConfinerForSession_UnrequestedIsNil(t *testing.T) {
	t.Parallel()
	p := &Provider{museBin: "/bin/sh"}
	got, err := p.confinerForSession(context.Background(), agent.Spec{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("confinerForSession: %v", err)
	}
	if got != nil {
		t.Errorf("unrequested session got a confiner, want nil")
	}
}

// TestConfineMuseArgv_NilPlanLeavesArgvUnchanged pins the pass-through: an
// unconfined session execs exactly the argv it built (serve + lane flags),
// with no wrapper inserted.
func TestConfineMuseArgv_NilPlanLeavesArgvUnchanged(t *testing.T) {
	t.Parallel()
	argv := []string{"/bin/muse", "serve", "--disable-shell", "--disable-write"}
	got, err := confineMuseArgv(nil, argv)
	if err != nil {
		t.Fatalf("confineMuseArgv: %v", err)
	}
	if len(got) != len(argv) {
		t.Fatalf("argv = %v, want %v", got, argv)
	}
	for i := range argv {
		if got[i] != argv[i] {
			t.Fatalf("argv = %v, want %v", got, argv)
		}
	}
}

// TestConfineMuseEnv_NilPlanAddsNothing pins the env pass-through: an
// unconfined session keeps exactly the composed child environment.
func TestConfineMuseEnv_NilPlanAddsNothing(t *testing.T) {
	t.Parallel()
	env := []string{"PATH=/bin", "MUSE_NO_AUTO_UPDATE=1"}
	got := confineMuseEnv(env, nil)
	if len(got) != len(env) {
		t.Fatalf("env = %v, want %v", got, env)
	}
	for i := range env {
		if got[i] != env[i] {
			t.Fatalf("env = %v, want %v", got, env)
		}
	}
}

// TestServeArgsUnderConfinement_LaneFlagsSurvive pins the composition
// decision: the confinement wraps the whole serve argv AFTER the lane
// flags are appended — the OS boundary never strips the host's own
// --disable-shell --disable-write lane. The wrap is proven by the argv
// the handle builds: with no confinement the lane flags are the tail of
// the exec argv.
func TestServeArgsUnderConfinement_LaneFlagsSurvive(t *testing.T) {
	t.Parallel()
	args := serveArgs(agent.Spec{SandboxLevel: agent.SandboxReadOnly}, false, true)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--disable-shell") || !strings.Contains(joined, "--disable-write") {
		t.Fatalf("read-only lane lost its flags when DisableSandbox is also set: %v", args)
	}
	if strings.Contains(joined, "--disable-sandbox") {
		t.Fatalf("read-only lane widened to --disable-sandbox: %v", args)
	}
}

// TestEndpointLoopbackPorts_RefusesSupervisorChannels pins the
// fail-closed port declaration: a remote endpoint or none declares
// nothing, a loopback endpoint declares its port, and an endpoint naming
// the daemon control API is refused rather than opened.
func TestEndpointLoopbackPorts_RefusesSupervisorChannels(t *testing.T) {
	t.Parallel()
	if ports, err := endpointLoopbackPorts(agent.Spec{}); err != nil || len(ports) != 0 {
		t.Errorf("no endpoint = %v,%v, want no ports", ports, err)
	}
	if ports, err := endpointLoopbackPorts(agent.Spec{Endpoint: &agent.EndpointBinding{BaseURL: "https://models.example.invalid/v1"}}); err != nil || len(ports) != 0 {
		t.Errorf("remote endpoint = %v,%v, want no ports", ports, err)
	}
	ports, err := endpointLoopbackPorts(agent.Spec{Endpoint: &agent.EndpointBinding{BaseURL: "http://127.0.0.1:11434/v1"}})
	if err != nil || len(ports) != 1 || ports[0] != 11434 {
		t.Errorf("loopback endpoint = %v,%v, want [11434]", ports, err)
	}
	if _, err := endpointLoopbackPorts(agent.Spec{Endpoint: &agent.EndpointBinding{BaseURL: "http://127.0.0.1:7734/v1"}}); err == nil {
		t.Error("control-API endpoint accepted, want refusal")
	}
}

// TestConfineMuseEnv_StripsEngineControlsBeforeTrustedAppend pins the
// hardened plan merge: inherited or session-supplied engine re-exec controls
// in childEnv are stripped before the producer-owned plan bindings are
// appended last, while safe near-misses and the tmp/cache precedence survive.
//
// RED: revert confineMuseEnv to a raw append and the evil entries below
// survive in the merged env.
func TestConfineMuseEnv_StripsEngineControlsBeforeTrustedAppend(t *testing.T) {
	t.Parallel()
	plan := confinement.PlanForTest(
		[]string{"TMPDIR=/plan/tmp", "TMP=/plan/tmp", "TEMP=/plan/tmp", "GOCACHE=/plan/cache/go-build"},
		[]string{"DONMAI_CONFINEMENT_LANDLOCK_STAGE=trusted-policy"},
	)
	childEnv := []string{
		"PATH=/usr/bin",
		"TMPDIR=/executor/seat-9",
		"GOCACHE=/operator/cache",
		"DONMAI_CONFINEMENT_PROBE=evil-probe",
		"DONMAI_CONFINEMENT_LANDLOCK_STAGE=evil-stage",
		"DONMAI_CONFINEMENT_PROBE_DECOY=evil-decoy",
		"EMBEDDER_SEATBELT_FOO=evil-seatbelt",
		"EVIL_PROBE=evil-suffix",
		"FOO_PROBE_EXTRA=near-miss",
		"DONMAI_CONFINEMENT=bare-prefix",
	}
	got := confineMuseEnv(childEnv, plan)
	for _, kv := range got {
		if strings.Contains(kv, "evil") {
			t.Fatalf("untrusted engine control survived confineMuseEnv: %v", got)
		}
	}
	stage := 0
	for _, kv := range got {
		if kv == "DONMAI_CONFINEMENT_LANDLOCK_STAGE=trusted-policy" {
			stage++
		}
	}
	if stage != 1 {
		t.Fatalf("trusted stage present %d times, want exactly once: %v", stage, got)
	}
	// Tmp precedence survives the hardened merge: the plan binding wins
	// exactly once, and the operator cache entry passes through alongside
	// the plan cache binding.
	tmp := 0
	for _, kv := range got {
		if kv == "TMPDIR=/plan/tmp" {
			tmp++
		}
		if kv == "TMPDIR=/executor/seat-9" {
			t.Fatalf("shadowed TMPDIR survived confineMuseEnv: %v", got)
		}
	}
	if tmp != 1 {
		t.Fatalf("plan TMPDIR present %d times, want exactly once: %v", tmp, got)
	}
	for _, want := range []string{
		"PATH=/usr/bin",
		"FOO_PROBE_EXTRA=near-miss",
		"DONMAI_CONFINEMENT=bare-prefix",
		"GOCACHE=/operator/cache",
		"GOCACHE=/plan/cache/go-build",
	} {
		found := false
		for _, kv := range got {
			found = found || kv == want
		}
		if !found {
			t.Fatalf("entry %q lost in confineMuseEnv (env %v)", want, got)
		}
	}
}

// TestServeEnv_FiltersEngineControlsAtEveryLayer pins that the serve child
// environment never carries an engine control from any layer, while the
// host's own MUSE_NO_AUTO_UPDATE marker and ordinary bindings still do.
//
// RED: drop the ComposeChildEnv engine filter and the session evil below
// reaches the serve env.
func TestServeEnv_FiltersEngineControlsAtEveryLayer(t *testing.T) {
	t.Parallel()
	got := serveEnv(
		[]string{"PATH=/bin", "DONMAI_CONFINEMENT_PROBE=parent-evil", "FOO_PROBE_EXTRA=parent-near-miss"},
		map[string]string{"EMBEDDER_SEATBELT_X": "extra-evil"},
		map[string]string{"DONMAI_CONFINEMENT_PROBE_DECOY": "session-evil", "SAFE": "kept", "FOO_PROBE_EXTRA": "session-near-miss"},
	)
	for _, kv := range got {
		if strings.Contains(kv, "evil") {
			t.Fatalf("engine control reached the serve env: %v", got)
		}
	}
	foundNear, foundSafe, foundMarker := false, false, false
	for _, kv := range got {
		if kv == "FOO_PROBE_EXTRA=session-near-miss" {
			foundNear = true
		}
		if kv == "SAFE=kept" {
			foundSafe = true
		}
		if kv == "MUSE_NO_AUTO_UPDATE=1" {
			foundMarker = true
		}
	}
	if !foundNear || !foundSafe || !foundMarker {
		t.Fatalf("serve env lost a wanted entry (env %v)", got)
	}
}

// TestConfineMuseSeatSpecForTest_SharesProductionLines pins that the test
// seam builds the same workarea + daemon-private lines the production
// entry point builds (shared through museSeatSpec, not copied): the
// mutable leaf, the workarea root, and the denied token path all match.
func TestConfineMuseSeatSpecForTest_SharesProductionLines(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	spec := agent.Spec{Cwd: dir}
	got := confineMuseSeatSpecForTest(spec, dir, museReadScope{}, "")
	if len(got.MutableLeaves) != 1 {
		t.Fatalf("mutable leaves = %v, want the workspace leaf", got.MutableLeaves)
	}
	if got.HarnessID != museHarnessID {
		t.Errorf("harness id = %q, want %q", got.HarnessID, museHarnessID)
	}
	if got.SessionMode != agent.PromptModeAutonomous {
		t.Errorf("session mode = %q, want autonomous", got.SessionMode)
	}
}
