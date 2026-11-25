package semverrange

import (
	"errors"
	"strings"
	"testing"
)

func TestParseVersion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"9.15.9", "9.15.9", true},
		{"v10.0.0", "10.0.0", true},
		{" 1.2.3 \n", "1.2.3", true},
		{"1.2.3-rc.1", "1.2.3-rc.1", true},
		{"1.2.3+sha512.abc", "1.2.3", true},
		{"1.2", "", false},
		{"01.2.3", "", false},
		{"1.2.x", "", false},
		{"", "", false},
		{"1.2.3-", "", false},
	} {
		got, err := ParseVersion(tc.in)
		if tc.ok != (err == nil) {
			t.Fatalf("ParseVersion(%q) err = %v, want ok=%v", tc.in, err, tc.ok)
		}
		if err != nil {
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("ParseVersion(%q) err = %v, want ErrInvalid", tc.in, err)
			}
			continue
		}
		if got.String() != tc.want {
			t.Fatalf("ParseVersion(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// The range tables below are the union of the two suites that tested a
// `versions` range before this package became the only implementation: the
// preparer's suite (origin "prep") and the manifest validator's suite
// (origin "kit"). Every case keeps the answer its suite expected, except the
// two rules the suites disagreed on, which are listed in
// TestRangeRulesTheSuitesDisagreedOn with both answers.
//
// A range of "" stands for a missing `versions` key, which both callers
// read as All.

func parseOrAll(t *testing.T, rng string) Range {
	t.Helper()
	if rng == "" {
		return All()
	}
	r, err := ParseRange(rng)
	if err != nil {
		t.Fatalf("ParseRange(%q): %v", rng, err)
	}
	return r
}

func TestRangeContainsSharedTable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		origin string
		rng    string
		in     []string
		out    []string
	}{
		{"prep", ">=9 <10", []string{"9.0.0", "9.15.9"}, []string{"8.15.0", "10.0.0", "10.0.0-rc.1"}},
		{"prep", ">=10 <11", []string{"10.0.0", "10.33.2"}, []string{"9.15.9", "11.0.0"}},
		{"prep", "9", []string{"9.0.0", "9.99.9"}, []string{"10.0.0", "8.0.0"}},
		{"prep", "=9.1", []string{"9.1.0", "9.1.7"}, []string{"9.2.0"}},
		{"prep", "9.1.2", []string{"9.1.2"}, []string{"9.1.3"}},
		{"prep", "^9.1.2", []string{"9.1.2", "9.9.0"}, []string{"10.0.0", "9.1.1"}},
		{"prep", "^0.2.3", []string{"0.2.9"}, []string{"0.3.0"}},
		{"prep", "^0.0.3", []string{"0.0.3"}, []string{"0.0.4"}},
		{"prep", "~9.1", []string{"9.1.0", "9.1.9"}, []string{"9.2.0"}},
		{"prep", ">9 <=11", []string{"10.0.0", "11.5.0"}, []string{"9.9.9", "12.0.0"}},
		{"prep", "<9 || >=11", []string{"8.0.0", "11.0.0"}, []string{"9.0.0", "10.1.0"}},

		{"kit", "", []string{"0.0.1", "9.15.9", "12.16.1", "10.0.0-rc.1"}, nil},
		{"kit", "*", []string{"1.2.3"}, nil},
		{"kit", ">=9 <10", []string{"9.0.0", "9.15.9", "9.99.99"}, []string{"8.15.0", "10.0.0", "10.0.0-rc.1"}},
		{"kit", ">=10 <11", []string{"10.0.0", "10.33.2"}, []string{"9.15.9", "11.0.0", "12.16.1"}},
		{"kit", ">= 9 < 10", []string{"9.5.0"}, []string{"10.0.0"}},
		{"kit", ">=9.5.0 <=9.7.2", []string{"9.5.0", "9.7.2", "9.6.0"}, []string{"9.4.9", "9.7.3"}},
		{"kit", ">9.5.0", []string{"9.5.1", "10.0.0"}, []string{"9.5.0"}},
		{"kit", ">9", []string{"10.0.0"}, []string{"9.99.0"}},
		{"kit", "<=9", []string{"9.99.0"}, []string{"10.0.0"}},
		{"kit", "9", []string{"9.0.0", "9.9.9"}, []string{"10.0.0", "8.9.9"}},
		{"kit", "9.15", []string{"9.15.0", "9.15.9"}, []string{"9.16.0"}},
		{"kit", "9.15.9", []string{"9.15.9"}, []string{"9.15.8", "9.15.10"}},
		{"kit", "=9.15.9", []string{"9.15.9"}, []string{"9.15.8"}},
		{"kit", "~9.15.2", []string{"9.15.2", "9.15.9"}, []string{"9.16.0", "9.15.1"}},
		{"kit", "~9", []string{"9.4.0"}, []string{"10.0.0"}},
		{"kit", "^9.15.2", []string{"9.15.2", "9.99.0"}, []string{"10.0.0", "9.15.1"}},
		{"kit", "^0.2.3", []string{"0.2.3", "0.2.9"}, []string{"0.3.0", "0.2.2"}},
		{"kit", "^0.0.3", []string{"0.0.3"}, []string{"0.0.4"}},
		{"kit", "^0", []string{"0.9.0"}, []string{"1.0.0"}},
		{"kit", ">=8 <9 || >=10 <11", []string{"8.15.0", "10.2.0"}, []string{"9.15.9", "11.0.0"}},
		{"kit", ">=9 <10", []string{"9.15.9+sha512.abcdef"}, nil},
	} {
		r := parseOrAll(t, tc.rng)
		for _, v := range tc.in {
			if !r.Contains(mustVersion(t, v)) {
				t.Errorf("[%s] %q should contain %s", tc.origin, tc.rng, v)
			}
		}
		for _, v := range tc.out {
			if r.Contains(mustVersion(t, v)) {
				t.Errorf("[%s] %q should not contain %s", tc.origin, tc.rng, v)
			}
		}
	}
}

