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

// LinearAssigneeNone is the LinearSearchRequest.Assignee value that keeps
// unassigned issues. Empty matches any assignee, and any other value is the
// Linear user id of the person whose issues to keep.
const LinearAssigneeNone = "none"

// LinearUserSearchMaxResults is the most users one LinearSearchUsers returns:
// one page of Linear's users listing.
const LinearUserSearchMaxResults = 50

// linearUserLookup is the verb that finds a Linear user's id, named in the
// refusals of the verbs that take one.
const linearUserLookup = "linear user search <query>"

// LinearUserMatches is what LinearSearchUsers found: the people who matched,
// and whether more matched than it returned.
type LinearUserMatches struct {
	Users     []linearclient.User `json:"users"`
	Truncated bool                `json:"truncated"`
}

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
// (a Linear user id, LinearAssigneeNone, or empty for any). Max is how many
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
// rotating refresh token. A cache starts empty in every host-CLI invocation,
// which is one process per verb, and serves the access token the install's
// envelope stores until it expires, so a verb refreshes only when no process
// has a live token. It converges with the server's and the poller's caches the
// way linearoauth.TokenCache describes.
//
// Only local mode reaches it — in multi mode the daemon is always the run's
// credential sidecar, whose verbs go through proxyCreds — and in local mode the
// credential lock is an in-process gate that takes no database. rt is the
// runtime whose live policy read gates every resolution (resolverGate).
func newLinearResolver(stores db.Stores, rt Runtime) linearclient.Resolver {
	apps := linearclient.NewOAuthAppResolver(stores.LinearApps, stores.Secrets, linearclient.DeploymentOAuthAppFromEnv())
	tokens := linearoauth.NewTokenCache(linearoauth.NewMinter(), apps, stores.Secrets, stores.LinearInstalls,
		linearoauth.NewCredentialLock(nil))
	return linearclient.NewResolverWithInstall(stores.Secrets, stores.Orgs, resolverGate(rt, eventsource.KindLinear), tokens)
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
// placeholder, and a run with no Linear proxy is refused from its manifest
// (refuseWithoutProxy). Local mode resolves the org credential through the
// secret store on the user's own machine. Either way the agent process never
// holds it.
//
// A missing credential is the unconfigured refusal; every other resolver error
// (a keychain outage, a token endpoint that is down) is the unresolved one, so
// it isn't misreported as absent.
func (c *LocalClient) linearSystemClient(ctx context.Context) (*linearclient.Client, error) {
	if err := c.requireSourceEnabled(ctx, eventsource.KindLinear); err != nil {
		return nil, err
	}
	if c.proxyCreds != nil {
		client, ok := proxyLinearClient(c.proxyCreds)
		if !ok {
			return nil, c.refuseWithoutProxy(ctx, eventsource.KindLinear)
		}
		return client, nil
	}
	if c.linearResolver == nil {
		c.linearResolver = newLinearResolver(c.stores, c.rt)
	}
	client, err := c.linearResolver.ForSystem(ctx, c.info.OrgID)
	if errors.Is(err, linearclient.ErrNoLinearSystemCredential) {
		return nil, &SourceRefusal{Kind: eventsource.KindLinear, State: domain.SourceUnconfigured}
	}
	var refusal *SourceRefusal
	if errors.As(err, &refusal) {
		return nil, err
	}
	if err != nil {
		return nil, c.refuseResolveFailure(eventsource.KindLinear, err)
	}
	return client, nil
}

func (c *LocalClient) LinearGetIssue(ctx context.Context, issue string) (_ *linearclient.Issue, err error) {
	defer c.explainSourceMiss(ctx, eventsource.KindLinear, &err)
	client, err := c.linearSystemClient(ctx)
	if err != nil {
		return nil, err
	}
	got, err := client.GetIssue(ctx, issue)
	if err != nil {
		return nil, err
	}
	// The addressed issue is the touched entity. The identifier, UUID and team
	// come off the response, since the argument may have been either key.
	c.rt.RecordReadTouch(ctx, domain.ProviderLinear, got.Identifier, got.ID, domain.EntityContainer{LinearTeamID: got.Team.ID}, domain.GitHubIdentity{}, got.URL)
	return got, nil
}

func (c *LocalClient) LinearListStates(ctx context.Context, issue string) (_ []linearclient.WorkflowState, err error) {
	defer c.explainSourceMiss(ctx, eventsource.KindLinear, &err)
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

func (c *LocalClient) LinearTransition(ctx context.Context, issue, state string) (_ linearclient.WorkflowState, err error) {
	defer c.explainSourceMiss(ctx, eventsource.KindLinear, &err)
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
	c.recordLinearIssue(ctx, got, domain.ActionIssueTransitioned, domain.DeliverableStateIssueUpdated,
		got.State.Name, target.Name, deliverableDetailsJSON(map[string]any{"state": target.Name}))
	return target, nil
}

func (c *LocalClient) LinearAddComment(ctx context.Context, issue, body string) (_ string, err error) {
	defer c.explainSourceMiss(ctx, eventsource.KindLinear, &err)
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

func (c *LocalClient) LinearAssign(ctx context.Context, issue, userID string) (_ linearclient.User, err error) {
	defer c.explainSourceMiss(ctx, eventsource.KindLinear, &err)
	if err := validateUserID("Linear", linearUserLookup, userID); err != nil {
		return linearclient.User{}, err
	}
	client, err := c.linearSystemClient(ctx)
	if err != nil {
		return linearclient.User{}, err
	}
	got, err := client.GetIssue(ctx, issue)
	if err != nil {
		return linearclient.User{}, err
	}
	user, err := findLinearUser(ctx, client, userID)
	if err != nil {
		return linearclient.User{}, err
	}
	switch {
	case user.App:
		return linearclient.User{}, fmt.Errorf("user %s (%s) is a Linear app user, which Linear does not let be assigned an issue", user.ID, user.Name)
	case !user.Active:
		return linearclient.User{}, fmt.Errorf("user %s (%s) is deactivated in Linear", user.ID, user.Name)
	}
	if err := client.AssignIssue(ctx, got.ID, user.ID); err != nil {
		return linearclient.User{}, err
	}
	c.recordLinearIssue(ctx, got, domain.ActionIssueAssigned, domain.DeliverableStateIssueUpdated, "", "",
		deliverableDetailsJSON(map[string]any{"assignee": map[string]any{"id": user.ID, "name": user.Name}}))
	return user, nil
}

func (c *LocalClient) LinearUnassign(ctx context.Context, issue string) (err error) {
	defer c.explainSourceMiss(ctx, eventsource.KindLinear, &err)
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
	c.recordLinearIssue(ctx, got, domain.ActionIssueUpdated, domain.DeliverableStateIssueUpdated, "", "",
		deliverableDetailsJSON(map[string]any{"assignee": nil}))
	return nil
}

func (c *LocalClient) LinearCreateIssue(ctx context.Context, req LinearCreateIssueRequest) (_ *linearclient.Issue, err error) {
	defer c.explainSourceMiss(ctx, eventsource.KindLinear, &err)
	if err := validateLinearTitle(req.Title); err != nil {
		return nil, err
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
	c.recordLinearIssue(ctx, created, domain.ActionIssueCreated, domain.DeliverableStateIssueCreated, "", "", "")
	return created, nil
}

func (c *LocalClient) LinearUpdateIssue(ctx context.Context, issue string, edit LinearIssueEdit) (err error) {
	defer c.explainSourceMiss(ctx, eventsource.KindLinear, &err)
	if edit.IsEmpty() {
		return errors.New("no fields to update")
	}
	if edit.Title != nil {
		if err := validateLinearTitle(*edit.Title); err != nil {
			return err
		}
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
	c.recordLinearIssue(ctx, got, domain.ActionIssueUpdated, domain.DeliverableStateIssueUpdated, "", "",
		deliverableDetailsJSON(map[string]any{"fields": linearEditedFieldNames(edit)}))
	return nil
}

func (c *LocalClient) LinearSetParent(ctx context.Context, issue, parent string) (err error) {
	defer c.explainSourceMiss(ctx, eventsource.KindLinear, &err)
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
	c.recordLinearIssue(ctx, got, domain.ActionIssueUpdated, domain.DeliverableStateIssueUpdated, "", "",
		deliverableDetailsJSON(map[string]any{"parent": parentIssue.Identifier}))
	return nil
}

func (c *LocalClient) LinearSetPriority(ctx context.Context, issue string, priority int) (err error) {
	defer c.explainSourceMiss(ctx, eventsource.KindLinear, &err)
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
	c.recordLinearIssue(ctx, got, domain.ActionIssueUpdated, domain.DeliverableStateIssueUpdated, "", "",
		deliverableDetailsJSON(map[string]any{"priority": priority}))
	return nil
}

func (c *LocalClient) LinearListChildren(ctx context.Context, issue string) (_ []linearclient.ChildIssue, err error) {
	defer c.explainSourceMiss(ctx, eventsource.KindLinear, &err)
	client, err := c.linearSystemClient(ctx)
	if err != nil {
		return nil, err
	}
	return client.ListChildren(ctx, issue)
}

func (c *LocalClient) LinearSearch(ctx context.Context, req LinearSearchRequest) (_ []linearclient.Issue, err error) {
	defer c.explainSourceMiss(ctx, eventsource.KindLinear, &err)
	if req.Max < 1 || req.Max > LinearSearchMaxResults {
		return nil, fmt.Errorf("max %d out of range 1-%d", req.Max, LinearSearchMaxResults)
	}
	if req.Assignee != "" && req.Assignee != LinearAssigneeNone {
		if err := validateUserID("Linear", linearUserLookup, req.Assignee); err != nil {
			return nil, err
		}
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
	case "":
	case LinearAssigneeNone:
		filter.Unassigned = true
	default:
		// Resolved rather than passed through, so an id that names nobody is
		// refused instead of answering with no issues.
		user, err := findLinearUser(ctx, client, req.Assignee)
		if err != nil {
			return nil, err
		}
		filter.AssigneeID = user.ID
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

// LinearSearchUsers returns the people LinearAssign can take. App users are
// left out, since Linear never lets one be assigned an issue; Truncated is
// Linear's own answer to whether its listing had a further page.
func (c *LocalClient) LinearSearchUsers(ctx context.Context, query string) (_ LinearUserMatches, err error) {
	defer c.explainSourceMiss(ctx, eventsource.KindLinear, &err)
	if strings.TrimSpace(query) == "" {
		return LinearUserMatches{}, errors.New("query is required: part of the person's name, display name or email")
	}
	client, err := c.linearSystemClient(ctx)
	if err != nil {
		return LinearUserMatches{}, err
	}
	page, err := client.ListUsers(ctx, query, "", LinearUserSearchMaxResults)
	if err != nil {
		return LinearUserMatches{}, err
	}
	people := make([]linearclient.User, 0, len(page.Items))
	for _, u := range page.Items {
		if !u.App {
			people = append(people, u)
		}
	}
	return LinearUserMatches{Users: people, Truncated: page.HasNextPage}, nil
}

// findLinearUser resolves a Linear user id to the user, refusing one that
// names nobody.
func findLinearUser(ctx context.Context, client *linearclient.Client, userID string) (linearclient.User, error) {
	user, err := client.GetUser(ctx, strings.TrimSpace(userID))
	if errors.Is(err, linearclient.ErrNotFound) {
		return linearclient.User{}, fmt.Errorf("no Linear user has the id %q; `%s` returns users' ids", userID, linearUserLookup)
	}
	return user, err
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

// validateLinearTitle is the title rule create and edit share: an issue
// always has a title, so neither route takes a blank one.
func validateLinearTitle(title string) error {
	if strings.TrimSpace(title) == "" {
		return errors.New("title must not be blank")
	}
	return nil
}

func validateLinearPriority(p int) error {
	if p < 0 || p > 4 {
		return fmt.Errorf("priority %d out of range 0-4 (0 none, 1 urgent, 2 high, 3 normal, 4 low)", p)
	}
	return nil
}

// linearEditedFieldNames lists which fields an edit touched, for the deliverable's
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

// --- linear deliverable recording ---
//
// Every Linear write an agent makes lands one `deliverables` row and its
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

// recordLinearIssue upserts the single `issue` deliverable for issue, keyed on its
// UUID (linear:issue:<uuid>) — never its identifier, which changes when the
// issue moves team or its team's key is renamed. target is the identifier and
// url the issue's link, both of which an entity rename moves. fromState /
// toState carry a transition's workflow states, empty otherwise.
func (c *LocalClient) recordLinearIssue(ctx context.Context, issue *linearclient.Issue, action, state, fromState, toState, detailsJSON string) {
	if issue == nil || issue.ID == "" {
		return
	}
	a := domain.Deliverable{
		Kind:        domain.DeliverableKindIssue,
		Target:      issue.Identifier,
		ExternalID:  issue.ID,
		URL:         issue.URL,
		State:       state,
		DedupKey:    domain.IssueObjectKey(domain.ProviderLinear, "", issue.ID),
		DetailsJSON: detailsJSON,
	}
	c.upsertLinearDeliverable(ctx, &a, linearAction(a, action, fromState, toState), issue.Team.ID)
}

// recordLinearComment upserts a `comment` deliverable keyed on the issue's UUID
// with the comment's id as its anchor (linear:comment:<issue uuid>:<comment
// id>). Its url is the issue's: the comment is reached from there.
func (c *LocalClient) recordLinearComment(ctx context.Context, issue *linearclient.Issue, commentID, body string) {
	if issue == nil || issue.ID == "" || commentID == "" {
		return
	}
	a := domain.Deliverable{
		Kind:        domain.DeliverableKindComment,
		Target:      issue.Identifier,
		ExternalID:  commentID,
		URL:         issue.URL,
		State:       domain.DeliverableStateCommentPosted,
		DedupKey:    domain.ObjectKey(domain.ProviderLinear, domain.DeliverableKindComment, issue.ID, commentID),
		DetailsJSON: deliverableDetailsJSON(map[string]any{"body": deliverableBodySnippet(body)}),
	}
	c.upsertLinearDeliverable(ctx, &a, linearAction(a, domain.ActionIssueCommentPosted, "", ""), issue.Team.ID)
}

// upsertLinearDeliverable stamps the provider onto a and writes it with its
// external-action row through the shared recording funnel (RecordExternalWrite),
// which logs and swallows a failure. linearTeamID is the team of the issue the
// write touched, which the funnel's touch creates the issue's entity with.
func (c *LocalClient) upsertLinearDeliverable(ctx context.Context, a *domain.Deliverable, act *domain.ExternalAction, linearTeamID string) {
	a.Provider = domain.ProviderLinear
	c.rt.Record(ctx, a, act, domain.EntityContainer{LinearTeamID: linearTeamID}, domain.GitHubIdentity{})
}

// linearAction builds the external-action row for a Linear write, under the
// org's Linear service identity, naming the issue or comment the deliverable
// is (issueObject).
func linearAction(a domain.Deliverable, action, from, to string) *domain.ExternalAction {
	act := &domain.ExternalAction{
		Provider:   domain.ProviderLinear,
		Action:     action,
		Target:     a.Target,
		ExternalID: a.ExternalID,
		URL:        a.URL,
		FromState:  from,
		ToState:    to,
		Credential: domain.CredentialLinearOrg,
		DetailJSON: a.DetailsJSON,
	}
	act.SetObject(issueObject(a))
	return act
}
