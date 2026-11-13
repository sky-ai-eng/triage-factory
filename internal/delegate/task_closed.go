// Telling a run what became of its task. A close writes the task and nothing
// else, so the work on it goes on, and the agent doing that work is told: by
// a note when it launched before the close, and by <run_context> when it
// launches after. A close held for a person leaves the task open, and the
// agent is told of that by a note too. All of it is composed here from what
// the task stores, so every path and both timings describe it the same way.

package delegate

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

// runCompletedCloseReason is the close_reason of a task its own work closed:
// its blueprint finished, or its last deliverable was resolved.
const runCompletedCloseReason = "run_completed"

// TellTaskClosed tells each of conversationIDs, the conversations working on
// a task when it closed, that it closed and how (domain.TaskClosedNote). The
// note is staged for the conversation's next tool boundary
// (stageNoteAtToolBoundary), never as a turn of its own, and a conversation
// is told of one close once: the close's own tell and a later resume
// (tellTaskResolved) send the same note under the same key.
//
// The note is composed from the task as stored, not from what the caller
// closed it with, so a task an event reopened since tells nothing, and one
// closed again since tells of the close that stands. Best effort, after the
// close committed: a failed read or stage logs, and costs the conversation
// the news until it next resumes, never the close.
func (s *Spawner) TellTaskClosed(ctx context.Context, orgID, taskID string, conversationIDs []string) {
	if len(conversationIDs) == 0 {
		return
	}
	closed, ok := s.closedTask(ctx, orgID, taskID)
	if !ok {
		return
	}
	for _, id := range conversationIDs {
		s.tellTaskClosed(ctx, orgID, id, closed)
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
		delegateLog.Warn("read the closed task's working conversations failed; each is told when it next resumes", "task_id", taskID, "error", err)
		return
	}
	s.TellTaskClosed(ctx, orgID, taskID, ids)
}

// TellTaskCloseHeld tells each of conversationIDs, the conversations working
// on a task when an event's close of it began to hold, that the event
// resolved the task's situation and the task stays open for a person
// (domain.TaskCloseHeldNote). Staged and deduplicated as TellTaskClosed's
// note is, once per hold. The note is composed from the hold as stored, so a
// hold cleared since tells nothing.
func (s *Spawner) TellTaskCloseHeld(ctx context.Context, orgID, taskID string, conversationIDs []string) {
	if len(conversationIDs) == 0 {
		return
	}
	held, ok := s.heldClose(ctx, orgID, taskID)
	if !ok {
		return
	}
	for _, id := range conversationIDs {
		s.stageTaskNote(ctx, orgID, id, held)
	}
}

// tellTaskResolved tells a conversation whose turn is about to start what
// became of its task while it was not running: at the start of an
// engagement, and before a turn a warm claim resumes, which no claim
// precedes. Every runtime reads staged notes before its agent's next model
// call, so the agent learns of it before it does anything.
//
// A closed task: only a conversation minted before the close is told this
// way. One minted after it is a launch on a closed task, and its
// <run_context> says so (closedTaskContext); that block is fixed for the
// conversation's life, so every later close of the task, after a reopen, is
// news to it again and reaches it here. The mint is compared to the second
// the close fell in, because SQLite stamps a mint to the second: a
// conversation minted just after a close would otherwise read as minted
// before it. The mint is read here, from the conversation's row: the row a
// claim hands the dispatcher does not carry it.
//
// A task whose close is held: every engagement is told, a launch included,
// once per hold. A hold can be cancelled after this turn, so it is a
// transcript row, which says what was true when it was sent, and never a
// line in the launch's block.
//
// Best effort: a failed read of the task or the hold runs the turn without
// the note, and a failed read of the conversation stages it, since a second
// telling costs the agent less than none.
func (s *Spawner) tellTaskResolved(ctx context.Context, orgID, conversationID, taskID string) {
	closed, ok := s.closedTask(ctx, orgID, taskID)
	if !ok {
		if held, ok := s.heldClose(ctx, orgID, taskID); ok {
			s.stageTaskNote(ctx, orgID, conversationID, held)
		}
		return
	}
	conv, err := s.conversations.GetSystem(ctx, orgID, conversationID)
	if err != nil {
		delegateLog.Warn("read the conversation to tell it of its task's close failed; staging the note", "conversation", conversationID, "error", err)
	} else if conv != nil && !conv.StartedAt.Before(closed.at.Truncate(time.Second)) {
		return
	}
	s.tellTaskClosed(ctx, orgID, conversationID, closed)
}

// closedTaskContext is the <run_context> line of a launch on a task that is
// already closed, "" for an open one. Read at the launch rather than taken
// from the claim's task, which a bring-up can outlast.
func (s *Spawner) closedTaskContext(ctx context.Context, orgID, taskID string) string {
	closed, ok := s.closedTask(ctx, orgID, taskID)
	if !ok {
		return ""
	}
	return domain.TaskClosedRunContext(closed.how)
}

// taskClosure is one close of a task as its runs are told of it.
type taskClosure struct {
	// key identifies the close among the task's closes (domain.TaskCloseKey).
	key string
	at  time.Time
	how domain.TaskClose
}

