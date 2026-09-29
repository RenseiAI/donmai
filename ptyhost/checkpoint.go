package ptyhost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/attachwire"
)

type hostCheckpointState struct {
	Schema        string
	Engine        []byte
	FilterState   oscTitleState
	FilterPending []byte
	FilterUTF8    int
	FilterEsc     bool
	Modes         [10]bool
}

func (v *vtHost) checkpoint() ([]byte, error) {
	// Session serializes checkpoints after write drains all synthetic re-feeds.
	if len(v.pendingFeed) != 0 {
		return nil, fmt.Errorf("ptyhost: checkpoint during pending feed")
	}
	engine, err := v.emu.ExportCheckpoint()
	if err != nil {
		return nil, fmt.Errorf("ptyhost: export engine: %w", err)
	}
	c := hostCheckpointState{Schema: attachwire.ContinuationSchema, Engine: engine, FilterState: v.feed.state, FilterPending: v.feed.pending, FilterUTF8: v.feed.utf8Rem, FilterEsc: v.feed.oscEsc, Modes: [10]bool{v.m.bracketedPaste, v.m.appCursorKeys, v.m.focusEvent, v.m.mouseX10, v.m.mouseNormal, v.m.mouseHighlight, v.m.mouseButton, v.m.mouseAny, v.m.mouseUTF8, v.m.mouseSGR}}
	if err := c.validateFilter(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("ptyhost: encode checkpoint: %w", err)
	}
	if len(b) > attachwire.MaxContinuationBytes {
		return nil, fmt.Errorf("ptyhost: checkpoint exceeds limit")
	}
	return b, nil
}

func (v *vtHost) restoreCheckpoint(b []byte) error {
	if len(b) == 0 || len(b) > attachwire.MaxContinuationBytes {
		return fmt.Errorf("ptyhost: invalid checkpoint size")
	}
	var c hostCheckpointState
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return fmt.Errorf("ptyhost: decode checkpoint: %w", err)
	}
	canonical, err := json.Marshal(c)
	if err != nil || !bytes.Equal(canonical, b) {
		return fmt.Errorf("ptyhost: incomplete or noncanonical checkpoint")
	}
	if c.Schema != attachwire.ContinuationSchema || c.FilterState > oscTitleDrop || len(c.FilterPending) > oscTitleSelectorMax+3 || c.FilterUTF8 < 0 || c.FilterUTF8 > 3 {
		return fmt.Errorf("ptyhost: invalid checkpoint state")
	}
	if err := c.validateFilter(); err != nil {
		return err
	}
	if err := v.emu.RestoreCheckpoint(c.Engine); err != nil {
		return fmt.Errorf("ptyhost: restore engine: %w", err)
	}
	v.feed = oscTitleFilter{state: c.FilterState, pending: c.FilterPending, utf8Rem: c.FilterUTF8, oscEsc: c.FilterEsc}
	m := c.Modes
	v.m = modeState{bracketedPaste: m[0], appCursorKeys: m[1], focusEvent: m[2], mouseX10: m[3], mouseNormal: m[4], mouseHighlight: m[5], mouseButton: m[6], mouseAny: m[7], mouseUTF8: m[8], mouseSGR: m[9]}
	v.pendingFeed = nil
	return nil
}

