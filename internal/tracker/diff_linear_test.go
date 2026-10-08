package tracker

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

var (
	linTodo     = domain.LinearStateRef{ID: "st-todo", Name: "Todo", Type: "unstarted"}
	linProgress = domain.LinearStateRef{ID: "st-prog", Name: "In Progress", Type: "started"}
	linDone     = domain.LinearStateRef{ID: "st-done", Name: "Done", Type: "completed"}
	linDoneSet  = []domain.LinearStateRef{linDone}
)

const linUpdatedAt = "2026-10-01T10:00:00.000Z"

// linSnap is an open, assigned issue with nothing unusual about it. Cases
// mutate a copy into the shape they need.
func linSnap() domain.LinearSnapshot {
	return domain.LinearSnapshot{
		ID: "uuid-1", Identifier: "ENG-1", Title: "Fix the thing",
		BodyHash: "hash-1", State: linTodo,
		Assignee: "Alice", AssigneeUserID: "u-alice",
		Priority: 3, PriorityLabel: "Medium", Labels: []string{},
		TeamID: "team-eng", TeamKey: "ENG",
		URL:       "https://linear.app/acme/issue/ENG-1",
		UpdatedAt: linUpdatedAt,
	}
}

func TestDiffLinearSnapshots(t *testing.T) {
	type want struct {
		eventType, dedupKey string
	}
	cases := []struct {
		name  string
		first bool // prev is the zero snapshot
		prev  func(*domain.LinearSnapshot)
		curr  func(*domain.LinearSnapshot)
		want  []want
	}{
		{name: "first discovery assigned", first: true,
			want: []want{{domain.EventLinearIssueAssigned, ""}}},
		{name: "first discovery unassigned", first: true,
			curr: func(s *domain.LinearSnapshot) { s.Assignee, s.AssigneeUserID = "", "" },
			want: []want{{domain.EventLinearIssueAvailable, ""}}},
		{name: "first discovery already done", first: true,
			curr: func(s *domain.LinearSnapshot) { s.State = linDone },
			want: []want{{domain.EventLinearIssueCompleted, ""}}},
		{name: "first discovery of a parent with open sub-issues", first: true,
			curr: func(s *domain.LinearSnapshot) { s.OpenChildCount = 2 }},
		{name: "first discovery never emits parent_changed", first: true,
			curr: func(s *domain.LinearSnapshot) { s.ParentID, s.ParentIdentifier = "uuid-9", "ENG-9" },
			want: []want{{domain.EventLinearIssueAssigned, ""}}},

		{name: "no change emits nothing"},
		{name: "assigned",
			prev: func(s *domain.LinearSnapshot) { s.Assignee, s.AssigneeUserID = "", "" },
			want: []want{{domain.EventLinearIssueAssigned, ""}}},
		{name: "reassigned",
			curr: func(s *domain.LinearSnapshot) { s.Assignee, s.AssigneeUserID = "Bob", "u-bob" },
			want: []want{{domain.EventLinearIssueAssigned, ""}}},
		{name: "assignee cleared",
			curr: func(s *domain.LinearSnapshot) { s.Assignee, s.AssigneeUserID = "", "" },
			want: []want{{domain.EventLinearIssueAvailable, ""}}},
		{name: "assignee renamed themselves",
			curr: func(s *domain.LinearSnapshot) { s.Assignee = "Alice Liddell" }},
		{name: "reassignment of a parent with open sub-issues",
			prev: func(s *domain.LinearSnapshot) { s.OpenChildCount = 1 },
			curr: func(s *domain.LinearSnapshot) { s.Assignee, s.AssigneeUserID, s.OpenChildCount = "Bob", "u-bob", 1 }},
		{name: "status changed",
			curr: func(s *domain.LinearSnapshot) { s.State = linProgress },
			want: []want{{domain.EventLinearIssueStatusChanged, "In Progress"}}},
		{name: "status changed into done",
			curr: func(s *domain.LinearSnapshot) { s.State = linDone },
			want: []want{{domain.EventLinearIssueStatusChanged, "Done"}, {domain.EventLinearIssueCompleted, ""}}},
		{name: "state renamed in Linear",
			curr: func(s *domain.LinearSnapshot) { s.State.Name = "To Do" }},
		{name: "priority changed",
			curr: func(s *domain.LinearSnapshot) { s.Priority, s.PriorityLabel = 1, "Urgent" },
			want: []want{{domain.EventLinearIssuePriorityChanged, "Urgent"}}},
		{name: "first comment",
			curr: func(s *domain.LinearSnapshot) { s.LastCommentID = "c-1" },
			want: []want{{domain.EventLinearIssueCommented, ""}}},
		{name: "another comment",
			prev: func(s *domain.LinearSnapshot) { s.LastCommentID = "c-1" },
			curr: func(s *domain.LinearSnapshot) { s.LastCommentID = "c-2" },
			want: []want{{domain.EventLinearIssueCommented, ""}}},
		{name: "only comment deleted",
			prev: func(s *domain.LinearSnapshot) { s.LastCommentID = "c-1" }},
		{name: "newest comment deleted",
			prev: func(s *domain.LinearSnapshot) { s.LastCommentID, s.LastCommentAt = "c-2", "2026-10-01T09:00:00.000Z" },
			curr: func(s *domain.LinearSnapshot) { s.LastCommentID, s.LastCommentAt = "c-1", "2026-10-01T08:00:00.000Z" }},
		{name: "comment after the newest was deleted",
			prev: func(s *domain.LinearSnapshot) { s.LastCommentID, s.LastCommentAt = "c-1", "2026-10-01T08:00:00.000Z" },
			curr: func(s *domain.LinearSnapshot) { s.LastCommentID, s.LastCommentAt = "c-3", "2026-10-01T09:30:00.000Z" },
			want: []want{{domain.EventLinearIssueCommented, ""}}},
		{name: "body edited",
			curr: func(s *domain.LinearSnapshot) { s.BodyHash = "hash-2" },
			want: []want{{domain.EventLinearIssueBodyUpdated, ""}}},
		{name: "body fingerprint unknown before",
			prev: func(s *domain.LinearSnapshot) { s.BodyHash = "" },
			curr: func(s *domain.LinearSnapshot) { s.BodyHash = "hash-2" }},
		{name: "parent set",
			curr: func(s *domain.LinearSnapshot) { s.ParentID, s.ParentIdentifier = "uuid-9", "ENG-9" },
			want: []want{{domain.EventLinearIssueParentChanged, "uuid-9"}}},
		{name: "parent changed",
			prev: func(s *domain.LinearSnapshot) { s.ParentID, s.ParentIdentifier = "uuid-9", "ENG-9" },
			curr: func(s *domain.LinearSnapshot) { s.ParentID, s.ParentIdentifier = "uuid-8", "ENG-8" },
			want: []want{{domain.EventLinearIssueParentChanged, "uuid-8"}}},
		{name: "parent cleared",
			prev: func(s *domain.LinearSnapshot) { s.ParentID, s.ParentIdentifier = "uuid-9", "ENG-9" },
			want: []want{{domain.EventLinearIssueParentChanged, linearNoParent}}},
		{name: "parent moved team, same parent",
			prev: func(s *domain.LinearSnapshot) { s.ParentID, s.ParentIdentifier = "uuid-9", "ENG-9" },
			curr: func(s *domain.LinearSnapshot) { s.ParentID, s.ParentIdentifier = "uuid-9", "OPS-4" }},
		{name: "moved to another team",
			// The move comes first, then what the move itself changed: the
			// new team's workflow state is a different state.
			curr: func(s *domain.LinearSnapshot) {
				s.Identifier, s.TeamID, s.TeamKey = "OPS-77", "team-ops", "OPS"
				s.State = domain.LinearStateRef{ID: "state-ops-todo", Name: "Triage", Type: "triage"}
			},
			want: []want{{domain.EventLinearIssueIdentifierChanged, ""}, {domain.EventLinearIssueStatusChanged, "Triage"}}},
		{name: "team key renamed",
			curr: func(s *domain.LinearSnapshot) { s.Identifier, s.TeamKey = "CORE-1", "CORE" },
			want: []want{{domain.EventLinearIssueIdentifierChanged, ""}}},
		{name: "last sub-issue closed",
			prev: func(s *domain.LinearSnapshot) { s.OpenChildCount = 2 },
			want: []want{{domain.EventLinearIssueBecameAtomic, ""}}},
		{name: "last sub-issue closed as the parent completed",
			prev: func(s *domain.LinearSnapshot) { s.OpenChildCount = 1 },
			curr: func(s *domain.LinearSnapshot) { s.State = linDone },
			want: []want{{domain.EventLinearIssueStatusChanged, "Done"}, {domain.EventLinearIssueCompleted, ""}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev, curr := linSnap(), linSnap()
			if tc.first {
				prev = domain.LinearSnapshot{}
			} else if tc.prev != nil {
				tc.prev(&prev)
			}
			if tc.curr != nil {
				tc.curr(&curr)
			}
			evts := DiffLinearSnapshots(prev, curr, "ent-1", linDoneSet)
			var got []want
			for _, e := range evts {
				got = append(got, want{e.EventType, e.DedupKey})
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("events = %v, want %v", got, tc.want)
			}
			wantAt, _ := time.Parse(time.RFC3339Nano, linUpdatedAt)
			for _, e := range evts {
				if e.EntityID == nil || *e.EntityID != "ent-1" {
					t.Errorf("%s: entity id = %v, want ent-1", e.EventType, e.EntityID)
				}
				if !e.OccurredAt.Equal(wantAt) {
					t.Errorf("%s: occurred_at = %v, want the issue's updatedAt %v", e.EventType, e.OccurredAt, wantAt)
				}
			}
		})
	}
}

