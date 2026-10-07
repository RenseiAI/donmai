//go:build linux

package confinement

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// sampleLinuxResolved is a minimal resolved session for render tests that
// run on Linux, where the shared seatbelt_test.go sample (macOS profile
// fixtures) is not compiled in.
func sampleLinuxResolved() *Resolved {
	return &Resolved{
		SessionID:    "s1",
		HarnessID:    "h1",
		WorkareaRoot: "/r/ws",
		MetadataDir:  "/r/ws/.workarea",
		Writable: []WritableRoot{
			{Path: "/r/ws/mut", Class: ClassMutableLeaf},
			{Path: "/r/ws/mut/.h", Class: ClassHarnessState},
			{Path: "/r/t", Class: ClassSessionTmp},
			{Path: "/r/c", Class: ClassSessionCache},
		},
		ReadOnly:   []string{"/r/ws/ro"},
		Protected:  []string{"/r/ws/mut/.h/ext"},
		Pins:       []string{"/r/ws/mut", "/r/ws/mut/.h"},
		Sockets:    []string{"/var/run/resolver"},
		SessionTmp: "/r/t",
	}
}

func linuxIdentity(path string) (string, error) { return path, nil }

// mustRenderBubblewrap renders the Linux mount-tree arguments for the sample
// session, failing the test on any refusal.
func mustRenderBubblewrap(t *testing.T, r *Resolved, rules []Rule) []string {
	t.Helper()
	args, err := renderBubblewrap(r, rules, linuxIdentity)
	if err != nil {
		t.Fatalf("renderBubblewrap: %v", err)
	}
	return args
}

func joinArgs(args []string) string { return "\n" + strings.Join(args, "\n") + "\n" }

// TestRenderBubblewrap_BindOrder pins the order the boundary depends on:
// read-only binds land first, the writable set over them, the denies after
// the allows so narrower rules win, and the self-mark last.
func TestRenderBubblewrap_BindOrder(t *testing.T) {
	text := joinArgs(mustRenderBubblewrap(t, sampleLinuxResolved(), nil))
	order := []string{
		"--unshare-user",
		"--tmpfs\n/\n",
		"--ro-bind\n/usr\n/usr",
		"--ro-bind\n/etc/resolv.conf\n/etc/resolv.conf",
		"--bind\n/r/ws/mut\n/r/ws/mut",
		"--bind\n/r/t\n/r/t",
		"--bind\n/dev/null\n/dev/null",
		"--ro-bind\n/var/run/resolver\n/var/run/resolver",
		"--ro-bind\n/r/ws/ro\n/r/ws/ro",
		"--ro-bind\n/r/ws/mut/.h/ext\n/r/ws/mut/.h/ext",
		"--ro-bind\n/r/ws\n/r/ws",
		"--ro-bind\n/tmp\n/tmp",
		"--setenv\n" + mountNamespaceMarkEnv + "\n1",
	}
	last := -1
	for _, needle := range order {
		at := strings.Index(text, needle)
		if at < 0 {
			t.Fatalf("rendering lacks %q:\n%s", needle, text)
		}
		if at <= last {
			t.Fatalf("%q is out of order:\n%s", needle, text)
		}
		last = at
	}
}

// TestRenderBubblewrap_AllowsOnlyTheDeclaredSet: the only read-write binds
// are the declared writable roots and the writable device nodes.
func TestRenderBubblewrap_AllowsOnlyTheDeclaredSet(t *testing.T) {
	args := mustRenderBubblewrap(t, sampleLinuxResolved(), nil)
	var writable []string
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--bind" {
			writable = append(writable, args[i+1])
		}
	}
	want := map[string]bool{"/r/ws/mut": true, "/r/ws/mut/.h": true, "/r/t": true, "/r/c": true, "/dev/null": true, "/dev/zero": true}
	if len(writable) != len(want) {
		t.Fatalf("writable binds = %v, want exactly %v", writable, want)
	}
	for _, path := range writable {
		if !want[path] {
			t.Errorf("writable bind names an undeclared path: %s", path)
		}
	}
}

// TestRenderBubblewrap_ComposerRulesWinLast: a composer deny inside the
// writable set is overlaid after the writable allow, its ancestors are
// pinned, and a service rule refuses the whole rendering instead of being
// dropped.
func TestRenderBubblewrap_ComposerRulesWinLast(t *testing.T) {
	args := mustRenderBubblewrap(t, sampleLinuxResolved(), []Rule{
		{Kind: RuleDenyWrite, Path: "/r/ws/mut/vendor/pkg", Scope: ScopeSubtree},
	})
	text := joinArgs(args)
	allowAt := strings.Index(text, "--bind\n/r/ws/mut\n/r/ws/mut")
	denyAt := strings.Index(text, "/r/ws/mut/vendor/pkg")
	if denyAt < 0 {
		t.Fatalf("rendering lacks the composer deny:\n%s", text)
	}
	if denyAt < allowAt {
		t.Fatalf("the composer deny renders before the writable allow; the allow would win:\n%s", text)
	}
	if !strings.Contains(text, "/r/ws/mut/vendor") {
		t.Fatalf("the composer deny's ancestors are not pinned:\n%s", text)
	}
	for _, rule := range []Rule{
		{Kind: RuleDenyServiceLookup, Service: "com.example.agent"},
		{Kind: "allow_write", Path: "/x", Scope: ScopeSubtree},
		{Kind: RuleDenyWrite, Path: "relative", Scope: ScopeSubtree},
		{Kind: RuleDenyWrite, Path: "/x", Scope: "glob"},
	} {
		if _, err := renderBubblewrap(sampleLinuxResolved(), []Rule{rule}, linuxIdentity); err == nil {
			t.Errorf("rule %+v rendered; want rule_unrenderable", rule)
		} else if reason, _ := ReasonOf(err); reason != ReasonRuleUnrenderable {
			t.Errorf("rule %+v: err=%v, want rule_unrenderable", rule, err)
		}
	}
}

