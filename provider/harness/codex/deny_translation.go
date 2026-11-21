package codex

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// Tool-permission denies on Codex.
//
// The runner hands Codex the session's deny list as
// Spec.PermissionConfig.DisallowPatterns, in the Claude permission grammar: a
// bare tool name ("Bash"), a command pattern ("Bash(git push*)", "Bash(rm:*)",
// "Bash(env)") or a path pattern ("Read(~/.ssh/**)", "Edit(docs/**)"). The
// approval bridge (approval.go) only sees what Codex asks about, and under
// on-request Codex asks about almost nothing, so each deny is also rendered
// into the Codex mechanism that holds whether or not Codex asks:
//
//   - a bare Bash deny turns Codex's shell tool off (features.shell_tool);
//   - a command prefix becomes an exec-policy prefix_rule with decision
//     "forbidden" in the private CODEX_HOME's rules directory. An exact command
//     becomes a "prompt" rule, so the bridge's exact match decides it instead
//     of a prefix rule blocking every longer command;
//   - a path deny becomes a "none" (read) or "read" (write) entry of a
//     permission profile that extends the built-in profile for the sandbox
//     level, selected on thread/start, thread/resume and turn/start.
//
// Checked against codex-cli 0.170.0: exec_command leaves the tool list
// `codex debug prompt-input` renders under features.shell_tool=false; `codex
// execpolicy check` evaluates the rule file; a rule file that fails to parse
// is skipped whole with only a configWarning, which is why the tokens are
// restricted below; `codex sandbox -P` refuses reads and writes through the
// profile entries; and the app-server echoes the selected profile.
//
// Residual risk, which no Codex setting closes:
//   - prefix rules match tokens, so "git push*" does not block "git pushx",
//     and nothing blocks a program reached through its own global options
//     ("git -C dir push");
//   - Codex splits plain compound scripts ("cd x && git push") before matching,
//     but evaluates a script with redirects or substitutions as one unmatched
//     command. Wrapper programs (env, xargs, bash -c, ...) get "prompt" rules so
//     the bridge can unwrap them; a script Codex does not split never asks;
//   - a write deny of a bare Edit/Write name only reaches the bridge, and
//     Codex does not ask about patches inside the writable roots;
//   - a full-access session with path denies runs under the Codex sandbox
//     with the whole disk writable (":danger-full-access" cannot be
//     extended), not unsandboxed;
//   - tool names Codex has no tool for (AskUserQuestion, WebFetch, MCP tool
//     names) are reported unsupported.

// DenyStatus is one translation-report verdict, in the vocabulary of the
// harness interaction contract.
type DenyStatus string

// DenyStatus values.
const (
	DenyApplied      DenyStatus = "applied"
	DenyApproximated DenyStatus = "approximated"
	DenyUnsupported  DenyStatus = "unsupported"
)

// DenyTranslation reports how one deny pattern reached Codex.
type DenyTranslation struct {
	Pattern   string     `json:"pattern"`
	Status    DenyStatus `json:"status"`
	Mechanism string     `json:"mechanism,omitempty"`
	Note      string     `json:"note,omitempty"`
}

// ErrDenyUntranslatable refuses a spawn whose deny list Codex cannot enforce:
// dropping the deny would run the session with less protection than it asked for.
var ErrDenyUntranslatable = errors.New("codex: tool-permission deny cannot be enforced")

// SystemSubtypeToolPermissionTranslation is the SystemEvent subtype that
// carries every deny Codex enforces only approximately or not at all.
const SystemSubtypeToolPermissionTranslation = "tool_permission_translation"

const (
	codexDenyProfileID = "donmai-deny"
	codexDenyRulesFile = "donmai-deny.rules"
)

// commandWrapperPrograms run their arguments as another command. Each gets a
// "prompt" rule whenever a command deny exists, so the bridge sees the line.
var commandWrapperPrograms = []string{"bash", "command", "dash", "env", "eval", "exec", "ksh", "nice", "nohup", "sh", "time", "timeout", "xargs", "zsh"}

type execRule struct {
	tokens   []string
	decision string
}

type denyPlan struct {
	disableShell bool
	rules        []execRule
	paths        map[string]string // Codex filesystem path or glob -> profile access
	report       []DenyTranslation
}

// translateDenies is pure and deterministic, so Spawn can refuse before any
// side effect and NewSpawnPlan can rebuild the same params.
func translateDenies(spec agent.Spec) (denyPlan, error) {
	var plan denyPlan
	if spec.PermissionConfig == nil {
		return plan, nil
	}
	for _, raw := range spec.PermissionConfig.DisallowPatterns {
		line, err := plan.add(strings.TrimSpace(raw), spec.Cwd)
		if err != nil {
			return denyPlan{}, fmt.Errorf("%w: %q: %w", ErrDenyUntranslatable, raw, err)
		}
		line.Pattern = raw
		plan.report = append(plan.report, line)
	}
	if len(plan.rules) > 0 {
		for _, program := range commandWrapperPrograms {
			plan.rules = append(plan.rules, execRule{tokens: []string{program}, decision: "prompt"})
		}
	}
	return plan, nil
}

