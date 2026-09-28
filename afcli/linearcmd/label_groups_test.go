package linearcmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
)

func TestListLabelsPreservesNativeGroupAndDuplicateScopeMetadata(t *testing.T) {
	setupLinearTest(t, func(w http.ResponseWriter, _ *http.Request) {
		writeLinearGQLData(w, `{"issueLabels":{"nodes":[
			{"id":"workspace-child","name":"draft","team":null,"isGroup":false,"groupType":null,"parent":{"id":"workspace-group","name":"content"}},
			{"id":"team-child","name":"draft","team":{"id":"team-1","key":"ENG"},"isGroup":false,"groupType":null,"parent":{"id":"team-group","name":"content"}},
			{"id":"flat-colon","name":"content:review","team":null,"isGroup":false,"groupType":null,"parent":null}
		],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
	})
	out, err := runLinearCmd(t, "", "list-labels")
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		ID       string  `json:"id"`
		Name     string  `json:"name"`
		Scope    string  `json:"scope"`
		IsGroup  bool    `json:"isGroup"`
		ParentID *string `json:"parentId"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("list-labels lost cross-scope duplicate: rows=%+v", rows)
	}
	seen := make(map[string]struct {
		scope  string
		parent *string
	})
	for _, row := range rows {
		seen[row.ID] = struct {
			scope  string
			parent *string
		}{row.Scope, row.ParentID}
		if row.ID == "flat-colon" && row.ParentID != nil {
			t.Fatalf("colon-prefixed flat label inferred as group child: %+v", row)
		}
	}
	if seen["workspace-child"].scope != "workspace" || seen["workspace-child"].parent == nil ||
		seen["team-child"].scope != "team" || seen["team-child"].parent == nil {
		t.Fatalf("native group membership/scope missing: rows=%+v", rows)
	}
}

type nativeLabelFixture struct {
	labels              []map[string]any
	issueLabels         []string
	creates             int
	updates             int
	adds                int
	issueLabelReads     int
	forbidWrite         bool
	keepArchivedSibling bool
}

func (f *nativeLabelFixture) label(id string) map[string]any {
	for _, label := range f.labels {
		if label["id"] == id {
			return label
		}
	}
	return nil
}

func (f *nativeLabelFixture) serve(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	write := func(data map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}
	if f.forbidWrite && strings.Contains(request.Query, "mutation ") {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	switch {
	case strings.Contains(request.Query, "ListTeams"):
		write(map[string]any{"teams": map[string]any{"nodes": []map[string]any{{"id": "team-1", "key": "ENG", "name": "Engineering"}}, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}}})
	case strings.Contains(request.Query, "ListLabelDetails"):
		active := make([]map[string]any, 0, len(f.labels))
		for _, label := range f.labels {
			if label["archived"] != true {
				active = append(active, label)
			}
		}
		write(map[string]any{"issueLabels": map[string]any{"nodes": active, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}}})
	case strings.Contains(request.Query, "ListIssueLabels"):
		f.issueLabelReads++
		start := 0
		if request.Variables["after"] == "first-page" {
			start = 1
		}
		end := len(f.issueLabels)
		if start == 0 && end > 1 {
			end = 1
		}
		rows := make([]map[string]any, 0, end-start)
		for _, id := range f.issueLabels[start:end] {
			label := f.label(id)
			var parent any
			if relation, ok := label["parent"].(map[string]any); ok {
				parent = map[string]any{"id": relation["id"]}
			}
			rows = append(rows, map[string]any{"id": id, "name": label["name"], "isGroup": label["isGroup"], "parent": parent})
		}
		hasNext := end < len(f.issueLabels)
		var cursor any
		if hasNext {
			cursor = "first-page"
		}
		write(map[string]any{"issue": map[string]any{"id": "issue-1", "labels": map[string]any{"nodes": rows, "pageInfo": map[string]any{"hasNextPage": hasNext, "endCursor": cursor}}}})
	case strings.Contains(request.Query, "CreateNativeLabel"):
		f.creates++
		input := request.Variables["input"].(map[string]any)
		id := fmt.Sprintf("created-%d", f.creates)
		label := map[string]any{"id": id, "name": input["name"], "isGroup": false, "groupType": nil, "team": nil, "parent": nil}
		if teamID, ok := input["teamId"].(string); ok {
			label["team"] = map[string]any{"id": teamID, "key": "ENG"}
		}
		if input["isGroup"] == true {
			label["isGroup"] = true
			label["groupType"] = input["groupType"]
		}
		if parentID, ok := input["parentId"].(string); ok {
			parent := f.label(parentID)
			label["parent"] = map[string]any{"id": parentID, "name": parent["name"]}
		}
		f.labels = append(f.labels, label)
		write(map[string]any{"issueLabelCreate": map[string]any{"success": true, "issueLabel": label}})
	case strings.Contains(request.Query, "ReparentLabel"):
		f.updates++
		id := request.Variables["id"].(string)
		parentID := request.Variables["input"].(map[string]any)["parentId"].(string)
		label, parent := f.label(id), f.label(parentID)
		label["parent"] = map[string]any{"id": parentID, "name": parent["name"]}
		write(map[string]any{"issueLabelUpdate": map[string]any{"success": true, "issueLabel": label}})
	case strings.Contains(request.Query, "GetIssue"):
		// GetIssue's inherited fragment has no pagination; model only its first
		// page so grouped selection must use ListIssueLabels for completeness.
		labels := make([]map[string]any, 0, 1)
		for i, id := range f.issueLabels {
			if i >= 1 {
				break
			}
			label := f.label(id)
			labels = append(labels, map[string]any{"id": id, "name": label["name"]})
		}
		write(map[string]any{"issue": map[string]any{"id": "issue-1", "identifier": "ENG-1", "title": "Issue", "team": map[string]any{"id": "team-1", "key": "ENG", "name": "Engineering"}, "state": map[string]any{"id": "state-1", "name": "Backlog"}, "labels": map[string]any{"nodes": labels}}})
	case strings.Contains(request.Query, "AddIssueLabel"):
		f.adds++
		targetID := request.Variables["labelId"].(string)
		parent, _ := f.label(targetID)["parent"].(map[string]any)
		var parentID any
		if parent != nil {
			parentID = parent["id"]
		}
		kept := make([]string, 0, len(f.issueLabels)+1)
		for _, id := range f.issueLabels {
			label := f.label(id)
			otherParent, _ := label["parent"].(map[string]any)
			if parentID == nil || otherParent == nil || otherParent["id"] != parentID || f.keepArchivedSibling && label["archived"] == true {
				kept = append(kept, id)
			}
		}
		kept = append(kept, targetID)
		f.issueLabels = kept
		write(map[string]any{"issueAddLabel": map[string]any{"success": true, "issue": map[string]any{"id": "issue-1", "identifier": "ENG-1", "title": "Issue", "team": map[string]any{"id": "team-1", "key": "ENG", "name": "Engineering"}, "state": map[string]any{"id": "state-1", "name": "Backlog"}, "labels": map[string]any{"nodes": []any{}}}}})
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func TestNativeGroupCommandsReuseManualGroupAndChild(t *testing.T) {
	fixture := &nativeLabelFixture{labels: []map[string]any{
		{"id": "manual-group", "name": "Type", "isGroup": true, "groupType": nil, "team": nil, "parent": nil},
		{"id": "manual-child", "name": "Bug", "isGroup": false, "groupType": nil, "team": nil, "parent": map[string]any{"id": "manual-group", "name": "Type"}},
	}}
	setupLinearTest(t, fixture.serve)
	for i := 0; i < 2; i++ {
		out, err := runLinearCmd(t, "", "create-label-group", "--name", "Type", "--workspace")
		if err != nil || decodeJSON(t, out)["reused"] != true {
			t.Fatalf("reuse manual group: out=%s err=%v", out, err)
		}
		out, err = runLinearCmd(t, "", "create-group-label", "--name", "Bug", "--group-id", "manual-group", "--workspace")
		if err != nil || decodeJSON(t, out)["reused"] != true {
			t.Fatalf("reuse manual child: out=%s err=%v", out, err)
		}
	}
	if fixture.creates != 0 {
		t.Fatalf("manual native labels duplicated: creates=%d", fixture.creates)
	}
}

func TestNativeGroupCommandsCreateReadBackAndReparentID(t *testing.T) {
	fixture := &nativeLabelFixture{labels: []map[string]any{
		{"id": "existing-flat", "name": "Existing", "isGroup": false, "groupType": nil, "team": map[string]any{"id": "team-1", "key": "ENG"}, "parent": nil},
		{"id": "workspace-group", "name": "Type", "isGroup": true, "groupType": "singleSelect", "team": nil, "parent": nil},
	}}
	setupLinearTest(t, fixture.serve)
	groupOut, err := runLinearCmd(t, "", "create-label-group", "--name", "Type", "--team", "ENG")
	if err != nil || decodeJSON(t, groupOut)["reused"] != false {
		t.Fatalf("create group: out=%s err=%v", groupOut, err)
	}
	childOut, err := runLinearCmd(t, "", "create-group-label", "--name", "Bug", "--group-id", "created-1", "--team", "ENG")
	if err != nil || decodeJSON(t, childOut)["reused"] != false {
		t.Fatalf("create child: out=%s err=%v", childOut, err)
	}
	movedOut, err := runLinearCmd(t, "", "reparent-label", "--label-id", "existing-flat", "--group-id", "created-1", "--team", "ENG")
	if err != nil {
		t.Fatalf("reparent: %v", err)
	}
	moved := decodeJSON(t, movedOut)["label"].(map[string]any)
	if moved["id"] != "existing-flat" || moved["parentId"] != "created-1" || fixture.creates != 2 || fixture.updates != 1 {
		t.Fatalf("identity/membership drift: moved=%v creates=%d updates=%d", moved, fixture.creates, fixture.updates)
	}
	if out, err := runLinearCmd(t, "", "create-label-group", "--name", "Type", "--team", "ENG"); err != nil || decodeJSON(t, out)["reused"] != true {
		t.Fatalf("repeat team group did not reuse exact scope: out=%s err=%v", out, err)
	}
	if out, err := runLinearCmd(t, "", "create-group-label", "--name", "Bug", "--group-id", "created-1", "--team", "ENG"); err != nil || decodeJSON(t, out)["reused"] != true {
		t.Fatalf("repeat child did not reuse exact parent: out=%s err=%v", out, err)
	}
	if fixture.creates != 2 {
		t.Fatalf("repeat requests created duplicates: %d", fixture.creates)
	}
}

func TestNativeGroupSelectionReplacesOnlySameGroup(t *testing.T) {
	fixture := &nativeLabelFixture{labels: []map[string]any{
		{"id": "group-1", "name": "Type", "isGroup": true, "groupType": "singleSelect", "team": nil, "parent": nil},
		{"id": "old", "name": "Bug", "isGroup": false, "groupType": nil, "team": nil, "parent": map[string]any{"id": "group-1", "name": "Type"}},
		{"id": "archived-old", "name": "Retired Bug", "isGroup": false, "groupType": nil, "team": nil, "parent": map[string]any{"id": "group-1", "name": "Type"}, "archived": true},
		{"id": "next", "name": "Feature", "isGroup": false, "groupType": nil, "team": nil, "parent": map[string]any{"id": "group-1", "name": "Type"}},
		{"id": "other", "name": "Urgent", "isGroup": false, "groupType": nil, "team": nil, "parent": nil},
		{"id": "archived-other", "name": "Retired Urgent", "isGroup": false, "groupType": nil, "team": nil, "parent": nil, "archived": true},
	}, issueLabels: []string{"old", "archived-old", "other", "archived-other"}}
	setupLinearTest(t, fixture.serve)
	out, err := runLinearCmd(t, "", "select-group-label", "ENG-1", "--label-id", "next")
	if err != nil {
		t.Fatal(err)
	}
	result := decodeJSON(t, out)
	if result["appliedLabelId"] != "next" || fixture.adds != 1 || fixture.issueLabelReads != 4 || len(fixture.issueLabels) != 3 || fixture.issueLabels[0] != "other" || fixture.issueLabels[1] != "archived-other" || fixture.issueLabels[2] != "next" {
		t.Fatalf("selection did not replace archived sibling/preserve archived unrelated label: out=%v state=%v adds=%d pages=%d", result, fixture.issueLabels, fixture.adds, fixture.issueLabelReads)
	}
}

func TestNativeGroupSelectionRefusesProviderRetainingArchivedSibling(t *testing.T) {
	fixture := &nativeLabelFixture{labels: []map[string]any{
		{"id": "group-1", "name": "Type", "isGroup": true, "groupType": "singleSelect", "team": nil, "parent": nil},
		{"id": "archived-old", "name": "Retired Bug", "isGroup": false, "groupType": nil, "team": nil, "parent": map[string]any{"id": "group-1", "name": "Type"}, "archived": true},
		{"id": "next", "name": "Feature", "isGroup": false, "groupType": nil, "team": nil, "parent": map[string]any{"id": "group-1", "name": "Type"}},
	}, issueLabels: []string{"archived-old"}, keepArchivedSibling: true}
	setupLinearTest(t, fixture.serve)
	if _, err := runLinearCmd(t, "", "select-group-label", "ENG-1", "--label-id", "next"); err == nil || !strings.Contains(err.Error(), "prior group selection") {
		t.Fatalf("provider retained archived sibling but command claimed success: %v", err)
	}
	if fixture.adds != 1 {
		t.Fatalf("native atomic add calls=%d, want one", fixture.adds)
	}
}

func TestNativeGroupCommandsRejectScopeAndAuthorization(t *testing.T) {
	fixture := &nativeLabelFixture{labels: []map[string]any{}, forbidWrite: true}
	setupLinearTest(t, fixture.serve)
	if _, err := runLinearCmd(t, "", "create-label-group", "--name", "Type"); err == nil || !strings.Contains(err.Error(), "choose exactly one scope") {
		t.Fatalf("missing scope error=%v", err)
	}
	if _, err := runLinearCmd(t, "", "create-label-group", "--name", "Type", "--workspace"); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("auth refusal error=%v", err)
	}
	if fixture.creates != 0 {
		t.Fatalf("forbidden create was retried or applied: %d", fixture.creates)
	}
}

func TestApplyLabelRefusesGroupedOrAmbiguousNamesButAcceptsFlatID(t *testing.T) {
	fixture := &nativeLabelFixture{labels: []map[string]any{
		{"id": "group-1", "name": "Type", "isGroup": true, "groupType": "singleSelect", "team": nil, "parent": nil},
		{"id": "group-child", "name": "Bug", "isGroup": false, "groupType": nil, "team": nil, "parent": map[string]any{"id": "group-1", "name": "Type"}},
		{"id": "workspace-flat", "name": "Needs Review", "isGroup": false, "groupType": nil, "team": nil, "parent": nil},
		{"id": "team-flat", "name": "Needs Review", "isGroup": false, "groupType": nil, "team": map[string]any{"id": "team-1", "key": "ENG"}, "parent": nil},
	}, issueLabels: []string{}}
	setupLinearTest(t, fixture.serve)
	if _, err := runLinearCmd(t, "", "apply-label", "ENG-1", "--label", "Bug"); err == nil || !strings.Contains(err.Error(), "select-group-label") {
		t.Fatalf("group child applied by ambiguous name: %v", err)
	}
	if _, err := runLinearCmd(t, "", "apply-label", "ENG-1", "--label", "Needs Review"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("cross-scope duplicate applied by name: %v", err)
	}
	out, err := runLinearCmd(t, "", "apply-label", "ENG-1", "--label-id", "team-flat")
	if err != nil || decodeJSON(t, out)["appliedLabel"] != "Needs Review" || fixture.adds != 1 {
		t.Fatalf("exact flat ID refused: out=%s err=%v adds=%d", out, err, fixture.adds)
	}
}

func TestNativeGroupRejectsMultiSelectAndCrossScopeBeforeMutation(t *testing.T) {
	fixture := &nativeLabelFixture{labels: []map[string]any{
		{"id": "multi-group", "name": "Type", "isGroup": true, "groupType": "multiSelect", "team": nil, "parent": nil},
		{"id": "workspace-group", "name": "Stage", "isGroup": true, "groupType": "singleSelect", "team": nil, "parent": nil},
		{"id": "team-flat", "name": "Existing", "isGroup": false, "groupType": nil, "team": map[string]any{"id": "team-1", "key": "ENG"}, "parent": nil},
	}}
	setupLinearTest(t, fixture.serve)
	if _, err := runLinearCmd(t, "", "create-label-group", "--name", "Type", "--workspace"); err == nil || !strings.Contains(err.Error(), "multiSelect") {
		t.Fatalf("multi-select group reused as single-select: %v", err)
	}
	if _, err := runLinearCmd(t, "", "create-group-label", "--name", "Bug", "--group-id", "multi-group", "--workspace"); err == nil || !strings.Contains(err.Error(), "multiSelect") {
		t.Fatalf("child created under multi-select group: %v", err)
	}
	if _, err := runLinearCmd(t, "", "reparent-label", "--label-id", "team-flat", "--group-id", "workspace-group", "--team", "ENG"); err == nil || !strings.Contains(err.Error(), "selected scope") {
		t.Fatalf("cross-scope reparent accepted: %v", err)
	}
	if fixture.creates != 0 || fixture.updates != 0 {
		t.Fatalf("invalid group/scope mutated labels: creates=%d updates=%d", fixture.creates, fixture.updates)
	}
}

func TestNativeGroupHelpExplainsNativeMembershipAndExamples(t *testing.T) {
	for _, command := range []string{"list-labels", "create-label-group", "create-group-label", "reparent-label", "select-group-label"} {
		out, err := runLinearCmd(t, "", command, "--help")
		if err != nil || !strings.Contains(out, "Example:") {
			t.Fatalf("%s help missing example: err=%v out=%q", command, err, out)
		}
	}
	out, err := runLinearCmd(t, "", "apply-label", "--help")
	if err != nil || !strings.Contains(out, "select-group-label") {
		t.Fatalf("apply-label help lacks native group guidance: err=%v out=%q", err, out)
	}
}

func TestNativeLabelCLIProxyProbePrecedesEveryProviderCall(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "")
	t.Setenv("LINEAR_ACCESS_TOKEN", "")
	t.Setenv("WORKER_AUTH_TOKEN", "")
	t.Setenv("DONMAI_API_URL", "")
	acknowledge := false
	providerCalls := 0
	probes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/cli/linear/graphql/no-fallback-v1" ||
			r.Header.Get("Authorization") != "Bearer rsk_fixture" ||
			r.Header.Get("X-Donmai-Linear-Auth-Policy") != "no-fallback-v1" {
			t.Errorf("unexpected strict proxy request: %s %s", r.Method, r.URL.Path)
		}
		if r.Method == http.MethodOptions {
			probes++
			if acknowledge {
				w.Header().Set("X-Donmai-Linear-Auth-Policy", "no-fallback-v1")
				w.Header().Set("Cache-Control", "no-store")
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		providerCalls++
		writeLinearGQLData(w, `{"issueLabels":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
	}))
	t.Cleanup(server.Close)
	run := func() (string, error) {
		root := New(func() afclient.DataSource { return afclient.NewAuthenticatedClient(server.URL, "rsk_fixture") }, "donmai")
		root.SilenceErrors = true
		var output bytes.Buffer
		root.SetOut(&output)
		root.SetErr(&output)
		root.SetArgs([]string{"list-labels"})
		err := root.Execute()
		return output.String(), err
	}
	if _, err := run(); err == nil || providerCalls != 0 || probes != 1 {
		t.Fatalf("old proxy dispatched provider call: err=%v provider=%d probes=%d", err, providerCalls, probes)
	}
	acknowledge = true
	out, err := run()
	if err != nil || strings.TrimSpace(out) != "[]" || providerCalls != 1 || probes != 2 {
		t.Fatalf("strict proxy flow: out=%q err=%v provider=%d probes=%d", out, err, providerCalls, probes)
	}
}
