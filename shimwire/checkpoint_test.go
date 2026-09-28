package shimwire

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/RenseiAI/donmai/attachwire"
)

func TestV5CheckpointVocabularyAndBounds(t *testing.T) {
	for _, mt := range []MessageType{TypeCheckpointRequest, TypeCheckpointResult} {
		for _, v := range []uint32{V1, V2, V3, V4} {
			if mt.AllowedIn(v) {
				t.Fatalf("%s leaked into v%d", mt, v)
			}
		}
		if !mt.AllowedIn(V5) {
			t.Fatalf("%s missing from v5", mt)
		}
	}
	request := CheckpointRequest{RequestID: 1, Generation: 2, Schema: attachwire.ContinuationSchema}
	b, err := EncodeCheckpointRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeCheckpointRequest(b)
	if err != nil || decoded != request {
		t.Fatalf("round trip=%+v %v", decoded, err)
	}
	var framed bytes.Buffer
	if err := NewWriter(&framed).WriteVersion(V5, TypeCheckpointRequest, b); err != nil {
		t.Fatal(err)
	}
	if _, err := NewReader(bytes.NewReader(framed.Bytes())).ReadVersion(V4); err == nil {
		t.Fatal("v4 admitted continuation message")
	}
	chunk := CheckpointResult{RequestID: 1, Generation: 2, Total: CheckpointChunkBytes, Bytes: make([]byte, CheckpointChunkBytes)}
	encoded, err := EncodeCheckpointResult(chunk)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded)+1 >= MaxMessageBytes {
		t.Fatal("chunk exceeded frozen frame bound")
	}
	got, err := DecodeCheckpointResult(encoded)
	if err != nil || !bytes.Equal(got.Bytes, chunk.Bytes) {
		t.Fatal("chunk round trip failed")
	}
	for _, mutate := range []func(*CheckpointResult){func(c *CheckpointResult) { c.Total = attachwire.MaxContinuationBytes + 1 }, func(c *CheckpointResult) { c.Offset = c.Total + 1 }, func(c *CheckpointResult) { c.Bytes = make([]byte, CheckpointChunkBytes+1) }, func(c *CheckpointResult) { c.Code = CodeInternal }} {
		c := chunk
		mutate(&c)
		if _, err := EncodeCheckpointResult(c); err == nil {
			t.Fatal("malformed chunk accepted")
		}
	}
}

func TestCheckpointCapabilityRequiresExplicitEpoch(t *testing.T) {
	for _, b := range []string{`{"schema":"donmai-vt/continuation-v1"}`, `{"schema":"donmai-vt/continuation-v1","hostEpoch":null}`, `{"hostEpoch":0}`} {
		var c CheckpointCapability
		if err := json.Unmarshal([]byte(b), &c); err == nil {
			t.Fatalf("incomplete capability accepted: %s", b)
		}
	}
	var c CheckpointCapability
	if err := json.Unmarshal([]byte(`{"schema":"donmai-vt/continuation-v1","hostEpoch":0}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.HostEpoch != 0 || c.Schema != attachwire.ContinuationSchema {
		t.Fatal("explicit epoch zero was changed")
	}
}
