package confinement

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestResolveSpec_DaemonPrivatePaths pins the resolution rules: daemon-
// private paths and directories resolve loosely (a token minted after the
// seat starts is covered), sort without repeats, refuse the filesystem root
// and relative paths, and refuse every session path they would narrow. A
// work area beneath a daemon-private directory — a host state home that
// holds the per-session worktrees beside the control token — resolves,
// while the same directory named as a daemon-private path refuses. None of
// the raw paths leak into refusal details.
func TestResolveSpec_DaemonPrivatePaths(t *testing.T) {
	t.Run("resolves loose and sorted", func(t *testing.T) {
		w := newSpecWorld(t)
		spec := w.spec()
		absent := filepath.Join(w.base, "daemon-state", "control-token")
		spec.DeniedPaths = []string{absent, filepath.Join(w.base, "daemon-state", "a-secret"), absent}
		spec.DeniedListings = []string{filepath.Dir(absent), filepath.Dir(absent)}
		resolved, err := resolveSpec(spec, w.guards(), evalSymlinks)
		if err != nil {
			t.Fatalf("resolveSpec: %v", err)
		}
		if len(resolved.Denied) != 2 || resolved.Denied[0] != filepath.Join(w.base, "daemon-state", "a-secret") || resolved.Denied[1] != absent {
			t.Fatalf("denied = %v", resolved.Denied)
		}
		if len(resolved.DeniedListings) != 1 || resolved.DeniedListings[0] != filepath.Dir(absent) {
			t.Fatalf("denied listings = %v", resolved.DeniedListings)
		}
	})
	t.Run("a work area beneath a daemon-private directory resolves", func(t *testing.T) {
		// newSpecWorld keeps the workarea under the state home, the way
		// a host keeps its per-session worktrees there.
		w := newSpecWorld(t)
		spec := w.spec()
		spec.DeniedPaths = []string{filepath.Join(w.stateHome, "control-token")}
		spec.DeniedListings = []string{w.stateHome}
		resolved, err := resolveSpec(spec, w.guards(), evalSymlinks)
		if err != nil {
			t.Fatalf("a work area beneath the token's directory: %v", err)
		}
		if len(resolved.DeniedListings) != 1 || resolved.DeniedListings[0] != w.stateHome {
			t.Fatalf("denied listings = %v", resolved.DeniedListings)
		}
		readScoped := spec
		readScoped.ReadScope = agent.FileReadWorkarea
		if _, err := resolveSpec(readScoped, w.guards(), evalSymlinks); err != nil {
			t.Fatalf("the same under the read scope: %v", err)
		}
	})
	world := func(t *testing.T) (specWorld, string) {
		t.Helper()
		w := newSpecWorld(t)
		dir := filepath.Join(w.base, "daemon-state")
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		return w, dir
	}
	for _, tc := range []struct {
		name string
		edit func(w specWorld, dir string, spec *Spec)
	}{
		{"a relative daemon-private path", func(_ specWorld, _ string, s *Spec) { s.DeniedPaths = []string{"control-token"} }},
		{"the filesystem root as a daemon-private path", func(_ specWorld, _ string, s *Spec) { s.DeniedPaths = []string{"/"} }},
		{"a relative daemon-private directory", func(_ specWorld, _ string, s *Spec) { s.DeniedListings = []string{"daemon-state"} }},
		{"the filesystem root as a daemon-private directory", func(_ specWorld, _ string, s *Spec) { s.DeniedListings = []string{"/"} }},
		{"the work area's own directory as a daemon-private path", func(w specWorld, _ string, s *Spec) { s.DeniedPaths = []string{w.stateHome} }},
		{"the workarea root as a daemon-private directory", func(w specWorld, _ string, s *Spec) { s.DeniedListings = []string{w.ws} }},
		{"a writable root inside a daemon-private path", func(w specWorld, _ string, s *Spec) {
			s.DeniedPaths = []string{w.state}
		}},
		{"a writable root that is a daemon-private directory", func(w specWorld, _ string, s *Spec) {
			s.DeniedListings = []string{w.tmp}
		}},
		{"a read-only leaf inside a daemon-private path", func(w specWorld, _ string, s *Spec) {
			s.DeniedPaths = []string{w.ro}
		}},
		{"a read path covering a daemon-private path", func(_ specWorld, dir string, s *Spec) {
			s.ReadScope = agent.FileReadWorkarea
			s.DeniedPaths = []string{filepath.Join(dir, "control-token")}
			s.ReadPaths = []string{dir}
		}},
		{"a read path inside a daemon-private path", func(_ specWorld, dir string, s *Spec) {
			s.ReadScope = agent.FileReadWorkarea
			s.DeniedPaths = []string{dir}
			s.ReadPaths = []string{filepath.Join(dir, "sub")}
		}},
		{"a read path that is a daemon-private directory", func(_ specWorld, dir string, s *Spec) {
			s.ReadScope = agent.FileReadWorkarea
			s.DeniedListings = []string{dir}
			s.ReadPaths = []string{dir}
		}},
		{"a read path covering a daemon-private directory", func(_ specWorld, dir string, s *Spec) {
			s.ReadScope = agent.FileReadWorkarea
			s.DeniedListings = []string{filepath.Join(dir, "sub")}
			s.ReadPaths = []string{dir}
		}},
	} {
		t.Run(tc.name+" is refused", func(t *testing.T) {
			w, dir := world(t)
			spec := w.spec()
			tc.edit(w, dir, &spec)
			_, err := resolveSpec(spec, w.guards(), evalSymlinks)
			if reason, _ := ReasonOf(err); reason != ReasonWritableSetUnrepresentable {
				t.Fatalf("err=%v, want writable_set_unrepresentable", err)
			}
			if strings.Contains(err.Error(), w.base) {
				t.Fatalf("refusal detail carries a path: %v", err)
			}
		})
	}
	t.Run("a daemon-private path nested inside the writable set stays denied and pinned", func(t *testing.T) {
		w := newSpecWorld(t)
		nested := filepath.Join(w.mut, "daemon-state", "control-token")
		spec := w.spec()
		spec.DeniedPaths = []string{nested}
		spec.DeniedListings = []string{filepath.Dir(nested)}
		resolved, err := resolveSpec(spec, w.guards(), evalSymlinks)
		if err != nil {
			t.Fatalf("resolveSpec: %v", err)
		}
		pinned := false
		for _, pin := range resolved.Pins {
			if pin == filepath.Dir(nested) {
				pinned = true
			}
		}
		if !pinned {
			t.Fatalf("pins %v lack the nested daemon-private directory", resolved.Pins)
		}
	})
}

