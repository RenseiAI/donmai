package app

import "github.com/RenseiAI/agentfactory-tui/afclient"

// Context is shared by pointer across all views.
type Context struct {
	DataSource afclient.DataSource
	Width      int
	Height     int
	BaseURL    string
	UseMock    bool
}
