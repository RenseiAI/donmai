package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/ptyhost"
	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

func TestExactSessionShimContinuationRef(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "dcc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	registry, err := sessionshim.NewRegistry(filepath.Join(dir, "r"))
	if err != nil {
		t.Fatal(err)
	}
	id := sessionshim.Identity{OrgID: "org-continuation", SessionID: "session-continuation"}
	echoArgv, err := daemonShimEchoCommand()
	if err != nil {
		t.Fatal(err)
	}
	sh, err := sessionshim.Start(sessionshim.Options{Identity: id, Registry: registry, ProcessEpoch: 5, Spec: ptyhost.Spec{Command: echoArgv, Env: daemonShimEchoEnv(), Epoch: 7}, Orphan: sessionshim.OrphanPolicy{Deadline: time.Minute, TerminationGrace: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sh.Terminate(ctx)
		_ = sh.Close()
	})
	adopted, err := sessionshim.Adopt(context.Background(), sessionshim.AdoptOptions{Registry: registry, ControllerID: "continuation-controller", ProtocolMin: shimwire.V1, ProtocolMax: shimwire.V5, RequireFullHostFrames: true})
	if err != nil || len(adopted.Adopted) != 1 {
		t.Fatalf("adopt: %v", err)
	}
	t.Cleanup(adopted.Close)
	controller := adopted.Adopted[0]
	ref := SessionShimControlRef{Identity: id, ShimID: controller.Hello().ShimID, ProcessEpoch: 5, ControllerGeneration: uint64(controller.Generation())}
	d := &Daemon{shims: &sessionShimState{adopted: make(map[sessionshim.Identity]adoptedShim)}}
	if d.SupportsAdoptedSessionShimContinuationFor(ref) {
		t.Fatal("unpublished authority was inspectable")
	}
	if _, err := d.InspectAdoptedSessionShimContinuationFor(context.Background(), ref, attachwire.ContinuationSchema); err == nil {
		t.Fatal("inspection bypassed publication")
	}
	for _, state := range []SessionShimAdoptionPreparationState{"", SessionShimPreparationAdoptedCandidateRecovery} {
		evidence, err := d.sessionShimAdoptionEvidence(context.Background(), controller, SessionShimAdoptionPreparationResult{State: state}, "host-continuation")
		if err != nil {
			t.Fatal(err)
		}
		if !evidence.ContinuationSupported {
			t.Fatal("pre-publication authenticated capability was lost")
		}
		if evidence.Identity != ref.Identity || evidence.ShimID != ref.ShimID || evidence.ProcessEpoch != ref.ProcessEpoch || evidence.ControllerGeneration != ref.ControllerGeneration {
			t.Fatal("capability evidence lost exact controller binding")
		}
		if state == SessionShimPreparationAdoptedCandidateRecovery && evidence.SnapshotProxy != nil {
			t.Fatal("recovery gained snapshot mutation authority")
		}
		if evidence.SnapshotProxy != nil {
			evidence.SnapshotProxy.deactivate()
		}
	}
	d.shims.adopted[id] = adoptedShim{controller: controller, shimID: ref.ShimID, adoption: SessionShimAdoptionEvidence{Identity: id, ProcessEpoch: 5, ControllerGeneration: ref.ControllerGeneration}}

	if !d.SupportsAdoptedSessionShimContinuationFor(ref) {
		t.Fatal("exact v5 source not advertised")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	checkpoint, err := d.InspectAdoptedSessionShimContinuationFor(ctx, ref, attachwire.ContinuationSchema)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Epoch != 7 {
		t.Fatal("PTY epoch was confused with process epoch")
	}
	for _, mutate := range []func(*SessionShimControlRef){func(r *SessionShimControlRef) { r.Identity.OrgID = "other" }, func(r *SessionShimControlRef) { r.ShimID = "other" }, func(r *SessionShimControlRef) { r.ProcessEpoch++ }, func(r *SessionShimControlRef) { r.ControllerGeneration++ }} {
		stale := ref
		mutate(&stale)
		if d.SupportsAdoptedSessionShimContinuationFor(stale) {
			t.Fatal("stale ref advertised continuation")
		}
		result, err := d.InspectAdoptedSessionShimContinuationFor(ctx, stale, attachwire.ContinuationSchema)
		if err == nil || result.Schema != "" {
			t.Fatal("stale ref released checkpoint state")
		}
	}
}
