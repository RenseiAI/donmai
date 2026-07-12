package attachtest

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
	"github.com/coder/websocket"
)

// Viewer is a minimal WSS viewer client for driving the relay from tests. It
// dials in, subscribes, and surfaces every received frame on Frames.
type Viewer struct {
	conn      *websocket.Conn
	frames    chan attachwire.Frame
	cancel    context.CancelFunc
	sessionID string
	inputSeq  uint64
	penGen    uint64
}

// AttachViewer dials wsURL as a viewer/driver, subscribes with the given
// resumeFrom (nil ≡ null ≡ 0, § 13), and starts reading frames.
func AttachViewer(ctx context.Context, wsURL, token string, role attachwire.Role, resumeFrom *int64) (*Viewer, error) {
	cl, err := parseClaims(token)
	if err != nil {
		return nil, err
	}
	dctx, dc := context.WithTimeout(ctx, 10*time.Second)
	conn, _, err := websocket.Dial(dctx, wsURL, &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": {"Bearer " + token}},
		Subprotocols: []string{attachwire.SubprotocolVersion},
	})
	dc()
	if err != nil {
		return nil, err
	}
	if conn.Subprotocol() != attachwire.SubprotocolVersion {
		_ = conn.Close(websocket.StatusProtocolError, "no subprotocol")
		return nil, fmt.Errorf("attachtest: viewer subprotocol not negotiated")
	}
	conn.SetReadLimit(64 << 20)

	sub := attachwire.Subscribe{
		SessionID:  cl.SessionID,
		AsRole:     role,
		ResumeFrom: resumeFrom,
	}
	f, err := attachwire.BuildControlFrame(sub)
	if err != nil {
		_ = conn.Close(websocket.StatusInternalError, "subscribe build")
		return nil, err
	}
	wctx, wc := context.WithTimeout(ctx, 10*time.Second)
	err = conn.Write(wctx, websocket.MessageBinary, f.Encode())
	wc()
	if err != nil {
		_ = conn.Close(websocket.StatusInternalError, "subscribe write")
		return nil, err
	}

	vctx, cancel := context.WithCancel(ctx)
	v := &Viewer{conn: conn, frames: make(chan attachwire.Frame, 256), cancel: cancel, sessionID: cl.SessionID}
	go v.readLoop(vctx)
	return v, nil
}

func (v *Viewer) readLoop(ctx context.Context) {
	defer close(v.frames)
	for {
		typ, data, err := v.conn.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageBinary {
			continue
		}
		frame, derr := attachwire.DecodeFrame(data)
		if derr != nil {
			return
		}
		select {
		case v.frames <- frame:
		case <-ctx.Done():
			return
		}
	}
}

// Frames is the received-frame channel; it closes when the viewer disconnects.
func (v *Viewer) Frames() <-chan attachwire.Frame { return v.frames }

// SendInput sends one unstamped Input frame (userIdLen 0, § 5); the relay stamps
// the userId. The per-connection inputSeq auto-increments.
func (v *Viewer) SendInput(ctx context.Context, data []byte) error {
	v.inputSeq++
	payload := attachwire.EncodeViewerInput(v.inputSeq, v.penGen, data)
	frame := attachwire.Frame{Type: attachwire.TypeInput, Payload: payload}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return v.conn.Write(wctx, websocket.MessageBinary, frame.Encode())
}

// Close disconnects the viewer.
func (v *Viewer) Close() error {
	v.cancel()
	return v.conn.Close(websocket.StatusNormalClosure, "bye")
}
