//go:build darwin

package pi

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/confinement"
)

// These tests run the real pnpm inside a confined pi seat under the workarea
// read scope, through the adapter's own session plan: its cache and runtime
// bindings and its read allowlist. They skip without pnpm on PATH, like the
// real-binary tests (hosted CI has none).

// pnpmVersion is a pnpm release number.
type pnpmVersion struct{ major, minor, patch int }

// pnpmStoreLockFixed is the first pnpm 12 release that keeps its store
// operation lock under XDG_RUNTIME_DIR, which a confined seat binds to its
// own directory. Earlier pnpm 12 releases lock under a shared directory in
// /tmp, which confinement denies: every install fails with
// ERR_PNPM_STORE_DIR_OPEN_OPERATION_LOCK. pnpm before 12 keeps no such lock.
var pnpmStoreLockFixed = pnpmVersion{12, 8, 2}

func (v pnpmVersion) String() string {
	return fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch)
}

func (v pnpmVersion) before(other pnpmVersion) bool {
	return slices.Compare([]int{v.major, v.minor, v.patch}, []int{other.major, other.minor, other.patch}) < 0
}

// parsePnpmVersion reads the release number from `pnpm --version` output: the
// last field, as major.minor.patch with any pre-release suffix dropped.
func parsePnpmVersion(output string) (pnpmVersion, error) {
	fields := strings.Fields(output)
	if len(fields) == 0 {
		return pnpmVersion{}, errors.New("no version printed")
	}
	core, _, _ := strings.Cut(strings.TrimPrefix(fields[len(fields)-1], "v"), "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return pnpmVersion{}, fmt.Errorf("%q is not major.minor.patch", fields[len(fields)-1])
	}
	var nums [3]int
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return pnpmVersion{}, fmt.Errorf("%q is not major.minor.patch", fields[len(fields)-1])
		}
		nums[i] = n
	}
	return pnpmVersion{nums[0], nums[1], nums[2]}, nil
}

// storeLockUnsupported reports whether this pnpm locks its store somewhere a
// confined seat cannot: a pnpm 12 release before pnpmStoreLockFixed.
func (v pnpmVersion) storeLockUnsupported() bool {
	return v.major == pnpmStoreLockFixed.major && v.before(pnpmStoreLockFixed)
}

// TestPnpmVersionGate pins which pnpm releases the live pnpm tests run
// against and which they skip with the upgrade instruction.
func TestPnpmVersionGate(t *testing.T) {
	for _, tc := range []struct {
		output      string
		want        pnpmVersion
		unsupported bool
	}{
		{"10.34.1\n", pnpmVersion{10, 34, 1}, false},
		{"11.0.0", pnpmVersion{11, 0, 0}, false},
		{"12.0.0", pnpmVersion{12, 0, 0}, true},
		{"12.8.1\n", pnpmVersion{12, 8, 1}, true},
		{"12.8.2", pnpmVersion{12, 8, 2}, false},
		{"12.9.1\n", pnpmVersion{12, 9, 1}, false},
		{"12.9.1-beta.3", pnpmVersion{12, 9, 1}, false},
		{"v13.0.0", pnpmVersion{13, 0, 0}, false},
	} {
		got, err := parsePnpmVersion(tc.output)
		if err != nil || got != tc.want {
			t.Errorf("parsePnpmVersion(%q) = %v, %v; want %v", tc.output, got, err, tc.want)
			continue
		}
		if got.storeLockUnsupported() != tc.unsupported {
			t.Errorf("pnpm %s: storeLockUnsupported = %v, want %v", got, !tc.unsupported, tc.unsupported)
		}
	}
	for _, bad := range []string{"", "pnpm", "12.9", "12.x.1", "-1.0.0"} {
		if _, err := parsePnpmVersion(bad); err == nil {
			t.Errorf("parsePnpmVersion(%q) accepted", bad)
		}
	}
}

