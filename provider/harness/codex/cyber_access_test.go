package codex

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

func TestResolveCyberAccessProgram(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		config  map[string]any
		want    string
		wantErr string
	}{
		{name: "no provider config", config: nil},
		{name: "other keys only", config: map[string]any{"serviceTier": "fast"}},
		{name: "explicit null", config: map[string]any{"cyberAccessProgram": nil}},
		{name: "standard", config: map[string]any{"cyberAccessProgram": "standard"}, want: "standard"},
		{name: "daybreak blue", config: map[string]any{"cyberAccessProgram": "daybreakBlue"}, want: "daybreakBlue"},
		{name: "daybreak red", config: map[string]any{"cyberAccessProgram": "daybreakRed"}, want: "daybreakRed"},
		{name: "boolean is refused", config: map[string]any{"cyberAccessProgram": true}, wantErr: "must be a string"},
		{name: "snake case is refused", config: map[string]any{"cyberAccessProgram": "daybreak_blue"}, wantErr: "is not one of"},
		{name: "empty string is refused", config: map[string]any{"cyberAccessProgram": ""}, wantErr: "is not one of"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveCyberAccessProgram(agent.Spec{ProviderConfig: tt.config})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tt.want {
				t.Fatalf("program = %q, want %q", got, tt.want)
			}
		})
	}
}

// The declared program rides turn/start only: thread/start carries no
// config override for it, and an undeclared session's turn is unchanged.
func TestNewSpawnPlan_CyberAccessProgramRidesTurnStartOnly(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		config map[string]any
		want   any
	}{
		{name: "declared", config: map[string]any{"cyberAccessProgram": "daybreakBlue", "context": 1000}, want: "daybreakBlue"},
		{name: "undeclared", config: map[string]any{"context": 1000}, want: nil},
		{name: "none", config: nil, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			plan := NewSpawnPlan(agent.Spec{Cwd: "/tmp", Model: "m", ProviderConfig: tt.config})
			got, ok := plan.TurnStart["cyberAccessProgram"]
			if tt.want == nil {
				if ok {
					t.Fatalf("cyberAccessProgram = %v, want absent", got)
				}
			} else if got != tt.want {
				t.Fatalf("cyberAccessProgram = %v, want %v", got, tt.want)
			}
			if _, ok := plan.ThreadStart["cyberAccessProgram"]; ok {
				t.Fatal("thread/start must not carry cyberAccessProgram")
			}
			if _, ok := plan.ThreadStart["config"]; ok {
				t.Fatal("thread/start must not carry a config override")
			}
		})
	}
}

func TestIgnoredSpecFields_ProviderConfigCyberAccessIsConsumed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		config     map[string]any
		wantIgnore bool
	}{
		{name: "only the consumed key", config: map[string]any{"cyberAccessProgram": "daybreakBlue"}, wantIgnore: false},
		{name: "consumed plus unknown", config: map[string]any{"cyberAccessProgram": "daybreakBlue", "x": 1}, wantIgnore: true},
		{name: "unknown only", config: map[string]any{"x": 1}, wantIgnore: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ignored := false
			for _, n := range ignoredSpecFields(agent.Spec{ProviderConfig: tt.config}) {
				if n.Field == "ProviderConfig" {
					if ignored {
						t.Fatal("ProviderConfig noted more than once")
					}
					ignored = true
				}
			}
			if ignored != tt.wantIgnore {
				t.Fatalf("ProviderConfig ignored = %v, want %v", ignored, tt.wantIgnore)
			}
		})
	}
}

// End to end over the fake app-server: the declared program reaches the
// wire on turn/start, and an invalid declaration fails the spawn before
// any thread is opened.
func TestSpawn_CyberAccessProgramOnTheWire(t *testing.T) {
	t.Parallel()
	t.Run("declared program reaches turn/start", func(t *testing.T) {
		t.Parallel()
		p, fs := newTestProvider(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		h, err := p.Spawn(ctx, agent.Spec{
			Prompt: "review", Cwd: "/tmp/wt", Autonomous: true,
			ProviderConfig: map[string]any{"cyberAccessProgram": "daybreakBlue"},
		})
		if err != nil {
			t.Fatalf("Spawn: %v", err)
		}
		_ = drainEvents(t, h.Events(), 5*time.Second)
		fs.mu.Lock()
		defer fs.mu.Unlock()
		found := false
		for i, m := range fs.methods {
			switch m {
			case "turn/start":
				found = true
				if got := fs.params[i]["cyberAccessProgram"]; got != "daybreakBlue" {
					t.Fatalf("turn/start cyberAccessProgram = %v, want daybreakBlue", got)
				}
			case "thread/start":
				if _, ok := fs.params[i]["config"]; ok {
					t.Fatal("thread/start must not carry a config override")
				}
			}
		}
		if !found {
			t.Fatalf("no turn/start sent: %v", fs.methods)
		}
	})
	t.Run("invalid declaration fails the spawn", func(t *testing.T) {
		t.Parallel()
		p, fs := newTestProvider(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := p.Spawn(ctx, agent.Spec{
			Prompt: "review", Cwd: "/tmp/wt", Autonomous: true,
			ProviderConfig: map[string]any{"cyberAccessProgram": "daybreak_blue"},
		})
		if !errors.Is(err, agent.ErrSpawnFailed) {
			t.Fatalf("Spawn err = %v, want ErrSpawnFailed", err)
		}
		fs.mu.Lock()
		defer fs.mu.Unlock()
		for _, m := range fs.methods {
			if m == "thread/start" || m == "turn/start" {
				t.Fatalf("%s sent despite an invalid declaration: %v", m, fs.methods)
			}
		}
	})
}

func TestBuildInteractiveLaunch_RefusesDeclaredCyberAccessProgram(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		config  map[string]any
		refused bool
	}{
		{name: "declared", config: map[string]any{"cyberAccessProgram": "daybreakBlue"}, refused: true},
		{name: "undeclared", config: map[string]any{"context": 1000}, refused: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := buildInteractiveLaunchEnv(agent.Spec{Cwd: t.TempDir(), ProviderConfig: tt.config}, func(string) string { return "" })
			refused := err != nil && strings.Contains(err.Error(), "cyberAccessProgram")
			if refused != tt.refused {
				t.Fatalf("refused = %v (err %v), want %v", refused, err, tt.refused)
			}
		})
	}
}
