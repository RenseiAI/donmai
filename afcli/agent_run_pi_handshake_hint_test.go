package afcli

import (
	"testing"
	"time"

	"github.com/RenseiAI/donmai/daemon"
)

// TestPiHandshakeTimeoutHint_Forms proves the daemon's harness
// configuration knob (pi.handshakeTimeoutSeconds) parses from the JSON
// wire shapes a resolved profile can carry: float64 seconds (the
// encoding/json default), ints, and duration strings. Zero, negative,
// missing, and garbage values all resolve to unset — the provider default
// stands — while unsafe-but-positive values ride through to provider
// construction, which rejects them loudly.
func TestPiHandshakeTimeoutHint_Forms(t *testing.T) {
	t.Parallel()

	detailWith := func(v any) *daemon.SessionDetail {
		return &daemon.SessionDetail{
			ResolvedProfile: &daemon.SessionResolvedProfile{
				Provider:       "pi",
				ProviderConfig: map[string]any{piHandshakeTimeoutHintKey: v},
			},
		}
	}
	for _, tc := range []struct {
		name   string
		detail *daemon.SessionDetail
		want   time.Duration
	}{
		{"nil detail", nil, 0},
		{"nil profile", &daemon.SessionDetail{}, 0},
		{"absent", detailWith(nil), 0},
		{"float seconds", detailWith(15.0), 15 * time.Second},
		{"int seconds", detailWith(20), 20 * time.Second},
		{"duration string", detailWith("25s"), 25 * time.Second},
		{"zero", detailWith(0.0), 0},
		{"negative", detailWith(-5.0), 0},
		{"garbage string", detailWith("soon"), 0},
		{"bool", detailWith(true), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.name == "absent" {
				tc.detail.ResolvedProfile.ProviderConfig = map[string]any{"unrelated.knob": true}
			}
			if got := piHandshakeTimeoutHint(tc.detail); got != tc.want {
				t.Errorf("piHandshakeTimeoutHint(%s) = %s; want %s", tc.name, got, tc.want)
			}
		})
	}
}

// TestPiCtorOptions_ThreadsHandshakeTimeout proves the hint actually
// reaches the provider Options the pi ctor constructs — without this, a
// daemon-stated deadline would be computed and silently dropped, and every
// real session would keep the default with no operational override.
func TestPiCtorOptions_ThreadsHandshakeTimeout(t *testing.T) {
	t.Parallel()

	if got := piCtorOptions(agentRunCtorHints{}); got.HandshakeTimeout != 0 {
		t.Errorf("piCtorOptions(zero value).HandshakeTimeout = %s; want unset", got.HandshakeTimeout)
	}
	if got := piCtorOptions(agentRunCtorHints{PiHandshakeTimeout: 20 * time.Second}); got.HandshakeTimeout != 20*time.Second {
		t.Errorf("piCtorOptions(20s).HandshakeTimeout = %s; want 20s", got.HandshakeTimeout)
	}
}

// TestAgentRunHints_ThreadsPiHandshakeTimeout proves agentRunHints picks
// up the daemon-stated pi handshake deadline alongside the existing
// per-session signals.
func TestAgentRunHints_ThreadsPiHandshakeTimeout(t *testing.T) {
	t.Parallel()

	d := &daemon.SessionDetail{
		ResolvedProfile: &daemon.SessionResolvedProfile{
			Provider:       "pi",
			ProviderConfig: map[string]any{piHandshakeTimeoutHintKey: 30.0},
		},
	}
	if got := agentRunHints(d); got.PiHandshakeTimeout != 30*time.Second {
		t.Errorf("agentRunHints(pi 30s).PiHandshakeTimeout = %s; want 30s", got.PiHandshakeTimeout)
	}
	if got := agentRunHints(nil); got.PiHandshakeTimeout != 0 {
		t.Errorf("agentRunHints(nil).PiHandshakeTimeout = %s; want unset", got.PiHandshakeTimeout)
	}
}
