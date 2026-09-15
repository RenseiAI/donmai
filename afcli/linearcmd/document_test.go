package linearcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/internal/linear"
)

func cliDocumentPayload(parentType, parentID string) string {
	parents := map[string]string{"issue": `null`, "project": `null`, "team": `null`, "cycle": `null`, "initiative": `null`, "release": `null`}
	if parentType == "issue" {
		parents[parentType] = fmt.Sprintf(`{"id":%q,"identifier":"ENG-1"}`, parentID)
	} else {
		parents[parentType] = fmt.Sprintf(`{"id":%q}`, parentID)
	}
	return fmt.Sprintf(`{"documentCreate":{"success":true,"lastSyncId":4,"document":{"id":"doc-1","title":"Native","url":"https://example.test/doc-1","slugId":"native","content":"# Native\nπ","icon":null,"color":null,"sortOrder":1,"documentContentId":null,"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z","archivedAt":null,"hiddenAt":null,"trashed":false,"creator":{"id":"creator"},"updatedBy":{"id":"updater"},"owner":null,"issue":%s,"project":%s,"team":%s,"cycle":%s,"initiative":%s,"release":%s,"lastAppliedTemplate":null}}}`, parents["issue"], parents["project"], parents["team"], parents["cycle"], parents["initiative"], parents["release"])
}

func executeDocumentCmd(t *testing.T, rootArgs []string, creator DocumentCreator, ds func() afclient.DataSource) (string, error) {
	t.Helper()
	root := New(ds, "donmai", creator)
	root.SilenceErrors = true
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs(rootArgs)
	return output.String(), root.Execute()
}

func hookDocumentResult(title, parentType, parentID string) *DocumentCreateResult {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &linear.DocumentCreateResult{Success: true, LastSyncID: 8, Document: linear.Document{
		ID: "doc", Title: title, URL: "https://example.test/doc", SlugID: "doc", SortOrder: 1, CreatedAt: now, UpdatedAt: now,
		Parent: linear.DocumentParent{Type: parentType, ID: parentID},
	}}
}

