package linear

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func documentPayloadJSON(parentType, parentID string, success bool) string {
	parentFields := map[string]string{
		"issue": `null`, "project": `null`, "team": `null`, "cycle": `null`, "initiative": `null`, "release": `null`,
	}
	if parentType == "issue" {
		parentFields[parentType] = `{"id":"` + parentID + `","identifier":"ENG-42"}`
	} else {
		parentFields[parentType] = `{"id":"` + parentID + `"}`
	}
	return `{"documentCreate":{"success":` + map[bool]string{true: "true", false: "false"}[success] + `,"lastSyncId":12,"document":{"id":"123e4567-e89b-42d3-a456-426614174000","title":"Design","url":"https://example.test/document/doc-1","slugId":"design","content":"# heading\nπ","icon":"📄","color":"blue","sortOrder":2.5,"documentContentId":"content-1","createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-02T00:00:00Z","archivedAt":null,"hiddenAt":null,"trashed":false,"creator":{"id":"creator-1"},"updatedBy":{"id":"updater-1"},"owner":{"id":"owner-1"},"issue":` + parentFields["issue"] + `,"project":` + parentFields["project"] + `,"team":` + parentFields["team"] + `,"cycle":` + parentFields["cycle"] + `,"initiative":` + parentFields["initiative"] + `,"release":` + parentFields["release"] + `,"lastAppliedTemplate":{"id":"template-1"}}}}`
}

func TestCreateDocumentPreservesNativeFields(t *testing.T) {
	var variables map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(request.Query, "documentCreate") || strings.Contains(request.Query, "fileUpload") || strings.Contains(request.Query, "attachmentCreate") {
			t.Fatalf("native document mutation = %s", request.Query)
		}
		variables = request.Variables["input"].(map[string]any)
		writeGQLData(w, documentPayloadJSON("issue", "issue-1", true))
	})

	input := CreateDocumentInput{
		Title:                 "Design",
		Content:               StringValue("# heading\nπ"),
		ID:                    StringValue("123e4567-e89b-42d3-a456-426614174000"),
		Icon:                  StringValue("📄"),
		Color:                 StringValue("blue"),
		SortOrder:             FloatValue(2.5),
		IssueID:               StringValue("issue-1"),
		ResourceFolderID:      StringValue("folder-1"),
		LastAppliedTemplateID: StringValue("template-1"),
		SubscriberIDs:         StringSliceValue([]string{}),
		OwnerID:               NullString(),
	}
	result, err := c.CreateDocument(context.Background(), input)
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	if result.Document.URL != "https://example.test/document/doc-1" || result.Document.Parent.Type != "issue" || result.Document.Parent.ID != "issue-1" {
		t.Fatalf("result document = %#v", result.Document)
	}
	if got := variables["ownerId"]; got != nil {
		t.Fatalf("ownerId = %#v, want explicit null", got)
	}
	if got, ok := variables["subscriberIds"].([]any); !ok || len(got) != 0 {
		t.Fatalf("subscriberIds = %#v, want explicit empty list", variables["subscriberIds"])
	}
	for _, field := range []string{"title", "content", "id", "icon", "color", "sortOrder", "issueId", "resourceFolderId", "lastAppliedTemplateId"} {
		if _, ok := variables[field]; !ok {
			t.Errorf("native input omitted %q: %#v", field, variables)
		}
	}
	if _, ok := variables["projectId"]; ok {
		t.Fatalf("projectId was sent despite omission: %#v", variables)
	}
}

