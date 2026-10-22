package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// --------------------------------------------------------------------
// What an event handler's scope predicate is checked against in Jira and
// Linear when it is saved.
//
// A Jira or Linear predicate names workflow states, priorities, issue types,
// labels and label groups by the provider's id, because a person can rename
// any of them and a filter on the name would then match nothing. The id a
// caller sends must be one the editor offers: a status or an issue type of a
// Jira project the handler's team tracks, one of the Jira instance's
// priorities, or a state, a label or a label group of a Linear team the
// handler's team tracks. A name, the value a caller would naturally reach for,
// is refused rather than stored as a filter that never fires.
//
// TF keeps no mirror of these catalogs, so the check asks the provider, under
// the org's service credential, and it runs before the write's transaction:
// a provider call held inside it would hold the write's lock for as long as the
// provider takes to answer. It is scoped to what the write adds, as the
// database-backed checks are (PredicateReferenceCheck): an entry the stored
// predicate already holds is not asked about, so a write that adds no id asks
// the provider nothing, and an id that went stale upstream never blocks an edit
// that leaves it in place.
//
// Faults are 422 INVALID_FIELD on scope_predicate, one per entry, reported
// with the database-backed checks' faults from inside the transaction. The
// provider not answering establishes nothing: the write is refused as an
// upstream error rather than storing an unverified id, unless an earlier
// catalog already produced faults, which are reported instead because they are
// the caller's to fix whatever the provider says. A provider the org has not
// connected is a 409 NOT_CONFIGURED.
// --------------------------------------------------------------------

// catalogKind is the provider catalog a predicate field's entries are values
// of.
type catalogKind int

const (
	catalogJiraStatus catalogKind = iota
	catalogJiraIssueType
	catalogJiraPriority
	catalogLinearState
	catalogLinearLabel
	catalogLinearLabelGroup
)

// jiraCatalogFields and linearCatalogFields map each predicate field whose
// entries name a catalog value to the catalog, per source.
var (
	jiraCatalogFields = map[string]catalogKind{
		"status_id_in":       catalogJiraStatus,
		"new_status_id_in":   catalogJiraStatus,
		"old_status_id_in":   catalogJiraStatus,
		"issue_type_id_in":   catalogJiraIssueType,
		"priority_id_in":     catalogJiraPriority,
		"new_priority_id_in": catalogJiraPriority,
		"old_priority_id_in": catalogJiraPriority,
	}
	linearCatalogFields = map[string]catalogKind{
		"status_id_in":      catalogLinearState,
		"new_status_id_in":  catalogLinearState,
		"old_status_id_in":  catalogLinearState,
		"label_id_in":       catalogLinearLabel,
		"has_label_id_in":   catalogLinearLabel,
		"label_group_id_in": catalogLinearLabelGroup,
	}
)

// catalogField is one predicate field checked against a catalog.
type catalogField struct {
	name string
	kind catalogKind
}

// predicateCatalogFields is each Jira and Linear event type's catalog fields,
// in the order its predicate declares them. The events package registers its
// schemas in its own init, which runs before this one because this package
// imports it.
var predicateCatalogFields = map[string][]catalogField{}

func init() {
	for eventType, schema := range events.All() {
		var byName map[string]catalogKind
		switch {
		case strings.HasPrefix(eventType, "jira:"):
			byName = jiraCatalogFields
		case strings.HasPrefix(eventType, "linear:"):
			byName = linearCatalogFields
		default:
			continue
		}
		for _, f := range schema.Fields {
			if kind, ok := byName[f.Name]; ok {
				predicateCatalogFields[eventType] = append(predicateCatalogFields[eventType], catalogField{name: f.Name, kind: kind})
			}
		}
	}
}

// catalogHints is the end of each kind's fault: what the field takes, and
// where a caller finds it.
var catalogHints = map[catalogKind]string{
	catalogJiraStatus: "is not the id of a status in the workflow of a Jira project this team tracks " +
		"(it takes a status id from POST /api/orgs/{org_id}/jira/projects/{jira_id}/statuses/list, not the status name)",
	catalogJiraIssueType: "is not the id of an issue type of a Jira project this team tracks " +
		"(it takes an issue type id from POST /api/orgs/{org_id}/jira/projects/{jira_id}/issue-types/list, not the issue type name)",
	catalogJiraPriority: "is not the id of one of this workspace's Jira priorities " +
		"(it takes a priority id from POST /api/orgs/{org_id}/jira/priorities/list, not the priority name)",
	catalogLinearState: "is not the id of a workflow state of a Linear team this team tracks " +
		"(it takes a state id from POST /api/orgs/{org_id}/linear/teams/{linear_team_id}/states/list, not the state name)",
	catalogLinearLabel: "is not the id of a label an issue in a Linear team this team tracks can carry " +
		"(it takes a label id from POST /api/orgs/{org_id}/linear/teams/{linear_team_id}/labels/list, not the label name)",
	catalogLinearLabelGroup: "is not the id of a label group of a Linear team this team tracks " +
		"(it takes the id of a label POST /api/orgs/{org_id}/linear/teams/{linear_team_id}/labels/list marks is_group, not the group name)",
}