func TestCreateDocumentDirectUsesNativeMutationAndIssueResolution(t *testing.T) {
	var calls []string
	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		switch {
		case strings.Contains(request.Query, "GetIssue"):
			calls = append(calls, "lookup")
			writeLinearGQLData(w, `{"issue":`+issueNodeJSON("issue-uuid", "ENG-1", "Issue", "Backlog", "team", "ENG", "Engineering")+`}`)
		case strings.Contains(request.Query, "documentCreate"):
			calls = append(calls, "create")
			if strings.Contains(request.Query, "fileUpload") || strings.Contains(request.Query, "attachmentCreate") {
				t.Fatalf("attachment mutation appeared in %s", request.Query)
			}
			input := request.Variables["input"].(map[string]any)
			if input["issueId"] != "issue-uuid" || input["content"] != "# Native\nπ" {
				t.Fatalf("input = %#v", input)
			}
			writeLinearGQLData(w, cliDocumentPayload("issue", "issue-uuid"))
		default:
			t.Fatalf("unexpected GraphQL query: %s", request.Query)
		}
	})

	path := t.TempDir() + "/document.md"
	if err := os.WriteFile(path, []byte("# Native\nπ"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runLinearCmd(t, "", "create-document", "ENG-1", "--title", "Native", "--file", path)
	if err != nil {
		t.Fatalf("create-document: %v\n%s", err, out)
	}
	if got := decodeJSON(t, out); got["success"] != true {
		t.Fatalf("output = %#v", got)
	}
	if strings.Join(calls, ",") != "lookup,create" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestCreateDocumentHookBypassesRawFactoryAndPreservesInput(t *testing.T) {
	factoryCalls := 0
	var got linear.CreateDocumentInput
	creator := CreateDocumentFunc(func(_ context.Context, input DocumentCreateInput) (*DocumentCreateResult, error) {
		got = input
		result := hookDocumentResult(input.Title, "issue", "issue-uuid")
		identifier := "ENG-9"
		result.Document.Parent.Identifier = &identifier
		return result, nil
	})
	out, err := executeDocumentCmd(t,
		[]string{"create-document", "--title", "Native", "--issue", "ENG-9", "--content", "# exact", "--no-owner", "--subscriber="},
		creator,
		func() afclient.DataSource {
			factoryCalls++
			t.Fatal("raw client factory must not run on hook path")
			return nil
		},
	)
	if err != nil {
		t.Fatalf("hook create-document: %v\n%s", err, out)
	}
	if factoryCalls != 0 || !got.IssueID.Set || got.IssueID.Value == nil || *got.IssueID.Value != "ENG-9" {
		t.Fatalf("factoryCalls=%d input=%#v", factoryCalls, got)
	}
	if !got.OwnerID.Set || got.OwnerID.Value != nil || !got.SubscriberIDs.Set || len(got.SubscriberIDs.Value) != 0 {
		t.Fatalf("omitted/null/list semantics lost: %#v", got)
	}
}

func TestCreateDocumentHookDenialNeverFallsBack(t *testing.T) {
	factoryCalls := 0
	denied := errors.New("not authorized")
	creator := CreateDocumentFunc(func(context.Context, DocumentCreateInput) (*DocumentCreateResult, error) { return nil, denied })
	_, err := executeDocumentCmd(t,
		[]string{"create-document", "--title", "Native", "--issue", "ENG-9"}, creator,
		func() afclient.DataSource { factoryCalls++; return afclient.NewMockClient() },
	)
	if !errors.Is(err, denied) {
		t.Fatalf("error = %v, want hook denial", err)
	}
	if factoryCalls != 0 {
		t.Fatalf("raw factory called %d times after hook denial", factoryCalls)
	}
}

func TestCreateDocumentHookRejectsFalseOrMalformedResult(t *testing.T) {
	validInput := []string{"create-document", "--title", "Native", "--issue", "ENG-9"}
	tests := []struct {
		name   string
		result *DocumentCreateResult
		want   string
	}{
		{"false", &linear.DocumentCreateResult{}, "not successful"},
		{"malformed", &linear.DocumentCreateResult{Success: true}, "malformed document"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			creator := CreateDocumentFunc(func(context.Context, DocumentCreateInput) (*DocumentCreateResult, error) { return tc.result, nil })
			_, err := executeDocumentCmd(t, validInput, creator, func() afclient.DataSource { t.Fatal("raw factory must not run"); return nil })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestCreateDocumentHookRejectsWrongParentType(t *testing.T) {
	creator := CreateDocumentFunc(func(_ context.Context, input DocumentCreateInput) (*DocumentCreateResult, error) {
		return hookDocumentResult(input.Title, "project", "project-ref"), nil
	})
	_, err := executeDocumentCmd(t, []string{"create-document", "--title", "Native", "--issue", "ENG-9"}, creator,
		func() afclient.DataSource { t.Fatal("raw factory must not run"); return nil })
	if err == nil || !strings.Contains(err.Error(), "does not match requested") {
		t.Fatalf("error = %v", err)
	}
}

func TestCreateDocumentHookRejectsWrongParentTarget(t *testing.T) {
	creator := CreateDocumentFunc(func(_ context.Context, input DocumentCreateInput) (*DocumentCreateResult, error) {
		return hookDocumentResult(input.Title, "project", "different-project"), nil
	})
	_, err := executeDocumentCmd(t, []string{"create-document", "--title", "Native", "--project", "project-ref"}, creator,
		func() afclient.DataSource { t.Fatal("raw factory must not run"); return nil })
	if err == nil || !strings.Contains(err.Error(), "parent id") {
		t.Fatalf("error = %v", err)
	}
}

func TestCreateDocumentHookSupportsAllParentsWithoutRawLookup(t *testing.T) {
	parents := []struct {
		flag string
		kind string
	}{
		{"issue", "issue"}, {"project", "project"}, {"team", "team"}, {"cycle", "cycle"}, {"initiative", "initiative"}, {"release", "release"},
	}
	for _, tc := range parents {
		t.Run(tc.kind, func(t *testing.T) {
			creator := CreateDocumentFunc(func(_ context.Context, input DocumentCreateInput) (*DocumentCreateResult, error) {
				return hookDocumentResult(input.Title, tc.kind, tc.kind+"-ref"), nil
			})
			_, err := executeDocumentCmd(t, []string{"create-document", "--title", "Native", "--" + tc.flag, tc.kind + "-ref"}, creator,
				func() afclient.DataSource { t.Fatal("raw factory must not run on hook path"); return nil })
			if err != nil {
				t.Fatalf("create-document: %v", err)
			}
		})
	}
}

func TestCreateDocumentRejectsConflictingContentOrParents(t *testing.T) {
	tests := [][]string{
		{"create-document", "--title", "Native", "--issue", "ENG-1", "--project", "project-1"},
		{"create-document", "--title", "Native", "--issue", "ENG-1", "--content", "inline", "--file", "file.md"},
	}
	for _, args := range tests {
		_, err := executeDocumentCmd(t, args, nil, nil)
		if err == nil {
			t.Fatalf("args %v unexpectedly succeeded", args)
		}
	}
}