// ContinuationCheckpoint returns complete state at the same atomic boundary as
// its picture and sequence. Callers explicitly select the supported schema.
// It does not allocate sequence numbers or publish a new-format frame onto the
// legacy stream. The caller must arrange checkpoint-plus-tail without gaps.
func (s *Session) ContinuationCheckpoint(selected string) (attachwire.ContinuationCheckpoint, error) {
	if selected != attachwire.ContinuationSchema {
		return attachwire.ContinuationCheckpoint{}, fmt.Errorf("ptyhost: unsupported continuation schema")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.continuationCheckpointLocked(selected)
}

func (s *Session) continuationCheckpointLocked(selected string) (attachwire.ContinuationCheckpoint, error) {
	v, ok := s.vt.(*vtHost)
	if !ok {
		return attachwire.ContinuationCheckpoint{}, fmt.Errorf("ptyhost: terminal does not support continuation")
	}
	state, err := v.checkpoint()
	if err != nil {
		return attachwire.ContinuationCheckpoint{}, err
	}
	checkpoint := attachwire.ContinuationCheckpoint{Schema: selected, Epoch: uint64(s.epoch), AtSeq: s.lastSeqLocked(), Picture: s.buildScreenLocked(), State: state}
	if s.exited {
		if checkpoint.AtSeq != s.exitSeq {
			return attachwire.ContinuationCheckpoint{}, fmt.Errorf("ptyhost: final checkpoint sequence mismatch")
		}
		final := s.exitPayload
		checkpoint.Exit = &final
	}
	return checkpoint, nil
}

// ContinuationTerminal restores the producer's exact built-in terminal and
// title-filter behavior for a continuation consumer. It is not thread-safe.
// Query replies are discarded; a viewer is not an input authority.
type ContinuationTerminal struct {
	host        *vtHost
	epoch       uint64
	echo        uint8
	closed      bool
	atSeq       attachwire.HostSeq
	exited      bool
	exitPayload attachwire.ExitPayload
}

// RestoreContinuation validates the entire checkpoint before returning a usable
// terminal. The caller advances its stream anchor only after this succeeds.
func RestoreContinuation(c attachwire.ContinuationCheckpoint) (*ContinuationTerminal, error) {
	if err := c.ValidateBoundary(attachwire.ContinuationSchema); err != nil {
		return nil, err
	}
	v := newVTHost(1, 1, 1, io.Discard, nil)
	v.emu.SetResponseWriter(io.Discard)
	if err := v.restoreCheckpoint(c.State); err != nil {
		_ = v.emu.Close()
		return nil, err
	}
	screen := buildScreen(v.raw(), c.Epoch, c.Picture.EchoMode, nil)
	a, err := screen.Encode()
	if err != nil {
		_ = v.emu.Close()
		return nil, err
	}
	b, err := c.Picture.Encode()
	if err != nil || !bytes.Equal(a, b) {
		_ = v.emu.Close()
		return nil, fmt.Errorf("ptyhost: checkpoint picture and state disagree")
	}
	terminal := &ContinuationTerminal{host: v, epoch: c.Epoch, echo: c.Picture.EchoMode, atSeq: c.AtSeq}
	if c.Exit != nil {
		terminal.exited = true
		terminal.exitPayload = *c.Exit
	}
	return terminal, nil
}

func (t *ContinuationTerminal) resize(cols, rows int) error {
	if t.closed || t.exited {
		return io.ErrClosedPipe
	}
	if cols < 1 || rows < 1 || cols > 4096 || rows > 4096 || cols > 262144/rows {
		return fmt.Errorf("ptyhost: invalid continuation geometry")
	}
	t.host.resize(cols, rows)
	return nil
}

// Screen returns the current legacy display projection.
func (t *ContinuationTerminal) Screen() attachwire.Screen {
	return buildScreen(t.host.raw(), t.epoch, t.echo, nil)
}

// Close releases the terminal resources.
func (t *ContinuationTerminal) Close() error { t.closed = true; return t.host.emu.Close() }

func (c hostCheckpointState) validateFilter() error {
	invalid := func() error { return fmt.Errorf("ptyhost: invalid title filter continuation") }
	if c.FilterState > oscTitleDrop || len(c.FilterPending) > oscTitleSelectorMax+3 || c.FilterUTF8 < 0 || c.FilterUTF8 > 3 {
		return invalid()
	}
	switch c.FilterState {
	case oscTitleGround:
		if len(c.FilterPending) != 0 || c.FilterEsc {
			return invalid()
		}
	case oscTitleEsc:
		if !bytes.Equal(c.FilterPending, []byte{oscTitleESC}) || c.FilterUTF8 != 0 || c.FilterEsc {
			return invalid()
		}
	case oscTitleCommand:
		p := c.FilterPending
		switch {
		case bytes.HasPrefix(p, []byte{oscTitleESC, ']'}):
			p = p[2:]
		case len(p) > 0 && p[0] == oscTitleC1OSC:
			p = p[1:]
		default:
			return invalid()
		}
		if c.FilterUTF8 != 0 {
			return invalid()
		}
		if c.FilterEsc {
			if len(p) == 0 || p[len(p)-1] != oscTitleESC {
				return invalid()
			}
			p = p[:len(p)-1]
		}
		for _, b := range p {
			if (b < '0' || b > '9') && b != oscTitleESC {
				return invalid()
			}
		}
	case oscTitlePass, oscTitleDrop:
		if len(c.FilterPending) != 0 {
			return invalid()
		}
	default:
		return invalid()
	}
	return nil
}

// OpenContinuation atomically captures complete state and subscribes at its exact
// sequence boundary. No output can fall between the state and live tail. The
// returned subscription carries unchanged canonical frames, including legacy
// picture snapshots that continuation consumers treat as non-rendering events.
func (s *Session) OpenContinuation(selected string) (attachwire.ContinuationCheckpoint, agent.InteractiveSubscription, error) {
	if selected != attachwire.ContinuationSchema {
		return attachwire.ContinuationCheckpoint{}, nil, fmt.Errorf("ptyhost: unsupported continuation schema")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.continuationCheckpointLocked(selected)
	if err != nil {
		return attachwire.ContinuationCheckpoint{}, nil, err
	}
	sub := newSubscription(s, nil, s.flow)
	if s.exited {
		sub.finish()
	} else {
		s.subs[sub] = struct{}{}
	}
	return c, sub, nil
}

// AtSeq is the last complete raw host frame applied to this mirror.
func (t *ContinuationTerminal) AtSeq() attachwire.HostSeq { return t.atSeq }

// ApplyRawFrame consumes an authenticated, unsanitized canonical host frame.
// Raw bytes stay inside the headless mirror; only Screen's escape-safe cell
// projection is suitable for rendering. A sequence gap or malformed frame is
// refused without advancing the anchor. Legacy picture Snapshots are checked
// but never repaint/reset complete continuation state.
func (t *ContinuationTerminal) ApplyRawFrame(f attachwire.Frame) error {
	if t.closed || t.exited {
		return fmt.Errorf("ptyhost: continuation stream is closed")
	}
	if uint64(t.atSeq) == ^uint64(0) || f.Seq != uint64(t.atSeq)+1 {
		return fmt.Errorf("ptyhost: continuation sequence gap")
	}
	switch f.Type {
	case attachwire.TypeOutput:
		t.host.write(attachwire.DecodeOutput(f.Payload).Data)
	case attachwire.TypeResize:
		resize, err := attachwire.DecodeResize(f.Payload)
		if err != nil {
			return err
		}
		if resize.Cols > 4096 || resize.Rows > 4096 {
			return fmt.Errorf("ptyhost: invalid continuation resize")
		}
		if err := t.resize(int(resize.Cols), int(resize.Rows)); err != nil {
			return err
		}
	case attachwire.TypeSnapshot:
		env, err := attachwire.DecodeSnapshotEnvelope(f.Payload)
		if err != nil {
			return err
		}
		if env.AtSeq != uint64(t.atSeq) || env.SnapFormat != attachwire.SnapFormatScreen {
			return fmt.Errorf("ptyhost: unexpected canonical snapshot")
		}
		picture, err := attachwire.DecodeScreen(env.Snap)
		if err != nil {
			return err
		}
		if picture.Epoch != t.epoch {
			return fmt.Errorf("ptyhost: canonical snapshot epoch changed")
		}
		// Echo is authoritative ancillary metadata, not VT continuation state.
		t.echo = picture.EchoMode
	case attachwire.TypeMarker:
		if _, err := attachwire.DecodeMarker(f.Payload); err != nil {
			return err
		}
	case attachwire.TypeExit:
		exit, err := attachwire.DecodeExit(f.Payload)
		if err != nil {
			return err
		}
		t.exitPayload = exit
		t.exited = true
		// The producer closes its PTY before publishing Exit, so termios is no
		// longer readable and its final picture reports EchoUnknown.
		t.echo = attachwire.EchoUnknown
	default:
		return fmt.Errorf("ptyhost: not a canonical host frame")
	}
	t.atSeq = attachwire.HostSeq(f.Seq)
	return nil
}

// Exit reports the immutable final observation when the checkpoint or tail ended.
func (t *ContinuationTerminal) Exit() (attachwire.ExitPayload, bool) { return t.exitPayload, t.exited }

// InspectContinuation implements the optional source contract with cancellation. The
// local capture performs bounded CPU work under the session mutex and never
// waits on network I/O. Cancellation is checked before and after capture.
func (s *Session) InspectContinuation(ctx context.Context, selected string) (attachwire.ContinuationCheckpoint, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return attachwire.ContinuationCheckpoint{}, err
	}
	checkpoint, err := s.ContinuationCheckpoint(selected)
	if err != nil {
		return attachwire.ContinuationCheckpoint{}, err
	}
	if err := ctx.Err(); err != nil {
		return attachwire.ContinuationCheckpoint{}, err
	}
	return checkpoint, nil
}

// SupportsContinuation reports whether this session owns the supported engine.
func (s *Session) SupportsContinuation() bool {
	if s == nil {
		return false
	}
	_, ok := s.vt.(*vtHost)
	return ok
}
