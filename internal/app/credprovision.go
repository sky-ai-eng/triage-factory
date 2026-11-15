package app

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/cred/credprovision"
	"github.com/sky-ai-eng/triage-factory/internal/pgnotify/ctlbus"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// credSealedPublishTimeout bounds the cred_sealed publish a seal makes once its
// bundle is written. The awaiting sweep seals one claim at a time, so a publish
// that stalled would hold up every claim behind it; one that times out costs
// the waiting executor one poll interval.
const credSealedPublishTimeout = 5 * time.Second

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
	a.credProvisioner.SetOnSealed(func(ctx context.Context, orgID, conversationID string) {
		ctx, cancel := context.WithTimeout(ctx, credSealedPublishTimeout)
		defer cancel()
		msg := ctlbus.Message{Kind: ctlbus.KindCredSealed, OrgID: orgID, ConversationID: conversationID}
		if err := ctlbus.Publish(ctx, a.database, msg); err != nil {
			appLog.Warn("tf_ctl cred_sealed publish failed; the waiting executor reads the bundle on its next poll",
				"conversation", conversationID, "error", err)
		}
	})
	return nil
}
