package delegate

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// DefaultOrphanedConversationSweepInterval is how often
// RunOrphanedConversationSweep parks orphans, which bounds how long one goes
// on reading as mid-flight.
const DefaultOrphanedConversationSweepInterval = time.Minute

// RunOrphanedConversationSweep parks the conversations the claim gate will
// never drive again (ConversationQueueStore.ParkOrphanedConversationsSystem)
// at once and then every interval, until ctx is cancelled. It runs once, on
// the brain: the park reads every org's rows, and a process starting is no
// reason to read them again.
func RunOrphanedConversationSweep(ctx context.Context, queue db.ConversationQueueStore, interval time.Duration) {
	if queue == nil {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		parkOrphanedConversations(ctx, queue)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// parkOrphanedConversations is one pass. Each orphan it parks is logged at
// error: the writers that end a run settle its conversations in the same
// transaction, so an orphan means one of them broke, and the log line is what
// lets someone find which. A failed pass is retried on the next tick.
func parkOrphanedConversations(ctx context.Context, queue db.ConversationQueueStore) {
	if ctx.Err() != nil {
		return
	}
	parked, err := queue.ParkOrphanedConversationsSystem(ctx)
	if err != nil {
		dispatchLog.Warn("park orphaned conversations failed; retrying on the next sweep", "error", err)
		return
	}
	for _, o := range parked {
		var step any
		if o.StepIndex != nil {
			step = *o.StepIndex
		}
		dispatchLog.Error("parked a conversation its blueprint run will never drive again — a run's conversations are settled with the run, so none should be left mid-flight",
			"org_id", o.OrgID, "conversation", o.ConversationID, "blueprint_run", o.BlueprintRunID,
			"step", step, "run_status", o.RunStatus, "run_cancel_requested", o.RunCancelRequested,
			"run_current_step", o.RunCurrentStepIndex, "park_reason", o.ParkReason())
	}
}
