package daemon

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
)

// The `pool` noun named four different things across this codebase; the
// daemon's warm-workarea control surface moved to the `workarea` noun. Every
// test in this file exercises the alias that keeps a caller pinned to an older
// release working, not the new surface — a rename whose alias silently does not
// work is worse than no rename at all.

// doGet issues a bare GET and returns the response so header assertions are
// possible; the shared requireGet helper discards headers.
func doGet(t *testing.T, addr, path string) *http.Response {
	t.Helper()
	res, err := http.Get("http://" + addr + path) //nolint:gosec // loopback test server
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

// doPost issues a bare POST and returns the response so header assertions are
// possible.
func doPost(t *testing.T, addr, path string, body any) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	res, err := http.Post("http://"+addr+path, "application/json", &buf) //nolint:gosec // loopback test server
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

// requireDeprecationSignal asserts the three-part deprecation signal is present
// and that it names a concrete removal version rather than "a future release".
func requireDeprecationSignal(t *testing.T, res *http.Response, wantReplacement string) {
	t.Helper()
	if got := res.Header.Get("Deprecation"); got != "true" {
		t.Errorf("Deprecation header = %q, want %q", got, "true")
	}
	if got := res.Header.Get("Link"); !strings.Contains(got, wantReplacement) {
		t.Errorf("Link header = %q, want it to name %q", got, wantReplacement)
	}
	warning := res.Header.Get("Warning")
	if !strings.Contains(warning, afclient.WorkareaAliasRemovalVersion) {
		t.Errorf("Warning header = %q, want it to declare removal version %q",
			warning, afclient.WorkareaAliasRemovalVersion)
	}
	if !strings.Contains(warning, wantReplacement) {
		t.Errorf("Warning header = %q, want it to name replacement %q", warning, wantReplacement)
	}
}

// requireNoDeprecationSignal asserts the current surface is NOT announced as
// deprecated. Without this the previous assertion would still pass if the
// signal were attached unconditionally to every response.
func requireNoDeprecationSignal(t *testing.T, res *http.Response) {
	t.Helper()
	if got := res.Header.Get("Deprecation"); got != "" {
		t.Errorf("Deprecation header on the current surface = %q, want empty", got)
	}
}

// TestDeprecatedPoolStatsPathStillServes proves GET /api/daemon/pool/stats
// still returns the workarea-cache snapshot after the rename.
func TestDeprecatedPoolStatsPathStillServes(t *testing.T) {
	_, srv, cleanup := mustStartDaemon(t)
	defer cleanup()

	res := doGet(t, srv.Addr(), "/api/daemon/pool/stats")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/daemon/pool/stats -> %d, want 200", res.StatusCode)
	}
	var resp afclient.WorkareaPoolStats
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode alias response: %v", err)
	}
	if resp.Members == nil {
		t.Error("alias served an empty body; want the same shape the current path serves")
	}
	requireDeprecationSignal(t, res, "/api/daemon/workarea/stats")
}

// TestWorkareaStatsPathIsNotDeprecated pins the other half: the current path
// serves the same shape and is not announced as deprecated.
func TestWorkareaStatsPathIsNotDeprecated(t *testing.T) {
	_, srv, cleanup := mustStartDaemon(t)
	defer cleanup()

	res := doGet(t, srv.Addr(), "/api/daemon/workarea/stats")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/daemon/workarea/stats -> %d, want 200", res.StatusCode)
	}
	requireNoDeprecationSignal(t, res)
}

// TestDeprecatedPoolEvictPathStillServes proves POST /api/daemon/pool/evict
// still reaches the eviction handler. With no EvictHandler wired the daemon
// answers 501 — which is the same answer the current path gives, and is
// distinguishable from the 404 an unregistered alias would produce.
func TestDeprecatedPoolEvictPathStillServes(t *testing.T) {
	_, srv, cleanup := mustStartDaemon(t)
	defer cleanup()

	body := afclient.EvictPoolRequest{RepoURL: "github.com/foo/bar", OlderThanSeconds: 60}

	alias := doPost(t, srv.Addr(), "/api/daemon/pool/evict", body)
	current := doPost(t, srv.Addr(), "/api/daemon/workarea/evict", body)

	if alias.StatusCode == http.StatusNotFound {
		t.Fatal("POST /api/daemon/pool/evict -> 404; the alias is not registered")
	}
	if alias.StatusCode != current.StatusCode {
		t.Errorf("alias status = %d, current surface status = %d; want identical",
			alias.StatusCode, current.StatusCode)
	}
	requireDeprecationSignal(t, alias, "/api/daemon/workarea/evict")
	requireNoDeprecationSignal(t, current)
}

