package stubagent

import (
	"strings"
	"testing"
)

func TestLoadToolPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		raw             string
		wantAllowed     string
		wantDisallowed  string
		wantErrContains string
	}{
		{
			name: "unset is the zero policy, not an error",
		},
		{
			name: "blank is the zero policy, not an error",
			raw:  "   ",
		},
		{
			name:           "both lists round trip",
			raw:            `{"allowedTools":["Read","Grep"],"disallowedTools":["Bash"]}`,
			wantAllowed:    "Read,Grep",
			wantDisallowed: "Bash",
		},
		{
			name: "an explicitly empty policy is not an error",
			raw:  `{}`,
		},
		{
			// Same posture as Parse: a value naming a knob this build does not
			// have would otherwise be read as though the knob were off.
			name:            "an unknown field is refused",
			raw:             `{"allowedTools":["Read"],"toolHooks":["x"]}`,
			wantErrContains: "toolHooks",
		},
		{
			name:            "a wrong-typed field is refused",
			raw:             `{"allowedTools":"Read"}`,
			wantErrContains: EnvToolPolicy,
		},
		{
			name:            "trailing garbage is refused",
			raw:             `{"allowedTools":["Read"]} and then some`,
			wantErrContains: EnvToolPolicy,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			policy, err := LoadToolPolicy(func(key string) string {
				if key != EnvToolPolicy {
					t.Fatalf("loader read %q, want only %q", key, EnvToolPolicy)
				}
				return tc.raw
			})
			if tc.wantErrContains != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrContains) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErrContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadToolPolicy: %v", err)
			}
			if got := strings.Join(policy.AllowedTools, ","); got != tc.wantAllowed {
				t.Errorf("AllowedTools = %q, want %q", got, tc.wantAllowed)
			}
			if got := strings.Join(policy.DisallowedTools, ","); got != tc.wantDisallowed {
				t.Errorf("DisallowedTools = %q, want %q", got, tc.wantDisallowed)
			}
		})
	}
}

func TestToolPolicyEmptyAndNotice(t *testing.T) {
	t.Parallel()

	if !(ToolPolicy{}).Empty() {
		t.Error("the zero policy must report Empty; the parent uses it to decide whether to record anything")
	}
	if (ToolPolicy{DisallowedTools: []string{"Bash"}}).Empty() {
		t.Error("a deny-list-only policy must not report Empty")
	}

	notice := ToolPolicy{AllowedTools: []string{"Read"}, DisallowedTools: []string{"Bash", "Write"}}.Notice()
	if !strings.HasPrefix(notice, ToolPolicyNoticePrefix) {
		t.Errorf("notice = %q, want it to start with %q", notice, ToolPolicyNoticePrefix)
	}
	for _, want := range []string{"allowed=[Read]", "disallowed=[Bash,Write]", "registers no tools"} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice = %q, want it to contain %q", notice, want)
		}
	}
	if strings.Contains(notice, "\n") {
		t.Errorf("notice = %q, want a single line; the child prints it with Fprintln", notice)
	}
}

func TestEncodeToolPolicyRoundTrips(t *testing.T) {
	t.Parallel()

	want := ToolPolicy{AllowedTools: []string{"Read"}, DisallowedTools: []string{"Bash"}}
	encoded, err := EncodeToolPolicy(want)
	if err != nil {
		t.Fatalf("EncodeToolPolicy: %v", err)
	}
	got, err := LoadToolPolicy(func(string) string { return encoded })
	if err != nil {
		t.Fatalf("LoadToolPolicy: %v", err)
	}
	if strings.Join(got.AllowedTools, ",") != "Read" || strings.Join(got.DisallowedTools, ",") != "Bash" {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}
