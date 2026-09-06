package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// GitHub access is strictly either/or per org (TFAC-328): a GitHub App XOR a
// PAT is live at any moment. This file owns the transitions between the two —
// the atomic PAT→App cutover, the App→PAT full teardown, the discard of a
// staged (not-yet-live) registration — plus the inform-only reachability
// preflights that tell an admin which tracked repos a switch would leave dark.
//
// The staged bit (org_github_apps.active) is the whole mechanism: a
// registration created while a PAT is live is written active=false and the PAT
// stays the live credential until a cutover flips the bit and deletes the PAT
// in one transaction. See handleGitHubAppRegisterCallback for the staging side.
//
// All three transitions here serialize on githubAppRegRMWLockSalt keyed by org,
// the same lock the PAT bind/unbind and the two registration paths take — one
// credential slot per workspace means one transition at a time, deployment-wide.
// They share one shape, so where the guards sit isn't something a reader has to
// re-derive per handler: the pre-lock read is ADVISORY (it fails the common
// rejection fast, before spending a GitHub round-trip on a transition that's
// going to be refused anyway), the lock is taken AFTER that round-trip so a slow
// or rate-limited GitHub never pins a pool connection for its duration, and the
// re-read inside the critical section is the authoritative one. The re-read also
// rebinds what the write uses, not just what it checks — a registration that
// changed under us must not be described by the row this request first saw.
//
// The lock alone would not be enough. What makes the stale window reachable is
// that everything learned before the round-trip can be invalidated by another
// admin (or the same admin's second tab) committing a transition in it, and
// GitHubAppsStore.SetActive is an unchecked UPDATE: a cutover that passed its
// guards and then lost the registration flips zero rows, reports success, and
// still deletes the PAT — leaving the org with no GitHub credential at all.

// The refusals the transitions below state twice — once advisory, once
// authoritative. Both evaluations of a guard owe the caller the same sentence;
// which one they hit is only a matter of when they arrived.
const (
	msgAppAlreadyLive  = "the GitHub App is already the live credential"
	msgAppNotInstalled = "install the App before switching"
	msgAppIsLiveCred   = "this GitHub App is the live credential; switch the org to a PAT to remove it"
	msgAppIsStaged     = "this GitHub App is staged, not live; discard the staged registration instead"
)

// switchPATRequest carries a user-supplied org PAT for both PAT routes — the
// preflight and the switch-to. The preflight validates and discards it; the commit
// validates and stores it (and re-validating on commit is why sending it twice
// from the same client is fine).
type switchPATRequest struct {
	PAT string `json:"pat"`
}

// githubAccessDiff is the inform-only reachability diff both switch preflights
// return (TFAC-328, inform-only v1 — no bulk-untrack). tracked = reachable +
// len(dark_repos): how many of the org's tracked repos the target credential
// can reach, and which tracked repos would go dark, each with the teams that
// own it.
type githubAccessDiff struct {
	Tracked   int        `json:"tracked"`
	Reachable int        `json:"reachable"`
	DarkRepos []darkRepo `json:"dark_repos"`
}

type darkRepo struct {
	Repo  string   `json:"repo"`
	Teams []string `json:"teams"`
}

// buildAccessDiff partitions the org's tracked repos against the set of repos
// the target credential can reach (lowercased "owner/repo" slugs). A tracked
// repo present in the set is reachable; one absent is dark and carries its
// owning teams so the admin knows who's affected.
func buildAccessDiff(tracked []domain.TrackedRepoTeams, reachable map[string]bool) githubAccessDiff {
	diff := githubAccessDiff{Tracked: len(tracked), DarkRepos: []darkRepo{}}
	for _, t := range tracked {
		if reachable[strings.ToLower(t.Slug())] {
			diff.Reachable++
			continue
		}
		teams := t.Teams
		if teams == nil {
			teams = []string{}
		}
		diff.DarkRepos = append(diff.DarkRepos, darkRepo{Repo: t.Slug(), Teams: teams})
	}
	return diff
}

// reachableSlugSet collapses a repo list into a set of lowercased "owner/repo"
// slugs for the diff membership test.
func reachableSlugSet(repos []ghclient.UserRepo) map[string]bool {
	set := make(map[string]bool, len(repos))
	for _, r := range repos {
		if r.FullName != "" {
			set[strings.ToLower(r.FullName)] = true
		}
	}
	return set
}