// TestDeprecatedPoolStatsQueryParamStillServes proves ?pool=true still selects
// the workarea section on GET /api/daemon/stats.
//
// This alias matters more than the path aliases: an unrecognised query
// parameter is ignored, not rejected, so dropping it would make an older CLI
// silently render no workarea section instead of failing.
func TestDeprecatedPoolStatsQueryParamStillServes(t *testing.T) {
	_, srv, cleanup := mustStartDaemon(t)
	defer cleanup()

	decode := func(res *http.Response) afclient.DaemonStatsResponse {
		t.Helper()
		var out afclient.DaemonStatsResponse
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			t.Fatalf("decode stats: %v", err)
		}
		return out
	}

	aliasRes := doGet(t, srv.Addr(), "/api/daemon/stats?pool=true")
	alias := decode(aliasRes)
	if alias.Pool == nil {
		t.Error("?pool=true produced no workarea section; the query-parameter alias is dead")
	}
	requireDeprecationSignal(t, aliasRes, "workarea=true")

	currentRes := doGet(t, srv.Addr(), "/api/daemon/stats?workarea=true")
	current := decode(currentRes)
	if current.Pool == nil {
		t.Error("?workarea=true produced no workarea section")
	}
	requireNoDeprecationSignal(t, currentRes)

	offRes := doGet(t, srv.Addr(), "/api/daemon/stats")
	if off := decode(offRes); off.Pool != nil {
		t.Error("stats without either parameter carried a workarea section; the flag is not selecting anything")
	}
}

// TestStatsResponseCarriesBothWorkareaAndPoolKeys proves the workarea-cache
// section is emitted under the current `workarea` key *and* the deprecated
// `pool` alias, whichever query spelling selected it.
//
// This is the response-field half of the same hazard the query-parameter alias
// covers, one level down. Both keys are `omitempty`, so a client that decodes
// only the spelling the daemon did not emit sees a nil section and renders
// nothing — no error, no status code, nothing to notice. Emitting only `pool`
// leaves the alias table scheduling the sole field that exists for deletion;
// emitting only `workarea` breaks every client pinned to a release before the
// rename.
func TestStatsResponseCarriesBothWorkareaAndPoolKeys(t *testing.T) {
	_, srv, cleanup := mustStartDaemon(t)
	defer cleanup()

	// The two shapes a pinned client can hold. Neither models the other key,
	// so each one decoding a section proves that key is really on the wire.
	type currentClient struct {
		Workarea *afclient.WorkareaPoolStats `json:"workarea"`
	}
	type preRenameClient struct {
		Pool *afclient.WorkareaPoolStats `json:"pool"`
	}

	cases := []struct {
		name         string
		query        string
		wantWorkarea bool
	}{
		{name: "current query parameter", query: "?workarea=true", wantWorkarea: true},
		{name: "deprecated query parameter", query: "?pool=true", wantWorkarea: true},
		{name: "section not selected", query: "", wantWorkarea: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := doGet(t, srv.Addr(), "/api/daemon/stats"+tc.query)
			body, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatalf("read stats body: %v", err)
			}

			var current currentClient
			if err := json.Unmarshal(body, &current); err != nil {
				t.Fatalf("decode as a post-rename client: %v", err)
			}
			var legacy preRenameClient
			if err := json.Unmarshal(body, &legacy); err != nil {
				t.Fatalf("decode as a pre-rename client: %v", err)
			}

			if !tc.wantWorkarea {
				if current.Workarea != nil || legacy.Pool != nil {
					t.Errorf("stats carried a workarea section with query %q; the parameter is not selecting anything", tc.query)
				}
				return
			}

			if current.Workarea == nil {
				t.Errorf("no `workarea` key on the wire for %q; the current spelling in 011's alias table does not exist", tc.query)
			}
			if legacy.Pool == nil {
				t.Errorf("no `pool` key on the wire for %q; a client pinned before %s renders no workarea section",
					tc.query, afclient.WorkareaAliasRemovalVersion)
			}
			if current.Workarea == nil || legacy.Pool == nil {
				return
			}
			// The pair must not be allowed to drift: a reader picking either
			// spelling has to get the same snapshot.
			gotCurrent, err := json.Marshal(current.Workarea)
			if err != nil {
				t.Fatalf("re-marshal workarea: %v", err)
			}
			gotLegacy, err := json.Marshal(legacy.Pool)
			if err != nil {
				t.Fatalf("re-marshal pool: %v", err)
			}
			if !bytes.Equal(gotCurrent, gotLegacy) {
				t.Errorf("the two spellings disagree:\n workarea = %s\n     pool = %s", gotCurrent, gotLegacy)
			}
		})
	}
}

