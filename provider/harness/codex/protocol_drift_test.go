package codex

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"testing"
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
