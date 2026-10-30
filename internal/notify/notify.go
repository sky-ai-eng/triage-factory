// Package notify is durable notifications (specs/durable-notifications.md):
// the kinds, the three verbs a producer calls, and everything a call site
// does not do — audience resolution, copy, coalescing, resolution, the
// websocket ping and failure handling.
//
// A kind is a package-level value built by Condition or Notice, in
// internal/notify/kinds or an extension's own package. A producer integrates
// with one or two lines:
//
//	kinds.LinearTeamUnreadable.Sync(ctx, orgID, unreadable)
//	notify.ClearSource(ctx, orgID, "linear")
//
// No verb returns a value. A failure is logged and counted in
// tf_notify_write_failures_total{kind}, and never fails the work that raised
// it; Set and Sync correct it on the next evaluation.
//
// A verb opens its own transaction, so call it outside any of the producer's:
// local mode's database has one connection, and a verb inside a transaction
// that holds it waits forever.
//
// Every verb is a no-op until Install. The server installs it at boot; the
// host-cli and jail-cli identities never do, which is correct for both.
package notify

import (
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/notify/internal/sink"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// EventType is the payload-free websocket ping sent when a row opens,
// escalates or resolves.
const EventType = "notifications_updated"

// Broadcaster is the hub surface the ping goes through; *websocket.Hub
// satisfies it, and its backplane carries a broadcast to every control pod.
type Broadcaster interface {
	Broadcast(websocket.Event)
}

// Backend is what the verbs write through.
type Backend struct {
	// Tx carries each call's writes in one transaction.
	Tx db.TxRunner
	// Notifications resolves a ping's recipients once the writes commit.
	Notifications db.NotificationStore
	// Orgs and LinearTeamRules are the tracking resolvers' reads.
	Orgs            db.OrgsStore
	LinearTeamRules db.LinearTeamRulesStore
	Hub             Broadcaster
	// Local is N=1: every audience resolves to the one user, and a ping is
	// one org envelope.
	Local bool
}

// Install makes the verbs write through b. The server calls it once at boot.
func Install(b Backend) {
	sink.Swap(&engine{b: b})
}

// Installed reports whether Install has run.
func Installed() bool {
	_, ok := sink.Load().(*engine)
	return ok
}
