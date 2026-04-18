package detail

import "github.com/RenseiAI/agentfactory-tui/afclient"

type detailDataMsg struct {
	detail *afclient.SessionDetailResponse
	err    error
}

type detailTickMsg struct{}

// Activity streaming messages
type activityMsg struct {
	activities []afclient.ActivityEvent
	cursor     *string
	err        error
}

type activityTickMsg struct{}

// Action messages
type stopAgentMsg struct {
	err error
}

type sendPromptMsg struct {
	text string
	err  error
}
