package afcli

import (
	"encoding/json"
	"testing"

	"github.com/RenseiAI/donmai/daemon"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

func TestSharedWorkareaFieldsSurviveSessionDetailToRunner(t *testing.T) {
	filter := &workarea.RepositoryFilter{Kind: workarea.RepositoryFilterNamed, Name: "context"}
	detail := &daemon.SessionDetail{
		SessionID: "child", WorkareaMode: "shared", ParentWorkareaID: "wa_parent",
		RepositoryFilter: filter, CacheSeedID: "seed-one",
	}
	queued, err := detailToQueuedWork(detail)
	if err != nil {
		t.Fatal(err)
	}
	if queued.WorkareaMode != "shared" || queued.ParentWorkareaID != "wa_parent" || queued.RepositoryFilter != filter || queued.CacheSeedID != "seed-one" {
		t.Fatalf("runner shared projection = %+v", queued)
	}
}

func TestReceiptedWorkareaIntentCannotBeOverwrittenByCompatibilityMirror(t *testing.T) {
	filter := &workarea.RepositoryFilter{Kind: workarea.RepositoryFilterNamed, Name: "context"}
	declaration := &workarea.RepositoryDeclarationV1{
		Protocol: workarea.ProtocolSessionRootV1,
		Repositories: []workarea.DeclaredRepositoryV1{{
			Source: workarea.RepositorySource{Repository: "repo", Ref: "main"}, Name: "repo",
			Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable,
		}},
	}
	payload, err := json.Marshal(map[string]any{
		"sessionId": "child", "repositoryDeclaration": declaration, "workareaMode": "shared",
		"parentWorkareaId": "wa_parent", "repositoryFilter": filter, "cacheSeedId": "seed-one",
	})
	if err != nil {
		t.Fatal(err)
	}
	detail := &daemon.SessionDetail{
		SessionID: "child", OperationalPayload: payload, RepositoryDeclaration: declaration,
		WorkareaMode: "shared", ParentWorkareaID: "wa_parent", RepositoryFilter: filter, CacheSeedID: "seed-one",
	}
	queued, err := detailToQueuedWork(detail)
	if err != nil || queued.ParentWorkareaID != "wa_parent" || queued.RepositoryFilter == nil || queued.RepositoryFilter.Name != "context" {
		t.Fatalf("matching receipted intent = %+v, %v", queued, err)
	}
	detail.ParentWorkareaID = "wa_other"
	if _, err := detailToQueuedWork(detail); err == nil {
		t.Fatal("unreceipted compatibility mirror overwrote parentWorkareaId")
	}
}
