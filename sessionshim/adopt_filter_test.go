package sessionshim

import (
	"context"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/ptyhost"
)

// startInProcessShim publishes one live shim into reg under id and keeps it
// alive for the test's lifetime.
func startInProcessShim(t *testing.T, reg *Registry, dir string, id Identity, epoch uint64) *Shim {
	t.Helper()
	sh, err := Start(Options{
		Identity:     id,
		Registry:     reg,
		Spec:         ptyhost.Spec{Command: []string{"/bin/sh", "-c", interactiveFixture}},
		WorkareaPath: dir + "/" + id.SessionID,
		Orphan: OrphanPolicy{
			Deadline:          time.Hour, // never fires on its own during this test
			TerminationGrace:  500 * time.Millisecond,
			PropagationMargin: 0,
		},
		ProcessEpoch: epoch,
	})
	if err != nil {
		t.Fatalf("Start %s: %v", id, err)
	}
	t.Cleanup(func() { _ = sh.Close() })
	if _, err := reg.Get(id); err != nil {
		t.Fatalf("no record published at start for %s: %v", id, err)
	}
	return sh
}

// TestAdoptFilterDialsOnlyTheIdentitiesItAccepts pins AdoptOptions.Filter on a
// registry holding two live shims. A filtered pass must adopt exactly the
// identity it names and leave the other record untouched — not dialled, not
// classified, not charged — which the next unfiltered pass proves by adopting
// that other shim at its FIRST generation while the filtered one is already
// at its second.
func TestAdoptFilterDialsOnlyTheIdentitiesItAccepts(t *testing.T) {
	if !peerCredSupported() {
		t.Skip("session shim adoption is unsupported on this platform")
	}
	dir := shortTempDir(t)
	reg, err := NewRegistry(dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	wanted := Identity{OrgID: "org-filter", SessionID: "sess-wanted"}
	other := Identity{OrgID: "org-filter", SessionID: "sess-other"}
	startInProcessShim(t, reg, dir, wanted, 1)
	startInProcessShim(t, reg, dir, other, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	filtered, err := Adopt(ctx, AdoptOptions{
		Registry:     reg,
		ControllerID: "controller-filtered",
		Filter:       func(id Identity) bool { return id == wanted },
	})
	if err != nil {
		t.Fatalf("filtered Adopt: %v", err)
	}
	if len(filtered.Adopted) != 1 || filtered.Adopted[0].Identity() != wanted {
		t.Fatalf("filtered pass adopted %d controller(s) %v, want exactly %s", len(filtered.Adopted), adoptedIdentities(filtered), wanted)
	}
	if len(filtered.Quarantined) != 0 || len(filtered.Tombstoned) != 0 {
		t.Fatalf("filtered pass classified records it was not asked about: quarantined=%+v tombstoned=%+v", filtered.Quarantined, filtered.Tombstoned)
	}
	if got := filtered.OccupiedSlots(); got != 1 {
		t.Fatalf("filtered pass occupied slots = %d, want 1 — only the named identity is charged", got)
	}
	if gen := filtered.Adopted[0].Generation(); gen != 1 {
		t.Fatalf("filtered adoption generation = %d, want 1", gen)
	}
	filtered.Close()

	none, err := Adopt(ctx, AdoptOptions{
		Registry:     reg,
		ControllerID: "controller-none",
		Filter:       func(Identity) bool { return false },
	})
	if err != nil {
		t.Fatalf("Adopt filtered to nothing: %v", err)
	}
	if len(none.Adopted) != 0 || len(none.Quarantined) != 0 || none.OccupiedSlots() != 0 {
		t.Fatalf("a filter accepting nothing still produced %+v", none)
	}

	full, err := Adopt(ctx, AdoptOptions{Registry: reg, ControllerID: "controller-full"})
	if err != nil {
		t.Fatalf("unfiltered Adopt: %v", err)
	}
	defer full.Close()
	if len(full.Adopted) != 2 {
		t.Fatalf("unfiltered pass adopted %v, want both identities", adoptedIdentities(full))
	}
	generations := map[Identity]uint64{}
	for _, ctrl := range full.Adopted {
		generations[ctrl.Identity()] = uint64(ctrl.Generation())
	}
	// The filtered pass advanced only the identity it named: the other shim
	// hands out its first generation now, which it could not do had the
	// filtered pass dialled it.
	if generations[wanted] != 2 || generations[other] != 1 {
		t.Fatalf("generations after the unfiltered pass = %v, want %s at 2 and %s at 1", generations, wanted, other)
	}
}

func adoptedIdentities(result AdoptionResult) []Identity {
	ids := make([]Identity, 0, len(result.Adopted))
	for _, ctrl := range result.Adopted {
		ids = append(ids, ctrl.Identity())
	}
	return ids
}
