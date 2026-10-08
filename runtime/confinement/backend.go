package confinement

import (
	"sort"

	"github.com/RenseiAI/donmai/agent"
)

// Backend applies a confinement boundary around one harness process. The
// macOS profile backend is the only implementation in this package today;
// a Linux mount-namespace backend is the next one.
type Backend interface {
	// Name is the backend's attestation name.
	Name() BackendName
	// Version is the backend implementation version plus the OS build. A
	// self-test record taken under another version is stale.
	Version() (string, error)
	// Check reports, at spawn, whether the backend can apply a boundary from
	// this process at all: backend_absent when its implementation is
	// missing, nested_sandbox when this process is already confined.
	Check() error
	// Canonical returns the spelling of an absolute path the backend matches
	// against (symbolic links resolved, platform aliases folded).
	Canonical(path string) (string, error)
	// Apply renders the resolved set plus the composer rules and returns how
	// to wrap a command in it. It refuses with rule_unrenderable or
	// writable_set_unrepresentable; it never drops a rule.
	Apply(ApplyRequest) (Applied, error)
}

// ApplyRequest is one session's resolved confinement.
type ApplyRequest struct {
	Resolved   *Resolved
	Rules      []Rule
	ProfileDir string
}

// Applied is a rendered confinement ready to wrap a command.
type Applied struct {
	// Rendered is the exact rendered spec; its digest is the record's
	// renderedSpecDigest.
	Rendered []byte
	// Wrap returns the argv that runs argv inside the boundary. argv[0] is
	// absolute.
	Wrap func(argv []string) []string
	// Release removes whatever Apply wrote. Nil when nothing was written.
	Release func() error
}

// WritableRoot is one root of the writable set with its class.
type WritableRoot struct {
	Path  string
	Class WritableClass
}

// Resolved is a validated Spec with every path in the backend's canonical
// spelling.
type Resolved struct {
	SessionID    string
	HarnessID    string
	WorkareaRoot string
	// MetadataDir is the workarea root's reserved metadata directory.
	MetadataDir string
	// Writable is the closed writable set, one entry per root.
	Writable []WritableRoot
	// ReadOnly are the read-only leaves; Protected the other paths kept
	// outside the set. Both are denied after the writable allows.
	ReadOnly  []string
	Protected []string
	// Pins are the directories between a writable root and a nested denied
	// path. They are denied as literals so that renaming an ancestor cannot
	// move a denied path out from under its rule.
	Pins []string
	// Denied are the daemon-private paths, canonical and sorted. They are
	// denied on read as well as on write, after every allow, so they win
	// even inside the session's read allowlist.
	Denied []string
	// Sockets are the declared sockets outside the writable set.
	Sockets []string
	// LoopbackTCPPorts are the declared loopback TCP ports.
	LoopbackTCPPorts []int
	// SessionTmp and Caches feed the environment bindings.
	SessionTmp string
	Caches     []Cache
	// ReadOnlyLeafNames are the read-only leaves' names, for the record.
	ReadOnlyLeafNames []string
	// ReadScope is the enforced fileRead level: empty for open reads, or
	// agent.FileReadWorkarea.
	ReadScope agent.ExecutionSecurityLevel
	// ReadPaths are the declared extra read paths, canonical and sorted.
	ReadPaths []string
}

// ReadAllowlist is the session's half of the read allowlist under a read
// scope: every writable root, every read-only leaf and every declared read
// path, sorted and without repeats. The backend adds its runtime paths.
func (r *Resolved) ReadAllowlist() []string {
	seen := map[string]bool{}
	var paths []string
	add := func(path string) {
		if !seen[path] {
			seen[path] = true
			paths = append(paths, path)
		}
	}
	for _, root := range r.Writable {
		add(root.Path)
	}
	for _, leaf := range r.ReadOnly {
		add(leaf)
	}
	for _, path := range r.ReadPaths {
		add(path)
	}
	sort.Strings(paths)
	return paths
}
