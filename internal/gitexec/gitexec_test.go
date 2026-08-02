package gitexec_test

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/internal/gitexec"
)

// authedRemote is the remote the test credential is minted for, and
// scopedHeaderKey is the git config key HardenedEnv must emit for it.
const (
	authedRemote    = "https://github.com/RenseiAI/donmai.git"
	scopedHeaderKey = "http.https://github.com/RenseiAI/donmai.git.extraHeader"
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
		auth          gitexec.Auth
		wantCount     string // GIT_CONFIG_COUNT
		wantHelperKey bool   // a KEY_n=credential.helper pair present
		wantHeaderVal string // expected scoped extraHeader VALUE, "" means absent
		wantAskpass   bool   // GIT_ASKPASS= present
	}{
		{
			name:          "no flags adds only non-interactive baseline",
			base:          nil,
			suppress:      false,
			auth:          gitexec.Auth{},
			wantCount:     "1",
			wantHelperKey: false,
			wantHeaderVal: "",
			wantAskpass:   false,
		},
		{
			name:          "suppress only injects empty credential.helper",
			base:          nil,
			suppress:      true,
			auth:          gitexec.Auth{},
			wantCount:     "2",
			wantHelperKey: true,
			wantHeaderVal: "",
			wantAskpass:   true,
		},
		{
			name:          "header only injects scoped extraHeader",
			base:          nil,
			suppress:      false,
			auth:          gitexec.Auth{Header: "Authorization: Bearer tok", RemoteURL: authedRemote},
			wantCount:     "2",
			wantHelperKey: false,
			wantHeaderVal: "Authorization: Bearer tok",
			wantAskpass:   false,
		},
		{
			name:          "suppress and header inject two pairs",
			base:          nil,
			suppress:      true,
			auth:          gitexec.Auth{Header: "AUTHORIZATION: basic abc123", RemoteURL: authedRemote},
			wantCount:     "3",
			wantHelperKey: true,
			wantHeaderVal: "AUTHORIZATION: basic abc123",
			wantAskpass:   true,
		},
		{
			name:          "header without a remote is dropped, not emitted unscoped",
			base:          nil,
			suppress:      true,
			auth:          gitexec.Auth{Header: "Authorization: Bearer tok"},
			wantCount:     "2",
			wantHelperKey: true,
			wantHeaderVal: "",
			wantAskpass:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := gitexec.HardenedEnv(tt.base, tt.suppress, tt.auth)
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

			// The bare, unscoped key must appear ONLY as the empty-valued
			// reset of an inherited list. A non-empty value there is the
			// defect this package exists to prevent.
			resetVal, resetOK := configValueForKey(got, "http.extraHeader")
			if !resetOK {
				t.Error("http.extraHeader reset pair absent; an inherited unscoped header would survive")
			}
			if resetVal != "" {
				t.Errorf("unscoped http.extraHeader = %q, want the empty-valued reset", resetVal)
			}

			hdrVal, hdrOK := configValueForKey(got, scopedHeaderKey)
			if tt.wantHeaderVal == "" {
				if hdrOK {
					t.Errorf("%s present (%q), want absent", scopedHeaderKey, hdrVal)
				}
			} else {
				if !hdrOK {
					t.Fatalf("%s absent, want %q", scopedHeaderKey, tt.wantHeaderVal)
				}
				if hdrVal != tt.wantHeaderVal {
					t.Errorf("%s VALUE = %q, want %q", scopedHeaderKey, hdrVal, tt.wantHeaderVal)
				}
			}
		})
	}
}

