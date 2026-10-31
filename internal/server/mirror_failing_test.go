package server

import (
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
	"github.com/sky-ai-eng/triage-factory/internal/notify/notifytest"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/zalando/go-keyring"
)

// The claim guards tell a team's admins when the move into their rule's
// in-progress canonical fails because the canonical is gone, and only then: a
// ticket that cannot reach a status still in the workflow, and any other
// failure, neither raise the condition nor resolve it, and the next move that
// lands resolves it. Each claim guard test has an org of its own, so a claim
// guard a sibling test left running cannot reach its recorder.

func TestApplyJiraClaim_RetiredCanonicalRaisesUntilAMoveLands(t *testing.T) {
	const org = "org-jira-claim-mirror"
	rec := notifytest.Record(t)
	fake := newJiraIssueFake(t, statusToDoID, jiraMeKey)
	rule := domain.JiraProjectStatusRules{
		TeamID:              runmode.LocalDefaultTeamID,
		JiraProjectID:       "proj-sky",
		ProjectKey:          "SKY",
		InProgressMembers:   jiraUnclaimInProgress,
		InProgressCanonical: domain.JiraStatusRef{ID: statusInProgressID, Name: "In Progress"},
	}
	run := func(stage func(*jiraIssueFake)) {
		fake.mu.Lock()
		fake.withdrawn, fake.unreachable, fake.failTransition = "", "", false
		if stage != nil {
			stage(fake)
		}
		fake.mu.Unlock()
		applyJiraClaim(t.Context(), fake.client(), org, "10005", "SKY-5", rule)
	}
	unreachable := func(f *jiraIssueFake) { f.unreachable = statusInProgressID }
	failing := func(f *jiraIssueFake) { f.failTransition = true }
	unchanged := func(after string) {
		t.Helper()
		if open := rec.Open(org, kinds.JiraMirrorFailing); len(open) != 1 || open[0].Seen != 1 {
			t.Errorf("open = %+v after %s, want the one occurrence, neither bumped nor resolved", open, after)
		}
	}

	run(failing)
	rec.Inactive(t, org, kinds.JiraMirrorFailing)
	run(unreachable)
	rec.Inactive(t, org, kinds.JiraMirrorFailing)

	run(func(f *jiraIssueFake) { f.withdrawn = statusInProgressID })
	want := kinds.JiraMirror{
		TeamID: runmode.LocalDefaultTeamID, JiraProjectID: "proj-sky",
		ProjectKey: "SKY", Status: "In Progress", IssueKey: "SKY-5",
	}
	if open := rec.Open(org, kinds.JiraMirrorFailing); len(open) != 1 || open[0].Params != want {
		t.Fatalf("open = %+v, want one occurrence with %+v", open, want)
	}

	run(failing)
	unchanged("a server error")
	run(unreachable)
	unchanged("a ticket that cannot reach a status the project has")

	run(nil)
	if got := fake.Writes(); len(got) != 1 || got[0] != (jiraWrite{kind: "transition", value: statusInProgressID}) {
		t.Fatalf("writes = %v, want the one move to In Progress", got)
	}
	rec.Inactive(t, org, kinds.JiraMirrorFailing)
}

func TestApplyLinearClaim_DeletedCanonicalRaisesUntilAMoveLands(t *testing.T) {
	const org = "org-linear-claim-mirror"
	rec := notifytest.Record(t)
	fake := newLinearIssueFake(t, &linear.User{ID: linearMe}, linearFixtureState(t, linearStateTodo))
	rule := linearClaimRule
	rule.TeamID = runmode.LocalDefaultTeamID
	run := func(stateGone, transitionDown bool) {
		fake.mu.Lock()
		fake.stateGone, fake.transitionDown = stateGone, transitionDown
		fake.mu.Unlock()
		applyLinearClaim(t.Context(), fake.client(), org, linearIssueUUID, linearIssueIdentifier, linearMe, rule)
	}

	run(false, true)
	rec.Inactive(t, org, kinds.LinearMirrorFailing)

	run(true, false)
	want := kinds.LinearMirror{
		TeamID: runmode.LocalDefaultTeamID, LinearTeamID: linearTeamEng,
		LinearTeamKey: "ENG", State: "In Progress", Identifier: linearIssueIdentifier,
	}
	if open := rec.Open(org, kinds.LinearMirrorFailing); len(open) != 1 || open[0].Params != want {
		t.Fatalf("open = %+v, want one occurrence with %+v", open, want)
	}

	run(false, true)
	if open := rec.Open(org, kinds.LinearMirrorFailing); len(open) != 1 || open[0].Seen != 1 {
		t.Errorf("open = %+v after a server error, want the one occurrence, neither bumped nor resolved", open)
	}

	run(false, false)
	if got := fake.Writes(); len(got) != 1 || got[0] != (linearWrite{kind: "transition", value: linearStateDoing}) {
		t.Fatalf("writes = %v, want the one move to In Progress", got)
	}
	rec.Inactive(t, org, kinds.LinearMirrorFailing)
}

