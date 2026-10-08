package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

// urlRedactionSentinels are canary credential fragments. No control route
// response body may contain any of them; the host and path parts must
// survive so the redacted value still names its repository.
const (
	urlRedactUser     = "redact-user"
	urlRedactPassword = "redact-secret"
)

func urlRedactSentinelRaw() string {
	return "https://" + urlRedactUser + ":" + urlRedactPassword + "@git.example.com/org/repo.git"
}

func assertNoURLCredential(t *testing.T, where, body string) {
	t.Helper()
	for _, sentinel := range []string{urlRedactUser, urlRedactPassword, urlRedactUser + ":" + urlRedactPassword} {
		if strings.Contains(body, sentinel) {
			t.Errorf("%s serves an embedded credential %q in: %s", where, sentinel, body)
		}
	}
	if !strings.Contains(body, "git.example.com/org/repo.git") {
		t.Errorf("%s redacted the host or path along with the credential: %s", where, body)
	}
}

// TestRedactRepositoryURL pins the unit contract: userinfo is dropped from
// http(s) URLs, including ones the URL parser rejects (fail closed), while
// slugs, scp-like remotes, and input without an authority credential pass
// through unchanged.
//
// RED: return raw unchanged and the credential cases fail.
func TestRedactRepositoryURL(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{name: "https userinfo stripped", in: urlRedactSentinelRaw(), want: "https://git.example.com/org/repo.git"},
		{name: "username only stripped", in: "https://redact-user@git.example.com/org/repo.git", want: "https://git.example.com/org/repo.git"},
		{name: "clean https untouched", in: "https://git.example.com/org/repo.git", want: "https://git.example.com/org/repo.git"},
		{name: "port preserved", in: "https://redact-user:redact-secret@git.example.com:8443/org/repo.git", want: "https://git.example.com:8443/org/repo.git"},
		{name: "slug untouched", in: "github.com/org/repo", want: "github.com/org/repo"},
		{name: "slug with at untouched", in: "github.com/org/repo@main", want: "github.com/org/repo@main"},
		{name: "scp-like untouched", in: "git@git.example.com:org/repo.git", want: "git@git.example.com:org/repo.git"},
		{name: "empty untouched", in: "", want: ""},
		{name: "unparseable password stripped", in: "https://redact-user:redact^secret@git.example.com/org/repo.git", want: "https://git.example.com/org/repo.git"},
		{name: "password with space stripped", in: "https://redact-user:redact secret@git.example.com/org/repo.git", want: "https://git.example.com/org/repo.git"},
		{name: "invalid escape stripped", in: "https://redact-user:redact%zzsecret@git.example.com/org/repo.git", want: "https://git.example.com/org/repo.git"},
		{name: "unescaped at in unparseable password stripped", in: "https://redact-user:re@dact^secret@git.example.com/org/repo.git", want: "https://git.example.com/org/repo.git"},
		{name: "unparseable without userinfo untouched", in: "https://git.example.com/org/repo^x@main", want: "https://git.example.com/org/repo^x@main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := redactRepositoryURL(tc.in); got != tc.want {
				t.Errorf("redactRepositoryURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	if got := redactRepositoryURLs(nil); got != nil {
		t.Errorf("redactRepositoryURLs(nil) = %v, want nil", got)
	}
}

// rawControlBody issues a GET against the production control route and
// returns the raw body, failing unless the status is 200. Raw bytes —
// not the decoded shape — are what the redaction tests assert on: a
// credential hiding in any field of the response must fail the test.
func rawControlBody(t *testing.T, addr, path string) string {
	t.Helper()
	res, err := http.Get("http://" + addr + path) //nolint:gosec // test-local daemon
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d (%v): %s", path, res.StatusCode, err, raw)
	}
	return string(raw)
}

