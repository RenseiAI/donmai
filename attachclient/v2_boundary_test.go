package attachclient

// Provenance: fresh-dial-boundary-precondition-2026-09-03 — grep a build for
// this marker to prove its fresh v2 dial accepts a signed carrier boundary that
// is legitimately ahead of the caller's local durable acknowledgement floor.
//
// THE STRAND THIS UNDOES
//
// A fresh candidate demanded EXACT EQUALITY between two cursors the durable
// proof contract deliberately keeps independent: the caller's local, fsync
// backed acknowledgement floor, and the external carrier's signed durable
// journal high water. The carrier's admission predicate, the control plane's
// proof parse, and the control plane's commit all refuse only the REGRESSION
// direction; the corpus states plainly that neither cursor may be inferred from
// the other. The carrier is ahead by exactly the window of frames it journaled
// but had not yet acknowledged back into the local sidecar — and an abrupt exit
// of the previous composing daemon freezes that window permanently.
//
// Measured live: a consumer daemon restarted mid-stream, the carrier had
// durably journaled three frames past the local floor, the carrier admitted the
// reservation and signed the boundary it holds — and the client refused its own
// successful reservation.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
	attachwirev2 "github.com/RenseiAI/donmai/attachwire/v2"
	"github.com/coder/websocket"
)

// v2BoundaryServer accepts one v2 candidate, asks for the mandatory resync, and
// hands every subsequent frame it reads to the caller's assertion.
func v2BoundaryServer(t *testing.T, assert func(context.Context, *websocket.Conn) error) (*httptest.Server, <-chan error) {
	t.Helper()
	serverErr := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{attachwirev2.SubprotocolVersion}})
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.CloseNow() //nolint:errcheck
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, _, err := readV2TestFrame(ctx, conn); err != nil {
			serverErr <- fmt.Errorf("read subscribe: %w", err)
			return
		}
		request, buildErr := attachwirev2.BuildControlFrame(attachwire.SnapshotRequest{Reason: attachwire.ReasonResync})
		if buildErr != nil {
			serverErr <- buildErr
			return
		}
		if err := conn.Write(ctx, websocket.MessageBinary, request.Encode()); err != nil {
			serverErr <- fmt.Errorf("write snapshot request: %w", err)
			return
		}
		serverErr <- assert(ctx, conn)
	}))
	t.Cleanup(server.Close)
	return server, serverErr
}

func v2BoundaryURL(server *httptest.Server) string {
	return strings.Replace(server.URL, "http://", "ws://", 1) + "/v2/rooms/session-v2"
}

