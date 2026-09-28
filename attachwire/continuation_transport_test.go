package attachwire

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestContinuationTransportRoundTrip(t *testing.T) {
	data := []byte("abc")
	digest := sha256.Sum256(data)
	accepted := ContinuationAccepted{Schema: ContinuationSchema, RequestID: "capture-1", HostEpoch: 7, AtSeq: 20, Total: 3, SHA256: hex.EncodeToString(digest[:])}
	raw := Frame{Type: TypeOutput, Seq: 21, RelTime: 9, Payload: EncodeOutput([]byte("\x1b]0;raw\x07X"))}.Encode()
	for _, m := range []ContinuationMessage{{Kind: ContinuationAcceptedKind, Accepted: accepted}, {Kind: ContinuationCheckpointChunkKind, Data: data}, {Kind: ContinuationHostFrameKind, Data: raw}, {Kind: ContinuationErrorKind, ErrorCode: "gap"}} {
		b, err := EncodeContinuationMessage(m)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodeContinuationMessage(b)
		if err != nil {
			t.Fatal(err)
		}
		if decoded.Kind != m.Kind || decoded.Accepted != m.Accepted || decoded.Offset != m.Offset || !bytes.Equal(decoded.Data, m.Data) || decoded.ErrorCode != m.ErrorCode {
			t.Fatal("opaque phase changed during round trip")
		}
	}
	chunk := ContinuationChunk{Schema: ContinuationSchema, RequestID: "capture-1", HostEpoch: 0, Total: 3, SHA256: accepted.SHA256, Data: data}
	b, err := json.Marshal(chunk)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeContinuationChunk(b)
	if err != nil || decoded.HostEpoch != 0 || !bytes.Equal(decoded.Data, data) {
		t.Fatalf("explicit epoch zero round trip: %v", err)
	}
}

func TestContinuationTransportRefusesMalformed(t *testing.T) {
	for _, b := range []string{`{}`, `{"schema":null}`, `{"schema":"donmai-vt/continuation-v1","schema":"donmai-vt/continuation-v1"}`, `{"schema":"donmai-vt/continuation-v1","extra":1}`, `{"schema":"unknown"}`} {
		if _, err := DecodeContinuationHello([]byte(b)); err == nil {
			t.Fatalf("malformed hello accepted: %s", b)
		}
	}
	chunk := ContinuationChunk{Schema: ContinuationSchema, RequestID: "capture", Total: 1, SHA256: strings.Repeat("a", 64), Data: []byte{1}}
	b, _ := json.Marshal(chunk)
	for _, bad := range [][]byte{bytes.Replace(b, []byte(`"hostEpoch":0,`), nil, 1), bytes.Replace(b, []byte(`"hostEpoch":0`), []byte(`"hostEpoch":null`), 1), append(append([]byte{}, b...), 0)} {
		if _, err := DecodeContinuationChunk(bad); err == nil {
			t.Fatal("incomplete chunk accepted")
		}
	}
	for _, mutate := range []func(*ContinuationChunk){func(c *ContinuationChunk) { c.Offset = 1 }, func(c *ContinuationChunk) { c.Total = MaxContinuationBytes + 1 }, func(c *ContinuationChunk) { c.Data = nil }, func(c *ContinuationChunk) { c.RequestID = "../escape" }, func(c *ContinuationChunk) { c.SHA256 = strings.Repeat("A", 64) }} {
		c := chunk
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Fatal("invalid upload chunk accepted")
		}
	}
	for _, bad := range [][]byte{nil, {99, 1}, {byte(ContinuationCheckpointChunkKind), 0, 0, 0, 0}, append([]byte{byte(ContinuationCheckpointChunkKind), 0, 0, 0, 0}, make([]byte, ContinuationChunkBytes+1)...), append([]byte{byte(ContinuationErrorKind)}, []byte("arbitrary-detail")...)} {
		if _, err := DecodeContinuationMessage(bad); err == nil {
			t.Fatal("invalid rail phase accepted")
		}
	}
	for _, f := range []Frame{{Type: TypeOutput, Seq: 0}, {Type: TypeInput, Seq: 1}} {
		if _, err := EncodeContinuationMessage(ContinuationMessage{Kind: ContinuationHostFrameKind, Data: f.Encode()}); err == nil {
			t.Fatal("noncanonical tail accepted")
		}
	}
	if _, err := DecodeContinuationChunkAck([]byte(`{"nextOffset":1}`)); err == nil {
		t.Fatal("missing completion acknowledgement accepted")
	}
}

func TestContinuationHostFramePreservesNonminimalOriginalVarint(t *testing.T) {
	raw := []byte{0x01, 0x81, 0x00, 0x00, 'X'}
	if _, err := DecodeFrame(raw); err != nil {
		t.Fatalf("frozen frame decoder rejected fixture: %v", err)
	}
	encoded, err := EncodeContinuationMessage(ContinuationMessage{Kind: ContinuationHostFrameKind, Data: raw})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeContinuationMessage(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded.Data, raw) {
		t.Fatal("opaque rail normalized original receipt bytes")
	}
}

func TestContinuationUploadGrantIsRequiredAndRedacted(t *testing.T) {
	grant := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xab}, 32))
	request := ContinuationRequest{Schema: ContinuationSchema, RequestID: "request", UploadGrant: grant}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "not-a-grant", grant + "=", strings.Repeat("A", 42)} {
		r := request
		r.UploadGrant = bad
		if err := r.Validate(); err == nil {
			t.Fatal("absent/malformed upload grant accepted")
		}
	}
	control := SnapshotRequest{Reason: ReasonJoin, Continuation: &request}
	for _, value := range []any{request, &request, control, &control} {
		if formatted := fmt.Sprintf("%+v", value); strings.Contains(formatted, grant) {
			t.Fatal("grant reached formatted log")
		}
		var out bytes.Buffer
		slog.New(slog.NewJSONHandler(&out, nil)).Info("control", "value", value)
		if strings.Contains(out.String(), grant) {
			t.Fatal("grant reached structured log")
		}
	}
	wire, err := json.Marshal(control)
	if err != nil || !bytes.Contains(wire, []byte(grant)) {
		t.Fatal("host control did not carry grant")
	}
	viewer := ContinuationAccepted{Schema: ContinuationSchema, RequestID: request.RequestID, HostEpoch: 1, AtSeq: 2, Total: 1, SHA256: strings.Repeat("a", 64)}
	frame, err := EncodeContinuationMessage(ContinuationMessage{Kind: ContinuationAcceptedKind, Accepted: viewer})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(frame, []byte(grant)) {
		t.Fatal("grant leaked onto viewer rail")
	}
}
