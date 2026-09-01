package db

import (
	"context"
	"fmt"
	"strconv"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
)

// DiscoverAppInstallations reads the org's own App PEM via SecretStore.GetSystem
// (claims-free, for the system/background backfill caller), mints an
// app-level JWT, and lists the App's installations through
// GET {apiBase}/app/installations. The returned rows carry orgID and the
// normalized baseURL they were listed from, and are ready to hand to
// GitHubAppsStore.UpsertInstallation.
//
// It DISCOVERS, and that is sound for exactly one reason: this is the
// per-org-key path, where the App key IS the tenant boundary. A key only ever
// lists the installations of the App it belongs to, so every installation it
// reports unambiguously belongs to orgID and GitHub — not TF — is what
// enforces that. The deployment App has no such property, which is why it has
// its own function below rather than a flag on this one.
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
	return listInstallations(ctx, minter, orgID, baseURL)
}

// RefreshBoundInstallations lists the DEPLOYMENT App's installations and keeps
// only the ones the org has already bound.
//
// For the managed class the reconcile REFRESHES; it never DISCOVERS. It may
// update rows that already exist and may never create one. Discovery belongs
// exclusively to the bind ceremony, which is the only thing that can assert
// that an installation is this workspace's.
//
// That is the whole specification, and the filter below is the whole
// implementation of it. One App key serves every workspace, so
// GET /app/installations answers with every tenant's installations; stamping
// orgID on that answer — which is what the per-org-key path above does, and is
// correct there — would claim every other tenant's installations for whoever
// happened to reconcile, and because the removal diff runs against this org's
// own active set, nothing downstream would ever correct it.
//
// bound is the org's own active installation ids. Filtering on the ACTIVE set
// rather than every row the org has ever held is deliberate: a soft-removed row
// is an uninstall, and GitHub mints a fresh installation id on re-install, so
// reviving one would be creating a binding nobody performed.
//
// A failed or partial listing yields an error and therefore changes nothing —
// ListInstallations fails the whole call rather than returning the pages it
// managed to read, so "GitHub no longer reports this installation" can never be
// confused with "we could not finish asking".
func RefreshBoundInstallations(ctx context.Context, deployment githubapp.DeploymentApp, orgID, baseURL string, bound []string) ([]domain.OrgGitHubAppInstallation, error) {
	minter, err := deployment.Minter(ghbase.APIBase(baseURL))
	if err != nil {
		return nil, err
	}
	listed, err := listInstallations(ctx, minter, orgID, baseURL)
	if err != nil {
		return nil, err
	}

	keep := make(map[string]bool, len(bound))
	for _, id := range bound {
		keep[id] = true
	}
	out := make([]domain.OrgGitHubAppInstallation, 0, len(bound))
	for _, inst := range listed {
		if keep[inst.InstallationID] {
			out = append(out, inst)
		}
	}
	return out, nil
}

// listInstallations lists an App's installations through minter and maps them
// onto rows stamped for orgID on the host baseURL resolves to. Both classes
// read the same endpoint and mint the same shape of row; what differs is whose
// key signed the JWT, and therefore what the caller is entitled to do with the
// answer.
func listInstallations(ctx context.Context, minter *githubapp.Minter, orgID, baseURL string) ([]domain.OrgGitHubAppInstallation, error) {
	raw, err := minter.ListInstallations(ctx)
	if err != nil {
		return nil, err
	}

	// One host for the whole listing: these installations were just enumerated
	// against this base URL, so the deployment they live on is the one we asked,
	// resolved to the same string the identity rows for that GitHub are keyed
	// under.
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
//
// It takes insts as given and applies them whole. Whether a listed
// installation may become a row of this org's is settled before the call —
// by the App key itself on the per-org-key path, by RefreshBoundInstallations'
// filter on the deployment-App path — so this function is the same set-diff
// for both and holds no notion of a tenant.
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
