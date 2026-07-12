package ptyhost

import (
	"errors"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/attachwire"
)

func mkFrame(seq uint64, n int) attachwire.Frame {
	return attachwire.Frame{Type: attachwire.TypeOutput, Seq: seq, Payload: make([]byte, n)}
}

// TestRingEvictionAndReplay exercises byte-bounded eviction and the §13 ring
// hit/miss classification directly.
func TestRingEvictionAndReplay(t *testing.T) {
	r := newRing(30)
	for i := uint64(1); i <= 5; i++ {
		r.append(mkFrame(i, 10)) // 10 bytes each; cap 30 keeps the last 3
	}
	if got := r.firstSeq(); got != 3 {
		t.Errorf("firstSeq = %d, want 3 (seq 1,2 evicted)", got)
	}
	if got := r.lastSeq(); got != 5 {
		t.Errorf("lastSeq = %d, want 5", got)
	}

	tests := []struct {
		name    string
		after   attachwire.HostSeq
		wantHit bool
		wantLen int
	}{
		{"from-oldest (0)", 0, true, 3},    // replay 3,4,5
		{"caught-up head (5)", 5, true, 0}, // nothing to replay, go live
		{"in-window (4)", 4, true, 1},      // replay 5
		{"at first-1 (2)", 2, true, 3},     // replay 3,4,5 (frame 3 still present)
		{"evicted (1)", 1, false, 0},       // frame 2 gone → ring miss
		{"ahead of head (7)", 7, false, 0}, // future position → ring miss
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			frames, hit := r.replayFrom(tc.after)
			if hit != tc.wantHit {
				t.Fatalf("hit = %v, want %v", hit, tc.wantHit)
			}
			if hit && len(frames) != tc.wantLen {
				t.Errorf("replay len = %d, want %d", len(frames), tc.wantLen)
			}
		})
	}
}

// TestRingSingleOversizeFrameRetained: a lone frame larger than the whole budget
// is still retained so a fresh subscriber can always get the latest.
func TestRingSingleOversizeFrameRetained(t *testing.T) {
	r := newRing(16)
	r.append(mkFrame(1, 1024))
	if got := r.firstSeq(); got != 1 {
		t.Errorf("oversize lone frame evicted (firstSeq=%d)", got)
	}
	frames, hit := r.replayFrom(0)
	if !hit || len(frames) != 1 {
		t.Errorf("replayFrom(0) hit=%v len=%d, want hit=true len=1", hit, len(frames))
	}
}

// TestSubscribeRingMiss drives the Session.Subscribe path: a tiny ring plus many
// small host frames (Markers) evicts the early seqs, so Subscribe(1) returns
// agent.ErrRingMiss while Subscribe(0) is served from the oldest retained frame.
func TestSubscribeRingMiss(t *testing.T) {
	s := mustSpawn(t, Spec{Command: []string{"sleep", "30"}, RingBytes: 40})
	for i := 0; i < 100; i++ {
		if err := s.EmitMarker("m"); err != nil {
			t.Fatalf("EmitMarker: %v", err)
		}
	}

	if _, err := s.Subscribe(1); !errors.Is(err, agent.ErrRingMiss) {
		t.Errorf("Subscribe(1) err = %v, want agent.ErrRingMiss", err)
	}

	sub, err := s.Subscribe(0)
	if err != nil {
		t.Fatalf("Subscribe(0) err = %v, want a hit from oldest", err)
	}
	_ = sub.Close()
}
