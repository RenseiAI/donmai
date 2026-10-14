package afcli

// examples_test.go — the `examples/` showcase workflows stay runnable from
// their READMEs alone on the shipping release.
//
// Each example directory carries a README (the runnable contract) plus a
// recorded run (`run.cast`, asciicast v2). This file pins three things the
// suite can check without a daemon, a harness, or network access:
//
//  1. Every command the READMEs ask a reader to type resolves on the real
//     command tree built exactly as the standalone binary builds it
//     (cmd/donmai/main.go: legacy worker/fleet, dashboard, A2A on).
//     A renamed flag or retired noun breaks the example before a reader
//     finds out.
//  2. Every recorded run is a well-formed asciicast v2 file whose replayed
//     output contains the example's commands in order — the cast the README
//     points at actually records the workflow it claims to.
//  3. The top-level README links to each example directory.
//
// The checks read the checked-in files from the repository root (located
// via the go.mod walk, the same seam internal/testregistration uses), so
// they run unchanged from any worktree.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/donmai/afclient"
)

// showcaseExamples is the fixed roster of showcase workflows. Adding a
// fourth example means adding its row here — the link, README-command,
// and cast checks below all key off this list.
var showcaseExamples = []struct {
	// dir is the examples/<dir> directory.
	dir string
	// commands are the `donmai ...` invocations the README asks a reader
	// to type. Each must resolve on the standalone tree (Find), so the
	// check catches renamed nouns/verbs/flags, not just typos.
	commands [][]string
	// castMarkers are strings the recorded run's replayed output must
	// contain in order. They mirror the commands above plus the
	// non-donmai steps (gh, echo, file contents) the workflow needs.
	castMarkers []string
}{
	{
		dir: "backlog-sweep",
		commands: [][]string{
			{"host", "setup"},
			{"host", "run"},
			{"github", "list-issues"},
			{"github", "add-labels"},
			{"host", "watch"},
			{"host", "workarea", "list"},
			{"host", "logs"},
		},
		castMarkers: []string{
			"donmai github list-issues",
			"donmai github add-labels",
			"donmai host run",
			"donmai host watch --all",
			"donmai host workarea list",
			"donmai host logs",
		},
	},
	{
		dir: "second-opinion",
		commands: [][]string{
			{"host", "provider", "list"},
			{"orchestrator"},
			{"host", "watch"},
		},
		castMarkers: []string{
			"donmai host provider list",
			"donmai orchestrator --single",
			"--dry-run",
			"donmai host watch --all",
			"gh pr list",
		},
	},
	{
		dir: "drift-check-pr",
		commands: [][]string{
			{"arch", "assess"},
			{"github", "create-comment"},
		},
		castMarkers: []string{
			"donmai arch assess",
			"--summary",
			"--require-diff",
			"donmai github create-comment",
			"drift.yml",
		},
	},
}

// showcaseRoot locates the repository root by walking up from the test's
// working directory until go.mod is found.
func showcaseRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod found walking up; test must run inside the repo")
		}
		dir = parent
	}
}

// standaloneExampleTree builds the command tree exactly as the standalone
// binary does (cmd/donmai/main.go): legacy worker/fleet, dashboard, and
// formal A2A on.
func standaloneExampleTree(t *testing.T) *cobra.Command {
	t.Helper()
	root := &cobra.Command{Use: "donmai"}
	RegisterCommands(root, Config{
		BinaryName:              "donmai",
		ClientFactory:           func() afclient.DataSource { return afclient.NewMockClient() },
		EnableDashboard:         true,
		EnableLegacyWorkerFleet: true,
		EnableA2AClient:         true,
	})
	if len(root.Commands()) == 0 {
		t.Fatal("RegisterCommands registered nothing — test fixture is stale")
	}
	return root
}