// TestRenderBubblewrap_DeclaredPortsNeedTheNetworkHelper: a spec that
// declares loopback ports is refused rather than rendered without its
// network rule.
func TestRenderBubblewrap_DeclaredPortsNeedTheNetworkHelper(t *testing.T) {
	r := sampleLinuxResolved()
	r.LoopbackTCPPorts = []int{1234}
	if _, err := renderBubblewrap(r, nil, linuxIdentity); err == nil {
		t.Fatal("a spec with declared loopback ports rendered without its network rule")
	} else if reason, _ := ReasonOf(err); reason != ReasonRuleUnrenderable {
		t.Fatalf("err=%v, want rule_unrenderable", err)
	}
}

// TestRenderBubblewrap_ReadScopeBindsTheAllowlist: under the workarea read
// scope the session's allowlist — writable roots, read-only leaves and
// declared read paths — is bound read-only last, while an unknown scope is
// refused, never rendered as open reads.
func TestRenderBubblewrap_ReadScopeBindsTheAllowlist(t *testing.T) {
	r := sampleLinuxResolved()
	r.ReadScope = "workarea"
	r.ReadPaths = []string{"/h/.config/git"}
	text := joinArgs(mustRenderBubblewrap(t, r, nil))
	for _, want := range []string{
		"--ro-bind\n/r/ws/mut\n/r/ws/mut",
		"--ro-bind\n/r/ws/ro\n/r/ws/ro",
		"--ro-bind\n/h/.config/git\n/h/.config/git",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("rendering lacks %q:\n%s", want, text)
		}
	}
	plain := joinArgs(mustRenderBubblewrap(t, sampleLinuxResolved(), nil))
	if strings.Count(plain, "--ro-bind\n/r/ws/mut\n/r/ws/mut") != 0 {
		t.Fatalf("a session without a read scope binds its allowlist read-only:\n%s", plain)
	}
	r.ReadScope = "home-minus-secrets"
	if _, err := renderBubblewrap(r, nil, linuxIdentity); err == nil {
		t.Fatal("an unrendered read scope was rendered as open reads")
	}
}

// TestRenderBubblewrap_MissingLauncherRefuses: Apply with no launcher on
// PATH refuses with backend_absent instead of rendering a boundary it
// cannot launch. It drives the production entry point: the Confiner's
// Prepare through the backend's Apply.
func TestRenderBubblewrap_MissingLauncherRefuses(t *testing.T) {
	w := newSpecWorld(t)
	b := &mountNamespaceBackend{launcherOverride: "/nonexistent/bwrap-test-launcher"}
	c := newTestConfiner(t, w, b, nil, "")
	passingRecord(t, c, agent.PromptModeAutonomous, agent.PromptModeHumanControlled)
	plan, err := c.Prepare(w.spec())
	if plan != nil {
		t.Fatal("Prepare returned a plan with no launcher present")
	}
	if reason, _ := ReasonOf(err); reason != ReasonBackendAbsent {
		t.Fatalf("Prepare: err=%v, want backend_absent", err)
	}
}

// TestBackendLinux_NameAndResolvers pins the attestation name and the
// resolver declaration the adapter binds into the boundary.
func TestBackendLinux_NameAndResolvers(t *testing.T) {
	b := &mountNamespaceBackend{}
	if b.Name() != BackendLinuxMountNamespace {
		t.Fatalf("Name = %q, want %q", b.Name(), BackendLinuxMountNamespace)
	}
	sockets := ResolverSockets()
	if len(sockets) != 1 || sockets[0] != "/etc/resolv.conf" {
		t.Fatalf("ResolverSockets = %v", sockets)
	}
	if _, err := b.Canonical("/tmp"); err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	if _, err := b.Version(); err != nil {
		t.Fatalf("Version: %v", err)
	}
}

// TestLandlockProbeRuns: the Landlock availability probe runs without
// failing the test either way — it reports kernel support honestly, and the
// mount tree stands alone where the kernel has none.
func TestLandlockProbeRuns(t *testing.T) {
	_ = landlockAvailable()
}

// TestCheckLauncher pins the launcher gate: an absolute path that is not an
// executable refuses, and an empty PATH finds nothing.
func TestCheckLauncher(t *testing.T) {
	t.Setenv("PATH", "")
	if err := checkLauncher("bwrap"); err == nil {
		t.Fatal("checkLauncher found a launcher on an empty PATH")
	} else if reason, _ := ReasonOf(err); reason != ReasonBackendAbsent {
		t.Fatalf("err=%v, want backend_absent", err)
	}
	if err := checkLauncher("/nonexistent/bwrap-test-launcher"); err == nil {
		t.Fatal("checkLauncher accepted a missing absolute launcher")
	} else if reason, _ := ReasonOf(err); reason != ReasonBackendAbsent {
		t.Fatalf("err=%v, want backend_absent", err)
	}
}