// TestControlRoutes_RedactRepositoryCredentials drives the production
// control routes with an operator configuration carrying a credentialed
// repository URL and asserts the sentinel credential reaches no response
// body: /api/daemon/stats (allowlist), /api/daemon/pool/stats (pool
// members, via the stats route's embedded pool too), the session list
// (display-only projection of the inbound spec), the heartbeat allowlist
// (last change beat's project entries), and the credential-free session
// detail (top-level repository plus the forwarded declaration sources).
//
// RED: serve any of these URLs unredacted and the raw-body assertion
// fails with the sentinel quoted in the failure output. The daemon runs
// the control gate enforced, so the detail assertion pins the
// credential-free projection exactly as the blocking probe drove it:
// no bearer, redacted repository, still 200.
func TestControlRoutes_RedactRepositoryCredentials(t *testing.T) {
	credentialed := urlRedactSentinelRaw()
	d, srv, cleanup := mustStartDaemonWith(t, func(o *Options) {
		o.PoolStatsProvider = stubPoolStatsProvider{members: []afclient.WorkareaPoolMember{
			{ID: "pool-1", Repository: credentialed, Ref: "main", Status: afclient.PoolMemberReady},
		}}
		o.RequireControlToken = true
		o.ControlToken = testControlToken
	})
	defer cleanup()
	d.mu.Lock()
	// The second entry is a remote the URL parser rejects (a '^' in the
	// password): its credential must not pass through either.
	d.config.Projects = []ProjectConfig{
		{ID: "demo", Repository: credentialed},
		{ID: "demo-unparsed", Repository: "https://" + urlRedactUser + ":" + urlRedactPassword + "^x@git.example.com/org/repo.git"},
	}
	d.config.Orchestrator.URL = credentialed
	d.mu.Unlock()
	// The spawner allowlist is snapshotted at construction; refresh it so
	// the seeded session below is admitted against the credentialed URL.
	d.spawner.SetProjects([]ProjectConfig{{ID: "demo", Repository: credentialed}})

	if _, err := d.AcceptWorkWithDetail(SessionSpec{
		SessionID: "sess-cred-url", Repository: credentialed, Ref: "main",
	}, &SessionDetail{
		SessionID:  "sess-cred-url",
		Repository: credentialed,
		RepositoryDeclaration: &workarea.RepositoryDeclarationV1{
			Protocol: workarea.ProtocolSessionRootV1,
			Repositories: []workarea.DeclaredRepositoryV1{{
				Source: workarea.RepositorySource{Repository: credentialed, Ref: "main"},
				Role:   workarea.RepositoryRolePrimary,
			}},
		},
	}); err != nil {
		t.Fatalf("AcceptWorkWithDetail: %v", err)
	}
	// Publish one heartbeat beat carrying the credentialed project
	// allowlist, so GET /api/daemon/heartbeat has a populated payload
	// to serve. The stub registration token selects the local stub beat
	// path, which records the payload without a network call.
	if d.heartbeat == nil {
		t.Fatal("daemon has no heartbeat service")
	}
	// Freeze the background beat first: the loop's immediate beat is an
	// async goroutine, and any loop beat composed after the spawner
	// refresh above transmits the same allowlist hash and therefore
	// OMITS the allowlist from its payload (change-only reporting).
	// Whichever beat runs last owns LastPayload, so a loop beat landing
	// after the manual one leaves GET /api/daemon/heartbeat with no
	// allowlist entries at all — the host and path vanish along with the
	// credential. Holding the beat lock across the baseline reset and the
	// manual beat below makes this beat deterministically last and
	// deterministically complete: no interleaving beat can slip between
	// the reset and the compose.
	d.heartbeat.Stop()
	d.heartbeat.sendMu.Lock()
	func() {
		defer d.heartbeat.sendMu.Unlock()
		d.heartbeat.mu.Lock()
		d.heartbeat.lastAllowlistHash = ""
		d.heartbeat.mu.Unlock()
		if err := d.heartbeat.sendOneSerialized(context.Background()); err != nil {
			t.Fatalf("heartbeat beat: %v", err)
		}
	}()

	for _, path := range []string{
		"/api/daemon/stats",
		"/api/daemon/stats?pool=true",
		"/api/daemon/pool/stats",
		"/api/daemon/sessions",
		"/api/daemon/heartbeat",
		"/api/daemon/doctor",
		"/api/daemon/sessions/sess-cred-url",
	} {
		assertNoURLCredential(t, "GET "+path, rawControlBody(t, srv.Addr(), path))
	}

	// The heartbeat's stored payload is the upstream POST body too:
	// serving must redact a copy, never the stored entries.
	for _, entry := range d.heartbeat.LastPayload().Allowlist {
		if !strings.Contains(entry.Repository, urlRedactPassword) {
			t.Errorf("heartbeat stored allowlist rewritten to %q: serving must redact a copy", entry.Repository)
		}
	}

	// The credentialed reads keep the functional URL: redaction is a
	// serving projection for credential-free reads, and the worker
	// clones from the credentialed read.
	stored, ok := d.SessionDetail("sess-cred-url")
	if !ok {
		t.Fatal("stored detail missing")
	}
	if stored.Repository != credentialed {
		t.Errorf("stored detail Repository = %q, want the functional URL %q", stored.Repository, credentialed)
	}
	readTok, ok := d.sessionReadToken("sess-cred-url")
	if !ok || readTok == "" {
		t.Fatal("no read credential minted for sess-cred-url")
	}
	for _, bearer := range []string{testControlToken, readTok} {
		req, err := http.NewRequest(http.MethodGet, "http://"+srv.Addr()+"/api/daemon/sessions/sess-cred-url", nil) //nolint:gosec,noctx
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("credentialed GET: %v", err)
		}
		raw, err := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if err != nil || res.StatusCode != http.StatusOK {
			t.Fatalf("credentialed GET = %d (%v): %s", res.StatusCode, err, raw)
		}
		if !strings.Contains(string(raw), credentialed) {
			t.Errorf("credentialed read lost the functional URL: %s", raw)
		}
	}
}

