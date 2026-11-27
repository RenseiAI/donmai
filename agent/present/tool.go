// Package present normalizes harness tool traffic for display: one tool
// family, human title and one-line detail per call across harnesses (Claude
// `Bash{command}`, Codex `shell` with a `/bin/zsh -lc "…"` wrapper, Pi
// `read{path}`, MCP `mcp__<server>__<tool>`), plus decoding of tool output.
// It is pure and dependency-free; testdata/vectors.json pins the rules so a
// second implementation can prove it renders the same labels.
package present

import (
	"strings"
)

// Family groups tools that render alike (glyph, detail key).
type Family string

// Tool families. Stable wire values.
const (
	FamilyCommand Family = "command"
	FamilyRead    Family = "read"
	FamilyEdit    Family = "edit"
	FamilySearch  Family = "search"
	FamilyWeb     Family = "web"
	FamilyAgent   Family = "agent"
	FamilyPlan    Family = "plan"
	FamilyMCP     Family = "mcp"
	FamilyOther   Family = "other"
)

var familyByName = func() map[string]Family {
	m := map[string]Family{}
	add := func(f Family, names ...string) {
		for _, n := range names {
			m[n] = f
		}
	}
	add(FamilyCommand, "bash", "shell", "sh", "zsh", "exec", "exec_command", "local_shell",
		"run_command", "execute_command", "terminal", "run_terminal_cmd", "command", "powershell")
	add(FamilyRead, "read", "read_file", "view", "cat", "open_file", "notebookread")
	add(FamilyEdit, "edit", "write", "multiedit", "apply_patch", "file_change", "notebookedit",
		"create_file", "str_replace", "str_replace_editor", "str_replace_based_edit_tool")
	add(FamilySearch, "grep", "glob", "ls", "find", "search", "file_search", "codebase_search", "list_files")
	add(FamilyWeb, "websearch", "web_search", "webfetch", "web_fetch", "fetch")
	add(FamilyAgent, "task", "agent", "subagent", "dispatch_agent")
	add(FamilyPlan, "todowrite", "todo_write", "update_plan", "exitplanmode")
	return m
}()

// detailKeys is the input-key priority per family; the first non-empty
// string wins. FamilyOther and FamilyMCP use the generic list.
var detailKeys = map[Family][]string{
	FamilyCommand: {"command", "cmd"},
	FamilyRead:    {"file_path", "path", "filePath", "notebook_path"},
	FamilyEdit:    {"file_path", "path", "filePath", "notebook_path"},
	FamilySearch:  {"pattern", "query", "path"},
	FamilyWeb:     {"url", "query"},
	FamilyAgent:   {"description", "prompt"},
	FamilyPlan:    {},
}

var genericDetailKeys = []string{"command", "file_path", "path", "filePath", "pattern", "query", "url", "description"}

// FamilyOf classifies a tool name, ignoring case and treating '-' as '_'.
func FamilyOf(name string) Family {
	key := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "-", "_")
	if strings.HasPrefix(key, "mcp__") || strings.HasPrefix(key, "mcp:") {
		return FamilyMCP
	}
	if f, ok := familyByName[key]; ok {
		return f
	}
	return FamilyOther
}

// Tool is the display form of one tool call.
//
// Name is the producer name verbatim ("tool" when absent); Title is the
// human label ("server › tool" for MCP); Detail is the one-line argument —
// the command without its shell wrapper, the path, pattern, URL or
// description — or empty.
type Tool struct {
	Name   string `json:"name"`
	Family Family `json:"family"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
}

// Summary is the one-line form "Name: detail", or the name alone. It keeps
// the producer name (not the title) so persisted summaries stay stable.
func (t Tool) Summary() string {
	if t.Detail == "" {
		return t.Name
	}
	return t.Name + ": " + t.Detail
}

// DescribeTool derives the display form of a call from its name and input.
func DescribeTool(name string, input map[string]any) Tool {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "tool"
	}
	fam := FamilyOf(name)
	t := Tool{Name: name, Family: fam, Title: mcpTitle(name)}
	if t.Title == "" {
		t.Title = name
	}
	keys, ok := detailKeys[fam]
	if !ok {
		keys = genericDetailKeys
	}
	for _, k := range keys {
		v := argString(input[k])
		if v == "" {
			continue
		}
		if k == "command" || k == "cmd" {
			v = UnwrapShell(v)
		}
		t.Detail = collapseWS(v)
		break
	}
	return t
}

// mcpTitle renders `mcp__server__tool` and `mcp:server/tool` as
// "server › tool"; "" for non-MCP names.
func mcpTitle(name string) string {
	switch {
	case strings.HasPrefix(name, "mcp__"):
		rest := strings.TrimPrefix(name, "mcp__")
		server, tool, ok := strings.Cut(rest, "__")
		if !ok || server == "" || tool == "" {
			return ""
		}
		return server + " › " + tool
	case strings.HasPrefix(name, "mcp:"):
		server, tool, ok := strings.Cut(strings.TrimPrefix(name, "mcp:"), "/")
		if !ok || server == "" || tool == "" {
			return ""
		}
		return server + " › " + tool
	}
	return ""
}

// argString reads a string, or an argv array (shell wrapper removed).
func argString(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case []any:
		parts := make([]string, 0, len(x))
		for _, p := range x {
			s, ok := p.(string)
			if !ok {
				return ""
			}
			parts = append(parts, s)
		}
		return strings.TrimSpace(unwrapArgv(parts))
	}
	return ""
}

func unwrapArgv(argv []string) string {
	if len(argv) == 3 && isShell(argv[0]) && isShellFlag(argv[1]) {
		return argv[2]
	}
	return strings.Join(argv, " ")
}

func isShell(s string) bool {
	base := s[strings.LastIndex(s, "/")+1:]
	switch base {
	case "sh", "bash", "zsh", "dash", "ksh", "fish":
		return true
	}
	return false
}

func isShellFlag(s string) bool { return s == "-c" || s == "-lc" || s == "-ic" || s == "-lic" }

// UnwrapShell removes a `[/usr/bin/env] <shell> -c|-lc <script>` wrapper and
// the script's outer quotes; any other shape is returned trimmed.
func UnwrapShell(cmd string) string {
	s := strings.TrimSpace(cmd)
	rest := strings.TrimPrefix(s, "/usr/bin/env ")
	sh, rest, ok := strings.Cut(rest, " ")
	if !ok || !isShell(sh) {
		return s
	}
	flag, script, ok := strings.Cut(strings.TrimLeft(rest, " "), " ")
	if !ok || !isShellFlag(flag) {
		return s
	}
	script = strings.TrimSpace(script)
	if unq, ok := unquote(script); ok {
		return strings.TrimSpace(unq)
	}
	return script
}

// unquote strips one pair of outer quotes, undoing double-quote escapes.
func unquote(s string) (string, bool) {
	if len(s) < 2 {
		return "", false
	}
	q := s[0]
	if (q != '"' && q != '\'') || s[len(s)-1] != q {
		return "", false
	}
	body := s[1 : len(s)-1]
	if q == '\'' {
		if strings.Contains(body, "'") {
			return "", false // '\'' splices: not a single quoted word
		}
		return body, true
	}
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c == '\\' && i+1 < len(body) && strings.IndexByte("\"\\$`", body[i+1]) >= 0 {
			b.WriteByte(body[i+1])
			i++
			continue
		}
		if c == '"' {
			return "", false // an unescaped inner quote: several words
		}
		b.WriteByte(c)
	}
	return b.String(), true
}

func collapseWS(s string) string { return strings.Join(strings.Fields(s), " ") }
