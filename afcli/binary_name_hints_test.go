package afcli

// binary_name_hints_test.go guards two sides of one defect class: help text
// that tells the user to run a command they cannot run.
//
// afcli is designed to be embedded downstream under a DIFFERENT binary name
// via afcli.RegisterCommands(root, Config{BinaryName: "<embedder>", ...}), and
// every command factory in this package receives that name (see binaryName()
// in helpers.go) for exactly this purpose. A leaf whose Example block is a
// static literal naming "donmai" instead then renders --help output telling a
// user of the embedding binary to run a binary they do not have: either
// "donmai: command not found", or worse, a separate install of the standalone
// binary running against the wrong context. That shipped in every leaf of
// `github` (12) and in `logs analyze`.
//
// WHY THIS IS DERIVED, NOT LISTED. The first version of this file asserted the
// two fixed trees by name — newGitHubCmd's leaves and newLogsCmd's analyze.
// That scope was hand-picked from what the sweep happened to have looked at,
// so it could only ever re-check the commands somebody already knew about; the
// thirteenth offender, or the next one somebody adds, is invisible to it by
// construction. The check below derives its scope instead: it builds the WHOLE
// registered tree under a name that is not "donmai" and reads every string a
// user can see, so a command added six months from now is covered without its
// author knowing this test exists.
//
// Two assertions over that tree:
//
//	1. no help text invokes the upstream binary by name, and
//	2. every command the help text DOES name actually resolves.
//
// (2) is the same guard from the opposite direction: a hint naming a command
// that was renamed or removed is as broken as one naming a foreign binary.
//
// SCOPE. This reads the rendered command tree, not the package source. Source
// strings outside the cobra tree legitimately name the standalone binary —
// state-dir paths (~/.config/donmai/..., owned by the separate
// runtime/statehome seam), package doc comments, and log-scope labels are all
// a different concern from "this text tells the user to run a command".
// Widening to a raw "no donmai substring anywhere" assertion produces dozens
// of those false positives, and a test that cries wolf gets disabled.
//
// The resolution gate is what keeps assertion (1) usable without that
// widening: prose like "the donmai binary (--donmai-bin / on PATH)" in `eval
// codeintel` mentions the upstream binary legitimately, and is not reported,
// because "donmai binary" does not resolve to a command. Only text that names
// a REAL command with the wrong binary in front of it is a defect.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/donmai/afclient"
)

// embedderBinaryName stands in for a downstream embedder's binary name.
// Deliberately does NOT contain the substring "donmai" (unlike "not-donmai"),
// so a plain strings.Contains(text, "donmai") cannot false-positive on it.
const embedderBinaryName = "acme-cli"

// upstreamBinaryName is the name this package must never hardcode into help
// text that an embedder will render.
const upstreamBinaryName = "donmai"

// minHintInvocations is the anti-vacuity floor. If tree construction or
// extraction breaks, both assertions below would pass over an empty set and
// this file would silently stop guarding anything — the failure mode that made
// the hand-picked version worth replacing. The tree carries ~80 invocations;
// the floor sits well under that so ordinary help-text edits never trip it.
const minHintInvocations = 40

// ─────────────────────────────────────────────────────────────────────────────
// The embedded tree
// ─────────────────────────────────────────────────────────────────────────────

// embeddedTree builds the full command tree exactly as a downstream embedder
// would, with every optional surface enabled so nothing is out of scope.
func embeddedTree(t *testing.T) *cobra.Command {
	t.Helper()
	root := &cobra.Command{Use: embedderBinaryName}
	RegisterCommands(root, Config{
		BinaryName:              embedderBinaryName,
		ClientFactory:           func() afclient.DataSource { return nil },
		EnableDashboard:         true,
		EnableLegacyWorkerFleet: true,
	})
	if len(root.Commands()) == 0 {
		t.Fatal("RegisterCommands registered nothing — test fixture is stale")
	}
	return root
}