// stubPoolStatsProvider serves a fixed pool snapshot for redaction tests.
// The provider below also records the snapshot pointer it hands out so
// the copy test can prove serving never rewrites provider-owned state.
type stubPoolStatsProvider struct {
	members []afclient.WorkareaPoolMember
}

func (s stubPoolStatsProvider) Stats(_ context.Context) (*afclient.WorkareaPoolStats, error) {
	return &afclient.WorkareaPoolStats{
		Members:   append([]afclient.WorkareaPoolMember(nil), s.members...),
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}, nil
}

// TestPoolStats_RedactKeepsProviderSnapshotProves proves the serving
// boundary redacts a copy: the provider-owned snapshot still carries
// the functional URL after the route is served. A future caller that
// regresses to in-place redaction poisons the provider's live state
// (and races concurrent readers on the same backing array); this test
// goes RED on that revert.
func TestPoolStats_RedactKeepsProviderSnapshot(t *testing.T) {
	credentialed := urlRedactSentinelRaw()
	provider := &recordingPoolStatsProvider{members: []afclient.WorkareaPoolMember{
		{ID: "pool-1", Repository: credentialed, Ref: "main", Status: afclient.PoolMemberReady},
	}}
	_, srv, cleanup := mustStartDaemonWith(t, func(o *Options) {
		o.PoolStatsProvider = provider
	})
	defer cleanup()

	assertNoURLCredential(t, "GET /api/daemon/pool/stats",
		rawControlBody(t, srv.Addr(), "/api/daemon/pool/stats"))

	handed := provider.last()
	if handed == nil {
		t.Fatal("provider handed out no snapshot")
	}
	for _, m := range handed.Members {
		if m.Repository != credentialed {
			t.Errorf("provider snapshot rewritten to %q: serving must redact a copy", m.Repository)
		}
	}
}

// recordingPoolStatsProvider wraps the fixed snapshot and remembers the
// exact pointer it handed out, so the copy test can inspect
// provider-owned state after the route is served.
type recordingPoolStatsProvider struct {
	mu      sync.Mutex
	members []afclient.WorkareaPoolMember
	handed  *afclient.WorkareaPoolStats
}

func (s *recordingPoolStatsProvider) Stats(_ context.Context) (*afclient.WorkareaPoolStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handed = &afclient.WorkareaPoolStats{
		Members:   append([]afclient.WorkareaPoolMember(nil), s.members...),
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	return s.handed, nil
}

func (s *recordingPoolStatsProvider) last() *afclient.WorkareaPoolStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handed
}

// TestWorkareaRoutes_RedactRepositoryCredentials seeds an archived workarea
// whose manifest carries a credentialed repository URL and drives the
// production list and inspect routes: neither response body may contain
// the sentinel credential.
//
// RED: serve the manifest URL unredacted and the raw-body assertion fails
// with the sentinel quoted in the failure output.
func TestWorkareaRoutes_RedactRepositoryCredentials(t *testing.T) {
	root := t.TempDir()
	writeFixtureArchive(t, root, fixtureArchive{
		id:       "wa-cred-url",
		manifest: archiveManifest{SessionID: "sess-cred", Repository: urlRedactSentinelRaw()},
	})
	// The inspect response echoes every manifest key: a credentialed URL
	// under a key other than "repository", or nested, must be redacted too.
	manifestPath := filepath.Join(root, "wa-cred-url", "manifest.json")
	rawManifest, err := os.ReadFile(manifestPath) //nolint:gosec // test-local fixture path
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(rawManifest, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest["cloneUrl"] = urlRedactSentinelRaw()
	manifest["sources"] = []any{map[string]any{"url": urlRedactSentinelRaw()}}
	if rawManifest, err = json.Marshal(manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, rawManifest, 0o600); err != nil {
		t.Fatal(err)
	}
	hsrv := newServerForWorkareaTest(t, root, 0)

	assertNoURLCredential(t, "GET /api/daemon/workareas",
		getControlBody(t, hsrv.URL+"/api/daemon/workareas"))
	assertNoURLCredential(t, "GET /api/daemon/workareas/wa-cred-url",
		getControlBody(t, hsrv.URL+"/api/daemon/workareas/wa-cred-url"))
}

func getControlBody(t *testing.T, url string) string {
	t.Helper()
	res, err := http.Get(url) //nolint:gosec // test-local server
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d (%v): %s", url, res.StatusCode, err, raw)
	}
	return string(raw)
}
