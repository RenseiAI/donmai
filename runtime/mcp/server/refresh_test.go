package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afclient/codeintel"
)

func TestServerRefreshReindexesExplicitUncommittedChange(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "refresh.go")
	writeRefreshSource(t, path, "BeforeRefresh")

	s, err := New(Config{Root: root})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.WaitReady(context.Background()); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}

	if got := searchSymbolsText(t, s, "BeforeRefresh"); !strings.Contains(got, "BeforeRefresh") {
		t.Fatalf("initial symbol result = %s, want BeforeRefresh", got)
	}
	writeRefreshSource(t, path, "AfterRefresh")

	if got := searchSymbolsText(t, s, "AfterRefresh"); strings.Contains(got, "AfterRefresh") {
		t.Fatalf("warm cache observed changed symbol before explicit Refresh: %s", got)
	}
	if got := searchSymbolsText(t, s, "BeforeRefresh"); !strings.Contains(got, "BeforeRefresh") {
		t.Fatalf("warm cache lost original symbol before explicit Refresh: %s", got)
	}

	if err := s.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := searchSymbolsText(t, s, "AfterRefresh"); !strings.Contains(got, "AfterRefresh") {
		t.Fatalf("refreshed symbol result = %s, want AfterRefresh", got)
	}
	if got := searchSymbolsText(t, s, "BeforeRefresh"); strings.Contains(got, "BeforeRefresh") {
		t.Fatalf("refreshed result retained old symbol: %s", got)
	}
}

func TestServerRefreshWaitsForInitialWarmupAndUsesCurrentResult(t *testing.T) {
	t.Parallel()
	root := fixtureRepo(t)
	warmDone := make(chan struct{})
	s := &Server{
		runner:   codeintel.NewNativeRunner(root),
		warmDone: warmDone,
		warmErr:  errors.New("initial warm-up failed"),
	}
	done := make(chan error, 1)
	go func() { done <- s.Refresh() }()

	select {
	case err := <-done:
		t.Fatalf("Refresh returned before initial warm-up completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(warmDone)
	if err := <-done; err != nil {
		t.Fatalf("Refresh returned stale warm-up error instead of current result: %v", err)
	}
}

func TestServerRefreshReturnsRunnerFailure(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeRefreshSource(t, filepath.Join(root, "refresh.go"), "CannotPersistRefresh")
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatalf("make root read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
	warmDone := make(chan struct{})
	close(warmDone)
	s := &Server{runner: codeintel.NewNativeRunner(root), warmDone: warmDone}
	if err := s.Refresh(); err == nil {
		t.Fatal("Refresh should report the current runner failure")
	}
}

func TestServerRefreshAndCallConcurrent(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, fixtureRepo(t))
	if err := s.WaitReady(context.Background()); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}

	const calls = 8
	const refreshes = 4
	errCh := make(chan error, calls+refreshes)
	var wg sync.WaitGroup
	for range calls {
		wg.Go(func() {
			res, err := s.Call(context.Background(), ToolSearchSymbols, json.RawMessage(`{"query":"Greeter"}`))
			if err != nil {
				errCh <- err
				return
			}
			if res.IsError {
				errCh <- errors.New("Call returned an operation error")
			}
		})
	}
	for range refreshes {
		wg.Go(func() {
			if err := s.Refresh(); err != nil {
				errCh <- err
			}
		})
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent Call/Refresh: %v", err)
	}
}

func writeRefreshSource(t *testing.T, path, symbol string) {
	t.Helper()
	src := "package refreshed\n\nfunc " + symbol + "() {}\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatalf("write %s: %v", symbol, err)
	}
}

func searchSymbolsText(t *testing.T, s *Server, query string) string {
	t.Helper()
	res, err := s.Call(context.Background(), ToolSearchSymbols, json.RawMessage(`{"query":`+strconv.Quote(query)+`}`))
	if err != nil {
		t.Fatalf("Call(%q): %v", query, err)
	}
	if res.IsError || len(res.Content) != 1 {
		t.Fatalf("Call(%q) = %+v, want a successful text result", query, res)
	}
	return res.Content[0].Text
}
