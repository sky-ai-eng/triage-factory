package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"

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
// A Jira or Linear predicate names statuses, priorities, issue types, labels
// and label groups by the provider's id, and the id must be one the editor
// offers: a value of a Jira project or Linear team the handler's team tracks,
// or one of the Jira instance's priorities. A name is refused rather than
// stored as a filter that never fires.
//
// TF keeps no mirror of these catalogs, so the check asks the provider under
// the org's service credential, before the write's transaction so a slow
// provider never holds the write's lock. Like the database-backed checks
// (PredicateReferenceCheck), it asks only about ids the write adds, so an id
// that went stale upstream never blocks an edit that keeps it.
//
// Faults are 422 INVALID_FIELD on scope_predicate. A provider that does not
// answer establishes nothing, so the write is refused as an upstream error
// rather than storing an unverified id, unless faults were already found,
// which are the caller's to fix whatever the provider says. A provider the org
// has not connected is a 409 NOT_CONFIGURED.
// --------------------------------------------------------------------

// predicateCatalog is a provider catalog whose ids some of a source's
// predicate fields hold.
type predicateCatalog struct {
	source string
	fields []string
	// hint ends a fault: what the field takes, and where a caller lists it.
	hint string
	ids  func(c *catalogCheck) ([]string, error)
}

// predicateCatalogs is every catalog, in the order a check reads them and so
// reports their faults.
var predicateCatalogs = []*predicateCatalog{
	{
		source: "jira", fields: []string{"status_id_in", "new_status_id_in", "old_status_id_in"},
		hint: catalogHint("a status in the workflow of a Jira project", "status", "jira/projects/{jira_id}/statuses"),
		ids: func(c *catalogCheck) ([]string, error) {
			return eachTracked(c.jiraRefs, c.jira, jira.IsNotFound, func(client *jira.Client, ref string) ([]string, error) {
				statuses, err := client.ProjectStatuses(c.ctx, ref)
				return idsOf(statuses, func(s jira.Status) string { return s.ID }), err
			})
		},
	},
	{
		source: "jira", fields: []string{"issue_type_id_in"},
		hint: catalogHint("an issue type of a Jira project", "issue type", "jira/projects/{jira_id}/issue-types"),
		ids: func(c *catalogCheck) ([]string, error) {
			return eachTracked(c.jiraRefs, c.jira, jira.IsNotFound, func(client *jira.Client, ref string) ([]string, error) {
				types, err := client.ListIssueTypes(c.ctx, ref)
				return idsOf(types, func(t jira.IssueType) string { return t.ID }), err
			})
		},
	},
	{
		// Priorities are the Jira instance's, not a project's.
		source: "jira", fields: []string{"priority_id_in", "new_priority_id_in", "old_priority_id_in"},
		hint: "is not the id of one of this workspace's Jira priorities " +
			"(it takes a priority id from POST /api/orgs/{org_id}/jira/priorities/list, not the priority name)",
		ids: func(c *catalogCheck) ([]string, error) {
			client, err := c.jira()
			if err != nil {
				return nil, err
			}
			priorities, err := client.ListPriorities(c.ctx)
			return idsOf(priorities, func(p jira.Priority) string { return p.ID }), err
		},
	},
	{
		source: "linear", fields: []string{"status_id_in", "new_status_id_in", "old_status_id_in"},
		hint: catalogHint("a workflow state of a Linear team", "state", "linear/teams/{linear_team_id}/states"),
		ids: func(c *catalogCheck) ([]string, error) {
			return eachTracked(c.linearTeamIDs, c.linear, isLinearNotFound, func(client *linear.Client, teamID string) ([]string, error) {
				states, err := client.ListWorkflowStates(c.ctx, teamID)
				return idsOf(states, func(s linear.WorkflowState) string { return s.ID }), err
			})
		},
	},
	{
		source: "linear", fields: []string{"label_id_in", "has_label_id_in"},
		hint: catalogHint("a label (not a label group) of a Linear team", "label", "linear/teams/{linear_team_id}/labels"),
		ids:  func(c *catalogCheck) ([]string, error) { return c.linearLabels(false) },
	},
	{
		source: "linear", fields: []string{"label_group_id_in"},
		hint: catalogHint("a label group (a label the list marks is_group) of a Linear team", "group", "linear/teams/{linear_team_id}/labels"),
		ids:  func(c *catalogCheck) ([]string, error) { return c.linearLabels(true) },
	},
}

