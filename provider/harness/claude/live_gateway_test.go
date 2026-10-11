package claude

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// This file is the gated live-CLI smoke (the SMOKE-GAP half): the gate's
// live matrix — a temp config home plus two loopback listeners — as a test
// the real harness CLI can run against. The fake binary stands in for the
// CLI's own network posture: it carries the session bearer to the gateway
// surface exactly as the projected child env directs, while the trust seed
// and the routing pin land beside it. A test that only asserted the env
// would prove the projection; this one additionally proves the bearer
// routes — the gateway listener observes it, the decoy listener never does.
//
// Gated, not skipped: every assertion runs on every platform the PTY lane
// supports (unix-only, like every other interactive Spawn test here), with
// no external network — both listeners are loopback, and the temp config
// home isolates the operator's real CLI config (see TestMain).

// TestSpawn_Interactive_LiveGatewayMatrix proves the full unattended
// posture end to end under a temp config home: the trust record seeds the
// session boundary, the routing pin restates the gateway destination on
// the winning layer, the bearer rides the unattended token pair (never
// argv, never the modal-generating name), and the bearer actually routes
// — the gateway listener observes it while a second loopback listener
// never does. RED proof: revert projectGatewayCredential to merge the
// binding env verbatim and the bearer rides the wrong name; drop the seed
// call and the trust record stays absent.
func TestSpawn_Interactive_LiveGatewayMatrix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	// Gateway surface: records the bearer it is offered.
	var gotAuth, gotPath atomic.Value
	gateway := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		gotPath.Store(r.URL.Path)
		w.Write([]byte(`{}`))
	})}
	gatewayLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen gateway: %v", err)
	}
	go func() { _ = gateway.Serve(gatewayLn) }()
	t.Cleanup(func() { _ = gateway.Close() })
	// Decoy surface: must never observe the bearer.
	var decoyHit atomic.Bool
	decoy := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decoyHit.Store(true)
		w.Write([]byte(`{}`))
	})}
	decoyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen decoy: %v", err)
	}
	go func() { _ = decoy.Serve(decoyLn) }()
	t.Cleanup(func() { _ = decoy.Close() })

	cfgHome := t.TempDir()
	t.Setenv(claudeConfigDirEnv, cfgHome)
	workdir := t.TempDir()
	gate := filepath.Join(t.TempDir(), "gate")
	gatewayURL := fmt.Sprintf("http://%s/v1", gatewayLn.Addr().String())

	p := newFakeInteractiveProvider(t, "while [ ! -e "+shellQuote(gate)+` ]; do sleep 0.05; done
printf '%s\n' "$@" > "$PWD/argv.txt"
{
  printf 'ANTHROPIC_BASE_URL=%s\n' "$ANTHROPIC_BASE_URL"
  printf 'ANTHROPIC_AUTH_TOKEN=[%s]\n' "$ANTHROPIC_AUTH_TOKEN"
  printf 'ANTHROPIC_API_KEY=[%s]\n' "$ANTHROPIC_API_KEY"
} > "$PWD/env.txt"
curl -s -o /dev/null -H "Authorization: Bearer $ANTHROPIC_AUTH_TOKEN" "$ANTHROPIC_BASE_URL/models"
`)
	const bearer = "live-matrix-bearer"
	h, err := p.Spawn(context.Background(), agent.Spec{
		Cwd:         workdir,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
		Endpoint: &agent.EndpointBinding{
			Company: agent.CompanyAnthropic,
			Model:   "served-model",
			Host:    agent.HostGateway,
			BaseURL: gatewayURL,
			Env:     map[string]string{EnvAuthToken: bearer},
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })

	// The seed lands while the child waits on the gate: assert it mid-run,
	// before the session-end cleanup removes it.
	if !trustSeededFor(t, cfgHome, workdir) {
		t.Fatal("session boundary was not trusted during the run; the live matrix has nothing to route")
	}
	// The ledger recorded the same entries the record carries, with an
	// owning session, so a crash here would still sweep them.
	seedPath := filepath.Join(cfgHome, claudeTrustFileName)
	if entries, ok := readTrustLedger(seedPath); !ok || len(entries) == 0 {
		t.Errorf("live seed recorded no ledger rows; a crash mid-run would leave trust behind")
	}
	if err := os.WriteFile(gate, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	awaitPTYExit(t, h)

	env := readCapturedFile(t, workdir, "env.txt")
	if !strings.Contains(env, "ANTHROPIC_BASE_URL="+gatewayURL) {
		t.Errorf("child env missing the gateway base URL; got:\n%s", env)
	}
	if !strings.Contains(env, "ANTHROPIC_AUTH_TOKEN=["+bearer+"]") {
		t.Errorf("child env missing the bearer on the unattended token pair; got:\n%s", env)
	}
	for _, line := range strings.Split(env, "\n") {
		if value, ok := strings.CutPrefix(line, "ANTHROPIC_API_KEY="); ok && strings.Trim(value, "[]") != "" {
			t.Errorf("modal-generating key name rides the child env with a value: %q", line)
		}
		if strings.Contains(line, bearer) && !strings.HasPrefix(line, "ANTHROPIC_AUTH_TOKEN=") {
			t.Errorf("bearer rides the child env outside the token pair: %q", line)
		}
	}
	argv := argvLines(readCapturedFile(t, workdir, "argv.txt"))
	for _, arg := range argv {
		if strings.Contains(arg, bearer) {
			t.Errorf("bearer rides argv (visible to every same-user process): %q", arg)
		}
	}
	// The bearer routed: the gateway observed it, the decoy never did.
	if auth, _ := gotAuth.Load().(string); auth != "Bearer "+bearer {
		t.Errorf("gateway observed Authorization %q, want the session bearer", auth)
	}
	if path, _ := gotPath.Load().(string); path != "/v1/models" {
		t.Errorf("gateway observed path %q, want the routed surface", path)
	}
	if decoyHit.Load() {
		t.Error("decoy loopback listener observed a request; the bearer routes only to the gateway")
	}
	// Session end removed the seed the run added: the boundary is gone
	// while the ledger drained with it.
	projects := readTrustProjects(t, seedPath)
	clean := filepath.Clean(workdir)
	if _, present := projects[clean]; present {
		t.Errorf("session boundary %q survives session end: %v", clean, projects)
	}
	if resolved, err := filepath.EvalSymlinks(clean); err == nil && resolved != clean {
		if _, present := projects[resolved]; present {
			t.Errorf("resolved session boundary %q survives session end: %v", resolved, projects)
		}
	}
	if entries, ok := readTrustLedger(seedPath); ok && len(entries) != 0 {
		t.Errorf("live cleanup left ledger rows behind: %v", entries)
	}
}
