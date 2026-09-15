package linear

import "time"

// OptionalString preserves Linear's three nullable-input states: omitted,
// explicit null, and a string value. Use StringValue or NullString when
// constructing a DocumentCreateInput.
type OptionalString struct {
	Set   bool
	Value *string
}

// StringValue returns an explicitly supplied string value.
func StringValue(value string) OptionalString { return OptionalString{Set: true, Value: &value} }

// NullString returns an explicitly supplied GraphQL null.
func NullString() OptionalString { return OptionalString{Set: true} }

// OptionalFloat preserves omitted, explicit null, and finite numeric input.
type OptionalFloat struct {
	Set   bool
	Value *float64
}

// FloatValue returns an explicitly supplied numeric value.
func FloatValue(value float64) OptionalFloat { return OptionalFloat{Set: true, Value: &value} }

// NullFloat returns an explicitly supplied GraphQL null.
func NullFloat() OptionalFloat { return OptionalFloat{Set: true} }

// OptionalStringSlice preserves omitted, explicit null, and explicit lists
// (including an empty list) for GraphQL list fields.
type OptionalStringSlice struct {
	Set   bool
	Value []string
}

// StringSliceValue returns an explicitly supplied string list.
func StringSliceValue(value []string) OptionalStringSlice {
	copyValue := make([]string, len(value))
	copy(copyValue, value)
	return OptionalStringSlice{Set: true, Value: copyValue}
}

// NullStringSlice returns an explicitly supplied GraphQL null.
func NullStringSlice() OptionalStringSlice { return OptionalStringSlice{Set: true} }

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
	Labels           []Label `json:"labels,omitempty"`
	ParentID         string  `json:"parentId,omitempty"`
	ParentIdentifier string  `json:"parentIdentifier,omitempty"`
	Assignee         *User   `json:"assignee,omitempty"`
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
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	SlugID   string   `json:"slugId,omitempty"`
	State    string   `json:"state"`
	TeamKeys []string `json:"teamKeys"`
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
	ProjectID   string   `json:"projectId,omitempty"`
	LabelIDs    []string `json:"labelIds,omitempty"`
	ParentID    *string  `json:"parentId,omitempty"` // pointer so null can be sent
	AssigneeID  string   `json:"assigneeId,omitempty"`
	Priority    *int     `json:"priority,omitempty"` // 0=no priority,1=urgent,2=high,3=medium,4=low
	Estimate    *int     `json:"estimate,omitempty"` // story points / t-shirt size value
}

// CreateDocumentInput is the current native Linear DocumentCreateInput. The
// optional fields deliberately retain omitted/null/value semantics; callers
// must set exactly one entity parent (IssueID, ProjectID, TeamID, CycleID,
// InitiativeID, or ReleaseID).
type CreateDocumentInput struct {
	Title                 string              `json:"title"`
	Content               OptionalString      `json:"content"`
	ID                    OptionalString      `json:"id"`
	Icon                  OptionalString      `json:"icon"`
	Color                 OptionalString      `json:"color"`
	SortOrder             OptionalFloat       `json:"sortOrder"`
	IssueID               OptionalString      `json:"issueId"`
	ProjectID             OptionalString      `json:"projectId"`
	TeamID                OptionalString      `json:"teamId"`
	CycleID               OptionalString      `json:"cycleId"`
	InitiativeID          OptionalString      `json:"initiativeId"`
	ReleaseID             OptionalString      `json:"releaseId"`
	ResourceFolderID      OptionalString      `json:"resourceFolderId"`
	LastAppliedTemplateID OptionalString      `json:"lastAppliedTemplateId"`
	SubscriberIDs         OptionalStringSlice `json:"subscriberIds"`
	OwnerID               OptionalString      `json:"ownerId"`
}

// DocumentParent is the one persisted native resource association.
type DocumentParent struct {
	Type       string  `json:"type"`
	ID         string  `json:"id"`
	Identifier *string `json:"identifier,omitempty"`
}

// Document is the normalized native Document result returned from creation.
// It excludes server-internal collaboration state and collection fields.
type Document struct {
	ID                    string         `json:"id"`
	Title                 string         `json:"title"`
	URL                   string         `json:"url"`
	SlugID                string         `json:"slugId"`
	Content               *string        `json:"content"`
	Icon                  *string        `json:"icon"`
	Color                 *string        `json:"color"`
	SortOrder             float64        `json:"sortOrder"`
	DocumentContentID     *string        `json:"documentContentId"`
	CreatedAt             time.Time      `json:"createdAt"`
	UpdatedAt             time.Time      `json:"updatedAt"`
	ArchivedAt            *time.Time     `json:"archivedAt"`
	HiddenAt              *time.Time     `json:"hiddenAt"`
	Trashed               *bool          `json:"trashed"`
	Parent                DocumentParent `json:"parent"`
	CreatorID             *string        `json:"creatorId"`
	UpdatedByID           *string        `json:"updatedById"`
	OwnerID               *string        `json:"ownerId"`
	LastAppliedTemplateID *string        `json:"lastAppliedTemplateId"`
}