func TestParseRangeRejectsSharedTable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ origin, rng string }{
		{"prep", ""},
		{"prep", "1 - 2"},
		{"prep", ">=9 ||"},
		{"prep", ">="},
		{"kit", "latest"},
		{"kit", ">="},
		{"kit", ">=9 <"},
		{"kit", "9.x.1"},
		{"kit", "1.2.3.4"},
		{"kit", ">=9.0.0-rc.1"},
		{"kit", "9.0.0+build"},
		{"kit", ">=10 <9"},
		{"kit", ">9 <9.0.0"},
		{"kit", "^*"},
		{"kit", "~x"},
		{"kit", "09"},
		{"kit", "a.b.c"},
		{"kit", ">=9 ||"},
		{"kit", "|| >=9"},
		{"kit", "-1"},
		{"kit", ">=9 && <10"},
		{"kit", "v9"},
	} {
		if _, err := ParseRange(tc.rng); !errors.Is(err, ErrInvalid) {
			t.Errorf("[%s] ParseRange(%q) err = %v, want ErrInvalid", tc.origin, tc.rng, err)
		}
	}
	for _, tc := range []struct{ rng, want string }{
		{">=10 <9", "matches no version"},
		{">=10.0.0-rc.1", "prerelease"},
	} {
		if _, err := ParseRange(tc.rng); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ParseRange(%q) err = %v, want it to say %q", tc.rng, err, tc.want)
		}
	}
}

func TestParseVersionRejectsNonVersionsSharedTable(t *testing.T) {
	t.Parallel()
	// The kit suite's Covers refused these as versions; ParseVersion is the
	// one parser both callers now use for the version side.
	for _, v := range []string{"", "latest", "9", "9.15", "9.15.9.1"} {
		if _, err := ParseVersion(v); !errors.Is(err, ErrInvalid) {
			t.Errorf("ParseVersion(%q) err = %v, want ErrInvalid", v, err)
		}
	}
}

