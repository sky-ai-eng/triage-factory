package delegate

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
	"github.com/sky-ai-eng/triage-factory/internal/notify/notifytest"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The bot mirrors tell a team's admins when the move into their rule's
// in-progress canonical fails because the canonical is gone, and only then: a
// ticket that cannot reach a status still in the workflow, and any other
// failure, neither raise the condition nor resolve it, and the next move that
// lands resolves it.

func TestRunJiraMirror_RetiredCanonicalRaisesUntilAMoveLands(t *testing.T) {
	const org = "org-jira-mirror-failing"
	rec := notifytest.Record(t)
	// Already the bot's, so each pass goes straight to the transition.
	fake := newRecordingJiraServer(t, "To Do", "bot")
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	s.SetJiraResolver(&fakeJiraResolver{client: fake.client()})
	rule := mirrorRule()
	rule.JiraProjectID = "proj-sky"
	run := func(stage func(*recordingJiraServer)) {
		fake.mu.Lock()
		fake.withdrawn, fake.unreachable, fake.failTransition = false, false, false
		if stage != nil {
			stage(fake)
		}
		fake.mu.Unlock()
		s.runJiraMirror(org, "ent-sky-1", "SKY-1", rule.TeamID, rule, false)
	}
	unreachable := func(f *recordingJiraServer) { f.unreachable = true }
	failing := func(f *recordingJiraServer) { f.failTransition = true }
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

	run(func(f *recordingJiraServer) { f.withdrawn = true })
	want := kinds.JiraMirror{
		TeamID: runmode.LocalDefaultTeamID, JiraProjectID: "proj-sky",
		ProjectKey: "SKY", Status: "In Progress", IssueKey: "SKY-1",
	}
	if open := rec.Open(org, kinds.JiraMirrorFailing); len(open) != 1 || open[0].Params != want {
		t.Fatalf("open = %+v, want one occurrence with %+v", open, want)
	}

	run(failing)
	unchanged("a server error")
	run(unreachable)
	unchanged("a ticket that cannot reach a status the project has")

	run(nil)
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
	// The rule names a team of this test's own, so a mirror another test in
	// the package left running, which records under the shared local org,
	// never reaches these assertions. The audit row keeps the local team.
	rule := linearMirrorRule()
	rule.TeamID = "team-linear-mirror-failing"
	mine := func() []notifytest.Occurrence {
		var out []notifytest.Occurrence
		for _, occ := range rec.Open(org, kinds.LinearMirrorFailing) {
			if occ.Params.(kinds.LinearMirror).TeamID == rule.TeamID {
				out = append(out, occ)
			}
		}
		return out
	}
	run := func(stateGone, transitionDown bool) {
		fake.mu.Lock()
		fake.failTransition, fake.transitionDown = stateGone, transitionDown
		fake.mu.Unlock()
		s.runLinearMirror(org, e.ID, linearMirrorIdentifier, runmode.LocalDefaultTeamID, rule)
	}

	run(false, true)
	if open := mine(); len(open) != 0 {
		t.Fatalf("open = %+v after a server error, want nothing", open)
	}

	run(true, false)
	want := kinds.LinearMirror{
		TeamID: rule.TeamID, LinearTeamID: linearMirrorTeamID,
		LinearTeamKey: "ENG", State: "In Progress", Identifier: linearMirrorIdentifier,
	}
	if open := mine(); len(open) != 1 || open[0].Params != want {
		t.Fatalf("open = %+v, want one occurrence with %+v", open, want)
	}

	run(false, true)
	if open := mine(); len(open) != 1 || open[0].Seen != 1 {
		t.Errorf("open = %+v after a server error, want the one occurrence, neither bumped nor resolved", open)
	}

	run(false, false)
	if got := fake.transitions(); len(got) != 1 || got[0] != linearDoing.ID {
		t.Fatalf("transitions = %v, want the one move to In Progress", got)
	}
	if open := mine(); len(open) != 0 {
		t.Errorf("open = %+v after a move landed, want nothing", open)
	}
}
