package afcli

import (
	"testing"

	"github.com/RenseiAI/donmai/afclient"
)

func TestRepoSlugFromRemote(t *testing.T) {
	tests := []struct {
		name, remote, want string
	}{
		{"https with .git", "https://github.com/RenseiAI/donmai.git", "RenseiAI/donmai"},
		{"https no .git", "https://github.com/RenseiAI/donmai", "RenseiAI/donmai"},
		{"ssh form", "git@github.com:RenseiAI/donmai.git", "RenseiAI/donmai"},
		{"ssh no .git", "git@github.com:acme/web", "acme/web"},
		{"trailing slash", "https://github.com/acme/web/", "acme/web"},
		{"empty", "", ""},
		{"garbage", "not-a-url", ""},
		{"host only", "https://github.com/", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := repoSlugFromRemote(tc.remote); got != tc.want {
				t.Errorf("repoSlugFromRemote(%q)=%q want %q", tc.remote, got, tc.want)
			}
		})
	}
}

func TestScopeLabel(t *testing.T) {
	tests := []struct {
		scope string
		all   bool
		want  string
	}{
		{"", false, "all projects"},
		{"", true, "all projects"},
		{"acme/web", false, "acme/web"},
		{"acme/web", true, "all projects"}, // --all overrides scope
	}
	for _, tc := range tests {
		if got := scopeLabel(tc.scope, tc.all); got != tc.want {
			t.Errorf("scopeLabel(%q,%v)=%q want %q", tc.scope, tc.all, got, tc.want)
		}
	}
}

func TestResolveHostWatchURL(t *testing.T) {
	t.Setenv(hostWatchEnvDaemonURL, "")
	if got := resolveHostWatchURL("http://flag:1"); got != "http://flag:1" {
		t.Errorf("flag should win, got %q", got)
	}
	t.Setenv(hostWatchEnvDaemonURL, "http://env:2")
	if got := resolveHostWatchURL(""); got != "http://env:2" {
		t.Errorf("env should be used when no flag, got %q", got)
	}
	if got := resolveHostWatchURL("http://flag:1"); got != "http://flag:1" {
		t.Errorf("flag should still win over env, got %q", got)
	}
}

// TestNewHostWatchCmd_Wiring asserts the command factory builds a usable
// cobra command with the expected flags (a thin smoke over the wiring).
func TestNewHostWatchCmd_Wiring(t *testing.T) {
	cmd := newHostWatchCmd()
	if cmd.Use != "fleet-watch" {
		t.Errorf("Use = %q, want fleet-watch", cmd.Use)
	}
	for _, f := range []string{"project", "all", "replay", "plain", "daemon-url"} {
		if cmd.Flags().Lookup(f) == nil {
			t.Errorf("missing flag --%s", f)
		}
	}
}

// fakeHostWatchSource confirms *afclient.DaemonClient satisfies the
// hostWatchSource interface (compile-time check) and that the interface is
// usable with a fake.
type fakeHostWatchSource struct{}

func (fakeHostWatchSource) GetSessions() ([]afclient.DaemonSessionHandle, error) { return nil, nil }
func (fakeHostWatchSource) GetStatus() (*afclient.DaemonStatusResponse, error)   { return nil, nil }
func (fakeHostWatchSource) GetStats(_, _ bool) (*afclient.DaemonStatsResponse, error) {
	return nil, nil
}

func TestHostWatchSource_Satisfied(t *testing.T) {
	var _ hostWatchSource = (*afclient.DaemonClient)(nil)
	var _ hostWatchSource = fakeHostWatchSource{}
	// Build a command with an injected fake factory to exercise that path.
	cmd := newHostWatchCmdWithSource(func(afclient.DaemonConfig) hostWatchSource {
		return fakeHostWatchSource{}
	})
	if cmd == nil {
		t.Fatal("nil command")
	}
}