// The stock deck's claim moves a ticket into the same canonical, so it raises
// and resolves the condition as the claim guard does.
func TestStockClaim_RetiredCanonicalRaisesUntilAMoveLands(t *testing.T) {
	keyring.MockInit()
	org := runmode.LocalDefaultOrgID
	rec := notifytest.Record(t)
	s := newTestServer(t)
	seedStockConfig(t, s)
	seedStockTicket(t, s, "SKY-1")
	seedStockTicket(t, s, "SKY-2")
	fake := newJiraIssueFake(t, statusToDoID, "")
	s.jiraResolver = &recordingJiraResolver{userClient: fake.client()}

	fake.withdrawn = statusInProgressID
	if _, body := postStock(t, s, "claim", "e_stock_SKY-1"); body.Applied != 0 {
		t.Fatalf("claim into a retired canonical applied %d, want it refused", body.Applied)
	}
	open := rec.Open(org, kinds.JiraMirrorFailing)
	if len(open) != 1 {
		t.Fatalf("open = %+v, want one occurrence", open)
	}
	if m := open[0].Params.(kinds.JiraMirror); m.TeamID != runmode.LocalDefaultTeamID || m.ProjectKey != "SKY" || m.IssueKey != "SKY-1" {
		t.Errorf("params = %+v, want the local team's SKY rule, last tried on SKY-1", m)
	}

	fake.mu.Lock()
	fake.withdrawn = ""
	fake.mu.Unlock()
	if _, body := postStock(t, s, "claim", "e_stock_SKY-2"); body.Applied != 1 {
		t.Fatalf("claim = %d applied, want 1", body.Applied)
	}
	rec.Inactive(t, org, kinds.JiraMirrorFailing)
}

// A save of the team's Jira rules resolves the condition for a project whose
// in-progress canonical it changes or which it drops, and leaves it open for
// one it leaves as it was.
func TestJiraProjectsPut_ResolvesTheMirrorConditionOfAChangedOrDroppedRule(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	s, _ := newServerWithJiraCatalog(t, "SKY")
	if rec := putJiraProjects(t, s, "SKY"); rec.Code != http.StatusOK {
		t.Fatalf("arming PUT = %d; body=%s", rec.Code, rec.Body.String())
	}
	stored, _ := teamJiraRules(t, s)
	if len(stored) != 1 {
		t.Fatalf("rules = %+v, want SKY", stored)
	}
	rec := notifytest.Record(t)
	raise := func() {
		kinds.JiraMirrorFailing.Set(t.Context(), org, kinds.JiraMirror{TeamID: stored[0].TeamID, JiraProjectID: stored[0].JiraProjectID}, true)
	}
	put := func(elements ...map[string]any) {
		t.Helper()
		if elements == nil {
			elements = []map[string]any{}
		}
		if res := putJiraProjectElements(t, s, elements...); res.Code != http.StatusOK {
			t.Fatalf("PUT = %d; body=%s", res.Code, res.Body.String())
		}
	}

	raise()
	put(map[string]any{"id": stored[0].JiraProjectID})
	rec.Active(t, org, kinds.JiraMirrorFailing)

	put(map[string]any{"id": stored[0].JiraProjectID, "in_progress": map[string]any{
		"member_ids": []string{statusInProgressID, statusInReviewID}, "canonical_id": statusInReviewID,
	}})
	rec.Inactive(t, org, kinds.JiraMirrorFailing)

	raise()
	put()
	rec.Inactive(t, org, kinds.JiraMirrorFailing)
}

// The Linear sibling of the Jira rules save.
func TestLinearTeamsPut_ResolvesTheMirrorConditionOfAChangedOrDroppedRule(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	s, _ := newServerWithLinearCatalog(t, linearFixtureEng)
	mustPutLinearTeams(t, s, armedLinearTeam(linearTeamEng))
	rec := notifytest.Record(t)
	raise := func() {
		kinds.LinearMirrorFailing.Set(t.Context(), org, kinds.LinearMirror{TeamID: runmode.LocalDefaultTeamID, LinearTeamID: linearTeamEng}, true)
	}

	raise()
	mustPutLinearTeams(t, s, map[string]any{"id": linearTeamEng})
	rec.Active(t, org, kinds.LinearMirrorFailing)

	moved := armedLinearTeam(linearTeamEng)
	moved["in_progress"] = map[string]any{"member_ids": []string{linearStateDoing, linearStateReview}, "canonical_id": linearStateReview}
	mustPutLinearTeams(t, s, moved)
	rec.Inactive(t, org, kinds.LinearMirrorFailing)

	raise()
	mustPutLinearTeams(t, s)
	rec.Inactive(t, org, kinds.LinearMirrorFailing)
}