// configValueForKey resolves the GIT_CONFIG_VALUE_n that pairs with the
// GIT_CONFIG_KEY_n holding key. Returns ("", false) when key is not injected.
//
// A key may legitimately appear more than once (an inherited pair plus our own
// reset, or a static helper plus its suppression), so this scans indices in
// order and returns the HIGHEST-numbered occurrence — the one git resolves.
// Ranging over the map instead would pick an arbitrary one and flake.
func configValueForKey(env []string, key string) (string, bool) {
	m := envMap(env)
	var val string
	var found bool
	for i := 0; ; i++ {
		k, ok := m[fmt.Sprintf("GIT_CONFIG_KEY_%d", i)]
		if !ok {
			return val, found
		}
		if k != key {
			continue
		}
		if v, ok := m[fmt.Sprintf("GIT_CONFIG_VALUE_%d", i)]; ok {
			val, found = v, true
		}
	}
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
	got := gitexec.HardenedEnv(base, true, gitexec.Auth{Header: "Authorization: Bearer tok", RemoteURL: authedRemote})
	m := envMap(got)

	// Three new pairs appended → count becomes 5.
	if m["GIT_CONFIG_COUNT"] != "5" {
		t.Fatalf("GIT_CONFIG_COUNT = %q, want 5", m["GIT_CONFIG_COUNT"])
	}
	// Pre-existing pairs must be untouched.
	if m["GIT_CONFIG_KEY_0"] != "user.name" || m["GIT_CONFIG_VALUE_0"] != "Bot" {
		t.Errorf("pre-existing pair 0 clobbered: %q=%q", m["GIT_CONFIG_KEY_0"], m["GIT_CONFIG_VALUE_0"])
	}
	if m["GIT_CONFIG_KEY_1"] != "user.email" || m["GIT_CONFIG_VALUE_1"] != "bot@example.test" {
		t.Errorf("pre-existing pair 1 clobbered: %q=%q", m["GIT_CONFIG_KEY_1"], m["GIT_CONFIG_VALUE_1"])
	}
	// New pairs land at 2, 3 and 4. The unscoped reset MUST precede the
	// scoped key, otherwise it would clear it again.
	if m["GIT_CONFIG_KEY_2"] != "http.extraHeader" || m["GIT_CONFIG_VALUE_2"] != "" {
		t.Errorf("new pair 2 = %q=%q, want http.extraHeader=<empty reset>", m["GIT_CONFIG_KEY_2"], m["GIT_CONFIG_VALUE_2"])
	}
	if m["GIT_CONFIG_KEY_3"] != "credential.helper" || m["GIT_CONFIG_VALUE_3"] != "" {
		t.Errorf("new pair 3 = %q=%q, want credential.helper=<empty>", m["GIT_CONFIG_KEY_3"], m["GIT_CONFIG_VALUE_3"])
	}
	if m["GIT_CONFIG_KEY_4"] != scopedHeaderKey || m["GIT_CONFIG_VALUE_4"] != "Authorization: Bearer tok" {
		t.Errorf("new pair 4 = %q=%q, want %s=<header>", m["GIT_CONFIG_KEY_4"], m["GIT_CONFIG_VALUE_4"], scopedHeaderKey)
	}
}

func TestHardenedEnvMalformedExistingCount(t *testing.T) {
	t.Parallel()

	// A malformed upstream count must not panic and must not produce a
	// negative/garbage index. We treat it as 0 and number from there.
	base := []string{"GIT_CONFIG_COUNT=not-a-number"}
	got := gitexec.HardenedEnv(base, true, gitexec.Auth{})
	m := envMap(got)
	if m["GIT_CONFIG_COUNT"] != "2" {
		t.Fatalf("GIT_CONFIG_COUNT = %q, want 2", m["GIT_CONFIG_COUNT"])
	}
	if m["GIT_CONFIG_KEY_0"] != "http.extraHeader" {
		t.Errorf("GIT_CONFIG_KEY_0 = %q, want http.extraHeader", m["GIT_CONFIG_KEY_0"])
	}
	if m["GIT_CONFIG_KEY_1"] != "credential.helper" {
		t.Errorf("GIT_CONFIG_KEY_1 = %q, want credential.helper", m["GIT_CONFIG_KEY_1"])
	}
}

