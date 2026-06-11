package gemini

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	googleendpoint "github.com/RenseiAI/donmai/provider/endpoint/google"
)

// TestSpawnURL_Table exercises the Spec.Endpoint → generateContent URL
// routing for the serving hosts the raw × google matrix declares.
func TestSpawnURL_Table(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		base    string
		model   string
		ep      *agent.EndpointBinding
		want    string
		wantErr bool
	}{
		{
			name:  "nil endpoint keeps the construction URL",
			base:  DefaultEndpoint,
			model: "gemini-3.5-flash",
			want:  "https://generativelanguage.googleapis.com/v1beta/models/gemini-3.5-flash:generateContent",
		},
		{
			name:  "direct host with binding base URL overrides",
			base:  DefaultEndpoint,
			model: "gemini-2.5-pro",
			ep: &agent.EndpointBinding{
				Company: agent.CompanyGoogle,
				Host:    agent.HostDirect,
				BaseURL: "https://mirror.example.com/",
			},
			want: "https://mirror.example.com/v1beta/models/gemini-2.5-pro:generateContent",
		},
		{
			name:  "direct host without base URL keeps the construction URL",
			base:  "https://construction.example.com",
			model: "gemini-2.5-pro",
			ep: &agent.EndpointBinding{
				Company: agent.CompanyGoogle,
				Host:    agent.HostDirect,
			},
			want: "https://construction.example.com/v1beta/models/gemini-2.5-pro:generateContent",
		},
		{
			name:  "vertex host routes the publisher resource path",
			base:  DefaultEndpoint,
			model: "gemini-2.5-pro",
			ep: &agent.EndpointBinding{
				Company: agent.CompanyGoogle,
				Host:    agent.HostVertex,
				BaseURL: "https://us-central1-aiplatform.googleapis.com",
				Region:  "us-central1",
				Env:     map[string]string{EnvVertexProject: "proj-1"},
			},
			want: "https://us-central1-aiplatform.googleapis.com/v1/projects/proj-1/locations/us-central1/publishers/google/models/gemini-2.5-pro:generateContent",
		},
		{
			name:  "vertex host without a project fails loudly",
			base:  DefaultEndpoint,
			model: "gemini-2.5-pro",
			ep: &agent.EndpointBinding{
				Company: agent.CompanyGoogle,
				Host:    agent.HostVertex,
				Region:  "us-central1",
			},
			wantErr: true,
		},
		{
			name:  "vertex host without a region fails loudly",
			base:  DefaultEndpoint,
			model: "gemini-2.5-pro",
			ep: &agent.EndpointBinding{
				Company: agent.CompanyGoogle,
				Host:    agent.HostVertex,
				Env:     map[string]string{EnvVertexProject: "proj-1"},
			},
			wantErr: true,
		},
		{
			name:  "non-google company fails loudly",
			base:  DefaultEndpoint,
			model: "gemini-2.5-pro",
			ep: &agent.EndpointBinding{
				Company: agent.CompanyAnthropic,
				Host:    agent.HostDirect,
			},
			wantErr: true,
		},
		{
			name:  "unroutable serving host fails loudly",
			base:  DefaultEndpoint,
			model: "gemini-2.5-pro",
			ep: &agent.EndpointBinding{
				Company: agent.CompanyGoogle,
				Host:    agent.HostBedrock,
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := spawnURL(tc.base, tc.model, tc.ep)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("spawnURL: want error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("spawnURL: %v", err)
			}
			if got != tc.want {
				t.Errorf("spawnURL = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestProvider_Spawn_EndpointVertexRouting proves a resolved vertex binding
// routes end-to-end: the request hits the binding's base URL on the full
// publisher resource path, authenticates with the binding's credential, and
// runs the binding's model — while the construction endpoint/key/model are
// all different (and must NOT be used).
func TestProvider_Spawn_EndpointVertexRouting(t *testing.T) {
	t.Parallel()

	var gotPath, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-goog-api-key")
		writeJSON(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
	}))
	defer srv.Close()

	p := mustNew(t, "https://construction-endpoint.invalid") // must never be dialed
	h, err := p.Spawn(context.Background(), agent.Spec{
		Prompt: "hi",
		Model:  "gemini-3.5-flash", // binding model must win
		Endpoint: &agent.EndpointBinding{
			Company: agent.CompanyGoogle,
			Model:   "gemini-2.5-pro",
			Host:    agent.HostVertex,
			BaseURL: srv.URL,
			Region:  "us-central1",
			Env: map[string]string{ //nolint:gosec // G101: fake test credential, not a secret
				EnvVertexProject: "proj-1",
				EnvAPIKeyPrimary: "vertex-key",
			},
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()
	drainUntilResult(t, h)

	wantPath := "/v1/projects/proj-1/locations/us-central1/publishers/google/models/gemini-2.5-pro:generateContent"
	if gotPath != wantPath {
		t.Errorf("request path = %q, want %q", gotPath, wantPath)
	}
	if gotKey != "vertex-key" {
		t.Errorf("x-goog-api-key = %q, want vertex-key (binding env must win)", gotKey)
	}
}

// TestProvider_Spawn_EndpointBaseURLOverride proves a direct-host binding's
// BaseURL re-routes the session away from the construction endpoint.
func TestProvider_Spawn_EndpointBaseURLOverride(t *testing.T) {
	t.Parallel()

	var construction, override int
	consSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		construction++
		writeJSON(w, `{"candidates":[{"content":{"parts":[{"text":"wrong"}]},"finishReason":"STOP"}]}`)
	}))
	defer consSrv.Close()
	overSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		override++
		writeJSON(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
	}))
	defer overSrv.Close()

	p := mustNew(t, consSrv.URL)
	h, err := p.Spawn(context.Background(), agent.Spec{
		Prompt: "hi",
		Endpoint: &agent.EndpointBinding{
			Company: agent.CompanyGoogle,
			Host:    agent.HostDirect,
			BaseURL: overSrv.URL,
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()
	drainUntilResult(t, h)

	if construction != 0 || override != 1 {
		t.Errorf("construction hits = %d (want 0), override hits = %d (want 1)", construction, override)
	}
}

// TestProvider_Spawn_EndpointHalfBoundVertexFails proves a half-configured
// vertex binding fails the spawn rather than silently routing to the public
// endpoint (which would mis-route and mis-bill).
func TestProvider_Spawn_EndpointHalfBoundVertexFails(t *testing.T) {
	t.Parallel()

	p := mustNew(t, "")
	_, err := p.Spawn(context.Background(), agent.Spec{
		Prompt: "hi",
		Endpoint: &agent.EndpointBinding{
			Company: agent.CompanyGoogle,
			Host:    agent.HostVertex,
			Region:  "us-central1", // no project id
		},
	})
	if err == nil {
		t.Fatal("Spawn: want error for half-bound vertex endpoint, got nil")
	}
}

// TestSpawnURL_ResolvedGoogleVertexCell pins the manifest↔harness contract
// end-to-end: the REAL google endpoint package resolves the declared vertex
// cell (region-templated aiplatform base URL + project-id env key) and the
// resulting binding routes through spawnURL to the full publisher resource
// path. Hand-rolled bindings in the tests above cannot catch a manifest
// edit (BaseURLTmpl, EnvKeys) that strands the declared raw × google ×
// vertex matrix cell — this test does.
func TestSpawnURL_ResolvedGoogleVertexCell(t *testing.T) {
	t.Parallel()

	binding, err := googleendpoint.New().Resolve(context.Background(), agent.EndpointRequest{
		Model:  "gemini-2.5-pro",
		Host:   agent.HostVertex,
		Auth:   agent.AuthBYOK,
		Region: "us-central1",
		EnvProvided: map[string]string{
			EnvVertexProject: "proj-1",
		},
	})
	if err != nil {
		t.Fatalf("google endpoint Resolve(vertex): %v", err)
	}

	got, err := spawnURL(DefaultEndpoint, binding.Model, &binding)
	if err != nil {
		t.Fatalf("spawnURL(resolved vertex binding): %v", err)
	}
	want := "https://us-central1-aiplatform.googleapis.com" +
		"/v1/projects/proj-1/locations/us-central1/publishers/google/models/gemini-2.5-pro:generateContent"
	if got != want {
		t.Errorf("spawnURL = %q, want %q", got, want)
	}
}
