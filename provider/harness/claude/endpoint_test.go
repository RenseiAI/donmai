package claude

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// TestApplyEndpoint_Table exercises the Spec.Endpoint → CLI env projection
// for every serving host the claude-code × anthropic matrix declares.
func TestApplyEndpoint_Table(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		spec      agent.Spec
		wantErr   bool
		wantEnv   map[string]string // exact expected Spec.Env (nil = unchanged input env)
		wantModel string
	}{
		{
			name:      "nil endpoint is a no-op",
			spec:      agent.Spec{Model: "claude-haiku", Env: map[string]string{"FOO": "bar"}},
			wantEnv:   map[string]string{"FOO": "bar"},
			wantModel: "claude-haiku",
		},
		{
			name: "oauth-cli host leaves env alone but binds the model",
			spec: agent.Spec{
				Model: "claude-haiku",
				Env:   map[string]string{"FOO": "bar"},
				Endpoint: &agent.EndpointBinding{
					Company: agent.CompanyAnthropic,
					Model:   "claude-sonnet-4-5",
					Host:    agent.HostOAuthCLI,
				},
			},
			wantEnv:   map[string]string{"FOO": "bar"},
			wantModel: "claude-sonnet-4-5",
		},
		{
			name: "direct host sets base URL and merges binding credentials",
			spec: agent.Spec{
				Env: map[string]string{"FOO": "bar"},
				Endpoint: &agent.EndpointBinding{
					Company: agent.CompanyAnthropic,
					Model:   "claude-sonnet-4-5",
					Host:    agent.HostDirect,
					BaseURL: "https://api.anthropic.com",
					Env:     map[string]string{"ANTHROPIC_API_KEY": "sk-test"},
				},
			},
			wantEnv: map[string]string{
				"FOO":               "bar",
				"ANTHROPIC_API_KEY": "sk-test",
				EnvBaseURL:          "https://api.anthropic.com",
			},
			wantModel: "claude-sonnet-4-5",
		},
		{
			name: "direct host without base URL only merges credentials",
			spec: agent.Spec{
				Endpoint: &agent.EndpointBinding{
					Company: agent.CompanyAnthropic,
					Host:    agent.HostDirect,
					Env:     map[string]string{"ANTHROPIC_API_KEY": "sk-test"},
				},
			},
			wantEnv: map[string]string{"ANTHROPIC_API_KEY": "sk-test"},
		},
		{
			name: "bedrock host flips the CLI knob and derives the region",
			spec: agent.Spec{
				Endpoint: &agent.EndpointBinding{
					Company: agent.CompanyAnthropic,
					Host:    agent.HostBedrock,
					Region:  "us-east-1",
					Env: map[string]string{
						"AWS_ACCESS_KEY_ID":     "AKIATEST",
						"AWS_SECRET_ACCESS_KEY": "secret",
					},
				},
			},
			wantEnv: map[string]string{
				EnvUseBedrock:           "1",
				EnvAWSRegion:            "us-east-1",
				"AWS_ACCESS_KEY_ID":     "AKIATEST",
				"AWS_SECRET_ACCESS_KEY": "secret",
			},
		},
		{
			name: "explicit binding AWS_REGION wins over the derived region",
			spec: agent.Spec{
				Endpoint: &agent.EndpointBinding{
					Company: agent.CompanyAnthropic,
					Host:    agent.HostBedrock,
					Region:  "us-east-1",
					Env:     map[string]string{EnvAWSRegion: "eu-west-1"},
				},
			},
			wantEnv: map[string]string{
				EnvUseBedrock: "1",
				EnvAWSRegion:  "eu-west-1",
			},
		},
		{
			name: "vertex host flips the CLI knob and derives the region",
			spec: agent.Spec{
				Endpoint: &agent.EndpointBinding{
					Company: agent.CompanyAnthropic,
					Host:    agent.HostVertex,
					Region:  "us-central1",
					Env: map[string]string{
						"ANTHROPIC_VERTEX_PROJECT_ID": "proj-1",
					},
				},
			},
			wantEnv: map[string]string{
				EnvUseVertex:                  "1",
				EnvVertexRegion:               "us-central1",
				"ANTHROPIC_VERTEX_PROJECT_ID": "proj-1",
			},
		},
		{
			name: "binding env wins over session env on collision",
			spec: agent.Spec{
				Env: map[string]string{"ANTHROPIC_API_KEY": "stale-key"},
				Endpoint: &agent.EndpointBinding{
					Company: agent.CompanyAnthropic,
					Host:    agent.HostDirect,
					Env:     map[string]string{"ANTHROPIC_API_KEY": "fresh-key"},
				},
			},
			wantEnv: map[string]string{"ANTHROPIC_API_KEY": "fresh-key"},
		},
		{
			name: "empty binding model keeps the spec model",
			spec: agent.Spec{
				Model: "claude-haiku",
				Endpoint: &agent.EndpointBinding{
					Company: agent.CompanyAnthropic,
					Host:    agent.HostDirect,
				},
			},
			wantEnv:   map[string]string{},
			wantModel: "claude-haiku",
		},
		{
			name: "non-anthropic company fails loudly",
			spec: agent.Spec{
				Endpoint: &agent.EndpointBinding{
					Company: agent.CompanyGoogle,
					Host:    agent.HostVertex,
				},
			},
			wantErr: true,
		},
		{
			name: "unroutable serving host fails loudly",
			spec: agent.Spec{
				Endpoint: &agent.EndpointBinding{
					Company: agent.CompanyAnthropic,
					Host:    agent.HostAzure,
				},
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := applyEndpoint(tc.spec)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("applyEndpoint: want error, got nil (spec=%+v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("applyEndpoint: %v", err)
			}
			if got.Model != tc.wantModel {
				t.Errorf("Model = %q, want %q", got.Model, tc.wantModel)
			}
			if len(got.Env) != len(tc.wantEnv) {
				t.Errorf("Env = %v, want %v", got.Env, tc.wantEnv)
			}
			for k, want := range tc.wantEnv {
				if got.Env[k] != want {
					t.Errorf("Env[%s] = %q, want %q", k, got.Env[k], want)
				}
			}
		})
	}
}