// closedTask reads the task and, when it is closed, the close. ok is false
// for an open task, a missing one, and a failed read.
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
	return taskClosure{
		key: domain.TaskCloseKey(*task.ClosedAt),
		at:  *task.ClosedAt,
		how: s.taskClose(ctx, orgID, *task),
	}, true
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
	case runCompletedCloseReason:
		return domain.TaskClose{Kind: domain.TaskClosedWorkCompleted}
	case db.DuplicateEntityMergedCloseReason:
		return domain.TaskClose{Kind: domain.TaskClosedDuplicate, DuplicateOf: s.duplicateOf(ctx, orgID, task)}
	}
	c := domain.TaskClose{Kind: domain.TaskClosedByEvent, EventType: task.CloseEventType}
	if task.CloseReason == db.ReassignedCloseReason {
		c.Kind = domain.TaskClosedReassigned
	}
	evt := s.closingEvent(ctx, orgID, task.ID)
	if evt == nil {
		return c
	}
	// The poll's close obligation stands in for a close that was lost: to an
	// agent the situation resolved, and no event it would recognize did it.
	if evt.EventType == domain.EventSystemEntityCloseOwed {
		c.EventType = ""
		return c
	}
	c.EventID, c.EventType, c.MetadataJSON = evt.ID, evt.EventType, evt.MetadataJSON
	// A failed current view tells the agent the metadata as recorded.
	if current, err := events.Current(ctx, orgID, evt.EventType, evt.MetadataJSON); err == nil {
		c.MetadataJSON = current
	}
	if c.Kind == domain.TaskClosedReassigned {
		// Jira's and Linear's assigned events both name the new assignee here.
		var meta struct {
			Assignee string `json:"assignee"`
		}
		if err := json.Unmarshal([]byte(c.MetadataJSON), &meta); err == nil {
			c.Assignee = meta.Assignee
		}
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

// tellTaskClosed stages the close note on one conversation unless it already
// has the note for this close.
func (s *Spawner) tellTaskClosed(ctx context.Context, orgID, conversationID string, closed taskClosure) {
	s.stageTaskNote(ctx, orgID, conversationID, taskNote{
		key:  closed.key,
		body: domain.TaskClosedNote(closed.how),
		prov: domain.NoteProvenance{EventID: closed.how.EventID, EventType: closed.how.EventType},
	})
}

// taskNote is one note about a conversation's task: that it closed, or that
// its close is held.
type taskNote struct {
	// key identifies what the note tells of among everything the
	// conversation is told about its task (domain.TaskCloseKey,
	// domain.TaskCloseHoldKey).
	key  string
	body string
	// prov is the event behind the note, when one is.
	prov domain.NoteProvenance
}

// heldClose reads the close the task holds and composes its note. ok is
// false for a task that holds none, and for a failed read.
func (s *Spawner) heldClose(ctx context.Context, orgID, taskID string) (taskNote, bool) {
	if s.tasks == nil || s.events == nil {
		return taskNote{}, false
	}
	hold, err := s.tasks.CloseDeferralForTaskSystem(ctx, orgID, taskID)
	if err != nil {
		delegateLog.Warn("read the task's held close failed; its runs are not told of it now", "task_id", taskID, "error", err)
		return taskNote{}, false
	}
	if hold == nil {
		return taskNote{}, false
	}
	evt, err := s.events.GetSystem(ctx, orgID, hold.EventID)
	if err != nil || evt == nil {
		delegateLog.Warn("read the held close's event failed; the task's runs are not told of it now", "task_id", taskID, "event_id", hold.EventID, "error", err)
		return taskNote{}, false
	}
	// A failed current view tells the agent the metadata as recorded.
	metadataJSON := evt.MetadataJSON
	if current, err := events.Current(ctx, orgID, evt.EventType, metadataJSON); err == nil {
		metadataJSON = current
	}
	return taskNote{
		key:  domain.TaskCloseHoldKey(hold.HeldSince),
		body: domain.TaskCloseHeldNote(evt.EventType, metadataJSON),
		prov: domain.NoteProvenance{EventID: evt.ID, EventType: evt.EventType},
	}, true
}

// stageTaskNote stages a note about the conversation's task unless the
// conversation already has it, delivered or waiting. A failed read of
// whether it was told stages the note anyway: a second copy costs the agent
// less than none.
func (s *Spawner) stageTaskNote(ctx context.Context, orgID, conversationID string, n taskNote) {
	if n.body == "" {
		return
	}
	if s.stagedInjections != nil {
		told, err := s.stagedInjections.HasNoteForTaskCloseSystem(ctx, orgID, conversationID, n.key)
		if err != nil {
			delegateLog.Warn("read whether the conversation has the note about its task failed; staging it", "conversation", conversationID, "error", err)
		} else if told {
			return
		}
	}
	n.prov.TaskClose = n.key
	if !s.stageNoteAtToolBoundary(ctx, orgID, conversationID, domain.MessageSubtypeInjectionSystemNote,
		domain.StagedInjectionProducerTaskClosing, n.body, n.prov) {
		delegateLog.Warn("the note about the conversation's task was not staged; it is told when it next resumes", "conversation", conversationID)
	}
}
