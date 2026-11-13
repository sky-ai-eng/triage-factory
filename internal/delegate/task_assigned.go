// Telling a run about work that overlaps its own. Several tasks can bear on
// one pull request or issue, each with runs of its own, and a person can be
// working one of them. A run is told when someone takes on a task its work
// overlaps, by a note, and a launch lists the overlapping work in
// <run_context>. One relation decides both (TaskStore.SiblingWorkSystem), so
// a conversation is told of a task exactly when a launch would have listed
// it. Nothing is stopped or handed over: the work goes on side by side, and
// the agent is told what it will meet.

package delegate

import (
	"context"
	"encoding/json"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

// HandleTaskAssigned is the bus subscriber for system:task:assigned: a task
// gained a new assignee, and the live conversations whose work overlaps the
// task are told (TellTaskAssigned).
func (s *Spawner) HandleTaskAssigned(evt domain.Event) {
	if evt.EventType != domain.EventSystemTaskAssigned {
		return
	}
	var meta events.SystemTaskAssignedMetadata
	if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil || meta.TaskID == "" {
		delegateLog.Warn("task assignment event has no usable metadata; nobody is told", "error", err)
		return
	}
	assignee := domain.UserAssignee(meta.UserID)
	switch {
	case meta.AgentID != "" && meta.UserID == "":
		assignee = domain.AgentAssignee(meta.AgentID)
	case meta.AgentID != "" || meta.UserID == "":
		delegateLog.Warn("task assignment event names no single assignee; nobody is told", "task_id", meta.TaskID)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), detachedWriteDeadline)
	defer cancel()
	s.TellTaskAssigned(ctx, evt.OrgID, meta.TaskID, assignee)
}

// TellTaskAssigned tells each live conversation whose work overlaps the task
// that assignee (domain.AgentAssignee, domain.UserAssignee) took the task on
// (domain.TaskAssignedNote). The note is staged for the conversation's next
// tool boundary (stageNoteAtToolBoundary), never as a turn of its own.
//
// The note is composed from the task as stored. A task that has closed, or
// whose claim is no longer assignee, tells nothing: the write that moved it
// on published an event of its own, or left nobody holding it.
//
// Best effort, after the assignment committed: a failed read or stage logs,
// and costs the conversation the news, never the claim. A run that launches
// later still reads the task in its <run_context>.
func (s *Spawner) TellTaskAssigned(ctx context.Context, orgID, taskID, assignee string) {
	if s.tasks == nil {
		return
	}
	assignment, err := s.tasks.AssignmentSystem(ctx, orgID, taskID)
	if err != nil {
		delegateLog.Warn("read the assigned task failed; the conversations its work overlaps are not told", "task_id", taskID, "error", err)
		return
	}
	if assignment == nil || assignment.Assignee() != assignee {
		return
	}
	recipients, err := s.tasks.SiblingWorkRecipientsSystem(ctx, orgID, taskID)
	if err != nil {
		delegateLog.Warn("read the conversations the assigned task's work overlaps failed; they are not told", "task_id", taskID, "error", err)
		return
	}
	prov := domain.NoteProvenance{AssignedTask: taskID, Assignee: assignee}
	for _, r := range recipients {
		told := *assignment
		told.Recurrence = r.EntityID == told.EntityID && r.EventType == told.EventType && r.DedupKey == told.DedupKey
		if !s.stageNoteAtToolBoundary(ctx, orgID, r.ConversationID, domain.MessageSubtypeInjectionSystemNote,
			domain.StagedInjectionProducerTaskAssigned, domain.TaskAssignedNote(told), prov) {
			delegateLog.Warn("the note about an assigned task was not staged; the conversation is not told", "conversation", r.ConversationID, "task_id", taskID)
		}
	}
}

// retireStaleAssignmentNotes withdraws the conversation's waiting assignment
// notes that stopped being true while it was not running, before the turn
// about to start reads the queue: a note says who holds a task now, and a
// conversation that sat idle can hold one about a task since closed or
// handed on. A working conversation reads its notes at its next tool
// boundary, so only an idle one's go stale, and every idle conversation's
// next turn starts through one of this function's two callers.
//
// Best effort: a failed retire leaves the notes to be read as they are.
func (s *Spawner) retireStaleAssignmentNotes(ctx context.Context, orgID, conversationID string) {
	if s.stagedInjections == nil {
		return
	}
	if _, err := s.stagedInjections.RetireStaleAssignmentNotesSystem(ctx, orgID, conversationID); err != nil {
		delegateLog.Warn("retire stale assignment notes failed; the turn reads them as staged", "conversation", conversationID, "error", err)
	}
}

// siblingWork reads the tasks whose work overlaps the conversation's and
// that someone is on. Best effort: a failed read lists nothing.
func (s *Spawner) siblingWork(ctx context.Context, orgID, conversationID string) []domain.SiblingTask {
	if s.tasks == nil {
		return nil
	}
	tasks, err := s.tasks.SiblingWorkSystem(ctx, orgID, conversationID)
	if err != nil {
		delegateLog.Warn("read the work that overlaps the conversation failed; it is not listed", "conversation", conversationID, "error", err)
		return nil
	}
	return tasks
}

// siblingWorkContext is the <run_context> block of a launch whose work
// overlaps other tasks, "" when none does. Read at the launch and fixed for
// the conversation's life; an assignment after it reaches the run as a note
// (TellTaskAssigned).
func (s *Spawner) siblingWorkContext(ctx context.Context, orgID, conversationID string) string {
	return domain.SiblingWorkRunContext(s.siblingWork(ctx, orgID, conversationID))
}

// tellWokenOfSiblingWork catches up a conversation a person's message woke
// after it had concluded, with the work that overlaps it now
// (domain.SiblingWorkNote). Nothing told it of an assignment while it sat,
// since only live conversations are told, and a resume replays the
// <run_context> the conversation launched with. Stages nothing when no work
// overlaps. A conversation woken from parked was live all along and is not
// told here.
func (s *Spawner) tellWokenOfSiblingWork(ctx context.Context, orgID, conversationID string) {
	body := domain.SiblingWorkNote(s.siblingWork(ctx, orgID, conversationID))
	if body == "" {
		return
	}
	if !s.stageNoteAtToolBoundary(ctx, orgID, conversationID, domain.MessageSubtypeInjectionSystemNote,
		domain.StagedInjectionProducerSiblingWork, body, domain.NoteProvenance{}) {
		delegateLog.Warn("the note listing overlapping work was not staged; the woken conversation is not told", "conversation", conversationID)
	}
}
