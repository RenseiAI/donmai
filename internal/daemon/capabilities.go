// Package daemon (internal) provides substrate capability detection for the
// local agentfactory-tui daemon.
//
// At startup the daemon probes the host for runtimes that are actually
// available on PATH and caches the result for the worker lifetime. The
// detected set is sent in the `provides[]` array on POST /api/workers/register
// and exposed via GET /api/daemon/capabilities for local clients.
//
// Architecture reference:
//
//	rensei-architecture/ADR-2026-05-12-capacity-pools-and-substrate-resolution.md
//	§ Stream H sub-lane — pool awareness
package daemon

import (
	"os/exec"
	"sync"
)

// SubstrateKind is the RuntimePath.kind string from the capability ontology
// (11-runtime-binding-strategy.md §1, SubstrateCapabilityDeclaration in the
// platform types). Values must match the platform's v1 closed enum.
type SubstrateKind string

// RuntimeKind constants — the platform v1 closed enum values.
const (
	KindNative     SubstrateKind = "native"
	KindNPM        SubstrateKind = "npm"
	KindPythonPip  SubstrateKind = "python-pip"
	KindHTTP       SubstrateKind = "http"
	KindMCPServer  SubstrateKind = "mcp-server"
	KindHostBinary SubstrateKind = "host-binary"
	KindWorkarea   SubstrateKind = "workarea"
)

// SubstrateCapability is the wire shape for a single capability entry in
// the provides[] array sent to /api/workers/register and returned from
// GET /api/daemon/capabilities.
type SubstrateCapability struct {
	Kind SubstrateKind `json:"kind"`
}

// CapabilitySet holds the detected substrate capabilities for this host.
// All methods are safe for concurrent use after construction.
type CapabilitySet struct {
	mu           sync.RWMutex
	capabilities []SubstrateCapability
}

// NewCapabilitySet returns an empty CapabilitySet. Call Detect to populate
// it, or use DetectCapabilities for the common single-call path.
func NewCapabilitySet() *CapabilitySet {
	return &CapabilitySet{}
}

// Capabilities returns a copy of the detected capabilities slice.
// The slice is never nil; it is empty before Detect is called.
func (c *CapabilitySet) Capabilities() []SubstrateCapability {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]SubstrateCapability, len(c.capabilities))
	copy(out, c.capabilities)
	return out
}

// Provides returns true when the given kind is in the detected set.
func (c *CapabilitySet) Provides(k SubstrateKind) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, cap := range c.capabilities {
		if cap.Kind == k {
			return true
		}
	}
	return false
}

// Detect probes the host for available runtimes and stores the result.
// It is safe to call multiple times; each call overwrites the previous
// result. The probe uses LookupFunc to resolve binary names on PATH —
// inject a custom resolver in tests.
func (c *CapabilitySet) Detect(lookup LookupFunc) {
	detected := detectCapabilities(lookup)
	c.mu.Lock()
	c.capabilities = detected
	c.mu.Unlock()
}

// LookupFunc resolves a binary name to its full path on PATH.
// exec.LookPath is the production implementation; tests inject stubs.
type LookupFunc func(name string) (string, error)

// DefaultLookup is the production LookupFunc backed by exec.LookPath.
func DefaultLookup(name string) (string, error) {
	return exec.LookPath(name)
}

// DetectCapabilities is the one-call convenience wrapper. It probes the
// host using exec.LookPath and returns the detected capability slice.
func DetectCapabilities() []SubstrateCapability {
	cs := NewCapabilitySet()
	cs.Detect(DefaultLookup)
	return cs.Capabilities()
}

// detectCapabilities performs the probe using the supplied lookup function
// and returns the resulting capability list. The detection rules are:
//
//   - native      — always present: the local daemon itself is a native process.
//   - npm         — requires `node` on PATH (npm/npx are bundled with Node).
//   - python-pip  — requires `python3` on PATH.
//   - http        — always present: the daemon can always spawn HTTP-served tools.
//   - mcp-server  — present when `node` OR `python3` is on PATH (both can host
//     MCP servers), or always if native is available (via HTTP transport).
//   - host-binary — always present: the daemon can exec arbitrary host binaries.
//   - workarea    — always present: the daemon manages local git workareas.
//
// Rationale for always-present entries: native, http, host-binary, and
// workarea describe capabilities of the local execution environment itself,
// not of third-party toolchains. They are always true for a local daemon.
// The platform v1 resolver uses them for the authMode=host-session / local
// axis (ADR-2026-05-12-capacity-pools-and-substrate-resolution.md §4).
func detectCapabilities(lookup LookupFunc) []SubstrateCapability {
	// Always-present capabilities for the local daemon.
	caps := []SubstrateCapability{
		{Kind: KindNative},
		{Kind: KindHTTP},
		{Kind: KindHostBinary},
		{Kind: KindWorkarea},
	}

	hasNode := binaryOnPath(lookup, "node")
	hasPython := binaryOnPath(lookup, "python3")

	if hasNode {
		caps = append(caps, SubstrateCapability{Kind: KindNPM})
	}
	if hasPython {
		caps = append(caps, SubstrateCapability{Kind: KindPythonPip})
	}
	// mcp-server: reachable via HTTP (always), or natively via node/python3.
	// We include it unconditionally because the HTTP transport is always present.
	caps = append(caps, SubstrateCapability{Kind: KindMCPServer})

	return caps
}

// binaryOnPath returns true when lookup finds name on PATH. A non-nil
// error (including exec.ErrNotFound) is treated as absent.
func binaryOnPath(lookup LookupFunc, name string) bool {
	path, err := lookup(name)
	return err == nil && path != ""
}