// TestFreshDialAcceptsCarrierBoundaryAheadOfLocalDurableAck is the RED/GREEN
// target. Signed proof: carrier boundary N = 120, resolved boundary K = 120 —
// the carrier durably holds through 120 with no unforwarded gap. The local
// acknowledgement floor lags at 117: the previous composing daemon died after
// 117 was acknowledged but before 118..120 were pushed back into the sidecar.
//
// The dial must succeed, and the leg must be seeded at the SIGNED boundary, so
// the mandatory Snapshot at K+1 = 121 is contiguous with no gap frame.
func TestFreshDialAcceptsCarrierBoundaryAheadOfLocalDurableAck(t *testing.T) {
	wantSnapshot := v2ResumeSnapshot(121).Encode()
	server, serverErr := v2BoundaryServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		frame, raw, err := readV2TestFrame(ctx, conn)
		if err != nil {
			return err
		}
		if frame.Type != attachwire.TypeSnapshot {
			message, controlErr := v2ControlFromFrame(frame)
			return fmt.Errorf("first post-request frame = %#v (%v), want the mandatory Snapshot with no gap", message, controlErr)
		}
		if !bytes.Equal(raw, wantSnapshot) {
			return fmt.Errorf("Snapshot bytes = seq %d, want the exact seq 121 frame", frame.Seq)
		}
		return nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	candidate, err := DialV2HostCandidate(ctx, V2HostConfig{
		AttachURL: v2BoundaryURL(server),
		TokenSource: func(context.Context) (string, error) {
			return v2TestToken(t, func(claims map[string]any) {
				claims["carrier_boundary"] = "120"
				claims["resolved_boundary"] = "120"
				claims["last_host_seq"] = "120"
			}), nil
		},
		DurableHighWater: 117,
	})
	if err != nil {
		t.Fatalf("fresh dial refused a legal carrier-ahead skew: %v", err)
	}
	defer candidate.Close() //nolint:errcheck

	// The seed is the assertion, not merely the absence of an error: a leg
	// seeded from the local floor would accept the dial and then refuse the
	// mandatory Snapshot as non-contiguous.
	candidate.mu.Lock()
	ackSeq, highestSent := candidate.ackSeq, candidate.highestSent
	candidate.mu.Unlock()
	if ackSeq != 120 || highestSent != 120 {
		t.Fatalf("fresh leg seeded at ack %d / highestSent %d, want both at the signed carrier boundary 120", ackSeq, highestSent)
	}

	if _, err := candidate.WaitMandatorySnapshotRequest(ctx); err != nil {
		t.Fatalf("mandatory snapshot request: %v", err)
	}
	if err := candidate.SendClaimsBoundCandidateSnapshot(ctx, wantSnapshot); err != nil {
		t.Fatalf("mandatory Snapshot at the signed resolved boundary+1: %v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

// TestFreshDialRefusesLocalDurableAckAheadOfCarrierBoundary pins the one
// direction that is still evidence: a local floor of 125 against a signed
// boundary of 120 cannot be produced by a live carrier, so the proof this dial
// holds is stale. It refuses with the typed error, naming both cursors and
// nothing else.
func TestFreshDialRefusesLocalDurableAckAheadOfCarrierBoundary(t *testing.T) {
	dialed := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { dialed = true }))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := DialV2HostCandidate(ctx, V2HostConfig{
		AttachURL: v2BoundaryURL(server),
		TokenSource: func(context.Context) (string, error) {
			return v2TestToken(t, func(claims map[string]any) {
				claims["carrier_boundary"] = "120"
				claims["resolved_boundary"] = "120"
				claims["last_host_seq"] = "120"
			}), nil
		},
		DurableHighWater: 125,
	})
	if err == nil {
		t.Fatal("fresh dial accepted a local durable high-water ahead of the signed carrier boundary")
	}
	if !errors.Is(err, ErrV2CarrierCursorDrift) {
		t.Fatalf("refusal %v is not classified as cursor drift; a composing caller cannot tell it from a genuine refusal", err)
	}
	var drift *V2CarrierCursorDriftError
	if !errors.As(err, &drift) {
		t.Fatalf("refusal %v does not carry the typed cursor pair", err)
	}
	if drift.DurableHighWater != 125 || drift.CarrierBoundary != 120 {
		t.Fatalf("drift = local %d / boundary %d, want 125 / 120", drift.DurableHighWater, drift.CarrierBoundary)
	}
	for _, cursor := range []string{"125", "120"} {
		if !strings.Contains(err.Error(), cursor) {
			t.Fatalf("refusal %q does not name the cursor %s", err.Error(), cursor)
		}
	}
	if dialed {
		t.Fatal("a stale-proof refusal still opened a carrier leg")
	}
}

// TestFreshDialCarrierAheadWithControllerGapStartsAtTheSignedBoundary is the
// gap arm of the same seed. Signed proof: carrier boundary N = 120, resolved
// boundary K = 125, local floor 117. The proof-bound controller_unforwarded gap
// must run 121..125 — from N+1, NOT from the local cursor+1 — and the Snapshot
// must follow at 126.
func TestFreshDialCarrierAheadWithControllerGapStartsAtTheSignedBoundary(t *testing.T) {
	wantSnapshot := v2ResumeSnapshot(126).Encode()
	server, serverErr := v2BoundaryServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		gapFrame, _, err := readV2TestFrame(ctx, conn)
		if err != nil {
			return fmt.Errorf("read gap: %w", err)
		}
		message, controlErr := v2ControlFromFrame(gapFrame)
		if controlErr != nil {
			return controlErr
		}
		gap, ok := message.(attachwirev2.HostGap)
		if !ok {
			return fmt.Errorf("first post-request frame = %#v, want a host gap", message)
		}
		if uint64(gap.FromSeq) != 121 || uint64(gap.ToSeq) != 125 ||
			gap.Reason != attachwirev2.GapControllerUnforwarded {
			return fmt.Errorf("gap = %d..%d/%s, want 121..125/%s from the signed boundary+1",
				gap.FromSeq, gap.ToSeq, gap.Reason, attachwirev2.GapControllerUnforwarded)
		}
		_, raw, err := readV2TestFrame(ctx, conn)
		if err != nil {
			return fmt.Errorf("read Snapshot: %w", err)
		}
		if !bytes.Equal(raw, wantSnapshot) {
			return errors.New("the gap was not followed by the exact seq 126 Snapshot")
		}
		return nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	candidate, err := DialV2HostCandidate(ctx, V2HostConfig{
		AttachURL: v2BoundaryURL(server),
		TokenSource: func(context.Context) (string, error) {
			return v2TestToken(t, func(claims map[string]any) {
				claims["carrier_boundary"] = "120"
				claims["resolved_boundary"] = "125"
				claims["last_host_seq"] = "125"
			}), nil
		},
		DurableHighWater: 117,
	})
	if err != nil {
		t.Fatalf("fresh dial refused a legal carrier-ahead skew with a controller gap: %v", err)
	}
	defer candidate.Close() //nolint:errcheck
	if _, err := candidate.WaitMandatorySnapshotRequest(ctx); err != nil {
		t.Fatalf("mandatory snapshot request: %v", err)
	}
	if err := candidate.SendClaimsBoundCandidateSnapshot(ctx, wantSnapshot); err != nil {
		t.Fatalf("claims-bound gap and Snapshot: %v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}