func TestHardenedEnvDoesNotMutateInput(t *testing.T) {
	t.Parallel()

	base := []string{"PATH=/usr/bin", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=a", "GIT_CONFIG_VALUE_0=b"}
	snapshot := append([]string(nil), base...)
	_ = gitexec.HardenedEnv(base, true, gitexec.Auth{Header: "Authorization: Bearer secret", RemoteURL: authedRemote})
	for i := range base {
		if base[i] != snapshot[i] {
			t.Fatalf("input mutated at %d: %q != %q", i, base[i], snapshot[i])
		}
	}
}

func TestHardenedEnvSecretNeverInKeyName(t *testing.T) {
	t.Parallel()

	const secret = "ghp_SUPERSECRETTOKEN1234567890"
	got := gitexec.HardenedEnv(nil, true, gitexec.Auth{Header: "Authorization: Bearer " + secret, RemoteURL: authedRemote})
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

func TestExtraHeaderConfigKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    string
		wantOK  bool
		comment string
	}{
		{
			name:   "https remote",
			in:     "https://github.com/org/repo.git",
			want:   "http.https://github.com/org/repo.git.extraHeader",
			wantOK: true,
		},
		{
			name:   "userinfo is stripped so one key covers both spellings",
			in:     "https://x-access-token:ghp_tok@github.com/org/repo.git",
			want:   "http.https://github.com/org/repo.git.extraHeader",
			wantOK: true,
		},
		{
			name:   "explicit port is preserved",
			in:     "https://ghe.example.com:8443/org/repo.git",
			want:   "http.https://ghe.example.com:8443/org/repo.git.extraHeader",
			wantOK: true,
		},
		{
			name:   "plain http remote",
			in:     "http://host.local/repo.git",
			want:   "http.http://host.local/repo.git.extraHeader",
			wantOK: true,
		},
		{
			name:   "query and fragment are dropped",
			in:     "https://github.com/org/repo.git?x=1#frag",
			want:   "http.https://github.com/org/repo.git.extraHeader",
			wantOK: true,
		},
		{
			name:   "surrounding whitespace is tolerated",
			in:     "  https://github.com/org/repo.git\t",
			want:   "http.https://github.com/org/repo.git.extraHeader",
			wantOK: true,
		},
		{name: "empty is unscopable", in: "", wantOK: false},
		{name: "scp-style ssh remote is unscopable", in: "git@github.com:org/repo.git", wantOK: false},
		{name: "ssh url is unscopable", in: "ssh://git@github.com/org/repo.git", wantOK: false},
		{name: "git protocol is unscopable", in: "git://github.com/org/repo.git", wantOK: false},
		{name: "local path is unscopable", in: "/srv/git/repo.git", wantOK: false},
		{name: "file url is unscopable", in: "file:///srv/git/repo.git", wantOK: false},
		{name: "hostless http url is unscopable", in: "https:///org/repo.git", wantOK: false},
		{name: "control character is unscopable", in: "https://github.com/org/re\npo.git", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := gitexec.ExtraHeaderConfigKey(tt.in)
			if ok != tt.wantOK {
				t.Fatalf("ExtraHeaderConfigKey(%q) ok = %v, want %v (got key %q)", tt.in, ok, tt.wantOK, got)
			}
			if ok && got != tt.want {
				t.Errorf("ExtraHeaderConfigKey(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if !ok && got != "" {
				t.Errorf("ExtraHeaderConfigKey(%q) returned key %q alongside ok=false, want empty", tt.in, got)
			}
		})
	}
}

// TestHardenedEnvNeverEmitsUnscopedHeader is the regression guard for the
// defect this scoping exists to prevent: an unscoped `http.extraHeader`
// attaches the credential to EVERY https remote git touches — including an
// anonymous clone of an unrelated public repository, which then fails with
// "Invalid username or token" precisely BECAUSE a credential was attached.
// A remote we cannot scope to must yield no header at all, never a bare key.
func TestHardenedEnvNeverEmitsUnscopedHeader(t *testing.T) {
	t.Parallel()

	const secret = "ghp_UNSCOPABLE_TOKEN_0987654321"
	unscopable := []string{
		"",
		"git@github.com:org/repo.git",
		"ssh://git@github.com/org/repo.git",
		"/srv/git/repo.git",
		"file:///srv/git/repo.git",
	}

	for _, remote := range unscopable {
		t.Run("remote="+remote, func(t *testing.T) {
			t.Parallel()
			got := gitexec.HardenedEnv(nil, true, gitexec.Auth{
				Header:    "Authorization: Bearer " + secret,
				RemoteURL: remote,
			})
			if v, _ := configValueForKey(got, "http.extraHeader"); v != "" {
				t.Errorf("unscoped http.extraHeader = %q for remote %q, want the empty reset", v, remote)
			}
			for _, e := range got {
				if strings.Contains(e, secret) {
					t.Fatalf("secret reached the env for unscopable remote %q via %q", remote, strings.SplitN(e, "=", 2)[0])
				}
			}
			// The credential-helper suppression is unaffected: it can only
			// ever remove a credential, never misattach one.
			if v, ok := configValueForKey(got, "credential.helper"); !ok || v != "" {
				t.Errorf("credential.helper = %q present=%v, want empty+present", v, ok)
			}
		})
	}
}

