package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/daemon/seatbudget"
	"github.com/RenseiAI/donmai/sessionshim"
)

func axesPollFixture(t *testing.T) PollWorkItem {
	t.Helper()
	cell := daemonExecutionCell()
	cell.Model.Author = "meta"
	cell.Endpoint.Operator = "proxy-operator"
	cell.Endpoint.Protocol = "openai-chat"
	return PollWorkItem{
		SessionID: "axes", EffectiveCell: rawJSON(t, cell),
		ResolvedProfile: &SessionResolvedProfile{
			Harness: "pi", Model: "misleading-vendor-model", Company: "openai",
			Endpoint: &SessionEndpointBinding{
				Company: "openai", Model: cell.Model.ID,
				EndpointID: cell.Endpoint.ID, ModelAuthor: "meta", EndpointOperator: "proxy-operator", Protocol: "openai-chat",
			},
		},
	}
}

func TestHostwatchAxesPollProjection(t *testing.T) {
	for _, name := range []string{"explicit cell", "binding only", "local profile without scalar company", "missing axes", "malformed cell", "conflicting author", "conflicting protocol", "conflicting company", "URL axis excluded"} {
		t.Run(name, func(t *testing.T) {
			item := axesPollFixture(t)
			wantAuthor, wantOperator, wantProtocol, wantSurface := "meta", "proxy-operator", "openai-chat", "openai"
			switch name {
			case "binding only":
				item.EffectiveCell = nil
			case "local profile without scalar company":
				item.ResolvedProfile.Company = ""
			case "missing axes":
				item.EffectiveCell = nil
				item.ResolvedProfile.Endpoint = nil
				wantAuthor, wantOperator, wantProtocol = "", "", ""
			case "malformed cell":
				item.EffectiveCell = json.RawMessage(`{"model":{"author":"guessed"}}`)
				wantAuthor, wantOperator, wantProtocol, wantSurface = "", "", "", "unknown"
			case "conflicting author":
				item.ResolvedProfile.Endpoint.ModelAuthor = "other"
				wantAuthor, wantOperator, wantProtocol, wantSurface = "", "", "", "unknown"
			case "conflicting protocol":
				item.ResolvedProfile.Endpoint.Protocol = "openai-responses"
				wantAuthor, wantOperator, wantProtocol, wantSurface = "", "", "", "unknown"
			case "conflicting company":
				item.ResolvedProfile.Company = "other"
				wantAuthor, wantOperator, wantProtocol, wantSurface = "", "", "", "unknown"
			case "URL axis excluded":
				item.EffectiveCell = nil
				item.ResolvedProfile.Endpoint.ModelAuthor = "https://example.test/private"
				wantAuthor = ""
			}
			company := item.ResolvedProfile.Company
			spec := PollItemToSessionSpec(item, nil)
			if spec.ModelAuthor != wantAuthor || spec.EndpointOperator != wantOperator || spec.Protocol != wantProtocol || spec.EndpointSurface != wantSurface {
				t.Fatalf("display axes=%q/%q/%q/%q want=%q/%q/%q/%q", spec.ModelAuthor, spec.EndpointOperator, spec.Protocol, spec.EndpointSurface, wantAuthor, wantOperator, wantProtocol, wantSurface)
			}
			if spec.Company != company || spec.Model != item.ResolvedProfile.Model || spec.Harness != "pi" {
				t.Fatal("display projection changed existing routing/authority inputs")
			}
		})
	}
}

func TestHostwatchAxesDirectProducer(t *testing.T) {
	spawner := NewWorkerSpawner(SpawnerOptions{Projects: []ProjectConfig{{ID: "example", Repository: "github.com/example/project"}}, MaxConcurrentSessions: 1})
	ended := sessionEnds(spawner)
	item := axesPollFixture(t)
	item.Repository = "github.com/example/project"
	item.ResolvedProfile.Company = "" // Authentic local producer shape: company lives on the explicit binding.
	spec := PollItemToSessionSpec(item, nil)
	handle, err := spawner.AcceptWork(spec)
	if err != nil {
		t.Fatal(err)
	}
	if handle.ModelProvider != "openai" || handle.ModelAuthor != "meta" || handle.EndpointOperator != "proxy-operator" || handle.Protocol != "openai-chat" {
		t.Fatalf("actual direct handle lost explicit display axes: %+v", handle)
	}
	waitSessionEnd(t, ended)
	waitForActiveCount(t, spawner, 0)
}

