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

	// A declared layout publishes the pack beside its leaves, so no
	// repository target ever carries the session document directory.
	spec := interactivePublicationSpec(queued, ".workarea/context/pack1/")
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
		if got.Name != want.name || got.Repository != want.repository || got.Branch != "agent/admitted" || !got.AllowRemoteHEAD || got.SessionDocuments != "" {
			t.Fatalf("repository[%d]=%+v", index, got)
		}
	}
}

// TestInteractivePublicationSpecCarriesFlatSessionDocuments pins the flat
// target: the one checkout a published pack can sit inside carries its exact
// directory, and a session without one carries nothing.
func TestInteractivePublicationSpecCarriesFlatSessionDocuments(t *testing.T) {
	queued := QueuedWork{
		QueuedWork: prompt.QueuedWork{SessionID: "99999999-9999-4999-8999-999999999999"},
	}
	queued.Repository = "https://example.invalid/flat.git"
	for _, want := range []string{"", ".workarea/context/pack1/"} {
		spec := interactivePublicationSpec(queued, want)
		if len(spec.Repositories) != 1 || spec.Repositories[0].SessionDocuments != want {
			t.Fatalf("publication spec for %q = %+v", want, spec)
		}
	}
}
