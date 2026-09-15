package daemon

import (
	"context"
	"sync"
	"testing"

	"github.com/RenseiAI/donmai/sessionshim"
)

func controlRefFor(t *testing.T, d *Daemon, id sessionshim.Identity) SessionShimControlRef {
	t.Helper()
	entry, err := d.adoptedShimEntry(id.OrgID, id.SessionID)
	if err != nil {
		t.Fatalf("adoptedShimEntry: %v", err)
	}
	return SessionShimControlRef{
		Identity:             id,
		ShimID:               entry.shimID,
		ProcessEpoch:         entry.adoption.ProcessEpoch,
		ControllerGeneration: entry.adoption.ControllerGeneration,
	}
}

func TestReportAdoptedSessionShimCarrierLostForExactRefIsOnceOnly(t *testing.T) {
	t.Parallel()
	var callbacks struct {
		sync.Mutex
		n int
	}
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
		policy: SessionShimReadoptionPolicy{Disabled: true},
		onBindLost: func(context.Context, sessionshim.Identity) {
			callbacks.Lock()
			callbacks.n++
			callbacks.Unlock()
		},
	})
	ref := controlRefFor(t, f.daemon, f.id)
	changed, err := f.daemon.ReportAdoptedSessionShimCarrierLostFor(ref)
	if err != nil || !changed {
		t.Fatalf("ReportAdoptedSessionShimCarrierLostFor = %v, %v, want true, nil", changed, err)
	}
	first := bindingFor(t, f.daemon, f.id)
	if first.CarrierBound || first.LastCarrierLossAt.IsZero() {
		t.Fatalf("binding after loss = %+v, want unbound with one loss instant", first)
	}
	changed, err = f.daemon.ReportAdoptedSessionShimCarrierLostFor(ref)
	if err != nil || changed {
		t.Fatalf("duplicate report = %v, %v, want false, nil", changed, err)
	}
	if second := bindingFor(t, f.daemon, f.id); second.LastCarrierLossAt != first.LastCarrierLossAt {
		t.Fatalf("duplicate report changed loss instant: first=%s second=%s", first.LastCarrierLossAt, second.LastCarrierLossAt)
	}
	callbacks.Lock()
	defer callbacks.Unlock()
	if callbacks.n != 1 {
		t.Fatalf("carrier-loss callbacks = %d, want 1", callbacks.n)
	}
}

func TestCarrierLossAndRebindForRefuseStaleOrReplacementAuthority(t *testing.T) {
	t.Parallel()
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{policy: SessionShimReadoptionPolicy{Disabled: true}})
	ref := controlRefFor(t, f.daemon, f.id)
	for name, mutate := range map[string]func(SessionShimControlRef) SessionShimControlRef{
		"foreign org":      func(in SessionShimControlRef) SessionShimControlRef { in.Identity.OrgID = "other-org"; return in },
		"foreign shim":     func(in SessionShimControlRef) SessionShimControlRef { in.ShimID = "other-shim"; return in },
		"stale epoch":      func(in SessionShimControlRef) SessionShimControlRef { in.ProcessEpoch++; return in },
		"stale generation": func(in SessionShimControlRef) SessionShimControlRef { in.ControllerGeneration++; return in },
	} {
		t.Run(name, func(t *testing.T) {
			changed, err := f.daemon.ReportAdoptedSessionShimCarrierLostFor(mutate(ref))
			if err == nil || changed {
				t.Fatalf("stale report = %v, %v, want false and refusal", changed, err)
			}
			if binding := bindingFor(t, f.daemon, f.id); !binding.CarrierBound {
				t.Fatalf("stale report mutated current binding: %+v", binding)
			}
		})
	}

	if changed, err := f.daemon.ReportAdoptedSessionShimCarrierLostFor(ref); err != nil || !changed {
		t.Fatalf("exact loss report = %v, %v", changed, err)
	}
	if result, err := f.daemon.RebindAdoptedSessionShimFor(context.Background(), ref); err != nil || result != SessionShimRebound {
		t.Fatalf("RebindAdoptedSessionShimFor = %s, %v, want rebound", result, err)
	}
	adoptions, _ := f.snapshot()
	if changed, err := f.daemon.ReportAdoptedSessionShimCarrierLostFor(ref); err == nil || changed {
		t.Fatalf("old ref after replacement = %v, %v, want stale refusal", changed, err)
	}
	if result, err := f.daemon.RebindAdoptedSessionShimFor(context.Background(), ref); err == nil || result != SessionShimNotAdopted {
		t.Fatalf("old ref rebind = %s, %v, want stale refusal", result, err)
	}
	if got, _ := f.snapshot(); got != adoptions {
		t.Fatalf("stale rebind drove %d adoptions, want unchanged at %d", got, adoptions)
	}
	if binding := bindingFor(t, f.daemon, f.id); !binding.CarrierBound {
		t.Fatalf("stale callback unbound replacement: %+v", binding)
	}
}
