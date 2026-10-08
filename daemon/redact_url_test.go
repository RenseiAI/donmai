package daemon

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afclient"
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
// http(s) URLs, while slugs, scp-like remotes, and unparseable input pass
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
// members, via the stats route's embedded pool too), and the session
// list (display-only projection of the inbound spec).
//
// RED: serve any of these URLs unredacted and the raw-body assertion
// fails with the sentinel quoted in the failure output.
func TestControlRoutes_RedactRepositoryCredentials(t *testing.T) {
	credentialed := urlRedactSentinelRaw()
	d, srv, cleanup := mustStartDaemonWith(t, func(o *Options) {
		o.PoolStatsProvider = stubPoolStatsProvider{members: []afclient.WorkareaPoolMember{
			{ID: "pool-1", Repository: credentialed, Ref: "main", Status: afclient.PoolMemberReady},
		}}
	})
	defer cleanup()
	d.mu.Lock()
	d.config.Projects = []ProjectConfig{{ID: "demo", Repository: credentialed}}
	d.mu.Unlock()
	// The spawner allowlist is snapshotted at construction; refresh it so
	// the seeded session below is admitted against the credentialed URL.
	d.spawner.SetProjects([]ProjectConfig{{ID: "demo", Repository: credentialed}})

	if _, err := d.AcceptWorkWithDetail(SessionSpec{
		SessionID: "sess-cred-url", Repository: credentialed, Ref: "main",
	}, &SessionDetail{SessionID: "sess-cred-url", Repository: credentialed}); err != nil {
		t.Fatalf("AcceptWorkWithDetail: %v", err)
	}

	for _, path := range []string{
		"/api/daemon/stats",
		"/api/daemon/stats?pool=true",
		"/api/daemon/pool/stats",
		"/api/daemon/sessions",
	} {
		assertNoURLCredential(t, "GET "+path, rawControlBody(t, srv.Addr(), path))
	}

	// The stored detail keeps the functional URL: redaction is a serving
	// projection, and the worker clones from the credentialed read.
	stored, ok := d.SessionDetail("sess-cred-url")
	if !ok {
		t.Fatal("stored detail missing")
	}
	if stored.Repository != credentialed {
		t.Errorf("stored detail Repository = %q, want the functional URL %q", stored.Repository, credentialed)
	}
}

// stubPoolStatsProvider serves a fixed pool snapshot for redaction tests.
type stubPoolStatsProvider struct {
	members []afclient.WorkareaPoolMember
}

func (s stubPoolStatsProvider) Stats(_ context.Context) (*afclient.WorkareaPoolStats, error) {
	return &afclient.WorkareaPoolStats{
		Members:   append([]afclient.WorkareaPoolMember(nil), s.members...),
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}, nil
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
