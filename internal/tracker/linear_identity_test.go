package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The identity tests: a Linear issue's entity is the row carrying its UUID in
// the org's workspace, so a new identifier is a rename of that row and a new
// workspace is a new namespace.

const opsTeamID = "team-ops"

var (
	opsTodo = domain.LinearStateRef{ID: "st-ops-todo", Name: "Triage", Type: "triage"}
	opsDone = domain.LinearStateRef{ID: "st-ops-done", Name: "Shipped", Type: "completed"}
)

// linRulesWithOps arms team ENG (linRules) and team OPS.
func linRulesWithOps() LinearRules {
	return append(linRules(), LinearTeamRule{
		ID: opsTeamID, Key: "OPS",
		Pickup: []domain.LinearStateRef{opsTodo},
		Done:   []domain.LinearStateRef{opsDone},
	})
}

// movedToOps is is after a move to team OPS: a new identifier, url, team and
// workflow state, and the same UUID.
func movedToOps(is linear.Issue, n int) linear.Issue {
	is.Identifier = fmt.Sprintf("OPS-%d", n)
	is.URL = fmt.Sprintf("https://linear.app/acme/issue/OPS-%d", n)
	is.Team = linear.Team{ID: opsTeamID, Key: "OPS"}
	is.State = linState(opsTodo)
	is.UpdatedAt = "2026-10-01T11:00:00.000Z"
	return is
}

