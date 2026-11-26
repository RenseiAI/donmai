package runner

import (
	"strings"

	"github.com/RenseiAI/donmai/runtime/workarea"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

// interactivePublicationSpec binds the disposition check to the session's
// mutable repositories. sessionDocuments is the flat checkout's own published
// pack directory (sessionDocumentPrefix), "" for every other session; it is
// set only on the flat target, the one checkout the pack can sit inside.
func interactivePublicationSpec(qw QueuedWork, sessionDocuments string) worktree.InteractivePublicationSpec {
	spec := worktree.InteractivePublicationSpec{SessionID: qw.SessionID}
	branch := trimRef(qw.Ref)
	allowRemoteHEAD := branch == ""
	if branch == "" {
		branch = strings.TrimSpace(qw.Branch)
		if branch == "" {
			branch = "agent/" + qw.SessionID
		}
	}
	if qw.RepositoryDeclaration == nil {
		name, err := workarea.RepositoryLeaf(qw.Repository)
		if err != nil {
			return spec
		}
		spec.Repositories = []worktree.PublicationTarget{{
			Name: name, Repository: qw.Repository, Branch: branch,
			AllowRemoteHEAD: allowRemoteHEAD, SessionDocuments: sessionDocuments,
		}}
		return spec
	}
	declaration, err := qw.RepositoryDeclaration.Normalize()
	if err != nil {
		return spec
	}
	for _, repository := range declaration.Repositories {
		if repository.Authority != workarea.RepositoryMutable {
			continue
		}
		spec.Repositories = append(spec.Repositories, worktree.PublicationTarget{
			Name: repository.Name, Repository: repository.Source.Repository, Branch: branch,
			AllowRemoteHEAD: allowRemoteHEAD,
		})
	}
	return spec
}
