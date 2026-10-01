package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/internal/localqueue"
	"github.com/RenseiAI/donmai/provider/harness/codex"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runner"
	"github.com/RenseiAI/donmai/runtime/activity"
	"github.com/RenseiAI/donmai/runtime/heartbeat"
	"github.com/RenseiAI/donmai/runtime/stepheartbeat"
)

type localHTTPTestCompiler struct {
	producer *runner.LocalAdmissionProducer
	identity LocalRuntimeIdentity
	version  string
}

func (c *localHTTPTestCompiler) Selectors() *executioncell.ExecutionSelectorRegistry {
	return &executioncell.ExecutionSelectorRegistry{HarnessVersions: map[string][]string{"codex": {c.version}}, Models: []string{"openai/gpt-5-codex"}, Endpoints: []string{"local-openai"}, AuthBindings: []string{"local-login"}, Placements: []string{c.identity.HostID}, Capabilities: []string{}}
}

func (c *localHTTPTestCompiler) Compile(ctx context.Context, r LocalCompileRequest) (LocalAdmissionEvidence, error) {
	stamp, err := NewLocalExecutionSecurityStamp(c.identity.ScopeID, InitialLocalExecutionSecurity(), time.Now())
	if err != nil {
		return LocalAdmissionEvidence{}, err
	}
	request := runner.LocalAdmissionRequest{SessionID: r.SessionID, ReceiptID: r.ReceiptID, PreflightChallengeID: r.ChallengeID}
	request.Work.Title = r.Source.Title
	request.Work.Body = r.Source.Body
	request.Work.WorkType = "development"
	request.Work.ExecutionSecurity = stamp
	request.Work.Repository = "https://github.com/" + r.Source.OwnerRepo + ".git"
	result, err := c.producer.Admit(ctx, request)
	if err != nil {
		return LocalAdmissionEvidence{}, err
	}
	return LocalAdmissionEvidence{Session: result.Session, Intent: result.Intent, Receipt: result.Receipt, EffectiveCell: result.EffectiveCell, RuntimeBinding: result.RuntimeBinding, OperationalPayload: result.OperationalPayload, HostPreflight: result.HostPreflight, ProducerEvidence: result.Evidence}, nil
}

func (c *localHTTPTestCompiler) Revalidate(_ context.Context, e LocalAdmissionEvidence) error {
	return ValidateLocalExecutionSecurityEvidence(c.identity.ScopeID, e.OperationalPayload, e.HostPreflight)
}

type localHTTPTestSource struct {
	mu          sync.Mutex
	pollErr     error
	publication func(LocalPublicationTask) (LocalPublicationResult, error)
}

func (s *localHTTPTestSource) Poll(context.Context) ([]LocalIntakeRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return nil, s.pollErr
}

func (*localHTTPTestSource) Eligible(context.Context, LocalIntakeRequest) (bool, error) {
	return true, nil
}

func (s *localHTTPTestSource) Publish(_ context.Context, task LocalPublicationTask) (LocalPublicationResult, error) {
	s.mu.Lock()
	publication := s.publication
	s.mu.Unlock()
	if publication != nil {
		return publication(task)
	}
	return LocalPublicationResult{}, fmt.Errorf("publication is not exercised by the receiver fixture")
}