func catalogHint(what, noun, route string) string {
	return fmt.Sprintf("is not the id of %s this team tracks (it takes a %s id from POST /api/orgs/{org_id}/%s/list, not the %s name)",
		what, noun, route, noun)
}

func catalogFor(source, field string) *predicateCatalog {
	for _, catalog := range predicateCatalogs {
		if catalog.source == source && slices.Contains(catalog.fields, field) {
			return catalog
		}
	}
	return nil
}

func idsOf[T any](items []T, id func(T) string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, id(item))
	}
	return out
}

func isLinearNotFound(err error) bool { return errors.Is(err, linear.ErrNotFound) }

// predicateCatalogClients resolves the org's service credentials the check
// asks Jira and Linear through, per call, so it uses whatever credential the
// org holds at the time. The Server is the implementation.
type predicateCatalogClients interface {
	jiraCatalogClient(ctx context.Context, orgID string) (*jira.Client, error)
	linearCatalogClient(ctx context.Context, orgID string) (*linear.Client, error)
}

func (s *Server) jiraCatalogClient(ctx context.Context, orgID string) (*jira.Client, error) {
	return s.jiraResolver.ForSystem(ctx, orgID)
}

func (s *Server) linearCatalogClient(ctx context.Context, orgID string) (*linear.Client, error) {
	return s.linearResolver.ForSystem(ctx, orgID)
}

// checkPredicateCatalog checks every id predicate adds over prior to
// eventType's catalog fields against teamID's tracked Jira projects or Linear
// teams, before the write's transaction. It returns the faults, for
// checkPredicateReferences to report with its own, or writes the response and
// reports false when the write must not proceed.
func (eh *eventHandlersHandler) checkPredicateCatalog(w http.ResponseWriter, r *http.Request, orgID, userID, teamID, eventType string, predicate, prior *string) ([]string, bool) {
	type entry struct{ field, id string }
	source, _, _ := strings.Cut(eventType, ":")
	schema, _ := events.Get(eventType)
	added := map[*predicateCatalog][]entry{}
	for _, f := range schema.Fields {
		catalog := catalogFor(source, f.Name)
		if catalog == nil {
			continue
		}
		priorIn, err := predicateIDList(prior, f.Name)
		if err == nil {
			var nextIn []string
			nextIn, err = predicateIDList(predicate, f.Name)
			for _, id := range addedEntries(priorIn, nextIn) {
				added[catalog] = append(added[catalog], entry{f.Name, id})
			}
		}
		if err != nil {
			internalError(w, "event_handlers", fmt.Errorf("read %s %s: %w", eventType, f.Name, err))
			return nil, false
		}
	}
	if len(added) == 0 {
		return nil, true
	}

	c := newCatalogCheck(r.Context(), eh.catalogs, orgID, teamID)
	if err := eh.tx.WithReadTx(r.Context(), orgID, userID, c.readTracked); err != nil {
		internalError(w, "event_handlers", err)
		return nil, false
	}
	var faults []string
	for _, catalog := range predicateCatalogs {
		if len(added[catalog]) == 0 {
			continue
		}
		ids, err := catalog.ids(c)
		if err != nil {
			writeCatalogStopped(w, orgID, source, faults, err)
			return nil, false
		}
		for _, e := range added[catalog] {
			if !slices.Contains(ids, e.id) {
				faults = append(faults, fmt.Sprintf("%s: %q %s", e.field, e.id, catalog.hint))
			}
		}
	}
	return faults, true
}