// invalidateInstallationToken drops the cached installation token for one
// installation via the hook the resolver wires (onInstallationTokensInvalid →
// token-cache Invalidate). Fired by every path that learns the installation's
// minted tokens are dead: the installation.deleted and installation.suspend
// webhooks, and — installation by installation — the cutover and teardown
// paths below. nil-safe.
func (s *Server) invalidateInstallationToken(orgID, installationID string) {
	if s.onInstallationTokensInvalid == nil {
		return
	}
	s.onInstallationTokensInvalid(orgID, installationID)
}

// invalidateInstallationTokens is invalidateInstallationToken over a whole
// list. Used by the cutover and teardown paths so a credential that just
// changed isn't served from a stale per-installation token.
func (s *Server) invalidateInstallationTokens(orgID string, insts []domain.OrgGitHubAppInstallation) {
	for _, inst := range insts {
		s.invalidateInstallationToken(orgID, inst.InstallationID)
	}
}

// teardownAppSecrets deletes the App's Vault/keychain secrets (client_secret,
// PEM, webhook_secret) by the refs carried on the registration row. An empty
// ref (a hookless App has no webhook secret) is skipped. Run inside the same
// tx as DeleteForOrg so the row and its secrets go together.
func teardownAppSecrets(ctx context.Context, tx db.TxStores, orgID string, app *domain.OrgGitHubApp) error {
	for _, ref := range []string{app.ClientSecretRef, app.PEMRef, app.WebhookSecretRef} {
		if ref == "" {
			continue
		}
		if _, err := tx.Secrets.Delete(ctx, orgID, ref); err != nil {
			return fmt.Errorf("delete app secret %s: %w", ref, err)
		}
	}
	return nil
}

