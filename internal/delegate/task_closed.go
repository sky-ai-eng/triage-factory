// Telling a run what became of its task. A close writes the task and nothing
// else, so the work on it goes on, and the agent doing that work is told by a
// note, once per close, worded for whether its conversation belongs to a
// blueprint run. A launch on a task already closed reads it in <run_context>
// as well, which stays in front of the agent for the conversation's life. A
// close held for a person leaves the task open and tells nothing. All of it is
// composed here from what the task stores, so every path describes it the
// same way.

package delegate

import (
	"context"
	"fmt"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

// TellTaskClosed tells each of conversationIDs, the conversations working on
// a task when it closed, that it closed and how (domain.TaskClosedNote), in
// the wording for that conversation's audience. The note is staged for the
// conversation's next tool boundary
// (stageNoteAtToolBoundary), never as a turn of its own, and a conversation
// is told of one close once: the close's own tell and a later engagement's
// start (tellTaskResolved) send the same note under the same key.
//
// The note is composed from the task as stored, not from what the caller
// closed it with, so a task an event reopened since tells nothing, and one
// closed again since tells of the close that stands. Best effort, after the
// close committed: a failed read or stage logs, and costs the conversation
// the news until its next engagement starts, never the close.
func (s *Spawner) TellTaskClosed(ctx context.Context, orgID, taskID string, conversationIDs []string) {
	if len(conversationIDs) == 0 {
		return
	}
	closed, ok := s.closedTask(ctx, orgID, taskID)
	if !ok {
		return
	}
	note := s.closeNote(ctx, orgID, closed)
	for _, id := range conversationIDs {
		s.stageTaskNote(ctx, orgID, id, closed.key, note)
	}
}

// TellWorkingTaskClosed is TellTaskClosed for a close whose door reports no
// working conversations: a person's dismiss or complete, and a duplicate
// merge's dismissal. It reads the task's working conversations after the
// close committed (TaskStore.WorkingConversationIDsSystem).
func (s *Spawner) TellWorkingTaskClosed(ctx context.Context, orgID, taskID string) {
	if s.tasks == nil {
		return
	}
	ids, err := s.tasks.WorkingConversationIDsSystem(ctx, orgID, taskID)
	if err != nil {
		delegateLog.Warn("read the closed task's working conversations failed; each is told when its next engagement starts", "task_id", taskID, "error", err)
		return
	}
	s.TellTaskClosed(ctx, orgID, taskID, ids)
}

// tellTaskResolved tells a conversation whose turn is about to start what
// became of its task while it was not running: at the start of an
// engagement, and before a turn a warm claim resumes, which no claim
// precedes. Every runtime reads staged notes before its agent's next model
// call, so the agent learns of it before it does anything.
//
// A closed task is told to every conversation that has not had the note for
// this close, a launch on the closed task included: its <run_context> says
// the task is closed (closedTaskContext), and the note adds the closing
// event's metadata, which that block cannot carry.
//
// Best effort: a failed read of the task runs the turn without the note.
func (s *Spawner) tellTaskResolved(ctx context.Context, orgID, conversationID, taskID string) {
	if closed, ok := s.closedTask(ctx, orgID, taskID); ok {
		s.stageTaskNote(ctx, orgID, conversationID, closed.key, s.closeNote(ctx, orgID, closed))
	}
}

// closedTaskContext is the <run_context> line of a launch on a task that is
// already closed, "" for an open one. Read at the launch rather than taken
// from the claim's task, which a bring-up can outlast.
func (s *Spawner) closedTaskContext(ctx context.Context, orgID, taskID string) string {
	closed, ok := s.closedTask(ctx, orgID, taskID)
	if !ok {
		return ""
	}
	return domain.TaskClosedRunContext(s.taskClose(ctx, orgID, closed.task))
}

// taskClosure is one close of a task as its runs are told of it.
type taskClosure struct {
	// key identifies the close among the task's closes (domain.TaskCloseKey).
	key  string
	task domain.Task
}

// closedTask reads the task and, when it is closed, the close. ok is false
// for an open task, a missing one, and a failed read. How it closed is left
// to taskClose: most callers find the conversation already told and never
// need it.
func (s *Spawner) closedTask(ctx context.Context, orgID, taskID string) (taskClosure, bool) {
	if s.tasks == nil {
		return taskClosure{}, false
	}
	task, err := s.tasks.GetSystem(ctx, orgID, taskID)
	if err != nil {
		delegateLog.Warn("read the task to tell its runs of a close failed; they are not told now", "task_id", taskID, "error", err)
		return taskClosure{}, false
	}
	if task == nil || task.ClosedAt == nil || (task.Status != "done" && task.Status != "dismissed") {
		return taskClosure{}, false
	}
	return taskClosure{key: domain.TaskCloseKey(*task.ClosedAt), task: *task}, true
}

// closeNote reads how the task closed the first time a conversation needs
// its note, and once for all of them, and composes the note for the
// conversation's audience.
func (s *Spawner) closeNote(ctx context.Context, orgID string, closed taskClosure) func(domain.TaskNoteAudience) taskNote {
	how := sync.OnceValue(func() domain.TaskClose { return s.taskClose(ctx, orgID, closed.task) })
	return func(a domain.TaskNoteAudience) taskNote {
		c := how()
		return taskNote{
			body: domain.TaskClosedNote(c, a),
			prov: domain.NoteProvenance{EventID: c.EventID, EventType: c.EventType},
		}
	}
}

// taskClose resolves how a closed task closed, from its close reason and
// what that reason points at: the closing event, or the task that stays open
// in a duplicate's place. A lookup that fails leaves its detail out, and the
// close is still told.
func (s *Spawner) taskClose(ctx context.Context, orgID string, task domain.Task) domain.TaskClose {
	switch task.CloseReason {
	case db.UserCompletedCloseReason:
		return domain.TaskClose{Kind: domain.TaskClosedCompleted}
	case db.UserDismissedCloseReason:
		return domain.TaskClose{Kind: domain.TaskClosedDismissed}
	case db.RunCompletedCloseReason:
		return domain.TaskClose{Kind: domain.TaskClosedWorkCompleted}
	case db.DuplicateEntityMergedCloseReason:
		return domain.TaskClose{Kind: domain.TaskClosedDuplicate, DuplicateOf: s.duplicateOf(ctx, orgID, task)}
	}
	c := domain.TaskClose{Kind: domain.TaskClosedByEvent}
	if task.CloseReason == db.ReassignedCloseReason {
		c.Kind = domain.TaskClosedReassigned
	}
	// An event's close stamps the event's type on the task, and no other
	// close does, so a task without one names no event. The poll's close
	// obligation stands in for a close that was lost: to an agent the
	// situation resolved, and no event it would recognize did it.
	if task.CloseEventType == "" || task.CloseEventType == domain.EventSystemEntityCloseOwed {
		return c
	}
	c.EventType = task.CloseEventType
	// The audit row is the task's newest close by an event, which is this
	// close only when it is of the type this close stamped.
	evt := s.closingEvent(ctx, orgID, task.ID)
	if evt == nil || evt.EventType != task.CloseEventType {
		return c
	}
	c.EventID, c.MetadataJSON = evt.ID, evt.MetadataJSON
	// A failed current view tells the agent the metadata as recorded.
	if current, err := events.Current(ctx, orgID, evt.EventType, evt.MetadataJSON); err == nil {
		c.MetadataJSON = current
	}
	return c
}

// closingEvent reads the event that closed the task, nil when the close
// recorded none or a read failed.
func (s *Spawner) closingEvent(ctx context.Context, orgID, taskID string) *domain.Event {
	if s.events == nil {
		return nil
	}
	eventID, err := s.tasks.ClosingEventIDSystem(ctx, orgID, taskID)
	if err != nil {
		delegateLog.Warn("read the task's closing event failed; the close is told without it", "task_id", taskID, "error", err)
		return nil
	}
	if eventID == "" {
		return nil
	}
	evt, err := s.events.GetSystem(ctx, orgID, eventID)
	if err != nil {
		delegateLog.Warn("read the task's closing event failed; the close is told without it", "task_id", taskID, "event_id", eventID, "error", err)
		return nil
	}
	return evt
}

// duplicateOf names the task that stays open in place of a duplicate the
// merge of two entity rows dismissed: the active task on the same entity
// under the same event type and dedup key, which the merge moved the
// duplicate beside. "" when it has closed since or the read failed. The name
// is the task's id alone, a value TF minted, because it is also read in
// <run_context>, where text a source authored must not appear.
func (s *Spawner) duplicateOf(ctx context.Context, orgID string, task domain.Task) string {
	active, err := s.tasks.FindActiveByEntitySystem(ctx, orgID, task.EntityID)
	if err != nil {
		delegateLog.Warn("read the task a duplicate was merged into failed; the close is told without it", "task_id", task.ID, "error", err)
		return ""
	}
	for _, t := range active {
		if t.ID != task.ID && t.EventType == task.EventType && t.DedupKey == task.DedupKey {
			return fmt.Sprintf("task %s", t.ID)
		}
	}
	return ""
}

// taskNote is one note about a conversation's task closing. An empty body is
// a note nobody is sent.
type taskNote struct {
	body string
	// prov is the event behind the note, when one is.
	prov domain.NoteProvenance
}

// stageTaskNote stages a note about the conversation's task unless the
// conversation already has the one key names (domain.TaskCloseKey),
// delivered or waiting. note is asked for only once the conversation is
// known to need it, for the conversation's audience (noteAudience). A failed
// read of whether it was told stages the note anyway: a second copy costs the
// agent less than none. The read and the write are two statements, so a
// close's own tell racing an engagement's start can stage two copies, at the
// same cost.
func (s *Spawner) stageTaskNote(ctx context.Context, orgID, conversationID, key string, note func(domain.TaskNoteAudience) taskNote) {
	if s.stagedInjections != nil {
		told, err := s.stagedInjections.HasNoteForTaskCloseSystem(ctx, orgID, conversationID, key)
		if err != nil {
			delegateLog.Warn("read whether the conversation has the note about its task failed; staging it", "conversation", conversationID, "error", err)
		} else if told {
			return
		}
	}
	n := note(s.noteAudience(ctx, orgID, conversationID))
	if n.body == "" {
		return
	}
	n.prov.TaskClose = key
	if _, ok := s.stageNoteAtToolBoundary(ctx, orgID, conversationID, domain.MessageSubtypeInjectionSystemNote,
		domain.StagedInjectionProducerTaskClosing, n.body, n.prov); !ok {
		delegateLog.Warn("the note about the conversation's task was not staged; it is told when its next engagement starts", "conversation", conversationID)
	}
}

// noteAudience is who reads a note about the conversation's task: a
// conversation that belongs to a blueprint run, which is offered
// stop_blueprint, or a chat, which is not. A failed read answers the chat,
// whose wording names no tool and so is true for either reader.
func (s *Spawner) noteAudience(ctx context.Context, orgID, conversationID string) domain.TaskNoteAudience {
	if s.conversations == nil {
		return domain.TaskNoteForChat
	}
	conv, err := s.conversations.GetSystem(ctx, orgID, conversationID)
	if err != nil {
		delegateLog.Warn("read whether the conversation belongs to a blueprint run failed; its note is worded for a chat", "conversation", conversationID, "error", err)
		return domain.TaskNoteForChat
	}
	if conv == nil || conv.BlueprintRunID == "" {
		return domain.TaskNoteForChat
	}
	return domain.TaskNoteForBlueprint
}
