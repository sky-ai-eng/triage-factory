package app

import (
	"context"

	"go.opentelemetry.io/otel"

	"github.com/sky-ai-eng/triage-factory/internal/commitsigner"
	"github.com/sky-ai-eng/triage-factory/internal/ctlbus"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// buildCommitSigning wires commit signing. In multi mode that is the control
// plane: on every control pod the signer that answers requests from any
// executor, and on every executor the requester a sandboxed run's sign_digest
// queues through, with the doorbells its answers ring. Local mode signs in the
// TF process, in each run's daemon, so it hands the spawner the budgets those
// daemons enforce. In both modes a brain-capable process builds the signer
// health checks, which registerSubscribers kicks off the GitHub poll. After
// buildExecution, which builds the spawner.
func (a *App) buildCommitSigning() {
	if a.plan.brain {
		a.signerHealth = &commitsigner.HealthChecker{Stores: a.stores, GitHub: a.ghResolver}
	}
	if runmode.Current() != runmode.ModeMulti {
		if a.plan.signsCommits && a.spawner != nil {
			a.spawner.SetCommitSigningBudgets(a.commitSigningBudgets)
			commitsigner.RegisterLimitGauge(otel.GetMeterProvider(), a.commitSigningBudgets)
		}
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