// handleGitHubAppCutover commits a staged PAT→App switch: it activates the
// registered App and deletes the org PAT in one transaction, after verifying
// the App is actually installed somewhere. After the commit XOR holds — the
// App is the live credential and no PAT remains. Org-admin only.
//
// POST /api/orgs/{org_id}/github/app/cutover
func (s *Server) handleGitHubAppCutover(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	// Advisory: refuse the common cases before spending the backfill's round
	// trip on them. The authoritative read is the one under the lock below.
	app, err := s.githubApps.GetForOrgSystem(ctx, orgID)
	if err != nil {
		internalError(w, "github-app", err)
		return
	}
	if app == nil {
		notFound(w, "github app")
		return
	}
	if app.Active {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: msgAppAlreadyLive})
		return
	}

	// Reconcile installations against GitHub (the same call the refresh
	// endpoint runs) so the install gate reflects current state, then require
	// at least one — cutting over to an App installed nowhere would dark the
	// org. The backfill works for a staged App (its active gate was removed).
	if err := s.githubApps.BackfillInstallationsFromAPI(ctx, orgID); err != nil {
		// The detail (which can carry vault/keychain topology) goes to the log,
		// not the response body — even though this is org-admin-only.
		githubAppLog.Error("cutover: backfill installations failed", "org", orgID, "error", err)
		httpx.WriteErrors(w, http.StatusBadGateway, httpx.ErrorItem{Reason: httpx.ReasonUpstreamUnavailable, Message: "failed to sync App installations from GitHub"})
		return
	}
	insts, err := s.githubApps.ListInstallationsForOrgSystem(ctx, orgID)
	if err != nil {
		internalError(w, "github-app", err)
		return
	}
	if len(insts) == 0 {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: msgAppNotInstalled})
		return
	}

	// The host both audit rows below record against.
	base, err := s.ghResolver.BaseURLFor(ctx, orgID)
	if err != nil {
		internalError(w, "github-app", err)
		return
	}

	// Serialize against every other credential transition for this org, taken
	// after the backfill above. See the file header for the shape.
	release, err := s.acquireKeyedLock(ctx, &s.githubAppRegMu, githubAppRegRMWLockSalt, orgID)
	if err != nil {
		internalError(w, "github-app", err)
		return
	}
	defer release()

	// The authoritative guards. Everything checked above raced the backfill, and
	// this is the transition with the destructive write: the PAT delete below is
	// unconditional, so committing against a registration that has since been
	// discarded or torn down destroys the only credential the org has left.
	// app is rebound, not just re-checked — the audit row names the App that is
	// actually going live.
	app, err = s.githubApps.GetForOrgSystem(ctx, orgID)
	if err != nil {
		internalError(w, "github-app", err)
		return
	}
	if app == nil {
		notFound(w, "github app")
		return
	}
	if app.Active {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: msgAppAlreadyLive})
		return
	}
	insts, err = s.githubApps.ListInstallationsForOrgSystem(ctx, orgID)
	if err != nil {
		internalError(w, "github-app", err)
		return
	}
	if len(insts) == 0 {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: msgAppNotInstalled})
		return
	}

	// Atomic flip: activate the App AND delete the org PAT in one transaction.
	// Both are app-pool writes (SetActive is org-admin-gated UPDATE; the PAT
	// delete is a Vault delete), so either both land or neither does.
	var activated *domain.OrgGitHubApp
	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var serr error
		activated, serr = tx.GitHubApps.SetActive(ctx, orgID, true)
		if serr != nil {
			return fmt.Errorf("activate app: %w", serr)
		}
		if activated == nil {
			// The authoritative read above confirmed the row moments ago, under
			// this same lock — a nil here means it vanished between that read and
			// this UPDATE. Refuse rather than proceed: the PAT delete below must
			// never run against a cutover that didn't actually activate anything.
			return fmt.Errorf("activate app: registration for org %s vanished mid-cutover", orgID)
		}
		if _, err := tx.Secrets.Delete(ctx, orgID, integrations.KeyGitHubPAT); err != nil {
			return fmt.Errorf("delete org pat: %w", err)
		}
		// The class is already byo_app — registration set it, staged or not, and
		// a cutover only flips WHICH credential is live within a system the org
		// was already in. Re-asserting it is the cheap half of a check that
		// costs nothing and catches the one bug this column invites: a settings
		// save that reset the class out from under the registration. Warn rather
		// than fail — a class that drifted is a reason to fix the class, never a
		// reason to refuse the cutover and strand the org mid-switch.
		if set, err := tx.Orgs.GetSettings(ctx, orgID); err != nil {
			return fmt.Errorf("read org settings: %w", err)
		} else if set.GitHubCredentialClass != domain.GitHubCredentialClassBYOApp {
			githubAppLog.Warn("credential class disagreed with the registration at cutover; re-asserting",
				"org", orgID, "found", set.GitHubCredentialClass, "want", domain.GitHubCredentialClassBYOApp)
		}
		if _, err := tx.Orgs.SetGitHubCredentialClass(ctx, orgID, domain.GitHubCredentialClassBYOApp); err != nil {
			return fmt.Errorf("set github credential class: %w", err)
		}
		// The cutover IS the credential change — the App goes live and
		// the PAT is destroyed in this one transaction. Record both sides, so
		// "when did this org stop using a PAT" is answerable from the log rather
		// than inferred from the App's registration date (which can predate the
		// cutover by any amount while staged).
		if err := tx.AccessChangeLog.Record(ctx, orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialSet,
			DetailJSON:  accessDetailCredentialNamed(domain.CredentialKindGitHubApp, base, app.Slug),
		}); err != nil {
			return fmt.Errorf("audit app cutover: %w", err)
		}
		return tx.AccessChangeLog.Record(ctx, orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialRemoved,
			DetailJSON:  accessDetailCredential(domain.CredentialKindGitHubPAT, base),
		})
	}); err != nil {
		internalError(w, "github-app", err)
		return
	}
	release() // idempotent; the defer stays as the early-return safety net

	githubAppLog.Info("cutover to app complete, pat deleted and app active", "org", orgID, "app_id", activated.AppID)

	// The App is now live. Drop any cached installation tokens (none should
	// exist for a previously-staged App, but be defensive) and re-resolve;
	// onGitHubChanged re-dues polling under the new credential and evicts the
	// reachable-repo cache. Fired in a goroutine like the settings-save path.
	s.invalidateInstallationTokens(orgID, insts)
	if s.onGitHubChanged != nil {
		go s.onGitHubChanged(orgID)
	}

	// The response is the resource SetActive just persisted, not a status stub
	// asserting the value the request sent — the SPA's cutoverToApp() discards
	// the body today (it re-fetches status separately), so this is a pure
	// improvement, not a wire break.
	writeJSON(w, http.StatusOK, s.githubAppStatus(ctx, orgID, userID,
		domain.GitHubCredentialClassBYOApp, activated, insts,
		s.webhookHealthDTO(ctx, orgID, activated),
	))
}

