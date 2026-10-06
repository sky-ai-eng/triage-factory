package linear

import (
	"sort"
)

// User is a Linear user. For a token minted by an app install it is the app
// user: named after the app, with an email at @oauthapp.linear.app, never an
// admin.
type User struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
	Active      bool   `json:"active"`
	Admin       bool   `json:"admin"`
	Guest       bool   `json:"guest"`
	IsMe        bool   `json:"isMe"`
}

// Organization is the Linear workspace a credential belongs to.
type Organization struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	URLKey string `json:"urlKey"`
}

// Team is a Linear team, the unit whose issues TF tracks.
type Team struct {
	ID      string `json:"id"`
	Key     string `json:"key"`
	Name    string `json:"name"`
	Private bool   `json:"private"`
}

// TeamPage is one page of teams. EndCursor resumes the listing while
// HasNextPage is true.
type TeamPage struct {
	Items       []Team
	EndCursor   string
	HasNextPage bool
}

// WorkflowState is one of a team's workflow states. Type is one of triage,
// backlog, unstarted, started, completed or canceled; Position orders the
// states the way Linear's board does.
type WorkflowState struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	Position float64 `json:"position"`
}

// WorkflowStatePage is one page of a team's workflow states, in Linear's
// order. EndCursor resumes the listing while HasNextPage is true.
type WorkflowStatePage struct {
	Items       []WorkflowState
	EndCursor   string
	HasNextPage bool
}

// IssueRef names an issue by both of its ids.
type IssueRef struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
}

// CommentRef identifies a comment and when it was made.
type CommentRef struct {
	ID        string `json:"id"`
	CreatedAt string `json:"createdAt"`
}

// ChildIssue is a sub-issue and its current state.
type ChildIssue struct {
	ID         string        `json:"id"`
	Identifier string        `json:"identifier"`
	State      WorkflowState `json:"state"`
}

// Label is an issue label: a team's own, or one defined for the whole
// workspace.
type Label struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Issue is a Linear issue as the IssueFields fragment selects it.
type Issue struct {
	ID, Identifier, Title, Description, URL string

	// Priority is 0 (no priority), 1 (urgent), 2 (high), 3 (normal) or
	// 4 (low); PriorityLabel is Linear's rendering of it.
	Priority      int
	PriorityLabel string

	State    WorkflowState
	Assignee *User
	Creator  *User
	Parent   *IssueRef
	Team     Team
	// Labels are label names, sorted.
	Labels []string

	// The timestamps are RFC 3339, or "" when unset.
	CreatedAt, UpdatedAt, CompletedAt, CanceledAt, ArchivedAt string
	Trashed                                                   bool

	// LastComment is the newest comment, nil when there is none.
	LastComment *CommentRef
	Children    []ChildIssue
}

// IssuePage is one page of issues. EndCursor resumes the search while
// HasNextPage is true.
type IssuePage struct {
	Items       []Issue
	EndCursor   string
	HasNextPage bool
}

// pageInfo is a connection's cursor state.
type pageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

// issueNode is an issue as IssueFields returns it, before toIssue flattens its
// connections.
type issueNode struct {
	ID            string        `json:"id"`
	Identifier    string        `json:"identifier"`
	Title         string        `json:"title"`
	Description   string        `json:"description"`
	URL           string        `json:"url"`
	Priority      float64       `json:"priority"`
	PriorityLabel string        `json:"priorityLabel"`
	CreatedAt     string        `json:"createdAt"`
	UpdatedAt     string        `json:"updatedAt"`
	CompletedAt   string        `json:"completedAt"`
	CanceledAt    string        `json:"canceledAt"`
	ArchivedAt    string        `json:"archivedAt"`
	Trashed       bool          `json:"trashed"`
	State         WorkflowState `json:"state"`
	Assignee      *User         `json:"assignee"`
	Creator       *User         `json:"creator"`
	Parent        *IssueRef     `json:"parent"`
	Team          Team          `json:"team"`
	Labels        struct {
		Nodes    []labelName `json:"nodes"`
		PageInfo pageInfo    `json:"pageInfo"`
	} `json:"labels"`
	Comments struct {
		Nodes []CommentRef `json:"nodes"`
	} `json:"comments"`
	Children struct {
		Nodes    []ChildIssue `json:"nodes"`
		PageInfo pageInfo     `json:"pageInfo"`
	} `json:"children"`
}

type labelName struct {
	Name string `json:"name"`
}

func (n issueNode) toIssue() Issue {
	labels := make([]string, 0, len(n.Labels.Nodes))
	for _, l := range n.Labels.Nodes {
		labels = append(labels, l.Name)
	}
	sort.Strings(labels)
	var last *CommentRef
	if len(n.Comments.Nodes) > 0 {
		c := n.Comments.Nodes[len(n.Comments.Nodes)-1]
		last = &c
	}
	return Issue{
		ID:            n.ID,
		Identifier:    n.Identifier,
		Title:         n.Title,
		Description:   n.Description,
		URL:           n.URL,
		Priority:      int(n.Priority),
		PriorityLabel: n.PriorityLabel,
		State:         n.State,
		Assignee:      n.Assignee,
		Creator:       n.Creator,
		Parent:        n.Parent,
		Team:          n.Team,
		Labels:        labels,
		CreatedAt:     n.CreatedAt,
		UpdatedAt:     n.UpdatedAt,
		CompletedAt:   n.CompletedAt,
		CanceledAt:    n.CanceledAt,
		ArchivedAt:    n.ArchivedAt,
		Trashed:       n.Trashed,
		LastComment:   last,
		Children:      n.Children.Nodes,
	}
}
