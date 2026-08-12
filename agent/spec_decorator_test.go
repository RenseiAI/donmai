package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
)

// recordingProvider is a plain (non-HarnessProvider) Provider double that
// records the exact Spec each Spawn/Resume call received, so tests can
// assert on what actually reached the provider boundary rather than on
// DecorateProvider's internals.
type recordingProvider struct {
	name        ProviderName
	spawnSpec   Spec
	spawnCalls  int
	resumeSpec  Spec
	resumeCalls int
}

func (p *recordingProvider) Name() ProviderName { return p.name }
func (p *recordingProvider) Capabilities() Capabilities {
	return Capabilities{SupportsSessionResume: true}
}

func (p *recordingProvider) Spawn(_ context.Context, spec Spec) (Handle, error) {
	p.spawnCalls++
	p.spawnSpec = spec
	return &noopHandle{id: "recording-spawn"}, nil
}

func (p *recordingProvider) Resume(_ context.Context, _ string, spec Spec) (Handle, error) {
	p.resumeCalls++
	p.resumeSpec = spec
	return &noopHandle{id: "recording-resume"}, nil
}

func (p *recordingProvider) Shutdown(_ context.Context) error { return nil }

// recordingHarnessProvider is the HarnessProvider-implementing sibling of
// recordingProvider — exercises the branch of DecorateProvider that must
// preserve HarnessProvider satisfaction (runner/harness_selection.go
// type-asserts registered providers against it for manifest-based
// selection).
type recordingHarnessProvider struct {
	recordingProvider
	manifest HarnessManifest
}

func (p *recordingHarnessProvider) Manifest() HarnessManifest { return p.manifest }

// validDelivery returns a structurally well-formed inline ExtensionDelivery
// (correct digest included) so tests can assert appended deliveries still
// pass the real production validator (ValidateExtensionDeliveries) and
// digest check (VerifyExtensionDigest) rather than a hand-rolled fixture
// that only looks right.
func validDelivery(t *testing.T, id, content string) ExtensionDelivery {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	return ExtensionDelivery{
		ID:       id,
		Kind:     ExtensionDeliveryInline,
		Source:   []byte(content),
		Basename: id + ".js",
		Digest:   hex.EncodeToString(sum[:]),
	}
}

func TestDecorateProvider_NilInputsPassThrough(t *testing.T) {
	t.Parallel()

	t.Run("nil decorator returns the identical provider", func(t *testing.T) {
		t.Parallel()
		p := &recordingProvider{name: ProviderStub}
		got := DecorateProvider(p, nil)
		if got != Provider(p) {
			t.Fatalf("DecorateProvider(p, nil) = %#v, want the identical p (no wrapping)", got)
		}
	})

	t.Run("nil provider returns nil", func(t *testing.T) {
		t.Parallel()
		got := DecorateProvider(nil, func(Spec) []ExtensionDelivery { return nil })
		if got != nil {
			t.Fatalf("DecorateProvider(nil, decorate) = %#v, want nil", got)
		}
	})
}

