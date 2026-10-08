//go:build darwin

package confinement

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// daemonLiveWorld is a live session laid out the way a host lays out its
// state home: one directory holds the daemon's control token (a sentinel
// here), a sibling file, and the session's whole work area beneath it —
// workarea, session tmp and caches. The token rides Spec.DeniedPaths and
// the directory Spec.DeniedListings, the shape the composing binary
// passes in.
type daemonLiveWorld struct {
	w           specWorld
	c           *Confiner
	tokenDir    string
	tokenFile   string
	siblingFile string
	// nestedFile is a second token file nested inside the writable set,
	// which the read allowlist covers under a read scope. Only a
	// daemon-private deny rendered after the allowlist refuses its read,
	// and only the daemon-private write deny refuses its write: the
	// blanket write deny cannot cover anything inside the writable set.
	nestedFile string
	nestedDir  string
}

func newDaemonLiveWorld(t *testing.T) daemonLiveWorld {
	t.Helper()
	w, c := liveWorld(t)
	// liveWorld keeps every session path beneath base, so base stands in
	// for the host state home that holds the token beside the work area.
	tokenFile := filepath.Join(w.base, "control-token")
	siblingFile := filepath.Join(w.base, "sibling-secret")
	if err := os.WriteFile(tokenFile, []byte("sentinel-control-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(siblingFile, []byte("sibling\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	nestedDir := filepath.Join(w.mut, "daemon-state")
	nestedFile := filepath.Join(nestedDir, "control-token")
	if err := os.MkdirAll(nestedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nestedFile, []byte("sentinel-nested-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return daemonLiveWorld{w: w, c: c, tokenDir: w.base, tokenFile: tokenFile, siblingFile: siblingFile, nestedFile: nestedFile, nestedDir: nestedDir}
}

func (d daemonLiveWorld) spec(scope agent.ExecutionSecurityLevel, readPaths ...string) Spec {
	spec := d.w.spec()
	spec.DeniedPaths = []string{d.tokenFile, d.nestedFile, d.nestedDir}
	spec.DeniedListings = []string{d.tokenDir}
	spec.ReadScope = scope
	spec.ReadPaths = readPaths
	return spec
}

// daemonPrivateReadBlock returns the daemon-private read rules of the
// plan's rendered profile, exactly as rendered, so a red control can strip
// them and only them.
func daemonPrivateReadBlock(t *testing.T, plan *Plan) string {
	t.Helper()
	wrapped, err := plan.Command([]string{"/usr/bin/true"})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	var profilePath string
	for i, arg := range wrapped {
		if arg == "-f" && i+1 < len(wrapped) {
			profilePath = wrapped[i+1]
		}
	}
	raw, err := os.ReadFile(profilePath) //nolint:gosec // G304: the profile path comes from the plan under test.
	if err != nil {
		t.Fatal(err)
	}
	_, block, found := strings.Cut(string(raw), "; Daemon-private paths: no read of any kind.")
	if !found {
		t.Fatalf("the rendered profile holds no daemon-private read rules:\n%s", raw)
	}
	block, _, _ = strings.Cut(block, "\n\n")
	return "; Daemon-private paths: no read of any kind." + block
}

// TestSeatbelt_DaemonPrivateDeniedEveryReadScope is the live proof, in
// every read scope (open reads, the default, and the workarea scope), that
// a confined child gets EPERM reading the control token (contents, an
// open, its metadata) and listing the directory that holds it, cannot
// write the token or plant a file beside it, and cannot read or write a
// token nested inside its own writable set — while ordinary seat work in a
// work area beneath that directory succeeds: reading and writing the work
// area, reading system files, git and a go build. A red control strips the
// daemon-private read rules from the rendered profile and shows the same
// reads then succeed, so the test discriminates those rules, not the
// fixture.
func TestSeatbelt_DaemonPrivateDeniedEveryReadScope(t *testing.T) {
	for _, scope := range []agent.ExecutionSecurityLevel{"", agent.FileReadWorkarea} {
		name := string(scope)
		if name == "" {
			name = "open"
		}
		t.Run(name, func(t *testing.T) {
			d := newDaemonLiveWorld(t)
			// Unconfined control: the token reads and its directory
			// lists, so a refusal below means the boundary.
			if raw, err := os.ReadFile(d.tokenFile); err != nil || !strings.Contains(string(raw), "sentinel-control-token") {
				t.Fatalf("unconfined control: the token does not read: %v", err)
			}
			if _, err := os.ReadDir(d.tokenDir); err != nil {
				t.Fatalf("unconfined control: the token directory does not list: %v", err)
			}
			gitconfig := filepath.Join(d.w.base, "gitconfig")
			if err := os.WriteFile(gitconfig, []byte("[user]\n\tname = probe\n\temail = probe@example.invalid\n[init]\n\tdefaultBranch = main\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var reads []string
			if scope != "" {
				// The read scope needs the toolchain and the git user
				// config declared, as an adapter declares them.
				reads = []string{goEnv(t, "GOROOT"), gitconfig}
			}
			plan, err := d.c.prepare(d.spec(scope, reads...), "")
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			t.Cleanup(func() { _ = plan.Release() })

			if code, out := runConfined(t, plan, d.w.mut, nil, "/bin/cat", d.tokenFile); code == 0 || !strings.Contains(out, "Operation not permitted") {
				t.Errorf("confined cat of the control token: exit %d: %q; want EPERM", code, out)
			}
			if code, out := runConfined(t, plan, d.w.mut, nil, "/bin/ls", d.tokenDir); code == 0 || !strings.Contains(out, "Operation not permitted") {
				t.Errorf("confined ls of the token directory: exit %d: %q; want EPERM", code, out)
			}
			if code, out := runConfined(t, plan, d.w.mut, nil, "/bin/cat", d.nestedFile); code == 0 || !strings.Contains(out, "Operation not permitted") {
				t.Errorf("confined cat of the nested token: exit %d: %q; want EPERM", code, out)
			}
			results := runProbeSteps(t, plan, d.w,
				probeStep{ID: "token-read", Op: opRead, Path: d.tokenFile},
				probeStep{ID: "token-open", Op: opOpen, Path: d.tokenFile},
				probeStep{ID: "token-stat", Op: opStat, Path: d.tokenFile},
				probeStep{ID: "dir-list", Op: opList, Path: d.tokenDir},
				probeStep{ID: "nested-read", Op: opRead, Path: d.nestedFile},
				probeStep{ID: "nested-list", Op: opList, Path: d.nestedDir},
				probeStep{ID: "token-write", Op: opWrite, Path: d.tokenFile},
				probeStep{ID: "sibling-create", Op: opCreate, Path: filepath.Join(d.tokenDir, "planted")},
				probeStep{ID: "nested-write", Op: opWrite, Path: d.nestedFile},
				probeStep{ID: "nested-create", Op: opCreate, Path: filepath.Join(d.nestedDir, "planted")},
			)
			for _, id := range []string{"token-read", "token-open", "token-stat", "dir-list", "nested-read", "nested-list", "token-write", "sibling-create", "nested-write", "nested-create"} {
				if result, ok := results[id]; !ok || !refused(result) {
					t.Errorf("confined probe %s: %+v; want EPERM", id, result)
				}
			}
			if raw, err := os.ReadFile(d.tokenFile); err != nil || string(raw) != "sentinel-control-token\n" {
				t.Errorf("the token changed despite the deny: %q, err %v", raw, err)
			}
			if raw, err := os.ReadFile(d.nestedFile); err != nil || string(raw) != "sentinel-nested-token\n" {
				t.Errorf("the nested token changed despite the deny: %q, err %v", raw, err)
			}
			for _, planted := range []string{filepath.Join(d.tokenDir, "planted"), filepath.Join(d.nestedDir, "planted")} {
				if exists(planted) {
					t.Errorf("%s landed despite the deny", filepath.Base(filepath.Dir(planted))+"/planted")
				}
			}

			// Ordinary seat work beneath the token's directory: the work
			// area reads and writes, the working directory resolves,
			// system files read, and git and a go build succeed.
			gomod, gopath := filepath.Join(d.w.cache, "go-mod"), filepath.Join(d.w.cache, "go-path")
			for _, dir := range []string{gomod, gopath} {
				if err := os.MkdirAll(dir, 0o750); err != nil {
					t.Fatal(err)
				}
			}
			env := []string{"HOME=" + d.w.home, "GIT_CONFIG_GLOBAL=" + gitconfig, "GIT_CONFIG_NOSYSTEM=1", "GOMODCACHE=" + gomod, "GOPATH=" + gopath, "GOTOOLCHAIN=local", "GOFLAGS=", "GOENV=off"}
			repo := filepath.Join(d.w.mut, "repo")
			script := strings.Join([]string{
				`set -e`,
				`test "$(/bin/pwd -P)" = "` + d.w.mut + `"`,
				`echo work > work.txt`,
				`grep -q work work.txt`,
				`ls . >/dev/null`,
				`/bin/cat /etc/hosts >/dev/null`,
				`/bin/cat /etc/shells >/dev/null`,
				`mkdir -p "` + repo + `" && cd "` + repo + `"`,
				`git init -q .`,
				`printf 'module example.com/probe\n\ngo 1.21\n' > go.mod`,
				`printf 'package main\n\nfunc main() { println("built") }\n' > main.go`,
				`git add -A`,
				`git commit -q -m first`,
				`git status --porcelain`,
				`git log --oneline | grep -q first`,
				`go build -o "$TMPDIR/probe" .`,
				`"$TMPDIR/probe"`,
				`echo seat-work-ok`,
			}, "\n")
			if code, out := runConfined(t, plan, d.w.mut, env, "/bin/sh", "-c", script); code != 0 || !strings.Contains(out, "seat-work-ok") || !strings.Contains(out, "built") {
				t.Fatalf("seat work beneath the token's directory: exit %d: %s", code, out)
			}

			// Red control: with the daemon-private read rules stripped from
			// the rendered profile, and nothing else, the token and the
			// nested token read. Under the read scope the token itself
			// stays held by the blanket read deny, so the nested token,
			// which the allowlist covers, is what discriminates there.
			red := runProbeStepsVia(t, d.w, func(env []string, argv []string) (int, string) {
				return runRewritten(t, plan, d.w.mut, env, daemonPrivateReadBlock(t, plan), "", argv...)
			},
				probeStep{ID: "token-read", Op: opRead, Path: d.tokenFile},
				probeStep{ID: "dir-list", Op: opList, Path: d.tokenDir},
				probeStep{ID: "nested-read", Op: opRead, Path: d.nestedFile},
			)
			if red["nested-read"].Err != "" {
				t.Errorf("with the daemon-private read rules stripped, the nested token still refused: %q; the test does not discriminate", red["nested-read"].Err)
			}
			if scope == "" {
				for _, id := range []string{"token-read", "dir-list"} {
					if red[id].Err != "" {
						t.Errorf("with the daemon-private read rules stripped and reads open, %s still refused: %q; the test does not discriminate", id, red[id].Err)
					}
				}
			}
		})
	}
}
