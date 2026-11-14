package app

import (
	"github.com/sky-ai-eng/triage-factory/internal/cred/llmcred"
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
	)
}
