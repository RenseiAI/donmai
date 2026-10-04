package confinement

import (
	"strings"
	"testing"
)

func sampleResolved() *Resolved {
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

func identity(path string) (string, error) { return path, nil }

func mustRender(t *testing.T, r *Resolved, rules []Rule) string {
	t.Helper()
	text, err := renderSeatbelt(r, []string{"/private/tmp"}, rules, identity)
	if err != nil {
		t.Fatalf("renderSeatbelt: %v", err)
	}
	return text
}

// TestRenderSeatbelt_LastMatchOrder pins the order the boundary depends on:
// SBPL applies the last matching rule.
func TestRenderSeatbelt_LastMatchOrder(t *testing.T) {
	r := sampleResolved()
	r.LoopbackTCPPorts = []int{1234}
	text := mustRender(t, r, []Rule{{Kind: RuleDenyServiceLookup, Service: "com.example.composer"}})
	order := []string{
		"(allow default)",
		"(deny file-write*)\n",
		"(deny file-link)\n",
		`(subpath "/private/tmp")`,
		`(literal "/dev/null")`,
		`(subpath "/r/ws/mut")`,
		"(deny file-write*\n  (subpath \"/r/ws/ro\")",
		`(subpath "/r/ws/mut/.h/ext")`,
		`(literal "/r/ws")`,
		"(deny file-write*\n  (literal \"/r/ws/mut\")",
		"(deny job-creation)",
		"(deny network-outbound (remote unix-socket))",
		`(remote unix-socket (path-literal "/var/run/resolver"))`,
		`(deny network-outbound (remote tcp "localhost:*"))`,
		`(allow network-outbound (remote tcp "localhost:1234"))`,
		`(deny mach-lookup (global-name "com.example.composer"))`,
	}
	last := -1
	for _, needle := range order {
		at := strings.Index(text, needle)
		if at < 0 {
			t.Fatalf("profile lacks %q:\n%s", needle, text)
		}
		if at <= last {
			t.Fatalf("%q is out of order:\n%s", needle, text)
		}
		last = at
	}
}

// TestRenderSeatbelt_AllowsOnlyTheDeclaredSet: the only write allows are the
// device nodes and the declared roots.
func TestRenderSeatbelt_AllowsOnlyTheDeclaredSet(t *testing.T) {
	text := mustRender(t, sampleResolved(), nil)
	if got := strings.Count(text, "(allow file-write*"); got != 2 {
		t.Fatalf("%d write-allow rules, want 2 (device nodes, writable set):\n%s", got, text)
	}
	if got := strings.Count(text, "(allow "); got != 5 {
		t.Fatalf("%d allow rules, want 5 (default, devices, writes, links, sockets):\n%s", got, text)
	}
	_, block, found := strings.Cut(text, "; The writable set")
	if !found {
		t.Fatalf("profile lacks the writable set:\n%s", text)
	}
	block, _, _ = strings.Cut(block, "\n\n")
	for _, line := range strings.Split(block, "\n") {
		if !strings.Contains(line, "(subpath") {
			continue
		}
		ok := false
		for _, root := range sampleResolved().Writable {
			if strings.Contains(line, `"`+root.Path+`"`) {
				ok = true
			}
		}
		if !ok {
			t.Errorf("writable allow names an undeclared path: %s", line)
		}
	}
}

func TestRenderSeatbelt_ClosesTheWriteProxies(t *testing.T) {
	text := mustRender(t, sampleResolved(), nil)
	for _, rule := range []string{
		"(deny file-mount)", "(deny file-unmount)", "(deny job-creation)", "(deny lsopen)",
		"(deny appleevent-send)", "(deny user-preference-write)", "(deny mach-priv-task-port)", "(deny mach-task-read)",
		"(deny mach-task-inspect)", "(deny mach-task-name)", "(deny network-outbound (remote unix-socket))",
		`(deny network-outbound (remote tcp "localhost:*"))`,
	} {
		if !strings.Contains(text, rule) {
			t.Errorf("profile lacks %s", rule)
		}
	}
	for _, class := range seatbeltLookupDenies {
		for _, service := range class.services {
			if !strings.Contains(text, `(global-name "`+service+`")`) {
				t.Errorf("profile does not close the %s service %s", class.class, service)
			}
		}
	}
	// The activity-continuation pasteboard client rides the same daemon as
	// the main pasteboard service: dropping it re-opens the clipboard.
	for _, service := range []string{
		"com.apple.pasteboard.1",
		"com.apple.coreservices.uauseractivitypasteboardclient.xpc",
	} {
		if !strings.Contains(text, `(global-name "`+service+`")`) {
			t.Errorf("profile does not deny the pasteboard service %s", service)
		}
	}
}

func TestRenderSeatbelt_RefusesPathsItCannotQuote(t *testing.T) {
	for _, bad := range []string{`/r/t"x`, `/r/t\x`, "/r/t\nx", "/r/t\x7f"} {
		r := sampleResolved()
		r.Writable[2].Path = bad
		_, err := renderSeatbelt(r, nil, nil, identity)
		if reason, _ := ReasonOf(err); reason != ReasonWritableSetUnrepresentable {
			t.Errorf("path %q: err=%v, want writable_set_unrepresentable", bad, err)
		}
	}
}

func TestRenderComposerRules(t *testing.T) {
	tests := []struct {
		name string
		rule Rule
		want []string
	}{
		{"deny read, literal", Rule{Kind: RuleDenyRead, Path: "/secrets/key", Scope: ScopeLiteral}, []string{`(deny file-read* (literal "/secrets/key"))`}},
		{"deny read, subtree", Rule{Kind: RuleDenyRead, Path: "/secrets", Scope: ScopeSubtree}, []string{`(deny file-read* (subpath "/secrets"))`}},
		{"deny write inside a writable root pins its ancestors", Rule{Kind: RuleDenyWrite, Path: "/r/ws/mut/a/b", Scope: ScopeSubtree}, []string{
			`(deny file-write* (subpath "/r/ws/mut/a/b"))`,
			`(deny file-link (subpath "/r/ws/mut/a/b"))`,
			`(deny file-write* (literal "/r/ws/mut"))`,
			`(deny file-write* (literal "/r/ws/mut/a"))`,
		}},
		{"deny service lookup", Rule{Kind: RuleDenyServiceLookup, Service: "com.example.agent"}, []string{`(deny mach-lookup (global-name "com.example.agent"))`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, err := renderComposerRules(sampleResolved(), []Rule{tt.rule}, identity)
			if err != nil {
				t.Fatalf("renderComposerRules: %v", err)
			}
			for _, line := range tt.want {
				if !strings.Contains(text, line) {
					t.Errorf("rendering lacks %s:\n%s", line, text)
				}
			}
			if strings.Contains(text, "(allow") {
				t.Errorf("a composer rule rendered an allow:\n%s", text)
			}
		})
	}
}

func TestRenderComposerRules_RefusesWhatItCannotRender(t *testing.T) {
	tests := []struct {
		name string
		rule Rule
	}{
		{"unknown kind", Rule{Kind: "allow_write", Path: "/x", Scope: ScopeSubtree}},
		{"empty kind", Rule{}},
		{"unknown scope", Rule{Kind: RuleDenyWrite, Path: "/x", Scope: "glob"}},
		{"relative path", Rule{Kind: RuleDenyRead, Path: "x", Scope: ScopeLiteral}},
		{"path rule carrying a service", Rule{Kind: RuleDenyWrite, Path: "/x", Scope: ScopeLiteral, Service: "com.example"}},
		{"service rule carrying a path", Rule{Kind: RuleDenyServiceLookup, Service: "com.example", Path: "/x"}},
		{"service with a quote", Rule{Kind: RuleDenyServiceLookup, Service: `com.example"`}},
		{"empty service", Rule{Kind: RuleDenyServiceLookup}},
		{"path with a quote", Rule{Kind: RuleDenyWrite, Path: `/x"`, Scope: ScopeLiteral}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := renderSeatbelt(sampleResolved(), nil, []Rule{{Kind: RuleDenyServiceLookup, Service: "com.example.ok"}, tt.rule}, identity)
			if reason, _ := ReasonOf(err); reason != ReasonRuleUnrenderable {
				t.Fatalf("err=%v, want rule_unrenderable (the rule is refused, never dropped)", err)
			}
		})
	}
}

