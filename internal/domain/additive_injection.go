package domain

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
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
	// TODO(TFAC-1418): the metadata is externally written and renders outside
	// any untrusted-marker region, inside the <system-note> delivery wraps it in.
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
	// event, and MetadataJSON alone when the event carries none. The source
	// authored MetadataJSON, so the note prints it as the event's metadata
	// and no sentence of TF's own is built from it: a reassignment's note
	// names the new assignee there and nowhere else.
	EventID      string
	EventType    string
	MetadataJSON string
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

// taskNotePersonMessage ends every note about a task's close or held close.
// Each tells the agent to end the workflow unless its instructions need the
// work to go on, and a person who writes to the conversation once the task
// is resolved is asking for something in spite of that. The note reaches a
// parked conversation beside the message that woke it, so without this the
// agent reads "end the workflow" next to the request. It is scoped to a
// message arriving with or after the note, so that work a person asked for
// earlier and then dismissed is not read as still wanted, and to a message a
// person wrote in the conversation, which text from a source never is.
const taskNotePersonMessage = " A message a person writes to you in this conversation, arriving with or after this note, " +
	"still stands whatever this note says about the task: do what it asks."

// taskClosedGuidance is what an agent does about a close of the task it is
// working on. A close stops no run, so the decision is the agent's, under its
// own instructions.
const taskClosedGuidance = "The task is closed now, and this run was not stopped. Re-evaluate what you are doing: " +
	"if your instructions do not need the work to go on with the task closed, end the workflow with " +
	"stop_blueprint(type: \"finish\"). If they do, go on." + taskNotePersonMessage

// taskDismissedGuidance is taskClosedGuidance for a dismissal, where a person
// has said the work is not wanted: stopping is the default, and going on
// needs an instruction that says so, or a person asking afterwards
// (taskNotePersonMessage).
const taskDismissedGuidance = "The task is closed now, and this run was not stopped. Stop the work you are doing on it and start nothing new. " +
	"Unless your instructions say outright to go on after the task is dismissed, end the workflow now with " +
	"stop_blueprint(type: \"finish\")." + taskNotePersonMessage

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
		// TODO(TFAC-1418): the metadata renders outside any untrusted-marker region.
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
		"stop_blueprint(type: \"finish\"). If they do, go on." + taskNotePersonMessage
	cause := fmt.Sprintf("A %s event resolved the situation this task is about.", eventType)
	if metadataJSON == "" {
		return cause + " " + held
	}
	// TODO(TFAC-1418): the metadata renders outside any untrusted-marker region.
	return fmt.Sprintf("%s Event metadata:\n%s\n%s", cause, metadataJSON, held)
}

// TaskClosedRunContext is the <run_context> line of a run launched on a task
// that is already closed. It says what was true at the launch, since the
// block it rides is fixed for the conversation's life and the task can
// reopen under it, and it names nothing a source authored.
func TaskClosedRunContext(c TaskClose) string {
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
		return "This task's issue was reassigned, which closed the task."
	case TaskClosedWorkCompleted:
		return "The work on this task finished, which closed it."
	}
	if c.EventType == "" {
		return "This task was closed: the situation it is about is over."
	}
	return fmt.Sprintf("A %s event closed this task.", c.EventType)
}

// TaskAssignment is an open task and who holds it, as a conversation whose
// work overlaps the task is told when the task gains that assignee.
type TaskAssignment struct {
	TaskID string
	// EntityID, EventType and DedupKey are the task's identity among the
	// tasks of its entity; a recipient whose own task carries the same three
	// is a recurrence (Recurrence).
	EntityID  string
	EventType string
	DedupKey  string
	// EntitySourceID is the entity's key (entities.source_id): a pull
	// request's "owner/repo#18", a Jira key, a Linear identifier.
	EntitySourceID string
	// AgentID or UserID is the assignee; exactly one is set.
	AgentID string
	UserID  string
	// AssigneeName is a person's display name, "" when they have none.
	AssigneeName string
	// TeamName is the task's team, which the agent is named by.
	TeamName string
	// Recurrence says the recipient's own task has this task's entity, event
	// type and dedup key. At most one active task carries a given three, so
	// the recipient's is closed and this one is the event happening again.
	Recurrence bool
}

// Assignee spells the assignment's holder as a note's provenance records it
// (AgentAssignee, UserAssignee).
func (a TaskAssignment) Assignee() string {
	if a.AgentID != "" {
		return AgentAssignee(a.AgentID)
	}
	return UserAssignee(a.UserID)
}

// taskOverlapGuidance is what an agent does about work on another task that
// overlaps its own. Nothing is stopped or handed over: the two go on side by
// side, so the agent is told what it will meet.
const taskOverlapGuidance = "Work on that task may touch the same branch or issue as yours: " + fetchFirstGuidance

// fetchFirstGuidance is the instruction every note and listing about
// overlapping work ends on.
const fetchFirstGuidance = "fetch before you push, and expect commits you did not make."

