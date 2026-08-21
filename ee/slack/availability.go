package slack

import (
	"context"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
)

// init plugs Slack into core's per-org event-source availability, the third
// core seam this package registers into (after the routing hooks and the
// dormancy gate) and the same shape as the other two: core holds no Slack
// symbol, and the answer is computed here where the workspace store is.
//
// Registered from init() rather than install() because the probe captures
// nothing — it reads the caller's own transaction — and init() runs exactly
// once per process, which is what the registry's duplicate panic wants.
//
// MultiOnly, because every route in this package 404s in local mode: the
// workspace store is Postgres-only, so a local deployment cannot connect a
// workspace at all. Reporting `unlicensed` there would be a claim about a
// purchasing decision local mode never asks anyone to make.
func init() {
	eventsource.Register("slack", eventsource.Registration{
		Probe:     availability,
		MultiOnly: true,
	})
}

// availability splits three ways, and the split is the point: an org without
// the entitlement is told the licence is missing (not its concern to fix), an
// entitled org with no connected workspace is told setup is missing (exactly
// its concern), and only a connected one is available.
//
// The connection test is the presence of a workspace ROW, never a
// workspaceView's live Socket Mode status: that status is absent for a
// webhook-only app and before the reconciler's first pass, so keying on it
// would report a perfectly healthy Events API workspace as unconfigured.
func availability(ctx context.Context, tx db.TxStores, orgID string) (eventsource.State, error) {
	if !entitlements.For(orgID).Has(entitlements.FeatureSlack) {
		return eventsource.StateUnlicensed, nil
	}
	bundle := slackstore.FromTx(tx)
	if bundle == nil {
		// This package is linked, so its store factories are too; reaching
		// here means a transaction built without them.
		return eventsource.StateUnconfigured, nil
	}
	workspaces, err := bundle.Workspaces.ListForOrg(ctx, orgID)
	if err != nil {
		return "", err
	}
	if len(workspaces) == 0 {
		return eventsource.StateUnconfigured, nil
	}
	return eventsource.StateAvailable, nil
}
