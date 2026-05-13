package daemon

import (
	"errors"
	"sort"
	"testing"
)

// stubLookup builds a LookupFunc that returns found/not-found based on the
// supplied set. Absent binaries return exec.ErrNotFound equivalent (non-nil).
func stubLookup(present ...string) LookupFunc {
	m := make(map[string]bool, len(present))
	for _, p := range present {
		m[p] = true
	}
	return func(name string) (string, error) {
		if m[name] {
			return "/usr/local/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
}

func kindsFrom(caps []SubstrateCapability) []string {
	out := make([]string, len(caps))
	for i, c := range caps {
		out[i] = string(c.Kind)
	}
	sort.Strings(out)
	return out
}

func TestDetectCapabilities_AlwaysPresent(t *testing.T) {
	// No node, no python3 — only always-present kinds should appear.
	caps := detectCapabilities(stubLookup())
	kinds := kindsFrom(caps)

	always := []string{
		string(KindNative),
		string(KindHTTP),
		string(KindHostBinary),
		string(KindWorkarea),
		string(KindMCPServer), // present via HTTP transport
	}
	sort.Strings(always)

	if len(kinds) != len(always) {
		t.Fatalf("len(caps) = %d, want %d; got %v", len(kinds), len(always), kinds)
	}
	for i, want := range always {
		if kinds[i] != want {
			t.Errorf("cap[%d] = %q, want %q", i, kinds[i], want)
		}
	}
}

func TestDetectCapabilities_WithNode(t *testing.T) {
	caps := detectCapabilities(stubLookup("node"))
	kinds := kindsFrom(caps)

	for _, want := range []string{string(KindNPM), string(KindMCPServer)} {
		found := false
		for _, k := range kinds {
			if k == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %q when node is on PATH; got %v", want, kinds)
		}
	}
}

func TestDetectCapabilities_WithPython(t *testing.T) {
	caps := detectCapabilities(stubLookup("python3"))
	kinds := kindsFrom(caps)

	for _, want := range []string{string(KindPythonPip), string(KindMCPServer)} {
		found := false
		for _, k := range kinds {
			if k == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %q when python3 is on PATH; got %v", want, kinds)
		}
	}
}

func TestDetectCapabilities_WithNodeAndPython(t *testing.T) {
	caps := detectCapabilities(stubLookup("node", "python3"))
	kinds := kindsFrom(caps)

	for _, want := range []string{
		string(KindNative),
		string(KindNPM),
		string(KindPythonPip),
		string(KindHTTP),
		string(KindMCPServer),
		string(KindHostBinary),
		string(KindWorkarea),
	} {
		found := false
		for _, k := range kinds {
			if k == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %q with both runtimes on PATH; got %v", want, kinds)
		}
	}
}

func TestDetectCapabilities_NoPythonExcludesPip(t *testing.T) {
	caps := detectCapabilities(stubLookup("node")) // node only, no python3
	kinds := kindsFrom(caps)
	for _, k := range kinds {
		if k == string(KindPythonPip) {
			t.Errorf("python-pip should be absent without python3; got %v", kinds)
		}
	}
}

func TestDetectCapabilities_NoNodeExcludesNPM(t *testing.T) {
	caps := detectCapabilities(stubLookup("python3")) // python only, no node
	kinds := kindsFrom(caps)
	for _, k := range kinds {
		if k == string(KindNPM) {
			t.Errorf("npm should be absent without node; got %v", kinds)
		}
	}
}

func TestCapabilitySet_Detect_And_Provides(t *testing.T) {
	cs := NewCapabilitySet()

	// Before detect: nothing.
	if cs.Provides(KindNative) {
		t.Error("Provides(native) should be false before Detect")
	}

	cs.Detect(stubLookup("node"))

	if !cs.Provides(KindNative) {
		t.Error("Provides(native) should be true after Detect")
	}
	if !cs.Provides(KindNPM) {
		t.Error("Provides(npm) should be true with node on PATH")
	}
	if cs.Provides(KindPythonPip) {
		t.Error("Provides(python-pip) should be false without python3")
	}
}

func TestCapabilitySet_Capabilities_ReturnsCopy(t *testing.T) {
	cs := NewCapabilitySet()
	cs.Detect(stubLookup())
	c1 := cs.Capabilities()
	c2 := cs.Capabilities()
	// Mutating the returned slice must not affect subsequent calls.
	if len(c1) == 0 {
		t.Skip("no capabilities detected — nothing to mutate")
	}
	c1[0] = SubstrateCapability{Kind: "mutated"}
	c3 := cs.Capabilities()
	if c3[0].Kind == "mutated" {
		t.Error("Capabilities() returned the internal slice (not a copy)")
	}
	_ = c2
}

func TestCapabilitySet_DetectIdempotent(t *testing.T) {
	cs := NewCapabilitySet()
	cs.Detect(stubLookup("node"))
	first := kindsFrom(cs.Capabilities())

	// Re-detect without node — result should update, not accumulate.
	cs.Detect(stubLookup())
	second := kindsFrom(cs.Capabilities())

	for _, k := range first {
		for _, k2 := range second {
			if k == string(KindNPM) && k2 == string(KindNPM) {
				t.Error("npm should be absent after re-detect without node")
			}
		}
	}
	// Verify npm gone from second result.
	for _, k := range second {
		if k == string(KindNPM) {
			t.Errorf("npm should be absent after re-detect without node; got %v", second)
		}
	}
}

func TestBinaryOnPath_Found(t *testing.T) {
	lookup := stubLookup("node")
	if !binaryOnPath(lookup, "node") {
		t.Error("binaryOnPath(node) = false, want true")
	}
}

func TestBinaryOnPath_NotFound(t *testing.T) {
	lookup := stubLookup()
	if binaryOnPath(lookup, "node") {
		t.Error("binaryOnPath(node) = true, want false")
	}
}

func TestBinaryOnPath_ErrorTreatedAsAbsent(t *testing.T) {
	lookup := func(name string) (string, error) {
		return "", errors.New("lookup error")
	}
	if binaryOnPath(lookup, "anything") {
		t.Error("error from lookup should be treated as absent")
	}
}

func TestBinaryOnPath_EmptyPathTreatedAsAbsent(t *testing.T) {
	// Some systems return ("", nil) instead of ("", err) for not-found.
	lookup := func(name string) (string, error) {
		return "", nil
	}
	if binaryOnPath(lookup, "node") {
		t.Error("empty path with nil error should be treated as absent")
	}
}
