package linear

import "time"

// IssueState is a type alias for a Linear issue state name (e.g. "In Progress").
type IssueState = string

// Issue represents a Linear issue returned by the API.
type Issue struct {
	ID          string     `json:"id"`
	Identifier  string     `json:"identifier"`
	Title       string     `json:"title"`
	Description string     `json:"description,omitempty"`
	URL         string     `json:"url,omitempty"`
	Priority    int        `json:"priority,omitempty"`
	CreatedAt   *time.Time `json:"createdAt,omitempty"`
	UpdatedAt   *time.Time `json:"updatedAt,omitempty"`
	State       struct {
		ID   string `json:"id,omitempty"`
		Name string `json:"name"`
	} `json:"state"`
	Team struct {
		ID   string `json:"id,omitempty"`
		Key  string `json:"key,omitempty"`
		Name string `json:"name"`
	} `json:"team,omitempty"`
	Project struct {
		ID   string `json:"id,omitempty"`
		Name string `json:"name"`
	} `json:"project,omitempty"`
	Labels   []Label `json:"labels,omitempty"`
	ParentID string  `json:"parentId,omitempty"`
	Assignee *User   `json:"assignee,omitempty"`
}

// Label represents a Linear issue label.
type Label struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// User represents a Linear user.
type User struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
}

// Comment represents a Linear issue comment.
type Comment struct {
	ID        string     `json:"id"`
	Body      string     `json:"body"`
	CreatedAt *time.Time `json:"createdAt,omitempty"`
	User      *User      `json:"user,omitempty"`
}

// IssueRelation represents a relation between two Linear issues.
type IssueRelation struct {
	ID                     string     `json:"id"`
	Type                   string     `json:"type"`
	IssueID                string     `json:"issueId,omitempty"`
	IssueIdentifier        string     `json:"issueIdentifier,omitempty"`
	RelatedIssueID         string     `json:"relatedIssueId,omitempty"`
	RelatedIssueIdentifier string     `json:"relatedIssueIdentifier,omitempty"`
	CreatedAt              *time.Time `json:"createdAt,omitempty"`
}

// WorkflowState represents a Linear workflow state.
type WorkflowState struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// Project represents a Linear project.
type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Team represents a Linear team.
type Team struct {
	ID   string `json:"id"`
	Key  string `json:"key"`
	Name string `json:"name"`
}

// CreateIssueInput is the input for creating a Linear issue.
type CreateIssueInput struct {
	TeamID      string   `json:"teamId"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	StateID     string   `json:"stateId,omitempty"`
	ProjectID   string   `json:"projectId,omitempty"`
	ParentID    string   `json:"parentId,omitempty"`
	LabelIDs    []string `json:"labelIds,omitempty"`
	AssigneeID  string   `json:"assigneeId,omitempty"`
}

// UpdateIssueInput is the input for updating a Linear issue.
type UpdateIssueInput struct {
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	StateID     string   `json:"stateId,omitempty"`
	LabelIDs    []string `json:"labelIds,omitempty"`
	ParentID    *string  `json:"parentId,omitempty"` // pointer so null can be sent
	AssigneeID  string   `json:"assigneeId,omitempty"`
}

// ─── internal GraphQL wire types ────────────────────────────────────────────

// graphqlRequest is the payload sent to the Linear GraphQL endpoint.
type graphqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

// graphqlError is a single error entry in a GraphQL response.
type graphqlError struct {
	Message string `json:"message"`
}

// issueNode is the JSON structure for a single issue node inside a connection.
type issueNode struct {
	ID          string     `json:"id"`
	Identifier  string     `json:"identifier"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	URL         string     `json:"url"`
	Priority    int        `json:"priority"`
	CreatedAt   *time.Time `json:"createdAt"`
	UpdatedAt   *time.Time `json:"updatedAt"`
	State       struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"state"`
	Team struct {
		ID   string `json:"id"`
		Key  string `json:"key"`
		Name string `json:"name"`
	} `json:"team"`
	Project *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"project,omitempty"`
	Labels struct {
		Nodes []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Parent *struct {
		ID string `json:"id"`
	} `json:"parent,omitempty"`
	Assignee *struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"assignee,omitempty"`
}

// commentNode is the JSON structure for a single comment node.
type commentNode struct {
	ID        string     `json:"id"`
	Body      string     `json:"body"`
	CreatedAt *time.Time `json:"createdAt"`
	User      *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"user,omitempty"`
}

// relationNode is the JSON structure for a single relation node.
type relationNode struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Issue *struct {
		ID         string `json:"id"`
		Identifier string `json:"identifier"`
	} `json:"issue,omitempty"`
	RelatedIssue *struct {
		ID         string `json:"id"`
		Identifier string `json:"identifier"`
	} `json:"relatedIssue,omitempty"`
	CreatedAt *time.Time `json:"createdAt"`
}

// workflowStateNode is the JSON structure for a workflow state.
type workflowStateNode struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// labelNode is the JSON structure for a label.
type labelNode struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// userNode is the JSON structure for a user.
type userNode struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// teamNode is the JSON structure for a team.
type teamNode struct {
	ID   string `json:"id"`
	Key  string `json:"key"`
	Name string `json:"name"`
}

// projectNode is the JSON structure for a project.
type projectNode struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ─── GraphQL response data shapes ────────────────────────────────────────────

type listIssuesData struct {
	Issues struct {
		Nodes []issueNode `json:"nodes"`
	} `json:"issues"`
}

type getIssueData struct {
	Issue *issueNode `json:"issue"`
}

type listCommentsData struct {
	Issue struct {
		Comments struct {
			Nodes []commentNode `json:"nodes"`
		} `json:"comments"`
	} `json:"issue"`
}

type createCommentData struct {
	CommentCreate struct {
		Success bool        `json:"success"`
		Comment commentNode `json:"comment"`
	} `json:"commentCreate"`
}

type createIssueData struct {
	IssueCreate struct {
		Success bool      `json:"success"`
		Issue   issueNode `json:"issue"`
	} `json:"issueCreate"`
}

type updateIssueData struct {
	IssueUpdate struct {
		Success bool      `json:"success"`
		Issue   issueNode `json:"issue"`
	} `json:"issueUpdate"`
}

type listRelationsData struct {
	Issue struct {
		Relations struct {
			Nodes []relationNode `json:"nodes"`
		} `json:"relations"`
		InverseRelations struct {
			Nodes []relationNode `json:"nodes"`
		} `json:"inverseRelations"`
	} `json:"issue"`
}

type createRelationData struct {
	IssueRelationCreate struct {
		Success       bool         `json:"success"`
		IssueRelation relationNode `json:"issueRelation"`
	} `json:"issueRelationCreate"`
}

type deleteRelationData struct {
	IssueRelationDelete struct {
		Success bool `json:"success"`
	} `json:"issueRelationDelete"`
}

type listWorkflowStatesData struct {
	WorkflowStates struct {
		Nodes []workflowStateNode `json:"nodes"`
	} `json:"workflowStates"`
}

type listLabelsData struct {
	IssueLabels struct {
		Nodes []labelNode `json:"nodes"`
	} `json:"issueLabels"`
}

type listUsersData struct {
	Users struct {
		Nodes []userNode `json:"nodes"`
	} `json:"users"`
}

type listTeamsData struct {
	Teams struct {
		Nodes []teamNode `json:"nodes"`
	} `json:"teams"`
}

type listProjectsData struct {
	Projects struct {
		Nodes []projectNode `json:"nodes"`
	} `json:"projects"`
}

type viewerData struct {
	Viewer struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"viewer"`
}