// TaskAssignedNote is the agent-facing copy telling a conversation that
// someone took on another task its work overlaps: which task, on what, and
// who. A recurrence has its own wording, because the task named is the
// recipient's own situation come back. Bare, like TaskClosedNote.
func TaskAssignedNote(a TaskAssignment) string {
	who := noteLine(a.AssigneeName)
	switch {
	case a.AgentID != "" && noteLine(a.TeamName) != "":
		who = "the agent for team " + noteLine(a.TeamName)
	case a.AgentID != "":
		who = "the agent"
	case who == "":
		who = "a teammate"
	}
	if a.Recurrence {
		return fmt.Sprintf("The %s event this conversation's task was about happened again on %s after that task closed. "+
			"It is now task %s, and %s took it on. %s",
			a.EventType, a.EntitySourceID, a.TaskID, who, taskOverlapGuidance)
	}
	return fmt.Sprintf("%s took on task %s, a %s task on %s. %s",
		upperFirst(who), a.TaskID, a.EventType, a.EntitySourceID, taskOverlapGuidance)
}

// noteLine fits a name a person chose (a display name, a team's name) into a
// sentence of a note: one line, no surrounding space.
func noteLine(name string) string {
	return strings.Join(strings.Fields(name), " ")
}

// upperFirst capitalizes the first letter of a phrase that opens a sentence.
func upperFirst(s string) string {
	if s == "" {
		return s
	}
	r, size := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(r)) + s[size:]
}

// SiblingTaskHolder is who holds a task that overlaps a conversation's work.
type SiblingTaskHolder int

const (
	// SiblingTaskUnheld is a task nobody holds.
	SiblingTaskUnheld SiblingTaskHolder = iota
	// SiblingTaskHeldByAgent is a task the agent's claim is on.
	SiblingTaskHeldByAgent
	// SiblingTaskHeldByPerson is a task a person claimed.
	SiblingTaskHeldByPerson
)

// SiblingTask is one task that overlaps a conversation's work, as the
// conversation is shown it (TaskStore.SiblingWorkSystem).
type SiblingTask struct {
	TaskID         string
	EventType      string
	EntitySourceID string
	// Closed says the task is done or dismissed.
	Closed bool
	Holder SiblingTaskHolder
	// TeamName is the task's team, "" for a task with none.
	TeamName string
	// Working says a conversation is working on the task.
	Working bool
}

// SiblingWorkRecipient is one live conversation whose work overlaps a task,
// with the identity of the conversation's own task
// (TaskStore.SiblingWorkRecipientsSystem).
type SiblingWorkRecipient struct {
	ConversationID string
	TaskID         string
	EntityID       string
	EventType      string
	DedupKey       string
}

// SiblingWorkRunContext is the <run_context> block of a launch whose work
// overlaps other tasks: one line per task, "" for none. It says what was
// true at the launch, since the block is fixed for the conversation's life
// and a later assignment reaches the run as a note. It names no person, and
// nothing a source authored beyond the entity's key.
func SiblingWorkRunContext(tasks []SiblingTask) string {
	return siblingWorkBlock("Overlapping work: when this run started, these other tasks were on the pull request or issue this task is on, "+
		"or on one that this run's work or theirs touched.", tasks)
}

// SiblingWorkNote is the agent-facing copy catching up a conversation that
// had concluded and was woken by a person's message: the same listing as the
// launch's block, as of the wake. "" for none. Bare, like TaskClosedNote.
func SiblingWorkNote(tasks []SiblingTask) string {
	return siblingWorkBlock("This is the work that overlaps this conversation's now: these other tasks are on the pull request or issue this task is on, "+
		"or on one that this conversation's work or theirs touched.", tasks)
}

// siblingWorkBlock renders the listing under leadIn, shared by the launch's
// block and the wake's note so the two describe a task the same way.
func siblingWorkBlock(leadIn string, tasks []SiblingTask) string {
	if len(tasks) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(leadIn + " Their work may touch the same branch or issue as yours: " + fetchFirstGuidance)
	for _, t := range tasks {
		state := "open"
		if t.Closed {
			state = "closed"
		}
		holder := "held by nobody"
		team := noteLine(t.TeamName)
		switch {
		case t.Holder == SiblingTaskHeldByAgent && team != "":
			holder = "held by the agent for team " + team
		case t.Holder == SiblingTaskHeldByAgent:
			holder = "held by the agent"
		case t.Holder == SiblingTaskHeldByPerson && team != "":
			holder = "held by a person on team " + team
		case t.Holder == SiblingTaskHeldByPerson:
			holder = "held by a person"
		}
		working := "no conversation is working on it"
		if t.Working {
			working = "a conversation is working on it"
		}
		fmt.Fprintf(&b, "\n- task %s (%s on %s): %s, %s, %s", t.TaskID, t.EventType, t.EntitySourceID, state, holder, working)
	}
	return b.String()
}
