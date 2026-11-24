package afcli

import (
	"testing"
	"time"

	"github.com/RenseiAI/donmai/daemon"
	providerpi "github.com/RenseiAI/donmai/provider/harness/pi"
)

// TestPiHandshakeTimeoutHint_Forms proves the daemon's harness
// configuration knob (pi.handshakeTimeoutSeconds) parses from the JSON
// wire shapes a resolved profile can carry: float64 seconds (the
// encoding/json default), ints, and duration strings. Absent resolves to
// unset — the provider default stands — while unsafe-but-positive values
// ride through to provider construction, which rejects them loudly.
// Present-but-invalid declarations (zero, negative, garbage, wrong type)
// never collapse to unset: they return a non-zero sentinel the ctor
// threads through so construction rejects them instead of silently
// installing the default.
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
		{"unsafe positive rides through", detailWith(0.5), 500 * time.Millisecond},
		{"zero is invalid, not unset", detailWith(0.0), invalidHandshakeTimeout},
		{"negative is invalid, not unset", detailWith(-5.0), invalidHandshakeTimeout},
		{"negative int is invalid", detailWith(-1), invalidHandshakeTimeout},
		{"garbage string is invalid", detailWith("soon"), invalidHandshakeTimeout},
		{"bool is invalid", detailWith(true), invalidHandshakeTimeout},
		{"nil value is invalid", detailWith(nil), invalidHandshakeTimeout},
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

// TestPiHandshakeTimeoutHint_InvalidIsRejected proves the fail-closed half
// of the knob: a present-but-invalid declaration threads through
// piCtorOptions and fails provider construction instead of silently
// installing the default.
func TestPiHandshakeTimeoutHint_InvalidIsRejected(t *testing.T) {
	t.Parallel()

	d := &daemon.SessionDetail{
		ResolvedProfile: &daemon.SessionResolvedProfile{
			Provider:       "pi",
			ProviderConfig: map[string]any{piHandshakeTimeoutHintKey: -1.0},
		},
	}
	h := agentRunHints(d)
	if h.PiHandshakeTimeout == 0 {
		t.Fatal("declared -1s became unset; construction would install the default instead of rejecting it")
	}
	opts := piCtorOptions(h)
	if opts.HandshakeTimeout == 0 {
		t.Fatal("invalid declaration was dropped by piCtorOptions; construction would install the default")
	}
	if _, err := providerpi.New(opts); err == nil {
		t.Fatal("New accepted a preserved-invalid handshake timeout; want a loud rejection")
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
	if got := piCtorOptions(agentRunCtorHints{PiHandshakeTimeout: invalidHandshakeTimeout}); got.HandshakeTimeout != invalidHandshakeTimeout {
		t.Errorf("piCtorOptions(invalid).HandshakeTimeout = %s; want the invalid sentinel preserved", got.HandshakeTimeout)
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

// TestPiHandshakeMaxAttemptsHint_Forms proves the attempt-budget knob
// (pi.handshakeMaxAttempts) parses the JSON wire shapes a resolved profile
// can carry. Absent resolves to unset (the default of two stands); positive
// integers ride through as-is (bounds are enforced at construction); and
// present-but-invalid declarations return a non-zero sentinel so
// construction rejects them instead of silently installing the default.
func TestPiHandshakeMaxAttemptsHint_Forms(t *testing.T) {
	t.Parallel()

	detailWith := func(v any) *daemon.SessionDetail {
		return &daemon.SessionDetail{
			ResolvedProfile: &daemon.SessionResolvedProfile{
				Provider:       "pi",
				ProviderConfig: map[string]any{piHandshakeMaxAttemptsHintKey: v},
			},
		}
	}
	for _, tc := range []struct {
		name   string
		detail *daemon.SessionDetail
		want   int
	}{
		{"nil detail", nil, 0},
		{"nil profile", &daemon.SessionDetail{}, 0},
		{"absent", detailWith(nil), 0},
		{"int", detailWith(3), 3},
		{"float", detailWith(4.0), 4},
		{"string", detailWith("3"), 3},
		{"out of bounds rides through", detailWith(100), 100},
		{"zero is invalid, not unset", detailWith(0), invalidHandshakeMaxAttempts},
		{"negative is invalid", detailWith(-1), invalidHandshakeMaxAttempts},
		{"fractional is invalid", detailWith(2.5), invalidHandshakeMaxAttempts},
		{"garbage string is invalid", detailWith("soon"), invalidHandshakeMaxAttempts},
		{"bool is invalid", detailWith(true), invalidHandshakeMaxAttempts},
		{"nil value is invalid", detailWith(nil), invalidHandshakeMaxAttempts},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.name == "absent" {
				tc.detail.ResolvedProfile.ProviderConfig = map[string]any{"unrelated.knob": true}
			}
			if got := piHandshakeMaxAttemptsHint(tc.detail); got != tc.want {
				t.Errorf("piHandshakeMaxAttemptsHint(%s) = %d; want %d", tc.name, got, tc.want)
			}
		})
	}
}

// TestPiCtorOptions_ThreadsHandshakeMaxAttempts proves the attempt-budget
// hint reaches the provider Options — and that a present-but-invalid
// declaration is preserved for construction to reject, not dropped to the
// default.
func TestPiCtorOptions_ThreadsHandshakeMaxAttempts(t *testing.T) {
	t.Parallel()

	if got := piCtorOptions(agentRunCtorHints{}); got.HandshakeMaxAttempts != 0 {
		t.Errorf("piCtorOptions(zero value).HandshakeMaxAttempts = %d; want unset", got.HandshakeMaxAttempts)
	}
	if got := piCtorOptions(agentRunCtorHints{PiHandshakeMaxAttempts: 3}); got.HandshakeMaxAttempts != 3 {
		t.Errorf("piCtorOptions(3).HandshakeMaxAttempts = %d; want 3", got.HandshakeMaxAttempts)
	}
	if got := piCtorOptions(agentRunCtorHints{PiHandshakeMaxAttempts: invalidHandshakeMaxAttempts}); got.HandshakeMaxAttempts != invalidHandshakeMaxAttempts {
		t.Errorf("piCtorOptions(invalid).HandshakeMaxAttempts = %d; want the invalid sentinel preserved", got.HandshakeMaxAttempts)
	}
	d := &daemon.SessionDetail{
		ResolvedProfile: &daemon.SessionResolvedProfile{
			Provider:       "pi",
			ProviderConfig: map[string]any{piHandshakeMaxAttemptsHintKey: 3.0},
		},
	}
	if got := agentRunHints(d); got.PiHandshakeMaxAttempts != 3 {
		t.Errorf("agentRunHints(pi 3 attempts).PiHandshakeMaxAttempts = %d; want 3", got.PiHandshakeMaxAttempts)
	}
}
