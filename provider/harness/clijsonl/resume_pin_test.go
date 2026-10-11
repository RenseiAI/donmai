package clijsonl

import (
	"slices"
	"testing"
)

// The driver admits only a well-formed routing pin (--settings + a
// destinations-only envelope) onto the --resume turn. These tests pin the
// filter itself: a dangling flag, a foreign flag, an envelope carrying
// hooks or non-destination names, and a split-pair smuggle must all drop,
// while the exact pin pair passes through byte-identical.

func TestResumeArgv_EmptyExtrasKeepByteIdenticalShape(t *testing.T) {
	t.Parallel()

	argv, stdin := resumeArgv("sess-1", "follow up", nil)
	want := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--dangerously-skip-permissions",
		"--resume", "sess-1",
	}
	if !slices.Equal(argv, want) {
		t.Fatalf("resumeArgv with no extras = %q, want the byte-identical %q", argv, want)
	}
	if stdin != "follow up" {
		t.Fatalf("resume stdin = %q, want the prompt (never argv)", stdin)
	}
}

func TestResumeArgv_PinPairRidesBetweenFlagsAndResume(t *testing.T) {
	t.Parallel()

	pin := `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:1/v1"}}`
	argv, _ := resumeArgv("sess-1", "follow up", []string{"--settings", pin})
	settings, ok := adjacentValue(argv, "--settings")
	if !ok || settings != pin {
		t.Fatalf("resume argv = %q, want the --settings pin pair carried verbatim", argv)
	}
	if got := argv[len(argv)-2:]; !slices.Equal(got, []string{"--resume", "sess-1"}) {
		t.Fatalf("resume argv tail = %q, want --resume <id> last (argv: %q)", got, argv)
	}
}

func TestResumeArgv_DropsNonPinFragments(t *testing.T) {
	t.Parallel()

	pin := `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:1/v1"}}`
	cases := map[string][]string{
		"dangling flag":        {"--settings"},
		"foreign flag":         {"--model", "opus"},
		"foreign flag + pin":   {"--model", "opus", "--settings", pin},
		"pin + trailing flag":  {"--settings", pin, "--model", "opus"},
		"pin + trailing value": {"--settings", pin, "stray"},
		"two settings":         {"--settings", pin, "--settings", pin},
		"empty settings value": {"--settings", ""},
		"non-JSON value":       {"--settings", "not-json"},
		// A pin whose session base URL is blank is not the pin: the
		// driver's guard cannot tell it from a caller smuggling an empty
		// override, so it vetoes. (Fallback blanks ride only beside a
		// non-empty session destination; see TestIsRoutingPinJSON.)
		"blank base URL with fallback blanks": {
			"--settings",
			`{"env":{"ANTHROPIC_BASE_URL":"","HTTPS_PROXY":""}}`,
		},
		"hooks envelope": {
			"--settings",
			`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/bin/evil"}]}]},"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:1/v1"}}`,
		},
		"bearer in envelope": {
			"--settings",
			`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:1/v1","ANTHROPIC_AUTH_TOKEN":"sess-bearer"}}`,
		},
		"unrelated name in envelope": {
			"--settings",
			`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:1/v1","UNRELATED_LURKER":"x"}}`,
		},
		"empty value in envelope": {
			"--settings",
			`{"env":{"ANTHROPIC_BASE_URL":""}}`,
		},
		"empty env in envelope": {"--settings", `{"env":{}}`},
		"no env in envelope":    {"--settings", `{"model":"opus"}`},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			argv, _ := resumeArgv("sess-1", "follow up", extra)
			if slices.Contains(argv, "--settings") {
				t.Fatalf("resume argv admits %q: %q — only the destinations-only pin pair may ride", extra, argv)
			}
		})
	}
}

func TestIsRoutingPinJSON(t *testing.T) {
	t.Parallel()

	good := []string{
		`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:1/v1"}}`,
		`{"env":{"CLAUDE_CODE_USE_BEDROCK":"1","AWS_REGION":"us-east-1"}}`,
		`{"env":{"CLAUDE_CODE_USE_VERTEX":"1","CLOUD_ML_REGION":"us-central1"}}`,
		// The widened pin states fallback blanks alongside the session's
		// own routing: a blank holds the ambient default on the winning
		// layer so a repo naming a proxy or flipping a provider switch
		// loses to it.
		`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:1/v1","CLAUDE_CODE_USE_BEDROCK":"","CLAUDE_CODE_USE_VERTEX":"","CLAUDE_CODE_USE_FOUNDRY":"","ANTHROPIC_BEDROCK_BASE_URL":"","ANTHROPIC_VERTEX_BASE_URL":"","ANTHROPIC_FOUNDRY_BASE_URL":"","CLAUDE_CODE_USE_ANTHROPIC_AWS":"","CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD":"","CLAUDE_CODE_USE_MANTLE":"","CLAUDE_CODE_USE_GATEWAY":"","ANTHROPIC_AWS_BASE_URL":"","ANTHROPIC_GOOGLE_CLOUD_BASE_URL":"","ANTHROPIC_BEDROCK_MANTLE_BASE_URL":"","ANTHROPIC_UNIX_SOCKET":"","HTTP_PROXY":"","HTTPS_PROXY":"","ALL_PROXY":"","http_proxy":"","https_proxy":"","all_proxy":""}}`,
		`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:1/v1","HTTPS_PROXY":"http://proxy.internal:8080"}}`,
	}
	for _, raw := range good {
		if !isRoutingPinJSON(raw) {
			t.Errorf("isRoutingPinJSON(%q) = false, want true", raw)
		}
	}
	bad := []string{
		"",
		"not-json",
		`{"env":{}}`,
		`{"model":"opus"}`,
		`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:1/v1"},"hooks":{}}`,
		`{"env":{"ANTHROPIC_AUTH_TOKEN":"sess-bearer"}}`,
		`{"env":{"ANTHROPIC_API_KEY":"sess-bearer"}}`,
		`{"env":{"ANTHROPIC_BASE_URL":""}}`,
		`{"env":{"AWS_REGION":""}}`,
		`{"env":{"CLOUD_ML_REGION":""}}`,
		`{"env":{"UNRELATED":"x"}}`,
		`{"env":"http://127.0.0.1:1/v1"}`,
		`[]`,
	}
	for _, raw := range bad {
		if isRoutingPinJSON(raw) {
			t.Errorf("isRoutingPinJSON(%q) = true, want false", raw)
		}
	}
}

func adjacentValue(argv []string, flag string) (string, bool) {
	i := slices.Index(argv, flag)
	if i < 0 || i+1 >= len(argv) {
		return "", false
	}
	return argv[i+1], true
}