// TestDecorateProvider_ReachesSpawnAndResume proves the ONE registered
// decorator runs on the Spec argument of BOTH real Provider methods —
// Spawn and Resume — not just one of them, satisfied via each method's
// real, unmodified signature (context.Context, ..., Spec) -> (Handle,
// error), exactly as agent-run orchestration and any future Resume caller
// invoke them. Table-driven over the two provider shapes DecorateProvider
// must support: a plain Provider and a HarnessProvider.
func TestDecorateProvider_ReachesSpawnAndResume(t *testing.T) {
	t.Parallel()

	delivery := validDelivery(t, "embedder-pack", "console.log('embedder')")
	decorate := func(Spec) []ExtensionDelivery { return []ExtensionDelivery{delivery} }

	tests := []struct {
		name string
		make func() (Provider, func() (spawnSpec, resumeSpec Spec, spawnCalls, resumeCalls int))
	}{
		{
			name: "plain Provider",
			make: func() (Provider, func() (Spec, Spec, int, int)) {
				rp := &recordingProvider{name: ProviderStub}
				return rp, func() (Spec, Spec, int, int) {
					return rp.spawnSpec, rp.resumeSpec, rp.spawnCalls, rp.resumeCalls
				}
			},
		},
		{
			name: "HarnessProvider",
			make: func() (Provider, func() (Spec, Spec, int, int)) {
				rp := &recordingHarnessProvider{
					recordingProvider: recordingProvider{name: ProviderPi},
					manifest:          HarnessManifest{Name: HarnessPi},
				}
				return rp, func() (Spec, Spec, int, int) {
					return rp.spawnSpec, rp.resumeSpec, rp.spawnCalls, rp.resumeCalls
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inner, read := tc.make()
			decorated := DecorateProvider(inner, decorate)

			if _, err := decorated.Spawn(context.Background(), Spec{Prompt: "spawn"}); err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			if _, err := decorated.Resume(context.Background(), "sess-1", Spec{Prompt: "resume"}); err != nil {
				t.Fatalf("Resume: %v", err)
			}

			spawnSpec, resumeSpec, spawnCalls, resumeCalls := read()
			if spawnCalls != 1 {
				t.Errorf("inner Spawn calls = %d, want 1 (decorator must delegate, not swallow)", spawnCalls)
			}
			if resumeCalls != 1 {
				t.Errorf("inner Resume calls = %d, want 1 (decorator must delegate, not swallow)", resumeCalls)
			}
			if len(spawnSpec.AdditionalExtensions) != 1 || spawnSpec.AdditionalExtensions[0].ID != delivery.ID {
				t.Errorf("Spawn: inner saw AdditionalExtensions=%+v, want [%q]", spawnSpec.AdditionalExtensions, delivery.ID)
			}
			if len(resumeSpec.AdditionalExtensions) != 1 || resumeSpec.AdditionalExtensions[0].ID != delivery.ID {
				t.Errorf("Resume: inner saw AdditionalExtensions=%+v, want [%q]", resumeSpec.AdditionalExtensions, delivery.ID)
			}
			// The un-decorated fields the caller set must survive untouched —
			// this hook appends one field, never rewrites the rest of the spec.
			if spawnSpec.Prompt != "spawn" {
				t.Errorf("Spawn: inner saw Prompt=%q, want %q (decorator must not touch other fields)", spawnSpec.Prompt, "spawn")
			}
			if resumeSpec.Prompt != "resume" {
				t.Errorf("Resume: inner saw Prompt=%q, want %q (decorator must not touch other fields)", resumeSpec.Prompt, "resume")
			}
		})
	}
}

// TestDecorateProvider_PreservesHarnessProvider proves decorating a
// HarnessProvider does not silently drop it to a plain Provider. Losing
// that satisfaction would make the decorated provider invisible to
// manifest-based admission (runner/harness_selection.go type-asserts
// registered providers against agent.HarnessProvider) — a real behavior
// change with no decorator-author intent behind it.
func TestDecorateProvider_PreservesHarnessProvider(t *testing.T) {
	t.Parallel()
	wantManifest := HarnessManifest{Name: HarnessPi, HumanLabel: "pi"}
	inner := &recordingHarnessProvider{
		recordingProvider: recordingProvider{name: ProviderPi},
		manifest:          wantManifest,
	}

	decorated := DecorateProvider(inner, func(Spec) []ExtensionDelivery { return nil })

	hp, ok := decorated.(HarnessProvider)
	if !ok {
		t.Fatalf("DecorateProvider(HarnessProvider, decorate) lost HarnessProvider satisfaction; got %T", decorated)
	}
	if got := hp.Manifest(); got.Name != wantManifest.Name || got.HumanLabel != wantManifest.HumanLabel {
		t.Errorf("Manifest() = %+v, want %+v (must forward to the wrapped provider unchanged)", got, wantManifest)
	}

	// A plain Provider must NOT gain HarnessProvider satisfaction from
	// wrapping — DecorateProvider must not fabricate a capability the
	// wrapped provider never declared.
	plain := &recordingProvider{name: ProviderStub}
	decoratedPlain := DecorateProvider(plain, func(Spec) []ExtensionDelivery { return nil })
	if _, ok := decoratedPlain.(HarnessProvider); ok {
		t.Fatalf("DecorateProvider(plain Provider, decorate) unexpectedly satisfies HarnessProvider")
	}
}

// TestDecorateProvider_OrderingAppendsAfterExisting proves the decorator
// only ever appends: any deliveries orchestration already placed on the
// spec (standing in for the harness's own boundary-first invariant, which
// this hook must never disturb — D1) keep their exact position, and the
// decorator's own deliveries land strictly after them, in the decorator's
// declared order.
func TestDecorateProvider_OrderingAppendsAfterExisting(t *testing.T) {
	t.Parallel()
	existing := validDelivery(t, "boundary-first", "existing")
	added1 := validDelivery(t, "embedder-a", "a")
	added2 := validDelivery(t, "embedder-b", "b")

	rp := &recordingProvider{name: ProviderStub}
	decorated := DecorateProvider(rp, func(Spec) []ExtensionDelivery {
		return []ExtensionDelivery{added1, added2}
	})

	inputSpec := Spec{AdditionalExtensions: []ExtensionDelivery{existing}}
	if _, err := decorated.Spawn(context.Background(), inputSpec); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	got := rp.spawnSpec.AdditionalExtensions
	want := []string{existing.ID, added1.ID, added2.ID}
	if len(got) != len(want) {
		t.Fatalf("AdditionalExtensions = %+v, want %d entries in order %v", got, len(want), want)
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("AdditionalExtensions[%d].ID = %q, want %q (order=%v)", i, got[i].ID, id, want)
		}
	}

	// The caller's original slice must be untouched — decorateSpec must
	// copy, not mutate in place, or a second Spawn/Resume sharing the same
	// backing QueuedWork-derived slice would see leaked entries.
	if len(inputSpec.AdditionalExtensions) != 1 || inputSpec.AdditionalExtensions[0].ID != existing.ID {
		t.Errorf("caller's input spec.AdditionalExtensions was mutated: %+v", inputSpec.AdditionalExtensions)
	}
}

