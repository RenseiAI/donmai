package afcli

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/daemon"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/internal/localqueue"
	"github.com/RenseiAI/donmai/internal/localruntimeauth"
)

func TestLocalOperatorClientBindsRoleOriginAndRedirects(t *testing.T) {
	t.Parallel()
	var recipientCalls atomic.Int32
	recipient := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { recipientCalls.Add(1); w.WriteHeader(http.StatusOK) }))
	t.Cleanup(recipient.Close)
	var expected string
	var protectedCalls atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if r.Header.Get("Authorization") != "" {
				t.Error("read-only request gained operator credential")
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+expected {
			t.Error("configured operator credential was not attached")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		protectedCalls.Add(1)
		if r.URL.Path == "/redirect-same" {
			http.Redirect(w, r, "/control", http.StatusTemporaryRedirect)
			return
		}
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, recipient.URL, http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(origin.Close)
	root := filepath.Join(t.TempDir(), "queue")
	auth, err := localruntimeauth.Bootstrap(root+".auth", root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = auth.Close() })
	identity := auth.Identity()
	store, err := localqueue.Open(root, localqueue.ConfiguredHostBinding{ScopeID: identity.ScopeID, HostID: identity.HostID, WorkerID: identity.WorkerID, Selectors: &executioncell.ExecutionSelectorRegistry{HarnessVersions: map[string][]string{}, Models: []string{}, Endpoints: []string{}, AuthBindings: []string{}, Placements: []string{}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	// This fixture owns the actual queue writer lease while publishing metadata.
	if _, err = auth.PublishEndpoint(context.Background(), origin.URL, "owned-fixture"); err != nil {
		t.Fatal(err)
	}
	credential, err := auth.OperatorCredential()
	if err != nil {
		t.Fatal(err)
	}
	expected = credential.BearerToken()
	cfg := daemon.DefaultConfig()
	cfg.APIVersion = daemon.LocalRuntimeConfigAPIVersion
	cfg.Orchestrator.URL = "file://" + root
	cfg.Orchestrator.AuthToken = ""
	cfg.LocalRuntime = &daemon.LocalRuntimeConfig{ExecutionSecurity: daemon.InitialLocalExecutionSecurity()}
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	if err = daemon.WriteConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	host, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	client, err := localOperatorClient(afclient.DaemonConfig{Host: host, Port: port}, path)
	if err != nil || client == nil {
		t.Fatalf("client: %v", err)
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		request, err := http.NewRequest(method, origin.URL+"/control", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatal(response.StatusCode)
		}
		if request.Header.Get("Authorization") != "" {
			t.Fatal("transport mutated caller-owned headers")
		}
	}
	response, err := client.Post(origin.URL+"/redirect", "application/json", strings.NewReader(`{}`))
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil {
		t.Fatal("operator client followed redirect")
	}
	response, err = client.Post(origin.URL+"/redirect-same", "application/json", strings.NewReader(`{}`))
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil {
		t.Fatal("operator client followed same-origin redirect")
	}
	response, err = client.Post(recipient.URL+"/control", "application/json", strings.NewReader(`{}`))
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil {
		t.Fatal("operator client sent a direct foreign-origin mutation")
	}
	if recipientCalls.Load() != 0 || protectedCalls.Load() != 3 {
		t.Fatalf("origin fence: recipient=%d protected=%d", recipientCalls.Load(), protectedCalls.Load())
	}
}

func TestDaemonClientCompositionPreservesControlToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(afclient.ControlTokenEnv, "synthetic-operator-control")
	for _, name := range []string{"controller_factory", "custom_http_client"} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/api/daemon/capacity" {
					t.Errorf("unexpected control request %s %s", r.Method, r.URL.Path)
				}
				if r.Header.Get("Authorization") != "Bearer synthetic-operator-control" {
					t.Error("caller control credential was dropped")
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode control request: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if body["key"] != "capacity.maxConcurrentSessions" || body["value"] != "0" {
					t.Errorf("control arguments changed: %v", body)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{}`)
			}))
			t.Cleanup(server.Close)
			parsed, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			host, portText, err := net.SplitHostPort(parsed.Host)
			if err != nil {
				t.Fatal(err)
			}
			port, err := strconv.Atoi(portText)
			if err != nil {
				t.Fatal(err)
			}
			cfg := afclient.DaemonConfig{Host: host, Port: port}
			var client daemonDoer
			if name == "controller_factory" {
				client = defaultDaemonFactory(cfg)
			} else {
				cfg.ControlToken = "synthetic-operator-control"
				client = afclient.NewDaemonClientWithHTTPClient(cfg, server.Client())
			}
			if _, err := client.SetCapacityConfig("capacity.maxConcurrentSessions", "0"); err != nil {
				t.Fatalf("actual control mutation: %v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("control requests = %d, want 1", calls.Load())
			}
		})
	}
}
