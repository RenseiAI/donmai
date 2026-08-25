package daemon

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

// TestQuarantineChangeIsPublished is the control for the defect P51 hit at
// layer 13. The platform's heartbeat preflight compares the beat's quarantine
// set against the snapshot the last adoption-batch commit stored, byte for
// byte, and demotes the host to `draining` when they disagree. A quarantine
// that changes the daemon's own projection without publishing it therefore
// takes the host out of service until something else republishes.
//
// The assertion is the agreement itself: whatever QuarantinedSessions reports
// must equal what the last published batch carried.
func TestQuarantineChangeIsPublished(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var published []SessionShimAdoptionBatch
	d := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
		HostIDForOrg: func(context.Context, string) (string, error) { return "wh_test_host", nil },
		OnAdoptionBatch: func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			mu.Lock()
			defer mu.Unlock()
			published = append(published, cloneSessionShimAdoptionBatch(batch))
			return SessionShimAdoptionBatchReceipt{DurableCorrelation: []byte("rev-1"), AdoptionRevision: "1"}, nil
		},
	}})

	q := sessionshim.NewQuarantinedSession(sessionshim.Record{
		SchemaVersion: sessionshim.RecordSchemaVersion,
		OrgID:         "org-publish", SessionID: "session-publish",
		ShimID: "shim-publish", ProcessEpoch: 4,
		CreatedAtUnixNano: time.Now().UnixNano(),
	}, sessionshim.QuarantineSocketUnreachable, "controller stream ended before a terminal observation", time.Now())
	q.ControllerGeneration = 2

	d.shims.mu.Lock()
	d.upsertShimQuarantineLocked(q)
	d.shims.mu.Unlock()

	d.publishSessionShimProjection(context.Background(), "org-publish")

	mu.Lock()
	defer mu.Unlock()
	if len(published) != 1 {
		t.Fatalf("published %d batches, want exactly one", len(published))
	}
	batch := published[0]
	if batch.OrgID != "org-publish" || batch.HostID != "wh_test_host" {
		t.Fatalf("batch scope = %q/%q, want org-publish/wh_test_host", batch.OrgID, batch.HostID)
	}
	beat := d.QuarantinedSessions()
	if len(beat) != len(batch.Quarantined) {
		t.Fatalf("heartbeat projection has %d quarantined, published batch has %d — the platform refuses a beat that disagrees",
			len(beat), len(batch.Quarantined))
	}
	for i := range beat {
		if beat[i] != batch.Quarantined[i] {
			t.Fatalf("quarantine entry %d differs between the beat and the published batch:\n beat=%+v\n batch=%+v",
				i, beat[i], batch.Quarantined[i])
		}
	}
}

// TestQuarantinePublishIsScopedToOneOrganization keeps the republish from
// leaking one organization's sessions into another's durable projection.
func TestQuarantinePublishIsScopedToOneOrganization(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var published []SessionShimAdoptionBatch
	d := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
		HostIDForOrg: func(_ context.Context, org string) (string, error) { return "wh_" + org, nil },
		OnAdoptionBatch: func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			mu.Lock()
			defer mu.Unlock()
			published = append(published, cloneSessionShimAdoptionBatch(batch))
			return SessionShimAdoptionBatchReceipt{DurableCorrelation: []byte("rev-1"), AdoptionRevision: "1"}, nil
		},
	}})
	for _, org := range []string{"org-a", "org-b"} {
		q := sessionshim.NewQuarantinedSession(sessionshim.Record{
			SchemaVersion: sessionshim.RecordSchemaVersion,
			OrgID:         org, SessionID: "session-" + org, ShimID: "shim-" + org, ProcessEpoch: 1,
			CreatedAtUnixNano: time.Now().UnixNano(),
		}, sessionshim.QuarantineSocketUnreachable, "test", time.Now())
		d.shims.mu.Lock()
		d.upsertShimQuarantineLocked(q)
		d.shims.mu.Unlock()
	}

	d.publishSessionShimProjection(context.Background(), "org-a")

	mu.Lock()
	defer mu.Unlock()
	if len(published) != 1 {
		t.Fatalf("published %d batches, want one", len(published))
	}
	if got := len(published[0].Quarantined); got != 1 {
		t.Fatalf("org-a batch carried %d quarantined sessions, want only its own", got)
	}
	if published[0].Quarantined[0].OrgID != "org-a" {
		t.Fatalf("org-a batch carried %q", published[0].Quarantined[0].OrgID)
	}
}