// TestDecorateProvider_NoOpDecoratorLeavesSpecUnchanged proves a decorator
// that appends nothing (nil or empty slice) does not allocate a new
// AdditionalExtensions slice at all — the "zero behavior change when no
// decorator registered" contract extended to "no observable change when a
// registered decorator has nothing to add this call."
func TestDecorateProvider_NoOpDecoratorLeavesSpecUnchanged(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		results []ExtensionDelivery
	}{
		{name: "nil slice", results: nil},
		{name: "empty slice", results: []ExtensionDelivery{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			original := []ExtensionDelivery{validDelivery(t, "pre-existing", "x")}
			spec := Spec{AdditionalExtensions: original}
			got := decorateSpec(spec, func(Spec) []ExtensionDelivery { return tc.results })
			if &got.AdditionalExtensions[0] != &original[0] {
				t.Errorf("no-op decorator reallocated AdditionalExtensions; want the identical backing array")
			}
			if len(got.AdditionalExtensions) != 1 {
				t.Errorf("AdditionalExtensions = %+v, want unchanged len 1", got.AdditionalExtensions)
			}
		})
	}
}

// TestDecorateProvider_AppendedDeliveriesValidate proves an appended
// delivery still passes the real, unmodified validators every harness
// adapter runs before materialization (ValidateExtensionDeliveries) and
// digest-checks correctly against its own source
// (VerifyExtensionDigest) — this hook must never produce a delivery shape
// the downstream digest/verification invariants would reject differently
// than they would have rejected a caller-supplied one.
func TestDecorateProvider_AppendedDeliveriesValidate(t *testing.T) {
	t.Parallel()
	d := validDelivery(t, "embedder-pack", "the extension source")

	rp := &recordingProvider{name: ProviderStub}
	decorated := DecorateProvider(rp, func(Spec) []ExtensionDelivery {
		return []ExtensionDelivery{d}
	})
	if _, err := decorated.Spawn(context.Background(), Spec{}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	got := rp.spawnSpec.AdditionalExtensions
	if len(got) != 1 {
		t.Fatalf("AdditionalExtensions = %+v, want exactly the one appended delivery", got)
	}
	if err := ValidateExtensionDeliveries(got); err != nil {
		t.Fatalf("ValidateExtensionDeliveries(appended) = %v, want nil", err)
	}
	if !VerifyExtensionDigest(got[0].Source, got[0].Digest) {
		t.Fatalf("VerifyExtensionDigest(appended.Source, appended.Digest) = false, want true")
	}
}

// TestDecorateProvider_AppendedMalformedDeliveryFailsRealValidation is split
// out from the happy-path test above so a compile-time typo in the
// intentionally-bad fixture can't hide a passing assertion inside another
// test's success path.
func TestDecorateProvider_AppendedMalformedDeliveryFailsRealValidation(t *testing.T) {
	t.Parallel()
	badRP := &recordingProvider{name: ProviderStub}
	decorated := DecorateProvider(badRP, func(Spec) []ExtensionDelivery {
		return []ExtensionDelivery{{ID: "bad", Kind: ExtensionDeliveryInline, Source: []byte("x"), Basename: "b.js"}} // no Digest
	})
	if _, err := decorated.Spawn(context.Background(), Spec{}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := ValidateExtensionDeliveries(badRP.spawnSpec.AdditionalExtensions); err == nil {
		t.Fatal("ValidateExtensionDeliveries(malformed appended delivery) = nil, want an error")
	}
}

// TestDecorateProvider_ResumeErrorPropagates proves the wrapper is a
// transparent pass-through for errors too: it must not swallow or alter an
// error the wrapped provider returns (e.g. ErrUnsupported for a provider
// that does not support resume).
func TestDecorateProvider_ResumeErrorPropagates(t *testing.T) {
	t.Parallel()
	p := &noopProvider{name: ProviderStub}
	decorated := DecorateProvider(Provider(p), func(Spec) []ExtensionDelivery {
		return []ExtensionDelivery{validDelivery(t, "x", "y")}
	})
	if _, err := decorated.Resume(context.Background(), "sess", Spec{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Resume err = %v, want ErrUnsupported (unchanged from the wrapped provider)", err)
	}
	if _, err := decorated.Spawn(context.Background(), Spec{}); !errors.Is(err, ErrSpawnFailed) {
		t.Fatalf("Spawn err = %v, want ErrSpawnFailed (unchanged from the wrapped provider)", err)
	}
}