// TestApplyEndpoint_DoesNotMutateInputEnv proves the projection returns a
// merged COPY: the caller's Spec.Env map must remain untouched (specs are
// shared currency; in-place mutation would leak routing vars upstream).
func TestApplyEndpoint_DoesNotMutateInputEnv(t *testing.T) {
	t.Parallel()
	in := map[string]string{"FOO": "bar"}
	_, err := applyEndpoint(agent.Spec{
		Env: in,
		Endpoint: &agent.EndpointBinding{
			Company: agent.CompanyAnthropic,
			Host:    agent.HostBedrock,
			Region:  "us-east-1",
		},
	})
	if err != nil {
		t.Fatalf("applyEndpoint: %v", err)
	}
	if len(in) != 1 || in["FOO"] != "bar" {
		t.Errorf("input env mutated: %v", in)
	}
}

// fakeEnvEchoCLI writes a /bin/sh script that simulates the claude CLI's
// stream-json output, echoing the endpoint-routing env vars it received as
// the assistant text so the test can assert the projection survived the
// full spawn path (applyEndpoint → composeEnv → subprocess).
func fakeEnvEchoCLI(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake CLI uses /bin/sh; skip on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-claude.sh")
	script := "#!/bin/sh\n" +
		`printf '{"type":"system","subtype":"init","session_id":"sess-ep-1"}\n'` + "\n" +
		`printf '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"%s|%s"}]}}\n' "$CLAUDE_CODE_USE_BEDROCK" "$AWS_REGION"` + "\n" +
		`printf '{"type":"result","subtype":"success","is_error":false,"num_turns":1}\n'` + "\n"
	// Write WITHOUT the exec bit, then chmod-add it after close (avoids
	// ETXTBSY on fork+exec under parallel test load — see clijsonl's
	// fakeCLI for the full rationale).
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write fake cli: %v", err)
	}
	if err := os.Chmod(path, 0o700); err != nil { //nolint:gosec // test fixture script needs exec bit
		t.Fatalf("chmod fake cli: %v", err)
	}
	return path
}

// TestProvider_Spawn_EndpointEnvReachesSubprocess proves Provider.Spawn
// actually routes a bedrock binding: the CLI knob + derived region must be
// visible in the spawned subprocess environment.
func TestProvider_Spawn_EndpointEnvReachesSubprocess(t *testing.T) {
	t.Parallel()

	cli := fakeEnvEchoCLI(t)
	p, err := New(Options{Binary: cli, LookPath: func(name string) (string, error) { return name, nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	h, err := p.Spawn(t.Context(), agent.Spec{
		Prompt: "echo env",
		Endpoint: &agent.EndpointBinding{
			Company: agent.CompanyAnthropic,
			Host:    agent.HostBedrock,
			Region:  "us-east-1",
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(t.Context()) }()

	deadline := time.After(30 * time.Second)
	var text string
	for text == "" {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				t.Fatal("events channel closed before assistant text")
			}
			if e, isText := ev.(agent.AssistantTextEvent); isText {
				text = e.Text
			}
		case <-deadline:
			t.Fatal("timed out waiting for assistant text")
		}
	}
	if text != "1|us-east-1" {
		t.Errorf("subprocess env projection = %q, want %q", text, "1|us-east-1")
	}
}

// TestProvider_Spawn_EndpointCompanyMismatchFails proves a mis-bound
// endpoint (a company this harness cannot route) fails the spawn loudly
// instead of silently running against the default host.
func TestProvider_Spawn_EndpointCompanyMismatchFails(t *testing.T) {
	t.Parallel()

	cli := fakeEnvEchoCLI(t)
	p, err := New(Options{Binary: cli, LookPath: func(name string) (string, error) { return name, nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = p.Spawn(t.Context(), agent.Spec{
		Prompt:   "x",
		Endpoint: &agent.EndpointBinding{Company: agent.CompanyOpenAI, Host: agent.HostDirect},
	})
	if err == nil {
		t.Fatal("Spawn: want error for non-anthropic endpoint company, got nil")
	}
}