// TestEveryQuarantineMutationPublishes is the invariant, not one instance of
// it. Two of the four sites that change the quarantine set published it and two
// did not, and nothing failed when they did not — which is how a dropped
// controller connection came to drain its own host. Any new call site must
// either publish, or assemble a batch that will be published, in the same
// function.
func TestEveryQuarantineMutationPublishes(t *testing.T) {
	t.Parallel()
	const (
		mutator   = "upsertShimQuarantineLocked"
		publishes = "publishSessionShimProjection"
		assembles = "completeSessionShimAdoptionBatch"
	)
	entries, err := os.ReadDir("./")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, filepath.Join("./", name), nil, 0)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", name, parseErr)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Name.Name == mutator {
				continue
			}
			var calls []string
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, isCall := n.(*ast.CallExpr)
				if !isCall {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr:
					calls = append(calls, fun.Sel.Name)
				case *ast.Ident:
					calls = append(calls, fun.Name)
				}
				return true
			})
			if !contains(calls, mutator) {
				continue
			}
			checked++
			if !contains(calls, publishes) && !contains(calls, assembles) {
				t.Errorf("%s: %s changes the quarantine projection without publishing it — "+
					"the platform demotes a host whose heartbeat disagrees with the last published batch",
					name, fn.Name.Name)
			}
		}
	}
	if checked == 0 {
		t.Fatalf("found no call site of %s — the guard is not watching anything", mutator)
	}
}

func contains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}

// TestAdoptedBatchOrderMatchesTheReceiverComparator pins the daemon's adopted
// ordering to the exact tuple the platform re-checks on arrival. The receiving
// side refuses a batch whose rows are not in ITS order, so a comparator that
// agrees on a prefix and diverges on the tail produces a batch that is correct
// here and rejected there. The tail is ControllerGeneration, which this
// comparator used to omit.
func TestAdoptedBatchOrderMatchesTheReceiverComparator(t *testing.T) {
	t.Parallel()
	outcome := func(org, session, shim string, epoch, generation uint64) SessionShimAdoptionOutcome {
		return SessionShimAdoptionOutcome{Evidence: SessionShimAdoptionEvidence{
			Identity:     sessionshim.Identity{OrgID: org, SessionID: session},
			ShimID:       shim,
			ProcessEpoch: epoch, ControllerGeneration: generation,
		}}
	}
	// Deliberately reversed on every key, innermost first.
	in := []SessionShimAdoptionOutcome{
		outcome("org-b", "session-a", "shim-a", 1, 1),
		outcome("org-a", "session-a", "shim-a", 1, 9),
		outcome("org-a", "session-a", "shim-a", 1, 2),
		outcome("org-a", "session-a", "shim-a", 0, 5),
		outcome("org-a", "session-a", "shim-0", 1, 5),
		outcome("org-a", "session-0", "shim-a", 1, 5),
	}
	sortSessionShimAdoptionOutcomes(in)

	// The receiver's comparator, transcribed from the platform's
	// compareBatchCorrelation: org, session, shim, processEpoch, generation.
	for i := 1; i < len(in); i++ {
		prev, cur := in[i-1].Evidence, in[i].Evidence
		switch {
		case prev.Identity.OrgID != cur.Identity.OrgID:
			if prev.Identity.OrgID > cur.Identity.OrgID {
				t.Fatalf("entry %d: org out of order (%q after %q)", i, cur.Identity.OrgID, prev.Identity.OrgID)
			}
		case prev.Identity.SessionID != cur.Identity.SessionID:
			if prev.Identity.SessionID > cur.Identity.SessionID {
				t.Fatalf("entry %d: session out of order (%q after %q)", i, cur.Identity.SessionID, prev.Identity.SessionID)
			}
		case prev.ShimID != cur.ShimID:
			if prev.ShimID > cur.ShimID {
				t.Fatalf("entry %d: shim out of order (%q after %q)", i, cur.ShimID, prev.ShimID)
			}
		case prev.ProcessEpoch != cur.ProcessEpoch:
			if prev.ProcessEpoch > cur.ProcessEpoch {
				t.Fatalf("entry %d: process epoch out of order (%d after %d)", i, cur.ProcessEpoch, prev.ProcessEpoch)
			}
		default:
			if prev.ControllerGeneration > cur.ControllerGeneration {
				t.Fatalf("entry %d: controller generation out of order (%d after %d) — "+
					"the receiver refuses a batch whose rows are not in its order",
					i, cur.ControllerGeneration, prev.ControllerGeneration)
			}
		}
	}
}
