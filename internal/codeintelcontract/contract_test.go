package codeintelcontract

import (
	"encoding/json"
	"testing"
)

func TestDescriptorsAreClosedUniqueAndCloned(t *testing.T) {
	values := Descriptors()
	if len(values) != 6 {
		t.Fatalf("descriptor count = %d, want 6", len(values))
	}
	seen := map[string]bool{}
	for _, value := range values {
		if value.Name == "" || value.Description == "" || !json.Valid(value.InputSchema) || seen[value.Name] {
			t.Fatalf("malformed descriptor: %+v", value)
		}
		seen[value.Name] = true
	}
	values[0].InputSchema[0] = 'x'
	if !json.Valid(Descriptors()[0].InputSchema) {
		t.Fatal("descriptor schema aliases caller memory")
	}
}
