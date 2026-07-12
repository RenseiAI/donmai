package attachwire

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestViewerInputBatchJSON(t *testing.T) {
	in1 := EncodeFrameBase64(Frame{Type: TypeInput, Payload: EncodeViewerInput(1, 0, []byte("a"))})
	batch := ViewerInputBatch{
		BatchID:       "b-1",
		FirstInputSeq: i64(1),
		LastInputSeq:  i64(1),
		Inputs:        []string{in1},
		Controls:      []string{},
	}
	raw, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	var got ViewerInputBatch
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, batch) {
		t.Fatalf("viewer batch round trip: got %#v want %#v", got, batch)
	}
}

func TestViewerInputBatchControlOnlyOmitsSeq(t *testing.T) {
	// A control-only batch omits firstInputSeq/lastInputSeq (§14).
	ctrl, _ := MarshalControl(Grab{})
	batch := ViewerInputBatch{
		BatchID:  "b-2",
		Inputs:   []string{},
		Controls: []string{EncodeFrameBase64(NewControlFrame(EncodeControlPayload(ctrl)))},
	}
	raw, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	if _, present := obj["firstInputSeq"]; present {
		t.Fatalf("control-only batch must omit firstInputSeq")
	}
	if _, present := obj["lastInputSeq"]; present {
		t.Fatalf("control-only batch must omit lastInputSeq")
	}
}

func TestHostFrameBatchJSON(t *testing.T) {
	frame := EncodeFrameBase64(Frame{Type: TypeOutput, Seq: 5, RelTime: 100, Payload: []byte("out")})
	batch := HostFrameBatch{
		BatchID:  "hb-1",
		FirstSeq: 5,
		LastSeq:  5,
		Frames:   []string{frame},
		OutOfSeq: []string{},
	}
	raw, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	var got HostFrameBatch
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, batch) {
		t.Fatalf("host batch round trip: got %#v want %#v", got, batch)
	}
	// The carried frame decodes back.
	f, err := DecodeFrameBase64(got.Frames[0])
	if err != nil {
		t.Fatal(err)
	}
	if f.Type != TypeOutput || f.Seq != 5 || f.RelTime != 100 || string(f.Payload) != "out" {
		t.Fatalf("carried frame mismatch: %#v", f)
	}
}

func TestPostResponseTaxonomyTypes(t *testing.T) {
	cases := []struct {
		v    any
		want string
	}{
		{InputBatchAccepted{BatchID: "b", AckInputSeq: 9}, `{"batchId":"b","ackInputSeq":9}`},
		{InputBatchRejected{BatchID: "b", AckInputSeq: 3}, `{"batchId":"b","ackInputSeq":3}`},
		{HostBatchAccepted{BatchID: "h", AckSeq: 12}, `{"batchId":"h","ackSeq":12}`},
		{HostBatchRejected{BatchID: "h", AckSeq: 4}, `{"batchId":"h","ackSeq":4}`},
	}
	for _, c := range cases {
		raw, err := json.Marshal(c.v)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != c.want {
			t.Fatalf("marshal %T = %s, want %s", c.v, raw, c.want)
		}
	}
}

func TestDecodeFrameBase64Invalid(t *testing.T) {
	if _, err := DecodeFrameBase64("!!!not base64!!!"); !IsFramingErr(err) {
		t.Fatalf("invalid base64 must be a framing error, got %v", err)
	}
	// valid base64, but an unknown frame type byte.
	bad := FrameBase64Encoding.EncodeToString([]byte{0x00, 0x00, 0x00})
	if _, err := DecodeFrameBase64(bad); !IsFramingErr(err) {
		t.Fatalf("undecodable frame must be a framing error, got %v", err)
	}
}
