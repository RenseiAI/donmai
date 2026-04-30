package daemon

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestHeartbeatService_StartStop(t *testing.T) {
	var count int32
	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID: "w1", Hostname: "h", IntervalSeconds: 1,
		GetActiveCount: func() int { return 0 },
		GetMaxCount:    func() int { return 1 },
		GetStatus:      func() RegistrationStatus { return RegistrationIdle },
		OnHeartbeat:    func(_ HeartbeatPayload) { atomic.AddInt32(&count, 1) },
	})
	hs.Start()
	if !hs.IsRunning() {
		t.Fatal("expected running after Start")
	}
	// Wait for the immediate first heartbeat.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(&count) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if atomic.LoadInt32(&count) == 0 {
		t.Fatal("expected at least one heartbeat")
	}
	hs.Stop()
	if hs.IsRunning() {
		t.Fatal("expected not running after Stop")
	}
	got := hs.LastPayload()
	if got.WorkerID != "w1" {
		t.Errorf("LastPayload.WorkerID = %q", got.WorkerID)
	}
}

func TestHeartbeatService_IdempotentStart(_ *testing.T) {
	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID: "x", Hostname: "h",
		GetActiveCount: func() int { return 0 },
		GetMaxCount:    func() int { return 1 },
		GetStatus:      func() RegistrationStatus { return RegistrationIdle },
	})
	hs.Start()
	hs.Start() // should be a no-op
	hs.Stop()
}
