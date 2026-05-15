package afcli

// githubTestBaseURL, when non-empty, overrides the default GitHub API base URL
// used by newGitHubClient(). Only used in tests — set via setGitHubTestBaseURL.
var githubTestBaseURL string

// setGitHubTestBaseURL overrides the GitHub client's base URL for tests.
// Call with "" to restore the default.
func setGitHubTestBaseURL(url string) {
	githubTestBaseURL = url
}
