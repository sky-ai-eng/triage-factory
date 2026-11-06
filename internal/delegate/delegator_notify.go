package delegate

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
	"github.com/sky-ai-eng/triage-factory/internal/wsevent/toast"
)

// What a run tells people about itself, and whom. A conversation a person
// delegated tells that person, and nobody else: a toast is for the delegator
// alone, and a failure or a park that waits on them is a notification for them
// (kinds.ConversationFailed, kinds.ConversationPaused), because they are often
// not watching when it happens. A conversation a trigger fired tells nobody;
// the board shows how it went.

// toastDelegator fires a toast for the person who delegated a conversation.
// creatorUserID is empty for a conversation a trigger fired, which fires
// nothing.
func (s *Spawner) toastDelegator(orgID, creatorUserID string, level toast.Level, body string) {
	if creatorUserID == "" {
		return
	}
	toast.FireUser(s.wsHub, orgID, creatorUserID, level, "", body)
}

// toastConversationDelegator is toastDelegator for a site that holds only the
// conversation's id.
func (s *Spawner) toastConversationDelegator(orgID, conversationID string, level toast.Level, body string) {
	if conv := s.readConversation(orgID, conversationID); conv != nil {
		s.toastDelegator(orgID, conv.CreatorUserID, level, body)
	}
}

// noteConversationFailed tells userID that conversationID failed: the person
// the engagement ran for, which is its delegator, or on a resume the person
// whose message woke it. An empty userID, a conversation a trigger fired and
// nobody has written to, tells nobody. Best effort, like every notification.
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
// the conversation's id: it tells the conversation's delegator.
func (s *Spawner) noteDelegatorFailed(orgID, conversationID string, kind domain.ConversationFailureKind, summary string) {
	if conv := s.readConversation(orgID, conversationID); conv != nil {
		s.noteConversationFailed(orgID, conversationID, conv.CreatorUserID, conv.TaskID, kind, summary)
	}
}

// noteConversationPaused opens kinds.ConversationPaused for a conversation
// that has just parked, when the park it now holds waits on the person who
// delegated it. It reads the row rather than trusting the park it asked for,
// because a stop requested in the meantime overrides the reason in the store.
// It never resolves: the brain's sweep (RunPausedConversationSync) does, from
// the same rows, once the conversation leaves that park.
func (s *Spawner) noteConversationPaused(orgID, conversationID string) {
	conv := s.readConversation(orgID, conversationID)
	if conv == nil || conv.CreatorUserID == "" || conv.EndedAt != nil || conv.ArchivedAt != nil ||
		!conv.ParkReason.AwaitsPerson() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), notifyReadTimeout)
	defer cancel()
	kinds.ConversationPaused.Set(ctx, orgID, kinds.PausedFor(domain.ConversationAwaitingPerson{
		ConversationID: conversationID,
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
