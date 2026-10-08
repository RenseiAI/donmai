package confinement

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/RenseiAI/donmai/agent"
)

// seatbeltProfileVersion is the macOS profile backend's implementation
// version. It is part of the backend version, so a self-test record taken
// under an older profile shape is stale.
const seatbeltProfileVersion = "seatbelt-profile-v7"

// seatbeltHost is what a rendering needs from the host beside the session.
type seatbeltHost struct {
	// shared are the shared temporary and cache locations, named so their
	// writes are denied (D2.1).
	shared []string
	// runtimeReads are host-specific runtime paths readable under a read
	// scope beside the static list: the active developer directory.
	runtimeReads []string
}

// seatbeltReadOps are the read operations a read scope governs. Extended
// attributes are paired with file contents everywhere: a transparently
// compressed file keeps its contents in the com.apple.ResourceFork and
// com.apple.decmpfs attributes, which getxattr returns when asked to show
// compression, and any other attribute is file data too. Metadata (stat,
// existence) stays readable.
const seatbeltReadOps = "file-read-data file-read-xattr"

// seatbeltPrivateReadOps are the read operations a daemon-private path is
// denied: every one, metadata included. They are named one by one rather
// than as file-read*: the profile judges a rule naming an operation ahead of
// a wildcard rule whatever their order, so a file-read* deny would lose to
// the read scope's file-read-data allow of the session allowlist and leave a
// daemon-private path nested inside the writable set readable.
const seatbeltPrivateReadOps = "file-read-data file-read-metadata file-read-xattr"

// seatbeltComposerReadOps are the read operations a composer read deny
// names: every one, metadata included. Like the daemon-private deny, they
// are named one by one rather than as file-read*: the profile judges a
// rule naming an operation ahead of a wildcard rule whatever their order,
// so a file-read* deny would lose to the read scope's file-read-data
// allow of the session allowlist and leave a denied path nested inside
// the writable set readable.
const seatbeltComposerReadOps = "file-read-data file-read-metadata file-read-xattr"

// seatbeltRuntimeReads are the runtime and toolchain paths readable under a
// read scope. dyld reads the root directory itself at every exec, so the
// root is readable as a literal: listing it shows the top-level names and
// nothing below them, and no directory under it is listable unless it is
// named here or in the session's allowlist.
var seatbeltRuntimeReads = []string{
	`(literal "/")`,
	`(subpath "/usr")`, // the OS userland and /usr/local toolchains; /usr/local/var is re-denied below
	`(subpath "/bin")`,
	`(subpath "/sbin")`,
	`(subpath "/System")`, // frameworks; the data-volume mirror is re-denied below
	`(subpath "/Library/Apple")`,
	`(subpath "/Library/Developer")`, // command line tools
	`(subpath "/opt/homebrew")`,      // Homebrew's binaries, libraries and etc; its var is re-denied below
	`(subpath "/private/etc")`,
	`(subpath "/private/var/db/timezone")`,
	`(literal "/private/var/run/resolv.conf")`,
	// The Xcode licence record. Every /usr/bin developer shim (git, python3,
	// make, cc) asks xcrun, and xcrun reads it to learn the licence was
	// accepted; refused, the shim exits with "You have not agreed to the
	// Xcode license agreements" on any host whose developer directory is
	// Xcode.app.
	`(literal "/Library/Preferences/com.apple.dt.Xcode.plist")`,
	// Device nodes, by name. A subpath would make every terminal readable:
	// a seat could open another session's pty, or an operator's terminal,
	// and capture what is typed into it. The directory is listable (shells
	// glob it) and /dev/fd/N are the descriptors a process substitution
	// hands its children.
	`(literal "/dev")`,
	`(literal "/dev/null")`,
	`(literal "/dev/zero")`,
	`(literal "/dev/random")`,
	`(literal "/dev/urandom")`,
	`(literal "/dev/tty")`,
	`(literal "/dev/ptmx")`,
	`(literal "/dev/dtracehelper")`,
	`(literal "/dev/autofs_nowait")`,
	`(literal "/dev/fd")`,
	`(regex #"^/dev/fd/[0-9]+$")`,
}

// seatbeltPackageData are package manager data trees inside the runtime
// allows: the databases, keys and application caches Homebrew keeps beside
// its binaries (MySQL and PostgreSQL data directories, TLS keys). They are
// as sensitive as anything in the home, so they are re-denied after the
// runtime allows. Their etc siblings stay readable: the system CA bundle and
// gitconfig live there. The session's own allowlist still renders after,
// and wins.
var seatbeltPackageData = []string{
	"/opt/homebrew/var",
	"/usr/local/var",
}

// seatbeltSystemVolumes holds the firmlinked data volume and the other
// system volumes. The kernel judges a firmlinked path by its canonical
// spelling, so the blanket deny already closes the user trees reached
// through the data volume; the re-deny keeps the volume roots unlistable
// and is defence in depth. It renders after the runtime allows; the OS
// cryptexes inside it stay readable.
const (
	seatbeltSystemVolumes = "/System/Volumes"
	seatbeltCryptexes     = "/System/Volumes/Preboot/Cryptexes"
)

// loopbackTCPDeny closes outbound TCP to the local machine (loopback and
// the host's own addresses, which the profile names "localhost"): the
// write proxy through a local TCP service. The spawner declares the ports
// the harness legitimately needs; nothing is allowed by default.
const loopbackTCPDeny = `(deny network-outbound (remote tcp "localhost:*"))`

