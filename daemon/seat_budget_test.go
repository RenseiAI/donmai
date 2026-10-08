package daemon

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/daemon/seatbudget"
)

// TestServer_Status_SeatBudget drives the PRODUCTION status route
// (GET /api/daemon/status) and asserts the seat budget block: a daemon
// with no budget configured reports mode none, and one with a budget
// reports the posture with the values.
func TestServer_Status_SeatBudget(t *testing.T) {
	t.Run("off by default", func(t *testing.T) {
		_, srv, cleanup := mustStartDaemon(t)
		defer cleanup()
		var resp afclient.DaemonStatusResponse
		requireGet(t, srv.Addr(), "/api/daemon/status", &resp)
		if resp.SeatBudget == nil {
			t.Fatal("SeatBudget is nil; want mode none (additive, always present)")
		}
		if resp.SeatBudget.Mode != "none" {
			t.Errorf("SeatBudget.Mode = %q; want none", resp.SeatBudget.Mode)
		}
	})
	t.Run("configured budget reports posture and values", func(t *testing.T) {
		// The budget rides daemon.yaml: write it into the config the test
		// daemon loads, the same path an operator takes.
		_, srv, cleanup := mustStartDaemonWith(t, func(opts *Options) {
			cfg, err := LoadConfig(opts.ConfigPath)
			if err != nil {
				t.Fatalf("load config: %v", err)
			}
			cfg.Capacity.SeatBudget = SeatBudgetConfig{CPUs: 2, MemoryMB: 4096, Mode: "best-effort"}
			if err := WriteConfig(opts.ConfigPath, cfg); err != nil {
				t.Fatalf("write config: %v", err)
			}
		})
		defer cleanup()
		var resp afclient.DaemonStatusResponse
		requireGet(t, srv.Addr(), "/api/daemon/status", &resp)
		if resp.SeatBudget == nil {
			t.Fatal("SeatBudget is nil; want the configured share")
		}
		if resp.SeatBudget.Mode != "best-effort" {
			t.Errorf("SeatBudget.Mode = %q; want best-effort", resp.SeatBudget.Mode)
		}
		if resp.SeatBudget.CPUs != 2 || resp.SeatBudget.MemoryMB != 4096 {
			t.Errorf("SeatBudget = %+v; want 2 cpu 4096MB", resp.SeatBudget)
		}
		if resp.SeatBudget.Detail == "" {
			t.Error("SeatBudget.Detail is empty; want the human line")
		}
	})
}

// TestServer_Status_SeatBudget_CgroupfsReportsNone pins the honest-fallback
// rule on the PRODUCTION status route: an enforced budget on a host whose
// placement cannot confine the seat reports mode none with the backend
// named. This host (darwin CI, or any systemd-less Linux) is exactly that
// host whenever the placement probe is not systemd: the test asserts the
// downgrade shape — mode none plus a reason — without pinning which
// backend this particular machine lacks.
func TestServer_Status_SeatBudget_CgroupfsReportsNone(t *testing.T) {
	// A systemd-less Linux host runs an enforced seat unconfined: the
	// report must be mode none with the backend named. This host is
	// darwin, so the suite pins the downgrade through the report helper
	// with the systemd-less placement simulated by the cgroupfs branch:
	// sessionSeatBudgetReport is the per-spawn half of the same rule, and
	// this host's own placement is likewise not systemd.
	if seatbudget.HostPlacement() == seatbudget.PlacementSystemd {
		t.Skip("systemd host confines the seat; the none-downgrade needs a systemd-less host")
	}
	rep := sessionSeatBudgetReport(seatbudget.Budget{CPUs: 2, MemoryMB: 1024, Mode: seatbudget.ModeEnforced}, true, seatbudget.PlacementCgroupFS)
	if rep.Mode != "none" {
		t.Errorf("SeatBudget.Mode = %q; want none (no systemd backend on this host)", rep.Mode)
	}
	if rep.Detail == "" {
		t.Error("SeatBudget.Detail is empty; want the missing backend named")
	}
	if rep.CPUs != 0 || rep.MemoryMB != 0 {
		t.Errorf("SeatBudget = %+v; want no values on an unconfined seat", rep)
	}
	// And the status half: this host's effective posture for an enforced
	// budget is best-effort at most — never enforced without systemd.
	status := seatBudgetReport(seatbudget.Budget{CPUs: 2, MemoryMB: 1024, Mode: seatbudget.ModeEnforced})
	if seatbudget.HostPlacement() != seatbudget.PlacementSystemd && status.Mode == string(seatbudget.ModeEnforced) {
		t.Errorf("status mode = enforced on a systemd-less host; want the downgrade")
	}
}