// DocumentCreateResult is Linear's successful DocumentPayload normalized for
// callers. A false success or malformed payload is returned as an error.
type DocumentCreateResult struct {
	Success    bool     `json:"success"`
	LastSyncID float64  `json:"lastSyncId"`
	Document   Document `json:"document"`
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
		ID         string `json:"id"`
		Identifier string `json:"identifier"`
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
	ID    string  `json:"id"`
	Type  *string `json:"type"`
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
	ID   *string `json:"id"`
	Name *string `json:"name"`
}

// userNode is the JSON structure for a user.
type userNode struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// teamNode is the JSON structure for a team.
type teamNode struct {
	ID   *string `json:"id"`
	Key  *string `json:"key"`
	Name *string `json:"name"`
}

// projectNode is the JSON structure for a project.
type projectNode struct {
	ID     *string         `json:"id"`
	Name   *string         `json:"name"`
	SlugID *string         `json:"slugId"`
	State  *string         `json:"state"`
	Teams  *teamConnection `json:"teams"`
}

// ─── GraphQL response data shapes ────────────────────────────────────────────

type listIssuesData struct {
	Issues struct {
		Nodes []issueNode `json:"nodes"`
	} `json:"issues"`
}

type issueConnection struct {
	Nodes    *[]issueNode        `json:"nodes"`
	PageInfo *connectionPageInfo `json:"pageInfo"`
}

type paginatedListIssuesData struct {
	Issues *issueConnection `json:"issues"`
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

type documentRelationNode struct {
	ID         *string `json:"id"`
	Identifier *string `json:"identifier"`
}

type documentNode struct {
	ID                  string               `json:"id"`
	Title               string               `json:"title"`
	URL                 string               `json:"url"`
	SlugID              string               `json:"slugId"`
	Content             *string              `json:"content"`
	Icon                *string              `json:"icon"`
	Color               *string              `json:"color"`
	SortOrder           *float64             `json:"sortOrder"`
	DocumentContentID   *string              `json:"documentContentId"`
	CreatedAt           *time.Time           `json:"createdAt"`
	UpdatedAt           *time.Time           `json:"updatedAt"`
	ArchivedAt          *time.Time           `json:"archivedAt"`
	HiddenAt            *time.Time           `json:"hiddenAt"`
	Trashed             *bool                `json:"trashed"`
	Creator             documentRelationNode `json:"creator"`
	UpdatedBy           documentRelationNode `json:"updatedBy"`
	Owner               documentRelationNode `json:"owner"`
	Issue               documentRelationNode `json:"issue"`
	Project             documentRelationNode `json:"project"`
	Team                documentRelationNode `json:"team"`
	Cycle               documentRelationNode `json:"cycle"`
	Initiative          documentRelationNode `json:"initiative"`
	Release             documentRelationNode `json:"release"`
	LastAppliedTemplate documentRelationNode `json:"lastAppliedTemplate"`
}

type createDocumentData struct {
	DocumentCreate struct {
		Success    bool          `json:"success"`
		LastSyncID *float64      `json:"lastSyncId"`
		Document   *documentNode `json:"document"`
	} `json:"documentCreate"`
}

type updateIssueData struct {
	IssueUpdate struct {
		Success bool      `json:"success"`
		Issue   issueNode `json:"issue"`
	} `json:"issueUpdate"`
}

type createIssueLabelData struct {
	IssueLabelCreate struct {
		Success    bool       `json:"success"`
		IssueLabel *labelNode `json:"issueLabel"`
	} `json:"issueLabelCreate"`
}

type addIssueLabelData struct {
	IssueAddLabel struct {
		Success bool       `json:"success"`
		Issue   *issueNode `json:"issue"`
	} `json:"issueAddLabel"`
}

type connectionPageInfo struct {
	HasNextPage *bool   `json:"hasNextPage"`
	EndCursor   *string `json:"endCursor"`
}

type relationConnection struct {
	Nodes    *[]*relationNode    `json:"nodes"`
	PageInfo *connectionPageInfo `json:"pageInfo"`
}

type teamConnection struct {
	Nodes    *[]*teamNode        `json:"nodes"`
	PageInfo *connectionPageInfo `json:"pageInfo"`
}

type projectConnection struct {
	Nodes    *[]*projectNode     `json:"nodes"`
	PageInfo *connectionPageInfo `json:"pageInfo"`
}

type labelConnection struct {
	Nodes    *[]*labelNode       `json:"nodes"`
	PageInfo *connectionPageInfo `json:"pageInfo"`
}

type listRelationsData struct {
	Issue *struct {
		Relations        *relationConnection `json:"relations"`
		InverseRelations *relationConnection `json:"inverseRelations"`
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
	IssueLabels *labelConnection `json:"issueLabels"`
}

type listUsersData struct {
	Users struct {
		Nodes []userNode `json:"nodes"`
	} `json:"users"`
}

type listTeamsData struct {
	Teams *teamConnection `json:"teams"`
}

type listProjectsData struct {
	Projects *projectConnection `json:"projects"`
}

type viewerData struct {
	Viewer struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"viewer"`
}
