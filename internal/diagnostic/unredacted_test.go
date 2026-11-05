package diagnostic

import "testing"

func TestUnredactedValues(t *testing.T) {
	got := UnredactedValues(`password="hunter2" token=[REDACTED] https://u:pw@example.test`)
	want := []string{"hunter2", "pw"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("UnredactedValues = %q; want %q", got, want)
	}
}

func TestUnredacted(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"plain build output", false},
		{"password=hunter2", true},
		{"password=[REDACTED]", false},
		{`{"password":"[REDACTED]"}`, false},
		{`{"token":[REDACTED]}`, false},
		{"Authorization: Bearer abc.def", true},
		{"authorization: [REDACTED]", false},
		{"https://user:pw@example.test/repo", true},
		{"https://user:[REDACTED]@example.test/repo", false},
		{"https://[REDACTED]:[REDACTED]@example.test/repo", false},
		{"key sk-abcdefghijklmnop0", true},
		{"password=[REDACTED]leftover-secret", true},
		{`{"token":[REDACTED]}, next`, false},
	}
	for _, tc := range cases {
		if got := Unredacted(tc.in); got != tc.want {
			t.Errorf("Unredacted(%q) = %v; want %v", tc.in, got, tc.want)
		}
		// Whatever Redact outputs must read as redacted: the archive's
		// proof relies on this fixed point.
		if out := Redact(tc.in); Unredacted(out) {
			t.Errorf("Unredacted(Redact(%q)) = true for %q", tc.in, out)
		}
	}
}
