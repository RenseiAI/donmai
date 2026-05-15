package github

import "context"

// GitHub is the interface satisfied by Client; inject this for testability.
type GitHub interface {
	// Issues — read
	GetIssue(ctx context.Context, owner, repo string, number int) (*Issue, error)
	ListIssues(ctx context.Context, owner, repo string, opts ListIssuesOptions) ([]Issue, error)
	ListIssueComments(ctx context.Context, owner, repo string, number int) ([]Comment, error)

	// Issues — write
	CreateIssue(ctx context.Context, owner, repo string, input CreateIssueInput) (*Issue, error)
	UpdateIssue(ctx context.Context, owner, repo string, number int, input UpdateIssueInput) (*Issue, error)
	CreateIssueComment(ctx context.Context, owner, repo string, number int, body string) (*Comment, error)
	AddLabels(ctx context.Context, owner, repo string, number int, labels []string) ([]Label, error)
	SetAssignees(ctx context.Context, owner, repo string, number int, assignees []string) (*Issue, error)

	// Repos
	GetRepo(ctx context.Context, owner, repo string) (*Repo, error)
	ListLabels(ctx context.Context, owner, repo string) ([]Label, error)

	// Users
	GetAuthenticatedUser(ctx context.Context) (*User, error)
}
