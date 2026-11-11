package kit

import (
	"errors"
	"testing"
)

func TestParseVersionRangeCoversTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		rng     string
		covers  []string
		refuses []string
	}{
		{"", []string{"0.0.1", "9.15.9", "12.16.1", "10.0.0-rc.1"}, nil},
		{"*", []string{"1.2.3"}, nil},
		{">=9 <10", []string{"9.0.0", "9.15.9", "9.99.99"}, []string{"8.15.0", "10.0.0", "10.0.0-rc.1", "9.0.0-rc.1"}},
		{">=10 <11", []string{"10.0.0", "10.33.2"}, []string{"9.15.9", "11.0.0", "12.16.1"}},
		{">= 9 < 10", []string{"9.5.0"}, []string{"10.0.0"}},
		{">=9.5.0 <=9.7.2", []string{"9.5.0", "9.7.2", "9.6.0"}, []string{"9.4.9", "9.7.3"}},
		{">9.5.0", []string{"9.5.1", "10.0.0"}, []string{"9.5.0"}},
		{">9", []string{"10.0.0"}, []string{"9.99.0"}},
		{"<=9", []string{"9.99.0"}, []string{"10.0.0"}},
		{"9", []string{"9.0.0", "9.9.9"}, []string{"10.0.0", "8.9.9"}},
		{"9.x", []string{"9.3.1"}, []string{"10.0.0"}},
		{"9.15", []string{"9.15.0", "9.15.9"}, []string{"9.16.0"}},
		{"9.15.9", []string{"9.15.9"}, []string{"9.15.8", "9.15.10"}},
		{"=9.15.9", []string{"9.15.9"}, []string{"9.15.8"}},
		{"~9.15.2", []string{"9.15.2", "9.15.9"}, []string{"9.16.0", "9.15.1"}},
		{"~9", []string{"9.4.0"}, []string{"10.0.0"}},
		{"^9.15.2", []string{"9.15.2", "9.99.0"}, []string{"10.0.0", "9.15.1"}},
		{"^0.2.3", []string{"0.2.3", "0.2.9"}, []string{"0.3.0", "0.2.2"}},
		{"^0.0.3", []string{"0.0.3"}, []string{"0.0.4"}},
		{"^0", []string{"0.9.0"}, []string{"1.0.0"}},
		{">=8 <9 || >=10 <11", []string{"8.15.0", "10.2.0"}, []string{"9.15.9", "11.0.0"}},
		{">=9 <10", []string{"9.15.9+sha512.abcdef"}, nil},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.rng, func(t *testing.T) {
			t.Parallel()
			r, err := ParseVersionRange(tc.rng)
			if err != nil {
				t.Fatalf("ParseVersionRange(%q): %v", tc.rng, err)
			}
			for _, v := range tc.covers {
				if ok, err := r.Covers(v); err != nil || !ok {
					t.Errorf("%q should cover %q (ok=%v err=%v)", tc.rng, v, ok, err)
				}
			}
			for _, v := range tc.refuses {
				if ok, err := r.Covers(v); err != nil || ok {
					t.Errorf("%q should not cover %q (ok=%v err=%v)", tc.rng, v, ok, err)
				}
			}
		})
	}
}

func TestParseVersionRangeRejects(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{
		"latest", ">=", ">=9 <", "9.x.1", "1.2.3.4", ">=9.0.0-rc.1", "9.0.0+build", ">=10 <9", ">9 <9.0.0",
		"^*", "~x", "09", "a.b.c", ">=9 ||", "|| >=9", "-1", ">=9 && <10", "v9",
	} {
		if _, err := ParseVersionRange(bad); !errors.Is(err, ErrVersionRangeInvalid) {
			t.Errorf("ParseVersionRange(%q) = %v, want ErrVersionRangeInvalid", bad, err)
		}
	}
}

func TestVersionRangeCoversRejectsNonVersions(t *testing.T) {
	t.Parallel()
	r, _ := ParseVersionRange(">=9 <10")
	for _, v := range []string{"", "latest", "9", "9.15", "9.15.9.1"} {
		if _, err := r.Covers(v); err == nil {
			t.Errorf("Covers(%q) accepted a non major.minor.patch version", v)
		}
	}
}

func TestVersionRangeOverlapsTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		a, b string
		want bool
	}{
		{">=9 <10", ">=10 <11", false},
		{">=9 <10", ">=9.5 <11", true},
		{">=9 <10", "<9", false},
		{">=9 <10", "<=9", true},
		{">=9 <=9.5.0", ">9.5.0", false},
		{">=9 <=9.5.0", ">=9.5.0", true},
		{"9", "10", false},
		{"9.15.9", ">=9 <10", true},
		{">=8 <9 || >=10 <11", ">=9 <10", false},
		{">=8 <9 || >=10 <11", ">=10.5 <12", true},
		{"", ">=9 <10", true},
		{"", "", true},
		{"*", "<1", true},
		{">=11", "<11", false},
		{">=11", "<=11", true},
	}
	for _, tc := range cases {
		a, err := ParseVersionRange(tc.a)
		if err != nil {
			t.Fatal(err)
		}
		b, err := ParseVersionRange(tc.b)
		if err != nil {
			t.Fatal(err)
		}
		if got := a.Overlaps(b); got != tc.want {
			t.Errorf("Overlaps(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
		if got := b.Overlaps(a); got != tc.want {
			t.Errorf("Overlaps(%q, %q) = %v, want %v (symmetry)", tc.b, tc.a, got, tc.want)
		}
	}
}