// TestDeprecatedCapacityKeyStillAccepted proves the daemon still applies
// capacity.poolMaxDiskGb, and that it lands on the same field the current key
// writes.
func TestDeprecatedCapacityKeyStillAccepted(t *testing.T) {
	d, srv, cleanup := mustStartDaemon(t)
	defer cleanup()

	res := doPost(t, srv.Addr(), "/api/daemon/capacity", map[string]string{
		"key":   afclient.LegacyWorkareaMaxDiskGbKey,
		"value": "137",
	})
	var resp afclient.SetCapacityResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode set-capacity response: %v", err)
	}
	if !resp.OK {
		t.Fatalf("daemon rejected the deprecated key: %s", resp.Message)
	}
	requireDeprecationSignal(t, res, afclient.WorkareaMaxDiskGbKey)

	d.mu.Lock()
	got := d.config.Capacity.PoolMaxDiskGb
	d.mu.Unlock()
	if got != 137 {
		t.Errorf("capacity applied via the deprecated key = %d, want 137", got)
	}
}

// TestLegacyPoolMaxDiskGbYAMLKeyStillRead proves a daemon.yaml written before
// the rename still yields its disk envelope.
//
// The decoder is non-strict, so without the alias this key would be dropped in
// silence and the field would default to 0 — which the daemon reads as "no
// limit", turning LRU eviction off and filling the disk.
func TestLegacyPoolMaxDiskGbYAMLKeyStillRead(t *testing.T) {
	t.Parallel()

	const legacyYAML = `apiVersion: donmai/v1
kind: DaemonConfig
machine:
  id: legacy-machine
orchestrator:
  url: file:///tmp/queue
capacity:
  maxConcurrentSessions: 4
  poolMaxDiskGb: 100
`
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	if err := os.WriteFile(path, []byte(legacyYAML), 0o600); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg == nil {
		t.Fatal("LoadConfig returned nil config")
	}
	if cfg.Capacity.PoolMaxDiskGb != 100 {
		t.Errorf("legacy poolMaxDiskGb = %d, want 100 (0 would silently disable LRU eviction)",
			cfg.Capacity.PoolMaxDiskGb)
	}
	if cfg.Capacity.MaxConcurrentSessions != 4 {
		t.Errorf("maxConcurrentSessions = %d, want 4; the sibling keys must survive the alias path",
			cfg.Capacity.MaxConcurrentSessions)
	}
}

// TestCurrentWorkareaMaxDiskGbYAMLKeyWins proves the current key is read, and
// that it takes precedence when a half-migrated file carries both.
func TestCurrentWorkareaMaxDiskGbYAMLKeyWins(t *testing.T) {
	t.Parallel()

	const bothYAML = `apiVersion: donmai/v1
kind: DaemonConfig
machine:
  id: both-machine
orchestrator:
  url: file:///tmp/queue
capacity:
  maxConcurrentSessions: 4
  poolMaxDiskGb: 100
  workareaMaxDiskGb: 250
`
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	if err := os.WriteFile(path, []byte(bothYAML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Capacity.PoolMaxDiskGb != 250 {
		t.Errorf("workareaMaxDiskGb = %d, want 250 (the current key must win over the alias)",
			cfg.Capacity.PoolMaxDiskGb)
	}
}
