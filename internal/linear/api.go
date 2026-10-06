package linear

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	// maxPageSize is the largest page a caller may ask for, and the most ids
	// GetIssues takes.
	maxPageSize = 50
	// listPageSize is the page size of a listing the client walks to its end
	// itself.
	listPageSize = 100
	// maxListPages bounds such a listing, so a cursor that never reports the
	// last page cannot loop forever.
	maxListPages = 50
)

// Viewer returns the user the credential acts as. For an app install's token
// that is the app user.
func (c *Client) Viewer(ctx context.Context) (User, error) {
	var data struct {
		Viewer User `json:"viewer"`
	}
	if err := c.query(ctx, viewerQuery, nil, &data); err != nil {
		return User{}, err
	}
	return data.Viewer, nil
}

// Organization returns the workspace the credential belongs to, which is the
// only way TF learns it.
func (c *Client) Organization(ctx context.Context) (Organization, error) {
	var data struct {
		Organization Organization `json:"organization"`
	}
	if err := c.query(ctx, organizationQuery, nil, &data); err != nil {
		return Organization{}, err
	}
	return data.Organization, nil
}

// Whoami returns the viewer and its workspace in one request.
func (c *Client) Whoami(ctx context.Context) (User, Organization, error) {
	var data struct {
		Viewer       User         `json:"viewer"`
		Organization Organization `json:"organization"`
	}
	if err := c.query(ctx, whoamiQuery, nil, &data); err != nil {
		return User{}, Organization{}, err
	}
	return data.Viewer, data.Organization, nil
}

// ListTeams returns one page of the teams the credential can see, at most
// first of them, narrowed by q: a case-insensitive substring of the key or
// the name. An empty q lists every team; after is the previous page's
// EndCursor, or "" for the first page.
func (c *Client) ListTeams(ctx context.Context, q, after string, first int) (TeamPage, error) {
	if first < 1 || first > maxPageSize {
		return TeamPage{}, fmt.Errorf("linear: team page size %d out of range 1-%d", first, maxPageSize)
	}
	vars := map[string]any{"first": first, "after": nullable(after)}
	if q = strings.TrimSpace(q); q != "" {
		vars["filter"] = map[string]any{"or": []any{
			map[string]any{"key": map[string]any{"containsIgnoreCase": q}},
			map[string]any{"name": map[string]any{"containsIgnoreCase": q}},
		}}
	}
	var data struct {
		Teams struct {
			Nodes    []Team   `json:"nodes"`
			PageInfo pageInfo `json:"pageInfo"`
		} `json:"teams"`
	}
	if err := c.query(ctx, teamsQuery, vars, &data); err != nil {
		return TeamPage{}, err
	}
	return TeamPage{
		Items:       data.Teams.Nodes,
		EndCursor:   data.Teams.PageInfo.EndCursor,
		HasNextPage: data.Teams.PageInfo.HasNextPage,
	}, nil
}

