package slack

import (
	"context"
	"errors"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
)

// init plugs Slack into core's per-org event-source availability, the third
// core seam this package registers into (after the routing hooks and the
// dormancy gate) and the same shape as the other two: core holds no Slack
// symbol, and the answer is computed here where the connection store is.
//
// Registered from init() rather than install() because the probe captures
// nothing — it reads the caller's own transaction — and init() runs exactly
// once per process, which is what the registry's duplicate panic wants.
//
// MultiOnly, because every route in this package 404s in local mode: the
// connection store is Postgres-only, so a local deployment cannot connect a
// workspace at all. Reporting `unlicensed` there would be a claim about a
// purchasing decision local mode never asks anyone to make.
func init() {
	eventsource.Register("slack", eventsource.Registration{
		Probe:       availability,
		SystemProbe: availabilitySystem,
		MultiOnly:   true,
	})
}

// availability splits three ways, and the split is the point: an org without
// the entitlement is told the licence is missing (not its concern to fix), an
// entitled org with no active connection is told setup is missing (exactly
// its concern), and only an org with one is available. An org whose
// connections are all inactive has nothing that acts or receives, so it is
// unconfigured.
//
// The connection test is the presence of an active connection ROW, never a
// workspaceView's live Socket Mode status: that status is absent for a
// webhook-only app and before the reconciler's first pass, so keying on it
// would report a perfectly healthy Events API workspace as unconfigured.
func availability(ctx context.Context, tx db.TxStores, orgID string) (eventsource.State, error) {
	if !entitlements.For(orgID).Has(entitlements.FeatureSlack) {
		return eventsource.StateUnlicensed, nil
	}
	bundle := slackstore.FromTx(tx)
	if bundle == nil {
		// A FAULT, not a state. This package being linked links its store
		// factories too, so a transaction without the bundle is a miswired
		// build — and answering "unconfigured" would file that under the one
		// state a reader is invited to go fix themselves, where it can sit
		// forever looking like an unconnected workspace.
		return "", errors.New("slack store bundle absent from the transaction: the slack store extension is not registered")
	}
	conns, err := bundle.Connections.ListForOrg(ctx, orgID)
	if err != nil {
		return "", err
	}
	return availabilityOf(conns), nil
}

// availabilitySystem is availability's claims-free twin, registered as the
// SystemProbe: the same three-way split, read through the admin-pool
// connection door for the JWT-less callers eventsource.ResolveSystem
// serves. The entitlement read is process-local either way, so only the
// connection read changes doors.
func availabilitySystem(ctx context.Context, stores db.Stores, orgID string) (eventsource.State, error) {
	if !entitlements.For(orgID).Has(entitlements.FeatureSlack) {
		return eventsource.StateUnlicensed, nil
	}
	bundle := slackstore.FromStores(stores)
	if bundle == nil {
		// The same FAULT as the claims probe's nil-bundle arm, for the same
		// reason: this package being linked links its store factories too.
		return "", errors.New("slack store bundle absent from the stores: the slack store extension is not registered")
	}
	conns, err := bundle.Connections.ListForOrgSystem(ctx, orgID)
	if err != nil {
		return "", err
	}
	return availabilityOf(conns), nil
}

// availabilityOf is an entitled org's state, from its connections.
func availabilityOf(conns []slackstore.Connection) eventsource.State {
	if len(activeConnections(conns)) == 0 {
		return eventsource.StateUnconfigured
	}
	return eventsource.StateAvailable
}