// handleGitHubAccessSwitchToPAT switches an org from its GitHub App to a PAT —
// a full App teardown. It validates the supplied PAT live, then in one
// transaction saves the PAT and tears down the App registration (row +
// installations) and its secrets. The PAT is saved first so an aborted
// teardown strands nothing. The App still exists on GitHub; the response flags
// that so the UI can point the admin at GitHub to delete it there. Org-admin
// only. Also valid from a staged state (re-committing to PAT mid-switch).
//
// POST /api/orgs/{org_id}/github/pat/switch-to
func (s *Server) handleGitHubAccessSwitchToPAT(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	var req switchPATRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	pat := strings.TrimSpace(req.PAT)
	if pat == "" {
		badRequest(w, "A GitHub personal access token is required.")
		return
	}

	// Advisory: refuse an org with nothing to switch off before spending the
	// validation round-trip on it. The authoritative read is under the lock.
	if app, err := s.githubApps.GetForOrgSystem(ctx, orgID); err != nil {
		internalError(w, "github-app", err)
		return
	} else if app == nil {
		notFound(w, "github app")
		return
	}

	base, err := s.ghResolver.BaseURLFor(ctx, orgID)
	if err != nil {
		internalError(w, "github-app", err)
		return
	}

	// Validate the PAT against the org's GitHub host (authenticated user
	// fetch) before touching anything — 422 and nothing changes on failure.
	// Keep the resolved login: switching to PAT makes it the org's GitHub
	// identity, so we persist it for OrgIdentityFor (TFAC-452).
	ghUser, err := auth.CaptureGitHubIdentity(ctx, base, pat)
	if err != nil {
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{Reason: httpx.ReasonUpstreamRejected, Message: githubPATValidationMessage(base, err), Field: "pat"})
		return
	}

	// Serialize against every other credential transition for this org, taken
	// after the validation above. See the file header for the shape.
	release, err := s.acquireKeyedLock(ctx, &s.githubAppRegMu, githubAppRegRMWLockSalt, orgID)
	if err != nil {
		internalError(w, "github-app", err)
		return
	}
	defer release()

	// The authoritative read. It carries the refs teardownAppSecrets deletes, so
	// a stale row here doesn't just mis-guard the teardown — it aims it: a
	// registration discarded and replaced during the validation window would
	// have its private key destroyed under the previous one's refs. Reading it
	// inside the section is what makes the row the teardown describes the row
	// the teardown removes.
	app, err := s.githubApps.GetForOrgSystem(ctx, orgID)
	if err != nil {
		internalError(w, "github-app", err)
		return
	}
	if app == nil {
		notFound(w, "github app")
		return
	}

	// Capture the installations before teardown so their cached tokens can be
	// invalidated after the commit.
	insts, err := s.githubApps.ListInstallationsForOrgSystem(ctx, orgID)
	if err != nil {
		internalError(w, "github-app", err)
		return
	}

	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		// Save the PAT first: an aborted teardown then leaves the org on a
		// working PAT with the App intact — recoverable from either side.
		if err := integrations.Save(ctx, tx.Secrets, orgID, auth.Credentials{GitHubURL: base, GitHubPAT: pat}); err != nil {
			return fmt.Errorf("save pat: %w", err)
		}
		// The PAT is now the org's GitHub identity (the App is torn down below) —
		// persist its login so OrgIdentityFor resolves the PAT tier (TFAC-452).
		if err := persistOrgGitHubIdentity(ctx, tx, orgID, ghUser.Login, ghUser.PrimaryEmail); err != nil {
			return fmt.Errorf("persist org github identity: %w", err)
		}
		if err := tx.GitHubApps.DeleteForOrg(ctx, orgID); err != nil {
			return fmt.Errorf("delete app: %w", err)
		}
		// The App registration is gone and the PAT saved above is the org's
		// credential — the org has moved between credential systems, so the class
		// moves with it, in this same transaction.
		if _, err := tx.Orgs.SetGitHubCredentialClass(ctx, orgID, domain.GitHubCredentialClassPAT); err != nil {
			return fmt.Errorf("set github credential class: %w", err)
		}
		// Secrets last. If this fails after DeleteForOrg has committed (in
		// Postgres the registration-row delete is in this tx; in local mode the
		// SQLite row delete already ran), the worst case is orphan secrets whose
		// refs no longer exist on any row — unreachable dead weight, never a
		// credential risk (the App row, and thus tier-1 minting, is already gone).
		if err := teardownAppSecrets(ctx, tx, orgID, app); err != nil {
			return err
		}
		// Mirror of the cutover — a PAT is bound and the App's stored key is
		// destroyed, both in this transaction, both recorded.
		if err := tx.AccessChangeLog.Record(ctx, orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialSet,
			DetailJSON:  accessDetailCredentialNamed(domain.CredentialKindGitHubPAT, base, ghUser.Login),
		}); err != nil {
			return fmt.Errorf("audit pat switch: %w", err)
		}
		return tx.AccessChangeLog.Record(ctx, orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialRemoved,
			DetailJSON:  accessDetailCredentialNamed(domain.CredentialKindGitHubApp, base, app.Slug),
		})
	}); err != nil {
		internalError(w, "github-app", err)
		return
	}
	release() // idempotent; the defer stays as the early-return safety net

	githubAppLog.Info("switched to pat, app torn down locally", "org", orgID, "app_id", app.AppID)

	// The webhook secret went with the App. Drop the receiver's cached
	// resolution now: the org verifies nothing from here on, and a cached
	// positive would keep accepting deliveries signed with the destroyed
	// secret until its TTL ran out.
	s.invalidateWebhookSecret(orgID)

	// Per-installation cached tokens die with the teardown; drop them now
	// rather than waiting out their ~1h expiry. onGitHubChanged re-dues polling
	// under the PAT and evicts the reachable-repo cache.
	s.invalidateInstallationTokens(orgID, insts)
	if s.onGitHubChanged != nil {
		go s.onGitHubChanged(orgID)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":                     "switched_to_pat",
		"github_app_deleted_locally": true,
		"github_app_settings_url":    base + "/settings/apps",
	})
}