// TestRenderSeatbelt_DaemonPrivatePathsDenied pins the profile shape in
// every read scope: the daemon-private paths are denied every read
// operation, named one by one (a file-read* wildcard would lose to the read
// scope's file-read-data allow), by subpath, and the daemon-private
// directories their listing
// by literal, open reads included; under the read scope both come after the
// session's read allowlist, so they win inside it; and the daemon-private
// paths are denied writes after the writable allows.
func TestRenderSeatbelt_DaemonPrivatePathsDenied(t *testing.T) {
	for _, scope := range []agent.ExecutionSecurityLevel{"", agent.FileReadWorkarea} {
		t.Run("scope="+string(scope), func(t *testing.T) {
			w := newSpecWorld(t)
			token := filepath.Join(w.stateHome, "control-token")
			spec := w.spec()
			spec.DeniedPaths = []string{token}
			spec.DeniedListings = []string{w.stateHome}
			spec.ReadScope = scope
			resolved, err := resolveSpec(spec, w.guards(), evalSymlinks)
			if err != nil {
				t.Fatalf("resolveSpec: %v", err)
			}
			text, err := renderSeatbelt(resolved, seatbeltHost{shared: []string{"/tmp"}}, nil, evalSymlinks)
			if err != nil {
				t.Fatalf("renderSeatbelt: %v", err)
			}
			readDeny := "(deny file-read-data file-read-metadata file-read-xattr\n  (subpath \"" + token + "\"))\n"
			listingDeny := "(deny file-read-data file-read-xattr\n  (literal \"" + w.stateHome + "\"))\n"
			order := []string{"(allow default)\n"}
			if scope != "" {
				order = append(order, "(deny file-read-data file-read-xattr)\n", "(allow file-read-data file-read-xattr\n  (subpath ")
			}
			order = append(order,
				readDeny,
				listingDeny,
				"(deny file-write*)\n(deny file-link)",
				"(allow file-write*\n",
				"(deny file-write*\n",
				`(subpath "`+token+`")`,
			)
			last := -1
			for _, needle := range order {
				at := strings.Index(text[last+1:], needle)
				if at < 0 {
					t.Fatalf("profile lacks %q after offset %d:\n%s", needle, last, text)
				}
				last += 1 + at
			}
			if strings.Count(text, readDeny) != 1 || strings.Count(text, listingDeny) != 1 {
				t.Fatalf("the daemon-private read denies render other than once:\n%s", text)
			}
		})
	}
}
