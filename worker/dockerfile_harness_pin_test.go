package worker

import (
	"encoding/json"
	"os"
	"regexp"
	"testing"
)

// workerImageDockerfiles are the two image definitions that install the
// provider CLIs: the container worker image and the e2b sandbox template.
// Paths are relative to this package directory.
var workerImageDockerfiles = []struct {
	name string
	path string
}{
	{name: "worker/Dockerfile", path: "Dockerfile"},
	{name: "worker/e2b/e2b.Dockerfile", path: "e2b/e2b.Dockerfile"},
}

// realCodexFixturePackage is the npm manifest CI installs for the real Codex
// MCP integration job (.github/workflows/ci.yml "Real Codex direct MCP
// fixture"). Pinning the images to the same version means CI exercises the
// exact Codex release the images ship.
const realCodexFixturePackage = "../provider/harness/codex/testdata/real-mcp-codex-cli/package.json"

var (
	claudeCodePinRe = regexp.MustCompile(`@anthropic-ai/claude-code@([0-9]+(?:\.[0-9]+)+)`)
	codexPinRe      = regexp.MustCompile(`@openai/codex@([0-9]+(?:\.[0-9]+)+)`)
)

// dockerfilePin returns the single version re captures in the Dockerfile at
// path. Zero or several pins fail the test: an image that installs a CLI
// twice, or not at all, has no well-defined version to compare.
func dockerfilePin(t *testing.T, name, path string, re *regexp.Regexp) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	matches := re.FindAllStringSubmatch(string(raw), -1)
	if len(matches) != 1 {
		t.Fatalf("%s: found %d pins matching %s, want exactly 1", name, len(matches), re)
	}
	return matches[0][1]
}

// TestHarnessCLIPins_MatchAcrossWorkerImages guards the Claude Code and Codex
// npm pins in worker/Dockerfile and worker/e2b/e2b.Dockerfile against
// drifting apart. A bump that lands in only one image leaves the other
// substrate running a CLI the model API may already reject.
func TestHarnessCLIPins_MatchAcrossWorkerImages(t *testing.T) {
	cases := []struct {
		cli string
		re  *regexp.Regexp
	}{
		{cli: "@anthropic-ai/claude-code", re: claudeCodePinRe},
		{cli: "@openai/codex", re: codexPinRe},
	}

	for _, tc := range cases {
		t.Run(tc.cli, func(t *testing.T) {
			first := workerImageDockerfiles[0]
			want := dockerfilePin(t, first.name, first.path, tc.re)
			for _, df := range workerImageDockerfiles[1:] {
				if got := dockerfilePin(t, df.name, df.path, tc.re); got != want {
					t.Errorf("%s pins %s at %s, %s pins %s; bump both images together",
						df.name, tc.cli, got, first.name, want)
				}
			}
		})
	}
}

// TestCodexPin_MatchesRealMCPFixture guards the images' Codex pin against
// drifting from the version CI's real Codex MCP integration job installs.
// Without it an image can ship a Codex release no CI job has exercised.
func TestCodexPin_MatchesRealMCPFixture(t *testing.T) {
	raw, err := os.ReadFile(realCodexFixturePackage)
	if err != nil {
		t.Fatalf("read real Codex fixture manifest: %v", err)
	}
	var manifest struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("parse real Codex fixture manifest: %v", err)
	}
	want := manifest.Dependencies["@openai/codex"]
	if want == "" {
		t.Fatalf("real Codex fixture manifest has no @openai/codex dependency")
	}

	for _, df := range workerImageDockerfiles {
		t.Run(df.name, func(t *testing.T) {
			if got := dockerfilePin(t, df.name, df.path, codexPinRe); got != want {
				t.Errorf("%s pins @openai/codex at %s, want %s (the real Codex MCP fixture version)",
					df.name, got, want)
			}
		})
	}
}
