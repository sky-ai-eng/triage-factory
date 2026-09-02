package db

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
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
	return keepBound(listed, orgID, bound), nil
}

// ManagedInstallationSet is one managed workspace's bound installation set: the
// org, the GitHub it lists against (the org's configured base URL, "" for
// github.com), and the installation ids the bind ceremony wrote for it.
type ManagedInstallationSet struct {
	OrgID   string
	BaseURL string
	Bound   []string
}

// RefreshManagedInstallationSets is the deployment-scoped sibling of
// RefreshBoundInstallations: it refreshes EVERY managed workspace's bound
// installations from as few listings as the sets allow — one per GitHub host
// they name, which for a deployment App is one — and fans each answer out to
// the orgs that bound the installations in it.
//
// The scope is the whole reason it exists beside its per-org sibling rather
// than being a loop over it. Under a shared key GET /app/installations returns
// the same answer whoever asks, so a listing per org per cycle would spend one
// shared rate budget N times for N identical, Link-paginated responses. One
// listing also makes uninstall detection a diff against a single consistent
// snapshot rather than N taken at different moments, and makes
// refresh-never-discovers structural: an installation nobody bound has no row to
// write to, so there is no filter here that a later change could relax.
//
// It holds the same invariants as the sibling, with the same mechanics. Every
// row it writes is one of a set's Bound ids, stamped with that set's org, so a
// listed installation no set names is skipped and never written. A failed or
// partial listing for a host changes nothing on that host — the error is
// carried to the return and the other hosts still refresh — because "we could
// not ask" must never be actioned as "GitHub no longer reports this". A
// per-org write failure is likewise carried rather than aborting the fan-out:
// one workspace's write failing is no reason to leave every other workspace
// stale until the next cycle.
//
// Sets are grouped by the host their base URL resolves to (EffectiveGitHubHost),
// which is the host every row is keyed under, so two spellings of github.com
// share one listing. Hosts are walked in sorted order for a deterministic
// request sequence.
//
// upsert writes one row (its OrgID already stamped); markRemoved soft-removes
// one of orgID's installations. Both are the store's own pool-bound writers,
// as with ReconcileInstallations.
func RefreshManagedInstallationSets(
	ctx context.Context,
	deployment githubapp.DeploymentApp,
	sets []ManagedInstallationSet,
	upsert func(domain.OrgGitHubAppInstallation) error,
	markRemoved func(orgID, installationID string) error,
) error {
	byHost := make(map[string][]ManagedInstallationSet)
	for _, set := range sets {
		host := EffectiveGitHubHost(set.BaseURL)
		byHost[host] = append(byHost[host], set)
	}
	hosts := make([]string, 0, len(byHost))
	for host := range byHost {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)

	var firstErr error
	fail := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	for _, host := range hosts {
		if err := ctx.Err(); err != nil {
			return err
		}
		group := byHost[host]
		baseURL := group[0].BaseURL
		minter, err := deployment.Minter(ghbase.APIBase(baseURL))
		if err != nil {
			// No deployment App is no listing for ANY host, and nothing else
			// this loop could learn on a later iteration — so it is the one
			// failure that ends the pass rather than being carried past.
			return err
		}
		// Listed once per host with no org stamped: the org is a property of
		// each set, applied as its rows are kept below.
		listed, err := listInstallations(ctx, minter, "", baseURL)
		if err != nil {
			fail(fmt.Errorf("list installations on %s: %w", host, err))
			continue
		}
		for _, set := range group {
			kept := keepBound(listed, set.OrgID, set.Bound)
			orgID := set.OrgID
			err := ReconcileInstallations(kept, set.Bound,
				upsert,
				func(installationID string) error { return markRemoved(orgID, installationID) },
			)
			if err != nil {
				fail(fmt.Errorf("reconcile managed installations for org %s: %w", orgID, err))
			}
		}
	}
	return firstErr
}

// ScanManagedInstallationSets folds rows of (org_id, base_url, installation_id),
// ordered by org, into one ManagedInstallationSet per org. Both dialects read
// the same three columns in the same order, so the fold lives here rather than
// twice.
func ScanManagedInstallationSets(rows *sql.Rows) ([]ManagedInstallationSet, error) {
	var out []ManagedInstallationSet
	for rows.Next() {
		var orgID, baseURL, installationID string
		if err := rows.Scan(&orgID, &baseURL, &installationID); err != nil {
			return nil, fmt.Errorf("scan managed installation set: %w", err)
		}
		if n := len(out); n > 0 && out[n-1].OrgID == orgID {
			out[n-1].Bound = append(out[n-1].Bound, installationID)
			continue
		}
		out = append(out, ManagedInstallationSet{OrgID: orgID, BaseURL: baseURL, Bound: []string{installationID}})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read managed installation sets: %w", err)
	}
	return out, nil
}

// keepBound narrows a listing to the installations in bound, each stamped as
// orgID's. It is the whole of the managed filter: a row leaves here only if the
// bind ceremony wrote its id for this org, which is what "never discovers"
// means mechanically.
func keepBound(listed []domain.OrgGitHubAppInstallation, orgID string, bound []string) []domain.OrgGitHubAppInstallation {
	keep := make(map[string]bool, len(bound))
	for _, id := range bound {
		keep[id] = true
	}
	out := make([]domain.OrgGitHubAppInstallation, 0, len(bound))
	for _, inst := range listed {
		if keep[inst.InstallationID] {
			inst.OrgID = orgID
			out = append(out, inst)
		}
	}
	return out
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
