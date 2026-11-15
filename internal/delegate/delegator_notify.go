package delegate

import (
	"context"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/wsevent/toast"
)

// What a run tells people about itself, and whom. A conversation a person
// delegated tells that person, and nobody else: a toast is for them alone, and
// a failure or a park that waits on them is a notification for them
// (kinds.ConversationFailed, kinds.ConversationPaused), because they are often
// not watching when it happens. A conversation a trigger fired tells nobody;
// the board shows how it went.
//
// The toast goes from the process the run ends on. The notifications do not:
// that process is often an executor, which may not write them, so the brain's
// DelegatorNotices sweep posts them from the conversation rows, and the run
// only rings its doorbell (kickDelegatorNotice) once the row records what
// happened.

// announcePark tells the delegator about a park that just landed: the paused
// notification's doorbell when the row now waits on them, and a toast when the
// park is not a deliberate stop. It reads the row rather than trusting the park
// it asked for, because a stop requested in the meantime overrides the reason
// in the store, so a turn that ends while the stall watchdog stops it parks
// stalled.
//
// Only a park that is not a deliberate stop toasts. A deliberate stop
// terminates the blueprint behind it, so "resumes on the next message" would
// be a promise this build cannot keep: the claim gate refuses a parked step
// under a finished blueprint until the resume work lands. A spend-capped park
// would not keep that promise either, because a message sent while the cap is
// still reached parks it again, so its toast says when a message continues it.
func (s *Spawner) announcePark(park liveParkContext) {
	conv := s.readConversation(park.orgID, park.conversationID)
	if conv == nil {
		return
	}
	if waitsOnDelegator(conv) {
		s.kickDelegatorNotice(park.orgID)
	}
	if park.reason.Deliberate {
		return
	}
	text := fmt.Sprintf("Run %s is open — resumes on the next message", shortConversationID(park.conversationID))
	if park.spendCapped {
		text = fmt.Sprintf("Run %s is paused at the daily spend cap — send it a message once the cap resets or is raised",
			shortConversationID(park.conversationID))
	}
	toast.FireUser(s.wsHub, park.orgID, conv.CreatorUserID, toast.LevelInfo, "", text)
}

// waitsOnDelegator is ListAwaitingPersonSystem's test on a row already read,
// so a park rings the doorbell only when it waits on the person who delegated
// it, which is when the sweep it wakes may have a notice to open.
func waitsOnDelegator(conv *domain.Conversation) bool {
	return conv.CreatorUserID != "" && conv.Status == domain.StatusOpen && conv.EndedAt == nil &&
		conv.ArchivedAt == nil && conv.ParkReason.AwaitsPerson(conv.Type)
}

// notifyReadTimeout bounds announcePark's read, so a slow database delays a
// run's ending by at most this.
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
		delegateLog.Warn("read the parked conversation failed; no toast, and its notice waits for the next sweep",
			"conversation", conversationID, "org_id", orgID, "error", err)
		return nil
	}
	return conv
}