// predicateCatalogs resolves the org's service credentials the catalog check
// asks Jira and Linear through. The Server is the implementation; the check
// reads it per call, so a client resolves under whatever credential the org
// holds at the time.
type predicateCatalogs interface {
	jiraCatalogClient(ctx context.Context, orgID string) (*jira.Client, error)
	linearCatalogClient(ctx context.Context, orgID string) (*linear.Client, error)
}

func (s *Server) jiraCatalogClient(ctx context.Context, orgID string) (*jira.Client, error) {
	return s.jiraResolver.ForSystem(ctx, orgID)
}

func (s *Server) linearCatalogClient(ctx context.Context, orgID string) (*linear.Client, error) {
	return s.linearResolver.ForSystem(ctx, orgID)
}

// catalogEntry is one id a write adds to a catalog field.
type catalogEntry struct {
	field catalogField
	id    string
}

// checkPredicateCatalog checks every id predicate adds over prior to
// eventType's catalog fields against the catalogs of teamID's tracked Jira
// projects or Linear teams, before the write's transaction. It returns the
// faults, for checkPredicateReferences to report with its own, or writes the
// response and reports false when the write must not proceed.
func (eh *eventHandlersHandler) checkPredicateCatalog(w http.ResponseWriter, r *http.Request, orgID, userID, teamID, eventType string, predicate, prior *string) ([]string, bool) {
	var pending []catalogEntry
	for _, f := range predicateCatalogFields[eventType] {
		priorIn, err := predicateIDList(prior, f.name)
		if err == nil {
			var nextIn []string
			nextIn, err = predicateIDList(predicate, f.name)
			for _, id := range addedEntries(priorIn, nextIn) {
				pending = append(pending, catalogEntry{field: f, id: id})
			}
		}
		if err != nil {
			internalError(w, "event_handlers", fmt.Errorf("read %s %s: %w", eventType, f.name, err))
			return nil, false
		}
	}
	if len(pending) == 0 {
		return nil, true
	}

	c := &catalogCheck{ctx: r.Context(), catalogs: eh.catalogs, orgID: orgID, teamID: teamID}
	if err := eh.tx.WithReadTx(r.Context(), orgID, userID, c.readTracked); err != nil {
		internalError(w, "event_handlers", err)
		return nil, false
	}
	var faults []string
	for _, kind := range []catalogKind{
		catalogJiraStatus, catalogJiraIssueType, catalogJiraPriority,
		catalogLinearState, catalogLinearLabel, catalogLinearLabelGroup,
	} {
		var ids []catalogEntry
		for _, e := range pending {
			if e.field.kind == kind {
				ids = append(ids, e)
			}
		}
		if len(ids) == 0 {
			continue
		}
		known, err := c.catalog(kind)
		if err != nil {
			writeCatalogStopped(w, orgID, kind, faults, err)
			return nil, false
		}
		for _, e := range ids {
			if !known[e.id] {
				faults = append(faults, fmt.Sprintf("%s: %q %s", e.field.name, e.id, catalogHints[kind]))
			}
		}
	}
	return faults, true
}

// writeCatalogStopped ends the check when a catalog could not be read. Faults
// found in an earlier catalog are reported over the failure, as the Jira and
// Linear write gates report theirs; a provider the org has not connected is a
// 409, and any other failure an upstream error. Nothing is stored either way.
func writeCatalogStopped(w http.ResponseWriter, orgID string, kind catalogKind, faults []string, err error) {
	provider := "Jira"
	switch kind {
	case catalogLinearState, catalogLinearLabel, catalogLinearLabelGroup:
		provider = "Linear"
	}
	if len(faults) > 0 {
		serverLog.Warn("predicate catalog check could not read a catalog; reporting the faults found first",
			"org", orgID, "provider", provider, "error", err)
		items := make([]httpx.ErrorItem, 0, len(faults))
		for _, msg := range faults {
			items = append(items, httpx.ErrorItem{Reason: httpx.ReasonInvalidField, Message: msg, Field: fieldScopePredicate})
		}
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, items...)
		return
	}
	if errors.Is(err, jira.ErrNoJiraSystemCredential) || errors.Is(err, linear.ErrNoLinearSystemCredential) {
		writeNotConfigured(w, provider+" is not connected for this workspace, so the ids the scope predicate names cannot be checked")
		return
	}
	serverLog.Warn("predicate catalog check could not reach the provider", "org", orgID, "provider", provider, "error", err)
	httpx.WriteErrors(w, http.StatusBadGateway, httpx.ErrorItem{
		Reason:  httpx.ReasonUpstreamUnavailable,
		Message: "could not confirm the scope predicate's ids with " + provider + ", so nothing was saved" + httpx.LocalDetail(err),
	})
}

// catalogCheck is one check's reads: the team's tracked sources, read once,
// each provider's client, resolved at most once and only once a catalog that
// needs it is read, and each catalog it has read.
type catalogCheck struct {
	ctx      context.Context
	catalogs predicateCatalogs
	orgID    string
	teamID   string

	jiraProjects []domain.JiraProjectStatusRules
	linearTeams  []domain.LinearTeamRules

	jiraClient   *jira.Client
	linearClient *linear.Client
	read         map[catalogKind]map[string]bool
}