// hintText is one user-visible string plus where it came from.
type hintText struct {
	source string
	body   string
}

// helpTexts walks the tree and yields every field whose contents reach a
// user's terminal, hidden and deprecated commands included — a hint in a
// hidden command's help is still a hint.
func helpTexts(root *cobra.Command) []hintText {
	var out []hintText
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		path := c.CommandPath()
		for _, f := range []struct{ name, body string }{
			{"Short", c.Short},
			{"Long", c.Long},
			{"Example", c.Example},
			{"Deprecated", c.Deprecated},
		} {
			if strings.TrimSpace(f.body) != "" {
				out = append(out, hintText{source: path + "." + f.name, body: f.body})
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// Extraction
// ─────────────────────────────────────────────────────────────────────────────

// hintInvocation is one "the user is told to run this" occurrence.
type hintInvocation struct {
	source string
	path   []string
}

// hintHistoricalCues mark text that names a command on purpose BECAUSE it is
// gone. Deliberately narrow, and scoped to the invocation's own sentence.
var hintHistoricalCues = []string{
	"formerly", "previously", "renamed", "replaced",
	"removed", "deprecated", "used to", "no longer",
}

// hintStepMarker matches the token that introduces a command: a shell prompt
// or bullet ("$", ">", "1.", "2)"), or a separator joining two commands in one
// hint ("acme-cli auth add → acme-cli host install").
var hintStepMarker = regexp.MustCompile(`^(\$|>|#|\*|-|\d+[.)]|→|->|&&|\|\||;|\||then)$`)

// hintCommandWord strips the decoration help text wraps around a word and
// reports whether what is left can be a cobra command name. Placeholders
// (`<id>`, `[name]`, `...`), flags, paths, URLs and prose with capitals or
// punctuation all fail here, which is what terminates a run.
func hintCommandWord(field string) (string, bool) {
	w := strings.TrimLeft(field, "`'\"(")
	w = strings.TrimRight(w, "`'\"),.;:!?")
	if w == "" {
		return "", false
	}
	for i, r := range w {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		case r == '-' && i > 0:
		default:
			return "", false
		}
	}
	return w, true
}

func hintIsWordByte(b byte) bool {
	return b == '-' || b == '_' ||
		(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// hintSentenceLookbehind returns the text before offset, cut back to the start
// of the current sentence, so a historical cue in a PREVIOUS sentence cannot
// silence a live hint.
func hintSentenceLookbehind(body string, offset int) string {
	start := offset - 240
	if start < 0 {
		start = 0
	}
	seg := body[start:offset]
	for i := len(seg) - 2; i >= 0; i-- {
		if seg[i] == '.' && (seg[i+1] == ' ' || seg[i+1] == '\n' || seg[i+1] == '\t') {
			return seg[i+1:]
		}
	}
	return seg
}

func hintHasHistoricalCue(seg string) bool {
	lower := strings.ToLower(seg)
	for _, cue := range hintHistoricalCues {
		if strings.Contains(lower, cue) {
			return true
		}
	}
	return false
}

// extractHintInvocations finds every place `body` tells the user to run `bin`.
//
// An occurrence counts only in an INVOCATION CONTEXT — quoted or backticked,
// at the start of a line, inside a parenthetical it closes, after a shell or
// step marker, or terminated by a flag or placeholder. Everything else is
// prose ("the donmai binary the WITH arm uses", "~/.config/donmai/...") and is
// dropped. That distinction is the whole reason this check can be run over
// every command instead of a hand-picked pair of them.
func extractHintInvocations(source, body, bin string) []hintInvocation {
	var out []hintInvocation
	for search := 0; search+len(bin) <= len(body); {
		rel := strings.Index(body[search:], bin)
		if rel < 0 {
			break
		}
		pos := search + rel
		search = pos + len(bin)

		// Whole word on the left.
		if pos > 0 && hintIsWordByte(body[pos-1]) {
			continue
		}
		// An invocation is the binary name followed by a space: rejects
		// "donmai-daemon", "donmai.dev", "~/.config/donmai/x.yaml".
		after := pos + len(bin)
		if after >= len(body) || (body[after] != ' ' && body[after] != '\t') {
			continue
		}

		lineStart := strings.LastIndexByte(body[:pos], '\n') + 1
		before := body[lineStart:pos]

		reason := ""
		var prevChar byte
		if pos > 0 {
			prevChar = body[pos-1]
		}
		switch {
		case pos > 0 && strings.IndexByte("`'\"$", prevChar) >= 0:
			reason = "quoted"
		case strings.TrimSpace(before) == "":
			reason = "line-start"
		}
		if reason == "" {
			if fields := strings.Fields(before); len(fields) > 0 && hintStepMarker.MatchString(fields[len(fields)-1]) {
				reason = "step"
			}
		}

		// Consume the command words. A run never wraps a line.
		rest := body[after:]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			rest = rest[:nl]
		}
		fields := strings.Fields(rest)
		var path []string
		consumed := 0
		for _, field := range fields {
			w, ok := hintCommandWord(field)
			if !ok {
				break
			}
			path = append(path, w)
			consumed++
			if strings.ContainsAny(field, "`'\"),.;:!?") {
				break
			}
		}
		if len(path) == 0 {
			continue
		}

		// "(donmai host install)" is an invocation; "(donmai stripped from
		// PATH, the guard)" is prose. The difference is whether the paren
		// closes on the run.
		if reason == "" && prevChar == '(' && consumed > 0 {
			last := fields[consumed-1]
			closed := strings.Contains(last, ")")
			if !closed && consumed < len(fields) {
				closed = strings.HasPrefix(fields[consumed], ")")
			}
			if closed {
				reason = "parenthetical"
			}
		}
		// A trailing flag or placeholder proves intent.
		if reason == "" && consumed < len(fields) {
			next := fields[consumed]
			if strings.HasPrefix(next, "-") || strings.HasPrefix(next, "<") || strings.HasPrefix(next, "[") {
				reason = "flagged"
			}
		}
		if reason == "" {
			continue // prose
		}
		if hintHasHistoricalCue(hintSentenceLookbehind(body, pos)) {
			continue // deliberately names a command that is gone
		}

		out = append(out, hintInvocation{source: source, path: path})
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// Resolution
// ─────────────────────────────────────────────────────────────────────────────

// classifyHintPath resolves a path against the tree and returns a non-empty
// description when the hint names something unreachable.
//
// cobra's Find does not simply error on an unknown subcommand — it returns the
// deepest command it DID match plus the unconsumed words, so "host frobnicate"
// comes back as `host` with remaining ["frobnicate"]. Leftover words are only
// a defect when the command they were handed cannot take positional arguments,
// which is exactly the case for a group command with subcommands and no Run of
// its own. That structural fact is what separates "agent run <id>" (leftover
// positional, fine) from "agent frobnicate" (leftover subcommand, broken)
// without a hand-maintained list of which commands take arguments.
func classifyHintPath(root *cobra.Command, path []string) string {
	cmd, remaining, err := root.Find(path)
	if err != nil {
		if i := strings.IndexByte(err.Error(), '\n'); i >= 0 {
			return err.Error()[:i]
		}
		return err.Error()
	}
	if len(remaining) == 0 {
		return ""
	}
	if cmd == root {
		return fmt.Sprintf("%q is not a registered top-level command", remaining[0])
	}
	if cmd.HasSubCommands() && !cmd.Runnable() {
		return fmt.Sprintf("%q has no subcommand %q (it is a group command and takes no arguments of its own)",
			cmd.CommandPath(), remaining[0])
	}
	return ""
}

// ─────────────────────────────────────────────────────────────────────────────
// The guards
// ─────────────────────────────────────────────────────────────────────────────

// TestEmbeddedHelpNeverInvokesUpstreamBinary is assertion (1): no help text
// rendered for an embedder may tell the user to run the upstream binary.
func TestEmbeddedHelpNeverInvokesUpstreamBinary(t *testing.T) {
	root := embeddedTree(t)

	offenders := map[string][]string{}
	for _, tx := range helpTexts(root) {
		for _, iv := range extractHintInvocations(tx.source, tx.body, upstreamBinaryName) {
			// Only a REAL command with the wrong binary in front of it is a
			// defect; prose that happens to follow the upstream name is not.
			if classifyHintPath(root, iv.path) != "" {
				continue
			}
			key := upstreamBinaryName + " " + strings.Join(iv.path, " ")
			offenders[key] = append(offenders[key], tx.source)
		}
	}

	for _, key := range sortedKeys(offenders) {
		t.Errorf("help text rendered for an embedder invokes %q — the command factory receives the "+
			"embedder's binary name (binaryName(cfg)); concatenate it into the string instead of "+
			"hardcoding %q\n    seen at: %s",
			key, upstreamBinaryName, strings.Join(offenders[key], ", "))
	}
}

// TestEmbeddedHelpCommandsResolve is assertion (2): every command the help
// text names must exist in the tree that registers it.
func TestEmbeddedHelpCommandsResolve(t *testing.T) {
	root := embeddedTree(t)

	var checked int
	offenders := map[string][]string{}
	details := map[string]string{}
	for _, tx := range helpTexts(root) {
		for _, iv := range extractHintInvocations(tx.source, tx.body, embedderBinaryName) {
			checked++
			detail := classifyHintPath(root, iv.path)
			if detail == "" {
				continue
			}
			key := embedderBinaryName + " " + strings.Join(iv.path, " ")
			offenders[key] = append(offenders[key], tx.source)
			details[key] = detail
		}
	}

	if checked < minHintInvocations {
		t.Fatalf("extracted only %d command invocations from the registered tree, expected at least %d — "+
			"extraction is broken, and a broken extractor makes both assertions in this file vacuous",
			checked, minHintInvocations)
	}

	for _, key := range sortedKeys(offenders) {
		t.Errorf("help text tells the user to run %q, but %s\n    seen at: %s",
			key, details[key], strings.Join(offenders[key], ", "))
	}
	if len(offenders) == 0 {
		t.Logf("checked %d command invocations across the registered tree, all resolve", checked)
	}
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// Proof the guards can fail
// ─────────────────────────────────────────────────────────────────────────────

// TestEmbeddedHelpGuards_FailOnPlantedHints grafts a command carrying both
// defects onto the REAL registered tree and asserts each guard reports its
// own. Without this the file would be indistinguishable from a no-op on any
// day the tree happens to be clean.
func TestEmbeddedHelpGuards_FailOnPlantedHints(t *testing.T) {
	root := embeddedTree(t)

	planted := &cobra.Command{
		Use:    "zz-planted-defect",
		Short:  "planted by binary_name_hints_test.go",
		Hidden: true,
		Long: "Examples:\n\n" +
			"  donmai github create-issue --title x\n" +
			"  acme-cli host frobnicate\n",
	}
	root.AddCommand(planted)
	t.Cleanup(func() { root.RemoveCommand(planted) })

	// Guard 1: the upstream-binary invocation is caught, and it is caught
	// BECAUSE the command it names is real.
	var foreign []string
	for _, iv := range extractHintInvocations("planted", planted.Long, upstreamBinaryName) {
		if classifyHintPath(root, iv.path) == "" {
			foreign = append(foreign, strings.Join(iv.path, " "))
		}
	}
	if len(foreign) != 1 || foreign[0] != "github create-issue" {
		t.Errorf("upstream-binary guard: got %v, want [github create-issue]", foreign)
	}

	// Guard 2: the nonexistent command is caught.
	var broken []string
	for _, iv := range extractHintInvocations("planted", planted.Long, embedderBinaryName) {
		if classifyHintPath(root, iv.path) != "" {
			broken = append(broken, strings.Join(iv.path, " "))
		}
	}
	if len(broken) != 1 || broken[0] != "host frobnicate" {
		t.Errorf("unresolvable-command guard: got %v, want [host frobnicate]", broken)
	}
}

// TestExtractHintInvocations_Table pins the invocation/prose boundary. These
// are the cases that decide whether this test is useful or gets disabled: too
// loose and every "the donmai binary" in a paragraph fails the build, too
// strict and the defects walk through.
func TestExtractHintInvocations_Table(t *testing.T) {
	tests := []struct {
		name string
		body string
		bin  string
		want []string
	}{
		// real invocations, one per admitting rule
		{"backticked", "Use `donmai host resume` to re-enable.", "donmai", []string{"host resume"}},
		{"single-quoted", "run 'donmai host install' first", "donmai", []string{"host install"}},
		{"bare literal", "donmai agent run <id>", "donmai", []string{"agent run"}},
		{"command block", "Examples:\n  donmai github create-issue --title x\n", "donmai", []string{"github create-issue"}},
		{"numbered step", "  1. brew upgrade donmai\n  2. donmai host update  # reload", "donmai", []string{"host update"}},
		{"shell prompt", "$ donmai kit install acme", "donmai", []string{"kit install acme"}},
		{"parenthetical", "Install the service (donmai host install)", "donmai", []string{"host install"}},
		{"trailing flag", "re-run with donmai kit install --allow-unsigned", "donmai", []string{"kit install"}},
		{"separator", "donmai host install → donmai host status", "donmai", []string{"host install", "host status"}},

		// placeholders terminate a run
		{"angle placeholder", "`donmai session show <id>`", "donmai", []string{"session show"}},
		{"square placeholder", "`donmai logs [name]`", "donmai", []string{"logs"}},
		{"ellipsis", "`donmai agent run ...`", "donmai", []string{"agent run"}},

		// prose must NOT be extracted — these all appear verbatim in this tree
		{"prose the-X", "Restart the donmai daemon process to pick it up.", "donmai", nil},
		{"prose bin flag", "WITHOUT (donmai stripped from PATH, the guard) and", "donmai", nil},
		{"prose binary", "Path to the donmai binary the WITH arm uses", "donmai", nil},
		{"state dir path", "Signatures live in ~/.config/donmai/log-signatures.yaml", "donmai", nil},
		{"hyphenated", "the donmai-daemon service unit", "donmai", nil},
		{"domain", "See https://donmai.dev/docs for details", "donmai", nil},

		// historical references, in-sentence only
		{"formerly", "the view formerly at `donmai fleet list`", "donmai", nil},
		{"cue previous sentence", "The flag was removed. Run `donmai fleet list` now.", "donmai", []string{"fleet list"}},

		// the embedder's own name extracts identically
		{"embedder name", "  acme-cli host install\n", "acme-cli", []string{"host install"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, iv := range extractHintInvocations("t", tc.body, tc.bin) {
				got = append(got, strings.Join(iv.path, " "))
			}
			if len(got) != len(tc.want) {
				t.Fatalf("extracted %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("invocation %d: got %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestClassifyHintPath_Table pins the resolve/reject boundary against the real
// registered tree.
func TestClassifyHintPath_Table(t *testing.T) {
	root := embeddedTree(t)

	tests := []struct {
		path   string
		broken bool
	}{
		{"host install", false},
		{"github create-issue", false},
		{"logs analyze", false},
		{"agent run", false},
		{"agent run some-session-id", false}, // positional on a runnable leaf
		{"kit install", false},

		{"host frobnicate", true},
		{"agent nonexistent", true},
		{"not-a-noun", true},
		{"github create-nothing", true},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			detail := classifyHintPath(root, strings.Fields(tc.path))
			switch {
			case tc.broken && detail == "":
				t.Errorf("%q should be reported broken, but classified clean", tc.path)
			case !tc.broken && detail != "":
				t.Errorf("%q should resolve, but was reported: %s", tc.path, detail)
			}
		})
	}
}
