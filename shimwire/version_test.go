package shimwire

import (
	"errors"
	"testing"
)

func TestNegotiateSelectsTheHighestSharedVersion(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name                             string
		peerMin, peerMax, ourMin, ourMax uint32
		want                             uint32
		wantErr                          bool
	}{
		{name: "identical ranges", peerMin: 1, peerMax: 1, ourMin: 1, ourMax: 1, want: 1},
		{
			// The case the ADR actually cares about: a daemon built long after a
			// shim must still adopt it. Version EQUALITY would refuse this.
			name:    "newer daemon adopts older shim",
			peerMin: 1, peerMax: 1, ourMin: 1, ourMax: 4, want: 1,
		},
		{
			name:    "older daemon adopts newer shim within overlap",
			peerMin: 1, peerMax: 4, ourMin: 1, ourMax: 2, want: 2,
		},
		{name: "picks the top of the overlap", peerMin: 2, peerMax: 7, ourMin: 5, ourMax: 9, want: 7},
		{name: "disjoint below", peerMin: 1, peerMax: 2, ourMin: 3, ourMax: 4, wantErr: true},
		{name: "disjoint above", peerMin: 9, peerMax: 10, ourMin: 1, ourMax: 2, wantErr: true},
		{name: "peer inverted range", peerMin: 5, peerMax: 1, ourMin: 1, ourMax: 9, wantErr: true},
		{name: "local inverted range", peerMin: 1, peerMax: 9, ourMin: 5, ourMax: 1, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Negotiate(tc.peerMin, tc.peerMax, tc.ourMin, tc.ourMax)
			if tc.wantErr {
				if !errors.Is(err, ErrVersionMismatch) {
					t.Fatalf("Negotiate = (%d, %v), want ErrVersionMismatch", got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Negotiate: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Negotiate = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestThisBuildAdvertisesASaneRange(t *testing.T) {
	t.Parallel()

	if ProtocolMin > ProtocolMax {
		t.Fatalf("this build advertises an inverted range [%d,%d]", ProtocolMin, ProtocolMax)
	}
	if ProtocolMin == 0 {
		t.Fatal("ProtocolMin is 0; version 0 is not selectable")
	}
	if got, err := Negotiate(ProtocolMin, ProtocolMax, ProtocolMin, ProtocolMax); err != nil || got != ProtocolMax {
		t.Fatalf("this build cannot negotiate with itself: got (%d, %v)", got, err)
	}
}

func TestStableFamilyAdvertisesV1V2AndSelectsHighestOverlap(t *testing.T) {
	t.Parallel()
	if ProtocolName != "session-shim-v1" || ProtocolMin != V1 || ProtocolMax != V2 {
		t.Fatalf("family/range = %q [%d,%d], want stable token [1,2]", ProtocolName, ProtocolMin, ProtocolMax)
	}
	if got, err := Negotiate(V1, V1, ProtocolMin, ProtocolMax); err != nil || got != V1 {
		t.Fatalf("old-shim overlap = (%d,%v), want selected v1", got, err)
	}
	if got, err := Negotiate(V1, V2, ProtocolMin, ProtocolMax); err != nil || got != V2 {
		t.Fatalf("new overlap = (%d,%v), want selected v2", got, err)
	}
}

func TestExtensionsFailClosedOnUnsupportedRequirement(t *testing.T) {
	t.Parallel()

	t.Run("required and unknown is refused", func(t *testing.T) {
		t.Parallel()
		// A silent downgrade is indistinguishable from a working session until the
		// missing behaviour matters, so an unsupported REQUIREMENT is fatal.
		e := Extensions{Required: []string{"some_future_capability"}}
		if err := e.CheckRequired(); !errors.Is(err, ErrExtensionUnsupported) {
			t.Fatalf("CheckRequired = %v, want ErrExtensionUnsupported", err)
		}
	})

	t.Run("required and known is accepted", func(t *testing.T) {
		t.Parallel()
		e := Extensions{Required: []string{ExtCarrierEpoch}}
		if err := e.CheckRequired(); err != nil {
			t.Fatalf("CheckRequired(%s) = %v, want nil", ExtCarrierEpoch, err)
		}
	})

	t.Run("unknown optional is ignored", func(t *testing.T) {
		t.Parallel()
		// An OSS-only peer must be able to talk to a composing one that offers
		// extensions it has never heard of.
		e := Extensions{Values: map[string]string{"unknown_ext": "whatever"}}
		if err := e.CheckRequired(); err != nil {
			t.Fatalf("CheckRequired with unknown OPTIONAL extension = %v, want nil", err)
		}
		if _, ok := e.Get("unknown_ext"); !ok {
			t.Fatal("Get should still surface an unknown optional value to a peer that wants it")
		}
		if _, ok := e.Get("absent"); ok {
			t.Fatal("Get of an absent name reported present")
		}
	})

	t.Run("empty set always passes", func(t *testing.T) {
		t.Parallel()
		if err := (Extensions{}).CheckRequired(); err != nil {
			t.Fatalf("CheckRequired on the empty set = %v; an OSS-only peer negotiates nothing", err)
		}
	})
}

func TestCarrierEpochIsTheOnlyNamedExtension(t *testing.T) {
	t.Parallel()

	// §D3: the OSS protocol defines ONE generic extension point and names no
	// relay, service, or hosted endpoint. This pins that boundary.
	if len(supported) != 1 {
		t.Fatalf("supported extensions = %v, want exactly {%s}", supported, ExtCarrierEpoch)
	}
	if !supported[ExtCarrierEpoch] {
		t.Fatalf("supported extensions = %v, want %s", supported, ExtCarrierEpoch)
	}
}