// TestHostwatchAxesShimHandleSeatBudget calls the actual shim handle
// publication function (trackLaunchedShim, the F1 report site) with a seat
// budget on the spawner and pins that the shim handle carries the seat's
// OWN launched facts — the launch record startShimProcess writes beside the
// discovery record — and never the daemon's current configuration. The
// spawner's share is deliberately different so a current-config report
// cannot hide. A shim seat that ran under its launched limits while its
// handle claimed the current share would be the false report this rule
// exists to prevent.
func TestHostwatchAxesShimHandleSeatBudget(t *testing.T) {
	registry, err := sessionshim.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	daemon := &Daemon{shims: newSessionShimState()}
	daemon.shims.registry = registry
	// The live scope is not readable here, so the launch record stands in.
	daemon.shimScope = shimScopeProbe{readLimits: func(string, bool) (seatbudget.SeatLimits, bool) {
		return seatbudget.SeatLimits{}, false
	}}
	daemon.spawner = NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "example", Repository: "github.com/example/project"}},
		MaxConcurrentSessions: 1,
		// Deliberately different from the launched facts below: the
		// handle must report the seat's own numbers, not this share.
		SeatBudget: SeatBudget{CPUs: 8, Mode: "best-effort"},
	})
	controller := &sessionshim.Controller{}
	item := axesPollFixture(t)
	item.ResolvedProfile.Company = ""
	spec := PollItemToSessionSpec(item, nil)
	id := sessionshim.Identity{OrgID: "example-org", SessionID: spec.SessionID}
	scope := seatbudget.ScopeNameForIncarnation(id.OrgID, id.SessionID, 1)
	if err := registry.PutSeatLaunch(sessionshim.NewSeatLaunch(id, 1, scope, sessionshim.SeatModeEnforced, 2, 1024, 0, time.Now())); err != nil {
		t.Fatalf("PutSeatLaunch: %v", err)
	}
	evidence := SessionShimAdoptionEvidence{Identity: id, ProcessEpoch: 1}
	handle := daemon.trackLaunchedShim(controller, spec, ProjectConfig{ID: "example"}, "", "", evidence, SessionShimAdoptionReceipt{}, false)
	if handle.SeatBudget == nil {
		t.Fatal("shim handle.SeatBudget is nil; want the seat posture")
	}
	if handle.SeatBudget.Mode != "enforced" || handle.SeatBudget.CPUs != 2 || handle.SeatBudget.MemoryMB != 1024 {
		t.Errorf("shim handle.SeatBudget = %+v; want the launched enforced 2 cpu / 1024MiB, not the current 8-cpu share", handle.SeatBudget)
	}
	if !strings.Contains(handle.SeatBudget.Detail, scope) {
		t.Errorf("shim handle detail = %q; want the owning scope named", handle.SeatBudget.Detail)
	}
}

// This calls the actual shim handle publication function without spawning or
// adopting a shim. It proves metadata projection, not authenticated adoption.
func TestHostwatchAxesShimHandleProjection(t *testing.T) {
	daemon := &Daemon{shims: newSessionShimState()}
	controller := &sessionshim.Controller{}
	item := axesPollFixture(t)
	item.ResolvedProfile.Company = ""
	spec := PollItemToSessionSpec(item, nil)
	handle := daemon.trackLaunchedShim(controller, spec, ProjectConfig{ID: "example"}, "", "", SessionShimAdoptionEvidence{}, SessionShimAdoptionReceipt{}, false)
	if handle.ModelProvider != "openai" || handle.ModelAuthor != "meta" || handle.EndpointOperator != "proxy-operator" || handle.Protocol != "openai-chat" {
		t.Fatalf("actual shim handle publication lost explicit display axes: %+v", handle)
	}
}