// ListWorkflowStates returns every workflow state of a team, ordered by
// position.
func (c *Client) ListWorkflowStates(ctx context.Context, teamID string) ([]WorkflowState, error) {
	if err := requireID("team id", teamID); err != nil {
		return nil, err
	}
	states, err := collect(func(after string) ([]WorkflowState, pageInfo, error) {
		var data struct {
			WorkflowStates struct {
				Nodes    []WorkflowState `json:"nodes"`
				PageInfo pageInfo        `json:"pageInfo"`
			} `json:"workflowStates"`
		}
		vars := map[string]any{"teamID": teamID, "first": listPageSize, "after": nullable(after)}
		if err := c.query(ctx, workflowStatesQuery, vars, &data); err != nil {
			return nil, pageInfo{}, err
		}
		return data.WorkflowStates.Nodes, data.WorkflowStates.PageInfo, nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(states, func(i, j int) bool { return states[i].Position < states[j].Position })
	return states, nil
}

// ListLabels returns the labels an issue in a team can carry: the team's own
// and the workspace's, sorted by name.
func (c *Client) ListLabels(ctx context.Context, teamID string) ([]Label, error) {
	if err := requireID("team id", teamID); err != nil {
		return nil, err
	}
	labels, err := collect(func(after string) ([]Label, pageInfo, error) {
		var data struct {
			IssueLabels struct {
				Nodes    []Label  `json:"nodes"`
				PageInfo pageInfo `json:"pageInfo"`
			} `json:"issueLabels"`
		}
		vars := map[string]any{"teamID": teamID, "first": listPageSize, "after": nullable(after)}
		if err := c.query(ctx, labelsQuery, vars, &data); err != nil {
			return nil, pageInfo{}, err
		}
		return data.IssueLabels.Nodes, data.IssueLabels.PageInfo, nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(labels, func(i, j int) bool { return labels[i].Name < labels[j].Name })
	return labels, nil
}

// GetIssue returns one issue by its UUID or its identifier ("ENG-123"). An
// issue Linear resolves to nothing is an error matching ErrNotFound.
func (c *Client) GetIssue(ctx context.Context, idOrIdentifier string) (*Issue, error) {
	if err := requireID("issue id", idOrIdentifier); err != nil {
		return nil, err
	}
	var data struct {
		Issue *issueNode `json:"issue"`
	}
	if err := c.query(ctx, issueQuery, map[string]any{"id": idOrIdentifier}, &data); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("linear: issue %s: %w", idOrIdentifier, err)
		}
		return nil, err
	}
	if data.Issue == nil {
		return nil, fmt.Errorf("%w: issue %s", ErrNotFound, idOrIdentifier)
	}
	issues, err := c.issues(ctx, []issueNode{*data.Issue})
	if err != nil {
		return nil, err
	}
	return &issues[0], nil
}

// GetIssues returns the issues with the given UUIDs, archived ones included.
// An id that resolves to nothing is simply absent from the result. At most
// maxPageSize ids per call; the caller batches.
func (c *Client) GetIssues(ctx context.Context, ids []string) ([]Issue, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > maxPageSize {
		return nil, fmt.Errorf("linear: %d issue ids in one call, at most %d", len(ids), maxPageSize)
	}
	var data struct {
		Issues struct {
			Nodes []issueNode `json:"nodes"`
		} `json:"issues"`
	}
	if err := c.query(ctx, issuesByIDQuery, map[string]any{"ids": ids, "first": maxPageSize}, &data); err != nil {
		return nil, err
	}
	return c.issues(ctx, data.Issues.Nodes)
}

// IssueFilter is the whole vocabulary SearchIssues accepts. Each set field
// narrows the search; nothing else is expressible.
type IssueFilter struct {
	// TeamID keeps issues of one team.
	TeamID string
	// StateIDsIn keeps issues in one of these states; a non-nil empty set
	// keeps none. StateIDsNotIn drops issues in any of these states.
	StateIDsIn    []string
	StateIDsNotIn []string
	// Unassigned keeps issues with no assignee; AssigneeID keeps issues
	// assigned to that user. They are exclusive.
	Unassigned bool
	AssigneeID string
}

// render is the filter as Linear's IssueFilter input.
func (f IssueFilter) render() (map[string]any, error) {
	if f.Unassigned && f.AssigneeID != "" {
		return nil, errors.New("linear: IssueFilter sets both Unassigned and AssigneeID")
	}
	out := map[string]any{}
	if f.TeamID != "" {
		out["team"] = map[string]any{"id": map[string]any{"eq": f.TeamID}}
	}
	state := map[string]any{}
	if len(f.StateIDsIn) > 0 {
		state["in"] = f.StateIDsIn
	}
	if len(f.StateIDsNotIn) > 0 {
		state["nin"] = f.StateIDsNotIn
	}
	if len(state) > 0 {
		out["state"] = map[string]any{"id": state}
	}
	switch {
	case f.Unassigned:
		out["assignee"] = map[string]any{"null": true}
	case f.AssigneeID != "":
		out["assignee"] = map[string]any{"id": map[string]any{"eq": f.AssigneeID}}
	}
	return out, nil
}

// SearchIssues returns one page of the issues matching f, most recently
// updated first; after is the previous page's EndCursor, or "" for the first
// page.
func (c *Client) SearchIssues(ctx context.Context, f IssueFilter, after string) (IssuePage, error) {
	filter, err := f.render()
	if err != nil {
		return IssuePage{}, err
	}
	if f.StateIDsIn != nil && len(f.StateIDsIn) == 0 {
		return IssuePage{}, nil
	}
	var data struct {
		Issues struct {
			Nodes    []issueNode `json:"nodes"`
			PageInfo pageInfo    `json:"pageInfo"`
		} `json:"issues"`
	}
	vars := map[string]any{"filter": filter, "first": maxPageSize, "after": nullable(after)}
	if err := c.query(ctx, searchIssuesQuery, vars, &data); err != nil {
		return IssuePage{}, err
	}
	items, err := c.issues(ctx, data.Issues.Nodes)
	if err != nil {
		return IssuePage{}, err
	}
	return IssuePage{
		Items:       items,
		EndCursor:   data.Issues.PageInfo.EndCursor,
		HasNextPage: data.Issues.PageInfo.HasNextPage,
	}, nil
}

// ListChildren returns an issue's sub-issues and their states. It walks the
// sub-issue list alone rather than fetching the whole issue. An issue Linear
// resolves to nothing is an error matching ErrNotFound.
func (c *Client) ListChildren(ctx context.Context, idOrIdentifier string) ([]ChildIssue, error) {
	if err := requireID("issue id", idOrIdentifier); err != nil {
		return nil, err
	}
	children, err := c.issueChildren(ctx, idOrIdentifier)
	if err != nil {
		return nil, fmt.Errorf("linear: sub-issues of issue %s: %w", idOrIdentifier, err)
	}
	return children, nil
}

// AssignIssue assigns an issue to a user. UnassignIssue clears it.
func (c *Client) AssignIssue(ctx context.Context, id, assigneeID string) error {
	if err := requireID("assignee id", assigneeID); err != nil {
		return err
	}
	return c.updateIssue(ctx, id, map[string]any{"assigneeId": assigneeID})
}

// UnassignIssue clears an issue's assignee.
func (c *Client) UnassignIssue(ctx context.Context, id string) error {
	return c.updateIssue(ctx, id, map[string]any{"assigneeId": nil})
}

// TransitionIssue moves an issue to a workflow state of its team.
func (c *Client) TransitionIssue(ctx context.Context, id, stateID string) error {
	if err := requireID("state id", stateID); err != nil {
		return err
	}
	return c.updateIssue(ctx, id, map[string]any{"stateId": stateID})
}

// SetPriority sets an issue's priority, 0 (none) through 4 (low).
func (c *Client) SetPriority(ctx context.Context, id string, priority int) error {
	if err := validatePriority(priority); err != nil {
		return err
	}
	return c.updateIssue(ctx, id, map[string]any{"priority": priority})
}

// SetParent makes parentID the issue's parent; an empty parentID clears it.
func (c *Client) SetParent(ctx context.Context, id, parentID string) error {
	return c.updateIssue(ctx, id, map[string]any{"parentId": nullable(parentID)})
}

// UpdateIssueFields is the set of changes UpdateIssue makes. A nil field is
// left as it is.
type UpdateIssueFields struct {
	Title       *string
	Description *string
	Priority    *int
	// ParentID set to "" clears the parent.
	ParentID       *string
	AddLabelIDs    []string
	RemoveLabelIDs []string
}

// IsEmpty reports whether the update would change nothing.
func (u UpdateIssueFields) IsEmpty() bool {
	return u.Title == nil && u.Description == nil && u.Priority == nil && u.ParentID == nil &&
		len(u.AddLabelIDs) == 0 && len(u.RemoveLabelIDs) == 0
}

// UpdateIssue applies f to an issue.
func (c *Client) UpdateIssue(ctx context.Context, id string, f UpdateIssueFields) error {
	if f.IsEmpty() {
		return errors.New("linear: no fields to update")
	}
	input := map[string]any{}
	if f.Title != nil {
		input["title"] = *f.Title
	}
	if f.Description != nil {
		input["description"] = *f.Description
	}
	if f.Priority != nil {
		if err := validatePriority(*f.Priority); err != nil {
			return err
		}
		input["priority"] = *f.Priority
	}
	if f.ParentID != nil {
		input["parentId"] = nullable(*f.ParentID)
	}
	if len(f.AddLabelIDs) > 0 {
		input["addedLabelIds"] = f.AddLabelIDs
	}
	if len(f.RemoveLabelIDs) > 0 {
		input["removedLabelIds"] = f.RemoveLabelIDs
	}
	return c.updateIssue(ctx, id, input)
}

// updateIssue sends one issueUpdate and checks that Linear reports it done
// and answers with the issue.
func (c *Client) updateIssue(ctx context.Context, id string, input map[string]any) error {
	if err := requireID("issue id", id); err != nil {
		return err
	}
	var data struct {
		IssueUpdate struct {
			Success bool       `json:"success"`
			Issue   *issueNode `json:"issue"`
		} `json:"issueUpdate"`
	}
	if err := c.mutate(ctx, issueUpdateMutation, map[string]any{"id": id, "input": input}, &data); err != nil {
		return err
	}
	_, err := payloadIssue("issueUpdate", data.IssueUpdate.Success, data.IssueUpdate.Issue)
	return err
}

// AddComment comments on an issue and returns the comment's id. The comment
// is attributed to the credential's user.
func (c *Client) AddComment(ctx context.Context, issueID, body string) (string, error) {
	if err := requireID("issue id", issueID); err != nil {
		return "", err
	}
	if strings.TrimSpace(body) == "" {
		return "", errors.New("linear: comment body is empty")
	}
	var data struct {
		CommentCreate struct {
			Success bool `json:"success"`
			Comment *struct {
				ID string `json:"id"`
			} `json:"comment"`
		} `json:"commentCreate"`
	}
	input := map[string]any{"issueId": issueID, "body": body}
	if err := c.mutate(ctx, commentCreateMutation, map[string]any{"input": input}, &data); err != nil {
		return "", err
	}
	if !data.CommentCreate.Success {
		return "", errors.New("linear: commentCreate returned success=false")
	}
	if data.CommentCreate.Comment == nil || data.CommentCreate.Comment.ID == "" {
		return "", errors.New("linear: commentCreate returned no comment")
	}
	return data.CommentCreate.Comment.ID, nil
}

// CreateIssueInput is a new issue. TeamID and Title are required; Priority 0
// is no priority, which is also Linear's default.
type CreateIssueInput struct {
	TeamID      string
	Title       string
	Description string
	ParentID    string
	Priority    int
	LabelIDs    []string
}

// CreateIssue creates an issue and returns it as Linear stored it.
func (c *Client) CreateIssue(ctx context.Context, in CreateIssueInput) (*Issue, error) {
	if err := requireID("team id", in.TeamID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Title) == "" {
		return nil, errors.New("linear: issue title is empty")
	}
	if err := validatePriority(in.Priority); err != nil {
		return nil, err
	}
	input := map[string]any{"teamId": in.TeamID, "title": in.Title}
	if in.Description != "" {
		input["description"] = in.Description
	}
	if in.ParentID != "" {
		input["parentId"] = in.ParentID
	}
	if in.Priority != 0 {
		input["priority"] = in.Priority
	}
	if len(in.LabelIDs) > 0 {
		input["labelIds"] = in.LabelIDs
	}
	var data struct {
		IssueCreate struct {
			Success bool       `json:"success"`
			Issue   *issueNode `json:"issue"`
		} `json:"issueCreate"`
	}
	if err := c.mutate(ctx, issueCreateMutation, map[string]any{"input": input}, &data); err != nil {
		return nil, err
	}
	n, err := payloadIssue("issueCreate", data.IssueCreate.Success, data.IssueCreate.Issue)
	if err != nil {
		return nil, err
	}
	issues, err := c.issues(ctx, []issueNode{*n})
	if err != nil {
		return nil, err
	}
	return &issues[0], nil
}

// payloadIssue checks an issue mutation's payload. Linear can answer
// success=false with no errors entry, which is still a failed write.
func payloadIssue(mutation string, success bool, n *issueNode) (*issueNode, error) {
	if !success {
		return nil, fmt.Errorf("linear: %s returned success=false", mutation)
	}
	if n == nil {
		return nil, fmt.Errorf("linear: %s returned no issue", mutation)
	}
	return n, nil
}

// issues converts fragment results to Issues. An issue whose labels or
// sub-issues ran past the fragment's first page has the whole list fetched,
// so no caller sees a truncated one.
func (c *Client) issues(ctx context.Context, nodes []issueNode) ([]Issue, error) {
	out := make([]Issue, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		if n.Labels.PageInfo.HasNextPage {
			labels, err := c.issueLabels(ctx, n.ID)
			if err != nil {
				return nil, fmt.Errorf("linear: labels of issue %s: %w", n.Identifier, err)
			}
			n.Labels.Nodes = labels
		}
		if n.Children.PageInfo.HasNextPage {
			children, err := c.issueChildren(ctx, n.ID)
			if err != nil {
				return nil, fmt.Errorf("linear: sub-issues of issue %s: %w", n.Identifier, err)
			}
			n.Children.Nodes = children
		}
		out[i] = n.toIssue()
	}
	return out, nil
}

// issueLabels walks every label of one issue.
func (c *Client) issueLabels(ctx context.Context, id string) ([]labelName, error) {
	return collect(func(after string) ([]labelName, pageInfo, error) {
		var data struct {
			Issue *struct {
				Labels struct {
					Nodes    []labelName `json:"nodes"`
					PageInfo pageInfo    `json:"pageInfo"`
				} `json:"labels"`
			} `json:"issue"`
		}
		vars := map[string]any{"id": id, "first": listPageSize, "after": nullable(after)}
		if err := c.query(ctx, issueLabelsQuery, vars, &data); err != nil {
			return nil, pageInfo{}, err
		}
		if data.Issue == nil {
			return nil, pageInfo{}, ErrNotFound
		}
		return data.Issue.Labels.Nodes, data.Issue.Labels.PageInfo, nil
	})
}

// issueChildren walks every sub-issue of one issue, named by UUID or
// identifier.
func (c *Client) issueChildren(ctx context.Context, id string) ([]ChildIssue, error) {
	return collect(func(after string) ([]ChildIssue, pageInfo, error) {
		var data struct {
			Issue *struct {
				Children struct {
					Nodes    []ChildIssue `json:"nodes"`
					PageInfo pageInfo     `json:"pageInfo"`
				} `json:"children"`
			} `json:"issue"`
		}
		vars := map[string]any{"id": id, "first": listPageSize, "after": nullable(after)}
		if err := c.query(ctx, issueChildrenQuery, vars, &data); err != nil {
			return nil, pageInfo{}, err
		}
		if data.Issue == nil {
			return nil, pageInfo{}, ErrNotFound
		}
		return data.Issue.Children.Nodes, data.Issue.Children.PageInfo, nil
	})
}

// collect walks a listing to its end, one page at a time.
func collect[T any](fetch func(after string) ([]T, pageInfo, error)) ([]T, error) {
	var out []T
	after := ""
	for range maxListPages {
		items, page, err := fetch(after)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if !page.HasNextPage {
			return out, nil
		}
		if page.EndCursor == "" || page.EndCursor == after {
			return nil, errors.New("linear: listing cursor did not advance")
		}
		after = page.EndCursor
	}
	return nil, fmt.Errorf("linear: listing ran past %d pages", maxListPages)
}

// nullable is s as a GraphQL variable: null when empty.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func requireID(what, v string) error {
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("linear: %s is empty", what)
	}
	return nil
}

func validatePriority(p int) error {
	if p < 0 || p > 4 {
		return fmt.Errorf("linear: priority %d out of range 0-4", p)
	}
	return nil
}
