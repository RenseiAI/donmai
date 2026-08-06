package runner

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestResolvedProfileHarnessWireRoundTrip(t *testing.T) {
	t.Parallel()
	in := ResolvedProfile{
		Harness:  "agy",
		Provider: agent.ProviderGemini,
		Model:    "gemini-3.1-pro",
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out ResolvedProfile
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round trip = %+v, want %+v", out, in)
	}
}
