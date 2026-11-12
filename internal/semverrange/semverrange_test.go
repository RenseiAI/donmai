package semverrange

import (
	"errors"
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

func TestRangeContains(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		rng  string
		in   []string
		out  []string
		fail bool
	}{
		{rng: ">=9 <10", in: []string{"9.0.0", "9.15.9"}, out: []string{"8.15.0", "10.0.0", "10.0.0-rc.1"}},
		{rng: ">=10 <11", in: []string{"10.0.0", "10.33.2", "10.0.0-rc.1"}, out: []string{"9.15.9", "11.0.0"}},
		{rng: "9", in: []string{"9.0.0", "9.99.9"}, out: []string{"10.0.0", "8.0.0"}},
		{rng: "=9.1", in: []string{"9.1.0", "9.1.7"}, out: []string{"9.2.0"}},
		{rng: "9.1.2", in: []string{"9.1.2"}, out: []string{"9.1.3"}},
		{rng: "^9.1.2", in: []string{"9.1.2", "9.9.0"}, out: []string{"10.0.0", "9.1.1"}},
		{rng: "^0.2.3", in: []string{"0.2.9"}, out: []string{"0.3.0"}},
		{rng: "^0.0.3", in: []string{"0.0.3"}, out: []string{"0.0.4"}},
		{rng: "~9.1", in: []string{"9.1.0", "9.1.9"}, out: []string{"9.2.0"}},
		{rng: ">9 <=11", in: []string{"10.0.0", "11.5.0"}, out: []string{"9.9.9", "12.0.0"}},
		{rng: "<9 || >=11", in: []string{"8.0.0", "11.0.0"}, out: []string{"9.0.0", "10.1.0"}},
		{rng: "", fail: true},
		{rng: "x", fail: true},
		{rng: "9.x", fail: true},
		{rng: "1 - 2", fail: true},
		{rng: ">=9 ||", fail: true},
		{rng: ">=", fail: true},
	} {
		r, err := ParseRange(tc.rng)
		if tc.fail {
			if err == nil || !errors.Is(err, ErrInvalid) {
				t.Fatalf("ParseRange(%q) err = %v, want ErrInvalid", tc.rng, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("ParseRange(%q): %v", tc.rng, err)
		}
		for _, v := range tc.in {
			if !r.Contains(mustVersion(t, v)) {
				t.Errorf("%q should contain %s", tc.rng, v)
			}
		}
		for _, v := range tc.out {
			if r.Contains(mustVersion(t, v)) {
				t.Errorf("%q should not contain %s", tc.rng, v)
			}
		}
	}
}

func TestRangeOverlaps(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{">=9 <10", ">=10 <11", false},
		{">=9 <=10", ">=10 <11", true},
		{">=9 <10", "9.15", true},
		{">=9", "<9", false},
		{">=9", ">=12", true},
		{"<9 || >=11", ">=10 <11", false},
		{"<9 || >=11", ">=10 <12", true},
	} {
		a, err := ParseRange(tc.a)
		if err != nil {
			t.Fatal(err)
		}
		b, err := ParseRange(tc.b)
		if err != nil {
			t.Fatal(err)
		}
		if got := a.Overlaps(b); got != tc.want {
			t.Errorf("%q overlaps %q = %v, want %v", tc.a, tc.b, got, tc.want)
		}
		if got := b.Overlaps(a); got != tc.want {
			t.Errorf("%q overlaps %q = %v, want %v (reversed)", tc.b, tc.a, got, tc.want)
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
