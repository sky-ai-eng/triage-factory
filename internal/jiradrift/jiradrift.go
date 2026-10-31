// Package jiradrift finds Jira configuration that names values Jira no longer
// has, and tells the admins of the team that owns it.
//
// Two kinds of configuration name Jira values, and they drift apart from Jira
// in different ways:
//
//   - A team's status rules for a project (jira_project_status_rules) name the
//     statuses of the project's workflow. A status deleted in Jira, or retired
//     from the workflow, leaves a rule naming nothing. That is
//     kinds.JiraStatusDrift, one occurrence per rule row.
//   - A Jira task rule's or trigger's filter names statuses, issue types and
//     priorities by id, each checked when the save adds it. A value deleted in
//     Jira, dropped from a workflow, or left behind on a project the team no
//     longer tracks leaves a filter that never matches it. That is
//     kinds.JiraPredicateDrift, one occurrence per handler.
//
// Both are matched by what TF stores, never by a name a person typed: a rule's
// refs compare as domain.JiraStatusRef.SameStatus does (by id, by name only for
// a ref that has no id), and a filter's ids by id. A rename in Jira therefore
// drifts nothing. A value is gone when the catalog it was chosen from no
// longer has it, which is the same question the save asks: the project's
// workflow for a rule, the statuses and issue types of every project the
// handler's team tracks, and the instance's priorities.
//
// The checks read Jira under the org's service credential. A catalog that
// cannot be read is no evidence about the values it holds, so whatever depends
// on it neither raises nor resolves.
package jiradrift

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

var driftLog = logging.Component("jiradrift")

// Catalog is the Jira reads drift is checked against. *jira.Client is one.
type Catalog interface {
	ProjectStatuses(ctx context.Context, idOrKey string) ([]jira.Status, error)
	ListIssueTypes(ctx context.Context, idOrKey string) ([]jira.IssueType, error)
	ListPriorities(ctx context.Context) ([]jira.Priority, error)
}

// EventTypes returns every Jira event type whose filter can name catalog
// values: the event types whose handlers Handlers needs.
func EventTypes() []string {
	var out []string
	for _, et := range domain.AllEventTypes() {
		if et.Source == "jira" && len(catalogFields(et.ID)) > 0 {
			out = append(out, et.ID)
		}
	}
	return out
}

// catalogFields is eventType's predicate fields that name catalog values.
func catalogFields(eventType string) []string {
	schema, ok := events.Get(eventType)
	if !ok {
		return nil
	}
	var out []string
	for _, f := range schema.Fields {
		if _, ok := events.JiraCatalogOf(f.Name); ok {
			out = append(out, f.Name)
		}
	}
	return out
}

// Pass is one look at the org's Jira catalogs, each read at most once however
// many rules and handlers need it. It stops asking once Jira fails in a way
// that is not about one request (a rate limit, an outage, a refused
// credential), so a host that is refusing requests is not sent more of them;
// everything the pass has not read by then is unanswered.
type Pass struct {
	c          Catalog
	stopped    bool
	statuses   map[string]catalogRead
	issueTypes map[string]catalogRead
	priorities *catalogRead
}

// catalogRead is one catalog as Jira answered it. ok is false when Jira did
// not answer, or answered with nothing at all, which is not evidence that any
// value is gone either.
type catalogRead struct {
	refs []domain.JiraStatusRef // statuses only
	ids  map[string]bool
	ok   bool
}

// NewPass starts a pass that reads through c.
func NewPass(c Catalog) *Pass {
	return &Pass{c: c, statuses: map[string]catalogRead{}, issueTypes: map[string]catalogRead{}}
}

// projectRef names a rule's project to Jira: by its id once the registry has
// resolved it, else by its key.
func projectRef(r domain.JiraProjectStatusRules) string {
	return cmp.Or(r.ProjectExternalID, r.ProjectKey)
}

