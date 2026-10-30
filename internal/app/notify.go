package app

import "github.com/sky-ai-eng/triage-factory/internal/notify"

// buildNotify installs durable notifications, on every server role: a
// producer raises a notification from whichever process it runs in, and the
// ping reaches every control pod through the hub's backplane. The host-cli
// and jail-cli identities never get here, so their verbs stay no-ops.
func (a *App) buildNotify() {
	notify.Install(notify.Backend{
		Tx:              a.stores.Tx,
		Notifications:   a.stores.Notifications,
		Orgs:            a.stores.Orgs,
		LinearTeamRules: a.stores.LinearTeamRules,
		Hub:             a.wsHub,
		Local:           a.local(),
	})
}
