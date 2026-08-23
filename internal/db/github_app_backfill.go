package db

import (
	"context"
	"fmt"
	"strconv"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
)

// DiscoverAppInstallations reads the org's App PEM via SecretStore.GetSystem
// (claims-free, for the system/background backfill caller), mints an
// app-level JWT, and lists the App's installations through
// GET {apiBase}/app/installations. The returned rows carry orgID and the
// normalized baseURL they were listed from, and are ready to hand to
// GitHubAppsStore.UpsertInstallation — the per-org App owns every installation
// it reports (v1 is per-org Apps only).
//
// Shared by both store backends so the JWT-mint + HTTP-list + secret-read
// logic lives in one place; each backend supplies the DB read of
// (appID, pemRef, baseURL) and the upsert.
func DiscoverAppInstallations(ctx context.Context, secrets SecretStore, orgID, appID, pemRef, baseURL string) ([]domain.OrgGitHubAppInstallation, error) {
	pem, err := secrets.GetSystem(ctx, orgID, pemRef)
	if err != nil {
		return nil, fmt.Errorf("read app pem: %w", err)
	}
	if pem == "" {
		return nil, fmt.Errorf("github app pem secret %q not found for org %s", pemRef, orgID)
	}

	key, err := githubapp.ParsePrivateKey([]byte(pem))
	if err != nil {
		return nil, err
	}
	appID64, err := strconv.ParseInt(appID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse app id %q: %w", appID, err)
	}

	minter, err := githubapp.NewMinter(githubapp.Config{
		PrivateKey: key,
		AppID:      appID64,
		APIBase:    ghbase.APIBase(baseURL),
	})
	if err != nil {
		return nil, err
	}

	raw, err := minter.ListInstallations(ctx)
	if err != nil {
		return nil, err
	}

	// One host for the whole listing: these installations were just enumerated
	// through this org's App key against this base URL, so the deployment they
	// live on is the one we asked, resolved to the same string the identity
	// rows for that GitHub are keyed under.
	host := EffectiveGitHubHost(baseURL)

	out := make([]domain.OrgGitHubAppInstallation, 0, len(raw))
	for _, in := range raw {
		// A zero account id means GitHub's payload omitted it; it maps to ""
		// (and then to a SQL NULL), so the row keeps whatever id it already
		// had rather than being blanked by a partial answer.
		var accountID string
		if in.AccountID != 0 {
			accountID = strconv.FormatInt(in.AccountID, 10)
		}
		out = append(out, domain.OrgGitHubAppInstallation{
			InstallationID: strconv.FormatInt(in.ID, 10),
			OrgID:          orgID,
			AccountType:    in.AccountType,
			AccountID:      accountID,
			AccountLogin:   in.AccountLogin,
			GitHubHost:     host,
			InstalledAt:    in.CreatedAt,
			// Suspension rides in from the listing, so the reconcile converges a
			// suspend (or unsuspend) whose webhook this deployment never saw —
			// GitHub does not re-deliver one, and local mode has no delivery path
			// at all. A GitHub that reports no suspension is asserting there is
			// none, which is why the upsert writes these verbatim instead of
			// preserving what the row already had.
			SuspendedAt: in.SuspendedAt,
			SuspendedBy: in.SuspendedBy,
			// The listing is the only source that reports the grant's width on
			// every pass. A narrowing fires installation_repositories, which
			// GitHub never re-delivers and local mode never receives, so
			// "selected" learned here is how the mirror converges at all.
			RepositorySelection: in.RepositorySelection,
		})
	}
	return out, nil
}

// ReconcileInstallations applies a freshly-listed set of installations to
// the mirror: upsert every listed row, then soft-remove any previously-
// active installation GitHub no longer reports. activeIDs is the org's
// pre-backfill active installation_id set; markRemoved is called for each
// active ID absent from insts. Shared by both store backends so the
// set-diff lives in one place; each backend supplies its own pool-bound
// upsert / mark-removed / active-read.
func ReconcileInstallations(
	insts []domain.OrgGitHubAppInstallation,
	activeIDs []string,
	upsert func(domain.OrgGitHubAppInstallation) error,
	markRemoved func(installationID string) error,
) error {
	keep := make(map[string]bool, len(insts))
	for _, inst := range insts {
		if err := upsert(inst); err != nil {
			return err
		}
		keep[inst.InstallationID] = true
	}
	for _, id := range activeIDs {
		if !keep[id] {
			if err := markRemoved(id); err != nil {
				return err
			}
		}
	}
	return nil
}