func TestCreateDocumentSupportsEveryNativeParent(t *testing.T) {
	parents := []struct {
		name string
		set  func(*CreateDocumentInput)
	}{
		{"issue", func(in *CreateDocumentInput) { in.IssueID = StringValue("issue-1") }},
		{"project", func(in *CreateDocumentInput) { in.ProjectID = StringValue("project-1") }},
		{"team", func(in *CreateDocumentInput) { in.TeamID = StringValue("team-1") }},
		{"cycle", func(in *CreateDocumentInput) { in.CycleID = StringValue("cycle-1") }},
		{"initiative", func(in *CreateDocumentInput) { in.InitiativeID = StringValue("initiative-1") }},
		{"release", func(in *CreateDocumentInput) { in.ReleaseID = StringValue("release-1") }},
	}
	for _, tc := range parents {
		t.Run(tc.name, func(t *testing.T) {
			var variables map[string]any
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Variables map[string]any `json:"variables"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				variables = request.Variables["input"].(map[string]any)
				writeGQLData(w, documentPayloadJSON(tc.name, tc.name+"-1", true))
			})
			input := CreateDocumentInput{Title: "Design"}
			tc.set(&input)
			result, err := c.CreateDocument(context.Background(), input)
			if err != nil {
				t.Fatalf("CreateDocument: %v", err)
			}
			if result.Document.Parent.Type != tc.name {
				t.Fatalf("parent = %#v", result.Document.Parent)
			}
			if got := variables[tc.name+"Id"]; got != tc.name+"-1" {
				t.Fatalf("%sId = %#v, want %q", tc.name, got, tc.name+"-1")
			}
		})
	}
}

func TestCreateDocumentRejectsFalseOrMalformedPayload(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{"false", documentPayloadJSON("issue", "issue-1", false), "not successful"},
		{"missing URL", `{"documentCreate":{"success":true,"lastSyncId":1,"document":{"id":"doc","title":"x"}}}`, "required document fields"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { writeGQLData(w, tc.data) })
			_, err := c.CreateDocument(context.Background(), CreateDocumentInput{Title: "x", IssueID: StringValue("issue-1")})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CreateDocument error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestCreateDocumentRejectsInvalidParentsBeforeRequest(t *testing.T) {
	requests := 0
	c, _ := newTestClient(t, func(_ http.ResponseWriter, _ *http.Request) { requests++ })
	_, err := c.CreateDocument(context.Background(), CreateDocumentInput{Title: "x", IssueID: StringValue("issue"), ProjectID: StringValue("project")})
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("CreateDocument error = %v", err)
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want 0", requests)
	}
}

func TestCreateDocumentRejectsInvalidClientIDBeforeRequest(t *testing.T) {
	requests := 0
	c, _ := newTestClient(t, func(_ http.ResponseWriter, _ *http.Request) { requests++ })
	_, err := c.CreateDocument(context.Background(), CreateDocumentInput{Title: "x", IssueID: StringValue("issue"), ID: StringValue("not-a-v4")})
	if err == nil || !strings.Contains(err.Error(), "UUID v4") {
		t.Fatalf("CreateDocument error = %v", err)
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want 0", requests)
	}
}

func TestCreateDocumentDoesNotRetryAmbiguousResponses(t *testing.T) {
	statuses := []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, http.StatusOK}
	for _, status := range statuses {
		t.Run(http.StatusText(status), func(t *testing.T) {
			requests := 0
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				requests++
				w.WriteHeader(status)
				if status == http.StatusOK {
					_, _ = w.Write([]byte(`not-json`))
				}
			})
			_, err := c.CreateDocument(context.Background(), CreateDocumentInput{Title: "x", IssueID: StringValue("issue")})
			if err == nil {
				t.Fatal("CreateDocument unexpectedly succeeded")
			}
			if requests != 1 {
				t.Fatalf("requests = %d, want one write attempt", requests)
			}
		})
	}
}

func TestCreateDocumentDoesNotRetryTransportError(t *testing.T) {
	attempts := 0
	c := &Client{
		BaseURL: "https://example.test/graphql",
		APIKey:  "fixture",
		HTTPClient: &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			attempts++
			return nil, errors.New("ambiguous transport failure")
		})},
	}
	_, err := c.CreateDocument(context.Background(), CreateDocumentInput{Title: "x", IssueID: StringValue("issue")})
	if err == nil || !strings.Contains(err.Error(), "request failed") {
		t.Fatalf("CreateDocument error = %v", err)
	}
	if attempts != 1 {
		t.Fatalf("transport attempts = %d, want one", attempts)
	}
}

func TestCreateDocumentProxiedDoesNotRetryAmbiguousResponse(t *testing.T) {
	requests := 0
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.Header.Get("Authorization"); got != "Bearer test-fixture-key-not-a-secret" {
			t.Fatalf("Authorization = %q", got)
		}
		w.WriteHeader(http.StatusBadGateway)
	})
	c.ProxyMode = true
	_, err := c.CreateDocument(context.Background(), CreateDocumentInput{Title: "x", IssueID: StringValue("issue")})
	if err == nil {
		t.Fatal("CreateDocument unexpectedly succeeded")
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want one proxied write attempt", requests)
	}
}

func TestCreateDocumentRejectsMismatchedClientIDOrParent(t *testing.T) {
	tests := []struct {
		name  string
		input CreateDocumentInput
		want  string
	}{
		{"client ID", CreateDocumentInput{Title: "x", ID: StringValue("123e4567-e89b-42d3-a456-426614174001"), IssueID: StringValue("issue-1")}, "document id"},
		{"issue identifier", CreateDocumentInput{Title: "x", IssueID: StringValue("ENG-999")}, "document parent id"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				writeGQLData(w, documentPayloadJSON("issue", "issue-1", true))
			})
			_, err := c.CreateDocument(context.Background(), tc.input)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CreateDocument error = %v, want %q", err, tc.want)
			}
		})
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
