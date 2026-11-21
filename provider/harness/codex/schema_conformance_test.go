package codex

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/RenseiAI/donmai/agent"
)

// compileParamSchema compiles one vendored request param schema
// (testdata/schema, refreshed by scripts/generate-codex-notification-list.py).
func compileParamSchema(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	path := filepath.Join("testdata", "schema", name+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(name+".json", doc); err != nil {
		t.Fatalf("add %s: %v", name, err)
	}
	s, err := c.Compile(name + ".json")
	if err != nil {
		t.Fatalf("compile %s: %v", name, err)
	}
	return s
}

func validateParams(t *testing.T, s *jsonschema.Schema, label string, params map[string]any) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("%s: marshal: %v", label, err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("%s: decode: %v", label, err)
	}
	if err := s.Validate(inst); err != nil {
		t.Errorf("%s does not satisfy the vendored Codex schema: %v\nparams: %s", label, err, raw)
	}
}

// TestCodexRequestsSatisfyVendoredSchemas validates every request the
// translator builds, across the Spec matrix, against the param schemas of the
// pinned Codex version: values, enums and nested shapes, not only key names
// (TestCodexRequestParamDrift covers those).
func TestCodexRequestsSatisfyVendoredSchemas(t *testing.T) {
	t.Parallel()
	threadStart := compileParamSchema(t, "ThreadStartParams")
	threadResume := compileParamSchema(t, "ThreadResumeParams")
	turnStart := compileParamSchema(t, "TurnStartParams")
	efforts := []agent.EffortLevel{"", agent.EffortLow, agent.EffortMedium, agent.EffortHigh, agent.EffortXHigh, agent.EffortMax}
	sandboxes := []agent.SandboxLevel{"", agent.SandboxReadOnly, agent.SandboxWorkspaceWrite, agent.SandboxFullAccess}
	for _, effort := range efforts {
		for _, sandbox := range sandboxes {
			for _, autonomous := range []bool{false, true} {
				spec := agent.Spec{
					Cwd: "/tmp/w", Model: "gpt-5.6-sol", Effort: effort, SandboxLevel: sandbox, Autonomous: autonomous,
					BaseInstructions: "base", SystemPromptAppend: "append", Prompt: "go",
					ResponseSchema: json.RawMessage(`{"type":"object"}`),
				}
				label := string(effort) + "/" + string(sandbox)
				plan := NewSpawnPlan(spec)
				validateParams(t, threadStart, "thread/start "+label, plan.ThreadStart)
				validateParams(t, turnStart, "turn/start "+label, turnStartParams("thread-1", spec, plan.PromptInput))
			}
		}
	}
	validateParams(t, threadResume, "thread/resume", threadResumeParams("thread-1"))
}

// TestVendoredSchemasMatchInventory pins the vendored schemas and the
// protocol inventory to one Codex release, so a version bump that refreshes
// only one of them fails.
func TestVendoredSchemasMatchInventory(t *testing.T) {
	t.Parallel()
	read := func(path string) map[string]any {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	provenance := read(filepath.Join("testdata", "schema", "PROVENANCE.json"))
	inventory := read(filepath.Join("testdata", "protocol-inventory.json"))
	if provenance["generator"] == nil || provenance["generator"] != inventory["generator"] {
		t.Fatalf("schema generator %v != inventory generator %v; re-run scripts/generate-codex-notification-list.py", provenance["generator"], inventory["generator"])
	}
	for _, name := range []string{"LICENSE", "NOTICE"} {
		if info, err := os.Stat(filepath.Join("testdata", "schema", name)); err != nil || info.Size() == 0 {
			t.Errorf("vendored Codex %s missing: %v", name, err)
		}
	}
}
