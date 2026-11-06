package delegate

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
)

// DefaultPausedConversationSyncInterval is how often RunPausedConversationSync
// syncs, which bounds how long kinds.ConversationPaused stays open after its
// conversation leaves the park.
const DefaultPausedConversationSyncInterval = 30 * time.Second

// RunPausedConversationSync resolves kinds.ConversationPaused every interval
// until ctx is cancelled, in every org where it is open. The park opens it,
// from whichever process parked the conversation; this resolves it, because a
// conversation leaves its park through more doors than one process sees (a
// message that wakes it, a stop, its task closing, an archive), and reading
// the rows covers every one of them. It runs once, on the brain.
func RunPausedConversationSync(ctx context.Context, conversations db.ConversationStore, interval time.Duration) {
	if conversations == nil {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			syncPausedConversations(ctx, conversations)
		}
	}
}

// syncPausedConversations is one pass. It resolves, and opens only a
// conversation that parked during the pass (reopenParkedDuring): an
// occurrence whose conversation still waits is kept as it is, and one whose
// conversation does not is resolved. So a conversation parked before anything
// opened the kind, or whose park failed to open it, is not raised here, and a
// pass writes nothing for a conversation that goes on waiting. An org whose
// read fails is left as it is.
func syncPausedConversations(ctx context.Context, conversations db.ConversationStore) {
	orgs, err := kinds.ConversationPaused.OpenOrgs(ctx)
	if err != nil {
		delegateLog.Warn("list orgs with a paused-run notification failed; resolving none this pass", "error", err)
		return
	}
	for _, orgID := range orgs {
		if ctx.Err() != nil {
			return
		}
		before, err := conversations.ListAwaitingPersonSystem(ctx, orgID)
		if err != nil {
			delegateLog.Warn("read the org's paused runs failed; leaving their notifications as they are",
				"org_id", orgID, "error", err)
			continue
		}
		kinds.ConversationPaused.Sync(ctx, orgID, nil, kinds.PausedForAll(before)...)
		reopenParkedDuring(ctx, conversations, orgID, before)
	}
}

// reopenParkedDuring opens the conversations that parked while a pass was
// between its read and its sync. A park opens its own occurrence, after the
// read, so the sync, told to keep only what the read saw, resolved it.
// Anything waiting now that the read did not see is one of those, or parked
// since, where a second open is a no-op; it is opened whether or not its park
// opened it.
func reopenParkedDuring(ctx context.Context, conversations db.ConversationStore, orgID string, before []domain.ConversationAwaitingPerson) {
	after, err := conversations.ListAwaitingPersonSystem(ctx, orgID)
	if err != nil {
		delegateLog.Warn("re-read the org's paused runs failed; one that parked during this pass may stay resolved until it parks again",
			"org_id", orgID, "error", err)
		return
	}
	seen := make(map[string]bool, len(before))
	for _, a := range before {
		seen[a.ConversationID] = true
	}
	for _, a := range after {
		if !seen[a.ConversationID] {
			kinds.ConversationPaused.Set(ctx, orgID, kinds.PausedFor(a), true)
		}
	}
}