// readTracked reads the team's tracked Jira projects on the org's current
// Jira site and its tracked Linear teams in the org's current Linear
// workspace, under the caller's claims. An org with no Jira site has no
// tracked Jira project, and one with no Linear workspace no tracked Linear
// team.
func (c *catalogCheck) readTracked(tx db.TxStores) error {
	settings, err := tx.Orgs.GetSettings(c.ctx, c.orgID)
	if err != nil {
		return fmt.Errorf("read org settings: %w", err)
	}
	if site := domain.EntityScope("jira", settings); site != "" {
		if c.jiraProjects, err = tx.JiraStatusRules.ListForTeam(c.ctx, c.teamID, site); err != nil {
			return fmt.Errorf("read team tracked jira projects: %w", err)
		}
	}
	if c.linearTeams, err = tx.LinearTeamRules.ListForTeam(c.ctx, c.teamID, domain.EntityScope("linear", settings)); err != nil {
		return fmt.Errorf("read team tracked linear teams: %w", err)
	}
	return nil
}

// catalog returns the ids kind's catalog holds for the team. A tracked project
// or Linear team the provider answers not-found for contributes nothing.
func (c *catalogCheck) catalog(kind catalogKind) (map[string]bool, error) {
	if known, ok := c.read[kind]; ok {
		return known, nil
	}
	known := map[string]bool{}
	var err error
	switch kind {
	case catalogJiraStatus, catalogJiraIssueType:
		err = c.eachJiraProject(func(client *jira.Client, ref string) error {
			if kind == catalogJiraStatus {
				statuses, err := client.ProjectStatuses(c.ctx, ref)
				for _, st := range statuses {
					known[st.ID] = true
				}
				return err
			}
			types, err := client.ListIssueTypes(c.ctx, ref)
			for _, it := range types {
				known[it.ID] = true
			}
			return err
		})
	case catalogJiraPriority:
		var client *jira.Client
		if client, err = c.jira(); err == nil {
			var priorities []jira.Priority
			priorities, err = client.ListPriorities(c.ctx)
			for _, p := range priorities {
				known[p.ID] = true
			}
		}
	case catalogLinearState:
		err = c.eachLinearTeam(func(client *linear.Client, teamID string) error {
			states, err := client.ListWorkflowStates(c.ctx, teamID)
			for _, st := range states {
				known[st.ID] = true
			}
			return err
		})
	case catalogLinearLabel, catalogLinearLabelGroup:
		err = c.eachLinearTeam(func(client *linear.Client, teamID string) error {
			labels, err := client.ListLabels(c.ctx, teamID)
			groups := linearLabelGroupIDs(labels)
			for _, l := range labels {
				// A group is not a label an issue can carry, and a label is
				// not a group.
				if (kind == catalogLinearLabelGroup) == groups[l.ID] {
					known[l.ID] = true
				}
			}
			return err
		})
	}
	if err != nil {
		return nil, err
	}
	if c.read == nil {
		c.read = map[catalogKind]map[string]bool{}
	}
	c.read[kind] = known
	return known, nil
}

// eachJiraProject calls read for each tracked Jira project, naming it by its
// Jira id, or by its key while the registry row has none. A team that tracks
// no project resolves no credential.
func (c *catalogCheck) eachJiraProject(read func(client *jira.Client, ref string) error) error {
	if len(c.jiraProjects) == 0 {
		return nil
	}
	client, err := c.jira()
	if err != nil {
		return err
	}
	for _, p := range c.jiraProjects {
		ref := p.ProjectExternalID
		if ref == "" {
			ref = p.ProjectKey
		}
		if err := read(client, ref); err != nil && !jira.IsNotFound(err) {
			return fmt.Errorf("jira project %s: %w", ref, err)
		}
	}
	return nil
}

// eachLinearTeam calls read for each tracked Linear team. A team that tracks
// no Linear team resolves no credential.
func (c *catalogCheck) eachLinearTeam(read func(client *linear.Client, teamID string) error) error {
	if len(c.linearTeams) == 0 {
		return nil
	}
	client, err := c.linear()
	if err != nil {
		return err
	}
	for _, t := range c.linearTeams {
		if err := read(client, t.LinearTeamID); err != nil && !errors.Is(err, linear.ErrNotFound) {
			return fmt.Errorf("linear team %s: %w", t.LinearTeamID, err)
		}
	}
	return nil
}

func (c *catalogCheck) jira() (*jira.Client, error) {
	if c.jiraClient == nil {
		client, err := c.catalogs.jiraCatalogClient(c.ctx, c.orgID)
		if err != nil {
			return nil, err
		}
		c.jiraClient = client
	}
	return c.jiraClient, nil
}

func (c *catalogCheck) linear() (*linear.Client, error) {
	if c.linearClient == nil {
		client, err := c.catalogs.linearCatalogClient(c.ctx, c.orgID)
		if err != nil {
			return nil, err
		}
		c.linearClient = client
	}
	return c.linearClient, nil
}
