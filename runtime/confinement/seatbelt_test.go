package confinement

import (
	"slices"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
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
	text, err := renderSeatbelt(r, seatbeltHost{shared: []string{"/private/tmp"}}, rules, identity)
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
		_, err := renderSeatbelt(r, seatbeltHost{}, nil, identity)
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
		{"deny read, literal", Rule{Kind: RuleDenyRead, Path: "/secrets/key", Scope: ScopeLiteral}, []string{`(deny file-read-data file-read-metadata file-read-xattr (literal "/secrets/key"))`}},
		{"deny read, subtree", Rule{Kind: RuleDenyRead, Path: "/secrets", Scope: ScopeSubtree}, []string{`(deny file-read-data file-read-metadata file-read-xattr (subpath "/secrets"))`}},
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

// TestRenderComposerRules pins that a composer read deny names every read
// operation one by one, never as file-read*: the profile judges a rule
// naming an operation ahead of a wildcard rule whatever their order, so a
// wildcard deny would lose to the session allowlist's file-read-data allow
// and leave a denied path nested inside the writable set readable. The
// shape matches the daemon-private read deny.
func TestRenderComposerRules_ReadDenyNamesEveryOperation(t *testing.T) {
	for _, rule := range []Rule{
		{Kind: RuleDenyRead, Path: "/secrets/key", Scope: ScopeLiteral},
		{Kind: RuleDenyRead, Path: "/secrets", Scope: ScopeSubtree},
	} {
		text, err := renderComposerRules(sampleResolved(), []Rule{rule}, identity)
		if err != nil {
			t.Fatalf("renderComposerRules: %v", err)
		}
		if strings.Contains(text, "file-read*") {
			t.Fatalf("a composer read deny renders a wildcard that loses to the session allowlist:\n%s", text)
		}
		if !strings.Contains(text, "(deny "+seatbeltComposerReadOps+" ") {
			t.Fatalf("a composer read deny does not name every read operation:\n%s", text)
		}
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
			_, err := renderSeatbelt(sampleResolved(), seatbeltHost{}, []Rule{{Kind: RuleDenyServiceLookup, Service: "com.example.ok"}, tt.rule}, identity)
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

func readScopedResolved() *Resolved {
	r := sampleResolved()
	r.ReadScope = agent.FileReadWorkarea
	r.ReadPaths = []string{"/h/.config/git", "/h/.gitconfig"}
	return r
}

func mustRenderHost(t *testing.T, r *Resolved, host seatbeltHost, rules []Rule) string {
	t.Helper()
	text, err := renderSeatbelt(r, host, rules, identity)
	if err != nil {
		t.Fatalf("renderSeatbelt: %v", err)
	}
	return text
}

// readSessionBlock returns the session read allowlist block of a rendering.
func readSessionBlock(t *testing.T, text string) string {
	t.Helper()
	_, block, found := strings.Cut(text, "; The session: writable set, read-only leaves, declared read paths.")
	if !found {
		t.Fatalf("profile lacks the session read allowlist:\n%s", text)
	}
	block, _, _ = strings.Cut(block, "\n\n")
	return block
}

// TestRenderSeatbelt_ReadScopeOrder pins the read-scope order: the blanket
// read deny comes after the default allow, the runtime allows after it, the
// package data deny and the system-volume carve-out after them, and the
// session's own allowlist last, so a session path wins over the carve-outs;
// a composer read deny still renders after everything and wins inside the
// allowlist.
func TestRenderSeatbelt_ReadScopeOrder(t *testing.T) {
	host := seatbeltHost{shared: []string{"/private/tmp"}, runtimeReads: []string{"/Applications/Xcode.app"}}
	text := mustRenderHost(t, readScopedResolved(), host, []Rule{{Kind: RuleDenyRead, Path: "/r/ws/mut/secret", Scope: ScopeSubtree}})
	order := []string{
		"(allow default)",
		"(deny file-read-data file-read-xattr)\n",
		"(allow file-read-data file-read-xattr\n  (literal \"/\")",
		`(subpath "/opt/homebrew")`,
		`(subpath "/Applications/Xcode.app")`,
		"(deny file-read-data file-read-xattr\n  (subpath \"/opt/homebrew/var\")\n  (subpath \"/usr/local/var\"))",
		`(deny file-read-data file-read-xattr (subpath "/System/Volumes"))`,
		`(allow file-read-data file-read-xattr (subpath "/System/Volumes/Preboot/Cryptexes"))`,
		"(allow file-read-data file-read-xattr\n  (subpath \"/h/.config/git\")",
		`(subpath "/r/ws/mut")`,
		"(deny file-write*)\n",
		`(deny file-read-data file-read-metadata file-read-xattr (subpath "/r/ws/mut/secret"))`,
	}
	last := -1
	for _, needle := range order {
		at := strings.Index(text[last+1:], needle)
		if at < 0 {
			t.Fatalf("profile lacks %q after offset %d:\n%s", needle, last, text)
		}
		last += 1 + at
	}
	if got := strings.Count(text, "(deny file-read-data file-read-xattr)"); got != 1 {
		t.Fatalf("%d blanket read denies, want 1:\n%s", got, text)
	}
}

// TestRenderSeatbelt_ReadRulesPairExtendedAttributes: every read rule a read
// scope renders governs extended attributes together with file contents.
// A rule on file contents alone leaves getxattr open: it returns any
// attribute, and the whole contents of a transparently compressed file,
// outside the allowlist.
func TestRenderSeatbelt_ReadRulesPairExtendedAttributes(t *testing.T) {
	host := seatbeltHost{shared: []string{"/private/tmp"}, runtimeReads: []string{"/Applications/Xcode.app"}}
	text := mustRenderHost(t, readScopedResolved(), host, nil)
	data := strings.Count(text, "file-read-data")
	paired := strings.Count(text, "file-read-data file-read-xattr")
	if data == 0 || data != paired {
		t.Fatalf("%d read rules name file-read-data, %d of them pair file-read-xattr, want all:\n%s", data, paired, text)
	}
	if got := strings.Count(text, "file-read-xattr"); got != paired {
		t.Fatalf("%d rules name file-read-xattr, %d pair it with file-read-data:\n%s", got, paired, text)
	}
}

// TestRenderSeatbelt_RuntimeReadsClosePackageDataAndTerminals: the runtime
// allows name Homebrew and /usr whole, so the data trees under them are
// denied after, while their etc trees stay readable; the allows name device
// nodes one by one, never the /dev subtree, so no terminal is readable; and
// the Xcode licence record is readable, so the /usr/bin developer shims run.
func TestRenderSeatbelt_RuntimeReadsClosePackageDataAndTerminals(t *testing.T) {
	text := mustRenderHost(t, readScopedResolved(), seatbeltHost{}, nil)
	filters := readAllowFilters(t, text)
	for _, want := range []string{
		`(literal "/dev")`, `(literal "/dev/null")`, `(literal "/dev/zero")`, `(literal "/dev/random")`,
		`(literal "/dev/urandom")`, `(literal "/dev/tty")`, `(literal "/dev/ptmx")`, `(literal "/dev/dtracehelper")`,
		`(literal "/dev/autofs_nowait")`, `(literal "/dev/fd")`, `(regex #"^/dev/fd/[0-9]+$")`,
		`(literal "/Library/Preferences/com.apple.dt.Xcode.plist")`,
		`(subpath "/opt/homebrew")`, `(subpath "/usr")`,
	} {
		if !slices.Contains(filters, want) {
			t.Errorf("the read allows lack %s", want)
		}
	}
	if slices.Contains(filters, `(subpath "/dev")`) {
		t.Error("a read allow names the /dev subtree: every terminal would be readable")
	}
	denied := false
	for _, form := range sbplForms(t, text) {
		if !strings.HasPrefix(form, "(deny file-read-data file-read-xattr") {
			continue
		}
		got := sbplFilters(t, form)
		if slices.Contains(got, `(subpath "/opt/homebrew/var")`) && slices.Contains(got, `(subpath "/usr/local/var")`) {
			denied = true
		}
	}
	if !denied {
		t.Errorf("no read deny names /opt/homebrew/var and /usr/local/var:\n%s", text)
	}
	if strings.Contains(text, "/etc") && strings.Contains(strings.ReplaceAll(text, `(subpath "/private/etc")`, ""), "/etc") {
		t.Errorf("a read rule names an etc tree other than /private/etc; those stay readable through the runtime allows:\n%s", text)
	}
}

// TestRenderSeatbelt_ReadAllowlistIsTheSession: the session's read
// allowlist is exactly its writable roots, its read-only leaves and its
// declared read paths — never the workarea root, its metadata, the home or
// the shared temporary locations — and no runtime path widens to them.
func TestRenderSeatbelt_ReadAllowlistIsTheSession(t *testing.T) {
	r := readScopedResolved()
	text := mustRenderHost(t, r, seatbeltHost{shared: []string{"/private/tmp"}}, nil)
	block := readSessionBlock(t, text)
	var got []string
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimRight(strings.TrimSpace(line), ")")
		if path, ok := strings.CutPrefix(line, "(subpath "); ok {
			got = append(got, strings.Trim(path, `"`))
		}
	}
	want := []string{"/h/.config/git", "/h/.gitconfig", "/r/c", "/r/t", "/r/ws/mut", "/r/ws/mut/.h", "/r/ws/ro"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("session read allowlist = %v, want %v", got, want)
	}
	filters := readAllowFilters(t, text)
	for _, never := range []string{`"/r/ws"`, `"/r/ws/.workarea"`, `"/h"`, `"/Users"`, `"/private"`, `"/private/tmp"`, `"/private/var"`, `"/private/var/folders"`, `"/Volumes"`, `"/opt"`, `"/opt/homebrew/var"`, `"/usr/local/var"`} {
		for _, rule := range []string{"(subpath " + never + ")", "(literal " + never + ")"} {
			if slices.Contains(filters, rule) {
				t.Errorf("a read allow names %s:\n%s", rule, text)
			}
		}
	}
}

// readAllowFilters returns every filter of every read-allow rule of a
// rendering: the runtime allows, the cryptex carve-out and the session's.
func readAllowFilters(t *testing.T, text string) []string {
	t.Helper()
	var filters []string
	for _, form := range sbplForms(t, text) {
		if strings.HasPrefix(form, "(allow file-read-data") {
			filters = append(filters, sbplFilters(t, form)...)
		}
	}
	if len(filters) == 0 {
		t.Fatalf("profile has no read allow:\n%s", text)
	}
	return filters
}

// sbplFilters returns the filters of one rule: the forms nested in it, to
// its closing parenthesis, however many lines the rule spans.
func sbplFilters(t *testing.T, form string) []string {
	t.Helper()
	return sbplForms(t, strings.TrimSuffix(strings.TrimPrefix(form, "("), ")"))
}

// sbplForms splits profile text into its top-level forms, balancing
// parentheses outside comments, string literals and regex literals.
func sbplForms(t *testing.T, text string) []string {
	t.Helper()
	var forms []string
	depth, start := 0, 0
	inString := false
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case inString:
			if c == '"' {
				inString = false
			}
		case c == ';':
			for i < len(text) && text[i] != '\n' {
				i++
			}
		case c == '"':
			inString = true
		case c == '(':
			if depth == 0 {
				start = i
			}
			depth++
		case c == ')':
			depth--
			if depth < 0 {
				t.Fatalf("unbalanced profile text at offset %d:\n%s", i, text)
			}
			if depth == 0 {
				forms = append(forms, text[start:i+1])
			}
		}
	}
	if depth != 0 || inString {
		t.Fatalf("unbalanced profile text:\n%s", text)
	}
	return forms
}