// TestDiffLinearSnapshots_DoneIsTheTeams: completed reads the done set it is
// handed, so a state outside it is just a status change.
func TestDiffLinearSnapshots_DoneIsTheTeams(t *testing.T) {
	prev, curr := linSnap(), linSnap()
	curr.State = linDone
	evts := DiffLinearSnapshots(prev, curr, "ent-1", nil)
	if got := eventTypes(evts); !slices.Equal(got, []string{domain.EventLinearIssueStatusChanged}) {
		t.Errorf("events with no done set = %v, want status_changed alone", got)
	}
}

// TestLinearEvents_EveryTypeCarriesTeamID is the invariant the router's team
// gate rests on: every Linear event the tracker can emit, whichever path
// emits it, carries linear_team_id whenever anything knows the team, an entity
// with no snapshot included. TestRefreshLinear_SnapshotlessUnreachable covers
// the case where nothing does. The emitting paths are driven until every
// linear:issue:* type in the catalog has been produced at least once, so a new
// type added without passing through here fails rather than going unchecked.
func TestLinearEvents_EveryTypeCarriesTeamID(t *testing.T) {
	var all []domain.Event
	diff := func(prev, curr domain.LinearSnapshot) {
		all = append(all, DiffLinearSnapshots(prev, curr, "ent-1", linDoneSet)...)
	}
	base := linSnap()
	unassigned := base
	unassigned.Assignee, unassigned.AssigneeUserID = "", ""
	done := base
	done.State = linDone
	changed := base
	changed.State = linProgress
	changed.Priority, changed.PriorityLabel = 1, "Urgent"
	changed.LastCommentID = "c-1"
	changed.BodyHash = "hash-2"
	changed.ParentID, changed.ParentIdentifier = "uuid-9", "ENG-9"
	withChildren := base
	withChildren.OpenChildCount = 1

	diff(domain.LinearSnapshot{}, unassigned) // available
	diff(unassigned, base)                    // assigned
	diff(base, changed)                       // status, priority, commented, body, parent
	diff(base, done)                          // completed
	diff(withChildren, base)                  // became_atomic
	moved := base
	moved.Identifier, moved.TeamID, moved.TeamKey = "OPS-77", "team-ops", "OPS"
	diff(base, moved) // identifier_changed

	// unreachable is the one Linear event the diff does not produce. It is
	// emitted for an entity with a snapshot, and for one without when Linear
	// still answered for the issue, which names the team.
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	pub := &recordingPublisher{}
	tr := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, stores.EventQueue, runmode.LocalDefaultOrgID)
	snapJSON, _ := json.Marshal(base)
	tr.emitLinearUnreachable(context.Background(), runmode.LocalDefaultOrgID,
		domain.Entity{ID: "ent-1", SourceID: base.Identifier, SnapshotJSON: string(snapJSON)}, nil, events.LinearUnreachableNotFound)
	answered := linIssue(2)
	answered.Trashed = true
	tr.emitLinearUnreachable(context.Background(), runmode.LocalDefaultOrgID,
		domain.Entity{ID: "ent-2", SourceID: "ENG-2", ExternalID: answered.ID}, &answered, events.LinearUnreachableTrashed)
	all = append(all, pub.nonSystemEvents()...)

	seen := map[string]bool{}
	for _, e := range all {
		seen[e.EventType] = true
		var m struct {
			LinearTeamID string `json:"linear_team_id"`
		}
		if err := json.Unmarshal([]byte(e.MetadataJSON), &m); err != nil {
			t.Fatalf("%s metadata does not parse: %v", e.EventType, err)
		}
		if m.LinearTeamID == "" {
			t.Errorf("%s metadata carries no linear_team_id: %s", e.EventType, e.MetadataJSON)
		}
	}
	for _, et := range domain.AllEventTypes() {
		if et.Source == "linear" && !seen[et.ID] {
			t.Errorf("%s was never emitted, so its metadata went unchecked", et.ID)
		}
	}
}
