package executioncell

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

func TestGeneratedArtifactDigestsAreCurrent(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, want string
		raw        []byte
	}{
		{"contract schema", ContractSchemaSHA256, contractSchemaJSON},
		{"fixture suite", FixtureSuiteSHA256, fixtureSuiteJSON},
	} {
		t.Run(test.name, func(t *testing.T) {
			digest := sha256.Sum256(test.raw)
			if got := hex.EncodeToString(digest[:]); got != test.want {
				t.Fatalf("artifact digest = %s, want generated %s; run go generate ./executioncell", got, test.want)
			}
		})
	}
}

func TestEveryNamedSchemaCompiles(t *testing.T) {
	t.Parallel()
	var envelope schemaEnvelope
	if err := json.Unmarshal(contractSchemaJSON, &envelope); err != nil {
		t.Fatalf("decode schema envelope: %v", err)
	}
	if len(envelope.Schemas) != 15 {
		t.Fatalf("named schema count = %d, want 15", len(envelope.Schemas))
	}
	for name := range envelope.Schemas {
		if _, err := schemaFor(name); err != nil {
			t.Errorf("compile %s: %v", name, err)
		}
	}
}
