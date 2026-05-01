package afcli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// TestFormatWorkerStat covers REN-1446's worker-id row formatting.
func TestFormatWorkerStat(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   *afclient.DaemonStatsResponse
		want string
	}{
		{"nil response", nil, "(unregistered)"},
		{"empty worker", &afclient.DaemonStatsResponse{}, "(unregistered)"},
		{"real worker", &afclient.DaemonStatsResponse{WorkerID: "wkr_abc"}, "wkr_abc"},
		{"stub worker", &afclient.DaemonStatsResponse{WorkerID: "worker-host-stub"}, "worker-host-stub (stub)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := formatWorkerStat(c.in); got != c.want {
				t.Errorf("formatWorkerStat = %q, want %q", got, c.want)
			}
		})
	}
}

// TestFormatRegistrationStat covers REN-1446's registration row formatting.
func TestFormatRegistrationStat(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   *afclient.DaemonStatsResponse
		want string
	}{
		{
			"nil",
			nil,
			"(unknown)",
		},
		{
			"no registration field",
			&afclient.DaemonStatsResponse{},
			"(unknown)",
		},
		{
			"running with heartbeat + poll",
			&afclient.DaemonStatsResponse{Registration: &afclient.DaemonRegistrationStats{
				Status:           "idle",
				HeartbeatRunning: true,
				PollRunning:      true,
				LastHeartbeatAt:  "2026-05-01T12:00:00Z",
			}},
			"idle · heartbeat=running · poll=running · last-heartbeat=2026-05-01T12:00:00Z",
		},
		{
			"heartbeat only",
			&afclient.DaemonStatsResponse{Registration: &afclient.DaemonRegistrationStats{
				Status:           "idle",
				HeartbeatRunning: true,
			}},
			"idle · heartbeat=running · poll=stopped",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := formatRegistrationStat(c.in); got != c.want {
				t.Errorf("formatRegistrationStat = %q, want %q", got, c.want)
			}
		})
	}
}

// TestFormatAllowedProjectsStat covers REN-1446's allowed-projects row.
func TestFormatAllowedProjectsStat(t *testing.T) {
	t.Parallel()
	zero := formatAllowedProjectsStat(&afclient.DaemonStatsResponse{})
	if !strings.HasPrefix(zero, "0") {
		t.Errorf("expected '0...' for empty list, got %q", zero)
	}
	one := formatAllowedProjectsStat(&afclient.DaemonStatsResponse{
		AllowedProjects: []string{"github.com/foo/bar"},
	})
	if one != "1 — github.com/foo/bar" {
		t.Errorf("got %q", one)
	}
	many := formatAllowedProjectsStat(&afclient.DaemonStatsResponse{
		AllowedProjects: []string{"a", "b", "c", "d", "e", "f", "g", "h"},
	})
	if !strings.HasPrefix(many, "8 — a, b, c, d, e, f") {
		t.Errorf("missing first 6 with truncation marker, got %q", many)
	}
	if !strings.Contains(many, "+2 more") {
		t.Errorf("missing truncation marker, got %q", many)
	}
}

// TestWriteDaemonStatsTable_IncludesNewRows confirms that the rendered table
// surfaces all three new rows for REN-1446.
func TestWriteDaemonStatsTable_IncludesNewRows(t *testing.T) {
	t.Parallel()
	r := &afclient.DaemonStatsResponse{
		Capacity: afclient.MachineCapacity{
			MaxConcurrentSessions: 4,
			MaxVCpuPerSession:     2,
			MaxMemoryMbPerSession: 4096,
			ReservedVCpu:          2,
			ReservedMemoryMb:      4096,
		},
		ActiveSessions:  1,
		QueueDepth:      0,
		Timestamp:       "2026-05-01T12:00:00Z",
		WorkerID:        "wkr_abc",
		Registration:    &afclient.DaemonRegistrationStats{Status: "idle", HeartbeatRunning: true, PollRunning: true},
		AllowedProjects: []string{"github.com/foo/bar"},
	}
	buf := &bytes.Buffer{}
	if err := writeDaemonStatsTable(buf, r); err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, want := range []string{
		"Worker:",
		"wkr_abc",
		"Registration:",
		"heartbeat=running",
		"poll=running",
		"Allowed projects:",
		"github.com/foo/bar",
		"Active sessions:",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("table missing %q:\n%s", want, buf.String())
		}
	}
}
