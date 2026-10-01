package daemon

import (
	"encoding/json"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"gopkg.in/yaml.v3"
)

func TestLocalExecutionSecurityExplicitSeedAndClosedRead(t *testing.T) {
	t.Parallel()
	seed := InitialLocalExecutionSecurity()
	if err := seed.Validate(); err != nil {
		t.Fatal(err)
	}
	if seed.Levels().ToolApproval != agent.ToolApprovalBypass || seed.Levels().Isolation != agent.IsolationHostUser {
		t.Fatal("visible seed changed")
	}
	raw, err := yaml.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	var decoded LocalExecutionSecurity
	if err = yaml.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded != *seed {
		t.Fatal("YAML seed roundtrip changed authored levels")
	}
	cfg := &Config{APIVersion: LocalRuntimeConfigAPIVersion, LocalRuntime: &LocalRuntimeConfig{ExecutionSecurity: seed}}
	if err = ValidateLocalExecutionSecurity(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.LocalRuntime.ExecutionSecurity = nil
	if err = ValidateLocalExecutionSecurity(cfg); agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityUnconfigured {
		t.Fatalf("missing own policy was not refused: %v", err)
	}
	cfg.LocalRuntime.ExecutionSecurity = InitialLocalExecutionSecurity()
	cfg.LocalRuntime.ExecutionSecurity.Network = ""
	if err = ValidateLocalExecutionSecurity(cfg); agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityUnconfigured {
		t.Fatalf("deleted dimension was not refused: %v", err)
	}
}

func TestLocalExecutionSecurityRejectsUnknownAndAbsentDimensions(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(InitialLocalExecutionSecurity())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"missing", "unknown-dimension", "unknown-level", "null"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			clone := map[string]any{}
			for key, value := range fields {
				clone[key] = value
			}
			switch change {
			case "missing":
				delete(clone, "network")
			case "unknown-dimension":
				clone["other"] = "open"
			case "unknown-level":
				clone["network"] = "sometimes"
			case "null":
				clone["network"] = nil
			}
			changed, err := json.Marshal(clone)
			if err != nil {
				t.Fatal(err)
			}
			var policy LocalExecutionSecurity
			if err = json.Unmarshal(changed, &policy); err == nil {
				t.Fatal("malformed own policy obtained an implicit level")
			}
		})
	}
}
