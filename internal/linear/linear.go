package linear

import "context"

// Linear is the interface satisfied by Client; inject this for testability.
type Linear interface {
	// Read operations
	GetIssue(ctx context.Context, id string) (*Issue, error)
	ListIssuesByProject(ctx context.Context, projectName string, states []string) ([]Issue, error)
	ListIssues(ctx context.Context, filter map[string]any, limit int, orderBy string) ([]Issue, error)
	ListSubIssues(ctx context.Context, parentID string) ([]Issue, error)
	ListBacklogIssues(ctx context.Context, projectID string) ([]Issue, error)
	GetIssueComments(ctx context.Context, issueID string) ([]Comment, error)
	GetIssueRelations(ctx context.Context, issueID string) (*RelationsResult, error)
	ListWorkflowStates(ctx context.Context, teamID string) (map[string]string, error)
	ListLabels(ctx context.Context) (map[string]string, error)
	GetTeamByName(ctx context.Context, nameOrKey string) (*Team, error)
	GetProjectByName(ctx context.Context, name string) (*Project, error)
	GetUserByNameOrEmail(ctx context.Context, nameOrEmail string) (*User, error)
	GetViewer(ctx context.Context) (*User, error)

	// Write operations
	CreateIssue(ctx context.Context, input CreateIssueInput) (*Issue, error)
	UpdateIssue(ctx context.Context, id string, input UpdateIssueInput) (*Issue, error)
	CreateComment(ctx context.Context, issueID, body string) (*Comment, error)
	CreateRelation(ctx context.Context, issueID, relatedIssueID, relationType string) (string, bool, error)
	DeleteRelation(ctx context.Context, relationID string) error
}
