package ptycli

import (
	"context"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

func TestShimBusinessResultPrecedesOwnerDrain(t *testing.T) {
	requireShell(t)
	dir := shortRegistryDir(t)
	launch := sessionshim.Launch{Identity: sessionshim.Identity{OrgID: "org-business", SessionID: "session-business"}, RegistryDir: dir, Orphan: sessionshim.DefaultOrphanPolicy(), ProcessEpoch: 1}
	for k, v := range launch.Env() {
		t.Setenv(k, v)
	}
	ctx, owner := sessionshim.WithOwnerLifetime(context.Background())
	h, err := Spawn(ctx, "/bin/sh", []string{"-c", "read -r line; printf final-screen; exit 0"}, agent.Spec{Cwd: dir, Interactive: &agent.InteractiveSpec{}}, agent.HarnessManifest{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = h.Stop(context.Background())
		if h.shim != nil {
			_ = h.shim.Close()
		}
	})
	reg, err := sessionshim.NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	adopted, err := sessionshim.Adopt(ctx, sessionshim.AdoptOptions{Registry: reg, ControllerID: "business-controller", ProtocolMin: shimwire.V1, ProtocolMax: shimwire.V2})
	if err != nil || len(adopted.Adopted) != 1 {
		t.Fatalf("Adopt = %+v %v", adopted, err)
	}
	t.Cleanup(adopted.Close)
	c := adopted.Adopted[0]
	if err := c.WriteInput([]byte("finish\r")); err != nil {
		t.Fatal(err)
	}
	var result agent.ResultEvent
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	awaiting := true
	for awaiting {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				t.Fatal("events closed before terminal result")
			}
			if r, ok := ev.(agent.ResultEvent); ok {
				result = r
				awaiting = false
			}
		case <-timer.C:
			t.Fatal("business result waited for the final-screen window")
		}
	}
	if !result.Success {
		t.Fatalf("result=%+v", result)
	}
	select {
	case <-h.InteractiveSession().Done():
	default:
		t.Fatal("business session Done was delayed")
	}
	if _, done := h.InteractiveSession().Exit(); !done {
		t.Fatal("business Exit hidden during final-screen window")
	}
	// Runner.Run defers this same Stop after reporting its business result.
	if err := h.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	drained := make(chan error, 1)
	go func() { drained <- owner.Wait(context.Background()) }()
	final, err := c.EmitSnapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot after business cleanup: %v", err)
	}
	frame, err := attachwire.DecodeFrame(final.Bytes)
	if err != nil || frame.Type != attachwire.TypeSnapshot || frame.Seq != attachwire.PostExitSnapshotSeq {
		t.Fatalf("final frame=%+v err=%v", frame, err)
	}
	select {
	case <-h.shim.Done():
		t.Fatal("owner stopped while controller retained final screen")
	default:
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-drained:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("owner did not drain on controller close")
	}
}