func TestLocalPendingPublicationSurvivesIntakeErrorWithCurrentPolicy(t *testing.T) {
	for _, policy := range []string{"current", "invalid", "stale-source"} {
		t.Run(policy, func(t *testing.T) {
			source := &localHTTPTestSource{}
			f := startLocalHTTPFixture(t, func(d *Daemon) { d.opts.LocalRuntime.Source = source })
			admitted := f.admit(t, 19)
			token := f.claim(t, admitted.Session)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// Join the real background cycle before arranging its next outcome;
			// the daemon remains Running and cycle itself is the subject below.
			if err := waitCompletionContext(ctx, f.runtime.beginStop()); err != nil {
				t.Fatal(err)
			}
			body := []byte(fmt.Sprintf(`{"workerId":%q,"status":"failed","summary":"intake error publication fixture"}`, f.runtime.identity.WorkerID))
			code, raw := f.request(t, http.MethodPost, basePath(admitted.Session.SessionID, "status"), token, body)
			if code != http.StatusOK {
				t.Fatalf("actual terminal commit %d: %s", code, raw)
			}
			projection, err := f.runtime.store.Session(ctx, f.runtime.identity.ScopeID, admitted.Session.SessionID)
			if err != nil || projection.Terminal == nil {
				t.Fatalf("terminal/outbox authority missing: %v", err)
			}
			pollErr := errors.New("fixture intake polling failed")
			var publications atomic.Int32
			source.mu.Lock()
			source.pollErr = pollErr
			source.publication = func(task LocalPublicationTask) (LocalPublicationResult, error) {
				publications.Add(1)
				if task.SessionID != admitted.Session.SessionID || task.Status != "failed" || task.Kind != localqueue.PublicationGitHubComment ||
					task.ResultSHA256 != projection.Terminal.BodySHA256 || task.Key == "" || task.Source.RepositoryID != 42 ||
					task.Source.OwnerRepo != "example/project" || task.Source.IssueNumber != 19 || task.Source.IssueURL != "https://github.com/example/project/issues/19" ||
					task.Source.Title != "" || task.Source.Body != "" {
					return LocalPublicationResult{}, errors.New("publication escaped its actual durable source/result authority")
				}
				return LocalPublicationResult{URL: task.Source.IssueURL + "#issuecomment-101", ReadAt: time.Now().UTC().Format(time.RFC3339Nano)}, nil
			}
			source.mu.Unlock()
			if policy != "current" {
				cfg := f.d.Config()
				if policy == "invalid" {
					cfg.LocalRuntime.ExecutionSecurity = nil
				} else {
					cfg.LocalRuntime.Repositories[0].Label = "changed-label"
				}
				if err := WriteConfig(f.d.opts.ConfigPath, cfg); err != nil {
					t.Fatal(err)
				}
			}
			if f.d.State() != StateRunning {
				t.Fatal("fixture must exercise the Running intake path")
			}
			cycleErr := f.runtime.cycle(ctx)
			pending, err := f.runtime.store.PendingPublication(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if policy != "current" {
				if cycleErr == nil || publications.Load() != 0 || len(pending) != 1 {
					t.Fatalf("invalid/stale policy granted publication: err=%v calls=%d pending=%d", cycleErr, publications.Load(), len(pending))
				}
				return
			}
			if !errors.Is(cycleErr, pollErr) || publications.Load() != 1 || len(pending) != 0 {
				t.Fatalf("intake failure starved authorized durable publication: err=%v calls=%d pending=%d", cycleErr, publications.Load(), len(pending))
			}
			if err := f.runtime.cycle(ctx); !errors.Is(err, pollErr) || publications.Load() != 1 {
				t.Fatalf("delivered terminal was republished or intake error lost: err=%v calls=%d", err, publications.Load())
			}
		})
	}
}

type localHTTPFixture struct {
	d                *Daemon
	server           *Server
	runtime          *localRuntime
	origin, operator string
	client           *http.Client
}

func startLocalHTTPFixture(t *testing.T, prepare ...func(*Daemon)) *localHTTPFixture {
	t.Helper()
	root := t.TempDir()
	binary := filepath.Join(root, "codex")
	if err := os.Symlink("/bin/sh", binary); err != nil {
		t.Fatal(err)
	}
	provider, err := codex.New(codex.Options{CodexBin: binary})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := runner.NewRegistryWithOptions(runner.RegistryOptions{RuntimeTransportMode: runner.RuntimeTransportLocal})
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Shutdown(context.Background()) })
	cfg := DefaultConfig()
	cfg.APIVersion = LocalRuntimeConfigAPIVersion
	cfg.Machine.ID = "local-http-fixture"
	cfg.Orchestrator.URL = "file://" + filepath.Join(root, "queue")
	cfg.Orchestrator.AuthToken = ""
	cfg.Capacity.MaxConcurrentSessions = 0
	cfg.LocalRuntime = &LocalRuntimeConfig{Harness: "codex", Model: "gpt-5-codex", ModelAuthor: "openai", ModelCatalogRevision: "explicit-fixture", ExecutionSecurity: InitialLocalExecutionSecurity(), Repositories: []LocalGitHubRepository{{RepositoryID: 42, OwnerRepo: "example/project", Label: "donmai"}}}
	configPath := filepath.Join(root, "daemon.yaml")
	if err = WriteConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	options := &LocalRuntimeOptions{Source: &localHTTPTestSource{}, PollInterval: time.Hour, NewCompiler: func(identity LocalRuntimeIdentity) (LocalRuntimeCompiler, error) {
		model := executioncell.ModelRef{ID: "gpt-5-codex", Author: "openai"}
		producer, err := runner.NewLocalAdmissionProducer(registry, runner.LocalAdmissionConfig{ScopeID: identity.ScopeID, WorkerID: identity.WorkerID, PlacementID: identity.HostID, ConfigurationRevision: "fixture-config", Harness: agent.HarnessCodex, Model: model, ModelCatalogEntry: &runner.LocalModelCatalogEntry{Model: model, Host: agent.HostOAuthCLI, Revision: "explicit-fixture"}, EndpointID: "local-openai", AuthBindingID: "local-login", BinaryPath: binary, CheckHostAuth: func(context.Context) error { return nil }})
		return &localHTTPTestCompiler{producer: producer, identity: identity, version: provider.Manifest().ContractABI}, err
	}}
	d := New(Options{ConfigPath: configPath, JWTPath: filepath.Join(root, "unused.jwt"), SkipWizard: true, HTTPHost: "127.0.0.1", HTTPPort: 0, LocalRuntime: options, SpawnerOptions: SpawnerOptions{WorkerCommand: localFixtureWorkerCommand(t), BaseEnv: map[string]string{"DONMAI_TEST_LOCAL_NO_RESULT": "1"}}, ProviderRegistry: runner.NewProviderView(registry)})
	if len(prepare) > 0 {
		prepare[0](d)
	}
	server := NewServer(d)
	_, err = server.StartBeforeDaemon()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		stopCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if err := d.Stop(stopCtx); err != nil {
			t.Error(err)
		}
		if err := server.Shutdown(stopCtx); err != nil {
			t.Error(err)
		}
	})
	if err = d.Start(ctx); err != nil {
		t.Fatal(err)
	}
	server.DaemonStarted()
	local := d.localRuntime.Load()
	if local == nil {
		t.Fatal("real file runtime not constructed")
	}
	credential, err := local.auth.OperatorCredential()
	if err != nil {
		t.Fatal(err)
	}
	return &localHTTPFixture{d: d, server: server, runtime: local, origin: "http://" + server.Addr(), operator: credential.BearerToken(), client: &http.Client{Timeout: 5 * time.Second}}
}