// handleGitHubAppDiscard discards a STAGED App registration — the exit for an
// abandoned PAT→App switch. It tears down the registration (row +
// installations) and its secrets, leaving the still-live PAT untouched (so no
// poller restart). 409 for an active App: removing a live App only happens
// through switch-to-pat. Org-admin only.
//
// DELETE /api/orgs/{org_id}/github/app
func (s *Server) handleGitHubAppDiscard(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	// Advisory, and here purely to keep the shape uniform with its two siblings:
	// this handler makes no network call, so the pre-lock read buys nothing but
	// a fast rejection that never waits on the lock.
	if app, err := s.githubApps.GetForOrgSystem(ctx, orgID); err != nil {
		internalError(w, "github-app", err)
		return
	} else if app == nil {
		notFound(w, "github app")
		return
	} else if app.Active {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: msgAppIsLiveCred})
		return
	}

	// Serialize against every other credential transition for this org. See the
	// file header for the shape.
	release, err := s.acquireKeyedLock(ctx, &s.githubAppRegMu, githubAppRegRMWLockSalt, orgID)
	if err != nil {
		internalError(w, "github-app", err)
		return
	}
	defer release()

	// The authoritative guards. The staged bit is what makes this handler safe
	// at all — discarding is a teardown with no replacement credential, so the
	// 409 is the only thing standing between an abandoned-switch exit and the
	// removal of an App that a concurrent cutover has since made live.
	app, err := s.githubApps.GetForOrgSystem(ctx, orgID)
	if err != nil {
		internalError(w, "github-app", err)
		return
	}
	if app == nil {
		notFound(w, "github app")
		return
	}
	if app.Active {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: msgAppIsLiveCred})
		return
	}

	insts, err := s.githubApps.ListInstallationsForOrgSystem(ctx, orgID)
	if err != nil {
		internalError(w, "github-app", err)
		return
	}

	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		if err := tx.GitHubApps.DeleteForOrg(ctx, orgID); err != nil {
			return fmt.Errorf("delete app: %w", err)
		}
		// No App registration remains — org_github_apps holds at most one row per
		// org, and DeleteForOrg just removed it — so the org is back to the PAT
		// system it never stopped running on (the staged App was never live; the
		// PAT stayed the credential throughout). Unconditional for that reason,
		// and in this transaction so the row and the class go together.
		if _, err := tx.Orgs.SetGitHubCredentialClass(ctx, orgID, domain.GitHubCredentialClassPAT); err != nil {
			return fmt.Errorf("set github credential class: %w", err)
		}
		if err := teardownAppSecrets(ctx, tx, orgID, app); err != nil {
			return err
		}
		// The staged App was never live, but its private key WAS
		// stored — destroying it is a credential removal like any other, and the
		// log already carries the matching registration row.
		return tx.AccessChangeLog.Record(ctx, orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialRemoved,
			DetailJSON:  accessDetailCredentialNamed(domain.CredentialKindGitHubApp, "", app.Slug),
		})
	}); err != nil {
		internalError(w, "github-app", err)
		return
	}
	release() // idempotent; the defer stays as the early-return safety net

	githubAppLog.Info("discarded staged app", "org", orgID, "app_id", app.AppID)

	// Same teardown as switch-to-pat as far as the receiver is concerned: the
	// registration and its webhook secret are gone, so the cached resolution
	// has to go with them.
	s.invalidateWebhookSecret(orgID)

	// Discarding a staged App doesn't change the live credential (the PAT was
	// and stays live), so there's no poller restart — just drop any cached
	// installation tokens (a staged App never minted any, but stay defensive).
	s.invalidateInstallationTokens(orgID, insts)

	// The App still exists on GitHub after a local discard, same as after a
	// switch-to-pat — surface the host's apps-settings URL so the UI can guide
	// deletion there. Best-effort: a base-URL read failure degrades to "".
	settingsURL := ""
	if base, berr := s.ghResolver.BaseURLFor(ctx, orgID); berr == nil {
		settingsURL = base + "/settings/apps"
	} else {
		githubAppLog.Warn("discard: resolve github base failed", "org", orgID, "error", berr)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":                     "discarded",
		"github_app_deleted_locally": true,
		"github_app_settings_url":    settingsURL,
	})
}

