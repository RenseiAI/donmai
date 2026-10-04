package pi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/confinement"
)

// TestPiConfinementEnabled_FollowsTheRequest pins the gate (ADR-2026-10-03
// D5.1): confinement is requested by the host requirement or by a declared
// repository authority, and by nothing else — never by the mere presence of
// a backend.
func TestPiConfinementEnabled_FollowsTheRequest(t *testing.T) {
	t.Parallel()
	authority := &agent.RepositoryAuthorityPolicy{WorkareaRoot: "/w", SelectedPath: "/w/a", MutablePaths: []string{"/w/a"}}
	for _, tc := range []struct {
		name string
		spec agent.Spec
		host bool
		want bool
	}{
		{"nothing requested", agent.Spec{Cwd: "/w/a"}, false, false},
		{"host requires", agent.Spec{Cwd: "/w/a"}, true, true},
		{"authority declared", agent.Spec{Cwd: "/w/a", RepositoryAuthority: authority}, false, true},
		{"authority without a root", agent.Spec{Cwd: "/w/a", RepositoryAuthority: &agent.RepositoryAuthorityPolicy{}}, false, false},
	} {
		if got := piConfinementEnabled(tc.spec, tc.host); got != tc.want {
			t.Errorf("%s: piConfinementEnabled = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestNew_HostConfinementSwitchOnlyTightens pins the host switch: the
// environment can turn the requirement on, never off.
func TestNew_HostConfinementSwitchOnlyTightens(t *testing.T) {
	for _, tc := range []struct {
		env    string
		option bool
		want   bool
	}{
		{"required", false, true},
		{"", false, false},
		{"off", true, true},
		{"yes", false, false},
	} {
		t.Setenv(piConfinementEnvVar, tc.env)
		p, err := New(Options{skipProcess: true, RequireConfinement: tc.option})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if p.opts.RequireConfinement != tc.want {
			t.Errorf("env %q option %v: RequireConfinement = %v, want %v", tc.env, tc.option, p.opts.RequireConfinement, tc.want)
		}
	}
}

// TestConfinerForSession_RequestWithoutBackendRefuses pins D5.3 on a host
// with no backend: a session that requested confinement is refused with
// the typed backend_absent reason, never run unconfined.
func TestConfinerForSession_RequestWithoutBackendRefuses(t *testing.T) {
	if confinement.DefaultBackend() != nil {
		t.Skip("this host has a confinement backend; the darwin live tests cover it")
	}
	p := &Provider{binary: "/bin/sh", opts: Options{RequireConfinement: true}}
	_, err := p.confinerForSession(context.Background(), agent.Spec{Cwd: t.TempDir()})
	if reason, ok := confinement.ReasonOf(err); !ok || reason != confinement.ReasonBackendAbsent || !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("confinerForSession = %v, want a spawn failure carrying %s", err, confinement.ReasonBackendAbsent)
	}
}

// TestConfinerForSession_RequestWithoutWorkdirRefuses: a requested
// confinement has nothing to confine without a working directory, and is
// refused rather than dropped.
func TestConfinerForSession_RequestWithoutWorkdirRefuses(t *testing.T) {
	t.Parallel()
	p := &Provider{binary: "/bin/sh", opts: Options{RequireConfinement: true}}
	if _, err := p.confinerForSession(context.Background(), agent.Spec{}); !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("confinerForSession with no working directory = %v, want a spawn failure", err)
	}
}

// TestEndpointLoopbackPorts declares exactly the session's own model
// endpoint port when it is on this machine.
func TestEndpointLoopbackPorts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		baseURL string
		want    []int
	}{
		{"http://127.0.0.1:8123/v1", []int{8123}},
		{"http://localhost:9000", []int{9000}},
		{"http://[::1]:7000/v1", []int{7000}},
		{"http://127.0.0.1/v1", []int{80}},
		{"https://localhost/v1", []int{443}},
		{"https://api.example.com/v1", nil},
		{"http://10.0.0.5:8080", nil},
		{"", nil},
		{"::not a url", nil},
	} {
		spec := agent.Spec{}
		if tc.baseURL != "" {
			spec.Endpoint = &agent.EndpointBinding{BaseURL: tc.baseURL}
		}
		if got := endpointLoopbackPorts(spec); !slices.Equal(got, tc.want) {
			t.Errorf("endpointLoopbackPorts(%q) = %v, want %v", tc.baseURL, got, tc.want)
		}
	}
}

