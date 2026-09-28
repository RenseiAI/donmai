package shimwire

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/attachwire"
)

func TestHelloCheckpointUsesOptionalExtension(t *testing.T) {
	original := map[string]string{ExtCarrierEpoch: "carrier-17"}
	h := Hello{Min: V1, Max: V5, Extensions: Extensions{Values: original}, Continuation: &CheckpointCapability{Schema: attachwire.ContinuationSchema, HostEpoch: 19}}
	raw, err := EncodeHello(h)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["continuation"]; ok {
		t.Fatal("preselection Hello has a new top-level field")
	}
	if _, ok := original[ExtContinuationCheckpoint]; ok {
		t.Fatal("encoding mutated caller extensions")
	}
	decoded, err := DecodeHello(raw)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Continuation == nil || *decoded.Continuation != *h.Continuation {
		t.Fatalf("capability lost: %+v", decoded.Continuation)
	}
	if decoded.Extensions.Values[ExtCarrierEpoch] != "carrier-17" || len(decoded.Extensions.Required) != 0 {
		t.Fatal("carrier or optional negotiation changed")
	}
	conflict := h
	conflict.Extensions = Extensions{Values: map[string]string{ExtContinuationCheckpoint: `{"schema":"different","hostEpoch":19}`}}
	if _, err := EncodeHello(conflict); err == nil {
		t.Fatal("conflicting capability sources accepted")
	}
	again, err := EncodeHello(decoded)
	if err != nil || !bytes.Equal(raw, again) {
		t.Fatalf("round trip: %v", err)
	}
	absent, err := DecodeHello([]byte(`{"protocolMin":1,"protocolMax":5}`))
	if err != nil || absent.Continuation != nil {
		t.Fatal("max version inferred capability")
	}
}

func TestHelloCheckpointMalformedExtension(t *testing.T) {
	for _, value := range []string{"", "null", `{}`, `{"schema":"x"}`, `{"schema":"x","hostEpoch":null}`, `{"schema":"x","hostEpoch":-1}`, `{"schema":"x","hostEpoch":1,"unknown":true}`, `{"schema":"x","hostEpoch":1} {}`, strings.Repeat("x", MaxCheckpointCapabilityBytes+1)} {
		t.Run(value, func(t *testing.T) {
			raw, err := json.Marshal(Hello{Extensions: Extensions{Values: map[string]string{ExtContinuationCheckpoint: value}}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeHello(raw); err == nil {
				t.Fatal("malformed capability accepted")
			}
		})
	}
	if _, err := DecodeHello([]byte(`{"continuation":{"schema":"x","hostEpoch":1}}`)); err == nil {
		t.Fatal("abandoned top-level advertisement accepted")
	}
	if err := (Extensions{Required: []string{ExtContinuationCheckpoint}}).CheckRequired(); err == nil {
		t.Fatal("optional-only advertisement became a requirement")
	}
}
