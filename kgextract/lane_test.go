package kgextract

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/worker"
)

// TestNewLane_CapabilityAndHandlerShipTogether pins the invariant the lane
// exists for: the tag a worker advertises and the executor that runs the work
// come from ONE construction, so no caller can obtain the tag without the
// handler. Advertising without executing is worse than not advertising at all —
// the coordinator pops the claimed item off the org queue and it is lost.
func TestNewLane_CapabilityAndHandlerShipTogether(t *testing.T) {
	t.Parallel()

	lane := NewLane(Options{})
	if lane.Capability != WorkTypeKGExtraction {
		t.Errorf("lane.Capability = %q, want %q", lane.Capability, WorkTypeKGExtraction)
	}
	if lane.Handler == nil {
		t.Error("lane.Handler is nil: the advertised capability would claim work nothing runs")
	}
	if lane.Executor == nil {
		t.Error("lane.Executor is nil")
	}
	if lane.Executor.emitterFactory == nil {
		t.Error("lane.Executor has no emitter factory: every item would fail before an emit is attempted")
	}
}

// TestNewLane_HandlerRoutesToExecutor proves the handler really reaches the
// executor (rather than being an inert stub): an item whose contract version the
// worker does not speak is rejected BY THE EXECUTOR, and an item for another
// work-type is skipped without touching it.
func TestNewLane_HandlerRoutesToExecutor(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		workType string
		version  int
		wantErr  string
	}{
		{
			name:     "kg_item_reaches_executor",
			workType: WorkTypeKGExtraction,
			version:  KGExtractionContractVersion + 99,
			wantErr:  "unsupported contract version",
		},
		{
			name:     "other_work_type_is_skipped",
			workType: "code-survival-scan",
			version:  KGExtractionContractVersion,
			wantErr:  "",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			lane := NewLane(Options{Logger: discardLogger()})
			raw, err := json.Marshal(KgExtractWorkItem{
				BatchJobID:      "batch:kg_extract:lane",
				WorkType:        tc.workType,
				ContractVersion: tc.version,
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			gotErr := lane.Handler(context.Background(), worker.BatchWorkItem{
				BatchJobID: "batch:kg_extract:lane",
				WorkType:   tc.workType,
				Raw:        raw,
			})
			switch {
			case tc.wantErr == "" && gotErr != nil:
				t.Fatalf("handler err = %v, want nil", gotErr)
			case tc.wantErr != "" && gotErr == nil:
				t.Fatalf("handler err = nil, want one containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(gotErr.Error(), tc.wantErr):
				t.Fatalf("handler err = %v, want one containing %q", gotErr, tc.wantErr)
			}
		})
	}
}

// TestNewLane_CustomEmitterFactoryWins verifies the caller-supplied factory is
// not clobbered by the default (the default only fills a nil field).
func TestNewLane_CustomEmitterFactoryWins(t *testing.T) {
	t.Parallel()

	var called bool
	lane := NewLane(Options{
		Logger: discardLogger(),
		EmitterFactory: func(context.Context, KgExtractWorkItem) (Emitter, error) {
			called = true
			return &stubEmitter{seq: []stubEmitResp{{out: `{"nodes":[],"edges":[]}`}}}, nil
		},
	})
	if _, err := lane.Executor.emitterFor(context.Background(), KgExtractWorkItem{}); err != nil {
		t.Fatalf("emitterFor: %v", err)
	}
	if !called {
		t.Error("custom EmitterFactory was not used")
	}
}

// TestDefaultEmitterFactory_UnknownProviderErrors keeps the "no silent success"
// posture: a provider this worker cannot run surfaces as an emitter error, which
// the executor folds into a status:"error" result the coordinator can see.
func TestDefaultEmitterFactory_UnknownProviderErrors(t *testing.T) {
	t.Parallel()

	_, err := DefaultEmitterFactory(context.Background(), KgExtractWorkItem{Provider: "not-a-provider"})
	if err == nil {
		t.Fatal("DefaultEmitterFactory: err = nil, want an error for an unwired provider")
	}
	if !strings.Contains(err.Error(), "not-a-provider") {
		t.Errorf("error should name the provider; got %v", err)
	}
}