// TestHardenedEnvHeaderScopeAgainstRealGit is the end-to-end proof, using the
// real git binary's own URL matcher rather than our reading of its docs: the
// header HardenedEnv emits resolves for the remote it authenticates and for
// the smart-HTTP endpoints underneath it, and does NOT resolve for a different
// repository on the same host, a different host, or a different scheme.
//
// The same-host case is the one host-only scoping (e.g. actions/checkout's
// `http.https://github.com/.extraheader`) would NOT catch, and it is the case
// that broke a SwiftPM checkout of a public github.com repository.
func TestHardenedEnvHeaderScopeAgainstRealGit(t *testing.T) {
	t.Parallel()

	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not available: %v", err)
	}

	const header = "AUTHORIZATION: basic dGVzdC10b2tlbg=="
	env := gitexec.HardenedEnv(nil, true, gitexec.Auth{Header: header, RemoteURL: authedRemote})
	// Isolate the probe from the developer's/CI's own git configuration so the
	// only source of http.* config is the env HardenedEnv just built.
	home := t.TempDir()
	env = append(env,
		"HOME="+home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+filepath.Join(home, "nonexistent-gitconfig"),
	)

	tests := []struct {
		name       string
		requestURL string
		wantHeader bool
	}{
		{name: "the authenticated remote itself", requestURL: authedRemote, wantHeader: true},
		{name: "its ref-discovery endpoint", requestURL: authedRemote + "/info/refs?service=git-upload-pack", wantHeader: true},
		{name: "its receive-pack endpoint (push)", requestURL: authedRemote + "/git-receive-pack", wantHeader: true},
		{name: "a different repo on the same host", requestURL: "https://github.com/migueldeicaza/SwiftTerm.git", wantHeader: false},
		{name: "a sibling repo in the same org", requestURL: "https://github.com/RenseiAI/another-repo.git", wantHeader: false},
		{name: "the host root", requestURL: "https://github.com/", wantHeader: false},
		{name: "a different host", requestURL: "https://gitlab.com/RenseiAI/donmai.git", wantHeader: false},
		{name: "a lookalike host", requestURL: "https://github.com.evil.example/RenseiAI/donmai.git", wantHeader: false},
		{name: "a different scheme", requestURL: "http://github.com/RenseiAI/donmai.git", wantHeader: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// git config --get-urlmatch applies git's own http.<url>.* matching
			// rules and prints the winning value, exiting 1 when nothing matches.
			// nolint:gosec // G204: gitBin is exec.LookPath("git"); every
			// argument is a constant from the table above.
			cmd := exec.Command(gitBin, "config", "--get-urlmatch", "http.extraHeader", tt.requestURL)
			cmd.Env = env
			cmd.Dir = home
			out, runErr := cmd.Output()
			got := strings.TrimSpace(string(out))

			if !tt.wantHeader {
				if got != "" {
					t.Fatalf("git would send a header to %s: %q — the credential must not leave its remote", tt.requestURL, got)
				}
				return
			}
			if runErr != nil {
				t.Fatalf("git config --get-urlmatch for %s: %v (output %q)", tt.requestURL, runErr, got)
			}
			if got != header {
				t.Errorf("header for %s = %q, want %q", tt.requestURL, got, header)
			}
		})
	}
}

