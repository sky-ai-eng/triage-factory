package app

import (
	"context"

	"go.opentelemetry.io/otel"

	"github.com/sky-ai-eng/triage-factory/internal/commitsigner"
	"github.com/sky-ai-eng/triage-factory/internal/ctlbus"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// buildCommitSigning wires multi mode's control-plane commit signing: on every
// control pod the signer that answers requests from any executor, and on every
// executor the requester a sandboxed run's sign_digest queues through, with the
// doorbells its answers ring. Local mode signs in the TF process and builds
// neither. After buildExecution, which builds the spawner the requester is
// handed to.
func (a *App) buildCommitSigning() {
	if runmode.Current() != runmode.ModeMulti {
		return
	}
	publish := func(ctx context.Context, msg ctlbus.Message) error {
		return ctlbus.Publish(ctx, a.database, msg)
	}
	if a.plan.signsCommits {
		a.commitSigner = commitsigner.NewControlPlane(a.stores, a.commitSigningBudgets, a.identity.ID, publish)
		commitsigner.RegisterLimitGauge(otel.GetMeterProvider(), a.commitSigningBudgets)
	}
	if a.plan.dispatcher {
		a.commitSignDoorbells = commitsigner.NewDoorbells()
		a.spawner.SetCommitSignRequester(&commitsigner.Requester{
			Signatures: a.stores.CommitSignatures,
			Publish:    publish,
			Doorbells:  a.commitSignDoorbells,
		})
	}
}
