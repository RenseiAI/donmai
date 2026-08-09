package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/kgextract"
	"github.com/RenseiAI/donmai/worker"
)

// kgPollServer serves one poll response carrying kgExtractWork[] on the first
// tick and an empty response afterwards, so the assertions stay deterministic.
// extraWork is appended to work[] on the SECOND and later ticks — used to prove
// session claiming keeps flowing while a kg item executes.
func kgPollServer(t *testing.T, item kgextract.KgExtractWorkItem, laterWork []PollWorkItem) *httptest.Server {
	t.Helper()

	// Serve the item the way the coordinator does — as the full object inside
	// kgExtractWork[] — rather than through PollResponse's Go type, whose Raw
	// field is `json:"-"` on the way out.
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal kg item: %v", err)
	}
	firstBody := []byte(`{"kgExtractWork":[` + string(raw) + `]}`)

	var ticks atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if ticks.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(firstBody)
			return
		}
		_ = json.NewEncoder(w).Encode(PollResponse{Work: laterWork})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func kgTestItem() kgextract.KgExtractWorkItem {
	return kgextract.KgExtractWorkItem{
		BatchJobID:      "batch:kg_extract:poll-1",
		WorkType:        kgextract.WorkTypeKGExtraction,
		ContractVersion: kgextract.KGExtractionContractVersion,
		OrgID:           "org_1",
		ProjectID:       "proj_1",
		Observations:    []kgextract.Observation{{ID: "obs-1", Content: "hello"}},
	}
}

