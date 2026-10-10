package agenthost

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	linearclient "github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/linearoauth"
)

// LinearSearchMaxResults is the most issues one LinearSearch returns. A search
// walks Linear's pages of 50 until it has what was asked for, inside the
// daemon's per-call budget, so the cap bounds the pages one call can take.
const LinearSearchMaxResults = 200

// The LinearSearchRequest.Assignee values. Empty matches any assignee.
const (
	// LinearAssigneeMe keeps issues assigned to the org's Linear identity —
	// the identity every exec linear write acts as.
	LinearAssigneeMe = "me"
	// LinearAssigneeNone keeps unassigned issues.
	LinearAssigneeNone = "none"
)

// LinearCreateIssueRequest is a new issue in the agent's vocabulary: the team
// by its key, the parent by identifier or UUID, labels by name. Priority 0 is
// no priority.
type LinearCreateIssueRequest struct {
	TeamKey     string   `json:"team_key"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Parent      string   `json:"parent,omitempty"`
	Priority    int      `json:"priority,omitempty"`
	Labels      []string `json:"labels,omitempty"`
}

// LinearIssueEdit is the set of changes LinearUpdateIssue makes. A nil field is
// left as it is; labels are named.
type LinearIssueEdit struct {
	Title        *string  `json:"title,omitempty"`
	Description  *string  `json:"description,omitempty"`
	Priority     *int     `json:"priority,omitempty"`
	AddLabels    []string `json:"add_labels,omitempty"`
	RemoveLabels []string `json:"remove_labels,omitempty"`
}

// IsEmpty reports whether the edit would change nothing.
func (e LinearIssueEdit) IsEmpty() bool {
	return e.Title == nil && e.Description == nil && e.Priority == nil &&
		len(e.AddLabels) == 0 && len(e.RemoveLabels) == 0
}

// LinearSearchRequest is the whole vocabulary of LinearSearch: one team by its
// key, optionally narrowed to states of that team by name and to an assignee
// (LinearAssigneeMe, LinearAssigneeNone, or empty for any). Max is how many
// issues to return, 1 through LinearSearchMaxResults. There is deliberately no
// raw filter: linear.IssueFilter is the client's whole vocabulary, and this is
// the part of it an agent can name.
type LinearSearchRequest struct {
	TeamKey  string   `json:"team_key"`
	States   []string `json:"states,omitempty"`
	Assignee string   `json:"assignee,omitempty"`
	Max      int      `json:"max"`
}

// newLinearResolver builds the org-credential resolver the local resolver path
// reads through, serving both org credential shapes: an api_key, and an app
// install whose access token the token cache refreshes off the install's
// rotating refresh token. The cache converges with the server's and the
// poller's own caches the way linearoauth.TokenCache describes.
//
// Only local mode reaches it — in multi mode the daemon is always the run's
// credential sidecar, whose verbs go through proxyCreds — and in local mode the
// credential lock is an in-process gate that takes no database.
func newLinearResolver(stores db.Stores) linearclient.Resolver {
	apps := linearclient.NewOAuthAppResolver(stores.LinearApps, stores.Secrets, linearclient.DeploymentOAuthAppFromEnv())
	tokens := linearoauth.NewTokenCache(linearoauth.NewMinter(), apps, stores.Secrets, stores.LinearInstalls,
		linearoauth.NewCredentialLock(nil))
	return linearclient.NewResolverWithInstall(stores.Secrets, stores.Orgs, tokens)
}

// SetLinearResolver overrides the resolver this client's Linear verbs resolve
// the org credential through. Used by the daemon to share one resolver (and its
// token cache) across every request in a run, and by tests to point the client
// at a fake Linear.
func (c *LocalClient) SetLinearResolver(r linearclient.Resolver) {
	c.linearResolver = r
}

// linearSystemClient builds the org's service-identity Linear client: the
// single funnel every Linear verb resolves through, so it is where a source an
// org admin turned off is refused (requireSourceEnabled). In multi mode the
// daemon lives in the run's credential sidecar and proxyCreds is always set, so
// the client talks to the sidecar's GraphQL proxy holding only the per-run
// placeholder. Local mode resolves the org credential through the secret store
// on the user's own machine. Either way the agent process never holds it.
//
// A missing credential maps to the "not configured" guidance; every other
// resolver error (a keychain outage, a revoked install) propagates as itself so
// it isn't misreported as absent.
func (c *LocalClient) linearSystemClient(ctx context.Context) (*linearclient.Client, error) {
	if err := c.requireSourceEnabled(ctx, eventsource.KindLinear); err != nil {
		return nil, err
	}
	if c.proxyCreds != nil {
		return proxyLinearClient(c.proxyCreds)
	}
	if c.linearResolver == nil {
		c.linearResolver = newLinearResolver(c.stores)
	}
	client, err := c.linearResolver.ForSystem(ctx, c.info.OrgID)
	if errors.Is(err, linearclient.ErrNoLinearSystemCredential) {
		return nil, errLinearNotConfigured
	}
	if err != nil {
		return nil, err
	}
	return client, nil
}

func (c *LocalClient) LinearGetIssue(ctx context.Context, issue string) (*linearclient.Issue, error) {
	client, err := c.linearSystemClient(ctx)
	if err != nil {
		return nil, err
	}
	got, err := client.GetIssue(ctx, issue)
	if err != nil {
		return nil, err
	}
	// The addressed issue is the touched entity. The identifier and UUID come
	// off the response, since the argument may have been either one.
	c.rt.RecordReadTouch(ctx, domain.ArtifactProviderLinear, got.Identifier, got.ID, got.URL)
	return got, nil
}

func (c *LocalClient) LinearListStates(ctx context.Context, issue string) ([]linearclient.WorkflowState, error) {
	client, err := c.linearSystemClient(ctx)
	if err != nil {
		return nil, err
	}
	got, err := client.GetIssue(ctx, issue)
	if err != nil {
		return nil, err
	}
	return client.ListWorkflowStates(ctx, got.Team.ID)
}

func (c *LocalClient) LinearTransition(ctx context.Context, issue, state string) (linearclient.WorkflowState, error) {
	client, err := c.linearSystemClient(ctx)
	if err != nil {
		return linearclient.WorkflowState{}, err
	}
	got, err := client.GetIssue(ctx, issue)
	if err != nil {
		return linearclient.WorkflowState{}, err
	}
	states, err := client.ListWorkflowStates(ctx, got.Team.ID)
	if err != nil {
		return linearclient.WorkflowState{}, err
	}
	target, err := matchLinearState(states, state, got.Team.Key)
	if err != nil {
		return linearclient.WorkflowState{}, err
	}
	if err := client.TransitionIssue(ctx, got.ID, target.ID); err != nil {
		return linearclient.WorkflowState{}, err
	}
	c.recordLinearIssue(ctx, got, domain.ActionIssueTransitioned, domain.ArtifactStateIssueUpdated,
		got.State.Name, target.Name, artifactDetailsJSON(map[string]any{"state": target.Name}))
	return target, nil
}

func (c *LocalClient) LinearAddComment(ctx context.Context, issue, body string) (string, error) {
	client, err := c.linearSystemClient(ctx)
	if err != nil {
		return "", err
	}
	got, err := client.GetIssue(ctx, issue)
	if err != nil {
		return "", err
	}
	commentID, err := client.AddComment(ctx, got.ID, body)
	if err != nil {
		return "", err
	}
	c.recordLinearComment(ctx, got, commentID, body)
	return commentID, nil
}

func (c *LocalClient) LinearAssignSelf(ctx context.Context, issue string) error {
	client, err := c.linearSystemClient(ctx)
	if err != nil {
		return err
	}
	got, err := client.GetIssue(ctx, issue)
	if err != nil {
		return err
	}
	viewer, err := client.Viewer(ctx)
	if err != nil {
		return err
	}
	if err := client.AssignIssue(ctx, got.ID, viewer.ID); err != nil {
		return err
	}
	c.recordLinearIssue(ctx, got, domain.ActionIssueAssigned, domain.ArtifactStateIssueUpdated, "", "",
		artifactDetailsJSON(map[string]any{"assignee": "self"}))
	return nil
}

func (c *LocalClient) LinearUnassign(ctx context.Context, issue string) error {
	client, err := c.linearSystemClient(ctx)
	if err != nil {
		return err
	}
	got, err := client.GetIssue(ctx, issue)
	if err != nil {
		return err
	}
	if err := client.UnassignIssue(ctx, got.ID); err != nil {
		return err
	}
	c.recordLinearIssue(ctx, got, domain.ActionIssueUpdated, domain.ArtifactStateIssueUpdated, "", "",
		artifactDetailsJSON(map[string]any{"assignee": nil}))
	return nil
}

func (c *LocalClient) LinearCreateIssue(ctx context.Context, req LinearCreateIssueRequest) (*linearclient.Issue, error) {
	if strings.TrimSpace(req.Title) == "" {
		return nil, errors.New("title is required")
	}
	if err := validateLinearPriority(req.Priority); err != nil {
		return nil, err
	}
	client, err := c.linearSystemClient(ctx)
	if err != nil {
		return nil, err
	}
	team, err := findLinearTeam(ctx, client, req.TeamKey)
	if err != nil {
		return nil, err
	}
	in := linearclient.CreateIssueInput{
		TeamID:      team.ID,
		Title:       req.Title,
		Description: req.Description,
		Priority:    req.Priority,
	}
	if req.Parent != "" {
		parent, err := client.GetIssue(ctx, req.Parent)
		if err != nil {
			return nil, fmt.Errorf("parent %s: %w", req.Parent, err)
		}
		in.ParentID = parent.ID
	}
	if len(req.Labels) > 0 {
		in.LabelIDs, err = resolveLinearLabels(ctx, client, team, req.Labels)
		if err != nil {
			return nil, err
		}
	}
	created, err := client.CreateIssue(ctx, in)
	if err != nil {
		return nil, err
	}
	c.recordLinearIssue(ctx, created, domain.ActionIssueCreated, domain.ArtifactStateIssueCreated, "", "", "")
	return created, nil
}

func (c *LocalClient) LinearUpdateIssue(ctx context.Context, issue string, edit LinearIssueEdit) error {
	if edit.IsEmpty() {
		return errors.New("no fields to update")
	}
	if edit.Priority != nil {
		if err := validateLinearPriority(*edit.Priority); err != nil {
			return err
		}
	}
	client, err := c.linearSystemClient(ctx)
	if err != nil {
		return err
	}
	got, err := client.GetIssue(ctx, issue)
	if err != nil {
		return err
	}
	fields := linearclient.UpdateIssueFields{
		Title:       edit.Title,
		Description: edit.Description,
		Priority:    edit.Priority,
	}
	if len(edit.AddLabels) > 0 {
		if fields.AddLabelIDs, err = resolveLinearLabels(ctx, client, got.Team, edit.AddLabels); err != nil {
			return err
		}
	}
	if len(edit.RemoveLabels) > 0 {
		if fields.RemoveLabelIDs, err = resolveLinearLabels(ctx, client, got.Team, edit.RemoveLabels); err != nil {
			return err
		}
	}
	if err := client.UpdateIssue(ctx, got.ID, fields); err != nil {
		return err
	}
	c.recordLinearIssue(ctx, got, domain.ActionIssueUpdated, domain.ArtifactStateIssueUpdated, "", "",
		artifactDetailsJSON(map[string]any{"fields": linearEditedFieldNames(edit)}))
	return nil
}

func (c *LocalClient) LinearSetParent(ctx context.Context, issue, parent string) error {
	if strings.TrimSpace(parent) == "" {
		return errors.New("parent is required")
	}
	client, err := c.linearSystemClient(ctx)
	if err != nil {
		return err
	}
	got, err := client.GetIssue(ctx, issue)
	if err != nil {
		return err
	}
	parentIssue, err := client.GetIssue(ctx, parent)
	if err != nil {
		return fmt.Errorf("parent %s: %w", parent, err)
	}
	if err := client.SetParent(ctx, got.ID, parentIssue.ID); err != nil {
		return err
	}
	c.recordLinearIssue(ctx, got, domain.ActionIssueUpdated, domain.ArtifactStateIssueUpdated, "", "",
		artifactDetailsJSON(map[string]any{"parent": parentIssue.Identifier}))
	return nil
}

func (c *LocalClient) LinearSetPriority(ctx context.Context, issue string, priority int) error {
	if err := validateLinearPriority(priority); err != nil {
		return err
	}
	client, err := c.linearSystemClient(ctx)
	if err != nil {
		return err
	}
	got, err := client.GetIssue(ctx, issue)
	if err != nil {
		return err
	}
	if err := client.SetPriority(ctx, got.ID, priority); err != nil {
		return err
	}
	c.recordLinearIssue(ctx, got, domain.ActionIssueUpdated, domain.ArtifactStateIssueUpdated, "", "",
		artifactDetailsJSON(map[string]any{"priority": priority}))
	return nil
}

func (c *LocalClient) LinearListChildren(ctx context.Context, issue string) ([]linearclient.ChildIssue, error) {
	client, err := c.linearSystemClient(ctx)
	if err != nil {
		return nil, err
	}
	return client.ListChildren(ctx, issue)
}

func (c *LocalClient) LinearSearch(ctx context.Context, req LinearSearchRequest) ([]linearclient.Issue, error) {
	if req.Max < 1 || req.Max > LinearSearchMaxResults {
		return nil, fmt.Errorf("max %d out of range 1-%d", req.Max, LinearSearchMaxResults)
	}
	switch req.Assignee {
	case "", LinearAssigneeMe, LinearAssigneeNone:
	default:
		return nil, fmt.Errorf("assignee %q is not one of %s, %s", req.Assignee, LinearAssigneeMe, LinearAssigneeNone)
	}
	client, err := c.linearSystemClient(ctx)
	if err != nil {
		return nil, err
	}
	team, err := findLinearTeam(ctx, client, req.TeamKey)
	if err != nil {
		return nil, err
	}
	filter := linearclient.IssueFilter{TeamID: team.ID}
	if len(req.States) > 0 {
		states, err := client.ListWorkflowStates(ctx, team.ID)
		if err != nil {
			return nil, err
		}
		filter.StateIDsIn = make([]string, 0, len(req.States))
		for _, name := range req.States {
			s, err := matchLinearState(states, name, team.Key)
			if err != nil {
				return nil, err
			}
			filter.StateIDsIn = append(filter.StateIDsIn, s.ID)
		}
	}
	switch req.Assignee {
	case LinearAssigneeMe:
		viewer, err := client.Viewer(ctx)
		if err != nil {
			return nil, err
		}
		filter.AssigneeID = viewer.ID
	case LinearAssigneeNone:
		filter.Unassigned = true
	}

	out := []linearclient.Issue{}
	after := ""
	for len(out) < req.Max {
		page, err := client.SearchIssues(ctx, filter, after)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if !page.HasNextPage || page.EndCursor == "" || page.EndCursor == after {
			break
		}
		after = page.EndCursor
	}
	if len(out) > req.Max {
		out = out[:req.Max]
	}
	return out, nil
}

// findLinearTeam resolves a team key to the team. ListTeams narrows by a
// substring of the key or the name, so the pages are scanned for the team
// whose key is the one asked for.
func findLinearTeam(ctx context.Context, client *linearclient.Client, key string) (linearclient.Team, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return linearclient.Team{}, errors.New("team key is required")
	}
	after := ""
	for range linearTeamPages {
		page, err := client.ListTeams(ctx, key, after, 50)
		if err != nil {
			return linearclient.Team{}, err
		}
		for _, t := range page.Items {
			if strings.EqualFold(t.Key, key) {
				return t, nil
			}
		}
		if !page.HasNextPage || page.EndCursor == "" || page.EndCursor == after {
			break
		}
		after = page.EndCursor
	}
	return linearclient.Team{}, fmt.Errorf("no Linear team with key %q is visible to the org's Linear identity", key)
}

// linearTeamPages bounds findLinearTeam's scan: 500 teams whose key or name
// contains the key asked for.
const linearTeamPages = 10

// matchLinearState picks the state named by nameOrID from a team's states: an
// exact id, else a name matched case-insensitively. A miss names the team's
// states, which is what the caller needs to retry.
func matchLinearState(states []linearclient.WorkflowState, nameOrID, teamKey string) (linearclient.WorkflowState, error) {
	nameOrID = strings.TrimSpace(nameOrID)
	if nameOrID == "" {
		return linearclient.WorkflowState{}, errors.New("state is required")
	}
	for _, s := range states {
		if s.ID == nameOrID {
			return s, nil
		}
	}
	for _, s := range states {
		if strings.EqualFold(s.Name, nameOrID) {
			return s, nil
		}
	}
	names := make([]string, 0, len(states))
	for _, s := range states {
		names = append(names, s.Name)
	}
	return linearclient.WorkflowState{}, fmt.Errorf("team %s has no workflow state %q; its states are: %s",
		teamKey, nameOrID, strings.Join(names, ", "))
}

// resolveLinearLabels maps label names to the ids of the labels an issue in
// team can carry, the team's own and the workspace's. A name is matched
// case-insensitively; one that matches no label, or matches two different
// labels, is refused rather than guessed.
func resolveLinearLabels(ctx context.Context, client *linearclient.Client, team linearclient.Team, names []string) ([]string, error) {
	labels, err := client.ListLabels(ctx, team.ID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(names))
	for _, name := range names {
		var match []string
		for _, l := range labels {
			if strings.EqualFold(l.Name, strings.TrimSpace(name)) {
				match = append(match, l.ID)
			}
		}
		switch len(match) {
		case 1:
			ids = append(ids, match[0])
		case 0:
			return nil, fmt.Errorf("no label named %q is available to team %s", name, team.Key)
		default:
			return nil, fmt.Errorf("label name %q matches %d labels available to team %s; rename one in Linear to tell them apart", name, len(match), team.Key)
		}
	}
	return ids, nil
}

func validateLinearPriority(p int) error {
	if p < 0 || p > 4 {
		return fmt.Errorf("priority %d out of range 0-4 (0 none, 1 urgent, 2 high, 3 normal, 4 low)", p)
	}
	return nil
}

// linearEditedFieldNames lists which fields an edit touched, for the artifact's
// details_json — the names only, in a stable order.
func linearEditedFieldNames(e LinearIssueEdit) []string {
	names := []string{}
	if e.Title != nil {
		names = append(names, "title")
	}
	if e.Description != nil {
		names = append(names, "description")
	}
	if e.Priority != nil {
		names = append(names, "priority")
	}
	if len(e.AddLabels) > 0 {
		names = append(names, "add_labels")
	}
	if len(e.RemoveLabels) > 0 {
		names = append(names, "remove_labels")
	}
	return names
}

// --- linear artifact recording ---
//
// Every Linear write an agent makes lands one `artifacts` row and its
// external-action audit row in the same write, under the org's Linear service
// identity, exactly as the Jira writes do: issue create and the in-place
// edits collapse onto the issue's one row, and each comment is a row of its
// own. Recording is best-effort and runs after the write took effect, so it
// can never fail or roll back the action it observed. Reads record nothing.
//
// Unlike Jira, no read-back follows the write: every verb resolves the issue
// before writing (to reach its team, or to write against its UUID), so the
// UUID the rows are keyed on and the identifier they display are already in
// hand.

// recordLinearIssue upserts the single `issue` artifact for issue, keyed on its
// UUID (linear:issue:<uuid>) — never its identifier, which changes when the
// issue moves team or its team's key is renamed. target is the identifier and
// url the issue's link, both of which an entity rename moves. fromState /
// toState carry a transition's workflow states, empty otherwise.
func (c *LocalClient) recordLinearIssue(ctx context.Context, issue *linearclient.Issue, action, state, fromState, toState, detailsJSON string) {
	if issue == nil || issue.ID == "" {
		return
	}
	a := domain.Artifact{
		Kind:        domain.ArtifactKindIssue,
		Target:      issue.Identifier,
		ExternalID:  issue.ID,
		URL:         issue.URL,
		State:       state,
		DedupKey:    domain.ArtifactDedupKey(domain.ArtifactProviderLinear, domain.ArtifactKindIssue, issue.ID, ""),
		DetailsJSON: detailsJSON,
	}
	c.upsertLinearArtifact(ctx, &a, linearAction(a, action, fromState, toState))
}

// recordLinearComment upserts a `comment` artifact keyed on the issue's UUID
// with the comment's id as its anchor (linear:comment:<issue uuid>:<comment
// id>). Its url is the issue's: the comment is reached from there.
func (c *LocalClient) recordLinearComment(ctx context.Context, issue *linearclient.Issue, commentID, body string) {
	if issue == nil || issue.ID == "" || commentID == "" {
		return
	}
	a := domain.Artifact{
		Kind:        domain.ArtifactKindComment,
		Target:      issue.Identifier,
		ExternalID:  commentID,
		URL:         issue.URL,
		State:       domain.ArtifactStateCommentPosted,
		DedupKey:    domain.ArtifactDedupKey(domain.ArtifactProviderLinear, domain.ArtifactKindComment, issue.ID, commentID),
		DetailsJSON: artifactDetailsJSON(map[string]any{"body": artifactBodySnippet(body)}),
	}
	c.upsertLinearArtifact(ctx, &a, linearAction(a, domain.ActionIssueCommentPosted, "", ""))
}

// upsertLinearArtifact stamps the provider onto a and writes it with its
// external-action row through the shared recording funnel (RecordExternalWrite),
// which logs and swallows a failure.
func (c *LocalClient) upsertLinearArtifact(ctx context.Context, a *domain.Artifact, act *domain.ExternalAction) {
	a.Provider = domain.ArtifactProviderLinear
	c.rt.Record(ctx, a, act)
}

// linearAction builds the external-action row for a Linear write, under the
// org's Linear service identity.
func linearAction(a domain.Artifact, action, from, to string) *domain.ExternalAction {
	return &domain.ExternalAction{
		Provider:   domain.ArtifactProviderLinear,
		Action:     action,
		Target:     a.Target,
		ExternalID: a.ExternalID,
		URL:        a.URL,
		FromState:  from,
		ToState:    to,
		Credential: domain.CredentialLinearOrg,
		DetailJSON: a.DetailsJSON,
	}
}
