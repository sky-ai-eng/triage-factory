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

// Comment is a comment on an issue: its id, when it was made and who wrote it.
// User is nil for a comment an integration posted, which BotActor names
// instead.
type Comment struct {
	ID        string    `json:"id"`
	CreatedAt string    `json:"createdAt"`
	User      *User     `json:"user"`
	BotActor  *BotActor `json:"botActor"`
}

// BotActor is the integration that posted a comment no user wrote.
type BotActor struct {
	Name string `json:"name"`
}

// ChildIssue is a sub-issue and its current state.
type ChildIssue struct {
	ID         string        `json:"id"`
	Identifier string        `json:"identifier"`
	State      WorkflowState `json:"state"`
}

// Label is an issue label: a team's own, or one defined for the whole
// workspace. Name is the label's own name, without its group's.
type Label struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Group is the name of the label group the label sits in, "" when it is
	// in none. Only ListLabels reads it; an issue's labels leave it empty.
	Group string `json:"group,omitempty"`
}

// Issue is a Linear issue as the IssueFields fragment selects it. Its JSON
// names are Linear's own field names, like the types it is built from, since
// an agent reads it as the output of an exec verb.
type Issue struct {
	ID          string `json:"id"`
	Identifier  string `json:"identifier"`
	Title       string `json:"title"`
	Description string `json:"description"`
	URL         string `json:"url"`

	// Priority is 0 (no priority), 1 (urgent), 2 (high), 3 (normal) or
	// 4 (low); PriorityLabel is Linear's rendering of it.
	Priority      int    `json:"priority"`
	PriorityLabel string `json:"priorityLabel"`

	State    WorkflowState `json:"state"`
	Assignee *User         `json:"assignee"`
	Creator  *User         `json:"creator"`
	Parent   *IssueRef     `json:"parent"`
	Team     Team          `json:"team"`
	// Labels are sorted by name.
	Labels []Label `json:"labels"`

	// The timestamps are RFC 3339, or "" when unset.
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
	CompletedAt string `json:"completedAt,omitempty"`
	CanceledAt  string `json:"canceledAt,omitempty"`
	ArchivedAt  string `json:"archivedAt,omitempty"`
	Trashed     bool   `json:"trashed"`

	// LastComment is the newest comment, nil when there is none.
	LastComment *Comment     `json:"lastComment"`
	Children    []ChildIssue `json:"children"`
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
		Nodes    []labelNode `json:"nodes"`
		PageInfo pageInfo    `json:"pageInfo"`
	} `json:"labels"`
	Comments struct {
		Nodes []Comment `json:"nodes"`
	} `json:"comments"`
	Children struct {
		Nodes    []ChildIssue `json:"nodes"`
		PageInfo pageInfo     `json:"pageInfo"`
	} `json:"children"`
}

// labelNode is a label as a document selects it. parent is selected only by
// labelsQuery.
type labelNode struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Parent *struct {
		Name string `json:"name"`
	} `json:"parent"`
}

func (n labelNode) toLabel() Label {
	l := Label{ID: n.ID, Name: n.Name}
	if n.Parent != nil {
		l.Group = n.Parent.Name
	}
	return l
}

// sortedLabels converts label nodes to Labels sorted by name, then id, so two
// labels sharing a name across groups still sort the same way every time.
func sortedLabels(nodes []labelNode) []Label {
	labels := make([]Label, 0, len(nodes))
	for _, n := range nodes {
		labels = append(labels, n.toLabel())
	}
	sort.Slice(labels, func(i, j int) bool {
		if labels[i].Name != labels[j].Name {
			return labels[i].Name < labels[j].Name
		}
		return labels[i].ID < labels[j].ID
	})
	return labels
}

func (n issueNode) toIssue() Issue {
	labels := sortedLabels(n.Labels.Nodes)
	var last *Comment
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
