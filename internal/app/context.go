package app

import "github.com/RenseiAI/donmai/afclient"

// Context is shared by pointer across all views.
type Context struct {
	DataSource afclient.DataSource
	Width      int
	Height     int
	BaseURL    string
	UseMock    bool
}
