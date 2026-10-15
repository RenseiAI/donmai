package pi

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"

	"github.com/RenseiAI/donmai/agent"
)

//go:embed testdata/delegation.jsonl
var delegationFixture []byte

// SubagentFixtureEvents replays the delegation fixture through the production
// RPC decoder and event adapter. The fixture producer must supply the deliveries
// it actually loaded; replay alone does not prove extension loading or admission.
func SubagentFixtureEvents(loaded []agent.ExtensionDelivery) ([]agent.Event, error) {
	if err := agent.ValidateExtensionDeliveries(loaded); err != nil {
		return nil, fmt.Errorf("subagent fixture deliveries: %w", err)
	}
	rpc := newRPCClient(io.Discard, bytes.NewReader(delegationFixture))
	state := &mapperState{delegationTools: declaredDelegationTools(loaded)}
	var events []agent.Event
	for raw := range rpc.Events() {
		mapped, _ := mapEvent(raw, state)
		events = append(events, mapped...)
	}
	if err := rpc.CloseErr(); err != nil {
		return nil, fmt.Errorf("subagent fixture RPC: %w", err)
	}
	return events, nil
}
