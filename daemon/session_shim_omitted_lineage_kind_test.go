// Provenance: shim-omitted-lineage-kind-2026-09-06 — grep a build for this
// marker to prove the re-composition can answer a refusal about a lineage this
// daemon's own stale-lineage declarations created.
//
// THE STRAND THIS UNDOES
//
// The declaration guard skipped any reported lineage whose controller
// generation was zero, on the correct principle that the control plane's report
// renders that field nullable and a null arrives as a zero. But it is nullable
// in exactly ONE of the three arms a completeness refusal reports from. The
// quarantined arm always sends a generation — and it is legitimately ZERO for
// precisely the rows this daemon publishes itself, because a frozen v1
// discovery record carries no authenticated generation and the quarantine
// projection's constructor hard-codes the conservative zero.
//
// So a host that declared a stale lineage (the fix this guard shipped
// alongside) and was later refused for omitting it could never answer: every
// re-composition skipped the one lineage it was being refused over, the loop
// broke with nothing declared, and the composition stood down with durable
// sessions off — permanently, on every start, over the daemon's own
// bookkeeping. The guard has to be per-arm, which means the arm has to travel
// with the report.

package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

// TestQuarantinedOmissionAtGenerationZeroIsDeclared is the strand above.
//
// The lineage is reported from the quarantined arm at generation zero — the
// exact shape this daemon's own stale-lineage declaration produces and the
// control plane reports straight back. It MUST be declarable, or a host can be
// refused forever over a row it wrote itself.
func TestQuarantinedOmissionAtGenerationZeroIsDeclared(t *testing.T) {
	ctx := context.Background()
	h := newCompositionHarness(t)
	h.start(ctx)

	const (
		ownSession = "sess-declared-by-this-host"
		ownShim    = "shim-declared-by-this-host"
		ownEpoch   = uint64(4)
	)
	var (
		mu      sync.Mutex
		batches []SessionShimAdoptionBatch
	)
	cfg := h.composedConfig(func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		mu.Lock()
		batches = append(batches, cloneSessionShimAdoptionBatch(batch))
		mu.Unlock()
		if _, ok := batchQuarantine(batch, ownSession); !ok {
			return SessionShimAdoptionBatchReceipt{}, &SessionShimAdoptionBatchLineagesOmitted{
				Lineages: []SessionShimOmittedLineage{{
					Identity:     sessionshim.Identity{OrgID: h.orgID, SessionID: ownSession},
					Kind:         SessionShimOmittedLineageQuarantined,
					ShimID:       ownShim,
					ProcessEpoch: ownEpoch,
					// Always sent by this arm, and legitimately zero: a frozen
					// v1 record has no authenticated generation to report.
					ControllerGeneration: 0,
				}},
				Err: errors.New("the batch omitted a prior active quarantine obligation"),
			}
		}
		return confirmedReceipt(batch, "revision-generation-zero"), nil
	})
	if err := h.daemon.InstallSessionShimComposition(ctx, cfg); err != nil {
		t.Fatalf("a refusal about a lineage this host itself declared failed the composition: %v", err)
	}
	if !h.daemon.SessionShimHostAttestation().Supports() {
		t.Fatal("durable sessions came up OFF over a quarantine obligation the daemon could have re-declared")
	}

	mu.Lock()
	committed := append([]SessionShimAdoptionBatch(nil), batches...)
	mu.Unlock()
	declared, ok := batchQuarantine(committed[len(committed)-1], ownSession)
	if !ok {
		t.Fatalf("the re-composed batch still omits the lineage; batch = %+v", committed[len(committed)-1])
	}
	if declared.ShimID != ownShim || declared.ProcessEpoch != ownEpoch {
		t.Fatalf("declared incarnation = %s@%d, want %s@%d", declared.ShimID, declared.ProcessEpoch, ownShim, ownEpoch)
	}
	if declared.ControllerGeneration != 0 {
		t.Fatalf("declared generation = %d, want the 0 the control plane reported — inventing one would present a "+
			"row its exact-key lookup cannot match", declared.ControllerGeneration)
	}
}

