package domain

import (
	"fmt"
	"time"
)

// AdditiveEventInjection is the agent-facing copy for an additive event that
// arrived while the task already had a live conversation: a follow-up on the
// conversation in progress (e.g. a second Slack mention on a live thread), not
// a request for a second one. metadataJSON is the
// triggering event's raw metadata JSON; "" (a best-effort lookup failure, or
// an event type with no metadata) still renders a body naming the event type
// alone.
//
// Bare (no <system-note> wrapper): the live path wraps one injection, the
// staged path bundles and wraps the block (StagedInjectionBlock).
func AdditiveEventInjection(eventType, metadataJSON string) string {
	if metadataJSON == "" {
		return fmt.Sprintf(
			"A new %s event occurred on this entity while this run is active. "+
				"Fold it into your current work if relevant; it will not spawn a separate run.",
			eventType,
		)
	}
	return fmt.Sprintf(
		"A new %s event occurred on this entity while this run is active. Event metadata:\n%s\n"+
			"Fold it into your current work if relevant; it will not spawn a separate run.",
		eventType, metadataJSON,
	)
}

// TaskCloseKind is how a task closed, as an agent working on it is told.
type TaskCloseKind int

const (
	// TaskClosedByEvent is an event's close: a sibling event that resolved
	// the situation, the entity ending, or the poll's reconciled close.
	TaskClosedByEvent TaskCloseKind = iota
	// TaskClosedCompleted is a person marking the task complete.
	TaskClosedCompleted
	// TaskClosedDismissed is a person dismissing the task.
	TaskClosedDismissed
	// TaskClosedDuplicate is the dismissal of a task that duplicated another
	// once two entity rows turned out to be one object.
	TaskClosedDuplicate
	// TaskClosedReassigned is the close of the task an earlier assignment
	// made, once its issue is assigned again.
	TaskClosedReassigned
	// TaskClosedWorkCompleted is the close the work on the task brings: its
	// blueprint finished, or the last deliverable was resolved. No note
	// tells it (TaskClosedNote), since the workflow it would end is already
	// over; a launch on such a task still reads it in <run_context>.
	TaskClosedWorkCompleted
)

// TaskClose is one close of a task, with what its note and its
// <run_context> line name.
type TaskClose struct {
	Kind TaskCloseKind
	// EventID, EventType and MetadataJSON are the closing event's, for an
	// event's close and a reassignment. All are "" when the close recorded no
	// event, and MetadataJSON alone when the event carries none.
	EventID      string
	EventType    string
	MetadataJSON string
	// Assignee is who a reassignment assigned the issue to, by display name.
	// "" when the event names nobody. The source authored it, as it did
	// MetadataJSON, so only the note carries either: <run_context> is TF's
	// own words.
	Assignee string
	// DuplicateOf names the task that stays open in a duplicate's place, by
	// a value TF minted. "" when it could not be read.
	DuplicateOf string
}

// TaskCloseKey identifies one close of a task among the closes of that task,
// by the time the close stamped: a task an event reopens and something closes
// again has two. A close note carries it (NoteProvenance.TaskClose), which is
// how a conversation is told of each close once.
func TaskCloseKey(closedAt time.Time) string {
	return closedAt.UTC().Format(time.RFC3339Nano)
}

// TaskCloseHoldKey identifies one held close of a task
// (TaskCloseDeferral.HeldSince), as TaskCloseKey identifies a close. The two
// never collide, so a conversation told a close is held is still told when it
// applies.
func TaskCloseHoldKey(heldSince time.Time) string {
	return "held:" + heldSince.UTC().Format(time.RFC3339Nano)
}

// taskClosedGuidance is what an agent does about a close of the task it is
// working on. A close stops no run, so the decision is the agent's, under its
// own instructions.
const taskClosedGuidance = "The task is closed now, and this run was not stopped. Re-evaluate what you are doing: " +
	"if your instructions do not need the work to go on with the task closed, end the workflow with " +
	"stop_blueprint(type: \"finish\"). If they do, go on."