// loopbackTCPAllow re-opens one declared loopback TCP port. It renders
// after the deny, so the last matching rule wins.
func loopbackTCPAllow(port int) string {
	return fmt.Sprintf(`(allow network-outbound (remote tcp "localhost:%d"))`, port)
}

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
	{class: "pasteboard", services: []string{
		"com.apple.pasteboard.1",
		// The activity-continuation pasteboard client is vended by the
		// same daemon; without it the clipboard stays reachable.
		"com.apple.coreservices.uauseractivitypasteboardclient.xpc",
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
//  1. allow by default;
//  2. under a read scope only: deny reading file contents, extended
//     attributes and directory listings, allow the runtime paths, re-deny
//     the package data trees and the system volumes, re-allow the OS
//     cryptexes, then allow the session's read allowlist, so it wins over
//     the carve-outs; metadata stays readable;
//  3. in every read scope, open reads included: deny every read of the
//     daemon-private paths (contents, metadata, extended attributes,
//     listings) and the listing of the daemon-private directories — after
//     every read allow, so they win even inside the session's allowlist;
//  4. deny every write, every hard link, and the shared temporary and cache
//     locations by name (D2, D2.1);
//  5. allow the device nodes and the writable set (D2);
//  6. deny the read-only leaves, the protected paths, the daemon-private
//     paths, the workarea root and its metadata, and pin every ancestor
//     between a writable root and a nested denied path against rename —
//     after the allows, so they win;
//  7. close the write proxies: mounting, job submission, launch services,
//     scripting events, preference writes, task ports, local sockets
//     outside the set, loopback TCP outside the declared ports, and the
//     pasteboard (D2.5);
//  8. the composer's deny-only rules, last, so they always win (D4.3): a
//     composer read deny holds inside the read allowlist too.
func renderSeatbelt(r *Resolved, host seatbeltHost, rules []Rule, canonical func(string) (string, error)) (string, error) {
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

	switch r.ReadScope {
	case "":
		// Open reads: no blanket read rule. The daemon-private read denies
		// below render all the same.
	case agent.FileReadWorkarea:
		runtimeFilters, err := filters("subpath", host.runtimeReads)
		if err != nil {
			return "", err
		}
		sessionFilters, err := filters("subpath", r.ReadAllowlist())
		if err != nil {
			return "", err
		}
		dataFilters, err := filters("subpath", seatbeltPackageData)
		if err != nil {
			return "", err
		}
		b.WriteString("; Read scope workarea: file contents, extended attributes and directory listings\n")
		b.WriteString("; are denied outside the allowlist. Metadata stays readable.\n")
		fmt.Fprintf(&b, "(deny %s)\n", seatbeltReadOps)
		b.WriteString("; Runtime and toolchain paths.\n")
		rule("allow", seatbeltReadOps, append(append([]string{}, seatbeltRuntimeReads...), runtimeFilters...))
		b.WriteString("; Package data trees (databases, keys), inside the runtime allows.\n")
		rule("deny", seatbeltReadOps, dataFilters)
		b.WriteString("; The system volumes, except the OS cryptexes.\n")
		fmt.Fprintf(&b, "(deny %s (subpath %q))\n", seatbeltReadOps, seatbeltSystemVolumes)
		fmt.Fprintf(&b, "(allow %s (subpath %q))\n", seatbeltReadOps, seatbeltCryptexes)
		b.WriteString("; The session: writable set, read-only leaves, declared read paths. They win over\n")
		b.WriteString("; every read rule above; only the daemon-private denies below win over them.\n")
		rule("allow", seatbeltReadOps, sessionFilters)
		b.WriteString("\n")
	default:
		return "", refuse(ReasonWritableSetUnrepresentable, "unknown read scope %q", r.ReadScope)
	}

	// Daemon-private reads, in every read scope (open reads included) and
	// after every read allow, so they win even over the session's
	// allowlist: a daemon-private path nested inside the writable set
	// stays unreadable.
	privateFilters, err := filters("subpath", r.Denied)
	if err != nil {
		return "", err
	}
	listingFilters, err := filters("literal", r.DeniedListings)
	if err != nil {
		return "", err
	}
	if len(privateFilters)+len(listingFilters) > 0 {
		b.WriteString("; Daemon-private paths: no read of any kind. Daemon-private directories: no listing;\n")
		b.WriteString("; they stay traversable. Every read scope, after every read allow, so they win.\n")
		rule("deny", seatbeltPrivateReadOps, privateFilters)
		rule("deny", seatbeltReadOps, listingFilters)
		b.WriteString("\n")
	}

	b.WriteString("; D2: writes and hard links are denied unless the writable set allows them.\n")
	b.WriteString("(deny file-write*)\n(deny file-link)\n")
	sharedFilters, err := filters("subpath", host.shared)
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
	// Daemon-private paths deny writes too, everywhere: even with reads
	// open a seat must not replace the control token (a planted token the
	// operator's CLI would then read) nor plant a sibling secret beside
	// it. They render as subtrees so a file minted after the seat starts
	// is covered the moment it appears.
	denied = append(denied, r.Denied...)
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
	b.WriteString("; Read-only leaves, protected paths, daemon-private paths and the workarea root, after the allows.\n")
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
	// The preferences daemon writes a domain's file on the caller's behalf,
	// outside any path rule.
	b.WriteString("(deny user-preference-write)\n")
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
	b.WriteString(loopbackTCPDeny + "\n")
	for _, port := range r.LoopbackTCPPorts {
		b.WriteString(loopbackTCPAllow(port) + "\n")
	}

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
				fmt.Fprintf(&b, "(deny %s %s)\n", seatbeltComposerReadOps, filter)
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
