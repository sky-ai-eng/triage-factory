package poller

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// appRegistration reads the org's own App registration for the cycle's
// before/after comparison: nil when the org has none (a PAT or managed
// workspace) and when the read fails, which is logged — a missed comparison
// costs one transition log line, and the resolver reads the row itself before
// it would mint from it.
func (m *Manager) appRegistration(ctx context.Context, orgID string) *domain.OrgGitHubApp {
	if m.apps == nil {
		return nil
	}
	app, err := m.apps.GetForOrgSystem(ctx, orgID)
	if err != nil {
		githubLog.WarnContext(ctx, "read app registration failed", "org", orgID, "error", err)
		return nil
	}
	return app
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
		// TODO(TFAC-878): this line and the Settings panel are the only notice
		// an admin gets that the workspace's App was deleted on GitHub, or its
		// key was, and the panel only tells whoever opens it. A durable
		// notification for the org's admins would open here and close in the
		// branch below.
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
