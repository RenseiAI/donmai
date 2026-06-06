package clijsonl

import (
	"slices"
	"testing"
)

func TestComposeEnv_DeterministicOrder(t *testing.T) {
	t.Parallel()

	out1 := composeEnv([]string{"PATH=/usr/bin"}, map[string]string{"B": "2", "A": "1"})
	out2 := composeEnv([]string{"PATH=/usr/bin"}, map[string]string{"A": "1", "B": "2"})

	if !slices.Equal(out1, out2) {
		t.Errorf("composeEnv not deterministic:\n%v\n%v", out1, out2)
	}
	want := []string{"PATH=/usr/bin", "A=1", "B=2"}
	if !slices.Equal(out1, want) {
		t.Errorf("composeEnv = %v, want %v", out1, want)
	}
}

func TestComposeEnv_EmptySpecEnv(t *testing.T) {
	t.Parallel()

	parent := []string{"PATH=/usr/bin", "HOME=/home/agent"}
	out := composeEnv(parent, nil)
	if !slices.Equal(out, parent) {
		t.Errorf("composeEnv with empty spec.Env = %v, want %v", out, parent)
	}
}
