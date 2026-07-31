package hostrelay

import (
	"bytes"
	"errors"
	"testing"
)

func TestEncodeDecodeRequestPreservesAuthorization(t *testing.T) {
	request := Request{
		RequestID:         "request-1",
		Method:            "POST",
		Path:              "/v1/tools/call",
		Headers:           []Header{{Name: "Authorization", Values: []string{"Bearer abc.def-_~"}}},
		Body:              []byte(`{"tool":"repo_map"}`),
		DeadlineUnixMilli: 42,
	}

	first, err := Encode(request)
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	second, err := Encode(request)
	if err != nil {
		t.Fatalf("Encode() second error = %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("Encode() is not deterministic:\nfirst:  %s\nsecond: %s", first, second)
	}

	decoded, err := Decode(first)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	got, ok := decoded.(*Request)
	if !ok {
		t.Fatalf("Decode() type = %T, want *Request", decoded)
	}
	if got.Headers[0].Values[0] != request.Headers[0].Values[0] {
		t.Errorf("Authorization = %q, want byte-identical %q", got.Headers[0].Values[0], request.Headers[0].Values[0])
	}
}

func TestRequestBoundsAndAllowlist(t *testing.T) {
	request := Request{
		RequestID:         "request-1",
		Method:            "GET",
		Path:              "/v1/tools/call",
		DeadlineUnixMilli: 1,
	}
	if _, err := Encode(request); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("Encode(GET request) error = %v, want ErrInvalidMessage", err)
	}

	request.Method = "POST"
	request.Body = make([]byte, MaxRequestBodyBytes+1)
	if _, err := Encode(request); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("Encode(oversized request) error = %v, want ErrInvalidMessage", err)
	}
}

func TestDecodeRejectsUnknownFieldsAndTypes(t *testing.T) {
	if _, err := Decode([]byte(`{"type":"request","payload":{"requestId":"request-1","method":"POST","path":"/v1/tools/call","deadlineUnixMilli":1,"extra":true}}`)); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("Decode(unknown field) error = %v, want ErrInvalidMessage", err)
	}
	if _, err := Decode([]byte(`{"type":"replay","payload":{}}`)); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("Decode(unknown type) error = %v, want ErrInvalidMessage", err)
	}
	if _, err := Decode([]byte(`{"type":"ping","payload":{"nonce":1}} {}`)); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("Decode(trailing JSON) error = %v, want ErrInvalidMessage", err)
	}
}

func TestHelloRequiresBoundedV1Contract(t *testing.T) {
	hello := Hello{
		Workload: Workload{OrgID: "org", PoolID: "pool", WorkerHostID: "host", WorkloadID: "workload"},
		Version:  Version, LocalRoute: "/v1/tools/call", MaxInFlight: DefaultMaxInFlight, Ready: true,
	}
	if _, err := Encode(hello); err != nil {
		t.Fatalf("Encode(Hello) error = %v", err)
	}
	hello.MaxInFlight++
	if _, err := Encode(hello); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("Encode(over-cap Hello) error = %v, want ErrInvalidMessage", err)
	}
}
