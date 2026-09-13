package runner

import (
	"testing"

	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

func TestInteractivePublicationSpecUsesOnlyAdmittedMutableRepositories(t *testing.T) {
	queued := QueuedWork{
		QueuedWork: prompt.QueuedWork{
			SessionID: "99999999-9999-4999-8999-999999999999",
		},
		Branch: "agent/admitted",
		RepositoryDeclaration: &workarea.RepositoryDeclarationV1{
			Protocol: workarea.ProtocolSessionRootV1,
			Repositories: []workarea.DeclaredRepositoryV1{
				{
					Name: "primary", Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable,
					Source: workarea.RepositorySource{Repository: "https://example.invalid/primary.git"},
				},
				{
					Name: "secondary", Role: workarea.RepositoryRoleSecondary, Authority: workarea.RepositoryMutable,
					Source: workarea.RepositorySource{Repository: "ssh://git@example.invalid/secondary.git"},
				},
				{
					Name: "context", Role: workarea.RepositoryRoleContext, Authority: workarea.RepositoryReadOnly,
					Source: workarea.RepositorySource{Repository: "https://example.invalid/context.git"},
				},
			},
		},
	}

	spec := interactivePublicationSpec(queued)
	if spec.SessionID != queued.SessionID || len(spec.Repositories) != 2 {
		t.Fatalf("publication spec=%+v", spec)
	}
	for index, want := range []struct {
		name       string
		repository string
	}{
		{"primary", "https://example.invalid/primary.git"},
		{"secondary", "ssh://git@example.invalid/secondary.git"},
	} {
		got := spec.Repositories[index]
		if got.Name != want.name || got.Repository != want.repository || got.Branch != "agent/admitted" || !got.AllowRemoteHEAD {
			t.Fatalf("repository[%d]=%+v", index, got)
		}
	}
}