// handleGitHubAppDisconnect tears down the org's LIVE App with nothing bound in
// its place: registration row, installations, secrets, class and host all go
// in one transaction, leaving the org in the state a fresh workspace is in —
// the one state from which every way in, the deployment's App included, is
// open. It is the door a workspace with an App of its own takes to reach the
// deployment App, which refuses a workspace that still holds a credential.
//
// Deliberately not the discard: the two are different intents. Discarding a
// staged registration abandons a switch and leaves the org running on its
// token; disconnecting the live App takes the org's GitHub access away until
// something else is bound. So this verb refuses a staged App (409) exactly as
// the discard refuses a live one — a request that meant one can never do the
// other, whichever way a concurrent cutover moves the row between the two.
//
// The host is cleared because nothing remains that uses it: an App or a PAT
// carries the host it was bound on, and a workspace with neither resolves the
// deployment default, as a fresh one does. Leaving a GHES host behind here
// would have the deployment App refuse the workspace for being on the wrong
// GitHub, with nothing on that GitHub to show for it. Org-admin only.
//
// POST /api/orgs/{org_id}/github/app/disconnect
func (s *Server) handleGitHubAppDisconnect(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	// Advisory, for a fast rejection that never waits on the lock; the
	// authoritative read is below.
	if app, err := s.githubApps.GetForOrgSystem(ctx, orgID); err != nil {
		internalError(w, "github-app", err)
		return
	} else if app == nil {
		notFound(w, "github app")
		return
	} else if !app.Active {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: msgAppIsStaged})
		return
	}

	// Serialize against every other credential transition for this org. See the
	// file header for the shape.
	release, err := s.acquireKeyedLock(ctx, &s.githubAppRegMu, githubAppRegRMWLockSalt, orgID)
	if err != nil {
		internalError(w, "github-app", err)
		return
	}
	defer release()

	// The authoritative read carries the secret refs the teardown deletes, so
	// it has to be the row the teardown removes — the same reason switch-to-pat
	// re-reads inside the section.
	app, err := s.githubApps.GetForOrgSystem(ctx, orgID)
	if err != nil {
		internalError(w, "github-app", err)
		return
	}
	if app == nil {
		notFound(w, "github app")
		return
	}
	if !app.Active {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: msgAppIsStaged})
		return
	}

	// The host, read before it is cleared: the audit row and the link out
	// both name the GitHub the App lived on.
	base, err := s.ghResolver.BaseURLFor(ctx, orgID)
	if err != nil {
		internalError(w, "github-app", err)
		return
	}

	// Captured before teardown so their cached tokens can be invalidated after
	// the commit.
	insts, err := s.githubApps.ListInstallationsForOrgSystem(ctx, orgID)
	if err != nil {
		internalError(w, "github-app", err)
		return
	}

	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		if err := tx.GitHubApps.DeleteForOrg(ctx, orgID); err != nil {
			return fmt.Errorf("delete app: %w", err)
		}
		// No registration remains and no token is bound, so the org is in the
		// rowless default: a PAT class with nothing stored, the state a fresh
		// workspace is in. In this transaction so the row and the class go
		// together.
		if _, err := tx.Orgs.SetGitHubCredentialClass(ctx, orgID, domain.GitHubCredentialClassPAT); err != nil {
			return fmt.Errorf("set github credential class: %w", err)
		}
		if err := teardownAppSecrets(ctx, tx, orgID, app); err != nil {
			return err
		}
		// Nothing is left that uses the host — the same branch the PAT
		// unbind takes once no registration remains.
		if err := integrations.ClearGitHub(ctx, tx.Secrets, orgID); err != nil {
			return fmt.Errorf("clear github host: %w", err)
		}
		orgSet, err := tx.Orgs.GetSettings(ctx, orgID)
		if err != nil {
			return fmt.Errorf("load org settings: %w", err)
		}
		orgSet.GitHubBaseURL = ""
		if _, err := tx.Orgs.UpdateSettings(ctx, orgID, orgSet); err != nil {
			return fmt.Errorf("save org settings: %w", err)
		}
		return tx.AccessChangeLog.Record(ctx, orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialRemoved,
			DetailJSON:  accessDetailCredentialNamed(domain.CredentialKindGitHubApp, base, app.Slug),
		})
	}); err != nil {
		internalError(w, "github-app", err)
		return
	}
	release() // idempotent; the defer stays as the early-return safety net

	githubAppLog.Info("disconnected live app, torn down locally", "org", orgID, "app_id", app.AppID)

	// The webhook secret went with the App; a cached positive would keep
	// accepting deliveries signed with the destroyed secret until its TTL ran
	// out. Per-installation tokens die with the teardown too. The change kick
	// re-dues polling under whatever remains — nothing, until something is
	// bound — and evicts the reachable-repo cache.
	s.invalidateWebhookSecret(orgID)
	s.invalidateInstallationTokens(orgID, insts)
	s.kickGitHubChanged(r, orgID)

	writeJSON(w, http.StatusOK, map[string]any{
		"status":                     "disconnected",
		"github_app_deleted_locally": true,
		"github_app_settings_url":    base + "/settings/apps",
	})
}

