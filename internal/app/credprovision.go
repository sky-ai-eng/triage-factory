package app

import (
	"github.com/sky-ai-eng/triage-factory/internal/cred/credprovision"
	"github.com/sky-ai-eng/triage-factory/internal/pgnotify/ctlbus"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// buildCredProvisioner constructs the brain-side sealed-credential-bundle
// provisioner for brain-capable roles in multi mode, nil
// elsewhere (local mode and TF_ROLE=executor never build one). Runs after
// openStores/buildExecution
// so a.stores is the real, secret-bearing bundle (never the disabled one
// an executor holds — a.plan.brain is false there anyway).
func (a *App) buildCredProvisioner() error {
	if runmode.Current() != runmode.ModeMulti || !a.plan.brain {
		return nil
	}
	a.credProvisioner = credprovision.NewManager(a.stores, a.llmResolver, a.linearResolver)
	a.credProvisioner.SetDeploymentSigner(a.deploymentSigner)
	// The executor waiting on the bundle is on another process, so the ring
	// rides tf_ctl to whichever one listens for the conversation.
	a.credProvisioner.SetOnSealed(func(orgID, conversationID string) {
		a.publishCtl(ctlbus.Message{Kind: ctlbus.KindCredSealed, OrgID: orgID, ConversationID: conversationID})
	})
	return nil
}
