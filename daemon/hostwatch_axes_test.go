package daemon

import (
	"encoding/json"
	"testing"

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