func (p *denyPlan) add(pattern, cwd string) (DenyTranslation, error) {
	name, arg, scoped := splitToolPattern(pattern)
	switch {
	case name == "Bash" && (!scoped || arg == "" || arg == "*"):
		p.disableShell = true
		return DenyTranslation{Status: DenyApplied, Mechanism: "features.shell_tool=false"}, nil
	case name == "Bash":
		return p.addCommand(arg), nil
	case scoped && (name == "Read" || isFileChangeTool(name)):
		path, err := codexDenyPath(arg, cwd)
		if err != nil {
			return DenyTranslation{}, err
		}
		access := "read" // a write deny keeps the path readable
		if name == "Read" {
			access = "none"
		}
		if p.paths == nil {
			p.paths = map[string]string{}
		}
		if p.paths[path] != "none" {
			p.paths[path] = access
		}
		return DenyTranslation{Status: DenyApplied, Mechanism: "permissions." + codexDenyProfileID + ".filesystem"}, nil
	case name == "Read":
		return DenyTranslation{}, errors.New("a deny of every read has no Codex rendering")
	case isFileChangeTool(name):
		return DenyTranslation{Status: DenyApproximated, Mechanism: "approval bridge", Note: "Codex does not ask about patches inside the writable roots"}, nil
	}
	return DenyTranslation{Status: DenyUnsupported, Note: "Codex has no tool by this name"}, nil
}

func (p *denyPlan) addCommand(arg string) DenyTranslation {
	body, exact := strings.TrimSpace(arg), true
	if trimmed, ok := strings.CutSuffix(body, ":*"); ok {
		body, exact = trimmed, false
	} else if trimmed, ok := strings.CutSuffix(body, "*"); ok {
		body, exact = trimmed, false
	}
	tokens := strings.Fields(body)
	if len(tokens) == 0 || strings.ContainsAny(body, "*?[]{}$`'\"\\|&;<>()~!#") || strings.ContainsFunc(body, isControl) {
		p.disableShell = true
		return DenyTranslation{Status: DenyApproximated, Mechanism: "features.shell_tool=false", Note: "not a plain command prefix; the shell tool is disabled instead"}
	}
	if exact {
		p.rules = append(p.rules, execRule{tokens: tokens, decision: "prompt"})
		return DenyTranslation{Status: DenyApplied, Mechanism: "execpolicy prompt rule + approval bridge exact match"}
	}
	p.rules = append(p.rules, execRule{tokens: tokens, decision: "forbidden"})
	return DenyTranslation{Status: DenyApplied, Mechanism: "execpolicy forbidden prefix rule"}
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// codexDenyPath renders a Claude path pattern as a Codex filesystem key, which
// must be absolute or start with "~/". Codex reads a brace set or an unclosed
// character class as literal text, so such a deny would protect nothing.
func codexDenyPath(glob, cwd string) (string, error) {
	switch {
	case glob == "":
		return "", errors.New("empty path pattern")
	case strings.ContainsAny(glob, "{}") || strings.ContainsFunc(glob, isControl):
		return "", errors.New("brace sets and control characters have no Codex glob form")
	case strings.Count(glob, "[") != strings.Count(glob, "]"):
		return "", errors.New("unbalanced character class")
	case strings.HasPrefix(glob, "~/"):
		return "~/" + strings.TrimPrefix(filepath.Clean(glob[2:]), "/"), nil
	case strings.HasPrefix(glob, "~") || strings.HasPrefix(glob, ":"):
		return "", errors.New("only ~/ home paths are supported")
	case filepath.IsAbs(glob):
		return filepath.Clean(glob), nil
	case cwd == "":
		return "", errors.New("relative path pattern without a working directory")
	}
	return filepath.Join(cwd, glob), nil
}

// threadConfig is the thread/start and thread/resume `config` override.
func (p denyPlan) threadConfig(spec agent.Spec) map[string]any {
	config := map[string]any{}
	if p.disableShell {
		config["features"] = map[string]any{"shell_tool": false}
	}
	if len(p.paths) > 0 {
		config["permissions"] = map[string]any{codexDenyProfileID: p.permissionProfile(spec)}
	}
	if len(config) == 0 {
		return nil
	}
	return config
}

// permissionProfile keeps the legacy sandbox level's meaning and adds the deny
// entries. Entries are written last, so a deny on a writable root wins.
func (p denyPlan) permissionProfile(spec agent.Spec) map[string]any {
	filesystem := map[string]any{}
	profile := map[string]any{"filesystem": filesystem, "network": map[string]any{"enabled": true}}
	switch resolveSandboxMode(spec) {
	case "danger-full-access":
		filesystem[":root"] = "write"
	case "workspace-write":
		profile["extends"] = ":workspace"
		for _, root := range sandboxWritableRoots(spec) {
			if root != "" {
				filesystem[root] = "write"
			}
		}
	case "read-only":
		profile["extends"] = ":read-only"
	default:
		profile["extends"] = ":read-only"
		delete(profile, "network")
	}
	for path, access := range p.paths {
		filesystem[path] = access
	}
	return profile
}

// execPolicyRules renders the rule file staged into CODEX_HOME/rules.
func (p denyPlan) execPolicyRules() string {
	var b strings.Builder
	for _, rule := range p.rules {
		quoted := make([]string, len(rule.tokens))
		for i, token := range rule.tokens {
			quoted[i] = strconv.Quote(token)
		}
		fmt.Fprintf(&b, "prefix_rule(pattern=[%s], decision=%q, justification=%q)\n",
			strings.Join(quoted, ", "), rule.decision, "denied by the session tool policy")
	}
	return b.String()
}

// notice reports every deny Codex does not enforce as asked, or nil.
func (p denyPlan) notice() *agent.SystemEvent {
	var weak []DenyTranslation
	for _, line := range p.report {
		if line.Status != DenyApplied {
			weak = append(weak, line)
		}
	}
	if len(weak) == 0 {
		return nil
	}
	return &agent.SystemEvent{
		Subtype: SystemSubtypeToolPermissionTranslation,
		Message: fmt.Sprintf("%d of %d tool-permission denies are approximated or unsupported on Codex", len(weak), len(p.report)),
		Raw:     weak,
	}
}
