package present

import (
	"encoding/json"
	"os"
	"testing"
)

type vectors struct {
	Tools []struct {
		Case    string         `json:"case"`
		Name    string         `json:"name"`
		Input   map[string]any `json:"input"`
		Family  Family         `json:"family"`
		Title   string         `json:"title"`
		Detail  string         `json:"detail"`
		Summary string         `json:"summary"`
	} `json:"tools"`
	Outputs []struct {
		Case    string `json:"case"`
		Content string `json:"content"`
		Text    string `json:"text"`
	} `json:"outputs"`
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode vectors: %v", err)
	}
	if len(v.Tools) == 0 || len(v.Outputs) == 0 {
		t.Fatal("vectors are empty")
	}
	return v
}

func TestVectors(t *testing.T) {
	t.Parallel()
	v := loadVectors(t)
	for _, tc := range v.Tools {
		got := DescribeTool(tc.Name, tc.Input)
		if got.Family != tc.Family || got.Title != tc.Title || got.Detail != tc.Detail || got.Summary() != tc.Summary {
			t.Errorf("%s: DescribeTool(%q) = %+v (summary %q), want %q/%q/%q (summary %q)",
				tc.Case, tc.Name, got, got.Summary(), tc.Family, tc.Title, tc.Detail, tc.Summary)
		}
	}
	for _, tc := range v.Outputs {
		if got := OutputText(tc.Content); got != tc.Text {
			t.Errorf("%s: OutputText(%q) = %q, want %q", tc.Case, tc.Content, got, tc.Text)
		}
	}
}

func TestUnwrapShellLeavesNonWrappersAlone(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":                           "",
		"  ls  ":                     "ls",
		"bash":                       "bash",
		"bash -x script.sh":          "bash -x script.sh",
		"/usr/bin/env bash -c 'pwd'": "pwd",
		`zsh -c "a" "b"`:             `"a" "b"`,
		`sh -c 'it'\''s'`:            `'it'\''s'`,
	}
	for in, want := range cases {
		if got := UnwrapShell(in); got != want {
			t.Errorf("UnwrapShell(%q) = %q, want %q", in, got, want)
		}
	}
}