// writeCatalogStopped ends the check when a catalog could not be read, as the
// Jira and Linear write gates do: faults already found are reported over the
// failure, a provider the org has not connected is a 409, and anything else is
// an upstream error. Nothing is stored either way.
func writeCatalogStopped(w http.ResponseWriter, orgID, source string, faults []string, err error) {
	provider := map[string]string{"jira": "Jira", "linear": "Linear"}[source]
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

// catalogCheck is one check's reads: the team's tracked sources, and each
// provider's client, resolved once and only when a catalog needs it.
type catalogCheck struct {
	ctx           context.Context
	orgID, teamID string
	// jiraRefs names each tracked Jira project by its Jira id, or by its key
	// while the registry row has none.
	jiraRefs      []string
	linearTeamIDs []string
	jira          func() (*jira.Client, error)
	linear        func() (*linear.Client, error)
}

func newCatalogCheck(ctx context.Context, clients predicateCatalogClients, orgID, teamID string) *catalogCheck {
	return &catalogCheck{
		ctx: ctx, orgID: orgID, teamID: teamID,
		jira:   sync.OnceValues(func() (*jira.Client, error) { return clients.jiraCatalogClient(ctx, orgID) }),
		linear: sync.OnceValues(func() (*linear.Client, error) { return clients.linearCatalogClient(ctx, orgID) }),
	}
}

// readTracked reads, under the caller's claims, the team's tracked Jira
// projects on the org's current Jira site and its tracked Linear teams in the
// org's current Linear workspace. An org with no Jira site tracks no project.
func (c *catalogCheck) readTracked(tx db.TxStores) error {
	settings, err := tx.Orgs.GetSettings(c.ctx, c.orgID)
	if err != nil {
		return fmt.Errorf("read org settings: %w", err)
	}
	if site := domain.EntityScope("jira", settings); site != "" {
		projects, err := tx.JiraStatusRules.ListForTeam(c.ctx, c.teamID, site)
		if err != nil {
			return fmt.Errorf("read team tracked jira projects: %w", err)
		}
		c.jiraRefs = idsOf(projects, func(p domain.JiraProjectStatusRules) string { return cmp.Or(p.ProjectExternalID, p.ProjectKey) })
	}
	teams, err := tx.LinearTeamRules.ListForTeam(c.ctx, c.teamID, domain.EntityScope("linear", settings))
	if err != nil {
		return fmt.Errorf("read team tracked linear teams: %w", err)
	}
	c.linearTeamIDs = idsOf(teams, func(t domain.LinearTeamRules) string { return t.LinearTeamID })
	return nil
}

// eachTracked collects read's ids over the tracked projects or teams refs
// names. One the provider answers not-found for contributes nothing, and no
// refs resolve no credential.
func eachTracked[C any](refs []string, client func() (C, error), notFound func(error) bool, read func(C, string) ([]string, error)) ([]string, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	cl, err := client()
	if err != nil {
		return nil, err
	}
	var all []string
	for _, ref := range refs {
		ids, err := read(cl, ref)
		if err != nil && !notFound(err) {
			return nil, fmt.Errorf("tracked %s: %w", ref, err)
		}
		all = append(all, ids...)
	}
	return all, nil
}

// linearLabels is the tracked Linear teams' label groups when groups is set,
// and their other labels otherwise: a group is not a label an issue can carry.
func (c *catalogCheck) linearLabels(groups bool) ([]string, error) {
	return eachTracked(c.linearTeamIDs, c.linear, isLinearNotFound, func(client *linear.Client, teamID string) ([]string, error) {
		labels, err := client.ListLabels(c.ctx, teamID)
		isGroup := linearLabelGroupIDs(labels)
		var ids []string
		for _, l := range labels {
			if isGroup[l.ID] == groups {
				ids = append(ids, l.ID)
			}
		}
		return ids, err
	})
}
