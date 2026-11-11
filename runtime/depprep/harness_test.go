package depprep

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeProgram is the fake manager's program name. It is deliberately not a
// real manager's name, so no host binary on /usr/bin:/bin can answer for it.
const fakeProgram = "fakepnpm"

// fakeManager configures one generated fake manager script.
type fakeManager struct {
	// version is what "--version" prints when the leaf pins nothing; a
	// pinned leaf gets its pin back, like a self-switching manager.
	version string
	// fetchExit and installExit are the phases' exit statuses.
	fetchExit, installExit int
	// installSleep makes the install phase hang for this many seconds.
	installSleep int
	// stray makes the install also write an undeclared file into the leaf.
	stray bool
}

// fixture is one test's leaf, scratch, toolchain and log directory.
type fixture struct {
	t         *testing.T
	root      string
	scratch   string
	toolchain string
	logs      string
}

func newFixture(t *testing.T, fm fakeManager) *fixture {
	t.Helper()
	base := t.TempDir()
	f := &fixture{
		t:         t,
		root:      filepath.Join(base, "leaf"),
		scratch:   filepath.Join(base, "scratch"),
		toolchain: filepath.Join(base, "toolchain"),
		logs:      filepath.Join(base, "logs"),
	}
	for _, d := range []string{f.root, f.scratch, f.toolchain, f.logs} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	// TempDir may sit under a symlinked prefix (macOS /var -> /private/var);
	// the request contract wants clean absolute paths, which these are.
	f.writeLeaf("pnpm-lock.yaml", "lockfileVersion: '9.0'\n")
	f.writeLeaf(".gitignore", "node_modules\n")
	f.writeFakeManager(fm)
	return f
}

