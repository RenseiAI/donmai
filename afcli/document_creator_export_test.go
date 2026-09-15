package afcli_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afcli"
	"github.com/RenseiAI/donmai/afclient"
	"github.com/spf13/cobra"
)

func TestLinearDocumentCreatorPublicContract(t *testing.T) {
	issue := "ENG-1"
	input := afcli.LinearDocumentCreateInput{
		Title:   "Native",
		IssueID: afcli.LinearOptionalString{Set: true, Value: &issue},
	}
	creator := afcli.LinearDocumentCreateFunc(func(_ context.Context, got afcli.LinearDocumentCreateInput) (*afcli.LinearDocumentCreateResult, error) {
		if got.IssueID.Value == nil || *got.IssueID.Value != issue {
			t.Fatalf("input = %#v", got)
		}
		now := time.Now().UTC()
		return &afcli.LinearDocumentCreateResult{Success: true, Document: afcli.LinearDocument{
			ID: "doc", Title: got.Title, URL: "https://example.test/doc", SlugID: "doc", SortOrder: 1, CreatedAt: now, UpdatedAt: now,
			Parent: afcli.LinearDocumentParent{Type: "issue", ID: issue},
		}}, nil
	})
	root := &cobra.Command{Use: "embedded"}
	afcli.RegisterCommands(root, afcli.Config{
		ClientFactory:         func() afclient.DataSource { t.Fatal("raw client factory must not run"); return nil },
		LinearDocumentCreator: creator,
	})
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"linear", "create-document", "--title", input.Title, "--issue", issue})
	if err := root.Execute(); err != nil {
		t.Fatalf("embedded create-document: %v", err)
	}
	if output.Len() == 0 {
		t.Fatal("embedded command wrote no result")
	}
}