// requirePnpm returns the version of the pnpm on PATH, or skips: without
// pnpm, and with a pnpm 12 whose store lock a confined seat cannot take. A
// skip says so and names the upgrade; the tests never run against a pnpm
// they would fail on for that reason, and never pass without running.
func requirePnpm(t *testing.T) pnpmVersion {
	t.Helper()
	if _, err := exec.LookPath("pnpm"); err != nil {
		t.Skip("real-toolchain pnpm test: `pnpm` not on PATH — skipping")
	}
	out, err := exec.Command("pnpm", "--version").Output()
	if err != nil {
		t.Skipf("real-toolchain pnpm test: pnpm --version: %v — skipping", err)
	}
	v, err := parsePnpmVersion(string(out))
	if err != nil {
		t.Fatalf("pnpm --version = %q: %v", out, err)
	}
	if v.storeLockUnsupported() {
		t.Skipf("real-toolchain pnpm test: pnpm %s locks its store under /tmp, which confinement denies (ERR_PNPM_STORE_DIR_OPEN_OPERATION_LOCK); pnpm %s and later lock under the seat's runtime directory — upgrade pnpm (`brew upgrade pnpm`) — skipping", v, pnpmStoreLockFixed)
	}
	return v
}

// privateRegistry is a registry stub serving one scoped package only to a
// request that carries its token.
type privateRegistry struct {
	srv                      *httptest.Server
	token, scope, name       string
	authorized, unauthorized atomic.Int32
}

func newPrivateRegistry(t *testing.T) *privateRegistry {
	t.Helper()
	r := &privateRegistry{token: "probe-token-" + strconv.FormatInt(time.Now().UnixNano(), 36), scope: "@probe", name: "@probe/private-pkg"}
	tarball := npmTarball(t, r.name, "1.0.0")
	sum := sha512.Sum512(tarball)
	integrity := "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer "+r.token {
			r.unauthorized.Add(1)
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		r.authorized.Add(1)
		path := strings.ReplaceAll(req.URL.EscapedPath(), "%2f", "/")
		path = strings.ReplaceAll(path, "%2F", "/")
		switch {
		case strings.HasSuffix(path, ".tgz"):
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(tarball)
		case path == "/"+r.name:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name":      r.name,
				"dist-tags": map[string]string{"latest": "1.0.0"},
				"time":      map[string]string{"created": "2020-01-01T00:00:00.000Z", "modified": "2020-01-01T00:00:00.000Z", "1.0.0": "2020-01-01T00:00:00.000Z"},
				"versions": map[string]any{"1.0.0": map[string]any{
					"name": r.name, "version": "1.0.0",
					"dist": map[string]string{"tarball": r.srv.URL + "/" + r.name + "/-/private-pkg-1.0.0.tgz", "integrity": integrity},
				}},
			})
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(r.srv.Close)
	return r
}

// npmrc is the user configuration that routes the scope to the stub and
// authenticates against it.
func (r *privateRegistry) npmrc() string {
	host := strings.TrimPrefix(r.srv.URL, "http://")
	return fmt.Sprintf("%s:registry=%s/\n//%s/:_authToken=%s\n", r.scope, r.srv.URL, host, r.token)
}

