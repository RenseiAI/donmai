package runner

import (
	"strings"

	"github.com/RenseiAI/donmai/runtime/workarea"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

func interactivePublicationSpec(qw QueuedWork) worktree.InteractivePublicationSpec {
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
			AllowRemoteHEAD: allowRemoteHEAD,
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