func (f *localHTTPFixture) request(t *testing.T, method, path, token string, body []byte) (int, []byte) {
	t.Helper()
	request, err := http.NewRequest(method, f.origin+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := f.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, raw
}

func (f *localHTTPFixture) admit(t *testing.T, number int) LocalIntakeResult {
	t.Helper()
	raw, _ := json.Marshal(LocalIntakeRequest{RepositoryID: 42, OwnerRepo: "example/project", IssueNumber: number, IssueURL: fmt.Sprintf("https://github.com/example/project/issues/%d", number), Title: "Authored issue", Body: "Implement the authored change."})
	code, body := f.request(t, http.MethodPost, localRuntimePrefix+"/intake", f.operator, raw)
	if code != http.StatusOK {
		t.Fatalf("intake %d: %s", code, body)
	}
	var admitted LocalIntakeResult
	if err := json.Unmarshal(body, &admitted); err != nil {
		t.Fatal(err)
	}
	return admitted
}

func (f *localHTTPFixture) claim(t *testing.T, session executioncell.SessionRef) string {
	t.Helper()
	ctx := context.Background()
	store := f.runtime.store
	attempt, err := store.Claim(ctx, store.CurrentRevision(), session)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := store.Session(ctx, f.runtime.identity.ScopeID, session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := f.runtime.auth.CreateAttempt(ctx, projection.Admission.Envelope.CredentialHandle, attempt)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.BeginLaunch(ctx, store.CurrentRevision(), attempt); err != nil {
		t.Fatal(err)
	}
	return credential.BearerToken()
}

func TestLocalReceiverAuthenticatesRolesAndExactAttempts(t *testing.T) {
	t.Parallel()
	f := startLocalHTTPFixture(t)
	before := f.runtime.store.CurrentRevision()
	code, _ := f.request(t, http.MethodPost, localRuntimePrefix+"/intake", "", []byte(`{}`))
	if code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated intake=%d", code)
	}
	if before != f.runtime.store.CurrentRevision() {
		t.Fatal("unauthenticated intake mutated store")
	}
	first := f.admit(t, 1)
	second := f.admit(t, 2)
	token := f.claim(t, first.Session)
	other := f.claim(t, second.Session)
	path := "/api/daemon/sessions/" + first.Session.SessionID
	for _, bad := range []string{"", f.operator, other} {
		code, _ = f.request(t, http.MethodGet, path, bad, nil)
		if code != http.StatusUnauthorized {
			t.Fatalf("wrong detail credential=%d", code)
		}
	}
	code, body := f.request(t, http.MethodGet, path, token, nil)
	if code != http.StatusOK {
		t.Fatalf("detail %d: %s", code, body)
	}
	var detail SessionDetail
	if err := json.Unmarshal(body, &detail); err != nil {
		t.Fatal(err)
	}
	if detail.AuthToken != "" || detail.McpAuthToken != "" || bytes.Contains(body, []byte(token)) {
		t.Fatal("detail exposed credentials")
	}
	projection, err := f.runtime.store.Session(context.Background(), f.runtime.identity.ScopeID, first.Session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(detail.OperationalPayload, projection.Admission.Envelope.OperationalPayload) {
		t.Fatal("detail changed exact payload")
	}
	code, _ = f.request(t, http.MethodPost, localRuntimePrefix+"/intake", token, []byte(`{}`))
	if code != http.StatusUnauthorized {
		t.Fatalf("attempt acquired intake authority=%d", code)
	}
	callback := localRuntimePrefix + "/api/sessions/" + first.Session.SessionID + "/status"
	poster, err := result.NewPoster(result.Options{PlatformURL: f.origin + localRuntimePrefix, AuthToken: token, WorkerID: f.runtime.identity.WorkerID, HTTPClient: f.client, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	outcome := agent.Result{Status: "failed", Summary: "owned fixture failed"}
	terminal, err := poster.PrepareTerminalStatusBody(context.Background(), first.Session.SessionID, outcome, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", f.operator, other} {
		code, _ = f.request(t, http.MethodPost, callback, bad, terminal)
		if code != http.StatusUnauthorized {
			t.Fatalf("wrong callback credential=%d", code)
		}
	}
	posted := poster.PostPreparedOutcome(context.Background(), first.Session.SessionID, outcome, terminal)
	if posted.CompletionErr != nil || posted.StatusErr != nil {
		t.Fatalf("real Poster: completion=%v status=%v", posted.CompletionErr, posted.StatusErr)
	}
	revision := f.runtime.store.CurrentRevision()
	code, _ = f.request(t, http.MethodPost, callback, token, terminal)
	if code != http.StatusOK || revision != f.runtime.store.CurrentRevision() {
		t.Fatal("exact terminal replay changed durable state")
	}
	changed := bytes.Replace(terminal, []byte("owned fixture failed"), []byte("different terminal"), 1)
	code, _ = f.request(t, http.MethodPost, callback, token, changed)
	if code != http.StatusConflict {
		t.Fatalf("different terminal replay=%d", code)
	}
	projection, err = f.runtime.store.Session(context.Background(), f.runtime.identity.ScopeID, first.Session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Terminal == nil || projection.Admission.DispatchState != localqueue.DispatchTerminal {
		t.Fatal("HTTP success preceded durable terminal")
	}
}

type (
	localHTTPObservedResponse struct {
		path string
		code int
	}
	localHTTPObservedTransport struct {
		responses chan localHTTPObservedResponse
	}
)

func (o *localHTTPObservedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := http.DefaultTransport.RoundTrip(r)
	if err == nil {
		select {
		case o.responses <- localHTTPObservedResponse{r.URL.Path, response.StatusCode}:
		default:
		}
	}
	return response, err
}

func TestLocalReceiverAcceptsShippedRuntimeEmitters(t *testing.T) {
	t.Parallel()
	f := startLocalHTTPFixture(t)
	admitted := f.admit(t, 3)
	token := f.claim(t, admitted.Session)
	observed := &localHTTPObservedTransport{responses: make(chan localHTTPObservedResponse, 32)}
	client := &http.Client{Transport: observed, Timeout: 5 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	base := f.origin + localRuntimePrefix
	id := admitted.Session.SessionID
	worker := f.runtime.identity.WorkerID
	pulse, err := heartbeat.New(heartbeat.Config{SessionID: id, WorkerID: worker, BaseURL: base, AuthToken: token, HTTPClient: client, Interval: 10 * time.Millisecond, MaxAttemptsPerTick: 1})
	if err != nil {
		t.Fatal(err)
	}
	step, err := stepheartbeat.New(stepheartbeat.Config{SessionID: id, WorkerID: worker, BaseURL: base, AuthToken: token, HTTPClient: client, Interval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	activityPoster, err := activity.New(activity.Config{SessionID: id, WorkerID: worker, BaseURL: base, AuthToken: token, HTTPClient: client, MaxRetries: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, start := range []func(context.Context) error{pulse.Start, step.Start, activityPoster.Start} {
		if err = start(ctx); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = pulse.Stop(); _ = step.Stop(); _ = activityPoster.Stop() })
	activityPoster.Send(ctx, agent.AssistantTextEvent{Text: "owned fixture observation"})
	seen := map[string]bool{}
	for len(seen) < 3 {
		select {
		case response := <-observed.responses:
			if response.code != http.StatusOK {
				t.Fatalf("actual emitter %s returned %d", response.path, response.code)
			}
			for _, suffix := range []string{"/lock-refresh", "/step-heartbeat", "/activity"} {
				if strings.HasSuffix(response.path, suffix) {
					seen[suffix] = true
				}
			}
		case <-ctx.Done():
			t.Fatalf("actual emitter routes missing: %v", seen)
		}
	}
	if err = pulse.Stop(); err != nil {
		t.Fatal(err)
	}
	if err = step.Stop(); err != nil {
		t.Fatal(err)
	}
	if err = activityPoster.Stop(); err != nil {
		t.Fatal(err)
	}
	raw := []byte(fmt.Sprintf(`{"workerId":%q}`, worker))
	code, body := f.request(t, http.MethodPost, basePath(id, "lock-refresh"), token, raw)
	if code != http.StatusOK {
		t.Fatal(string(body))
	}
	var reply struct {
		Refreshed bool
		Stop      bool
	}
	if err = json.Unmarshal(body, &reply); err != nil || !reply.Refreshed || reply.Stop {
		t.Fatalf("live exact-host heartbeat=%s", body)
	}
	if err = f.runtime.requestStop(ctx, id); err != nil {
		t.Fatal(err)
	}
	code, body = f.request(t, http.MethodPost, basePath(id, "lock-refresh"), token, raw)
	if code != http.StatusOK {
		t.Fatal(string(body))
	}
	if err = json.Unmarshal(body, &reply); err != nil || reply.Refreshed || !reply.Stop {
		t.Fatalf("stopped exact-host heartbeat=%s", body)
	}
}

func basePath(id, route string) string {
	return localRuntimePrefix + "/api/sessions/" + id + "/" + route
}

func TestLocalOwnedWorkerHelper(t *testing.T) {
	control := os.Getenv("DONMAI_TEST_LOCAL_WORKER_CONTROL")
	if control == "" {
		return
	}
	token := os.Getenv("DONMAI_RUNTIME_JWT")
	id := os.Getenv("DONMAI_SESSION_ID")
	origin := localWorkerFixtureOrigin(t, os.Getenv(EnvDaemonControlURL))
	control = localWorkerFixtureOrigin(t, control)
	if token == "" || id == "" || origin == "" {
		t.Fatal("owned worker did not receive its callback authority")
	}
	client := localWorkerFixtureClient(t, origin)
	request, err := http.NewRequest(http.MethodGet, "http://127.0.0.1/", nil)
	request.URL.Path = "/api/daemon/sessions/" + id
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var detail SessionDetail
	err = json.NewDecoder(response.Body).Decode(&detail)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || detail.AuthToken != "" || detail.McpAuthToken != "" {
		t.Fatal("owned worker did not hydrate protected blank-auth detail")
	}
	if detail.PlatformURL != origin+localRuntimePrefix {
		t.Fatal("owned worker callback origin differs")
	}
	if err = ValidateLocalExecutionSecurityEvidence(detail.OrganizationID, detail.OperationalPayload, detail.HostAdaptationReceipt); err != nil {
		t.Fatal(err)
	}
	response, err = localWorkerFixtureClient(t, control).Get("http://127.0.0.1/?pid=" + strconv.Itoa(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal("owned control did not release worker")
	}
	poster, err := result.NewPoster(result.Options{PlatformURL: detail.PlatformURL, AuthToken: token, WorkerID: detail.WorkerID, HTTPClient: client, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	terminal := agent.Result{Status: "failed", Summary: "owned child completed its callback fixture"}
	body, err := poster.PrepareTerminalStatusBody(context.Background(), id, terminal, nil)
	if err != nil {
		t.Fatal(err)
	}
	posted := poster.PostPreparedOutcome(context.Background(), id, terminal, body)
	if posted.CompletionErr != nil || posted.StatusErr != nil {
		t.Fatalf("child callback failed: %v / %v", posted.CompletionErr, posted.StatusErr)
	}
}

func TestLocalDispatchOwnsActualChildAndHydratesProtectedDetail(t *testing.T) {
	t.Parallel()
	ready := make(chan int, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseChild := func() { releaseOnce.Do(func() { close(release) }) }
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pid, err := strconv.Atoi(r.URL.Query().Get("pid"))
		if err != nil {
			http.Error(w, "invalid pid", 400)
			return
		}
		ready <- pid
		select {
		case <-release:
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(control.Close)
	t.Cleanup(releaseChild)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f := startLocalHTTPFixture(t, func(d *Daemon) {
		d.opts.SpawnerOptions.WorkerCommand = []string{executable, "-test.run=^TestLocalOwnedWorkerHelper$", "--"}
		d.opts.SpawnerOptions.BaseEnv = map[string]string{"DONMAI_TEST_LOCAL_WORKER_CONTROL": control.URL}
	})
	t.Cleanup(releaseChild)
	admitted := f.admit(t, 5)
	// The periodic intake stays at zero capacity. Admit exactly this owned fixture
	// through the real local dispatch after explicitly opening one spawner slot.
	if err = f.d.spawner.SetMaxConcurrentSessions(1); err != nil {
		t.Fatal(err)
	}
	projection, err := f.runtime.store.Session(context.Background(), f.runtime.identity.ScopeID, admitted.Session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dispatched := make(chan error, 1)
	go func() { dispatched <- f.runtime.dispatch(ctx, projection.Admission, f.runtime.source) }()
	var pid int
	dispatchDone := false
	select {
	case pid = <-ready:
	case err = <-dispatched:
		dispatchDone = true
		if err != nil {
			t.Fatal(err)
		}
		select {
		case pid = <-ready:
		case <-ctx.Done():
			t.Fatal("spawned child did not fetch actual detail")
		}
	case <-ctx.Done():
		t.Fatal("owned child did not start")
	}
	if !dispatchDone {
		select {
		case err = <-dispatched:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("dispatch did not record launch")
		}
	}
	projection, err = f.runtime.store.Session(ctx, f.runtime.identity.ScopeID, admitted.Session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Launch == nil || projection.Launch.ProcessID != pid || projection.Launch.ProcessStart == "" {
		t.Fatal("actual child PID/start was not durably correlated")
	}
	f.runtime.mu.Lock()
	permits := len(f.runtime.launchPermits)
	f.runtime.mu.Unlock()
	if permits != 0 {
		t.Fatal("successful launch retained its private permit")
	}
	releaseChild()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		projection, err = f.runtime.store.Session(ctx, f.runtime.identity.ScopeID, admitted.Session.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if projection.Terminal != nil {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("child terminal callback did not commit")
		}
	}
	if projection.Terminal.Status != "failed" {
		t.Fatal("child reported unexpected terminal state")
	}
}

func TestLocalInstalledCredentialRailRefusesMissingOwnPolicyBeforeCaller(t *testing.T) {
	t.Parallel()
	var callerHooks atomic.Int32
	f := startLocalHTTPFixture(t, func(d *Daemon) {
		d.opts.SpawnerOptions.OnPreSpawn = func(_ SessionSpec, env []string) ([]string, error) { callerHooks.Add(1); return env, nil }
	})
	admitted := f.admit(t, 7)
	_ = f.claim(t, admitted.Session)
	if err := os.Remove(f.d.opts.ConfigPath); err != nil {
		t.Fatal(err)
	}
	env, err := f.d.spawner.opts.OnPreSpawn(SessionSpec{SessionID: admitted.Session.SessionID}, nil)
	if err == nil || len(env) != 0 {
		t.Fatal("missing policy released a runtime credential")
	}
	if callerHooks.Load() != 0 {
		t.Fatal("missing own policy invoked caller credential release before refusal")
	}
	if f.d.spawner.ActiveCount() != 0 {
		t.Fatal("credential refusal launched a process")
	}
}

func localWorkerFixtureOrigin(t *testing.T, raw string) string {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		t.Fatal("worker fixture requires canonical owned loopback origin")
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		t.Fatal("worker fixture requires explicit valid port")
	}
	return "http://127.0.0.1:" + strconv.Itoa(port)
}

func localWorkerFixtureClient(t *testing.T, origin string) *http.Client {
	t.Helper()
	parsed, err := url.Parse(origin)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatal(err)
	}
	// The owned subprocess has no general network transport. Both request hosts
	// and the dial destination are pinned to loopback, with only an integer port
	// supplied by the parent fixture.
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	transport := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("fixture redirects refused") }}
}

type localBlockingSource struct {
	entered chan struct{}
	active  atomic.Bool
	closed  atomic.Bool
}

func (s *localBlockingSource) Poll(ctx context.Context) ([]LocalIntakeRequest, error) {
	s.active.Store(true)
	defer s.active.Store(false)
	close(s.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (*localBlockingSource) Eligible(context.Context, LocalIntakeRequest) (bool, error) {
	return false, nil
}

func (*localBlockingSource) Publish(context.Context, LocalPublicationTask) (LocalPublicationResult, error) {
	return LocalPublicationResult{}, errors.New("no publication")
}

func (s *localBlockingSource) Close() error {
	if s.active.Load() {
		return errors.New("source closed during active call")
	}
	s.closed.Store(true)
	return nil
}

func TestLocalStopCancelsJoinsAndClosesOwnedSource(t *testing.T) {
	t.Parallel()
	source := &localBlockingSource{entered: make(chan struct{})}
	f := startLocalHTTPFixture(t, func(d *Daemon) {
		d.opts.LocalRuntime.NewSource = func([]LocalGitHubRepository) (LocalRuntimeSource, error) { return source, nil }
	})
	select {
	case <-source.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("local source did not start after listener readiness")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.d.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if source.active.Load() || !source.closed.Load() {
		t.Fatal("Stop returned before owned source join/Close")
	}
	if _, _, err := f.runtime.sourceLease(); err == nil {
		t.Fatal("stopped source admitted a new operation")
	}
	if err := f.d.Stop(ctx); err != nil {
		t.Fatal("idempotent Stop failed")
	}
}

func localFixtureWorkerCommand(t *testing.T) []string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return []string{executable, "-test.run=^TestLocalNoResultWorkerHelper$", "--"}
}

func TestLocalNoResultWorkerHelper(t *testing.T) {
	if os.Getenv("DONMAI_TEST_LOCAL_NO_RESULT") == "1" {
		t.Fatal("owned worker exits without a terminal callback")
	}
}

func localOperatorControlClient(t *testing.T, fixture *localHTTPFixture) *afclient.DaemonClient {
	t.Helper()
	host, textPort, err := net.SplitHostPort(fixture.server.Addr())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(textPort)
	if err != nil {
		t.Fatal(err)
	}
	// The actual daemon client sends the protected local operator bearer on
	// its ordinary POST routes. The auth store and both server gates stay real.
	return afclient.NewDaemonClient(afclient.DaemonConfig{Host: host, Port: port, ControlToken: fixture.operator})
}

func TestLocalOperatorControlsUseVerifiedBearer(t *testing.T) {
	configure := func(d *Daemon) {
		d.opts.RequireControlToken = true
		d.opts.ControlToken = "synthetic-installed-control-only"
	}
	t.Run("pause-resume-capacity", func(t *testing.T) {
		fixture := startLocalHTTPFixture(t, configure)
		client := localOperatorControlClient(t, fixture)
		if response, err := client.Pause(); err != nil || response == nil || !response.OK {
			t.Fatalf("actual local pause client: response=%+v err=%v", response, err)
		}
		if response, err := client.Resume(); err != nil || response == nil || !response.OK {
			t.Fatalf("actual local resume client: response=%+v err=%v", response, err)
		}
		if response, err := client.SetCapacityConfig("capacity.maxConcurrentSessions", "1"); err != nil || response == nil || !response.OK || fixture.d.MaxConcurrentSessions() != 1 {
			t.Fatalf("actual local capacity client: response=%+v err=%v limit=%d", response, err, fixture.d.MaxConcurrentSessions())
		}
	})
	t.Run("session-stop", func(t *testing.T) {
		fixture := startLocalHTTPFixture(t, configure)
		admitted := fixture.admit(t, 28)
		worker := fixture.claim(t, admitted.Session)
		path := "/api/daemon/sessions/" + admitted.Session.SessionID + "/stop"
		if code, _ := fixture.request(t, http.MethodPost, path, worker, nil); code != http.StatusUnauthorized {
			t.Fatalf("worker attempt token gained session-stop control: %d", code)
		}
		if code, body := fixture.request(t, http.MethodPost, path, fixture.operator, nil); code != http.StatusOK || !bytes.Contains(body, []byte("stop requested")) {
			t.Fatalf("operator session-stop: status=%d body=%s", code, body)
		}
		projection, err := fixture.runtime.store.Session(context.Background(), fixture.runtime.identity.ScopeID, admitted.Session.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		seen := false
		for _, observation := range projection.Observations {
			seen = seen || observation.Kind == localqueue.ObservationStopRequest
		}
		if !seen {
			t.Fatal("operator session-stop did not reach the durable request handler")
		}
	})
	t.Run("drain", func(t *testing.T) {
		fixture := startLocalHTTPFixture(t, configure)
		response, err := localOperatorControlClient(t, fixture).Drain(1)
		if err != nil || response == nil || !response.OK {
			t.Fatalf("actual local drain client: response=%+v err=%v", response, err)
		}
	})
	t.Run("stop", func(t *testing.T) {
		fixture := startLocalHTTPFixture(t, configure)
		response, err := localOperatorControlClient(t, fixture).Stop()
		if err != nil || response == nil || !response.OK {
			t.Fatalf("actual local stop client: response=%+v err=%v", response, err)
		}
	})
}

func TestLocalOperatorControlsRefuseOtherBearersAndUnavailableControl(t *testing.T) {
	t.Run("roles", func(t *testing.T) {
		fixture := startLocalHTTPFixture(t, func(d *Daemon) {
			d.opts.RequireControlToken = true
			d.opts.ControlToken = "synthetic-installed-control-only"
		})
		admitted := fixture.admit(t, 29)
		worker := fixture.claim(t, admitted.Session)
		before := fixture.d.State()
		for _, bearer := range []string{"", "synthetic-wrong", worker, "synthetic-installed-control-only"} {
			if code, _ := fixture.request(t, http.MethodPost, "/api/daemon/pause", bearer, nil); code != http.StatusUnauthorized {
				t.Fatalf("nonoperator bearer reached pause: status=%d", code)
			}
		}
		if fixture.d.State() != before {
			t.Fatal("refused bearer changed daemon state")
		}
	})
	t.Run("required-control-unavailable", func(t *testing.T) {
		fixture := startLocalHTTPFixture(t, func(d *Daemon) {
			d.opts.RequireControlToken = true
			d.opts.ControlToken = ""
		})
		before := fixture.d.State()
		if code, body := fixture.request(t, http.MethodPost, "/api/daemon/pause", fixture.operator, nil); code != http.StatusServiceUnavailable || !bytes.Contains(body, []byte("control token unavailable")) {
			t.Fatalf("required unavailable material: status=%d body=%s", code, body)
		}
		if fixture.d.State() != before {
			t.Fatal("required unavailable control material reached pause")
		}
		if code, _ := fixture.request(t, http.MethodPost, "/api/daemon/pause", "synthetic-wrong", nil); code != http.StatusUnauthorized {
			t.Fatalf("wrong local bearer reached unavailable gate: %d", code)
		}
	})
}
