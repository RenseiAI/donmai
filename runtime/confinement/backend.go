package confinement

import (
	"sort"

	"github.com/RenseiAI/donmai/agent"
)

// Backend applies a confinement boundary around one harness process. The
// macOS profile backend and the Linux mount-namespace backend are the
// implementations in this package today.
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
	// Environ returns the KEY=VALUE bindings the boundary itself needs on
	// top of the plan environment (a Landlock stage policy, a marker): the
	// launcher applies them to the spawned process. Nil when none.
	Environ func() []string
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
	// Sockets are the declared sockets outside the writable set.
	Sockets []string
	// LoopbackTCPPorts are the declared loopback TCP ports.
	LoopbackTCPPorts []int
	// SessionTmp and Caches feed the environment bindings.
	SessionTmp string
	Caches     []Cache
	// Home is the operator home and StateHome the host state home. Both
	// are bound read-only so file metadata stays readable (the seatbelt
	// contract reads metadata everywhere); file contents and listings
	// outside the allowlist are denied by the Landlock stage, which grants
	// no access there. Empty when the session was resolved without host
	// directories (unit renders).
	Home      string
	StateHome string
	// ReadOnlyLeafNames are the read-only leaves' names, for the record.
	ReadOnlyLeafNames []string
	// ReadScope is the enforced fileRead level: empty for open reads, or
	// agent.FileReadWorkarea.
	ReadScope agent.ExecutionSecurityLevel
	// ReadPaths are the declared extra read paths, canonical and sorted.
	ReadPaths []string
}

// loopbackEgressDeclarer is implemented by a backend whose boundary leaves
// outbound TCP to the local machine open on undeclared ports. The self-test
// judges the undeclared-port loopback dials by it: a backend that declares
// nothing must refuse them, and one that declares the gap must let them
// through, so a backend that silently starts or stops filtering loopback
// TCP fails exactly the probes that name it.
type loopbackEgressDeclarer interface {
	leavesLoopbackEgressOpen() bool
}

// leavesLoopbackEgressOpen reports whether backend declares outbound TCP to
// the local machine open on undeclared ports. A backend that declares
// nothing is held to the contract's default: denied except on the declared
// ports.
func leavesLoopbackEgressOpen(backend Backend) bool {
	declarer, ok := backend.(loopbackEgressDeclarer)
	return ok && declarer.leavesLoopbackEgressOpen()
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
