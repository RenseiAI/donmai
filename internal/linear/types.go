package linear

// IssueState is a type alias for a Linear issue state name (e.g. "In Progress").
type IssueState = string

// Issue represents a Linear issue returned by the API.
type Issue struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
	Title      string `json:"title"`
	State      struct {
		Name string `json:"name"`
	} `json:"state"`
	Project struct {
		Name string `json:"name"`
	} `json:"project"`
	ParentID string `json:"parentId,omitempty"`
}

// graphqlRequest is the payload sent to the Linear GraphQL endpoint.
type graphqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

// graphqlResponse is the top-level envelope returned by Linear's GraphQL API.
type graphqlResponse[T any] struct {
	Data   T               `json:"data"`
	Errors []graphqlError  `json:"errors,omitempty"`
}

// graphqlError is a single error entry in a GraphQL response.
type graphqlError struct {
	Message string `json:"message"`
}

// issueNode is the JSON structure for a single issue node inside a connection.
type issueNode struct {
	ID         string          `json:"id"`
	Identifier string          `json:"identifier"`
	Title      string          `json:"title"`
	State      struct{ Name string `json:"name"` } `json:"state"`
	Project    struct{ Name string `json:"name"` } `json:"project"`
	Parent     *struct{ ID string `json:"id"` }    `json:"parent,omitempty"`
}

// listIssuesData is the "data" field for list-issues queries.
type listIssuesData struct {
	Issues struct {
		Nodes []issueNode `json:"nodes"`
	} `json:"issues"`
}

// getIssueData is the "data" field for the GetIssue query.
type getIssueData struct {
	Issue *issueNode `json:"issue"`
}
