package servicepriority

import "testing"

func TestParse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		raw     string
		want    Mode
		wantErr bool
	}{
		{name: "unset", raw: "", want: ""},
		{name: "blank", raw: "  ", want: ""},
		{name: "default", raw: "default", want: Default},
		{name: "background", raw: "background", want: Background},
		{name: "case and space", raw: " Background ", want: Background},
		{name: "removed utility mode", raw: "utility", wantErr: true},
		{name: "unknown", raw: "faster", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Parse(tc.raw)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Parse(%q) error = %v, wantErr %v", tc.raw, err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("Parse(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestEffective(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want Mode }{
		{"", Default},
		{Default, Default},
		{Background, Background},
	} {
		if got := Effective(tc.in); got != tc.want {
			t.Errorf("Effective(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