func (f *fixture) writeLeaf(rel, content string) {
	f.t.Helper()
	p := filepath.Join(f.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) pin(decl string) {
	f.writeLeaf("package.json", fmt.Sprintf(`{"name":"leaf","packageManager":%q}`, decl))
}

// writeFakeManager generates the fake manager. It logs each phase to
// order.log, dumps its environment per phase, and models the behaviours
// Prepare must control: the store must carry over from fetch to install, a
// dependency postinstall runs unless npm_config_ignore_scripts=true, and a
// repository .pnpmfile.cjs runs unless npm_config_ignore_pnpmfile=true.
func (f *fixture) writeFakeManager(fm fakeManager) {
	f.t.Helper()
	version := fm.version
	if version == "" {
		version = "9.15.9"
	}
	stray := ""
	if fm.stray {
		stray = `echo stray > "$PWD/stray.txt"`
	}
	script := fmt.Sprintf(`#!/bin/sh
logs=%[1]q
case "$1" in
--version)
  echo version >> "$logs/order.log"
  pin=$(sed -n 's/.*"packageManager":"pnpm@\([0-9.]*\).*/\1/p' package.json 2>/dev/null)
  if [ -n "$pin" ]; then echo "$pin"; else echo %[2]q; fi
  exit 0 ;;
fetch)
  echo fetch >> "$logs/order.log"
  env > "$logs/fetch.env"
  if [ -f .pnpmfile.cjs ] && [ "$npm_config_ignore_pnpmfile" != "true" ]; then touch "$logs/pnpmfile-ran"; fi
  if [ -f .pnpmfile.cjs ]; then echo "$npm_config_ignore_pnpmfile" > "$logs/fetch-pnpmfile-setting"; fi
  mkdir -p "$NPM_CONFIG_STORE_DIR" && touch "$NPM_CONFIG_STORE_DIR/fetched"
  exit %[3]d ;;
install)
  echo install >> "$logs/order.log"
  env > "$logs/install.env"
  if [ %[5]d -gt 0 ]; then sleep %[5]d; fi
  [ -f "$NPM_CONFIG_STORE_DIR/fetched" ] || { echo "store not fetched" >&2; exit 3; }
  mkdir -p node_modules/dep
  if [ "$npm_config_ignore_scripts" != "true" ]; then touch node_modules/dep/postinstall-ran; fi
  %[6]s
  echo "install output line"
  exit %[4]d ;;
esac
echo "unexpected: $*" >&2
exit 64
`, f.logs, version, fm.fetchExit, fm.installExit, fm.installSleep, stray)
	if err := os.WriteFile(filepath.Join(f.toolchain, fakeProgram), []byte(script), 0o755); err != nil { //nolint:gosec // test fixture must be executable
		f.t.Fatal(err)
	}
}

// pnpmEntry is a pnpm-shaped entry over the fake program.
func pnpmEntry(versions string) Entry {
	return Entry{
		Manager:        "pnpm",
		Ecosystem:      "node",
		Versions:       versions,
		KitID:          "typescript",
		KitDigestKind:  "sha256",
		KitDigest:      "abc123",
		Lockfiles:      []string{"pnpm-lock.yaml"},
		Inputs:         []string{"pnpm-workspace.yaml", "**/package.json", ".npmrc", ".pnpmfile.cjs"},
		VersionCommand: fakeProgram + " --version",
		Commands: Commands{
			Fetch:   fakeProgram + " fetch",
			Install: fakeProgram + " install --offline --frozen-lockfile",
			Online:  fakeProgram + " install --prefer-offline --frozen-lockfile",
		},
		Store: StoreLayout{
			Env:       []string{"NPM_CONFIG_STORE_DIR"},
			Sharing:   "content-addressed",
			Integrity: "on-use",
			Content:   []string{"v10/files", "v10/index"},
		},
		ImportEnv: map[string]string{"NPM_CONFIG_PACKAGE_IMPORT_METHOD": "clone-or-copy"},
		NoCode: &NoCode{Env: map[string]string{
			"npm_config_ignore_scripts":  "true",
			"npm_config_ignore_pnpmfile": "true",
		}},
		Installed:       []string{"node_modules", "**/node_modules"},
		RunsPackageCode: true,
	}
}

func (f *fixture) request() Request {
	return Request{
		Root:      f.root,
		Subject:   Subject{Kind: SubjectInvocation, ID: "inv-1", Attrs: map[string]string{"org": "o"}},
		Toolchain: []string{f.toolchain},
		Scratch:   f.scratch,
		Network:   NetworkAllow,
	}
}

// countingRunner counts every process and remembers each step deadline.
type countingRunner struct {
	inner     procRunner
	n         atomic.Int64
	mu        sync.Mutex
	deadlines []time.Time
}

func (c *countingRunner) run(ctx context.Context, p proc) (procResult, error) {
	c.n.Add(1)
	if d, ok := ctx.Deadline(); ok {
		c.mu.Lock()
		c.deadlines = append(c.deadlines, d)
		c.mu.Unlock()
	}
	return c.inner.run(ctx, p)
}

// newPreparer builds a Preparer whose processes are counted.
func newPreparer(t *testing.T, cfg Config) (*Preparer, *countingRunner) {
	t.Helper()
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	counter := &countingRunner{inner: osRunner{}}
	p.exec = counter
	return p, counter
}

func (f *fixture) readLog(name string) string {
	f.t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.logs, name))
	if err != nil {
		return ""
	}
	return string(raw)
}

func (f *fixture) order() []string {
	return strings.Fields(f.readLog("order.log"))
}

// envKeys parses an env dump into its key set. Values are never returned.
func (f *fixture) envKeys(name string) map[string]bool {
	f.t.Helper()
	keys := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(f.readLog(name)))
	for sc.Scan() {
		if eq := strings.IndexByte(sc.Text(), '='); eq > 0 {
			keys[sc.Text()[:eq]] = true
		}
	}
	return keys
}

// envValue returns one key's value from an env dump.
func (f *fixture) envValue(name, key string) string {
	f.t.Helper()
	sc := bufio.NewScanner(strings.NewReader(f.readLog(name)))
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), key+"=") {
			return strings.TrimPrefix(sc.Text(), key+"=")
		}
	}
	return ""
}

func (f *fixture) exists(rel string) bool {
	_, err := os.Lstat(filepath.Join(f.root, filepath.FromSlash(rel)))
	return err == nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is required for the leaf hygiene test: %v", err)
	}
	cmd := exec.Command("git", "init", "-q", dir) //nolint:gosec // test fixture: a fresh temp directory
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
}

func wantCode(t *testing.T, err error, code Code) *Error {
	t.Helper()
	if err == nil {
		t.Fatalf("err = nil, want code %s", code)
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("err %T is not a *depprep.Error: %v", err, err)
	}
	if e.Code != code {
		t.Fatalf("code = %s, want %s (%v)", e.Code, code, err)
	}
	if !errors.Is(err, sentinels[code]) {
		t.Fatalf("errors.Is(err, sentinel of %s) = false", code)
	}
	return e
}
