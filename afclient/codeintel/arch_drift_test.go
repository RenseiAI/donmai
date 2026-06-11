package codeintel

// arch_drift_test.go — pins the drift-seam CONTRACT (types + schema). There is
// deliberately no adapter implementation under test: per
// ADR-2026-06-07-intelligence-implementation-is-platform.md the OSS tree ships
// the ModelAdapter seam only; implementations live platform-side.

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// compileDriftSchema compiles DriftVerdictSchema, failing the test on any
// malformed-schema regression.
func compileDriftSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(DriftVerdictSchema))
	if err != nil {
		t.Fatalf("DriftVerdictSchema is not valid JSON: %v", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("archdrift://verdict-schema", doc); err != nil {
		t.Fatalf("AddResource: %v", err)
	}
	sch, err := c.Compile("archdrift://verdict-schema")
	if err != nil {
		t.Fatalf("DriftVerdictSchema does not compile as a JSON Schema: %v", err)
	}
	return sch
}

// TestDriftVerdictSchema_Validation pins the schema contract a ModelAdapter
// implementation's output is certified against.
func TestDriftVerdictSchema_Validation(t *testing.T) {
	t.Parallel()
	sch := compileDriftSchema(t)

	tests := []struct {
		name    string
		payload string
		valid   bool
	}{
		{
			name:    "critical deviation",
			payload: `{"deviations":[{"observation":"global state","severity":"critical","rationale":"violates no-globals"}]}`,
			valid:   true,
		},
		{
			name:    "empty deviations",
			payload: `{"deviations":[]}`,
			valid:   true,
		},
		{
			name:    "optional citation",
			payload: `{"deviations":[{"observation":"x","severity":"info","rationale":"r","citation":"doc §2"}]}`,
			valid:   true,
		},
		{
			name:    "missing required severity",
			payload: `{"deviations":[{"observation":"x","rationale":"r"}]}`,
			valid:   false,
		},
		{
			name:    "severity outside enum",
			payload: `{"deviations":[{"observation":"x","severity":"fatal","rationale":"r"}]}`,
			valid:   false,
		},
		{
			name:    "missing deviations key",
			payload: `{"verdict":"ok"}`,
			valid:   false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader([]byte(tc.payload)))
			if err != nil {
				t.Fatalf("payload is not valid JSON: %v", err)
			}
			err = sch.Validate(doc)
			if tc.valid && err != nil {
				t.Errorf("payload should validate, got: %v", err)
			}
			if !tc.valid && err == nil {
				t.Error("payload should NOT validate, but did")
			}
		})
	}
}

// TestDeviation_WireShape pins the JSON field names Deviation marshals to —
// they must stay compatible with DriftVerdictSchema (and the legacy TS shape)
// so a platform-side ModelAdapter and this contract never skew.
func TestDeviation_WireShape(t *testing.T) {
	t.Parallel()

	d := Deviation{
		Observation: "global state",
		Severity:    SeverityCritical,
		Rationale:   "violates no-globals",
		Citation:    "conventions §1",
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"observation", "severity", "rationale", "citation"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("Deviation wire shape missing %q: %s", key, raw)
		}
	}

	// A schema-valid verdict document must decode into the typed contract.
	verdict := []byte(`{"deviations":[{"observation":"x","severity":"warning","rationale":"r"}]}`)
	sch := compileDriftSchema(t)
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(verdict))
	if err != nil {
		t.Fatalf("verdict not valid JSON: %v", err)
	}
	if err := sch.Validate(doc); err != nil {
		t.Fatalf("verdict should validate: %v", err)
	}
	var parsed struct {
		Deviations []Deviation `json:"deviations"`
	}
	if err := json.Unmarshal(verdict, &parsed); err != nil {
		t.Fatalf("typed decode: %v", err)
	}
	if len(parsed.Deviations) != 1 || parsed.Deviations[0].Severity != SeverityWarning {
		t.Fatalf("typed decode mismatch: %+v", parsed.Deviations)
	}
}
