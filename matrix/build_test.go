package matrix

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

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
