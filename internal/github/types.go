package github

import "time"

// Issue represents a GitHub issue.
type Issue struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	State     string    `json:"state"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Labels    []Label   `json:"labels"`
	Assignees []User    `json:"assignees"`
	User      *User     `json:"user"`
	Milestone *Milestone `json:"milestone"`
	Comments  int       `json:"comments"`
}

// Comment represents a GitHub issue comment.
type Comment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	User      *User     `json:"user"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Label represents a GitHub label.
type Label struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

// User represents a GitHub user.
type User struct {
	Login     string `json:"login"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	HTMLURL   string `json:"html_url"`
	AvatarURL string `json:"avatar_url"`
}

// Repo represents a GitHub repository.
type Repo struct {
	FullName    string `json:"full_name"`
	Name        string `json:"name"`
	Description string `json:"description"`
	HTMLURL     string `json:"html_url"`
	Private     bool   `json:"private"`
	OpenIssues  int    `json:"open_issues_count"`
}

// Milestone represents a GitHub milestone.
type Milestone struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
}

// CreateIssueInput holds the fields for creating a new issue.
type CreateIssueInput struct {
	Title     string   `json:"title"`
	Body      string   `json:"body,omitempty"`
	Labels    []string `json:"labels,omitempty"`
	Assignees []string `json:"assignees,omitempty"`
	Milestone *int     `json:"milestone,omitempty"`
}

// UpdateIssueInput holds the fields that can be updated on an issue.
// Only non-zero / non-nil fields are sent.
type UpdateIssueInput struct {
	Title     string   `json:"title,omitempty"`
	Body      string   `json:"body,omitempty"`
	State     string   `json:"state,omitempty"` // "open" or "closed"
	Labels    []string `json:"labels,omitempty"`
	Assignees []string `json:"assignees,omitempty"`
	Milestone *int     `json:"milestone,omitempty"`
}

// ListIssuesOptions filters for listing issues.
type ListIssuesOptions struct {
	State     string // "open", "closed", "all" (default: "open")
	Labels    string // comma-separated label names
	Assignee  string // username or "none" or "*"
	Creator   string // username
	Milestone string // milestone number or "*" or "none"
	Sort      string // "created", "updated", "comments" (default: "created")
	Direction string // "asc", "desc" (default: "desc")
	Since     string // ISO 8601 timestamp
	PerPage   int    // max items (default: 30, max: 100)
	Page      int
}
