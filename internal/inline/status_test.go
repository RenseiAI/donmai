package inline

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/internal/api"
)

func TestFormatStatusLine(t *testing.T) {
	tests := []struct {
		name  string
		stats *api.StatsResponse
		want  string
	}{
		{
			name:  "zero values",
			stats: &api.StatsResponse{},
			want:  "0 workers | 0 agents | 0 queued | 0 completed | $0.00 today",
		},
		{
			name: "typical values",
			stats: &api.StatsResponse{
				WorkersOnline:  3,
				AgentsWorking:  5,
				QueueDepth:     2,
				CompletedToday: 10,
				TotalCostToday: 35.36,
			},
			want: "3 workers | 5 agents | 2 queued | 10 completed | $35.36 today",
		},
		{
			name: "large numbers",
			stats: &api.StatsResponse{
				WorkersOnline:  999999,
				AgentsWorking:  999999,
				QueueDepth:     999999,
				CompletedToday: 999999,
				TotalCostToday: 123456.78,
			},
			want: "999999 workers | 999999 agents | 999999 queued | 999999 completed | $123456.78 today",
		},
		{
			name: "cost rounds down at 0.1",
			stats: &api.StatsResponse{
				TotalCostToday: 0.1,
			},
			want: "0 workers | 0 agents | 0 queued | 0 completed | $0.10 today",
		},
		{
			name: "cost half-even rounds 0.125 to 0.12",
			stats: &api.StatsResponse{
				TotalCostToday: 0.125,
			},
			// Go's fmt package uses "round half to even" for %.2f
			// 0.125 is not exactly representable in float64, it's closer to
			// 0.12499999... which rounds down to 0.12.
			want: "0 workers | 0 agents | 0 queued | 0 completed | $0.12 today",
		},
		{
			name: "cost rounds up 99.999 to 100.00",
			stats: &api.StatsResponse{
				TotalCostToday: 99.999,
			},
			want: "0 workers | 0 agents | 0 queued | 0 completed | $100.00 today",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatStatusLine(tt.stats)
			if got != tt.want {
				t.Errorf("FormatStatusLine() =\n\t%q\nwant\n\t%q", got, tt.want)
			}
		})
	}
}

// errDataSource is a DataSource that returns a fixed error from GetStats.
// All other methods panic — they must not be called by PrintStatus.
type errDataSource struct {
	api.DataSource
	err error
}

func (e *errDataSource) GetStats() (*api.StatsResponse, error) {
	return nil, e.err
}

func TestPrintStatus(t *testing.T) {
	// Helper to swap DataWriter to a temp file for the duration of a subtest.
	swapDataWriter := func(t *testing.T, name string) *os.File {
		t.Helper()
		path := filepath.Join(t.TempDir(), name)
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			t.Fatalf("create temp file: %v", err)
		}
		orig := DataWriter
		DataWriter = f
		t.Cleanup(func() {
			DataWriter = orig
			_ = f.Close()
		})
		return f
	}

	t.Run("writes formatted line to DataWriter", func(t *testing.T) {
		f := swapDataWriter(t, "status.txt")

		if err := PrintStatus(api.NewMockClient()); err != nil {
			t.Fatalf("PrintStatus: %v", err)
		}
		if err := f.Sync(); err != nil {
			t.Fatalf("sync: %v", err)
		}
		b, err := os.ReadFile(f.Name()) // #nosec G304 -- test-scoped temp path
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		got := string(b)
		if !strings.Contains(got, "workers |") {
			t.Errorf("stdout missing 'workers |' substring; got:\n%s", got)
		}
		if !strings.HasSuffix(got, "\n") {
			t.Errorf("DataLn output should end with newline; got: %q", got)
		}
	})

	t.Run("propagates DataSource error", func(t *testing.T) {
		_ = swapDataWriter(t, "status-err.txt")
		sentinel := errors.New("boom")
		err := PrintStatus(&errDataSource{err: sentinel})
		if !errors.Is(err, sentinel) {
			t.Errorf("PrintStatus() err = %v, want %v", err, sentinel)
		}
	})
}
