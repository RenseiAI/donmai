package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/internal/localqueue"
	"github.com/RenseiAI/donmai/internal/localruntimeauth"
	"github.com/RenseiAI/donmai/result"
)

func (f *localHTTPFixture) restart(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	options := f.d.opts
	host, portRaw, err := net.SplitHostPort(f.server.Addr())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portRaw)
	if err != nil {
		t.Fatal(err)
	}
	options.HTTPHost = host
	options.HTTPPort = port
	if err = f.d.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err = f.server.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	next := New(options)
	server := NewServer(next)
	if _, err = server.StartBeforeDaemon(); err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(context.Background())
	t.Cleanup(func() {
		stop()
		ctx, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if err := next.Stop(ctx); err != nil {
			t.Error(err)
		}
		if err := server.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	if err = next.Start(runCtx); err != nil {
		t.Fatal(err)
	}
	server.DaemonStarted()
	f.d = next
	f.server = server
	f.runtime = next.localRuntime.Load()
	if "http://"+server.Addr() != f.origin {
		t.Fatal("owned restart changed the immutable callback origin")
	}
}

func TestLocalRecoveryHoldsAmbiguousWorkAndPreservesTerminalReplay(t *testing.T) {
	t.Parallel()
	var launches atomic.Int32
	f := startLocalHTTPFixture(t, func(d *Daemon) {
		d.opts.SpawnerOptions.OnPreSpawn = func(_ SessionSpec, env []string) ([]string, error) { launches.Add(1); return env, nil }
	})
	ctx := context.Background()
	authority := f.runtime.identity
	claimed := f.admit(t, 11)
	claimedAttempt, err := f.runtime.store.Claim(ctx, f.runtime.store.CurrentRevision(), claimed.Session)
	if err != nil {
		t.Fatal(err)
	}
	claimedProjection, err := f.runtime.store.Session(ctx, authority.ScopeID, claimed.Session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	unknown := f.admit(t, 12)
	unknownToken := f.claim(t, unknown.Session)
	unknownProjection, err := f.runtime.store.Session(ctx, authority.ScopeID, unknown.Session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	terminal := f.admit(t, 13)
	terminalToken := f.claim(t, terminal.Session)
	body := []byte(fmt.Sprintf(`{"workerId":%q,"status":"failed","summary":"restart fixture terminal"}`, authority.WorkerID))
	code, raw := f.request(t, http.MethodPost, basePath(terminal.Session.SessionID, "status"), terminalToken, body)
	if code != http.StatusOK {
		t.Fatalf("terminal %d: %s", code, raw)
	}
	pending := f.admit(t, 14)
	revision := f.runtime.store.CurrentRevision()
	f.restart(t)
	if f.runtime.identity != authority || f.runtime.store.CurrentRevision() != revision {
		t.Fatal("restart rewrote authority or journal")
	}
	held := f.d.ActiveSessions()
	if len(held) != 2 {
		t.Fatalf("recovered unresolved occupancy=%d, want2", len(held))
	}
	for _, session := range held {
		if session.State != SessionUnknown || session.PID != 0 || session.Repository == "" || session.ProjectName == "" {
			t.Fatalf("ambiguous ownership was misrepresented: %+v", session)
		}
	}
	if launches.Load() != 0 {
		t.Fatal("restart silently respawned ambiguous work")
	}
	dispatch, err := f.runtime.store.PendingDispatch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dispatch) != 1 || dispatch[0].Envelope.Session.SessionID != pending.Session.SessionID {
		t.Fatal("restart requeued an already claimed attempt")
	}
	if _, err = f.runtime.auth.LoadAttempt(claimedProjection.Admission.Envelope.CredentialHandle, claimedAttempt); !errors.Is(err, localruntimeauth.ErrMissingCredential) {
		t.Fatalf("missing claimed credential was regenerated: %v", err)
	}
	originalKey, err := f.runtime.auth.LoadAttempt(unknownProjection.Admission.Envelope.CredentialHandle, *unknownProjection.Attempt)
	if err != nil {
		t.Fatal(err)
	}
	if originalKey.BearerToken() != unknownToken {
		t.Fatal("restart rotated attempt authority")
	}
	reloaded, err := f.runtime.store.Session(ctx, authority.ScopeID, unknown.Session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Admission.DispatchState != localqueue.DispatchUnknown || !bytes.Equal(reloaded.Admission.Envelope.OperationalPayload, unknownProjection.Admission.Envelope.OperationalPayload) || !bytes.Equal(reloaded.Admission.Envelope.HostPreflight, unknownProjection.Admission.Envelope.HostPreflight) {
		t.Fatal("restart rewrote exact admitted payload or host plan")
	}
	code, raw = f.request(t, http.MethodPost, basePath(terminal.Session.SessionID, "status"), terminalToken, body)
	if code != http.StatusOK {
		t.Fatalf("retained terminal key did not replay: %d %s", code, raw)
	}
	if f.runtime.store.CurrentRevision() != revision {
		t.Fatal("terminal response-loss replay advanced journal after restart")
	}
}

type localLostTerminalResponse struct {
	lost   atomic.Bool
	mu     sync.Mutex
	bodies [][]byte
}

func (l *localLostTerminalResponse) RoundTrip(request *http.Request) (*http.Response, error) {
	terminal := strings.HasSuffix(request.URL.Path, "/status")
	if terminal {
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		_ = request.Body.Close()
		request.Body = io.NopCloser(bytes.NewReader(raw))
		l.mu.Lock()
		l.bodies = append(l.bodies, bytes.Clone(raw))
		l.mu.Unlock()
	}
	response, err := http.DefaultTransport.RoundTrip(request)
	if err == nil && terminal && !l.lost.Swap(true) {
		_ = response.Body.Close()
		return nil, errors.New("owned fixture lost response after server commit")
	}
	return response, err
}

func TestLocalTerminalResponseLossReplaysExactPreparedBody(t *testing.T) {
	t.Parallel()
	f := startLocalHTTPFixture(t)
	session := f.admit(t, 15)
	token := f.claim(t, session.Session)
	loss := &localLostTerminalResponse{}
	poster, err := result.NewPoster(result.Options{PlatformURL: f.origin + localRuntimePrefix, WorkerID: f.runtime.identity.WorkerID, AuthToken: token, HTTPClient: &http.Client{Transport: loss, Timeout: 5 * time.Second}, MaxAttempts: 2, BaseDelay: 0})
	if err != nil {
		t.Fatal(err)
	}
	terminal := agent.Result{Status: "failed", Summary: "response-loss fixture"}
	body, err := poster.PrepareTerminalStatusBody(context.Background(), session.Session.SessionID, terminal, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := f.runtime.store.CurrentRevision()
	posted := poster.PostPreparedOutcome(context.Background(), session.Session.SessionID, terminal, body)
	if posted.CompletionErr != nil || posted.StatusErr != nil {
		t.Fatalf("response loss was not replayed: %v/%v", posted.CompletionErr, posted.StatusErr)
	}
	loss.mu.Lock()
	copies := append([][]byte(nil), loss.bodies...)
	loss.mu.Unlock()
	if len(copies) != 2 || !bytes.Equal(copies[0], body) || !bytes.Equal(copies[1], body) {
		t.Fatal("lost terminal response did not resend exact retained bytes")
	}
	projection, err := f.runtime.store.Session(context.Background(), f.runtime.identity.ScopeID, session.Session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Terminal == nil || projection.Terminal.BodySHA256 != localDigest(body) || f.runtime.store.CurrentRevision() != before+1 {
		t.Fatal("lost acknowledgement caused multiple terminal transactions")
	}
}