// TestRenderSeatbelt_OpenReadsRenderNoReadRules: without a read scope the
// profile carries no read rule at all, as before; an unknown scope that
// slips past resolution is refused, never rendered as open reads.
func TestRenderSeatbelt_OpenReadsRenderNoReadRules(t *testing.T) {
	text := mustRender(t, sampleResolved(), nil)
	if strings.Contains(text, "file-read") {
		t.Fatalf("a profile with open reads renders a read rule:\n%s", text)
	}
	r := sampleResolved()
	r.ReadScope = agent.FileReadHomeMinusSecrets
	if _, err := renderSeatbelt(r, seatbeltHost{}, nil, identity); err == nil {
		t.Fatal("an unrendered read scope was rendered as open reads")
	}
}

// TestRenderSeatbelt_ReadScopeRefusesPathsItCannotQuote: a read path or a
// runtime path the profile cannot express refuses the rendering.
func TestRenderSeatbelt_ReadScopeRefusesPathsItCannotQuote(t *testing.T) {
	r := readScopedResolved()
	r.ReadPaths = []string{`/h/"x`}
	if _, err := renderSeatbelt(r, seatbeltHost{}, nil, identity); err == nil {
		t.Fatal("a read path with a quote rendered")
	}
	if _, err := renderSeatbelt(readScopedResolved(), seatbeltHost{runtimeReads: []string{"/A\\pp"}}, nil, identity); err == nil {
		t.Fatal("a runtime path with a backslash rendered")
	}
}
