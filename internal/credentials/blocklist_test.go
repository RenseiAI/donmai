package credentials

import (
	"reflect"
	"sort"
	"testing"
)

func TestIsBlocked_KnownKeys(t *testing.T) {
	t.Parallel()
	for _, name := range AgentEnvBlocklist {
		if !IsBlocked(name) {
			t.Errorf("IsBlocked(%q) = false, want true", name)
		}
	}
}

func TestIsBlocked_NonBlockedKeys(t *testing.T) {
	t.Parallel()
	cases := []string{
		"PATH",
		"HOME",
		"ANTHROPIC_API_KEY", // explicitly NOT in this list; it lives in runtime/env
		"LINEAR_API_KEY",
		"GITHUB_TOKEN",
		"FOO_BAR",
		"",
	}
	for _, name := range cases {
		if IsBlocked(name) {
			t.Errorf("IsBlocked(%q) = true, want false", name)
		}
	}
}

func TestIsBlocked_CaseSensitive(t *testing.T) {
	t.Parallel()
	if IsBlocked("rensei_daemon_jwt") {
		t.Errorf("IsBlocked(lowercased) = true, want false (env vars are case-sensitive)")
	}
}

func TestFilter_RemovesBlockedKeys(t *testing.T) {
	t.Parallel()
	in := []string{
		"PATH=/usr/bin",
		"DONMAI_DAEMON_JWT=secret-jwt-value",
		"HOME=/Users/x",
		"WORKER_API_KEY=rsk_abc",
		"DONMAI_CREDENTIAL_CAPABILITY=per-session-secret",
		"FOO=bar",
		"M2M_JWT_SECRET=hmac",
	}
	got := Filter(in)
	want := []string{
		"PATH=/usr/bin",
		"HOME=/Users/x",
		"FOO=bar",
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Filter():\n  got:  %v\n  want: %v", got, want)
	}
}

func TestFilter_PassesThroughNonBlocked(t *testing.T) {
	t.Parallel()
	in := []string{
		"PATH=/usr/bin",
		"HOME=/Users/x",
		"FOO=bar",
	}
	got := Filter(in)
	if !reflect.DeepEqual(got, in) {
		t.Errorf("Filter() altered non-blocked input:\n  got:  %v\n  want: %v", got, in)
	}
}

func TestFilter_MalformedEntriesPassThrough(t *testing.T) {
	t.Parallel()
	in := []string{
		"PATH=/usr/bin",
		"BARE_NAME_NO_EQ", // no "=", should pass through unchanged
		"=value-no-key",   // "=" at index 0 → name is ""; "" is not blocked → pass through
	}
	got := Filter(in)
	if !reflect.DeepEqual(got, in) {
		t.Errorf("Filter() did not preserve malformed entries:\n  got:  %v\n  want: %v", got, in)
	}
}

func TestFilter_EmptyInput(t *testing.T) {
	t.Parallel()
	// Nil and empty should both yield zero-length output.
	if got := Filter(nil); len(got) != 0 {
		t.Errorf("Filter(nil) length = %d, want 0", len(got))
	}
	if got := Filter([]string{}); len(got) != 0 {
		t.Errorf("Filter([]) length = %d, want 0", len(got))
	}
}

func TestFilter_PreservesValueEqualsSign(t *testing.T) {
	t.Parallel()
	// A value containing "=" should be preserved intact.
	in := []string{"FOO=a=b=c"}
	got := Filter(in)
	if !reflect.DeepEqual(got, in) {
		t.Errorf("Filter() mangled value with embedded '=':\n  got:  %v\n  want: %v", got, in)
	}
}

// TestBlocklistContents pins the exact contents of AgentEnvBlocklist.
// Update this test deliberately when adding or removing a blocked key —
// rensei-tui's daemon/credentials/socket.go hardcodes the same list and
// must be edited in lock-step.
func TestBlocklistContents(t *testing.T) {
	t.Parallel()
	want := []string{
		"DONMAI_DAEMON_JWT",
		"DONMAI_DAEMON_API_KEY",
		"M2M_JWT_SECRET",
		"AUDIT_SIGNING_KEY_PRIVATE",
		"AUDIT_SIGNING_KEY_PUBLIC",
		"WORKOS_API_KEY",
		"WORKOS_COOKIE_PASSWORD",
		"DONMAI_RUNTIME_JWT",
		"WORKER_API_KEY",
		"DONMAI_DAEMON_TOKEN",
		"DONMAI_ORCHESTRATOR_URL",
		"DONMAI_CREDENTIAL_CAPABILITY",
	}
	got := append([]string{}, AgentEnvBlocklist...)
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AgentEnvBlocklist drifted (sync with rensei-tui daemon/credentials/socket.go):\n  got:  %v\n  want: %v", got, want)
	}
}