// taskDismissedGuidance is taskClosedGuidance for a dismissal, where a person
// has said the work is not wanted: stopping is the default, and going on
// needs an instruction that says so.
const taskDismissedGuidance = "The task is closed now, and this run was not stopped. Stop the work you are doing on it and start nothing new. " +
	"Unless your instructions say outright to go on after the task is dismissed, end the workflow now with " +
	"stop_blueprint(type: \"finish\")."

// TaskClosedNote is the agent-facing copy telling a conversation that the
// task it is on closed, how, and what to do about it: one note per kind of
// close, the same for every conversation whoever started its work. Bare,
// like AdditiveEventInjection. "" for a close no note tells
// (TaskClosedWorkCompleted).
func TaskClosedNote(c TaskClose) string {
	guidance := taskClosedGuidance
	switch c.Kind {
	case TaskClosedWorkCompleted:
		return ""
	case TaskClosedDismissed:
		guidance = taskDismissedGuidance
	}
	if c.MetadataJSON != "" && (c.Kind == TaskClosedByEvent || c.Kind == TaskClosedReassigned) {
		return fmt.Sprintf("%s Event metadata:\n%s\n%s", taskCloseCause(c), c.MetadataJSON, guidance)
	}
	return taskCloseCause(c) + " " + guidance
}

// TaskCloseHeldNote is the agent-facing copy telling a conversation that an
// event resolved the situation its task is about while a deliverable on the
// task awaits a person, so the task stays open (TaskCloseDeferral). It is the
// one note about a task that has not closed: without it a run on such a task
// hears nothing of the event until the person acts. metadataJSON is the
// closing event's metadata; "" renders a body naming the event type alone.
// Bare, like TaskClosedNote.
func TaskCloseHeldNote(eventType, metadataJSON string) string {
	const held = "The task is still open: a draft pull request or a pending review from the work on it is waiting for a person's decision, " +
		"and the task closes once they have decided. This run was not stopped. Re-evaluate what you are doing: " +
		"if your instructions do not need the work to go on now that the situation is resolved, end the workflow with " +
		"stop_blueprint(type: \"finish\"). If they do, go on."
	cause := fmt.Sprintf("A %s event resolved the situation this task is about.", eventType)
	if metadataJSON == "" {
		return cause + " " + held
	}
	return fmt.Sprintf("%s Event metadata:\n%s\n%s", cause, metadataJSON, held)
}

// TaskClosedRunContext is the <run_context> line of a run launched on a task
// that is already closed. It says what was true at the launch, since the
// block it rides is fixed for the conversation's life and the task can
// reopen under it, and it names nothing a source authored.
func TaskClosedRunContext(c TaskClose) string {
	c.Assignee = ""
	return "Task state: this task was already closed when this run started. " + taskCloseCause(c)
}

// taskCloseCause is the sentence saying how the task closed, shared by the
// note and the <run_context> line so the two describe a close the same way.
func taskCloseCause(c TaskClose) string {
	switch c.Kind {
	case TaskClosedCompleted:
		return "A person marked this task complete."
	case TaskClosedDismissed:
		return "A person dismissed this task."
	case TaskClosedDuplicate:
		if c.DuplicateOf == "" {
			return "This task was closed as a duplicate of another task about the same issue or pull request."
		}
		return fmt.Sprintf("This task was closed as a duplicate of %s, which is about the same issue or pull request and stays open.", c.DuplicateOf)
	case TaskClosedReassigned:
		if c.Assignee == "" {
			return "This task's issue was reassigned, which closed the task."
		}
		return fmt.Sprintf("This task's issue was reassigned to %s, which closed the task.", c.Assignee)
	case TaskClosedWorkCompleted:
		return "The work on this task finished, which closed it."
	}
	if c.EventType == "" {
		return "This task was closed: the situation it is about is over."
	}
	return fmt.Sprintf("A %s event closed this task.", c.EventType)
}
