package gitexec_test

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/internal/gitexec"
)

// envMap turns a KEY=VALUE slice into a map for assertion convenience. When a
// key repeats, the LAST value wins — matching how execve resolves a duplicated
// key in a process environment.
func envMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, e := range env {
		k, v, ok := strings.Cut(e, "=")
		if !ok {
			continue
		}
		m[k] = v
	}
	return m
}

func TestHardenedEnvBaseline(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		base          []string
		suppress      bool
		authHeader    string
		wantCount     string // GIT_CONFIG_COUNT, "" means absent
		wantHelperKey bool   // a KEY_n=credential.helper pair present
		wantHeaderVal string // expected http.extraHeader VALUE, "" means absent
		wantAskpass   bool   // GIT_ASKPASS= present
	}{
		{
			name:          "no flags adds only non-interactive baseline",
			base:          nil,
			suppress:      false,
			authHeader:    "",
			wantCount:     "",
			wantHelperKey: false,
			wantHeaderVal: "",
			wantAskpass:   false,
		},
		{
			name:          "suppress only injects empty credential.helper",
			base:          nil,
			suppress:      true,
			authHeader:    "",
			wantCount:     "1",
			wantHelperKey: true,
			wantHeaderVal: "",
			wantAskpass:   true,
		},
		{
			name:          "header only injects extraHeader",
			base:          nil,
			suppress:      false,
			authHeader:    "Authorization: Bearer tok",
			wantCount:     "1",
			wantHelperKey: false,
			wantHeaderVal: "Authorization: Bearer tok",
			wantAskpass:   false,
		},
		{
			name:          "suppress and header inject two pairs",
			base:          nil,
			suppress:      true,
			authHeader:    "AUTHORIZATION: basic abc123",
			wantCount:     "2",
			wantHelperKey: true,
			wantHeaderVal: "AUTHORIZATION: basic abc123",
			wantAskpass:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := gitexec.HardenedEnv(tt.base, tt.suppress, tt.authHeader)
			m := envMap(got)

			if m["GIT_TERMINAL_PROMPT"] != "0" {
				t.Errorf("GIT_TERMINAL_PROMPT = %q, want 0", m["GIT_TERMINAL_PROMPT"])
			}
			if m["GCM_INTERACTIVE"] != "never" {
				t.Errorf("GCM_INTERACTIVE = %q, want never", m["GCM_INTERACTIVE"])
			}

			gotCount, hasCount := m["GIT_CONFIG_COUNT"]
			if tt.wantCount == "" {
				if hasCount {
					t.Errorf("GIT_CONFIG_COUNT present (%q), want absent", gotCount)
				}
			} else if gotCount != tt.wantCount {
				t.Errorf("GIT_CONFIG_COUNT = %q, want %q", gotCount, tt.wantCount)
			}

			_, hasAskpass := m["GIT_ASKPASS"]
			if hasAskpass != tt.wantAskpass {
				t.Errorf("GIT_ASKPASS present = %v, want %v", hasAskpass, tt.wantAskpass)
			}
			if tt.wantAskpass && m["GIT_ASKPASS"] != "" {
				t.Errorf("GIT_ASKPASS = %q, want empty", m["GIT_ASKPASS"])
			}

			helperVal, helperOK := configValueForKey(got, "credential.helper")
			if helperOK != tt.wantHelperKey {
				t.Errorf("credential.helper pair present = %v, want %v", helperOK, tt.wantHelperKey)
			}
			if tt.wantHelperKey && helperVal != "" {
				t.Errorf("credential.helper VALUE = %q, want empty", helperVal)
			}

			hdrVal, hdrOK := configValueForKey(got, "http.extraHeader")
			if tt.wantHeaderVal == "" {
				if hdrOK {
					t.Errorf("http.extraHeader present (%q), want absent", hdrVal)
				}
			} else {
				if !hdrOK {
					t.Fatalf("http.extraHeader absent, want %q", tt.wantHeaderVal)
				}
				if hdrVal != tt.wantHeaderVal {
					t.Errorf("http.extraHeader VALUE = %q, want %q", hdrVal, tt.wantHeaderVal)
				}
			}
		})
	}
}

// configValueForKey resolves the GIT_CONFIG_VALUE_n that pairs with the
// GIT_CONFIG_KEY_n holding key. Returns ("", false) when key is not injected.
func configValueForKey(env []string, key string) (string, bool) {
	m := envMap(env)
	for k, v := range m {
		if !strings.HasPrefix(k, "GIT_CONFIG_KEY_") {
			continue
		}
		if v != key {
			continue
		}
		n := strings.TrimPrefix(k, "GIT_CONFIG_KEY_")
		val, ok := m["GIT_CONFIG_VALUE_"+n]
		return val, ok
	}
	return "", false
}