// npmTarball builds a package tarball the way the registry serves one.
func npmTarball(t *testing.T, name, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for file, body := range map[string]string{
		"package/package.json": fmt.Sprintf(`{"name":%q,"version":%q,"main":"index.js"}`, name, version),
		"package/index.js":     "module.exports = 'private';\n",
	} {
		if err := tw.WriteHeader(&tar.Header{Name: file, Mode: 0o644, Size: int64(len(body)), ModTime: time.Unix(1577836800, 0)}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// seatPlan prepares the confined session plan a pi seat in w gets under the
// workarea read scope, with home as the operator home and endpoint as the
// session's model endpoint (its loopback port is the one a seat may dial).
// drop removes read paths, for the controls.
func seatPlan(t *testing.T, w liveWorld, home, endpoint string, drop ...string) (*confinement.Plan, sessionLayout) {
	t.Helper()
	dirs := *liveConfinementDirs(t)
	dirs.home = home
	bin := writeLiveHarness(t, w)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c, err := ensurePiConfiner(liveCtx(t), bin, dirs, []string{exe})
	if err != nil {
		t.Fatalf("ensurePiConfiner: %v", err)
	}
	spec := w.spec()
	if endpoint != "" {
		spec.Endpoint = &agent.EndpointBinding{BaseURL: endpoint}
	}
	layout, err := materializeExtensionForSpec(spec, true)
	if err != nil {
		t.Fatal(err)
	}
	p := &Provider{binary: bin, opts: Options{ConfinementReadScope: agent.FileReadWorkarea, RequireConfinement: true, confinementDirs: &dirs}}
	reads, err := p.sessionReadScope(home)
	if err != nil {
		t.Fatal(err)
	}
	// pnpm's own install, as a host declares a toolchain outside the
	// runtime paths (a Homebrew install is inside them already).
	if path, err := exec.LookPath("pnpm"); err == nil {
		if root, err := confinement.InstallRoot(path); err == nil {
			reads.paths = append(reads.paths, root)
		}
	}
	reads.paths = slices.DeleteFunc(reads.paths, func(path string) bool { return slices.Contains(drop, path) })
	plan, err := confinePiSession(spec, layout, c, reads)
	if err != nil {
		t.Fatalf("confinePiSession: %v", err)
	}
	t.Cleanup(func() { _ = plan.Release() })
	return plan, layout
}

// runSeat runs argv in dir inside the plan with the seat's environment
// bindings, the given home and any extra entries, and returns its exit code
// and output.
func runSeat(t *testing.T, plan *confinement.Plan, dir, home string, extra []string, argv ...string) (int, string) {
	t.Helper()
	wrapped, err := plan.Command(argv)
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	cmd := exec.Command(wrapped[0], wrapped[1:]...) //nolint:gosec // G204: the confined argv under test.
	cmd.Dir = dir
	cmd.Env = append(append(append(os.Environ(), plan.Environment()...), "HOME="+home, "CI=1"), extra...)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), string(out)
	}
	if err != nil {
		t.Fatalf("run %v: %v", argv, err)
	}
	return 0, string(out)
}

// TestRealToolchain_PnpmPrivateInstallConfined: a confined seat under the
// workarea read scope installs a private scoped package — the registry
// answers only to the token in the operator's ~/.npmrc, which the adapter
// declares readable — and then reinstalls it with --frozen-lockfile
// --prefer-offline from its per-session store. Its store operation lock
// lands in the session's runtime directory. The controls: without ~/.npmrc
// readable the private package does not resolve, and (pnpm 12 and later)
// without the runtime binding the store lock falls back to a shared
// directory under /tmp the confinement refuses.
func TestRealToolchain_PnpmPrivateInstallConfined(t *testing.T) {
	version := requirePnpm(t)
	major := version.major
	reg := newPrivateRegistry(t)
	world := func(t *testing.T) (liveWorld, string) {
		w := newLiveWorld(t)
		home := filepath.Join(filepath.Dir(w.root), "home")
		mustMkdir(t, home)
		if err := os.WriteFile(filepath.Join(home, ".npmrc"), []byte(reg.npmrc()), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(w.mut, "package.json"), []byte(`{"name":"seat","version":"1.0.0","private":true,"dependencies":{"`+reg.name+`":"1.0.0"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		return w, home
	}

	w, home := world(t)
	plan, layout := seatPlan(t, w, home, reg.srv.URL)
	if code, out := runSeat(t, plan, w.mut, home, nil, "pnpm", "install"); code != 0 {
		t.Fatalf("confined pnpm install: exit %d:\n%s", code, out)
	}
	if err := os.RemoveAll(filepath.Join(w.mut, "node_modules")); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if code, out := runSeat(t, plan, w.mut, home, nil, "pnpm", "install", "--frozen-lockfile", "--prefer-offline"); code != 0 {
		t.Fatalf("confined pnpm install --frozen-lockfile --prefer-offline: exit %d:\n%s", code, out)
	}
	t.Logf("confined pnpm %d install --frozen-lockfile --prefer-offline: %s", major, time.Since(start).Round(time.Millisecond))
	if _, err := os.Stat(filepath.Join(w.mut, "node_modules", reg.name, "package.json")); err != nil {
		t.Fatalf("the private package is not installed: %v", err)
	}
	if reg.authorized.Load() == 0 || reg.unauthorized.Load() != 0 {
		t.Fatalf("registry saw %d authorized and %d unauthorized requests; want the token on every one", reg.authorized.Load(), reg.unauthorized.Load())
	}
	if major >= 12 {
		locks, _ := filepath.Glob(filepath.Join(layout.root, piSessionCacheDir, "run", "pnpm-store-operation-locks-*"))
		if len(locks) == 0 {
			t.Fatalf("pnpm %d took no store lock in the session runtime directory", major)
		}
	}

	t.Run("without ~/.npmrc readable", func(t *testing.T) {
		w, home := world(t)
		plan, _ := seatPlan(t, w, home, reg.srv.URL, filepath.Join(home, ".npmrc"))
		code, out := runSeat(t, plan, w.mut, home, nil, "pnpm", "install")
		if code == 0 || !strings.Contains(out, reg.name) {
			t.Fatalf("pnpm install without the user configuration: exit %d; want the private package unresolved:\n%s", code, out)
		}
		if _, err := os.Stat(filepath.Join(w.mut, "node_modules", reg.name)); err == nil {
			t.Fatal("the private package was installed without the user configuration")
		}
	})

	t.Run("without the session runtime directory", func(t *testing.T) {
		if major < 12 {
			t.Skipf("pnpm %s keeps no store operation lock; nothing to control", version)
		}
		w, home := world(t)
		plan, _ := seatPlan(t, w, home, reg.srv.URL)
		code, out := runSeat(t, plan, w.mut, home, []string{"XDG_RUNTIME_DIR="}, "pnpm", "install")
		if code == 0 || !strings.Contains(out, "pnpm-store-operation-locks") {
			t.Fatalf("pnpm install without the runtime binding: exit %d; want the store lock refused:\n%s", code, out)
		}
	})
}

// TestRealToolchain_PnpmFrozenInstallInRepoConfined runs a real checkout's
// own install — pnpm install --frozen-lockfile --prefer-offline — inside a
// confined seat under the workarea read scope, with the operator's real
// home, and reports how long it took and what the per-session store holds.
// The checkout is DONMAI_TEST_PNPM_REPO: a clone with its own .git
// directory (a confined leaf owns its git directory) and no node_modules
// yet. It is installed in place. Skips when unset.
func TestRealToolchain_PnpmFrozenInstallInRepoConfined(t *testing.T) {
	repo := os.Getenv("DONMAI_TEST_PNPM_REPO")
	if repo == "" {
		t.Skip("DONMAI_TEST_PNPM_REPO names no checkout — skipping")
	}
	major := requirePnpm(t).major
	repo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	w := liveWorld{root: filepath.Dir(repo), mut: repo}
	plan, layout := seatPlan(t, w, home, "")
	start := time.Now()
	code, out := runSeat(t, plan, repo, home, nil, "pnpm", "install", "--frozen-lockfile", "--prefer-offline")
	elapsed := time.Since(start)
	if code != 0 {
		t.Fatalf("confined pnpm %d install --frozen-lockfile --prefer-offline in %s: exit %d after %s:\n%s", major, filepath.Base(repo), code, elapsed.Round(time.Second), tail(out, 40))
	}
	store := filepath.Join(layout.root, piSessionCacheDir, "pnpm-store")
	du, _ := exec.Command("/usr/bin/du", "-sk", store).Output() //nolint:gosec // G204: fixed tool, test path.
	kb, _ := strconv.Atoi(strings.Fields(string(du) + " 0")[0])
	t.Logf("confined pnpm %d frozen install of %s: %s, per-session store %.2f GiB", major, filepath.Base(repo), elapsed.Round(time.Second), float64(kb)/(1<<20))
}

func tail(s string, lines int) string {
	parts := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n")
}