// TestSessionStateFS_RefusesPlantedLinks is the portable half of the resume
// hazard (the darwin live test drives it through Resume): for a confined
// layout, a symbolic link at any parent-side write path — the working
// directory, the state root, the boundary extension, the injected
// extensions directory, a cache directory, the exclude file — refuses
// before anything is written outside.
func TestSessionStateFS_RefusesPlantedLinks(t *testing.T) {
	t.Parallel()
	body := []byte("export default function activate(pi) {}\n")
	delivery := agent.ExtensionDelivery{ID: "bridge", Kind: agent.ExtensionDeliveryInline, Source: body, Basename: "bridge.ts", Digest: sha256Hex(body), Required: true}
	for _, tc := range []struct {
		name  string
		plant func(t *testing.T, cwd, outside string, layout sessionLayout)
		step  func(spec agent.Spec) error
	}{
		{"working directory", func(t *testing.T, cwd, outside string, _ sessionLayout) {
			relink(t, cwd, filepath.Join(outside, "leaf"), true)
		}, materializeStep},
		{"state root", func(t *testing.T, _, outside string, layout sessionLayout) {
			relink(t, layout.root, filepath.Join(outside, "state"), true)
		}, materializeStep},
		{"boundary extension", func(t *testing.T, _, outside string, layout sessionLayout) {
			mkdirFixture(t, layout.root)
			relink(t, layout.extension, filepath.Join(outside, "policy.ts"), false)
		}, materializeStep},
		{"git exclude", func(t *testing.T, cwd, outside string, _ sessionLayout) {
			mkdirFixture(t, filepath.Join(cwd, ".git", "info"))
			relink(t, filepath.Join(cwd, ".git", "info", "exclude"), filepath.Join(outside, "exclude"), false)
		}, materializeStep},
		{"git directory", func(t *testing.T, cwd, outside string, _ sessionLayout) {
			relink(t, filepath.Join(cwd, ".git"), filepath.Join(outside, "git"), true)
		}, materializeStep},
		{"injected extensions", func(t *testing.T, _, outside string, layout sessionLayout) {
			mkdirFixture(t, layout.root)
			relink(t, layout.injected, filepath.Join(outside, "injected"), true)
		}, func(spec agent.Spec) error {
			layout, err := materializeExtensionForSpec(spec, true)
			if err != nil {
				return err
			}
			_, err = materializeAdditionalExtensions(layout, []agent.ExtensionDelivery{delivery})
			return err
		}},
		{"cache directory", func(t *testing.T, _, outside string, layout sessionLayout) {
			mkdirFixture(t, layout.root)
			relink(t, filepath.Join(layout.root, piSessionCacheDir), filepath.Join(outside, "cache"), true)
		}, func(spec agent.Spec) error {
			layout, err := materializeExtensionForSpec(spec, true)
			if err != nil {
				return err
			}
			return writeStateDirs(layout)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := t.TempDir()
			cwd := filepath.Join(base, "leaf")
			outside := filepath.Join(base, "outside")
			mkdirFixture(t, filepath.Join(cwd, ".git"))
			mkdirFixture(t, outside)
			spec := agent.Spec{Cwd: cwd}
			tc.plant(t, cwd, outside, newSessionLayoutForSpec(spec))
			before := listTree(t, outside)
			err := tc.step(spec)
			if err == nil || !strings.Contains(err.Error(), "symbolic link") {
				t.Fatalf("step = %v, want a refusal naming the symbolic link", err)
			}
			if after := listTree(t, outside); after != before {
				t.Fatalf("wrote outside before refusing:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
}

// materializeStep is the first parent-side write a launch makes.
func materializeStep(spec agent.Spec) error {
	_, err := materializeExtensionForSpec(spec, true)
	return err
}

// writeStateDirs creates the session tmp and cache directories the way
// confinePiSession does, without needing a backend.
func writeStateDirs(layout sessionLayout) error {
	state, err := openSessionStateFS(layout)
	if err != nil {
		return err
	}
	defer func() { _ = state.Close() }()
	for _, dir := range []string{
		filepath.Join(layout.root, piSessionTmpDir),
		filepath.Join(layout.root, piSessionCacheDir, "go-build"),
	} {
		if err := state.mkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// TestSessionStateFS_WritesFreshUnlinkedCopies pins the write semantics:
// an existing file hard-linked elsewhere is replaced, never written through,
// so the other name keeps its bytes; and a confined inline delivery is its
// own single-link file.
func TestSessionStateFS_WritesFreshUnlinkedCopies(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	cwd := filepath.Join(base, "leaf")
	mkdirFixture(t, cwd)
	spec := agent.Spec{Cwd: cwd}
	layout, err := materializeExtensionForSpec(spec, true)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	other := filepath.Join(base, "other-name")
	if err := os.Link(layout.extension, other); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("outside bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := materializeExtensionForSpec(spec, true); err != nil {
		t.Fatalf("re-materialize over a hard-linked extension: %v", err)
	}
	if got, _ := os.ReadFile(other); string(got) != "outside bytes" {
		t.Errorf("the write went through the hard link: other name now holds %q", got)
	}
	if got, _ := os.ReadFile(layout.extension); string(got) != string(extensionSource()) {
		t.Errorf("the extension was not rewritten")
	}

	body := []byte("export default function activate(pi) {}\n")
	paths, err := materializeAdditionalExtensions(layout, []agent.ExtensionDelivery{
		{ID: "bridge", Kind: agent.ExtensionDeliveryInline, Source: body, Basename: "bridge.ts", Digest: sha256Hex(body), Required: true},
	})
	if err != nil {
		t.Fatalf("materializeAdditionalExtensions: %v", err)
	}
	if links := linkCount(t, paths[0]); links != 1 {
		t.Errorf("confined inline delivery has %d links, want 1: a shared-cache hard link makes the confined set unrepresentable", links)
	}
}

func relink(t *testing.T, link, target string, dir bool) {
	t.Helper()
	if dir {
		mkdirFixture(t, target)
	} else if err := os.WriteFile(target, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(link); err != nil {
		t.Fatal(err)
	}
	mkdirFixture(t, filepath.Dir(link))
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func mkdirFixture(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
}

// listTree renders every entry under dir with its size.
func listTree(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		b.WriteString(rel + ":" + info.Mode().String() + ":" + strconv.FormatInt(info.Size(), 10) + " ")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func linkCount(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("no link count for %s", path)
	}
	return uint64(st.Nlink)
}