// handleGitHubAppCutoverPreflight returns the inform-only reachability diff for
// a PAT→App cutover: how many of the org's tracked repos the App's
// installations reach, and which would go dark. 404 unless an App is
// registered. Org-admin only. Persists no credentials, but it does reconcile
// the installation mirror as a side effect (so the preview is accurate) — hence
// Cache-Control: no-store below, and why it can't be treated as a pure-safe GET
// despite the verb.
//
// GET /api/orgs/{org_id}/github/app/cutover-preflight
func (s *Server) handleGitHubAppCutoverPreflight(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	// This GET has a write side-effect (the installation-mirror reconcile
	// below) and returns a 200 body, so make it explicitly uncacheable rather
	// than relying on s.api never adding cache headers — a cached 200 would
	// serve a stale preview without re-running the reconcile.
	w.Header().Set("Cache-Control", "no-store")
	ctx := r.Context()

	app, err := s.githubApps.GetForOrgSystem(ctx, orgID)
	if err != nil {
		internalError(w, "github-app", err)
		return
	}
	if app == nil {
		notFound(w, "github app")
		return
	}

	// Reconcile the installation mirror against GitHub first, the same call the
	// cutover commit runs, so the preview reflects what the cutover would
	// actually see — otherwise an admin who just installed the App (mirror still
	// empty in local mode, where no webhook arrives) would see every tracked
	// repo as dark and might wrongly abort. Best-effort: this is inform-only, so
	// a backfill failure logs and falls through to whatever's already mirrored
	// rather than failing the preview.
	if berr := s.githubApps.BackfillInstallationsFromAPI(ctx, orgID); berr != nil {
		githubAppLog.Warn("cutover-preflight: backfill installations failed, using current mirror", "org", orgID, "error", berr)
	}

	insts, err := s.githubApps.ListInstallationsForOrgSystem(ctx, orgID)
	if err != nil {
		internalError(w, "github-app", err)
		return
	}

	var reachable []ghclient.UserRepo
	if len(insts) > 0 {
		base, berr := s.ghResolver.BaseURLFor(ctx, orgID)
		if berr != nil {
			internalError(w, "github-app", berr)
			return
		}
		// Mint directly from the App PEM rather than through the resolver: the
		// App being previewed is staged (inactive) so the resolver's tier 1
		// would skip it and hand back the live PAT (which can't use the
		// installation endpoint). Per-installation failures are isolated, so a
		// partial union is fine for an inform-only preview.
		reachable, err = s.appInstallationReposUnion(ctx, orgID, base, app, insts)
		if err != nil {
			// The detail (which can carry vault/keychain topology from the PEM
			// read) goes to the log, not the response body.
			githubAppLog.Error("cutover-preflight: enumerate app repos failed", "org", orgID, "error", err)
			httpx.WriteErrors(w, http.StatusBadGateway, httpx.ErrorItem{Reason: httpx.ReasonUpstreamUnavailable, Message: "failed to enumerate App repositories"})
			return
		}
	}

	tracked, err := s.allStores.TeamGitHubRepos.ListOrgReposWithTeamsSystem(ctx, orgID)
	if err != nil {
		internalError(w, "github-app", err)
		return
	}

	writeJSON(w, http.StatusOK, buildAccessDiff(tracked, reachableSlugSet(reachable)))
}

