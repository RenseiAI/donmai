package strategies

import (
	"strings"
	"testing"
)

func TestNew_Factory(t *testing.T) {
	tests := []struct {
		name     string
		wantName string
		wantErr  bool
	}{
		{name: NameRebase, wantName: NameRebase},
		{name: NameMerge, wantName: NameMerge},
		{name: NameSquash, wantName: NameSquash},
		{name: "bogus", wantErr: true},
		{name: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run("name="+tt.name, func(t *testing.T) {
			s, err := New(tt.name)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("New(%q) = nil error, want error", tt.name)
				}
				if !strings.Contains(err.Error(), "unknown landing strategy") {
					t.Errorf("error = %q, want it to mention unknown landing strategy", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("New(%q) error: %v", tt.name, err)
			}
			if s.Name() != tt.wantName {
				t.Errorf("Name() = %q, want %q", s.Name(), tt.wantName)
			}
		})
	}
}

func TestSplitLines(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"\n\n", nil},
		{"a", []string{"a"}},
		{"a\nb\nc\n", []string{"a", "b", "c"}},
		{"\na\n\nb\n", []string{"a", "b"}},
	}
	for _, tt := range tests {
		got := splitLines(tt.in)
		if len(got) != len(tt.want) {
			t.Fatalf("splitLines(%q) = %v, want %v", tt.in, got, tt.want)
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("splitLines(%q)[%d] = %q, want %q", tt.in, i, got[i], tt.want[i])
			}
		}
	}
}