// TestOmittedLineageDeclarabilityByArm pins the guard per arm.
//
// The adopted arm is the only one whose generation is nullable, so it is the
// only one where a zero means "the reporter dropped it". The quarantined and
// preparing arms both always report a generation and are both answerable by a
// declaration at the exact incarnation. What is NOT declarable is an arm this
// daemon does not recognise — the zero value above all, which is what an
// embedder that has not been taught to report one sends: the generation rule
// cannot be chosen without the arm, so the refusal degrades visibly instead.
func TestOmittedLineageDeclarabilityByArm(t *testing.T) {
	batch := SessionShimAdoptionBatch{OrgID: "org", HostID: "host"}
	lineage := func(session string, kind SessionShimOmittedLineageKind, generation uint64) SessionShimOmittedLineage {
		return SessionShimOmittedLineage{
			Identity:             sessionshim.Identity{OrgID: "org", SessionID: session},
			Kind:                 kind,
			ShimID:               "shim-" + session,
			ProcessEpoch:         1,
			ControllerGeneration: generation,
		}
	}
	for _, tc := range []struct {
		name     string
		lineage  SessionShimOmittedLineage
		declared bool
	}{
		{"quarantined at generation zero", lineage("q0", SessionShimOmittedLineageQuarantined, 0), true},
		{"quarantined with a generation", lineage("q1", SessionShimOmittedLineageQuarantined, 3), true},
		{"adopted with a generation", lineage("a1", SessionShimOmittedLineageAdopted, 3), true},
		{"adopted with the generation dropped", lineage("a0", SessionShimOmittedLineageAdopted, 0), false},
		{"preparing with a generation", lineage("p1", SessionShimOmittedLineagePreparing, 3), true},
		{"preparing at generation zero", lineage("p0", SessionShimOmittedLineagePreparing, 0), true},
		{"arm not reported", lineage("u1", "", 3), false},
		{"arm not recognised", lineage("x1", "some-future-arm", 3), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, declarations := sessionShimBatchDeclaringOmittedLineages(
				batch, []SessionShimOmittedLineage{tc.lineage}, "detail")
			if (len(declarations) == 1) != tc.declared {
				t.Fatalf("declarations = %d, want declared = %v", len(declarations), tc.declared)
			}
		})
	}
}

// TestPreparingOmissionIsDeclaredAndRecovers is the arm this change was wrong
// about on its first pass, so it is pinned end to end rather than at the guard.
//
// A prepared recovery obligation is a handoff the control plane admitted but
// has not seen committed, and the prepare path records it WITH a shim id and a
// process epoch — the ordinary shape, not the exception. Presenting it
// quarantined at that incarnation converts the obligation in place, before the
// completeness check that refused the batch is evaluated, so the batch commits.
//
// Refusing to declare it instead would stand the scope's durable sessions down
// over a refusal the daemon could have settled, permanently: the platform only
// retires such an obligation on its own for a session still provisioning, so an
// active one keeps refusing every batch until an operator intervenes.
func TestPreparingOmissionIsDeclaredAndRecovers(t *testing.T) {
	ctx := context.Background()
	h := newCompositionHarness(t)
	h.start(ctx)

	const (
		preparing      = "sess-prepared-handoff"
		preparingShim  = "shim-prepared-handoff"
		preparingEpoch = uint64(6)
	)
	var (
		mu      sync.Mutex
		batches []SessionShimAdoptionBatch
	)
	cfg := h.composedConfig(func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		mu.Lock()
		batches = append(batches, cloneSessionShimAdoptionBatch(batch))
		mu.Unlock()
		// Models the receiver: a quarantined outcome at the obligation's exact
		// incarnation converts the preparing row, so the refusal stops.
		if _, ok := batchQuarantine(batch, preparing); !ok {
			return SessionShimAdoptionBatchReceipt{}, &SessionShimAdoptionBatchLineagesOmitted{
				Lineages: []SessionShimOmittedLineage{{
					Identity:             sessionshim.Identity{OrgID: h.orgID, SessionID: preparing},
					Kind:                 SessionShimOmittedLineagePreparing,
					ShimID:               preparingShim,
					ProcessEpoch:         preparingEpoch,
					ControllerGeneration: 2,
				}},
				Err: errors.New("the batch omitted a prepared live recovery obligation"),
			}
		}
		return confirmedReceipt(batch, "revision-preparing"), nil
	})
	if err := h.daemon.InstallSessionShimComposition(ctx, cfg); err != nil {
		t.Fatalf("a refusal about a prepared obligation failed the composition: %v", err)
	}
	if !h.daemon.SessionShimHostAttestation().Supports() {
		t.Fatal("durable sessions came up OFF over a prepared obligation the daemon could have declared")
	}

	mu.Lock()
	committed := append([]SessionShimAdoptionBatch(nil), batches...)
	mu.Unlock()
	declared, ok := batchQuarantine(committed[len(committed)-1], preparing)
	if !ok {
		t.Fatalf("the re-composed batch still omits the prepared obligation; batch = %+v", committed[len(committed)-1])
	}
	if declared.ShimID != preparingShim || declared.ProcessEpoch != preparingEpoch {
		t.Fatalf("declared incarnation = %s@%d, want %s@%d — the conversion matches on the exact incarnation",
			declared.ShimID, declared.ProcessEpoch, preparingShim, preparingEpoch)
	}
	if declared.ControllerGeneration != 2 {
		t.Fatalf("declared generation = %d, want the 2 the control plane reported", declared.ControllerGeneration)
	}
}

