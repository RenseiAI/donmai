package attachwire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/rand"
	"testing"
)

// TestVarintFixtureTableByteExact reproduces EVERY row of the §2.1 boundary-value
// fixture table byte-exactly, both directions. This is the authoritative varint
// conformance gate.
func TestVarintFixtureTableByteExact(t *testing.T) {
	rows := []struct {
		value uint64
		bytes []byte
	}{
		{0, []byte{0x00}},
		{127, []byte{0x7F}},
		{128, []byte{0x80, 0x01}},
		{16383, []byte{0xFF, 0x7F}},
		{16384, []byte{0x80, 0x80, 0x01}},
		{4294967295, []byte{0xFF, 0xFF, 0xFF, 0xFF, 0x0F}},
		{18446744073709551615, []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x01}},
	}
	for _, row := range rows {
		// Encode direction: value -> bytes.
		got := AppendUvarint(nil, row.value)
		if !bytes.Equal(got, row.bytes) {
			t.Errorf("AppendUvarint(%d) = % X, want % X", row.value, got, row.bytes)
		}
		// PutUvarint must agree with AppendUvarint.
		buf := make([]byte, MaxVarintLen)
		n := PutUvarint(buf, row.value)
		if !bytes.Equal(buf[:n], row.bytes) {
			t.Errorf("PutUvarint(%d) = % X, want % X", row.value, buf[:n], row.bytes)
		}
		// Decode direction: bytes -> value.
		v, consumed, err := Uvarint(row.bytes)
		if err != nil {
			t.Errorf("Uvarint(% X) unexpected error: %v", row.bytes, err)
			continue
		}
		if v != row.value {
			t.Errorf("Uvarint(% X) = %d, want %d", row.bytes, v, row.value)
		}
		if consumed != len(row.bytes) {
			t.Errorf("Uvarint(% X) consumed %d, want %d", row.bytes, consumed, len(row.bytes))
		}
	}
}

func TestVarintByteIdenticalToStdlib(t *testing.T) {
	rng := rand.New(rand.NewSource(0x5eed)) //nolint:gosec // G404: non-cryptographic randomness for test data
	for i := 0; i < 20000; i++ {
		x := rng.Uint64()
		// Bias toward small values and boundaries too.
		switch i % 4 {
		case 1:
			x &= 0x7F
		case 2:
			x &= 0x3FFF
		case 3:
			x >>= uint(rng.Intn(64))
		}
		mine := AppendUvarint(nil, x)
		std := binary.AppendUvarint(nil, x)
		if !bytes.Equal(mine, std) {
			t.Fatalf("encode of %d: mine=% X std=% X", x, mine, std)
		}
		v, n, err := Uvarint(mine)
		if err != nil || v != x || n != len(mine) {
			t.Fatalf("decode of %d: v=%d n=%d err=%v", x, v, n, err)
		}
	}
}

func TestVarintOverflow(t *testing.T) {
	cases := map[string][]byte{
		"eleven continuation bytes": {0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x01},
		"tenth byte above top bit":  {0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x02},
	}
	for name, buf := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := Uvarint(buf)
			if !errors.Is(err, ErrVarintOverflow) {
				t.Fatalf("want ErrVarintOverflow, got %v", err)
			}
			if !IsFramingErr(err) {
				t.Fatalf("overflow must classify as a framing error")
			}
		})
	}
}

func TestVarintTruncation(t *testing.T) {
	cases := map[string][]byte{
		"empty buffer":            {},
		"single continuation":     {0x80},
		"nine continuation bytes": {0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80},
	}
	for name, buf := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := Uvarint(buf)
			if !errors.Is(err, ErrVarintTruncated) {
				t.Fatalf("want ErrVarintTruncated, got %v", err)
			}
			if !IsFramingErr(err) {
				t.Fatalf("truncation must classify as a framing error")
			}
		})
	}
}

func TestFramingErrorCodeAndUnwrap(t *testing.T) {
	if ErrVarintOverflow.Code() != CodeFraming {
		t.Fatalf("framing error code = %q, want %q", ErrVarintOverflow.Code(), CodeFraming)
	}
	wrapped := &FramingError{Reason: "outer", cause: ErrVarintTruncated}
	if !errors.Is(wrapped, ErrVarintTruncated) {
		t.Fatalf("errors.Is should traverse the wrapped cause")
	}
	if IsFramingErr(errors.New("plain")) {
		t.Fatalf("a plain error must not be a framing error")
	}
}
