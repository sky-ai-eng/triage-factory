package delegate

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
	"github.com/sky-ai-eng/triage-factory/internal/notify/notifytest"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The bot mirrors tell a team's admins when the move into their rule's
// in-progress canonical fails because the canonical is unreachable, and only
// then: any other failure neither raises the condition nor resolves it, and
// the next move that lands resolves it.

func TestRunJiraMirror_UnreachableCanonicalRaisesUntilAMoveLands(t *testing.T) {
	const org = "org-jira-mirror-failing"
	rec := notifytest.Record(t)
	// Already the bot's, so each pass goes straight to the transition.
	fake := newRecordingJiraServer(t, "To Do", "bot")
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	s.SetJiraResolver(&fakeJiraResolver{client: fake.client()})
	rule := mirrorRule()
	rule.JiraProjectID = "proj-sky"
	run := func(withdrawn, failTransition bool) {
		fake.mu.Lock()
		fake.withdrawn, fake.failTransition = withdrawn, failTransition
		fake.mu.Unlock()
		s.runJiraMirror(org, "ent-sky-1", "SKY-1", rule.TeamID, rule, false)
	}

	run(false, true)
	rec.Inactive(t, org, kinds.JiraMirrorFailing)

	run(true, false)
	want := kinds.JiraMirror{
		TeamID: runmode.LocalDefaultTeamID, JiraProjectID: "proj-sky",
		ProjectKey: "SKY", Status: "In Progress", IssueKey: "SKY-1",
	}
	if open := rec.Open(org, kinds.JiraMirrorFailing); len(open) != 1 || open[0].Params != want {
		t.Fatalf("open = %+v, want one occurrence with %+v", open, want)
	}

	run(false, true)
	if open := rec.Open(org, kinds.JiraMirrorFailing); len(open) != 1 || open[0].Seen != 1 {
		t.Errorf("open = %+v after a server error, want the one occurrence, neither bumped nor resolved", open)
	}

	run(false, false)
	if _, transitions := fake.snapshot(); len(transitions) != 1 || transitions[0] != "In Progress" {
		t.Fatalf("transitions = %v, want the one move to In Progress", transitions)
	}
	rec.Inactive(t, org, kinds.JiraMirrorFailing)
}

func TestRunLinearMirror_DeletedCanonicalRaisesUntilAMoveLands(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	rec := notifytest.Record(t)
	database := newDelegateTestDB(t)
	bindLinearWorkspace(t, database, linearMirrorWorkspace)
	e := seedLinearMirrorEntity(t, database, linearMirrorWorkspace, linearMirrorIdentifier)
	s, fake, _ := newLinearMirrorSpawner(t, database, linearTodo, nil)
	run := func(stateGone, transitionDown bool) {
		fake.mu.Lock()
		fake.failTransition, fake.transitionDown = stateGone, transitionDown
		fake.mu.Unlock()
		s.runLinearMirror(org, e.ID, linearMirrorIdentifier, runmode.LocalDefaultTeamID, linearMirrorRule())
	}

	run(false, true)
	rec.Inactive(t, org, kinds.LinearMirrorFailing)

	run(true, false)
	want := kinds.LinearMirror{
		TeamID: runmode.LocalDefaultTeamID, LinearTeamID: linearMirrorTeamID,
		LinearTeamKey: "ENG", State: "In Progress", Identifier: linearMirrorIdentifier,
	}
	if open := rec.Open(org, kinds.LinearMirrorFailing); len(open) != 1 || open[0].Params != want {
		t.Fatalf("open = %+v, want one occurrence with %+v", open, want)
	}

	run(false, true)
	if open := rec.Open(org, kinds.LinearMirrorFailing); len(open) != 1 || open[0].Seen != 1 {
		t.Errorf("open = %+v after a server error, want the one occurrence, neither bumped nor resolved", open)
	}

	run(false, false)
	if got := fake.transitions(); len(got) != 1 || got[0] != linearDoing.ID {
		t.Fatalf("transitions = %v, want the one move to In Progress", got)
	}
	rec.Inactive(t, org, kinds.LinearMirrorFailing)
}