func (p *Pass) workflow(ctx context.Context, ref string) catalogRead {
	if got, ok := p.statuses[ref]; ok {
		return got
	}
	var got catalogRead
	if !p.stopped && ref != "" {
		statuses, err := p.c.ProjectStatuses(ctx, ref)
		if p.answered(ctx, err, "statuses", ref) && len(statuses) > 0 {
			got = catalogRead{ok: true, ids: map[string]bool{}}
			for _, s := range statuses {
				got.refs = append(got.refs, domain.JiraStatusRef{ID: s.ID, Name: s.Name})
				got.ids[s.ID] = true
			}
		}
	}
	p.statuses[ref] = got
	return got
}

func (p *Pass) projectIssueTypes(ctx context.Context, ref string) catalogRead {
	if got, ok := p.issueTypes[ref]; ok {
		return got
	}
	var got catalogRead
	if !p.stopped && ref != "" {
		types, err := p.c.ListIssueTypes(ctx, ref)
		if p.answered(ctx, err, "issue types", ref) && len(types) > 0 {
			got = catalogRead{ok: true, ids: map[string]bool{}}
			for _, t := range types {
				got.ids[t.ID] = true
			}
		}
	}
	p.issueTypes[ref] = got
	return got
}

func (p *Pass) instancePriorities(ctx context.Context) catalogRead {
	if p.priorities != nil {
		return *p.priorities
	}
	var got catalogRead
	if !p.stopped {
		priorities, err := p.c.ListPriorities(ctx)
		if p.answered(ctx, err, "priorities", "") && len(priorities) > 0 {
			got = catalogRead{ok: true, ids: map[string]bool{}}
			for _, pr := range priorities {
				got.ids[pr.ID] = true
			}
		}
	}
	p.priorities = &got
	return got
}

// answered reports whether a read returned, and stops the pass on a failure
// that is about Jira rather than this one request. Rejected is the one class
// about the request: a project Jira will not describe, which says nothing
// about the next one.
func (p *Pass) answered(ctx context.Context, err error, what, project string) bool {
	if err == nil {
		return true
	}
	class, _ := upstream.ClassOf(err)
	if class != upstream.Rejected || errors.Is(err, context.Canceled) || ctx.Err() != nil {
		p.stopped = true
	}
	driftLog.Log(ctx, upstream.LogLevel(err, slog.LevelWarn), "jira drift: catalog read failed; leaving what depends on it as it is",
		"catalog", what, "project", project, "error", err)
	return false
}

// RuleStatuses returns rule's occurrence of kinds.JiraStatusDrift against the
// statuses its project's workflow has, and whether the rule names any status
// that workflow does not. A canonical is one of its rule's members, so it is
// named once.
//
// Stopped is set when a rule Triage Factory acts on keeps none of its
// statuses, or loses the status it moves tickets into: pickup finds new work,
// in progress and done classify and receive tickets. In review feeds nothing
// TF polls or writes, so its drift is never what stops anything.
func RuleStatuses(rule domain.JiraProjectStatusRules, live []domain.JiraStatusRef) (kinds.JiraStatusRule, bool) {
	out := kinds.JiraStatusRule{TeamID: rule.TeamID, JiraProjectID: rule.JiraProjectID, ProjectKey: rule.ProjectKey}
	seen := map[string]bool{}
	gone := func(ref domain.JiraStatusRef) bool {
		if ref.IsZero() || domain.ContainsStatus(live, ref) {
			return false
		}
		if key := domain.JiraStatusDedupKey(ref); !seen[key] {
			seen[key] = true
			out.Statuses = append(out.Statuses, cmp.Or(ref.Name, ref.ID))
		}
		return true
	}
	for _, r := range []struct {
		members   []domain.JiraStatusRef
		canonical domain.JiraStatusRef
		acts      bool
	}{
		{rule.PickupMembers, domain.JiraStatusRef{}, true},
		{rule.InProgressMembers, rule.InProgressCanonical, true},
		{rule.InReviewMembers, rule.InReviewCanonical, false},
		{rule.DoneMembers, rule.DoneCanonical, true},
	} {
		kept := 0
		for _, m := range r.members {
			if !gone(m) {
				kept++
			}
		}
		if gone(r.canonical) && r.acts {
			out.Stopped = true
		}
		if r.acts && len(r.members) > 0 && kept == 0 {
			out.Stopped = true
		}
	}
	return out, len(out.Statuses) > 0
}

