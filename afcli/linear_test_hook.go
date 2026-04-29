package afcli

// linearTestBaseURL, when non-empty, overrides the default Linear API base URL
// used by newLinearClient(). Only used in tests — set via setTestBaseURL.
var linearTestBaseURL string

// setTestBaseURL overrides the Linear client's base URL for tests.
// Call with "" to restore the default.
func setTestBaseURL(url string) {
	linearTestBaseURL = url
}
