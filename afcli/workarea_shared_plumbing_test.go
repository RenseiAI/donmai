package afcli

import (
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
