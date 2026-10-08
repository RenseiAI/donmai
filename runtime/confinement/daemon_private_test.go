package confinement

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestResolveSpec_DaemonPrivatePaths pins the resolution rules: daemon-
// private paths resolve loosely (a token minted after the seat starts is
// covered), sort without repeats, refuse the filesystem root and relative
// paths, refuse a read path that would re-open one, and refuse a writable
// root inside one. None of the raw paths leak into refusal details.
func TestResolveSpec_DaemonPrivatePaths(t *testing.T) {
	newWorld := func(t *testing.T) (specWorld, func(string) (string, error)) {
		t.Helper()
		return newSpecWorld(t), evalSymlinks
	}
	t.Run("resolves loose and sorted", func(t *testing.T) {
		w, canonical := newWorld(t)
		spec := w.spec()
		absent := filepath.Join(w.stateHome, "daemon-state", "control-token")
		spec.DeniedPaths = []string{absent, filepath.Dir(absent), absent}
		resolved, err := resolveSpec(spec, w.guards(), canonical)
		if err != nil {
			t.Fatalf("resolveSpec: %v", err)
		}
		if len(resolved.Denied) != 2 || resolved.Denied[0] != filepath.Dir(absent) || resolved.Denied[1] != absent {
			t.Fatalf("denied = %v", resolved.Denied)
		}
		if len(resolved.Pins) == 0 {
			t.Error("no ancestor pins for the daemon-private paths")
		}
	})
	t.Run("refusals", func(t *testing.T) {
		w, canonical := newWorld(t)
		for _, tc := range []struct {
			name  string
			paths []string
		}{
			{"relative", []string{"control-token"}},
			{"filesystem root", []string{"/"}},
		} {
			spec := w.spec()
			spec.DeniedPaths = tc.paths
			if _, err := resolveSpec(spec, w.guards(), canonical); err == nil {
				t.Errorf("%s daemon-private path was accepted", tc.name)
			}
		}
	})
	t.Run("a read path covering one is refused", func(t *testing.T) {
		w, canonical := newWorld(t)
		dir := filepath.Join(w.base, "daemon-state")
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		spec := w.spec()
		spec.ReadScope = agent.FileReadWorkarea
		spec.DeniedPaths = []string{filepath.Join(dir, "control-token")}
		spec.ReadPaths = []string{dir}
		_, err := resolveSpec(spec, w.guards(), canonical)
		if reason, _ := ReasonOf(err); reason != ReasonWritableSetUnrepresentable {
			t.Fatalf("a read path over a daemon-private path: err=%v, want writable_set_unrepresentable", err)
		}
	})
	t.Run("a writable root inside one is refused", func(t *testing.T) {
		w, canonical := newWorld(t)
		dir := filepath.Join(w.mut, "daemon-state")
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		spec := w.spec()
		spec.DeniedPaths = []string{dir}
		spec.HarnessState = append(spec.HarnessState, dir)
		_, err := resolveSpec(spec, w.guards(), canonical)
		if reason, _ := ReasonOf(err); reason != ReasonWritableSetUnrepresentable {
			t.Fatalf("a writable root inside a daemon-private path: err=%v, want writable_set_unrepresentable", err)
		}
	})
}

// TestRenderSeatbelt_DaemonPrivatePathsDenied pins the profile order: the
// daemon-private read deny renders after the runtime allows and before the
// session allowlist (so it wins over the carve-outs but never over the
// session — a session path covering one is refused at resolve time), and
// the write deny renders after the writable allows.
func TestRenderSeatbelt_DaemonPrivatePathsDenied(t *testing.T) {
	w := newSpecWorld(t)
	tokenDir := filepath.Join(w.base, "daemon-state")
	spec := w.spec()
	spec.DeniedPaths = []string{filepath.Join(tokenDir, "control-token"), tokenDir}
	spec.ReadScope = agent.FileReadWorkarea
	resolved, err := resolveSpec(spec, w.guards(), evalSymlinks)
	if err != nil {
		t.Fatalf("resolveSpec: %v", err)
	}
	text, err := renderSeatbelt(resolved, seatbeltHost{shared: []string{"/tmp"}}, nil, evalSymlinks)
	if err != nil {
		t.Fatalf("renderSeatbelt: %v", err)
	}
	order := []string{
		`(subpath "/opt/homebrew")`,
		`(subpath "` + tokenDir + `")`,
	}
	_ = order
	last := -1
	for _, needle := range []string{
		"(deny file-read-data file-read-xattr)\n",
		`(subpath "/opt/homebrew")`,
		`(subpath "` + tokenDir + `")`,
		"(allow file-read-data file-read-xattr\n  (subpath ",
		"(deny file-write*)\n(deny file-link)",
		`(subpath "` + tokenDir + `")`,
	} {
		at := strings.Index(text[last+1:], needle)
		if at < 0 {
			t.Fatalf("profile lacks %q after offset %d:\n%s", needle, last, text)
		}
		last += 1 + at
	}
	// The session allowlist holds no daemon-private path.
	allow := resolved.ReadAllowlist()
	for _, path := range allow {
		for _, denied := range resolved.Denied {
			if insideOrEqual(denied, path) || insideOrEqual(path, denied) {
				t.Fatalf("read allowlist %q overlaps daemon-private %q", path, denied)
			}
		}
	}
}