// TestPollService_RoutesKgExtractWork proves the daemon's poll path decodes the
// kgExtractWork[] lane and hands each item to the executor. Before this lane
// existed the field was not decoded at all, so a claimed item — already popped
// off the coordinator's queue — was destroyed by the poll that claimed it.
func TestPollService_RoutesKgExtractWork(t *testing.T) {
	item := kgTestItem()
	srv := kgPollServer(t, item, nil)

	got := make(chan worker.BatchWorkItem, 1)
	p := NewPollService(PollOptions{
		WorkerID:        "wkr_kg",
		OrchestratorURL: srv.URL,
		RuntimeJWT:      "rt-jwt",
		IntervalSeconds: 1,
		OnWork:          func(PollWorkItem) error { return nil },
		OnKgExtractWork: func(_ context.Context, bi worker.BatchWorkItem) error {
			select {
			case got <- bi:
			default:
			}
			return nil
		},
	})
	p.Start()
	t.Cleanup(p.Stop)

	select {
	case bi := <-got:
		if bi.BatchJobID != item.BatchJobID {
			t.Errorf("batchJobId = %q, want %q", bi.BatchJobID, item.BatchJobID)
		}
		if bi.WorkType != kgextract.WorkTypeKGExtraction {
			t.Errorf("workType = %q, want %q", bi.WorkType, kgextract.WorkTypeKGExtraction)
		}
		var decoded kgextract.KgExtractWorkItem
		if err := json.Unmarshal(bi.Raw, &decoded); err != nil {
			t.Fatalf("raw payload does not decode into the contract type: %v", err)
		}
		if len(decoded.Observations) != 1 || decoded.OrgID != "org_1" {
			t.Errorf("decoded item lost its payload: %+v", decoded)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("kg-extraction item was never dispatched")
	}
}

// TestNewPollService_FillsKgExecutorWhenUnwired is the atomicity guard on the
// poll side: a service constructed WITHOUT an explicit handler still executes a
// claimed item, through the real kgextract lane. The item here declares a
// contract version this worker does not speak, so the executor rejects it — and
// that rejection reaching the log is the proof the item was executed rather than
// dropped on the floor.
func TestNewPollService_FillsKgExecutorWhenUnwired(t *testing.T) {
	item := kgTestItem()
	item.ContractVersion = kgextract.KGExtractionContractVersion + 99
	srv := kgPollServer(t, item, nil)

	warns := make(chan string, 8)
	p := NewPollService(PollOptions{
		WorkerID:        "wkr_kg",
		OrchestratorURL: srv.URL,
		RuntimeJWT:      "rt-jwt",
		IntervalSeconds: 1,
		OnWork:          func(PollWorkItem) error { return nil },
		LogWarn: func(format string, args ...any) {
			select {
			case warns <- fmt.Sprintf(format, args...):
			default:
			}
		},
	})
	if p.opts.OnKgExtractWork == nil {
		t.Fatal("NewPollService left OnKgExtractWork nil: a decoded item would be dropped")
	}
	p.Start()
	t.Cleanup(p.Stop)

	deadline := time.After(5 * time.Second)
	for {
		select {
		case line := <-warns:
			if strings.Contains(line, item.BatchJobID) && strings.Contains(line, "contract version") {
				return
			}
		case <-deadline:
			t.Fatal("default kg lane never executed the claimed item")
		}
	}
}

// TestPollService_KgExtractRunsOffPollGoroutine pins the isolation the resident
// daemon needs: an extraction can take minutes, and the poll loop is also the
// only path that claims agent sessions and routes inbox messages. A kg item must
// therefore never run ON the poll goroutine.
func TestPollService_KgExtractRunsOffPollGoroutine(t *testing.T) {
	release := make(chan struct{})
	srv := kgPollServer(t, kgTestItem(), []PollWorkItem{{SessionID: "sess-after-kg"}})

	entered := make(chan struct{}, 1)
	work := make(chan string, 1)
	p := NewPollService(PollOptions{
		WorkerID:        "wkr_kg",
		OrchestratorURL: srv.URL,
		RuntimeJWT:      "rt-jwt",
		IntervalSeconds: 1,
		OnWork: func(item PollWorkItem) error {
			select {
			case work <- item.SessionID:
			default:
			}
			return nil
		},
		OnKgExtractWork: func(ctx context.Context, _ worker.BatchWorkItem) error {
			select {
			case entered <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil
		},
	})
	p.Start()
	t.Cleanup(func() {
		close(release)
		p.Stop()
	})

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("kg handler never entered")
	}
	select {
	case id := <-work:
		if id != "sess-after-kg" {
			t.Errorf("dispatched session = %q, want %q", id, "sess-after-kg")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session claiming stalled while a kg-extraction item was executing")
	}
}

// TestPollService_StopJoinsInFlightKgExtract: a claimed item owns a result the
// coordinator is waiting for, so shutdown must not report completion while one
// is still running — while still never holding the caller past its own deadline.
func TestPollService_StopJoinsInFlightKgExtract(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	srv := kgPollServer(t, kgTestItem(), nil)

	entered := make(chan struct{}, 1)
	p := NewPollService(PollOptions{
		WorkerID:        "wkr_kg",
		OrchestratorURL: srv.URL,
		RuntimeJWT:      "rt-jwt",
		IntervalSeconds: 1,
		OnWork:          func(PollWorkItem) error { return nil },
		OnKgExtractWork: func(context.Context, worker.BatchWorkItem) error {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
			return nil
		},
	})
	p.Start()
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("kg handler never entered")
	}

	bounded, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := p.StopContext(bounded); err == nil {
		t.Fatal("StopContext reported a clean stop while a claimed kg item was still executing")
	}

	releaseOnce.Do(func() { close(release) })
	if err := p.StopContext(context.Background()); err != nil {
		t.Fatalf("StopContext after the item finished: %v", err)
	}
}

// TestPollService_KgExtractConcurrencyIsBounded: items are queued, never
// dropped, when more arrive than the bound allows — a claimed item that is
// skipped for load is a lost item.
func TestPollService_KgExtractConcurrencyIsBounded(t *testing.T) {
	const items = maxConcurrentKgExtract + 2

	release := make(chan struct{})
	var (
		mu      sync.Mutex
		inFlt   int
		maxSeen int
		done    sync.WaitGroup
	)
	done.Add(items)

	p := NewPollService(PollOptions{
		WorkerID:        "wkr_kg",
		OrchestratorURL: "http://127.0.0.1:1",
		RuntimeJWT:      "rt-jwt",
		IntervalSeconds: 1,
		OnWork:          func(PollWorkItem) error { return nil },
		OnKgExtractWork: func(context.Context, worker.BatchWorkItem) error {
			mu.Lock()
			inFlt++
			if inFlt > maxSeen {
				maxSeen = inFlt
			}
			mu.Unlock()
			<-release
			mu.Lock()
			inFlt--
			mu.Unlock()
			done.Done()
			return nil
		},
	})

	for i := range items {
		p.dispatchKgExtract(context.Background(), worker.BatchWorkItem{
			BatchJobID: fmt.Sprintf("batch:kg_extract:%d", i),
			WorkType:   kgextract.WorkTypeKGExtraction,
		})
	}
	// Give the queued goroutines a chance to pile up against the semaphore.
	time.Sleep(50 * time.Millisecond)
	close(release)
	done.Wait()

	mu.Lock()
	defer mu.Unlock()
	if maxSeen > maxConcurrentKgExtract {
		t.Errorf("peak concurrent kg executions = %d, want <= %d", maxSeen, maxConcurrentKgExtract)
	}
	if maxSeen == 0 {
		t.Error("no kg item executed")
	}
}