// TestHardenedEnvNeutralisesInheritedUnscopedHeader is the regression guard for
// the second half of the defect: this process may itself have been started with
// an unscoped http.extraHeader in its environment (that is how the original
// incident propagated), and passing it through would let a stale credential
// outrank whatever the invocation actually intends to use.
//
// It must be neutralised even when the invocation supplies no auth of its own —
// a `git push` that should authenticate via the remote's own credential is
// exactly the case that fails when a stale unscoped header is present.
func TestHardenedEnvNeutralisesInheritedUnscopedHeader(t *testing.T) {
	t.Parallel()

	inherited := []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=AUTHORIZATION: basic c3RhbGUtdG9rZW4=",
	}

	for _, tc := range []struct {
		name string
		auth gitexec.Auth
	}{
		{name: "no auth of our own", auth: gitexec.Auth{}},
		{name: "our own scoped auth", auth: gitexec.Auth{Header: "Authorization: Bearer fresh", RemoteURL: authedRemote}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := gitexec.HardenedEnv(inherited, false, tc.auth)
			m := envMap(got)

			// The inherited pair is preserved in place (the numbering contract
			// never rewrites what the caller supplied) …
			if m["GIT_CONFIG_KEY_0"] != "http.extraHeader" {
				t.Fatalf("inherited pair 0 clobbered: %q", m["GIT_CONFIG_KEY_0"])
			}
			// … but a later empty-valued reset must supersede it. Config order
			// is what makes that true, so assert on ordering, not just presence.
			resetIdx := -1
			for i := 0; ; i++ {
				key, ok := m[fmt.Sprintf("GIT_CONFIG_KEY_%d", i)]
				if !ok {
					break
				}
				if key == "http.extraHeader" && m[fmt.Sprintf("GIT_CONFIG_VALUE_%d", i)] == "" {
					resetIdx = i
				}
			}
			if resetIdx <= 0 {
				t.Fatalf("no empty-valued http.extraHeader reset after the inherited pair (index %d): %v", resetIdx, got)
			}
			if count := m["GIT_CONFIG_COUNT"]; count <= fmt.Sprint(resetIdx) && count != "" {
				// The reset is only visible to git if it is within the count.
				if n, err := strconv.Atoi(count); err != nil || n <= resetIdx {
					t.Fatalf("GIT_CONFIG_COUNT = %q does not cover the reset at index %d", count, resetIdx)
				}
			}
		})
	}
}

// TestHardenedEnvInheritedHeaderNotSentByRealGit closes the loop with the real
// git binary: given an inherited unscoped header, git must send NOTHING to an
// unrelated remote and the caller's own scoped header to its own remote.
func TestHardenedEnvInheritedHeaderNotSentByRealGit(t *testing.T) {
	t.Parallel()

	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not available: %v", err)
	}

	const ourHeader = "AUTHORIZATION: basic b3Vycy1mcmVzaA=="
	env := gitexec.HardenedEnv([]string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=AUTHORIZATION: basic c3RhbGUtdG9rZW4=",
	}, false, gitexec.Auth{Header: ourHeader, RemoteURL: authedRemote})

	home := t.TempDir()
	env = append(env,
		"HOME="+home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+filepath.Join(home, "nonexistent-gitconfig"),
	)

	for _, tc := range []struct {
		name       string
		requestURL string
		want       string
	}{
		{name: "our own remote gets our own header", requestURL: authedRemote, want: ourHeader},
		{name: "an unrelated public repo gets nothing", requestURL: "https://github.com/migueldeicaza/SwiftTerm.git", want: ""},
		{name: "an unrelated host gets nothing", requestURL: "https://gitlab.com/x/y.git", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// nolint:gosec // G204: gitBin is exec.LookPath("git"); every
			// argument is a constant from the table above.
			cmd := exec.Command(gitBin, "config", "--get-urlmatch", "http.extraHeader", tc.requestURL)
			cmd.Env = env
			cmd.Dir = home
			out, _ := cmd.Output()
			if got := strings.TrimSpace(string(out)); got != tc.want {
				t.Errorf("header git would send to %s = %q, want %q", tc.requestURL, got, tc.want)
			}
		})
	}
}