// TestShowcaseExampleCommandsResolve drives the production entry point —
// the real cobra tree — for every `donmai ...` invocation the example
// READMEs ask a reader to type. A command that no longer resolves (a
// renamed noun, a retired subcommand, a moved flag group) fails here,
// not in a reader's terminal.
func TestShowcaseExampleCommandsResolve(t *testing.T) {
	t.Parallel()

	root := standaloneExampleTree(t)
	for _, ex := range showcaseExamples {
		ex := ex
		t.Run(ex.dir, func(t *testing.T) {
			t.Parallel()
			for _, args := range ex.commands {
				cmd, flags, err := root.Find(args)
				if err != nil {
					t.Errorf("README command `donmai %s`: Find: %v", strings.Join(args, " "), err)
					continue
				}
				if cmd == nil || cmd.Name() != args[len(args)-1] {
					t.Errorf("README command `donmai %s` resolved to %v (leftover flags %v); want the %q leaf",
						strings.Join(args, " "), cmd, flags, args[len(args)-1])
				}
			}
		})
	}
}

// replayCastOutput concatenates the "o" event payloads of an asciicast v2
// file — what a player renders — skipping the header line.
func replayCastOutput(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // repo-owned fixture path
	if err != nil {
		t.Fatalf("read cast %s: %v", path, err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("cast %s has %d lines; want a header plus events", path, len(lines))
	}
	var header map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &header); err != nil {
		t.Fatalf("cast %s header is not JSON: %v", path, err)
	}
	if v, _ := header["version"].(float64); v != 2 {
		t.Fatalf("cast %s header version = %v, want 2", path, header["version"])
	}
	var out strings.Builder
	for i, ln := range lines[1:] {
		var ev []json.RawMessage
		if err := json.Unmarshal([]byte(ln), &ev); err != nil || len(ev) != 3 {
			t.Fatalf("cast %s line %d is not a 3-tuple event: %q", path, i+2, ln)
		}
		var code, chunk string
		if err := json.Unmarshal(ev[1], &code); err != nil {
			t.Fatalf("cast %s line %d: bad event code: %v", path, i+2, err)
		}
		if code != "o" {
			continue
		}
		if err := json.Unmarshal(ev[2], &chunk); err != nil {
			t.Fatalf("cast %s line %d: bad output chunk: %v", path, i+2, err)
		}
		out.WriteString(chunk)
	}
	return out.String()
}

// TestShowcaseRecordedRunsMatchReadmes replays each example's recorded run
// and requires the workflow's commands to appear in order. A cast that was
// recorded for a different workflow — or truncated to a header — fails
// here, which is the only thing standing between the README's "recorded
// run" link and a file that shows something else.
func TestShowcaseRecordedRunsMatchReadmes(t *testing.T) {
	t.Parallel()

	root := showcaseRoot(t)
	for _, ex := range showcaseExamples {
		ex := ex
		t.Run(ex.dir, func(t *testing.T) {
			t.Parallel()
			castPath := filepath.Join(root, "examples", ex.dir, "run.cast")
			if _, err := os.Stat(castPath); err != nil {
				t.Fatalf("recorded run missing: %s (README links it)", castPath)
			}
			replay := replayCastOutput(t, castPath)
			cursor := 0
			for _, marker := range ex.castMarkers {
				idx := strings.Index(replay[cursor:], marker)
				if idx < 0 {
					t.Errorf("recorded run %s: marker %q not found after offset %d\nreplay:\n%s",
						castPath, marker, cursor, replay)
					return
				}
				cursor += idx + len(marker)
			}
		})
	}
}

// TestShowcaseReadmeLinksExamples requires the top-level README to link to
// every showcase workflow directory — the acceptance criterion that the
// examples are reachable from the top.
func TestShowcaseReadmeLinksExamples(t *testing.T) {
	t.Parallel()

	root := showcaseRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "README.md")) //nolint:gosec // repo-owned path
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	body := string(data)
	for _, ex := range showcaseExamples {
		link := "examples/" + ex.dir
		if !strings.Contains(body, link) {
			t.Errorf("README.md does not link to %q; examples must be reachable from the top", link)
		}
		readmePath := filepath.Join(root, "examples", ex.dir, "README.md")
		if _, err := os.Stat(readmePath); err != nil {
			t.Errorf("example %s has no README.md: %v", ex.dir, err)
		}
	}
}
