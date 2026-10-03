package confinement

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// seatbeltProfileVersion is the macOS profile backend's implementation
// version. It is part of the backend version, so a self-test record taken
// under an older profile shape is stale.
const seatbeltProfileVersion = "seatbelt-profile-v1"

// lookupDeny is one class of named services the profile closes (D2.5).
type lookupDeny struct {
	class    string
	services []string
}

// seatbeltLookupDenies are the services through which a confined process
// could have something run or mounted outside its boundary. Each class is
// also a widening probe of the self-test.
var seatbeltLookupDenies = []lookupDeny{
	{class: "launch_services", services: []string{
		"com.apple.coreservices.launchservicesd",
		"com.apple.lsd.open",
		"com.apple.lsd.openurl",
		"com.apple.lsd.modifydb",
	}},
	{class: "apple_events", services: []string{
		"com.apple.coreservices.appleevents",
	}},
	{class: "mount", services: []string{
		"com.apple.DiskArbitration.diskarbitrationd",
		"com.apple.DiskArbitration.DiskArbitrationAgent",
		"com.apple.diskimagesiod.xpc",
		"com.apple.diskimagesiod.ram.xpc",
		"com.apple.diskimagesiod.spb.xpc",
		"com.apple.diskimagesiod.rootcopy.xpc",
		"com.apple.diskimagespawner",
		"com.apple.diskimagespawner.xpc",
	}},
}

// seatbeltDeviceNodes are the device nodes a confined process may write: the
// null and zero devices, its controlling terminal and its own descriptors.
var seatbeltDeviceNodes = []string{
	`(literal "/dev/null")`,
	`(literal "/dev/zero")`,
	`(literal "/dev/tty")`,
	`(regex #"^/dev/fd/[0-9]+$")`,
}

var servicePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// renderSeatbelt renders the macOS profile. SBPL applies the last matching
// rule, so the order is the boundary:
//
//  1. allow by default — reads stay open in this slice;
//  2. deny every write, every hard link, and the shared temporary and cache
//     locations by name (D2, D2.1);
//  3. allow the device nodes and the writable set (D2);
//  4. deny the read-only leaves, the protected paths, the workarea root and
//     its metadata, and pin every ancestor between a writable root and a
//     nested denied path against rename — after the allows, so they win;
//  5. close the write proxies: mounting, job submission, launch services,
//     scripting events, task ports and local sockets outside the set (D2.5);
//  6. the composer's deny-only rules, last, so they always win (D4.3).
func renderSeatbelt(r *Resolved, shared []string, rules []Rule, canonical func(string) (string, error)) (string, error) {
	var b strings.Builder
	quote := func(path string) (string, error) {
		quoted, ok := sbplString(path)
		if !ok {
			return "", refuse(ReasonWritableSetUnrepresentable, "a declared path holds a character the profile cannot express")
		}
		return quoted, nil
	}
	filters := func(kind string, paths []string) ([]string, error) {
		var out []string
		for _, path := range paths {
			quoted, err := quote(path)
			if err != nil {
				return nil, err
			}
			out = append(out, fmt.Sprintf("(%s %s)", kind, quoted))
		}
		return out, nil
	}
	rule := func(action, operations string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "(%s %s\n", action, operations)
		for i, item := range items {
			b.WriteString("  ")
			b.WriteString(item)
			if i == len(items)-1 {
				b.WriteString(")")
			}
			b.WriteString("\n")
		}
	}

	fmt.Fprintf(&b, "(version 1)\n; executor OS confinement, %s\n", seatbeltProfileVersion)
	b.WriteString("(allow default)\n\n")

	b.WriteString("; D2: writes and hard links are denied unless the writable set allows them.\n")
	b.WriteString("(deny file-write*)\n(deny file-link)\n")
	sharedFilters, err := filters("subpath", shared)
	if err != nil {
		return "", err
	}
	b.WriteString("; D2.1: the shared temporary and cache locations, named.\n")
	rule("deny", "file-write*", sharedFilters)
	b.WriteString("\n")

	b.WriteString("; Device nodes a process needs.\n")
	rule("allow", "file-write*", seatbeltDeviceNodes)
	var writable []string
	for _, root := range r.Writable {
		writable = append(writable, root.Path)
	}
	writableFilters, err := filters("subpath", writable)
	if err != nil {
		return "", err
	}
	b.WriteString("; The writable set: mutable leaves, harness state, session tmp, session caches.\n")
	rule("allow", "file-write*", writableFilters)
	rule("allow", "file-link", writableFilters)
	b.WriteString("\n")

	denied := append(append([]string{}, r.ReadOnly...), r.Protected...)
	denied = append(denied, r.MetadataDir)
	deniedFilters, err := filters("subpath", denied)
	if err != nil {
		return "", err
	}
	rootFilter, err := filters("literal", []string{r.WorkareaRoot})
	if err != nil {
		return "", err
	}
	pinFilters, err := filters("literal", r.Pins)
	if err != nil {
		return "", err
	}
	b.WriteString("; Read-only leaves, protected paths and the workarea root, after the allows.\n")
	rule("deny", "file-write*", deniedFilters)
	rule("deny", "file-link", deniedFilters)
	rule("deny", "file-write*", rootFilter)
	b.WriteString("; Ancestors of nested denied paths, pinned against rename.\n")
	rule("deny", "file-write*", pinFilters)
	b.WriteString("\n")

	b.WriteString("; D2.5: write proxies.\n")
	b.WriteString("(deny file-mount)\n(deny file-unmount)\n")
	b.WriteString("(deny job-creation)\n")
	b.WriteString("(deny lsopen)\n")
	b.WriteString("(deny appleevent-send)\n")
	var lookups []string
	for _, class := range seatbeltLookupDenies {
		for _, service := range class.services {
			lookups = append(lookups, fmt.Sprintf(`(global-name "%s")`, service))
		}
	}
	rule("deny", "mach-lookup", lookups)
	b.WriteString("(deny mach-priv-task-port)\n(deny mach-task-read)\n(deny mach-task-inspect)\n(deny mach-task-name)\n")
	b.WriteString("(deny network-outbound (remote unix-socket))\n")
	socketAllows := make([]string, 0, len(writable)+len(r.Sockets))
	for _, path := range writable {
		quoted, err := quote(path)
		if err != nil {
			return "", err
		}
		socketAllows = append(socketAllows, fmt.Sprintf("(remote unix-socket (subpath %s))", quoted))
	}
	for _, path := range r.Sockets {
		quoted, err := quote(path)
		if err != nil {
			return "", err
		}
		socketAllows = append(socketAllows, fmt.Sprintf("(remote unix-socket (path-literal %s))", quoted))
	}
	rule("allow", "network-outbound", socketAllows)

	composer, err := renderComposerRules(r, rules, canonical)
	if err != nil {
		return "", err
	}
	if composer != "" {
		b.WriteString("\n; Composer rules, last.\n")
		b.WriteString(composer)
	}
	return b.String(), nil
}

