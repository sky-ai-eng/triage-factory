package poller

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
)

// appRegistration reads the org's own App registration for the cycle's
// before/after comparison: nil when the org has none (a PAT or managed org).
// ok is false when the read fails, which is logged; the registration is then
// nil too. A missed comparison costs one transition log line, and the resolver
// reads the row itself before it would mint from it.
func (m *Manager) appRegistration(ctx context.Context, orgID string) (app *domain.OrgGitHubApp, ok bool) {
	if m.apps == nil {
		return nil, false
	}
	app, err := m.apps.GetForOrgSystem(ctx, orgID)
	if err != nil {
		githubLog.WarnContext(ctx, "read app registration failed", "org", orgID, "error", err)
		return nil, false
	}
	return app, true
}

// notifyAppUsability tells the org's admins while GitHub refuses the org's
// active App, from the registration the reconcile left. It is a Sync over at
// most one App rather than a Set on this one, so an occurrence for an App the
// org no longer holds resolves too: one replaced by a new App, or removed in
// favor of the deployment App, which leaves no registration at all.
//
// A staged App raises nothing. The PAT it would replace still polls, and the
// cutover that would make it live refuses an App GitHub rejects, with the same
// reason the Settings banner already shows.
func notifyAppUsability(ctx context.Context, orgID string, app *domain.OrgGitHubApp) {
	var unusable []kinds.GitHubApp
	if app != nil && app.Active && app.Unusable() {
		unusable = append(unusable, kinds.GitHubApp{AppID: app.AppID, Slug: app.Slug, Reason: app.UnusableReason})
	}
	kinds.GitHubAppUnusable.Sync(ctx, orgID, unusable)
}

// logAppUsabilityChange is the logging contract for GitHub accepting the org's
// App: one Warn when the reconcile finds it unusable (or unusable for a new
// reason), one Info when a listing succeeds again, and nothing while it stays
// where it was. before and after are the registration either side of the
// reconcile; a registration swapped for a different App in between has no
// "before" to compare against.
func logAppUsabilityChange(ctx context.Context, orgID string, before, after *domain.OrgGitHubApp, reconcileErr error) {
	if after == nil {
		return
	}
	var previous domain.GitHubAppUnusableReason
	var previousSince time.Time
	if before != nil && before.AppID == after.AppID {
		previous, previousSince = before.UnusableReason, before.UnusableSince
	}
	switch {
	case after.UnusableReason == previous:
	case after.Unusable():
		githubLog.WarnContext(ctx, "github app no longer accepted by GitHub; polling stopped until it is replaced",
			"org", orgID, "app_id", after.AppID, "slug", after.Slug, "reason", after.UnusableReason,
			"active", after.Active, "error", reconcileErr)
	default:
		attrs := []any{"org", orgID, "app_id", after.AppID, "previous_reason", previous}
		if !previousSince.IsZero() {
			attrs = append(attrs, "unusable_for", time.Since(previousSince).Round(time.Second))
		}
		githubLog.InfoContext(ctx, "github app accepted by GitHub again", attrs...)
	}
}
