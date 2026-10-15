package clijsonl

import (
	"bytes"
	_ "embed"

	"github.com/RenseiAI/donmai/agent"
)

//go:embed testdata/subagent_agent_call.jsonl
var nativeDelegationFixture []byte

// SubagentFixtureEvents executes the native-delegation fixture through the
// stateful stream-json adapter. Matrix generation uses the same fixture as
// the adapter's conformance tests, without probing a binary or using a network.
func SubagentFixtureEvents() []agent.Event {
	var mapper LineMapper
	var events []agent.Event
	for _, line := range bytes.Split(nativeDelegationFixture, []byte("\n")) {
		events = append(events, mapper.MapLine(line)...)
	}
	return events
}
