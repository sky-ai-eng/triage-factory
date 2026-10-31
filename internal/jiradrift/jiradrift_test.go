package jiradrift

import (
	"context"
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
	"github.com/sky-ai-eng/triage-factory/internal/notify/notifytest"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// classified is an upstream failure of a given class.
type classified upstream.Class

func (c classified) Error() string                 { return "jira: " + string(c) }
func (c classified) UpstreamClass() upstream.Class { return upstream.Class(c) }

// fakeJira answers the catalog reads from maps, and records each one.
type fakeJira struct {
	statuses   map[string][]jira.Status
	issueTypes map[string][]jira.IssueType
	priorities []jira.Priority
	// errs fails a read: "statuses:<ref>", "issue_types:<ref>" or "priorities".
	errs  map[string]error
	calls []string
}

func (f *fakeJira) ProjectStatuses(_ context.Context, ref string) ([]jira.Status, error) {
	f.calls = append(f.calls, "statuses:"+ref)
	return f.statuses[ref], f.errs["statuses:"+ref]
}

func (f *fakeJira) ListIssueTypes(_ context.Context, ref string) ([]jira.IssueType, error) {
	f.calls = append(f.calls, "issue_types:"+ref)
	return f.issueTypes[ref], f.errs["issue_types:"+ref]
}

func (f *fakeJira) ListPriorities(context.Context) ([]jira.Priority, error) {
	f.calls = append(f.calls, "priorities")
	return f.priorities, f.errs["priorities"]
}

func ref(id, name string) domain.JiraStatusRef { return domain.JiraStatusRef{ID: id, Name: name} }

func refs(rs ...domain.JiraStatusRef) []domain.JiraStatusRef { return rs }

var (
	todo   = ref("1", "To Do")
	ready  = ref("2", "Ready")
	doing  = ref("3", "In Progress")
	review = ref("4", "In Review")
	done   = ref("5", "Done")
)

// armed is a rule naming every status above, the way a team maps a workflow.
func armed(team, row, projectID, key string) domain.JiraProjectStatusRules {
	return domain.JiraProjectStatusRules{
		TeamID: team, JiraProjectID: row, ProjectExternalID: projectID, ProjectKey: key,
		PickupMembers:     refs(todo, ready),
		InProgressMembers: refs(doing), InProgressCanonical: doing,
		InReviewMembers: refs(review), InReviewCanonical: review,
		DoneMembers: refs(done), DoneCanonical: done,
	}
}

func without(all []domain.JiraStatusRef, gone ...domain.JiraStatusRef) []domain.JiraStatusRef {
	return slices.DeleteFunc(slices.Clone(all), func(r domain.JiraStatusRef) bool { return slices.Contains(gone, r) })
}

var workflow = refs(todo, ready, doing, review, done)

func TestRuleStatuses(t *testing.T) {
	rule := armed("team", "row", "10000", "SKY")
	cases := []struct {
		name        string
		rule        domain.JiraProjectStatusRules
		live        []domain.JiraStatusRef
		wantDrift   bool
		wantStopped bool
		wantNames   []string
	}{
		{name: "every status is in the workflow", rule: rule, live: workflow},
		{
			name: "a status renamed in Jira keeps its id", rule: rule,
			live: append(without(workflow, ready), ref("2", "Ready for dev")),
		},
		{
			name: "one of two pickup statuses is gone", rule: rule, live: without(workflow, ready),
			wantDrift: true, wantNames: []string{"Ready"},
		},
		{
			name: "every pickup status is gone", rule: rule, live: without(workflow, todo, ready),
			wantDrift: true, wantStopped: true, wantNames: []string{"To Do", "Ready"},
		},
		{
			name: "the in-progress status tickets are moved into is gone", rule: rule, live: without(workflow, doing),
			wantDrift: true, wantStopped: true, wantNames: []string{"In Progress"},
		},
		{
			name: "the in-review status is gone, which nothing acts on", rule: rule, live: without(workflow, review),
			wantDrift: true, wantNames: []string{"In Review"},
		},
		{
			name: "a ref with no id matches by name",
			rule: domain.JiraProjectStatusRules{PickupMembers: refs(ref("", "Ready"))},
			live: workflow,
		},
		{
			name: "a ref with no id is gone by name",
			rule: domain.JiraProjectStatusRules{PickupMembers: refs(ref("", "Triage"), todo)},
			live: workflow, wantDrift: true, wantNames: []string{"Triage"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, drifted := RuleStatuses(tc.rule, tc.live)
			if drifted != tc.wantDrift || got.Stopped != tc.wantStopped || !slices.Equal(got.Statuses, tc.wantNames) {
				t.Errorf("RuleStatuses = %+v, drifted %v; want drifted %v, stopped %v, statuses %v",
					got, drifted, tc.wantDrift, tc.wantStopped, tc.wantNames)
			}
		})
	}
}

// TestPassRules_RaisesKeepsAndResolves: a rule naming a status its workflow
// lost is raised, a rule whose workflow Jira would not describe is kept as it
// was, and the occurrence of a rule that no longer exists resolves.
func TestPassRules_RaisesKeepsAndResolves(t *testing.T) {
	rec := notifytest.Record(t)
	ctx, org := context.Background(), "org-rules"

	drifted := armed("team-a", "row-sky", "10000", "SKY")
	unread := armed("team-a", "row-ops", "10001", "OPS")
	// Open before the pass: the unread project's, and one for a rule since
	// removed.
	kinds.JiraStatusDrift.Set(ctx, org, kinds.JiraStatusRule{TeamID: "team-a", JiraProjectID: "row-ops", Statuses: []string{"Ready"}}, true)
	kinds.JiraStatusDrift.Set(ctx, org, kinds.JiraStatusRule{TeamID: "team-a", JiraProjectID: "row-gone", Statuses: []string{"Ready"}}, true)

	f := &fakeJira{
		statuses: map[string][]jira.Status{"10000": jiraStatuses(without(workflow, ready))},
		errs:     map[string]error{"statuses:10001": classified(upstream.Rejected)},
	}
	NewPass(f).Rules(ctx, org, []domain.JiraProjectStatusRules{drifted, unread})

	open := rec.Open(org, kinds.JiraStatusDrift)
	if keys := keysOf(open); !slices.Equal(keys, []string{"team-a:row-ops", "team-a:row-sky"}) {
		t.Fatalf("open = %v, want the drifted rule raised and the unread one kept", keys)
	}
	if got := open[1].Params.(kinds.JiraStatusRule); got.ProjectKey != "SKY" || !slices.Equal(got.Statuses, []string{"Ready"}) || got.Stopped {
		t.Errorf("raised params = %+v", got)
	}
	if open[0].Seen != 1 {
		t.Errorf("the unread rule was raised again (seen %d); a pass that could not read it says nothing", open[0].Seen)
	}
}

// TestPassRules_StopsAskingWhenJiraIsDown: once a read fails on Jira rather
// than on the request, the pass reads nothing more, and keeps every rule it
// did not read.
func TestPassRules_StopsAskingWhenJiraIsDown(t *testing.T) {
	rec := notifytest.Record(t)
	ctx, org := context.Background(), "org-down"
	kinds.JiraStatusDrift.Set(ctx, org, kinds.JiraStatusRule{TeamID: "team-a", JiraProjectID: "row-2"}, true)

	f := &fakeJira{errs: map[string]error{"statuses:10001": classified(upstream.RateLimited)}}
	NewPass(f).Rules(ctx, org, []domain.JiraProjectStatusRules{
		armed("team-a", "row-1", "10001", "ONE"),
		armed("team-a", "row-2", "10002", "TWO"),
	})

	if !slices.Equal(f.calls, []string{"statuses:10001"}) {
		t.Errorf("reads = %v, want the pass to stop after the rate limit", f.calls)
	}
	rec.Active(t, org, kinds.JiraStatusDrift, "team-a:row-2")
}

// TestPassRules_OneReadPerProject: two teams' rules for one project read its
// workflow once.
func TestPassRules_OneReadPerProject(t *testing.T) {
	notifytest.Record(t)
	f := &fakeJira{statuses: map[string][]jira.Status{"10000": jiraStatuses(workflow)}}
	NewPass(f).Rules(context.Background(), "org-shared", []domain.JiraProjectStatusRules{
		armed("team-a", "row", "10000", "SKY"),
		armed("team-b", "row", "10000", "SKY"),
	})
	if len(f.calls) != 1 {
		t.Errorf("reads = %v, want one", f.calls)
	}
}

func handler(id, team, eventType, predicate string) domain.EventHandler {
	return domain.EventHandler{
		ID: id, Kind: domain.EventHandlerKindTrigger, TeamID: team, EventType: eventType,
		Enabled: true, ScopePredicateJSON: &predicate,
	}
}

// TestPassHandlers checks which handlers a pass raises, keeps and resolves.
func TestPassHandlers(t *testing.T) {
	rec := notifytest.Record(t)
	ctx, org := context.Background(), "org-handlers"
	statusChanged, assigned := domain.EventJiraIssueStatusChanged, domain.EventJiraIssueAssigned

	rules := []domain.JiraProjectStatusRules{
		armed("team-a", "row-sky", "10000", "SKY"),
		armed("team-a", "row-ops", "10001", "OPS"),
		armed("team-b", "row-web", "10002", "WEB"),
	}
	opsOnly := ref("90", "Deploying")
	f := &fakeJira{
		statuses: map[string][]jira.Status{
			"10000": jiraStatuses(workflow),
			"10001": jiraStatuses(append(slices.Clone(workflow), opsOnly)),
		},
		issueTypes: map[string][]jira.IssueType{"10000": {{ID: "100"}}, "10001": {{ID: "101"}}},
		priorities: []jira.Priority{{ID: "1"}, {ID: "2"}},
		errs:       map[string]error{"statuses:10002": classified(upstream.Rejected)},
	}

	disabled := handler("disabled", "team-a", statusChanged, `{"new_status_id_in":["999"]}`)
	disabled.Enabled = false
	handlers := []domain.EventHandler{
		// A status another of the team's projects has is not gone.
		handler("other-project", "team-a", statusChanged, `{"new_status_id_in":["90"]}`),
		// One of two statuses gone: still fires on the other.
		handler("partial", "team-a", statusChanged, `{"new_status_id_in":["2","999"]}`),
		// A priority the instance does not have, and nothing else in the field.
		handler("priority", "team-a", assigned, `{"priority_id_in":["7"],"issue_type_id_in":["100"]}`),
		// team-b's only project could not be read.
		handler("unread", "team-b", statusChanged, `{"new_status_id_in":["999"]}`),
		// team-c tracks nothing, so receives no Jira event to match.
		handler("untracked", "team-c", statusChanged, `{"new_status_id_in":["999"]}`),
		disabled,
	}
	kinds.JiraPredicateDrift.Set(ctx, org, kinds.JiraHandlerFilter{HandlerID: "unread"}, true)
	kinds.JiraPredicateDrift.Set(ctx, org, kinds.JiraHandlerFilter{HandlerID: "disabled"}, true)
	kinds.JiraPredicateDrift.Set(ctx, org, kinds.JiraHandlerFilter{HandlerID: "untracked"}, true)

	NewPass(f).Handlers(ctx, org, rules, handlers)

	open := map[string]kinds.JiraHandlerFilter{}
	for _, o := range rec.Open(org, kinds.JiraPredicateDrift) {
		open[o.Key] = o.Params.(kinds.JiraHandlerFilter)
	}
	if keys := keysOf(rec.Open(org, kinds.JiraPredicateDrift)); !slices.Equal(keys, []string{"partial", "priority", "unread"}) {
		t.Fatalf("open = %v, want partial and priority raised and unread kept", keys)
	}
	if got := open["partial"]; !slices.Equal(got.Statuses, []string{"999"}) || got.Stopped || got.TeamID != "team-a" {
		t.Errorf("partial = %+v, want status 999 gone and the trigger still firing", got)
	}
	if got := open["priority"]; !slices.Equal(got.Priorities, []string{"7"}) || len(got.IssueTypes) != 0 || !got.Stopped {
		t.Errorf("priority = %+v, want priority 7 gone, stopping the trigger", got)
	}
	if slices.Contains(f.calls, "issue_types:10002") {
		t.Errorf("reads = %v: team-b's filters name no issue type", f.calls)
	}
}

// TestPassHandler_SetsOneHandler: the check after a save raises the handler it
// is given and resolves it once the filter is fixed, touching no other.
func TestPassHandler_SetsOneHandler(t *testing.T) {
	rec := notifytest.Record(t)
	ctx, org := context.Background(), "org-one-handler"
	rules := []domain.JiraProjectStatusRules{armed("team-a", "row-sky", "10000", "SKY")}
	f := &fakeJira{statuses: map[string][]jira.Status{"10000": jiraStatuses(workflow)}}
	kinds.JiraPredicateDrift.Set(ctx, org, kinds.JiraHandlerFilter{HandlerID: "other"}, true)

	h := handler("rule", "team-a", domain.EventJiraIssueStatusChanged, `{"new_status_id_in":["999"]}`)
	h.Kind, h.Name = domain.EventHandlerKindRule, "Ready work"
	NewPass(f).Handler(ctx, org, h, rules)
	rec.Active(t, org, kinds.JiraPredicateDrift, "rule", "other")

	fixed := `{"new_status_id_in":["2"]}`
	h.ScopePredicateJSON = &fixed
	NewPass(f).Handler(ctx, org, h, rules)
	if keys := keysOf(rec.Open(org, kinds.JiraPredicateDrift)); !slices.Equal(keys, []string{"other"}) {
		t.Errorf("open = %v, want the fixed rule resolved and the other left", keys)
	}
}

// TestRulesSaved: a save resolves or raises each project whose workflow it
// read and each project it removed, and leaves the rest to the pass.
func TestRulesSaved(t *testing.T) {
	rec := notifytest.Record(t)
	ctx, org, team := context.Background(), "org-saved", "team-a"
	for _, row := range []string{"row-fixed", "row-removed", "row-unread"} {
		kinds.JiraStatusDrift.Set(ctx, org, kinds.JiraStatusRule{TeamID: team, JiraProjectID: row}, true)
	}
	fixed, still, unread := armed("", "row-fixed", "1", "FIX"), armed("", "row-still", "2", "STL"), armed("", "row-unread", "3", "UNR")
	prev := []domain.JiraProjectStatusRules{fixed, still, unread, armed("", "row-removed", "4", "RMV")}
	next := []domain.JiraProjectStatusRules{fixed, still, unread}

	RulesSaved(ctx, org, team, prev, next, map[string][]domain.JiraStatusRef{
		"row-fixed": workflow,
		"row-still": without(workflow, ready),
	})

	if keys := keysOf(rec.Open(org, kinds.JiraStatusDrift)); !slices.Equal(keys, []string{"team-a:row-still", "team-a:row-unread"}) {
		t.Errorf("open = %v, want the still-drifted project raised and the unread one left", keys)
	}
}

func TestEventTypes(t *testing.T) {
	got := EventTypes()
	if !slices.Contains(got, domain.EventJiraIssueStatusChanged) || !slices.Contains(got, domain.EventJiraIssuePriorityChanged) {
		t.Errorf("EventTypes = %v, want the status and priority events", got)
	}
	if slices.Contains(got, domain.EventJiraIssueCommented) {
		t.Errorf("EventTypes = %v: a commented filter names no catalog value", got)
	}
}

func jiraStatuses(rs []domain.JiraStatusRef) []jira.Status {
	out := make([]jira.Status, len(rs))
	for i, r := range rs {
		out[i] = jira.Status{ID: r.ID, Name: r.Name}
	}
	return out
}

func keysOf(occs []notifytest.Occurrence) []string {
	out := make([]string, len(occs))
	for i, o := range occs {
		out[i] = o.Key
	}
	return out
}
