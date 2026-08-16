package afcli

import (
	"encoding/json"
	"testing"

	"github.com/RenseiAI/donmai/daemon"
)

// TestDetailToQueuedWork_ContextWindow pins the contextWindow leg of the
// dispatch bridge at the wire: a resolvedProfile JSON payload carrying the
// top-level contextWindow field must decode onto
// daemon.SessionResolvedProfile (before the field existed, the lenient
// decode dropped the key silently) and survive detailToQueuedWork into the
// runner-visible ProviderConfig under the same "contextWindow" key
// runner.ResolvedModelProfile.ToResolvedProfile produces — the key the
// provider harnesses read. Precedence is also pinned: an explicit
// providerConfig.contextWindow wins over the top-level field, and a
// modelProfile.context (the richer shape that supersedes resolvedProfile)
// wins over both when present.
func TestDetailToQueuedWork_ContextWindow(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		detailJSON  string
		wantDecoded int // expected SessionResolvedProfile.ContextWindow after decode
		want        any // expected ProviderConfig["contextWindow"]; nil => key absent
	}{
		{
			name:        "top-level contextWindow reaches ProviderConfig",
			detailJSON:  `{"sessionId":"s1","resolvedProfile":{"provider":"stub","contextWindow":1000000}}`,
			wantDecoded: 1_000_000,
			want:        1_000_000,
		},
		{
			name:        "explicit providerConfig.contextWindow wins over top-level",
			detailJSON:  `{"sessionId":"s2","resolvedProfile":{"provider":"stub","contextWindow":1000000,"providerConfig":{"contextWindow":500000}}}`,
			wantDecoded: 1_000_000,
			want:        float64(500_000),
		},
		{
			name:       "absent contextWindow stays absent (legacy dispatch)",
			detailJSON: `{"sessionId":"s3","resolvedProfile":{"provider":"stub"}}`,
			want:       nil,
		},
		{
			name:        "modelProfile dispatch still honors top-level contextWindow",
			detailJSON:  `{"sessionId":"s4","modelProfile":{"id":"mp_1","providerId":"stub","model":"m"},"resolvedProfile":{"provider":"stub","contextWindow":1000000}}`,
			wantDecoded: 1_000_000,
			want:        1_000_000,
		},
		{
			name:        "modelProfile.context supersedes the top-level field",
			detailJSON:  `{"sessionId":"s5","modelProfile":{"id":"mp_1","providerId":"stub","model":"m","context":500000},"resolvedProfile":{"provider":"stub","contextWindow":1000000}}`,
			wantDecoded: 1_000_000,
			want:        500_000,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var d daemon.SessionDetail
			if err := json.Unmarshal([]byte(tc.detailJSON), &d); err != nil {
				t.Fatalf("unmarshal detail: %v", err)
			}
			if d.ResolvedProfile == nil {
				t.Fatal("resolvedProfile did not decode")
			}
			if d.ResolvedProfile.ContextWindow != tc.wantDecoded {
				t.Fatalf("decoded ContextWindow = %d, want %d", d.ResolvedProfile.ContextWindow, tc.wantDecoded)
			}
			qw, err := detailToQueuedWork(&d)
			if err != nil {
				t.Fatalf("detailToQueuedWork: %v", err)
			}
			got, ok := qw.ResolvedProfile.ProviderConfig["contextWindow"]
			if tc.want == nil {
				if ok {
					t.Fatalf("ProviderConfig[contextWindow] = %v, want absent", got)
				}
				return
			}
			if !ok || got != tc.want {
				t.Fatalf("ProviderConfig[contextWindow] = %v (present=%t), want %v", got, ok, tc.want)
			}
		})
	}
}
