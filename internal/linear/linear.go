package linear

import "context"

// Linear is the interface satisfied by Client; inject this for testability.
type Linear interface {
	ListIssuesByProject(ctx context.Context, projectName string, states []string) ([]Issue, error)
	GetIssue(ctx context.Context, id string) (*Issue, error)
	ListSubIssues(ctx context.Context, parentID string) ([]Issue, error)
}