func TestHardenedEnvContinuesExistingCount(t *testing.T) {
	t.Parallel()

	// Caller pre-seeded two config pairs (indices 0,1) and a count of 2.
	base := []string{
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=user.name",
		"GIT_CONFIG_VALUE_0=Bot",
		"GIT_CONFIG_KEY_1=user.email",
		"GIT_CONFIG_VALUE_1=bot@example.test",
	}
	got := gitexec.HardenedEnv(base, true, "Authorization: Bearer tok")
	m := envMap(got)

	// Two new pairs appended → count becomes 4.
	if m["GIT_CONFIG_COUNT"] != "4" {
		t.Fatalf("GIT_CONFIG_COUNT = %q, want 4", m["GIT_CONFIG_COUNT"])
	}
	// Pre-existing pairs must be untouched.
	if m["GIT_CONFIG_KEY_0"] != "user.name" || m["GIT_CONFIG_VALUE_0"] != "Bot" {
		t.Errorf("pre-existing pair 0 clobbered: %q=%q", m["GIT_CONFIG_KEY_0"], m["GIT_CONFIG_VALUE_0"])
	}
	if m["GIT_CONFIG_KEY_1"] != "user.email" || m["GIT_CONFIG_VALUE_1"] != "bot@example.test" {
		t.Errorf("pre-existing pair 1 clobbered: %q=%q", m["GIT_CONFIG_KEY_1"], m["GIT_CONFIG_VALUE_1"])
	}
	// New pairs land at 2 and 3.
	if m["GIT_CONFIG_KEY_2"] != "credential.helper" || m["GIT_CONFIG_VALUE_2"] != "" {
		t.Errorf("new pair 2 = %q=%q, want credential.helper=<empty>", m["GIT_CONFIG_KEY_2"], m["GIT_CONFIG_VALUE_2"])
	}
	if m["GIT_CONFIG_KEY_3"] != "http.extraHeader" || m["GIT_CONFIG_VALUE_3"] != "Authorization: Bearer tok" {
		t.Errorf("new pair 3 = %q=%q, want http.extraHeader=<header>", m["GIT_CONFIG_KEY_3"], m["GIT_CONFIG_VALUE_3"])
	}
}

func TestHardenedEnvMalformedExistingCount(t *testing.T) {
	t.Parallel()

	// A malformed upstream count must not panic and must not produce a
	// negative/garbage index. We treat it as 0 and number from there.
	base := []string{"GIT_CONFIG_COUNT=not-a-number"}
	got := gitexec.HardenedEnv(base, true, "")
	m := envMap(got)
	if m["GIT_CONFIG_COUNT"] != "1" {
		t.Fatalf("GIT_CONFIG_COUNT = %q, want 1", m["GIT_CONFIG_COUNT"])
	}
	if m["GIT_CONFIG_KEY_0"] != "credential.helper" {
		t.Errorf("GIT_CONFIG_KEY_0 = %q, want credential.helper", m["GIT_CONFIG_KEY_0"])
	}
}

func TestHardenedEnvDoesNotMutateInput(t *testing.T) {
	t.Parallel()

	base := []string{"PATH=/usr/bin", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=a", "GIT_CONFIG_VALUE_0=b"}
	snapshot := append([]string(nil), base...)
	_ = gitexec.HardenedEnv(base, true, "Authorization: Bearer secret")
	for i := range base {
		if base[i] != snapshot[i] {
			t.Fatalf("input mutated at %d: %q != %q", i, base[i], snapshot[i])
		}
	}
}

func TestHardenedEnvSecretNeverInKeyName(t *testing.T) {
	t.Parallel()

	const secret = "ghp_SUPERSECRETTOKEN1234567890"
	got := gitexec.HardenedEnv(nil, true, "Authorization: Bearer "+secret)
	for _, e := range got {
		k, v, ok := strings.Cut(e, "=")
		if !ok {
			continue
		}
		if strings.Contains(k, secret) {
			t.Fatalf("secret leaked into env KEY name: %q", k)
		}
		// The secret may only appear in a GIT_CONFIG_VALUE_n position.
		if strings.Contains(v, secret) && !strings.HasPrefix(k, "GIT_CONFIG_VALUE_") {
			t.Fatalf("secret leaked into non-VALUE key %q", k)
		}
	}
}

func TestCleanURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		in        string
		wantClean string
		wantStrip bool
	}{
		{
			name:      "https with userinfo is stripped",
			in:        "https://x-access-token:ghp_tok@github.com/org/repo.git",
			wantClean: "https://github.com/org/repo.git",
			wantStrip: true,
		},
		{
			name:      "https without userinfo unchanged",
			in:        "https://github.com/org/repo.git",
			wantClean: "https://github.com/org/repo.git",
			wantStrip: false,
		},
		{
			name:      "http with userinfo is stripped",
			in:        "http://user:pw@host.local/repo.git",
			wantClean: "http://host.local/repo.git",
			wantStrip: true,
		},
		{
			name:      "ssh scp-style left untouched",
			in:        "git@github.com:org/repo.git",
			wantClean: "git@github.com:org/repo.git",
			wantStrip: false,
		},
		{
			name:      "ssh url scheme left untouched",
			in:        "ssh://git@github.com/org/repo.git",
			wantClean: "ssh://git@github.com/org/repo.git",
			wantStrip: false,
		},
		{
			name:      "local path left untouched",
			in:        "/srv/git/repo.git",
			wantClean: "/srv/git/repo.git",
			wantStrip: false,
		},
		{
			name:      "empty left untouched",
			in:        "",
			wantClean: "",
			wantStrip: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotClean, gotStrip := gitexec.CleanURL(tt.in)
			if gotClean != tt.wantClean {
				t.Errorf("CleanURL(%q) clean = %q, want %q", tt.in, gotClean, tt.wantClean)
			}
			if gotStrip != tt.wantStrip {
				t.Errorf("CleanURL(%q) stripped = %v, want %v", tt.in, gotStrip, tt.wantStrip)
			}
		})
	}
}
