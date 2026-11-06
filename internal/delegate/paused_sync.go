package delegate

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
)

// DefaultPausedConversationSyncInterval is how often RunPausedConversationSync
// syncs, which bounds how long kinds.ConversationPaused stays open after its
// conversation leaves the park.
const DefaultPausedConversationSyncInterval = 30 * time.Second

// RunPausedConversationSync syncs kinds.ConversationPaused every interval
// until ctx is cancelled, in every org where it is open. The park opens it,
// from whichever process parked the conversation; this resolves it, because a
// conversation leaves its park through more doors than one process sees (a
// message that wakes it, a stop, its task closing, an archive), and a sync
// from the rows covers every one of them, including a write that failed. It
// runs once, on the brain.
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

// syncPausedConversations is one pass. An org whose read fails is left as it
// is, neither bumped nor resolved on no evidence.
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
		awaiting, err := conversations.ListAwaitingPersonSystem(ctx, orgID)
		if err != nil {
			delegateLog.Warn("read the org's paused runs failed; leaving their notifications as they are",
				"org_id", orgID, "error", err)
			continue
		}
		kinds.ConversationPaused.Sync(ctx, orgID, kinds.PausedForAll(awaiting))
	}
}
