package app

import (
	"github.com/sky-ai-eng/triage-factory/internal/llmcred"
	"github.com/sky-ai-eng/triage-factory/internal/memoryprovision"
	"github.com/sky-ai-eng/triage-factory/internal/systemllm"
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
		func(orgID, taskID, taskStatus string) {
			// The task's wait just ended: the same task_updated shape every
			// other producer emits, so the Board patches the card and the
			// composer re-enables without polling for it.
			a.wsHub.Broadcast(websocket.Event{
				Type:  "task_updated",
				OrgID: orgID,
				Data:  map[string]string{"task_id": taskID, "status": taskStatus},
			})
		},
	)
}
