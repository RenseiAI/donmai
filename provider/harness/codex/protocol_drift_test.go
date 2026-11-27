package codex

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// Read the actual dispatch switch, not a second list of "mapped" names which
// could survive deletion of the production routing. The installed CLI's schema
// inventory is independent of both this switch and the explicit ignore policy.
func dispatchCases(t *testing.T, function string) map[string]bool {
	t.Helper()
	source, err := parser.ParseFile(token.NewFileSet(), "event_mapping.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, decl := range source.Decls {
		f, ok := decl.(*ast.FuncDecl)
		if !ok || f.Name.Name != function {
			continue
		}
		// Only the top-level dispatch switch counts. Nested status switches
		// must not accidentally classify a future method with the same name.
		for _, statement := range f.Body.List {
			dispatch, ok := statement.(*ast.SwitchStmt)
			if !ok {
				continue
			}
			for _, statement := range dispatch.Body.List {
				clause, ok := statement.(*ast.CaseClause)
				if !ok {
					continue
				}
				for _, expr := range clause.List {
					literal, ok := expr.(*ast.BasicLit)
					if !ok || literal.Kind != token.STRING {
						continue
					}
					name, err := strconv.Unquote(literal.Value)
					if err != nil {
						t.Fatal(err)
					}
					names[name] = true
				}
			}
		}
	}
	return names
}

func TestCodexProtocolDrift(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("testdata/protocol-inventory.json")
	if err != nil {
		t.Fatal(err)
	}
	var inventory struct {
		Notifications []string `json:"notifications"`
		Items         []string `json:"items"`
	}
	if err := json.Unmarshal(raw, &inventory); err != nil {
		t.Fatal(err)
	}
	if len(inventory.Notifications) == 0 || len(inventory.Items) == 0 {
		t.Fatal("empty protocol inventory")
	}
	for _, group := range []struct {
		names   []string
		mapped  map[string]bool
		ignored map[string]string
	}{
		{inventory.Notifications, dispatchCases(t, "mapNotification"), ignoredNotifications},
		{inventory.Items, dispatchCases(t, "mapItem"), ignoredItems},
	} {
		for _, name := range group.names {
			_, ignored := group.ignored[name]
			if !group.mapped[name] && !ignored {
				t.Errorf("protocol drift: %s is neither mapped nor explicitly ignored; regenerate and review the dispatch policy", name)
			}
		}
	}
}

// TestCodexRequestParamDrift refuses a request key the installed Codex CLI
// does not read. The app-server deserializes params leniently: an unknown key
// is dropped without an error, so a misnamed param (turn/start once sent
// reasoningEffort where the schema reads effort) silently loses its setting
// on every session. Every key the builders emit for a fully populated Spec
// must be a property of the vendored request param schema.
func TestCodexRequestParamDrift(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("testdata/protocol-inventory.json")
	if err != nil {
		t.Fatal(err)
	}
	var inventory struct {
		RequestParams map[string][]string `json:"requestParams"`
	}
	if err := json.Unmarshal(raw, &inventory); err != nil {
		t.Fatal(err)
	}
	spec := agent.Spec{
		Cwd: "/tmp/w", Model: "gpt-5.6-sol", Effort: agent.EffortXHigh,
		BaseInstructions: "base", SystemPromptAppend: "append",
		SandboxLevel: agent.SandboxWorkspaceWrite, Autonomous: true,
		ResponseSchema: json.RawMessage(`{"type":"object"}`),
	}
	plan := NewSpawnPlan(spec)
	denied := spec
	denied.PermissionConfig = &agent.PermissionConfig{DisallowPatterns: []string{"Bash"}}
	deniedPlan := NewSpawnPlan(denied)
	for _, request := range []struct {
		method string
		params map[string]any
		must   []string
	}{
		{method: "thread/start", params: plan.ThreadStart, must: []string{"baseInstructions", "developerInstructions", "sandbox"}},
		{method: "turn/start", params: turnStartParams("thread-1", spec, plan.PromptInput), must: []string{"effort", "sandboxPolicy", "outputSchema"}},
		{method: "thread/start", params: deniedPlan.ThreadStart, must: []string{"config"}},
		{method: "thread/resume", params: deniedPlan.ThreadResume, must: []string{"threadId", "config"}},
	} {
		known := inventory.RequestParams[request.method]
		if len(known) == 0 {
			t.Fatalf("protocol inventory has no %s params; regenerate it", request.method)
		}
		for _, key := range request.must {
			if _, ok := request.params[key]; !ok {
				t.Errorf("%s: fully populated spec emitted no %q, so this test no longer covers it", request.method, key)
			}
		}
		for key := range request.params {
			if !slices.Contains(known, key) {
				t.Errorf("%s sends %q, which the vendored app-server schema for %s does not read (it would be dropped silently)", request.method, key, request.method)
			}
		}
	}
}