// TestMalformedRegistryEntryDoesNotSeedAScope is the second half.
//
// A registry file too corrupt to decode is quarantined with whatever identity
// is available, which is NONE. Seeding the batch scope set from it asks for a
// batch scoped to the empty organization, and the first thing that scope's loop
// does is resolve a host identity for it — a hard failure of the whole
// composition, on every start, for one corrupt file on disk. Same class as a
// stale record for an unserved scope, reached through a different door.
//
// The entry must keep its capacity charge: something may still be running out
// there, and §D7 charges for exactly that. It simply names no scope.
func TestMalformedRegistryEntryDoesNotSeedAScope(t *testing.T) {
	ctx := context.Background()
	h := newCompositionHarness(t)
	h.start(ctx)

	if err := writeMalformedShimRecord(t, h.registryDir, "corrupt"); err != nil {
		t.Fatalf("write malformed record: %v", err)
	}

	var (
		mu      sync.Mutex
		batches []SessionShimAdoptionBatch
	)
	cfg := h.composedConfig(func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		mu.Lock()
		batches = append(batches, cloneSessionShimAdoptionBatch(batch))
		mu.Unlock()
		return confirmedReceipt(batch, "revision-malformed"), nil
	})
	if err := h.daemon.InstallSessionShimComposition(ctx, cfg); err != nil {
		t.Fatalf("one undecodable registry file failed the whole composition: %v", err)
	}
	if !h.daemon.SessionShimHostAttestation().Supports() {
		t.Fatal("durable sessions came up OFF over an undecodable registry file")
	}

	mu.Lock()
	committed := append([]SessionShimAdoptionBatch(nil), batches...)
	mu.Unlock()
	for _, batch := range committed {
		if batch.OrgID == "" {
			t.Fatalf("composed a batch for the empty organization: %+v", batch)
		}
		for _, entry := range batch.Quarantined {
			if entry.OrgID == "" || entry.SessionID == "" {
				t.Fatalf("a batch carried a quarantine entry with no lifecycle identity, which the receiver's "+
					"schema refuses outright: %+v", entry)
			}
		}
	}

	// It is still occupied capacity — visible, charged, and named nowhere else.
	var malformed bool
	for _, q := range h.daemon.QuarantinedSessions() {
		if q.Reason == sessionshim.QuarantineRecordMalformed {
			malformed = true
			if !q.ConsumesCapacity {
				t.Fatal("an undecodable registry entry stopped charging capacity; something may still be running")
			}
		}
	}
	if !malformed {
		t.Fatal("the undecodable entry was dropped instead of quarantined")
	}
}

// writeMalformedShimRecord publishes a registry entry the scan can find but
// cannot decode — the shape a truncated write or a corrupted file leaves.
//
// It goes through the registry's own directory so the scan's safety checks
// (mode, ownership) still pass; only the CONTENT is unreadable, which is
// exactly the case that yields a quarantine entry with no lifecycle identity.
func writeMalformedShimRecord(t *testing.T, registryDir, name string) error {
	t.Helper()
	registry, err := sessionshim.NewRegistry(registryDir)
	if err != nil {
		return err
	}
	// A well-formed record for a throwaway identity gives us a correctly-named
	// file in a correctly-moded directory; truncating its contents is what
	// makes it undecodable without touching anything else.
	seed := sessionshim.Identity{OrgID: "org-corrupt", SessionID: name}
	if err := registry.Put(sessionshim.Record{
		SchemaVersion:     sessionshim.RecordSchemaVersion,
		OrgID:             seed.OrgID,
		SessionID:         seed.SessionID,
		ShimID:            "shim-" + name,
		ProcessEpoch:      1,
		PID:               1,
		ProcessStartedAt:  1,
		SocketPath:        registry.SocketPath(seed),
		ProtocolMin:       shimwire.ProtocolMin,
		ProtocolMax:       shimwire.ProtocolMax,
		Phase:             shimwire.PhaseRunning,
		CreatedAtUnixNano: time.Now().UnixNano(),
	}); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(registryDir, seed.RecordName()), []byte("{not json"), 0o600)
}
