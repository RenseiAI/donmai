package app

import "github.com/RenseiAI/agentfactory-tui/internal/api"

// Context is shared by pointer across all views.
type Context struct {
	DataSource api.DataSource
	Width      int
	Height     int
	BaseURL    string
	UseMock    bool
}
