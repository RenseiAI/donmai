package seatqos

import (
	"testing"
)

func TestParse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		raw     string
		want    Class
		wantErr bool
	}{
		{name: "empty is no preference", raw: "", want: ""},
		{name: "default", raw: "default", want: Default},
		{name: "utility", raw: "utility", want: Utility},
		{name: "background", raw: "background", want: Background},
		{name: "case and space are ignored", raw: " Utility ", want: Utility},
		{name: "unknown", raw: "nice", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Parse(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Parse(%q) = %q, nil; want an error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("Parse(%q) = %q; want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestEffective(t *testing.T) {
	t.Parallel()
	if got := Effective(""); got != Default {
		t.Errorf("Effective(%q) = %q; want default", "", got)
	}
	if got := Effective(Utility); got != Utility {
		t.Errorf("Effective(utility) = %q; want utility", got)
	}
}

func TestWraps(t *testing.T) {
	t.Parallel()
	if Wraps("") || Wraps(Default) {
		t.Error("Wraps reports a wrap for the default class; the default spawn path must stay unwrapped")
	}
	if !Wraps(Utility) || !Wraps(Background) {
		t.Error("Wraps reports no wrap for a non-default class; the spawn would silently drop the operator's ask")
	}
	// Values that bypassed Parse never wrap: they read as default instead
	// of reaching the wrapper as a clamp nobody validated.
	for _, unvetted := range []Class{"nice", "maintenance", "Utility", " background", "-x"} {
		if Wraps(unvetted) {
			t.Errorf("Wraps(%q) = true; an unvetted class must never wrap", unvetted)
		}
	}
}

func TestClasses(t *testing.T) {
	t.Parallel()
	got := Classes()
	if len(got) != 3 || got[0] != Default || got[1] != Utility || got[2] != Background {
		t.Errorf("Classes() = %v; want [default utility background]", got)
	}
}
