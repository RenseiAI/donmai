package matrix

import (
	"reflect"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestBuildHarnessesRejectsDuplicateCanonicalID(t *testing.T) {
	manifest := agent.HarnessManifest{
		Name: agent.HarnessCodex, HumanLabel: "Codex", Family: agent.FamilyHarness,
		ContractABI: "harness/v2",
	}
	_, _, err := buildHarnessesFrom([]HarnessHarvest{
		{Name: agent.HarnessCodex, Manifest: func() agent.HarnessManifest { return manifest }},
		{Name: agent.HarnessCodex, Manifest: func() agent.HarnessManifest { return manifest }},
	})
	if err == nil || !strings.Contains(err.Error(), `duplicate harness id "codex"`) {
		t.Fatalf("buildHarnessesFrom() error = %v, want duplicate canonical id denial", err)
	}
}

func TestConcreteInBoxHarnessesDoNotUnionCapabilities(t *testing.T) {
	_, byName, err := buildHarnesses()
	if err != nil {
		t.Fatalf("buildHarnesses(): %v", err)
	}
	gemini, ok := byName[agent.HarnessGeminiDirect]
	if !ok {
		t.Fatal("gemini-direct harness missing")
	}
	ollama, ok := byName[agent.HarnessOllama]
	if !ok {
		t.Fatal("ollama harness missing")
	}

	if !reflect.DeepEqual(gemini.Drives, []agent.WireProtocol{agent.ProtoGeminiGenerate}) ||
		!reflect.DeepEqual(gemini.DrivesHosts, []agent.ServingHost{agent.HostDirect, agent.HostVertex}) ||
		gemini.Caps.StreamingTransport != "sse" || !gemini.Caps.AcceptsMcpServerSpec {
		t.Fatalf("gemini-direct capability row is not concrete: %+v", gemini)
	}
	if !reflect.DeepEqual(ollama.Drives, []agent.WireProtocol{agent.ProtoOllama}) ||
		!reflect.DeepEqual(ollama.DrivesHosts, []agent.ServingHost{agent.HostLocal}) ||
		ollama.Caps.StreamingTransport != "ndjson" || ollama.Caps.AcceptsMcpServerSpec {
		t.Fatalf("ollama capability row is not concrete: %+v", ollama)
	}
	if len(gemini.PromptDelivery) != 1 || !strings.HasPrefix(gemini.PromptDelivery[0].ID, "gemini-direct/") {
		t.Fatalf("gemini-direct prompt profiles = %+v", gemini.PromptDelivery)
	}
	if len(ollama.PromptDelivery) != 1 || !strings.HasPrefix(ollama.PromptDelivery[0].ID, "ollama/") {
		t.Fatalf("ollama prompt profiles = %+v", ollama.PromptDelivery)
	}
}

func TestConcreteInBoxCellsPreserveEvidenceLevels(t *testing.T) {
	built, err := Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	want := map[CellKey]struct {
		stability string
		smoked    bool
	}{
		{Harness: agent.HarnessGeminiDirect, Endpoint: agent.CompanyGoogle, Host: agent.HostDirect}: {stability: "stable", smoked: true},
		{Harness: agent.HarnessGeminiDirect, Endpoint: agent.CompanyGoogle, Host: agent.HostVertex}: {stability: "beta", smoked: false},
		{Harness: agent.HarnessOllama, Endpoint: agent.CompanyLocal, Host: agent.HostLocal}:         {stability: "beta", smoked: false},
	}
	for _, cell := range built.Matrix.Cells {
		expected, ok := want[cell.Key()]
		if !ok {
			continue
		}
		if cell.Stability != expected.stability || cell.Smoked != expected.smoked {
			t.Errorf("cell %+v evidence = stability %q smoked %v; want %q/%v", cell.Key(), cell.Stability, cell.Smoked, expected.stability, expected.smoked)
		}
		delete(want, cell.Key())
	}
	if len(want) != 0 {
		t.Fatalf("missing concrete in-box cells: %+v", want)
	}
}

func TestBuildCellsRejectsHostOutsideHarnessDrivesHosts(t *testing.T) {
	harnesses, harnessByName, err := buildHarnesses()
	if err != nil {
		t.Fatalf("buildHarnesses(): %v", err)
	}
	if len(harnesses) == 0 {
		t.Fatal("buildHarnesses(): no harnesses")
	}
	_, endpointByCompany := buildEndpoints()

	tests := []struct {
		name string
		cell HarnessEndpointCell
		want string
	}{
		{
			name: "amp cannot use Anthropic OAuth host",
			cell: HarnessEndpointCell{
				Harness:   agent.HarnessAmp,
				Endpoint:  agent.CompanyAnthropic,
				Host:      agent.HostOAuthCLI,
				Protocol:  agent.ProtoAnthropicMessages,
				AuthModes: []agent.AuthMode{agent.AuthHostSession},
			},
			want: `host "oauth-cli" not in harness "amp" drivesHosts [direct]`,
		},
		{
			name: "codex cannot use OpenAI gateway host",
			cell: HarnessEndpointCell{
				Harness:   agent.HarnessCodex,
				Endpoint:  agent.CompanyOpenAI,
				Host:      agent.HostGateway,
				Protocol:  agent.ProtoOpenAIChat,
				AuthModes: []agent.AuthMode{agent.AuthBYOK},
			},
			want: `host "gateway" not in harness "codex" drivesHosts [azure direct oauth-cli]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := buildCells([]HarnessEndpointCell{tt.cell}, harnessByName, endpointByCompany)
			if err == nil {
				t.Fatal("buildCells() error = nil, want undeclared host error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("buildCells() error = %q, want substring %q", err, tt.want)
			}
		})
	}
}