// TestSeatBudgetDetail_SystemdlessEnforcedIsPreDowngradeOnly pins the F2
// contract: seatBudgetDetail's systemd-less enforced line (values plus the
// no-backend suffix) is the PRE-downgrade rendering seatBudgetReport
// consumes — no caller may publish it as a posture. The published status
// half for the same input is mode none with no values. A caller that
// rendered the detail line directly would claim an enforced budget for a
// seat that runs unconfined.
func TestSeatBudgetDetail_SystemdlessEnforcedIsPreDowngradeOnly(t *testing.T) {
	// Pin the systemd-less Linux downgrade on ANY host: an enforced seat
	// with the cgroupfs placement reports mode none with no values — the
	// published posture — while the raw detail line keeps the no-backend
	// suffix for the pre-downgrade rendering path. A caller that
	// published the detail line directly would claim an enforced budget
	// for a seat that runs unconfined.
	b := seatbudget.Budget{CPUs: 2, MemoryMB: 1024, Mode: seatbudget.ModeEnforced}
	if got := seatBudgetReportFor(b, seatbudget.PlacementCgroupFS, "linux"); got.Mode != string(seatbudget.ModeNone) || got.CPUs != 0 || got.MemoryMB != 0 {
		t.Fatalf("status report = %+v; want mode none with no values", got)
	}
	if got := seatBudgetReportFor(b, seatbudget.PlacementNone, "linux"); got.Mode != string(seatbudget.ModeNone) {
		t.Errorf("no-placement report = %+v; want mode none", got)
	}
	detail := seatBudgetDetailFor(b, "linux")
	if !strings.Contains(detail, "no enforcement backend") {
		t.Errorf("detail = %q; want the pre-downgrade line naming the missing backend", detail)
	}
	if got := seatBudgetReportFor(b, seatbudget.PlacementSystemd, "linux"); got.Mode != string(seatbudget.ModeEnforced) {
		t.Errorf("systemd report = %+v; want enforced", got)
	}
}

// TestPollItemToSessionDetail_SeatBudget pins the dispatch threading: the
// WithSeatBudget option stamps the share onto the SessionDetail the worker
// fetches, and omitting it leaves the field nil (budgeting off).
func TestPollItemToSessionDetail_SeatBudget(t *testing.T) {
	item := PollWorkItem{SessionID: "s1", Repository: "github.com/a/b"}
	projects := []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}}
	plain := PollItemToSessionDetail(item, projects, "http://127.0.0.1:1", "tok", "w1")
	if plain.SeatBudget != nil {
		t.Errorf("SeatBudget = %+v; want nil when no option is passed", plain.SeatBudget)
	}
	stamped := PollItemToSessionDetail(item, projects, "http://127.0.0.1:1", "tok", "w1",
		WithSeatBudget(&SessionSeatBudget{Mode: "best-effort", CPUs: 2, Detail: "caps"}))
	if stamped.SeatBudget == nil || stamped.SeatBudget.Mode != "best-effort" || stamped.SeatBudget.CPUs != 2 {
		t.Errorf("SeatBudget = %+v; want the stamped share", stamped.SeatBudget)
	}
	// The option copies: mutating the caller's struct must not move the detail.
	mut := &SessionSeatBudget{Mode: "enforced", CPUs: 4}
	stamped2 := PollItemToSessionDetail(item, projects, "http://127.0.0.1:1", "tok", "w1", WithSeatBudget(mut))
	mut.CPUs = 99
	if stamped2.SeatBudget.CPUs != 4 {
		t.Errorf("SeatBudget.CPUs = %d; want defensive copy 4", stamped2.SeatBudget.CPUs)
	}
}

// TestLoadConfig_SeatBudgetRoundTrip pins the config block: seatBudget
// survives a write/read round trip with its values intact.
func TestLoadConfig_SeatBudgetRoundTrip(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Machine.ID = "budget-host"
	cfg.Orchestrator.URL = "http://127.0.0.1:1"
	cfg.Capacity.SeatBudget = SeatBudgetConfig{CPUs: 2, MemoryMB: 4096, IOWeight: 200, Mode: "best-effort"}
	path := t.TempDir() + "/daemon.yaml"
	if err := WriteConfig(path, cfg); err != nil {
		t.Fatalf("write: %v", err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Capacity.SeatBudget != cfg.Capacity.SeatBudget {
		t.Errorf("SeatBudget = %+v; want %+v", loaded.Capacity.SeatBudget, cfg.Capacity.SeatBudget)
	}
}