// seedLinearTask attaches a task to the entity the way the router would, so a
// test can show it survives a rename.
func (fx *linearFixture) seedLinearTask(t *testing.T, entityID string) string {
	t.Helper()
	ctx := context.Background()
	eventID, err := fx.stores.Events.RecordSystem(ctx, runmode.LocalDefaultOrgID, domain.Event{
		EntityID: &entityID, EventType: domain.EventLinearIssueAssigned, MetadataJSON: "{}",
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := fx.stores.Tasks.FindOrCreate(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, entityID, domain.EventLinearIssueAssigned, "", eventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	return task.ID
}

// TestRefreshLinear_MoveBetweenArmedTeams: an issue moved from one armed team
// to another is the same entity under its new identifier. Its tasks stay on
// it, its url and its artifacts' targets follow it, and the move is
// identifier_changed, emitted before anything else the cycle diffs — never
// unreachable.
func TestRefreshLinear_MoveBetweenArmedTeams(t *testing.T) {
	fx := newLinearFixture(t)
	is := linIssue(4)
	is.Assignee = &linear.User{ID: "u-alice", Name: "Alice"}
	fx.seed(t, is)
	before := fx.entity(t, "ENG-4")
	taskID := fx.seedLinearTask(t, before.ID)
	art, err := fx.stores.Artifacts.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, domain.Artifact{
		TeamID: runmode.LocalDefaultTeamID, Provider: domain.ArtifactProviderLinear, Kind: domain.ArtifactKindIssue,
		Target: "ENG-4", ExternalID: is.ID, State: domain.ArtifactStateIssueUpdated,
		DedupKey: domain.ArtifactDedupKey(domain.ArtifactProviderLinear, domain.ArtifactKindIssue, is.ID, ""),
	})
	if err != nil {
		t.Fatalf("seed artifact: %v", err)
	}

	moved := movedToOps(is, 77)
	fx.client.put(moved)
	// Found by the destination team's pickup query too, so discovery meets it
	// under the new identifier before the refresh does.
	fx.client.search["pickup:"+opsTeamID] = []linear.Issue{moved}

	evts, err := fx.cycle(t, linRulesWithOps())
	if err != nil {
		t.Fatalf("RefreshLinear: %v", err)
	}
	got := eventTypes(evts)
	if len(got) == 0 || got[0] != domain.EventLinearIssueIdentifierChanged {
		t.Fatalf("events = %v, want identifier_changed first", got)
	}
	if slices.Contains(got, domain.EventLinearIssueUnreachable) {
		t.Fatalf("events = %v: a move between armed teams retired the entity", got)
	}
	var meta events.LinearIssueIdentifierChangedMetadata
	if err := json.Unmarshal([]byte(evts[0].MetadataJSON), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.IssueIdentifier != "OPS-77" || meta.OldIdentifier != "ENG-4" || meta.LinearTeamID != opsTeamID ||
		meta.OldLinearTeamID != linTeamID || meta.OldLinearTeamKey != "ENG" || meta.IssueID != is.ID {
		t.Errorf("identifier_changed metadata = %+v", meta)
	}

	after := fx.entity(t, "OPS-77")
	if after.ID != before.ID || after.URL != moved.URL || after.State != "active" || after.ExternalID != is.ID {
		t.Errorf("entity after the move = %+v, want %s renamed in place", after, before.ID)
	}
	if old, _ := fx.stores.Entities.GetBySourceSystem(context.Background(), runmode.LocalDefaultOrgID, "linear", linWorkspace, "ENG-4"); old != nil {
		t.Errorf("the old identifier still resolves: %+v", old)
	}
	if task, err := fx.stores.Tasks.GetSystem(context.Background(), runmode.LocalDefaultOrgID, taskID); err != nil || task == nil || task.EntityID != before.ID {
		t.Errorf("task = %+v err=%v, want it still on the entity", task, err)
	}
	if a, err := fx.stores.Artifacts.Get(context.Background(), runmode.LocalDefaultOrgID, art.ID); err != nil || a.Target != "OPS-77" || a.DedupKey != art.DedupKey {
		t.Errorf("artifact = %+v err=%v, want its target moved to OPS-77 and its key kept", a, err)
	}
	if snap := fx.snapshot(t, "OPS-77"); snap.Identifier != "OPS-77" || snap.TeamID != opsTeamID {
		t.Errorf("snapshot = %+v, want the move committed", snap)
	}

	if evts, err := fx.cycle(t, linRulesWithOps()); err != nil || len(evts) != 0 {
		t.Fatalf("next cycle: events=%v err=%v, want the move not re-emitted", eventTypes(evts), err)
	}
}

// TestRefreshLinear_MoveToUnarmedTeam: an issue moved to a team no rule arms
// is renamed and its identifier_changed emitted, then it retires as
// unreachable with reason moved. The retirement names the team it left, which
// is the team that was tracking it and the one whose tasks it closes.
func TestRefreshLinear_MoveToUnarmedTeam(t *testing.T) {
	fx := newLinearFixture(t)
	is := linIssue(5)
	fx.seed(t, is)
	before := fx.entity(t, "ENG-5")
	fx.client.put(movedToOps(is, 78))

	evts, err := fx.cycle(t, linRules())
	if err != nil {
		t.Fatalf("RefreshLinear: %v", err)
	}
	if got := eventTypes(evts); !slices.Equal(got, []string{domain.EventLinearIssueIdentifierChanged, domain.EventLinearIssueUnreachable}) {
		t.Fatalf("events = %v, want identifier_changed then unreachable", got)
	}
	var changed events.LinearIssueIdentifierChangedMetadata
	if err := json.Unmarshal([]byte(evts[0].MetadataJSON), &changed); err != nil {
		t.Fatal(err)
	}
	if changed.OldIdentifier != "ENG-5" || changed.IssueIdentifier != "OPS-78" || changed.OldLinearTeamID != linTeamID {
		t.Errorf("identifier_changed metadata = %+v", changed)
	}
	var gone events.LinearIssueUnreachableMetadata
	if err := json.Unmarshal([]byte(evts[1].MetadataJSON), &gone); err != nil {
		t.Fatal(err)
	}
	if gone.Reason != events.LinearUnreachableMoved || gone.LinearTeamID != linTeamID || gone.LinearTeamKey != "ENG" || gone.IssueIdentifier != "OPS-78" {
		t.Errorf("unreachable metadata = %+v, want reason moved naming the old team", gone)
	}
	if after := fx.entity(t, "OPS-78"); after.ID != before.ID {
		t.Errorf("entity = %s, want %s renamed", after.ID, before.ID)
	}
}

// TestRefreshLinear_MovedAndRetiredInOneObservation: an issue that moved and
// was also trashed or archived before a cycle saw either is renamed first, so
// its identifier_changed is still reported, and then it retires under its new
// identifier with the reason it is gone, naming the team that tracked it.
func TestRefreshLinear_MovedAndRetiredInOneObservation(t *testing.T) {
	cases := []struct {
		name       string
		teams      LinearRules
		mutate     func(*linear.Issue)
		wantReason string
	}{
		{"trashed after a move to an armed team", linRulesWithOps(), func(is *linear.Issue) {
			is.Trashed, is.ArchivedAt = true, "2026-10-01T12:00:00Z"
		}, events.LinearUnreachableTrashed},
		{"archived after a move to an unarmed team", linRules(), func(is *linear.Issue) {
			is.ArchivedAt = "2026-10-01T12:00:00Z"
		}, events.LinearUnreachableArchived},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newLinearFixture(t)
			is := linIssue(9)
			fx.seed(t, is)
			before := fx.entity(t, "ENG-9")
			moved := movedToOps(is, 80)
			tc.mutate(&moved)
			fx.client.put(moved)

			evts, err := fx.cycle(t, tc.teams)
			if err != nil {
				t.Fatalf("RefreshLinear: %v", err)
			}
			if got := eventTypes(evts); !slices.Equal(got, []string{domain.EventLinearIssueIdentifierChanged, domain.EventLinearIssueUnreachable}) {
				t.Fatalf("events = %v, want identifier_changed then unreachable", got)
			}
			var changed events.LinearIssueIdentifierChangedMetadata
			if err := json.Unmarshal([]byte(evts[0].MetadataJSON), &changed); err != nil {
				t.Fatal(err)
			}
			if changed.OldIdentifier != "ENG-9" || changed.IssueIdentifier != "OPS-80" || changed.OldLinearTeamID != linTeamID {
				t.Errorf("identifier_changed metadata = %+v", changed)
			}
			var gone events.LinearIssueUnreachableMetadata
			if err := json.Unmarshal([]byte(evts[1].MetadataJSON), &gone); err != nil {
				t.Fatal(err)
			}
			if gone.Reason != tc.wantReason || gone.IssueIdentifier != "OPS-80" || gone.LinearTeamID != linTeamID {
				t.Errorf("unreachable metadata = %+v, want reason %s under OPS-80 naming the old team", gone, tc.wantReason)
			}
			if after := fx.entity(t, "OPS-80"); after.ID != before.ID {
				t.Errorf("entity = %s, want %s renamed", after.ID, before.ID)
			}
		})
	}
}

// TestRefreshLinear_ReopensUnderANewIdentifier: a closed issue that comes back
// open in another armed team is found by its UUID and reactivated as the same
// entity, renamed. Reactivation does not swallow the move: the refresh diffs it
// and identifier_changed names the identifier and team the entity was stored
// under.
func TestRefreshLinear_ReopensUnderANewIdentifier(t *testing.T) {
	fx := newLinearFixture(t)
	is := linIssue(8)
	fx.seed(t, is)
	before := fx.entity(t, "ENG-8")
	if _, err := fx.stores.Entities.MarkClosed(context.Background(), runmode.LocalDefaultOrgID, before.ID); err != nil {
		t.Fatalf("close: %v", err)
	}
	moved := movedToOps(is, 79)
	fx.client.put(moved)
	fx.client.search["pickup:"+opsTeamID] = []linear.Issue{moved}

	evts, err := fx.cycle(t, linRulesWithOps())
	if err != nil {
		t.Fatalf("RefreshLinear: %v", err)
	}
	if got := eventTypes(evts); !slices.Equal(got, []string{domain.EventLinearIssueIdentifierChanged}) {
		t.Fatalf("events = %v, want [identifier_changed]", got)
	}
	var meta events.LinearIssueIdentifierChangedMetadata
	if err := json.Unmarshal([]byte(evts[0].MetadataJSON), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.OldIdentifier != "ENG-8" || meta.IssueIdentifier != "OPS-79" || meta.OldLinearTeamID != linTeamID ||
		meta.OldLinearTeamKey != "ENG" || meta.LinearTeamID != opsTeamID {
		t.Errorf("identifier_changed metadata = %+v", meta)
	}
	after := fx.entity(t, "OPS-79")
	if after.ID != before.ID || after.State != "active" {
		t.Errorf("entity = %+v, want %s reactivated under OPS-79", after, before.ID)
	}
	if snap := fx.snapshot(t, "OPS-79"); snap.Identifier != "OPS-79" || snap.TeamID != opsTeamID {
		t.Errorf("snapshot = %+v, want the fresh identity committed", snap)
	}
	if evts, err := fx.cycle(t, linRulesWithOps()); err != nil || len(evts) != 0 {
		t.Fatalf("next cycle: events=%v err=%v, want the move not re-emitted", eventTypes(evts), err)
	}
}

// TestRefreshLinear_TeamKeyRenameRenamesEveryIssue: renaming a team's key
// changes every identifier in it, and every entity follows its issue.
func TestRefreshLinear_TeamKeyRenameRenamesEveryIssue(t *testing.T) {
	fx := newLinearFixture(t)
	fx.seed(t, linIssue(1), linIssue(2), linIssue(3))
	ids := map[int]string{}
	for _, n := range []int{1, 2, 3} {
		ids[n] = fx.entity(t, fmt.Sprintf("ENG-%d", n)).ID
		is := linIssue(n)
		is.Identifier = fmt.Sprintf("CORE-%d", n)
		is.Team.Key = "CORE"
		fx.client.put(is)
	}
	rules := linRules()
	rules[0].Key = "CORE"

	evts, err := fx.cycle(t, rules)
	if err != nil {
		t.Fatalf("RefreshLinear: %v", err)
	}
	if got := eventTypes(evts); !slices.Equal(got, []string{
		domain.EventLinearIssueIdentifierChanged, domain.EventLinearIssueIdentifierChanged, domain.EventLinearIssueIdentifierChanged,
	}) {
		t.Fatalf("events = %v, want one identifier_changed per issue", got)
	}
	for n, id := range ids {
		if e := fx.entity(t, fmt.Sprintf("CORE-%d", n)); e.ID != id {
			t.Errorf("CORE-%d = %s, want %s renamed", n, e.ID, id)
		}
	}
}

// TestRefreshLinear_WorkspaceSwitch: the org's credential now belongs to
// another workspace. Every active row from the old one retires with
// scope_changed without Linear being asked about it, and a new-workspace issue
// that happens to share an old identifier is a new entity: no old row's text
// changes, and nothing is reactivated or adopted.
func TestRefreshLinear_WorkspaceSwitch(t *testing.T) {
	fx := newLinearFixture(t)
	fx.seed(t, linIssue(1))
	oldActive := fx.entity(t, "ENG-1")
	// A closed old row whose identifier a new issue will reuse.
	closed, _, err := fx.stores.Entities.FindOrCreateSystem(context.Background(), runmode.LocalDefaultOrgID, "linear", linWorkspace, "ENG-2", "uuid-2", "issue", "Old two", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fx.stores.Entities.MarkClosed(context.Background(), runmode.LocalDefaultOrgID, closed.ID); err != nil {
		t.Fatal(err)
	}

	fx.workspace = "ws-new"
	fx.client = newFakeLinear()
	newTeam := LinearRules{{ID: "team-new", Key: "ENG", Pickup: []domain.LinearStateRef{linTodo}, Done: linDoneSet}}
	assigned := linIssue(1)
	assigned.ID, assigned.Title = "uuid-new-1", "A different issue"
	assigned.Team = linear.Team{ID: "team-new", Key: "ENG"}
	assigned.Assignee = &linear.User{ID: "u-viewer", Name: "Viewer"}
	pickup := linIssue(2)
	pickup.ID, pickup.Title = "uuid-new-2", "Another different issue"
	pickup.Team = linear.Team{ID: "team-new", Key: "ENG"}
	fx.client.search["assigned:team-new"] = []linear.Issue{assigned}
	fx.client.search["pickup:team-new"] = []linear.Issue{pickup}
	fx.client.put(assigned)
	fx.client.put(pickup)

	evts, err := fx.cycle(t, newTeam)
	if err != nil {
		t.Fatalf("RefreshLinear: %v", err)
	}
	var retired, assignedTo []string
	for _, e := range evts {
		switch e.EventType {
		case domain.EventLinearIssueUnreachable:
			var meta events.LinearIssueUnreachableMetadata
			_ = json.Unmarshal([]byte(e.MetadataJSON), &meta)
			if meta.Reason != events.LinearUnreachableScopeChanged {
				t.Errorf("unreachable reason = %q, want scope_changed", meta.Reason)
			}
			retired = append(retired, *e.EntityID)
		case domain.EventLinearIssueAssigned:
			assignedTo = append(assignedTo, *e.EntityID)
		default:
			t.Errorf("unexpected %s", e.EventType)
		}
	}
	if !slices.Equal(retired, []string{oldActive.ID}) {
		t.Errorf("retired = %v, want only the old workspace's active row %s", retired, oldActive.ID)
	}
	for _, b := range fx.client.batches {
		if slices.Contains(b, linIssue(1).ID) {
			t.Errorf("an old-workspace issue was in a batch read: %v", b)
		}
	}
	if calls := getIssueCalls(fx.client); len(calls) != 0 {
		t.Errorf("Linear was asked about issues one at a time: %v", calls)
	}

	newOne := fx.entityIn(t, "ws-new", "ENG-1")
	if newOne.ID == oldActive.ID || newOne.ExternalID != "uuid-new-1" {
		t.Errorf("new-workspace ENG-1 = %+v, want its own entity", newOne)
	}
	if !slices.Equal(assignedTo, []string{newOne.ID}) {
		t.Errorf("assigned emitted for %v, want the new entity %s", assignedTo, newOne.ID)
	}
	newTwo := fx.entityIn(t, "ws-new", "ENG-2")
	if newTwo.ID == closed.ID {
		t.Error("the new ENG-2 adopted the closed old row")
	}

	stillOld := fx.entityIn(t, linWorkspace, "ENG-1")
	if stillOld.ID != oldActive.ID || stillOld.Title != oldActive.Title || stillOld.Description != oldActive.Description || stillOld.SnapshotJSON != oldActive.SnapshotJSON {
		t.Errorf("old ENG-1 changed: before %+v after %+v", oldActive, stillOld)
	}
	if stillClosed := fx.entityIn(t, linWorkspace, "ENG-2"); stillClosed.ID != closed.ID || stillClosed.State != "closed" || stillClosed.Title != "Old two" {
		t.Errorf("old ENG-2 = %+v, want it closed and untouched", stillClosed)
	}
}

// TestRefreshLinear_ReusedKeyWaitsForTheOldHolder: team ENG's key was renamed
// to CORE and a new team took ENG, and a new issue there got ENG-1 before TF
// saw the old ENG-1 move. The new issue is skipped while the old one still
// holds the identifier, the old one's refresh renames it, and the new issue is
// created the cycle after.
func TestRefreshLinear_ReusedKeyWaitsForTheOldHolder(t *testing.T) {
	fx := newLinearFixture(t)
	old := linIssue(1)
	fx.seed(t, old)
	holder := fx.entity(t, "ENG-1")

	renamed := old
	renamed.Identifier, renamed.Team.Key = "CORE-1", "CORE"
	fx.client.put(renamed)
	reuser := linIssue(1)
	reuser.ID, reuser.Title = "uuid-reuser", "A new ENG-1"
	reuser.Team = linear.Team{ID: "team-eng2", Key: "ENG"}
	fx.client.put(reuser)
	fx.client.search["pickup:team-eng2"] = []linear.Issue{reuser}
	rules := LinearRules{
		{ID: linTeamID, Key: "CORE", Pickup: []domain.LinearStateRef{linTodo}, Done: linDoneSet},
		{ID: "team-eng2", Key: "ENG", Pickup: []domain.LinearStateRef{linTodo}, Done: linDoneSet},
	}

	if _, err := fx.cycle(t, rules); err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	if got, _ := fx.stores.Entities.GetByExternalIDSystem(context.Background(), runmode.LocalDefaultOrgID, "linear", linWorkspace, "uuid-reuser"); got != nil {
		t.Fatalf("the reusing issue was created while the old holder still held ENG-1: %+v", got)
	}
	if e := fx.entity(t, "CORE-1"); e.ID != holder.ID {
		t.Fatalf("CORE-1 = %s, want the old holder %s renamed", e.ID, holder.ID)
	}

	if _, err := fx.cycle(t, rules); err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	created := fx.entity(t, "ENG-1")
	if created.ID == holder.ID || created.ExternalID != "uuid-reuser" {
		t.Errorf("ENG-1 = %+v, want the reusing issue's own entity", created)
	}
}
