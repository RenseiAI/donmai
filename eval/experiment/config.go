package experiment

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// LoadedConfig is an operator-authored prompt-experiment definition with prompt
// bytes resolved process-locally from sibling files. Durable receipts retain only
// the resulting SHA-256 VariantRef, never the raw prompt text or file path.
type LoadedConfig struct {
	Definition Definition
	GraderIDs  []string
}

type fileConfig struct {
	ID           string            `json:"id"`
	Arms         []fileArm         `json:"arms"`
	GraderIDs    []string          `json:"graderIds"`
	ContextReset *fileContextReset `json:"contextReset,omitempty"`
}

type fileArm struct {
	ID               ArmID  `json:"id"`
	SubjectRef       string `json:"subjectRef"`
	SystemPromptFile string `json:"systemPromptFile"`
}

type fileContextReset struct {
	AfterTurn          int    `json:"afterTurn"`
	ContinuationPrompt string `json:"continuationPrompt"`
}

// LoadConfigFile strictly loads an operator-authored experiment config. Prompt
// paths must stay beneath the config directory so a reviewed fixture cannot
// silently bind an unrelated host file. Variant refs are derived from the exact
// prompt bytes read; callers never provide or override them.
func LoadConfigFile(path string) (LoadedConfig, error) {
	body, err := os.ReadFile(path) //nolint:gosec // explicit operator-supplied config path.
	if err != nil {
		return LoadedConfig{}, fmt.Errorf("read experiment config %q: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var raw fileConfig
	if err := dec.Decode(&raw); err != nil {
		return LoadedConfig{}, fmt.Errorf("decode experiment config %q: %w", path, err)
	}
	if err := requireJSONEOF(dec); err != nil {
		return LoadedConfig{}, fmt.Errorf("decode experiment config %q: %w", path, err)
	}
	if strings.TrimSpace(raw.ID) == "" {
		return LoadedConfig{}, fmt.Errorf("experiment id is required")
	}
	if len(raw.Arms) < 2 {
		return LoadedConfig{}, fmt.Errorf("experiment requires at least two arms")
	}
	if len(raw.GraderIDs) == 0 {
		return LoadedConfig{}, fmt.Errorf("experiment requires at least one grader id")
	}
	graderIDs := make([]string, 0, len(raw.GraderIDs))
	seenGraders := make(map[string]struct{}, len(raw.GraderIDs))
	for _, graderID := range raw.GraderIDs {
		graderID = strings.TrimSpace(graderID)
		if graderID == "" {
			return LoadedConfig{}, fmt.Errorf("experiment grader id is required")
		}
		if _, ok := seenGraders[graderID]; ok {
			return LoadedConfig{}, fmt.Errorf("duplicate grader id %q", graderID)
		}
		seenGraders[graderID] = struct{}{}
		graderIDs = append(graderIDs, graderID)
	}

	configDir := filepath.Dir(path)
	arms := make([]Arm, 0, len(raw.Arms))
	for _, rawArm := range raw.Arms {
		promptPath, err := fixturePath(configDir, rawArm.SystemPromptFile)
		if err != nil {
			return LoadedConfig{}, fmt.Errorf("arm %q system prompt: %w", rawArm.ID, err)
		}
		prompt, err := os.ReadFile(promptPath) //nolint:gosec // path is confined to the reviewed config directory.
		if err != nil {
			return LoadedConfig{}, fmt.Errorf("read arm %q system prompt: %w", rawArm.ID, err)
		}
		arms = append(arms, Arm{
			ID:           rawArm.ID,
			SubjectRef:   strings.TrimSpace(rawArm.SubjectRef),
			VariantRef:   SHA256VariantRef(string(prompt)),
			SystemPrompt: string(prompt),
		})
	}

	definition := Definition{ID: raw.ID, Arms: arms}
	if raw.ContextReset != nil {
		if raw.ContextReset.AfterTurn <= 0 {
			return LoadedConfig{}, fmt.Errorf("contextReset.afterTurn must be positive")
		}
		if strings.TrimSpace(raw.ContextReset.ContinuationPrompt) == "" {
			return LoadedConfig{}, fmt.Errorf("contextReset.continuationPrompt is required")
		}
		definition.Perturbations = []Perturbation{ContextResetAtTurn(
			raw.ContextReset.AfterTurn,
			raw.ContextReset.ContinuationPrompt,
		)}
	}
	if err := definition.Validate(); err != nil {
		return LoadedConfig{}, err
	}
	return LoadedConfig{Definition: definition, GraderIDs: graderIDs}, nil
}

func fixturePath(configDir, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("systemPromptFile is required")
	}
	if filepath.IsAbs(value) {
		return "", fmt.Errorf("systemPromptFile must be relative to the config directory")
	}
	clean := filepath.Clean(value)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("systemPromptFile must stay within the config directory")
	}
	base, err := filepath.Abs(configDir)
	if err != nil {
		return "", fmt.Errorf("resolve config directory: %w", err)
	}
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		return "", fmt.Errorf("resolve config directory: %w", err)
	}
	candidate, err := filepath.Abs(filepath.Join(configDir, clean))
	if err != nil {
		return "", fmt.Errorf("resolve systemPromptFile: %w", err)
	}
	candidate, err = filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve systemPromptFile: %w", err)
	}
	rel, err := filepath.Rel(base, candidate)
	if err != nil {
		return "", fmt.Errorf("compare systemPromptFile to config directory: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("systemPromptFile must stay within the config directory after resolving symlinks")
	}
	return candidate, nil
}

func requireJSONEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return err
	}
	return fmt.Errorf("multiple JSON values are not allowed")
}