// Rules checks every rule in rules, which is every team's status rules on the
// org's Jira site, and syncs kinds.JiraStatusDrift over them: a rule naming a
// status its workflow does not have is raised, one whose workflow could not be
// read is kept, and every other open occurrence, including a rule that no
// longer exists, resolves.
func (p *Pass) Rules(ctx context.Context, orgID string, rules []domain.JiraProjectStatusRules) {
	var drifted, keep []kinds.JiraStatusRule
	for _, r := range rules {
		live := p.workflow(ctx, projectRef(r))
		if !live.ok {
			keep = append(keep, kinds.JiraStatusRule{TeamID: r.TeamID, JiraProjectID: r.JiraProjectID})
			continue
		}
		if d, ok := RuleStatuses(r, live.refs); ok {
			drifted = append(drifted, d)
		}
	}
	kinds.JiraStatusDrift.Sync(ctx, orgID, drifted, keep...)
}

// Handlers checks every handler in handlers, which is every enabled handler on
// EventTypes in the org, and syncs kinds.JiraPredicateDrift over them. rules
// is every team's status rules on the org's Jira site, which is what each
// team tracks. A handler whose catalogs could not all be read is kept, and
// every other open occurrence, including a handler deleted or disabled since,
// resolves.
func (p *Pass) Handlers(ctx context.Context, orgID string, rules []domain.JiraProjectStatusRules, handlers []domain.EventHandler) {
	tracked := map[string][]string{}
	for _, r := range rules {
		if ref := projectRef(r); !slices.Contains(tracked[r.TeamID], ref) {
			tracked[r.TeamID] = append(tracked[r.TeamID], ref)
		}
	}
	var drifted, keep []kinds.JiraHandlerFilter
	for _, h := range handlers {
		d, gone, known := p.handler(ctx, h, tracked[h.TeamID])
		switch {
		case !known:
			keep = append(keep, kinds.JiraHandlerFilter{HandlerID: h.ID})
		case gone:
			drifted = append(drifted, d)
		}
	}
	kinds.JiraPredicateDrift.Sync(ctx, orgID, drifted, keep...)
}

// Handler checks one handler and sets its occurrence, for a save that changed
// it; teamRules is its team's status rules on the org's Jira site. A handler
// whose catalogs could not all be read is left as it is, for the next pass.
func (p *Pass) Handler(ctx context.Context, orgID string, h domain.EventHandler, teamRules []domain.JiraProjectStatusRules) {
	var refs []string
	for _, r := range teamRules {
		if ref := projectRef(r); !slices.Contains(refs, ref) {
			refs = append(refs, ref)
		}
	}
	d, gone, known := p.handler(ctx, h, refs)
	if known {
		kinds.JiraPredicateDrift.Set(ctx, orgID, d, gone)
	}
}