func TestSbplString(t *testing.T) {
	if got, ok := sbplString("/a b/c"); !ok || got != `"/a b/c"` {
		t.Fatalf("sbplString = %q, %v", got, ok)
	}
	for _, bad := range []string{"", `a"b`, `a\b`, "a\tb", "\xff"} {
		if _, ok := sbplString(bad); ok {
			t.Errorf("sbplString(%q) accepted", bad)
		}
	}
}

// TestRenderSeatbelt_LoopbackTCPPorts: the loopback deny is always rendered,
// and each declared port renders one allow after it so the last matching
// rule wins. The deny names the local machine rather than one address
// family, so narrowing it to IPv4 (tcp4) must fail this test.
func TestRenderSeatbelt_LoopbackTCPPorts(t *testing.T) {
	plain := mustRender(t, sampleResolved(), nil)
	if !strings.Contains(plain, `(deny network-outbound (remote tcp "localhost:*"))`) {
		t.Fatalf("profile lacks the loopback TCP deny:\n%s", plain)
	}
	if strings.Contains(plain, `(allow network-outbound (remote tcp "localhost:`) {
		t.Fatalf("profile allows a loopback TCP port nobody declared:\n%s", plain)
	}
	r := sampleResolved()
	r.LoopbackTCPPorts = []int{22, 8080}
	text := mustRender(t, r, nil)
	denyAt := strings.Index(text, `(deny network-outbound (remote tcp "localhost:*"))`)
	for _, want := range []string{
		`(allow network-outbound (remote tcp "localhost:22"))`,
		`(allow network-outbound (remote tcp "localhost:8080"))`,
	} {
		at := strings.Index(text, want)
		if at < 0 {
			t.Fatalf("profile lacks %s:\n%s", want, text)
		}
		if at < denyAt {
			t.Fatalf("%s renders before the deny; the deny would win:\n%s", want, text)
		}
	}
}
