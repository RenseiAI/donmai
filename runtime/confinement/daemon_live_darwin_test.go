//go:build darwin

package confinement

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// daemonLiveWorld is a live session carrying daemon-private paths through
// the production path: the token file itself plus its parent directory, the
// way the composing binary passes them in. The token holds a sentinel so a
// read that gets through is observed, not only allowed.
type daemonLiveWorld struct {
	w           specWorld
	c           *Confiner
	tokenFile   string
	tokenDir    string
	siblingFile string
	// nestedFile is a second token file nested inside the writable set.
	// The blanket write deny cannot cover it — the writable allow renders
	// after the blanket deny — so only the daemon-private write deny
	// holds. A regression that drops that deny still refuses every
	// outside-set write through the blanket, and only a probe here turns
	// red at execution.
	nestedFile string
	nestedDir  string
}

func newDaemonLiveWorld(t *testing.T) daemonLiveWorld {
	t.Helper()
	w, c := liveWorld(t)
	dir := filepath.Join(w.base, "daemon-state")
	tokenFile := filepath.Join(dir, "control-token")
	siblingFile := filepath.Join(dir, "sibling-secret")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte("sentinel-control-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(siblingFile, []byte("sibling\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The nested secret stands in for a daemon-private path inside the
	// writable set: denied on write through the daemon-private deny, not
	// the blanket, the way the resolver keeps a denied path inside the
	// set denied instead of refusing the spawn.
	nestedDir := filepath.Join(w.mut, "daemon-state")
	nestedFile := filepath.Join(nestedDir, "control-token")
	if err := os.MkdirAll(nestedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nestedFile, []byte("sentinel-nested-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return daemonLiveWorld{w: w, c: c, tokenFile: tokenFile, tokenDir: dir, siblingFile: siblingFile, nestedFile: nestedFile, nestedDir: nestedDir}
}

func (d daemonLiveWorld) spec() Spec {
	spec := d.w.spec()
	spec.DeniedPaths = []string{d.tokenFile, d.tokenDir, d.nestedFile, d.nestedDir}
	return spec
}

func (d daemonLiveWorld) readSpec() Spec {
	spec := d.spec()
	spec.ReadScope = agent.FileReadWorkarea
	return spec
}

// TestSeatbelt_DaemonTokenDeniedLive: a confined child gets EPERM reading
// the token file and listing its directory, and EPERM creating beside it,
// while a read inside the leaf still works. A second token nested inside
// the writable set pins the daemon-private write deny itself: the blanket
// write deny cannot cover it, so dropping the daemon-private write deny
// lets the nested probes through while every outside-set probe still
// refuses. The unconfined control reads the token fine, and with the
// daemon-private deny rewritten out of the profile the same read
// succeeds — so the test discriminates the deny, not the fixture.
func TestSeatbelt_DaemonTokenDeniedLive(t *testing.T) {
	d := newDaemonLiveWorld(t)

	// Unconfined control: the token reads, so a refusal below means the
	// boundary, not the fixture.
	raw, err := os.ReadFile(d.tokenFile)
	if err != nil || !strings.Contains(string(raw), "sentinel-control-token") {
		t.Fatalf("unconfined control: the token does not read: %v", err)
	}

	plan, err := d.c.prepare(d.readSpec(), "")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer func() { _ = plan.Release() }()

	if code, out := runConfined(t, plan, d.w.mut, nil, "/bin/cat", d.tokenFile); code == 0 || !strings.Contains(out, "Operation not permitted") {
		t.Errorf("confined cat of the control token: exit %d: %q; want EPERM", code, out)
	}
	if code, out := runConfined(t, plan, d.w.mut, nil, "/bin/ls", d.tokenDir); code == 0 || !strings.Contains(out, "Operation not permitted") {
		t.Errorf("confined ls of the token directory: exit %d: %q; want EPERM", code, out)
	}
	if code, out := runConfined(t, plan, d.w.mut, nil, "/bin/cat", d.siblingFile); code == 0 || !strings.Contains(out, "Operation not permitted") {
		t.Errorf("confined cat of the sibling secret: exit %d: %q; want EPERM", code, out)
	}
	results := runProbeSteps(t, plan, d.w,
		probeStep{ID: "token-read", Op: opRead, Path: d.tokenFile},
		probeStep{ID: "token-open", Op: opOpen, Path: d.tokenFile},
		probeStep{ID: "dir-list", Op: opList, Path: d.tokenDir},
		probeStep{ID: "token-write", Op: opWrite, Path: d.tokenFile},
		probeStep{ID: "sibling-create", Op: opCreate, Path: filepath.Join(d.tokenDir, "planted")},
		probeStep{ID: "nested-write", Op: opWrite, Path: d.nestedFile},
		probeStep{ID: "nested-create", Op: opCreate, Path: filepath.Join(d.nestedDir, "planted")},
	)
	for id, result := range results {
		if !refused(result) {
			t.Errorf("confined probe %s: %+v; want a refusal", id, result)
		}
	}
	if exists(filepath.Join(d.tokenDir, "planted")) {
		t.Error("the sibling create landed despite the deny")
	}
	// The nested probes are judged by effect on disk too, so a backend
	// that merely hides the error string still fails: the nested token
	// keeps its sentinel bytes and the planted sibling never lands.
	if raw, err := os.ReadFile(d.nestedFile); err != nil || string(raw) != "sentinel-nested-token\n" {
		t.Errorf("the nested token changed despite the deny: %q, err %v", raw, err)
	}
	if exists(filepath.Join(d.nestedDir, "planted")) {
		t.Error("the nested create landed despite the deny")
	}
	// Legitimate seat work is unaffected: a read inside the leaf works.
	inside := filepath.Join(d.w.mut, "inside.txt")
	if err := os.WriteFile(inside, []byte("inside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out := runConfined(t, plan, d.w.mut, nil, "/bin/cat", inside); code != 0 || !strings.Contains(out, "inside") {
		t.Errorf("confined cat inside the leaf: exit %d: %q; legitimate work must succeed", code, out)
	}

	// Red control: the daemon-private deny holds on its own. Rewriting
	// the blanket read deny out of the profile leaves the token refused
	// through the daemon-private deny — while a sibling decoy with no
	// daemon-private deny reads fine, proving the rewrite worked.
	siblingDecoy := filepath.Join(d.w.base, "sibling-decoy")
	if err := os.WriteFile(siblingDecoy, []byte("decoy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	red := runProbeStepsVia(t, d.w, func(env []string, argv []string) (int, string) {
		return runRewritten(t, plan, d.w.mut, env, "(deny file-read-data file-read-xattr)\n", "", argv...)
	},
		probeStep{ID: "token-read", Op: opRead, Path: d.tokenFile},
		probeStep{ID: "decoy-read", Op: opRead, Path: siblingDecoy},
	)
	if red["decoy-read"].Err != "" {
		t.Fatalf("with the blanket read deny stripped, the plain decoy still refused: %q; the test does not discriminate", red["decoy-read"].Err)
	}
	if !refused(red["token-read"]) {
		t.Errorf("with the blanket read deny stripped, the token read: %+v; the daemon-private deny must hold on its own", red["token-read"])
	}
}