// handler is h's occurrence against the catalogs of projects, its team's
// tracked projects by Jira ref, whether its filter names anything gone, and
// whether the catalogs that decide it could all be read.
//
// A handler that is disabled, is not on a Jira event, or belongs to a team
// tracking no project names nothing gone: a disabled handler matches nothing
// by choice, and a team tracking no project receives no Jira event to match.
func (p *Pass) handler(ctx context.Context, h domain.EventHandler, projects []string) (kinds.JiraHandlerFilter, bool, bool) {
	out := kinds.JiraHandlerFilter{HandlerID: h.ID, TeamID: h.TeamID, HandlerKind: h.Kind, RuleName: h.Name, EventType: h.EventType}
	fields := catalogFields(h.EventType)
	if !h.Enabled || h.ScopePredicateJSON == nil || len(fields) == 0 || len(projects) == 0 {
		return out, false, true
	}
	var predicate map[string]json.RawMessage
	if err := json.Unmarshal([]byte(*h.ScopePredicateJSON), &predicate); err != nil {
		driftLog.ErrorContext(ctx, "jira drift: a stored filter does not decode; leaving it as it is", "handler", h.ID, "error", err)
		return out, false, false
	}
	known := true
	for _, field := range fields {
		var ids []string
		if raw, ok := predicate[field]; ok {
			if err := json.Unmarshal(raw, &ids); err != nil {
				driftLog.ErrorContext(ctx, "jira drift: a stored filter field does not decode; leaving the filter as it is",
					"handler", h.ID, "field", field, "error", err)
				return out, false, false
			}
		}
		if len(ids) == 0 {
			continue
		}
		catalog, _ := events.JiraCatalogOf(field)
		has, ok := p.catalog(ctx, catalog, projects)
		if !ok {
			known = false
			continue
		}
		var gone []string
		for _, id := range ids {
			if !has[id] {
				gone = append(gone, id)
			}
		}
		if len(gone) == len(ids) {
			out.Stopped = true
		}
		switch catalog {
		case events.JiraStatusCatalog:
			out.Statuses = appendNew(out.Statuses, gone...)
		case events.JiraIssueTypeCatalog:
			out.IssueTypes = appendNew(out.IssueTypes, gone...)
		case events.JiraPriorityCatalog:
			out.Priorities = appendNew(out.Priorities, gone...)
		}
	}
	if !known {
		return out, false, false
	}
	return out, len(out.Statuses)+len(out.IssueTypes)+len(out.Priorities) > 0, true
}

// catalog is the ids of catalog the handler's projects offer: the union over
// projects for a status or an issue type, the instance's for a priority. ok is
// false when any read it needs did not answer, since a value missing from the
// rest may be the unread one's.
func (p *Pass) catalog(ctx context.Context, catalog events.JiraCatalog, projects []string) (map[string]bool, bool) {
	if catalog == events.JiraPriorityCatalog {
		read := p.instancePriorities(ctx)
		return read.ids, read.ok
	}
	has := map[string]bool{}
	for _, ref := range projects {
		var read catalogRead
		if catalog == events.JiraIssueTypeCatalog {
			read = p.projectIssueTypes(ctx, ref)
		} else {
			read = p.workflow(ctx, ref)
		}
		if !read.ok {
			return nil, false
		}
		for id := range read.ids {
			has[id] = true
		}
	}
	return has, true
}

func appendNew(list []string, ids ...string) []string {
	for _, id := range ids {
		if !slices.Contains(list, id) {
			list = append(list, id)
		}
	}
	return list
}

// RulesSaved sets kinds.JiraStatusDrift for a save of teamID's status rules.
// prev is the set before the save and next the set it stored; live holds, by
// jira_projects row, the workflow of each project the save read, which is
// every project with a rule the save changed. A project the save read is
// raised or resolved from that read, and a project the save took out of the
// set resolves, since a rule that no longer exists names nothing. Any other
// project is left for the next pass, since an unchanged rule may still name a
// status that is gone.
func RulesSaved(ctx context.Context, orgID, teamID string, prev, next []domain.JiraProjectStatusRules, live map[string][]domain.JiraStatusRef) {
	for _, r := range next {
		workflow, read := live[r.JiraProjectID]
		if !read || len(workflow) == 0 {
			continue
		}
		r.TeamID = teamID
		d, drifted := RuleStatuses(r, workflow)
		kinds.JiraStatusDrift.Set(ctx, orgID, d, drifted)
	}
	for _, r := range prev {
		if domain.RuleForJiraProject(next, r.JiraProjectID) == nil {
			kinds.JiraStatusDrift.Set(ctx, orgID, kinds.JiraStatusRule{TeamID: teamID, JiraProjectID: r.JiraProjectID}, false)
		}
	}
}

// Untracked is the check of an org whose teams track no Jira project on its
// site: no rule names a status, and no team receives a Jira event for a filter
// to miss, so every open occurrence of both kinds resolves.
func Untracked(ctx context.Context, orgID string) {
	kinds.JiraStatusDrift.Sync(ctx, orgID, nil)
	kinds.JiraPredicateDrift.Sync(ctx, orgID, nil)
}