// appInstallationReposUnion returns the union of repos every installation of
// app grants, minting installation tokens directly from the App's PEM rather
// than through the credential resolver. The resolver's tier 1 deliberately
// skips an inactive (staged) App, so during the cutover preview — which runs
// while the App is still staged — it would hand back the live PAT client (which
// 403s on /installation/repositories). Minting directly is active-agnostic,
// exactly what previewing a not-yet-live App needs. Per-installation failures
// are isolated (logged + skipped) so one bad mint doesn't blank the preview; a
// PEM/parse error that dooms every installation propagates.
func (s *Server) appInstallationReposUnion(ctx context.Context, orgID, base string, app *domain.OrgGitHubApp, insts []domain.OrgGitHubAppInstallation) ([]ghclient.UserRepo, error) {
	minter, err := s.appMinterAt(ctx, orgID, base, app)
	if err != nil {
		return nil, err
	}

	byName := make(map[string]ghclient.UserRepo)
	for _, inst := range insts {
		installID, perr := strconv.ParseInt(inst.InstallationID, 10, 64)
		if perr != nil {
			githubAppLog.Warn("cutover-preflight: bad installation id, skipping", "org", orgID, "installation", inst.InstallationID, "error", perr)
			continue
		}
		tok, terr := minter.MintInstallationToken(ctx, installID)
		if terr != nil {
			githubAppLog.Warn("cutover-preflight: mint token failed, skipping", "org", orgID, "account", inst.AccountLogin, "error", terr)
			continue
		}
		repos, lerr := ghclient.NewClient(base, tok.Value).ListInstallationRepos(ctx)
		if lerr != nil {
			githubAppLog.Warn("cutover-preflight: list repos failed, skipping", "org", orgID, "account", inst.AccountLogin, "error", lerr)
			continue
		}
		for _, repo := range repos {
			if repo.FullName == "" {
				continue
			}
			byName[strings.ToLower(repo.FullName)] = repo
		}
	}

	out := make([]ghclient.UserRepo, 0, len(byName))
	for _, repo := range byName {
		out = append(out, repo)
	}
	return out, nil
}

// patPreflightResponse is the PAT preflight body: the same reachability diff
// plus the login the PAT authenticates as. The diff fields are promoted via
// the embedded struct.
type patPreflightResponse struct {
	githubAccessDiff
	Login string `json:"login"`
}

// handleGitHubAccessPATPreflight validates a supplied PAT, enumerates its
// reach, and returns the inform-only reachability diff for an App→PAT switch
// plus the PAT's login. Does NOT store the PAT — the commit endpoint
// re-validates. Org-admin only.
//
// POST /api/orgs/{org_id}/github/pat/preflight
func (s *Server) handleGitHubAccessPATPreflight(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	var req switchPATRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	pat := strings.TrimSpace(req.PAT)
	if pat == "" {
		badRequest(w, "A GitHub personal access token is required.")
		return
	}

	base, err := s.ghResolver.BaseURLFor(ctx, orgID)
	if err != nil {
		internalError(w, "github-access", err)
		return
	}

	ghUser, err := auth.CaptureGitHubIdentity(ctx, base, pat)
	if err != nil {
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{Reason: httpx.ReasonUpstreamRejected, Message: githubPATValidationMessage(base, err), Field: "pat"})
		return
	}

	repos, err := ghclient.NewClient(base, pat).ListUserRepos(ctx)
	if err != nil {
		// The detail (ListUserRepos folds GitHub's response body into the
		// error) goes to the log, not the response body.
		githubAccessLog.Error("pat preflight: enumerate repos failed", "org", orgID, "error", err)
		httpx.WriteErrors(w, http.StatusBadGateway, httpx.ErrorItem{Reason: httpx.ReasonUpstreamUnavailable, Message: "failed to enumerate repositories for that token"})
		return
	}

	tracked, err := s.allStores.TeamGitHubRepos.ListOrgReposWithTeamsSystem(ctx, orgID)
	if err != nil {
		internalError(w, "github-access", err)
		return
	}

	writeJSON(w, http.StatusOK, patPreflightResponse{
		githubAccessDiff: buildAccessDiff(tracked, reachableSlugSet(repos)),
		Login:            ghUser.Login,
	})
}

func githubPATValidationMessage(base string, err error) string {
	if errors.Is(err, auth.ErrGitHubEmailPermission) {
		return "That token needs read access to GitHub email addresses. For a classic token, add the user:email scope."
	}
	if errors.Is(err, auth.ErrGitHubPrimaryEmailUnavailable) {
		return "GitHub did not return a verified primary email for that account."
	}
	return "That token didn't validate against " + base + ". Double-check it and try again."
}
