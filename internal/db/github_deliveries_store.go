package db

import (
	"context"
	"time"
)

// GitHubDeliveryPruneAge is how long a recorded delivery is retained before
// the opportunistic prune sweeps it. Comfortably past the burst of
// redeliveries that follows an outage, and deliberately short of GitHub's
// 30-day redelivery window: a copy replayed a week later re-applies, which is
// an operator deliberately re-running one delivery rather than the automatic
// retry this dedup absorbs.
const GitHubDeliveryPruneAge = 72 * time.Hour

// GitHubDeliveryStore owns github_webhook_deliveries — the dedup record of
// GitHub App webhook deliveries the receiver has already applied.
//
// GitHub never auto-retries a failed delivery, but it redelivers on demand
// (the Redeliver button; POST /app/hook/deliveries/{delivery_id}/attempts),
// which is the recovery path an operator uses after an outage. Without this
// record a redelivery re-runs the installation upsert, re-publishes to the
// bus, and lands on its refresh marks again, so a redelivery is ordinary
// traffic the receiver has to be able to recognize.
//
// Keyed (installation_id, delivery_id), not (org_id, delivery_id): the
// org-keyed form cannot be written before the tenant is known, which is
// exactly the shape a shared-App receiver has. A delivery that carries no
// installation object keys on the delivery id alone (installationID "").
//
// Admin-pool-only in Postgres, same posture as InstanceStore /
// PollReadinessStore: the receiver is pre-auth, holding a trusted org_id from
// the URL path and no request claims for an RLS policy to gate on. SQLite is
// the same store against the one connection — a local install GitHub can
// reach registers a live hook URL and runs the same receiver, so this is a
// real implementation in both dialects rather than a local stub.
type GitHubDeliveryStore interface {
	// MarkDeliveredSystem records one (installationID, deliveryID) delivery
	// via an insert-on-conflict.
	//
	// fresh=true means this delivery was not previously recorded and the
	// caller should apply it; fresh=false means a duplicate, which is
	// terminal for that delivery — the caller applies nothing and acks.
	//
	// A content delivery is recorded on the store SystemTxStores hands, in
	// the transaction that writes its refresh marks, so the record commits
	// only with them: if the marks fail, the delivery stays unrecorded and a
	// redelivery applies it. A concurrent copy of the same delivery waits for
	// that transaction, and reads as a duplicate once it commits and as fresh
	// if it rolls back.
	//
	// An installation delivery is recorded on its own, BEFORE the caller
	// applies it, which is what makes its side effects at-most-once. The
	// cost is that one whose application then fails is not replayable — the
	// installation mirror's own convergence path
	// (BackfillInstallationsFromAPI) is what corrects that, exactly as it
	// does for a delivery that never arrived.
	MarkDeliveredSystem(ctx context.Context, installationID, deliveryID string) (fresh bool, err error)

	// PruneDeliveredSystem deletes deliveries older than
	// GitHubDeliveryPruneAge. The receiver calls it best-effort after each
	// delivery is recorded, never inside the recording transaction: the
	// delete locks the stale rows it removes, so inside one it would hold
	// every concurrent delivery's prune until that transaction ended, and in
	// Postgres its failure would abort the transaction it ran in.
	PruneDeliveredSystem(ctx context.Context) error
}
