package app

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/cred/llmcred"
	"github.com/sky-ai-eng/triage-factory/internal/pgnotify/wakebus"
	"github.com/sky-ai-eng/triage-factory/internal/systemjob/memoryprovision"
	"github.com/sky-ai-eng/triage-factory/internal/systemjob/systemllm"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// buildMemoryProvisioner constructs the brain-side generator for the memory a
// conversation owes when it ended without its agent having written one.
//
// Unlike buildCredProvisioner beside it, the gate is a.plan.brain alone rather
// than brain-in-multi: local is always the brain, an ending there owes a memory
// exactly as one in a fleet does, and the same code settles it at N=1.
//
// The seams are the ones every system call takes here — the process's one
// ledger recorder, the org's background-jobs model resolved per attempt, and
// in multi the role-aware credential resolver (nil in local, which sends the
// call through the agent runtime against the host's own environment, the way
// the scorer and the profiler do).
func (a *App) buildMemoryProvisioner() {
	if !a.plan.brain {
		return
	}
	a.memoryProvisioner = memoryprovision.NewManager(
		a.stores,
		a.llmRecorder,
		systemllm.NewModelFunc(a.stores.Orgs),
		a.runSecrets,
		llmcred.SystemEnvResolver(a.llmResolver, "tf-memory"),
		func(orgID, _, _ string) {
			// What the task's read says about its memory wait just changed:
			// an attempt opened, failed, or ended the wait. The same
			// payload-free task_updated every other producer emits, so a
			// screen showing the task refetches it without polling.
			a.wsHub.Broadcast(websocket.Event{
				Type:  "task_updated",
				OrgID: orgID,
				Data:  map[string]any{},
			})
		},
		a.memoryFiled,
	)
}

// memoryFiled wakes the dispatchers when the provisioner files a memory row in
// orgID. The claim filter holds a task's queued conversation while an ended
// conversation on that task has no memory row, so the row landing is what
// makes that conversation claimable, and no enqueue's wake can stand in for
// it: each of those is sent while the filter still refuses the row.
//
// The provisioner runs on the brain. Where the brain also runs the dispatcher
// (local), the wake is in-process; a control pod has no dispatcher and
// publishes on tf_wake, which every executor's dispatcher listens on.
// Best-effort either way: a wake that is lost costs the conversation one scan
// interval, never its claim.
func (a *App) memoryFiled(orgID string) {
	if a.plan.dispatcher {
		a.spawner.WakeDispatcher()
		return
	}
	if err := wakebus.Publish(context.Background(), a.database, wakebus.KindMemory, orgID); err != nil {
		appLog.Warn("tf_wake publish failed after a memory was filed; the queued conversation waits for the dispatcher's next scan",
			"org", orgID, "error", err)
	}
}