func TestRangeOverlapsSharedTable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		origin string
		a, b   string
		want   bool
	}{
		{"prep", ">=9 <10", ">=10 <11", false},
		{"prep", ">=9 <=10", ">=10 <11", true},
		{"prep", ">=9 <10", "9.15", true},
		{"prep", ">=9", "<9", false},
		{"prep", ">=9", ">=12", true},
		{"prep", "<9 || >=11", ">=10 <11", false},
		{"prep", "<9 || >=11", ">=10 <12", true},

		{"kit", ">=9 <10", ">=10 <11", false},
		{"kit", ">=9 <10", ">=9.5 <11", true},
		{"kit", ">=9 <10", "<9", false},
		{"kit", ">=9 <10", "<=9", true},
		{"kit", ">=9 <=9.5.0", ">9.5.0", false},
		{"kit", ">=9 <=9.5.0", ">=9.5.0", true},
		{"kit", "9", "10", false},
		{"kit", "9.15.9", ">=9 <10", true},
		{"kit", ">=8 <9 || >=10 <11", ">=9 <10", false},
		{"kit", ">=8 <9 || >=10 <11", ">=10.5 <12", true},
		{"kit", "", ">=9 <10", true},
		{"kit", "", "", true},
		{"kit", "*", "<1", true},
		{"kit", ">=11", "<11", false},
		{"kit", ">=11", "<=11", true},
	} {
		a, b := parseOrAll(t, tc.a), parseOrAll(t, tc.b)
		if got := a.Overlaps(b); got != tc.want {
			t.Errorf("[%s] %q overlaps %q = %v, want %v", tc.origin, tc.a, tc.b, got, tc.want)
		}
		if got := b.Overlaps(a); got != tc.want {
			t.Errorf("[%s] %q overlaps %q = %v, want %v (reversed)", tc.origin, tc.b, tc.a, got, tc.want)
		}
	}
}

// TestRangeRulesTheSuitesDisagreedOn pins the two rules on which the
// suites expected different answers, with the answer kept and why.
func TestRangeRulesTheSuitesDisagreedOn(t *testing.T) {
	t.Parallel()
	// 1. A pre-release of a release line belongs to that line. The prep
	// suite expected ">=10 <11" to contain 10.0.0-rc.1; the kit suite
	// expected ">=9 <10" to refuse 9.0.0-rc.1 (pre-releases were covered
	// only by a missing range). Kept: the prep answer, so a leaf pinning a
	// manager's pre-release selects the entry (and store layout) of its own
	// line instead of toolchain_mismatch. Neither suite's ">=9 <10" claims
	// 10.0.0-rc.1, which was the kit suite's concern.
	for _, tc := range []struct {
		rng, v string
		want   bool
	}{
		{">=10 <11", "10.0.0-rc.1", true},
		{">=9 <10", "9.0.0-rc.1", true},
		{">=9 <10", "10.0.0-rc.1", false},
		{"9.15.9", "9.15.9-rc.1", false},
	} {
		if got := parseOrAll(t, tc.rng).Contains(mustVersion(t, tc.v)); got != tc.want {
			t.Errorf("%q contains %s = %v, want %v", tc.rng, tc.v, got, tc.want)
		}
	}
	// 2. Wildcards. The prep suite expected "x" and "9.x" to be refused;
	// the kit suite accepted "*" and "9.x" as npm x-ranges. Kept: the kit
	// answer, with the kit suite's limits ("9.x.1", "^*" and "~x" refused).
	for _, tc := range []struct{ rng, in, out string }{
		{"x", "1.2.3", ""},
		{"9.x", "9.3.1", "10.0.0"},
		{"9.x.x", "9.0.0", "8.9.9"},
		{">=9.x <10.x", "9.15.9", "10.0.0"},
	} {
		r := parseOrAll(t, tc.rng)
		if !r.Contains(mustVersion(t, tc.in)) {
			t.Errorf("%q should contain %s", tc.rng, tc.in)
		}
		if tc.out != "" && r.Contains(mustVersion(t, tc.out)) {
			t.Errorf("%q should not contain %s", tc.rng, tc.out)
		}
	}
}

func TestComparePreRelease(t *testing.T) {
	t.Parallel()
	ordered := []string{"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0"}
	for i := 0; i+1 < len(ordered); i++ {
		a, b := mustVersion(t, ordered[i]), mustVersion(t, ordered[i+1])
		if Compare(a, b) >= 0 || Compare(b, a) <= 0 {
			t.Fatalf("%s should order before %s", a, b)
		}
	}
}

func mustVersion(t *testing.T, s string) Version {
	t.Helper()
	v, err := ParseVersion(s)
	if err != nil {
		t.Fatalf("ParseVersion(%q): %v", s, err)
	}
	return v
}
