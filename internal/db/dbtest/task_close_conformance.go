package dbtest

import (
	"context"
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TaskCloseFactory is what a per-backend test file hands to
// RunTaskCloseConformance. It returns the wired TaskStore, the orgID every
// method takes, and a seeder staging the fixture shapes.
//
// This has its own factory rather than riding TaskStoreFactory because what
// the suite asserts sits across four tables: the fixtures it needs are a
// task, an event, a blueprint_run, and a conversation linking them, and the
// backends disagree on nearly every column of the latter two.
type TaskCloseFactory func(t *testing.T) (store db.TaskStore, orgID string, seed TaskCloseSeeder)

// TaskCloseWorkState is the state of the work on a task that no close may
// change: the blueprint run and the conversation under it.
type TaskCloseWorkState struct {
	RunStatus       string
	CancelRequested bool
	// ConversationStatus is the stored status, "" for NULL (mid-flight).
	ConversationStatus string
	StopRequested      bool
	Ended              bool
}

// TaskCloseSeeder stages the (task, event, blueprint, conversation) graph a
// close leaves alone, and reads back what the close wrote. Production mints
// these rows through several stores at once; the callbacks write them
// directly against each backend's own schema so the suite stays schema-blind.
type TaskCloseSeeder struct {
	// Task stages a fresh entity + queued task and returns the task id.
	// Each call must produce a distinct entity so the tasks dedup index
	// doesn't collapse independent subtests.
	Task func(t *testing.T) (taskID string)

	// Event stages one event on the task's entity and returns its id — the
	// closing event the audit row references.
	Event func(t *testing.T, taskID string) (eventID string)

	// BlueprintAndConversation stages a blueprint_run in blueprintStatus with one child
	// conversation on taskID whose STORED status is convStatus (empty string
	// = SQL NULL, the mid-flight state). Returns both ids.
	BlueprintAndConversation func(t *testing.T, taskID, blueprintStatus, convStatus string) (blueprintRunID, convID string)

	// BareConversation stages a conversation on taskID with NO blueprint
	// parent, in the given stored status. Returns its id.
	BareConversation func(t *testing.T, taskID, convStatus string) (convID string)

	// CancelRequested reads a blueprint_run's cancel_requested flag.
	CancelRequested func(t *testing.T, blueprintRunID string) bool

	// WorkState reads the blueprint run and the conversation under it.
	WorkState func(t *testing.T, blueprintRunID, conversationID string) TaskCloseWorkState

	// TaskStatus reads a task's status column.
	TaskStatus func(t *testing.T, taskID string) string

	// CloseAuditCount counts the task's task_events rows of kind 'closed'.
	CloseAuditCount func(t *testing.T, taskID string) int
}

// taskCloseDoor is one of TaskStore's closes, as the suite drives it:
// whether it closed the task, and the status and close reason it writes.
type taskCloseDoor struct {
	name   string
	status string
	reason string
	// audited says the door records the closing event in task_events.
	audited bool
	close   func(t *testing.T, s db.TaskStore, orgID, taskID, eventID string) (closed bool)
}

// taskCloseDoors are every close TaskStore offers: an event's close, held
// or not, the system's own (a blueprint's finish), and a person's dismiss
// and complete. EntityStore's terminating close and duplicate merge are held
// to the same rule in their own suites.
func taskCloseDoors(ctx context.Context) []taskCloseDoor {
	const passed = domain.EventGitHubPRCICheckPassed
	return []taskCloseDoor{
		{name: "CloseOnEventSystem", status: "done", reason: db.SiblingEventCloseReason, audited: true,
			close: func(t *testing.T, s db.TaskStore, orgID, taskID, eventID string) bool {
				t.Helper()
				out, err := s.CloseOnEventSystem(ctx, orgID, taskID, db.SiblingEventCloseReason, passed, eventID)
				if err != nil {
					t.Fatalf("CloseOnEventSystem: %v", err)
				}
				return out.Closed
			}},
		{name: "CloseOrHoldSystem", status: "done", reason: db.SiblingEventCloseReason, audited: true,
			close: func(t *testing.T, s db.TaskStore, orgID, taskID, eventID string) bool {
				t.Helper()
				out, err := s.CloseOrHoldSystem(ctx, orgID, taskID, db.SiblingEventCloseReason, passed, eventID)
				if err != nil {
					t.Fatalf("CloseOrHoldSystem: %v", err)
				}
				return out.Closed
			}},
		{name: "CloseSystem", status: "done", reason: "run_completed",
			close: func(t *testing.T, s db.TaskStore, orgID, taskID, _ string) bool {
				t.Helper()
				if _, err := s.CloseSystem(ctx, orgID, taskID, "run_completed", ""); err != nil {
					t.Fatalf("CloseSystem: %v", err)
				}
				return true
			}},
		{name: "CloseByUser_dismissed", status: "dismissed", reason: db.UserDismissedCloseReason,
			close: func(t *testing.T, s db.TaskStore, orgID, taskID, _ string) bool {
				t.Helper()
				if _, err := s.CloseByUser(ctx, orgID, taskID, "dismissed", TaskVersion(t, s, orgID, taskID)); err != nil {
					t.Fatalf("CloseByUser(dismissed): %v", err)
				}
				return true
			}},
		{name: "CloseByUser_done", status: "done", reason: db.UserCompletedCloseReason,
			close: func(t *testing.T, s db.TaskStore, orgID, taskID, _ string) bool {
				t.Helper()
				if _, err := s.CloseByUser(ctx, orgID, taskID, "done", TaskVersion(t, s, orgID, taskID)); err != nil {
					t.Fatalf("CloseByUser(done): %v", err)
				}
				return true
			}},
	}
}

// RunTaskCloseConformance is the shared suite for the plain close: every
// close TaskStore offers writes the task's close and nothing else.
//
// The pressure is on what a close leaves alone. A conversation working on
// the task goes on working, the blueprint behind it is not called off and so
// advances to its next step, and a step parked on a person stays as
// resumable as it was. A close that stamped cancel intent or a stop on any
// of them would end work nobody asked to end, and a stamped blueprint is one
// the claim gate never drives again.
//
// Every event close passes closeEventType and closingEventID as the pair the
// callers do: an event-driven close names both, a non-event close names
// neither.
func RunTaskCloseConformance(t *testing.T, mk TaskCloseFactory) {
	t.Helper()
	ctx := context.Background()

	// The conversation shapes a close can find on a task, each under a
	// running blueprint: working mid-flight, between two steps (its step
	// concluded and the blueprint not yet advanced), and parked on a person.
	shapes := []struct {
		name       string
		convStatus string
		// working says the close reports the conversation for its caller to
		// tell.
		working bool
	}{
		{name: "working", convStatus: "", working: true},
		{name: "between_steps", convStatus: SeedConcluded, working: true},
		{name: "parked", convStatus: domain.StatusOpen},
	}

	for _, door := range taskCloseDoors(ctx) {
		for _, shape := range shapes {
			t.Run(door.name+"_leaves_a_"+shape.name+"_conversation_as_it_was", func(t *testing.T) {
				s, orgID, seed := mk(t)
				taskID := seed.Task(t)
				eventID := seed.Event(t, taskID)
				runID, convID := seed.BlueprintAndConversation(t, taskID, "running", shape.convStatus)
				before := seed.WorkState(t, runID, convID)

				if !door.close(t, s, orgID, taskID, eventID) {
					t.Fatal("the task did not close")
				}

				got, err := s.Get(ctx, orgID, taskID)
				if err != nil || got == nil {
					t.Fatalf("Get = %+v (err %v)", got, err)
				}
				if got.Status != door.status || got.CloseReason != door.reason || got.ClosedAt == nil {
					t.Errorf("task = (status %q, reason %q, closed_at %v), want (%s, %s, a timestamp)",
						got.Status, got.CloseReason, got.ClosedAt, door.status, door.reason)
				}
				wantAudit := 0
				if door.audited {
					wantAudit = 1
				}
				if n := seed.CloseAuditCount(t, taskID); n != wantAudit {
					t.Errorf("close-audit rows = %d, want %d", n, wantAudit)
				}
				if after := seed.WorkState(t, runID, convID); after != before {
					t.Errorf("the close changed the work on the task:\n before %+v\n after  %+v", before, after)
				}
			})
		}
	}

	t.Run("An_event_close_reports_the_working_conversations", func(t *testing.T) {
		for _, shape := range shapes {
			s, orgID, seed := mk(t)
			taskID := seed.Task(t)
			_, convID := seed.BlueprintAndConversation(t, taskID, "running", shape.convStatus)

			out, err := s.CloseOnEventSystem(ctx, orgID, taskID, db.SiblingEventCloseReason, domain.EventGitHubPRCICheckPassed, seed.Event(t, taskID))
			if err != nil {
				t.Fatalf("%s: CloseOnEventSystem: %v", shape.name, err)
			}
			var want []string
			if shape.working {
				want = []string{convID}
			}
			if !out.Closed || out.Held || !slices.Equal(out.WorkingConversationIDs, want) {
				t.Errorf("%s: outcome = %+v, want closed, telling %v", shape.name, out, want)
			}
		}
	})

	t.Run("A_blueprintless_conversation_is_left_alone", func(t *testing.T) {
		s, orgID, seed := mk(t)
		taskID := seed.Task(t)
		convID := seed.BareConversation(t, taskID, "")
		before := seed.WorkState(t, "", convID)

		out, err := s.CloseOnEventSystem(ctx, orgID, taskID, "entity_closed", domain.EventGitHubPRMerged, seed.Event(t, taskID))
		if err != nil {
			t.Fatalf("CloseOnEventSystem: %v", err)
		}
		if !out.Closed {
			t.Errorf("outcome = %+v, want closed", out)
		}
		if after := seed.WorkState(t, "", convID); after != before {
			t.Errorf("the close changed the conversation:\n before %+v\n after  %+v", before, after)
		}
	})

	t.Run("A_task_with_no_conversations_closes", func(t *testing.T) {
		s, orgID, seed := mk(t)
		taskID := seed.Task(t)

		out, err := s.CloseOnEventSystem(ctx, orgID, taskID, db.SiblingEventCloseReason, domain.EventGitHubPRCICheckPassed, seed.Event(t, taskID))
		if err != nil {
			t.Fatalf("CloseOnEventSystem: %v", err)
		}
		if !out.Closed || len(out.WorkingConversationIDs) != 0 {
			t.Errorf("outcome = %+v, want closed with nobody to tell", out)
		}
		if got := seed.TaskStatus(t, taskID); got != "done" {
			t.Errorf("task status = %q, want done", got)
		}
		if n := seed.CloseAuditCount(t, taskID); n != 1 {
			t.Errorf("close-audit rows = %d, want 1", n)
		}
	})

	t.Run("An_already_terminal_task_is_left_as_it_is", func(t *testing.T) {
		// The replay case, and a close racing a person's: the second close
		// finds the task closed and writes nothing, not even the audit row
		// naming an event that did not close it.
		s, orgID, seed := mk(t)
		taskID := seed.Task(t)
		first := seed.Event(t, taskID)
		if out, err := s.CloseOnEventSystem(ctx, orgID, taskID, "entity_closed", domain.EventGitHubPRMerged, first); err != nil || !out.Closed {
			t.Fatalf("first close = (%+v, %v), want closed", out, err)
		}
		_, convID := seed.BlueprintAndConversation(t, taskID, "running", "")

		for _, eventID := range []string{first, seed.Event(t, taskID)} {
			out, err := s.CloseOnEventSystem(ctx, orgID, taskID, db.SiblingEventCloseReason, domain.EventGitHubPRCICheckPassed, eventID)
			if err != nil {
				t.Fatalf("replayed close: %v", err)
			}
			if out.Closed || out.Held || len(out.WorkingConversationIDs) != 0 {
				t.Errorf("close of a terminal task = %+v, want neither, with nobody to tell (not %s)", out, convID)
			}
		}
		if n := seed.CloseAuditCount(t, taskID); n != 1 {
			t.Errorf("close-audit rows = %d, want the first close's alone", n)
		}
		got, err := s.Get(ctx, orgID, taskID)
		if err != nil || got == nil {
			t.Fatalf("Get = %+v (err %v)", got, err)
		}
		if got.CloseReason != "entity_closed" || got.CloseEventType != domain.EventGitHubPRMerged {
			t.Errorf("close = (%q, %q), want the first close's (entity_closed, %s)", got.CloseReason, got.CloseEventType, domain.EventGitHubPRMerged)
		}
	})

	t.Run("A_close_naming_no_event_writes_no_audit_row", func(t *testing.T) {
		// The poll's reconciled close can reach a task with no event to
		// name: neither an event to record nor a close_event_type to record
		// it under. The only shape in which either is empty.
		s, orgID, seed := mk(t)
		taskID := seed.Task(t)

		out, err := s.CloseOnEventSystem(ctx, orgID, taskID, "reconciled", "", "")
		if err != nil {
			t.Fatalf("CloseOnEventSystem: %v", err)
		}
		if !out.Closed {
			t.Errorf("outcome = %+v, want closed", out)
		}
		if n := seed.CloseAuditCount(t, taskID); n != 0 {
			t.Errorf("close-audit rows = %d, want 0", n)
		}
	})
}
