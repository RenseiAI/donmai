package attachclient

import (
	"context"
	"errors"
	"fmt"

	"github.com/RenseiAI/donmai/attachwire"
)

// handleInbound performs the Session-side effect of one relay→host frame and
// returns any frames the caller must transmit back (only the post-Exit Snapshot
// reply, § 12.2 — which rides the WSS conn or the degraded outOfSeq array). All
// relay→host frames carry seq = 0 / rel_time = 0 (§ 2); the header is ignored.
//
// Error dispositions:
//   - a *attachwire.FramingError (unknown/invalid payload, 0-dim Resize) → the
//     caller closes the leg with an error control (code framing) then reconnects;
//   - ErrEpochStale / *RelayStopError → terminal, RunHost stops;
//   - any other error → transient, the caller reconnects.
//
// It does NOT dedup Input — the WSS lane is exactly-once. The degraded lane's
// at-least-once dedup wraps this via handleDegradedInbound.
func (h *host) handleInbound(ctx context.Context, f attachwire.Frame) ([]attachwire.Frame, error) {
	switch f.Type {
	case attachwire.TypeInput:
		return h.applyInput(f.Payload)
	case attachwire.TypeResize:
		return h.applyResize(f.Payload)
	case attachwire.TypeControl:
		return h.applyControl(ctx, f.Payload)
	default:
		// § 6.3: the host receives only Input / Resize / Control. A known but
		// unexpected type (Output/Marker/Exit/Snapshot from the relay) is not a
		// framing error (the type byte is valid) — ignore it for forward-compat.
		h.log.Debug("attachclient: ignoring unexpected relay→host frame type", "type", f.Type)
		return nil, nil
	}
}

// applyInput applies a stamped Input to the PTY (§ 5). Unstamped Input
// (userIdLen == 0) is dropped — the host trust posture: an unstamped frame never
// reaches WriteInput.
func (h *host) applyInput(payload []byte) ([]attachwire.Frame, error) {
	in, err := attachwire.DecodeInput(payload)
	if err != nil {
		return nil, err // framing
	}
	if !in.Stamped() {
		h.log.Warn("attachclient: dropping unstamped Input (userIdLen==0) — host trust posture §5", "inputSeq", in.InputSeq)
		return nil, nil
	}
	if _, err := h.cfg.Session.WriteInput(in.Data); err != nil {
		return nil, fmt.Errorf("attachclient: writing input to the session: %w", err)
	}
	return nil, nil
}

// applyResize applies the relay's authoritative geometry verbatim (§ 8).
// DecodeResize rejects cols == 0 || rows == 0 as a framing error.
func (h *host) applyResize(payload []byte) ([]attachwire.Frame, error) {
	rz, err := attachwire.DecodeResize(payload)
	if err != nil {
		return nil, err // framing (incl. 0-dim geometry, § 3.1/§ 8)
	}
	//nolint:gosec // G115: terminal geometry is small; the relay is the authoritative source
	if err := h.cfg.Session.Resize(uint32(rz.Cols), uint32(rz.Rows), uint32(rz.PxWidth), uint32(rz.PxHeight)); err != nil {
		return nil, fmt.Errorf("attachclient: applying resize: %w", err)
	}
	return nil, nil
}

func (h *host) applyControl(ctx context.Context, payload []byte) ([]attachwire.Frame, error) {
	j, err := attachwire.DecodeControlPayload(payload)
	if err != nil {
		return nil, err // framing
	}
	msg, err := attachwire.DecodeControl(j)
	if err != nil {
		if errors.Is(err, attachwire.ErrUnknownControlType) {
			// § 6.3 soft case: unknown control type is ignored (forward-compat),
			// distinct from an unknown frame TYPE byte (a hard framing error).
			h.log.Debug("attachclient: ignoring unknown control message type (§6.3)")
			return nil, nil
		}
		// A malformed known-shape control is tolerated (ignore) rather than
		// treated as a framing error: § 7 mandates ignoring unknown fields, and a
		// forgiving reader avoids tearing the leg down over a relay-side quirk.
		h.log.Warn("attachclient: ignoring malformed control message", "err", err)
		return nil, nil
	}
	return h.handleControl(ctx, msg)
}

func (h *host) handleControl(ctx context.Context, msg attachwire.ControlMessage) ([]attachwire.Frame, error) {
	switch m := msg.(type) {
	case attachwire.SnapshotRequest:
		frame, inStream, err := h.cfg.Session.EmitSnapshot()
		if err != nil {
			return nil, fmt.Errorf("attachclient: emitting snapshot for %s request: %w", m.Reason, err)
		}
		if inStream {
			// Pre-Exit: the snapshot is seq-bearing and rides the subscription —
			// nothing to transmit directly.
			return nil, nil
		}
		// Post-Exit: header seq 0, atSeq == Exit.seq — transmit directly.
		return []attachwire.Frame{frame}, nil

	case attachwire.Kill:
		signal := ""
		if m.Signal != nil {
			signal = *m.Signal
		}
		h.invokeKill(ctx, string(m.Reason), signal)
		// The normal Exit flow follows (the Session drains → Exit frame streams
		// out); we synthesize nothing.
		return nil, nil

	case attachwire.ControlError:
		if m.Code == attachwire.CodeEpochStale {
			return nil, ErrEpochStale
		}
		if !m.Retryable {
			return nil, &RelayStopError{Code: m.Code, Message: m.Message}
		}
		h.log.Warn("attachclient: retryable relay error control", "code", m.Code, "message", m.Message)
		return nil, nil

	default:
		// § 6.3: the host ignores any known control other than
		// snapshot_request / kill / error.
		h.log.Debug("attachclient: ignoring control not addressed to the host (§6.3)", "type", msg.ControlType())
		return nil, nil
	}
}