// renderComposerRules renders the deny-only composer rules. Any rule the
// profile cannot express refuses the whole rendering; none is dropped.
func renderComposerRules(r *Resolved, rules []Rule, canonical func(string) (string, error)) (string, error) {
	var b strings.Builder
	var writable []string
	for _, root := range r.Writable {
		writable = append(writable, root.Path)
	}
	for i, rule := range rules {
		switch rule.Kind {
		case RuleDenyRead, RuleDenyWrite:
			if rule.Service != "" || !filepath.IsAbs(rule.Path) {
				return "", refuse(ReasonRuleUnrenderable, "composer rule %d: a path rule needs an absolute path and no service", i)
			}
			path, err := canonicalLoose(rule.Path, canonical)
			if err != nil {
				return "", refuse(ReasonRuleUnrenderable, "composer rule %d: %v", i, err)
			}
			quoted, ok := sbplString(path)
			if !ok {
				return "", refuse(ReasonRuleUnrenderable, "composer rule %d: the path holds a character the profile cannot express", i)
			}
			var filter string
			switch rule.Scope {
			case ScopeLiteral:
				filter = "(literal " + quoted + ")"
			case ScopeSubtree:
				filter = "(subpath " + quoted + ")"
			default:
				return "", refuse(ReasonRuleUnrenderable, "composer rule %d: unknown scope %q", i, rule.Scope)
			}
			if rule.Kind == RuleDenyRead {
				fmt.Fprintf(&b, "(deny file-read* %s)\n", filter)
			} else {
				fmt.Fprintf(&b, "(deny file-write* %s)\n(deny file-link %s)\n", filter, filter)
			}
			for _, pin := range ancestorPins(writable, []string{path}) {
				quotedPin, ok := sbplString(pin)
				if !ok {
					return "", refuse(ReasonRuleUnrenderable, "composer rule %d: an ancestor holds a character the profile cannot express", i)
				}
				fmt.Fprintf(&b, "(deny file-write* (literal %s))\n", quotedPin)
			}
		case RuleDenyServiceLookup:
			if rule.Path != "" || rule.Scope != "" || !servicePattern.MatchString(rule.Service) {
				return "", refuse(ReasonRuleUnrenderable, "composer rule %d: a service rule needs a plain service name and no path", i)
			}
			fmt.Fprintf(&b, "(deny mach-lookup (global-name \"%s\"))\n", rule.Service)
		default:
			return "", refuse(ReasonRuleUnrenderable, "composer rule %d: unknown kind %q", i, rule.Kind)
		}
	}
	return b.String(), nil
}

// sbplString quotes s as a profile string literal. It refuses, rather than
// escapes, anything beyond plain printable text: a quote, a backslash, a
// control character or invalid UTF-8.
func sbplString(s string) (string, bool) {
	if s == "" || !utf8.ValidString(s) {
		return "", false
	}
	for _, r := range s {
		if r == '"' || r == '\\' || r < 0x20 || r == 0x7f {
			return "", false
		}
	}
	return `"` + s + `"`, true
}
