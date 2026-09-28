package attachwire

import (
	"bytes"
	"testing"
)

func TestContinuationCodecBounds(t *testing.T) {
	screen := Screen{Epoch: 3, EchoMode: EchoUnknown, Cols: 1, Rows: 1, CursorVisible: true, Primary: []Cell{{RuneBytes: []byte("X")}}}
	c := ContinuationCheckpoint{Schema: ContinuationSchema, Epoch: 3, AtSeq: 18, Picture: screen, State: []byte("engine-state")}
	b, err := c.Encode(ContinuationSchema)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeContinuationCheckpoint(b, ContinuationSchema)
	if err != nil {
		t.Fatal(err)
	}
	picture, err := screen.Encode()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := decoded.Picture.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if decoded.AtSeq != 18 || decoded.Epoch != 3 || !bytes.Equal(restored, picture) || !bytes.Equal(decoded.State, c.State) {
		t.Fatal("checkpoint round trip changed state or boundary")
	}
	for i := 0; i < len(b); i++ {
		if _, err := DecodeContinuationCheckpoint(b[:i], ContinuationSchema); err == nil {
			t.Fatalf("truncated at%d accepted", i)
		}
	}
	if _, err := DecodeContinuationCheckpoint(append(append([]byte{}, b...), 0), ContinuationSchema); err == nil {
		t.Fatal("trailing bytes accepted")
	}
}
