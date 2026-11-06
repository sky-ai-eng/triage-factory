package delegate

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
	"github.com/sky-ai-eng/triage-factory/internal/wsevent/toast"
)

// What a run tells people about itself, and whom. A conversation a person
// delegated tells that person, and nobody else: a toast is for them alone, and
// a failure or a park that waits on them is a notification for them
// (kinds.ConversationFailed, kinds.ConversationPaused), because they are often
// not watching when it happens. A conversation a trigger fired tells nobody;
// the board shows how it went.

// toastConversationDelegator fires a toast for the person who delegated
// conversationID, for a site that holds only its id. toast.FireUser fires
// nothing for a conversation a trigger fired, whose creator is empty.
func (s *Spawner) toastConversationDelegator(orgID, conversationID string, level toast.Level, body string) {
	if conv := s.readConversation(orgID, conversationID); conv != nil {
		toast.FireUser(s.wsHub, orgID, conv.CreatorUserID, level, "", body)
	}
}

// noteConversationFailed tells userID, the person who delegated
// conversationID, that it failed. An empty userID, a conversation a trigger
// fired, tells nobody. Call it only once the failure is recorded: the notice
// outlives the moment, so one for a failure that did not land, or that a
// racing verdict overtook, is a lasting false report. Best effort, like every
// notification.
func (s *Spawner) noteConversationFailed(orgID, conversationID, userID, taskID string, kind domain.ConversationFailureKind, summary string) {
	if userID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), notifyReadTimeout)
	defer cancel()
	kinds.ConversationFailed.Post(ctx, orgID, kinds.FailedConversation{
		ConversationID: conversationID,
		UserID:         userID,
		Subject:        s.taskSubject(ctx, orgID, taskID),
		FailureKind:    kind,
		Summary:        summary,
	})
}

// noteDelegatorFailed is noteConversationFailed for a site that holds only
// the conversation's id.
func (s *Spawner) noteDelegatorFailed(orgID, conversationID string, kind domain.ConversationFailureKind, summary string) {
	if conv := s.readConversation(orgID, conversationID); conv != nil {
		s.noteConversationFailed(orgID, conversationID, conv.CreatorUserID, conv.TaskID, kind, summary)
	}
}

// noteConversationPaused opens kinds.ConversationPaused for a conversation
// that has just parked, when the park it now holds waits on the person who
// delegated it. It reads the row rather than trusting the park it asked for,
// because a stop requested in the meantime overrides the reason in the store.
func (s *Spawner) noteConversationPaused(orgID, conversationID string) {
	if conv := s.readConversation(orgID, conversationID); conv != nil {
		s.notePaused(orgID, conv)
	}
}

// notePaused is noteConversationPaused for a row already read. Its test is
// ListAwaitingPersonSystem's, so a park this opens is one the brain's sweep
// keeps, and the sweep resolves it once the conversation leaves the park.
func (s *Spawner) notePaused(orgID string, conv *domain.Conversation) {
	if conv.CreatorUserID == "" || conv.Status != domain.StatusOpen || conv.EndedAt != nil ||
		conv.ArchivedAt != nil || !conv.ParkReason.AwaitsPerson() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), notifyReadTimeout)
	defer cancel()
	kinds.ConversationPaused.Set(ctx, orgID, kinds.PausedFor(domain.ConversationAwaitingPerson{
		ConversationID: conv.ID,
		CreatorUserID:  conv.CreatorUserID,
		ParkReason:     conv.ParkReason,
		EntitySourceID: s.taskSubject(ctx, orgID, conv.TaskID),
	}), true)
}

// notifyReadTimeout bounds the reads and the write one of the notes above
// makes, so a slow database delays a run's ending by at most this.
const notifyReadTimeout = 5 * time.Second

// readConversation is conversationID's row, or nil when it cannot be read.
func (s *Spawner) readConversation(orgID, conversationID string) *domain.Conversation {
	if s.conversations == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), notifyReadTimeout)
	defer cancel()
	conv, err := s.conversations.GetSystem(ctx, orgID, conversationID)
	if err != nil {
		delegateLog.Warn("read the conversation to tell its delegator failed; telling nobody",
			"conversation", conversationID, "org_id", orgID, "error", err)
		return nil
	}
	return conv
}

// taskSubject is the key of the entity taskID is about, for copy; empty when
// it cannot be read, which leaves the copy without it.
func (s *Spawner) taskSubject(ctx context.Context, orgID, taskID string) string {
	if s.tasks == nil || taskID == "" {
		return ""
	}
	task, err := s.tasks.GetSystem(ctx, orgID, taskID)
	if err != nil || task == nil {
		return ""
	}
	return task.EntitySourceID
}
