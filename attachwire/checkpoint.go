package attachwire

import (
	"fmt"
	"unicode"
	"unicode/utf8"
)

// ContinuationSchema is an explicitly selected complete-state contract. It is
// separate from the frozen picture format. No caller may infer selection from
// receiving a picture or from sending an offer to an older peer.
const ContinuationSchema = "donmai-vt/continuation-v1"

// MaxContinuationBytes bounds the complete local checkpoint, including its
// picture projection and embedder state. Transport support is negotiated
// separately; this size is not a change to a carrier's frame limit.
const MaxContinuationBytes = 20 << 20

// ContinuationCheckpoint is an atomic host-state boundary. AtSeq and Epoch
// identify precisely the history consumed by State. Picture is for display;
// State is mandatory for faithful continuation and must never be inferred from
// Picture. This value is not published onto legacy snapshot subscriptions.
type ContinuationCheckpoint struct {
	Schema  string
	Epoch   uint64
	AtSeq   HostSeq
	Picture Screen
	State   []byte
	// Exit is present only when AtSeq is the final Exit sequence.
	Exit *ExitPayload
}

// Encode serializes a complete checkpoint only for the selected schema.
func (c ContinuationCheckpoint) Encode(selected string) ([]byte, error) {
	if err := c.ValidateBoundary(selected); err != nil {
		return nil, err
	}
	picture, err := c.Picture.Encode()
	if err != nil {
		return nil, fmt.Errorf("attachwire: continuation picture: %w", err)
	}
	b := AppendUvarint(nil, uint64(len(c.Schema)))
	b = append(b, c.Schema...)
	b = AppendUvarint(b, c.Epoch)
	b = AppendUvarint(b, uint64(c.AtSeq))
	b = AppendUvarint(b, uint64(len(picture)))
	b = append(b, picture...)
	b = AppendUvarint(b, uint64(len(c.State)))
	b = append(b, c.State...)
	var final []byte
	if c.Exit != nil {
		if c.AtSeq == 0 {
			return nil, fmt.Errorf("attachwire: final checkpoint has no Exit sequence")
		}
		final = c.Exit.Encode()
		if _, err := DecodeExit(final); err != nil {
			return nil, err
		}
	}
	b = AppendUvarint(b, uint64(len(final)))
	b = append(b, final...)
	if len(b) > MaxContinuationBytes {
		return nil, fmt.Errorf("attachwire: continuation exceeds limit")
	}
	return b, nil
}

// DecodeContinuationCheckpoint validates selection, framing, and epoch binding.
// The receiver must also validate/restore State before advancing its anchor.
func DecodeContinuationCheckpoint(b []byte, selected string) (ContinuationCheckpoint, error) {
	fail := func(err error) (ContinuationCheckpoint, error) {
		return ContinuationCheckpoint{}, fmt.Errorf("attachwire: continuation: %w", err)
	}
	if selected != ContinuationSchema || len(b) > MaxContinuationBytes {
		return fail(fmt.Errorf("unsupported schema or oversized payload"))
	}
	r := newReader(b)
	schema, err := r.lenPrefixed()
	if err != nil {
		return fail(err)
	}
	if string(schema) != selected {
		return fail(fmt.Errorf("schema mismatch"))
	}
	epoch, err := r.uvarint()
	if err != nil {
		return fail(err)
	}
	seq, err := r.uvarint()
	if err != nil {
		return fail(err)
	}
	picture, err := r.lenPrefixed()
	if err != nil {
		return fail(err)
	}
	screen, err := DecodeScreen(picture)
	if err != nil {
		return fail(err)
	}
	if screen.Epoch != epoch {
		return fail(fmt.Errorf("epoch mismatch"))
	}
	state, err := r.lenPrefixed()
	if err != nil {
		return fail(err)
	}
	if len(state) == 0 {
		return fail(fmt.Errorf("missing complete state"))
	}
	final, err := r.lenPrefixed()
	if err != nil {
		return fail(err)
	}
	var exit *ExitPayload
	if len(final) != 0 {
		if seq == 0 {
			return fail(fmt.Errorf("final checkpoint has no Exit sequence"))
		}
		payload, err := DecodeExit(final)
		if err != nil {
			return fail(err)
		}
		exit = &payload
	}
	if err := r.expectDone(); err != nil {
		return fail(err)
	}
	c := ContinuationCheckpoint{Schema: selected, Epoch: epoch, AtSeq: HostSeq(seq), Picture: screen, State: append([]byte(nil), state...), Exit: exit}
	if err := c.ValidateBoundary(selected); err != nil {
		return fail(err)
	}
	return c, nil
}

// ValidateBoundary checks schema, epoch and final-observation metadata without
// re-encoding the opaque state. Restore/import and picture validation remain
// required before committing a continuation cursor.
func (c ContinuationCheckpoint) ValidateBoundary(selected string) error {
	if selected != ContinuationSchema || c.Schema != selected {
		return fmt.Errorf("attachwire: unsupported continuation schema")
	}
	if c.Picture.Epoch != c.Epoch || len(c.State) == 0 || len(c.State) > MaxContinuationBytes {
		return fmt.Errorf("attachwire: invalid continuation boundary")
	}
	if err := c.Picture.validateHeader(); err != nil {
		return err
	}
	if c.Exit != nil {
		if c.AtSeq == 0 || len(c.Exit.Signal) > 64 || !utf8.ValidString(c.Exit.Signal) {
			return fmt.Errorf("attachwire: invalid final checkpoint observation")
		}
		for _, r := range c.Exit.Signal {
			if unicode.IsControl(r) {
				return fmt.Errorf("attachwire: control in final signal name")
			}
		}
		if _, err := DecodeExit(c.Exit.Encode()); err != nil {
			return err
		}
	}
	return nil
}
