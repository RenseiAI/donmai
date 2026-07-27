package experiment

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigFileBindsPromptFilesAndContextReset(t *testing.T) {
	dir := t.TempDir()
	incumbent := "incumbent prompt\n"
	candidate := "candidate prompt\n"
	if err := os.WriteFile(filepath.Join(dir, "incumbent.txt"), []byte(incumbent), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "candidate.txt"), []byte(candidate), 0o600); err != nil {
		t.Fatal(err)
	}
	config := `{
		"id":"durability-v1",
		"arms":[
			{"id":"incumbent","subjectRef":"agent/base","systemPromptFile":"incumbent.txt"},
			{"id":"candidate","subjectRef":"agent/base","systemPromptFile":"candidate.txt"}
		],
		"graderIds":["behavior/recovery-v1"],
		"contextReset":{"afterTurn":4,"continuationPrompt":"Recover from durable state and finish."}
	}`
	path := filepath.Join(dir, "experiment.json")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadConfigFile(path)
	if err != nil {
		t.Fatalf("LoadConfigFile: %v", err)
	}
	if loaded.Definition.ID != "durability-v1" || len(loaded.Definition.Arms) != 2 {
		t.Fatalf("definition = %+v", loaded.Definition)
	}
	if loaded.Definition.Arms[0].SystemPrompt != incumbent || loaded.Definition.Arms[1].SystemPrompt != candidate {
		t.Fatalf("prompt bytes changed: %+v", loaded.Definition.Arms)
	}
	for _, arm := range loaded.Definition.Arms {
		if arm.VariantRef != SHA256VariantRef(arm.SystemPrompt) {
			t.Fatalf("arm %s variant ref was not bound to prompt bytes", arm.ID)
		}
	}
	if len(loaded.Definition.Perturbations) != 1 || loaded.Definition.Perturbations[0].Name() != "context-reset" {
		t.Fatalf("perturbations = %+v", loaded.Definition.Perturbations)
	}
	if len(loaded.GraderIDs) != 1 || loaded.GraderIDs[0] != "behavior/recovery-v1" {
		t.Fatalf("grader ids = %v", loaded.GraderIDs)
	}
}

func TestLoadConfigFileRejectsPromptSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("private prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escaped.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "inside.txt"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := `{"id":"x","arms":[{"id":"a","subjectRef":"s","systemPromptFile":"escaped.txt"},{"id":"b","subjectRef":"s","systemPromptFile":"inside.txt"}],"graderIds":["behavior/recovery-v1"]}`
	path := filepath.Join(dir, "experiment.json")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfigFile(path)
	if err == nil || !strings.Contains(err.Error(), "resolving symlinks") {
		t.Fatalf("error = %v, want symlink confinement failure", err)
	}
}

func TestLoadConfigFileFailsClosed(t *testing.T) {
	tests := []struct {
		name   string
		config string
		want   string
	}{
		{
			name:   "unknown field",
			config: `{"id":"x","arms":[],"graderIds":[],"surprise":true}`,
			want:   "unknown field",
		},
		{
			name:   "missing grader",
			config: `{"id":"x","arms":[{"id":"a","subjectRef":"s","systemPromptFile":"a.txt"},{"id":"b","subjectRef":"s","systemPromptFile":"b.txt"}],"graderIds":[]}`,
			want:   "grader",
		},
		{
			name:   "absolute prompt path",
			config: `{"id":"x","arms":[{"id":"a","subjectRef":"s","systemPromptFile":"/tmp/a.txt"},{"id":"b","subjectRef":"s","systemPromptFile":"b.txt"}],"graderIds":["behavior/recovery-v1"]}`,
			want:   "relative",
		},
		{
			name:   "trailing json value",
			config: `{"id":"x","arms":[],"graderIds":[]} {}`,
			want:   "multiple json values",
		},
		{
			name:   "invalid reset checkpoint",
			config: `{"id":"x","arms":[{"id":"a","subjectRef":"s","systemPromptFile":"a.txt"},{"id":"b","subjectRef":"s","systemPromptFile":"b.txt"}],"graderIds":["behavior/recovery-v1"],"contextReset":{"afterTurn":0,"continuationPrompt":"recover"}}`,
			want:   "afterturn",
		},
		{
			name:   "missing reset continuation",
			config: `{"id":"x","arms":[{"id":"a","subjectRef":"s","systemPromptFile":"a.txt"},{"id":"b","subjectRef":"s","systemPromptFile":"b.txt"}],"graderIds":["behavior/recovery-v1"],"contextReset":{"afterTurn":4,"continuationPrompt":" "}}`,
			want:   "continuationprompt",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range []string{"a.txt", "b.txt"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(dir, "experiment.json")
			if err := os.WriteFile(path, []byte(tt.config), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadConfigFile(path)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.want)) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}
