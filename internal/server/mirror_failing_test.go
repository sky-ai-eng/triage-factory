package server

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
	"github.com/sky-ai-eng/triage-factory/internal/notify/notifytest"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The claim guards tell a team's admins when the move into their rule's
// in-progress canonical fails because the canonical is unreachable, and only
// then: any other failure neither raises the condition nor resolves it, and
// the next move that lands resolves it. Each test has an org of its own, so a
// claim guard a sibling test left running cannot reach its recorder.

func TestApplyJiraClaim_UnreachableCanonicalRaisesUntilAMoveLands(t *testing.T) {
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
	run := func(withdrawn bool, failTransition bool) {
		fake.mu.Lock()
		fake.withdrawn, fake.failTransition = "", failTransition
		if withdrawn {
			fake.withdrawn = statusInProgressID
		}
		fake.mu.Unlock()
		applyJiraClaim(t.Context(), fake.client(), org, "10005", "SKY-5", rule)
	}

	run(false, true)
	rec.Inactive(t, org, kinds.JiraMirrorFailing)

	run(true, false)
	want := kinds.JiraMirror{
		TeamID: runmode.LocalDefaultTeamID, JiraProjectID: "proj-sky",
		ProjectKey: "SKY", Status: "In Progress", IssueKey: "SKY-5",
	}
	if open := rec.Open(org, kinds.JiraMirrorFailing); len(open) != 1 || open[0].Params != want {
		t.Fatalf("open = %+v, want one occurrence with %+v", open, want)
	}

	run(false, true)
	if open := rec.Open(org, kinds.JiraMirrorFailing); len(open) != 1 || open[0].Seen != 1 {
		t.Errorf("open = %+v after a server error, want the one occurrence, neither bumped nor resolved", open)
	}

	run(false, false)
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
