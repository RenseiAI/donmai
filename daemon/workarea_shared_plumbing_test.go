package daemon

import (
	"testing"

	"github.com/RenseiAI/donmai/runtime/workarea"
)

func TestSharedWorkareaFieldsSurvivePollSpecAndDetailProjection(t *testing.T) {
	filter := &workarea.RepositoryFilter{Kind: workarea.RepositoryFilterNamed, Name: "context"}
	declaration := &workarea.RepositoryDeclarationV1{
		Protocol: workarea.ProtocolSessionRootV1,
		Repositories: []workarea.DeclaredRepositoryV1{{
			Source: workarea.RepositorySource{Repository: "repo", Ref: "main"},
			Name:   "repo", Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable,
		}},
	}
	item := PollWorkItem{
		SessionID: "child", Repository: "repo", RepositoryDeclaration: declaration,
		WorkareaMode: "shared", ParentWorkareaID: "wa_parent", RepositoryFilter: filter, CacheSeedID: "seed-one",
	}
	spec := PollItemToSessionSpec(item, nil)
	detail := PollItemToSessionDetail(item, nil, "", "", "")
	if spec.WorkareaMode != "shared" || spec.ParentWorkareaID != "wa_parent" || spec.RepositoryFilter != filter || spec.CacheSeedID != "seed-one" || spec.RepositoryDeclaration != declaration {
		t.Fatalf("shared SessionSpec projection = %+v", spec)
	}
	if detail.WorkareaMode != "shared" || detail.ParentWorkareaID != "wa_parent" || detail.RepositoryFilter != filter || detail.CacheSeedID != "seed-one" || detail.RepositoryDeclaration != declaration {
		t.Fatalf("shared SessionDetail projection = %+v", detail)
	}
}
