package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// Org integration credentials as first-class resources: the GitHub bot PAT and
// the Jira service credential each get an explicit bind (PUT) and unbind
// (DELETE) route, instead of riding as two fields inside the bulk org-settings
// save.
//
// The split is on blast radius, not field count. A credential write validates
// against a live host, mutates the vault, re-points the poller, and owes the
// access change-log a row; a poll interval is a column. Folding them into one
// request meant every one of those obligations had to be re-derived from which
// fields happened to be present — which is how the change-log ended up with no
// record of a PAT ever being set or revoked from Settings. Everything that
// remains on POST /api/settings/org is now pure config: it touches no secret,
// makes no outbound call, and can't destroy access as a side effect.
//
// The host stays config (github_base_url / jira_base_url on the settings route)
// because the GitHub App path needs a host with no credential in sight. The
// credential routes still take it in the body — a token is only meaningful
// against the host it authenticates to, and validating against a host the org
// hasn't committed to proves nothing — so the bind writes both together and the
// unbind clears both. Only these routes may write the secret; that's the
// property that matters.

// githubPATRequest is the PUT /github/access/pat body: the host the token
// authenticates against and the token itself, both required. There is no
// "leave blank to keep current" — a blank token means the caller had nothing to
// bind and shouldn't have called at all, which is exactly the ambiguity that
// disappears once the credential has its own route.
type githubPATRequest struct {
	BaseURL string `json:"base_url"`
	PAT     string `json:"pat"`
}

// handleGitHubPATPut binds (or rotates) the org's GitHub bot PAT — PAT_1, the
// token TF authenticates to GitHub with. It validates the token live against
// base_url, then in one transaction stores the credential, persists the
// resolved @login for the agents row, records the access-log row, and updates
// the org's base URL. Org-admin only.
//
// Deliberately NOT bound to the caller's GitHub identity: per-user identity
// (PAT_2) is captured by its own surface, so access and identity stay
// independent even when the same token is used for both.
//
// PUT /api/orgs/{org_id}/github/access/pat
func (s *Server) handleGitHubPATPut(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	var req githubPATRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}
	baseURL := strings.TrimRight(strings.TrimSpace(req.BaseURL), "/")
	pat := strings.TrimSpace(req.PAT)
	if baseURL == "" {
		badRequestField(w, "A GitHub base URL is required.", "base_url")
		return
	}
	if pat == "" {
		badRequestField(w, "A GitHub personal access token is required.", "pat")
		return
	}

	// Binding a PAT is only ever right for an org this build can place in a
	// credential system it knows. A class it can't name is refused outright
	// rather than treated as "no App row, so nothing to conflict with" — that
	// inference is the one this column exists to remove, and here it would
	// resolve to a WRITE: an org whose credential lives somewhere this build
	// can't see would silently acquire a PAT and be recorded as a PAT org.
	//
	// Only the unknown arm needs saying. A byo_app org is caught by the App-row
	// gate below with its own, more useful message, and a pat org is exactly who
	// this endpoint is for.
	if _, err := s.githubCredentialClass(ctx, orgID); err != nil {
		if errors.Is(err, ErrUnknownGitHubCredentialClass) {
			settingsOrgLog.Error("unknown github credential class; refusing to bind a pat", "org", orgID)
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "this workspace's GitHub credential is managed in a way this version doesn't recognize — don't bind a token here",
				"field": "pat",
			})
			return
		}
		internalError(w, "github-access", err)
		return
	}

	// GitHub access is strictly App XOR PAT per org: an org on an App switches
	// credentials through the dedicated switch flow, which tears the App down
	// atomically. Binding a PAT underneath a live App would leave two live
	// credentials and an ambiguous resolver.
	//
	// Checked twice. This first read is unlocked and advisory — it rejects the
	// common case before spending a network round-trip on a token we're going to
	// refuse anyway. The authoritative check is the one under the lock below.
	app, err := s.githubApps.GetForOrgSystem(ctx, orgID)
	if err != nil {
		internalError(w, "github-access", err)
		return
	}
	if app != nil {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "this workspace uses a GitHub App — use the switch flow",
			"field": "pat",
		})
		return
	}

	ghUser, err := auth.ValidateGitHub(ctx, baseURL, pat)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "GitHub: " + err.Error(),
			"field": "pat",
		})
		return
	}

	// Serialize against App registration for this org — the same lock the
	// manifest callback and the import take, so a registration can't land
	// between our XOR check and our write. Without it the two orderings both
	// break the invariant: a registration that reads "no PAT" registers itself
	// ACTIVE, and our write then puts a live PAT under a live App. Taken after
	// the network round-trip above, matching the import handler, so a slow or
	// rate-limited GitHub API window never pins a pool connection.
	release, err := s.acquireKeyedLock(ctx, &s.githubAppRegMu, githubAppRegRMWLockSalt, orgID)
	if err != nil {
		internalError(w, "github-access", err)
		return
	}
	defer release()

	// The authoritative gates: BOTH re-read inside the critical section, because
	// the advisory checks above raced everything that happened during validation.
	// The class first — it decides whether the App-row question is even the right
	// one to be asking — then the XOR gate itself.
	if _, err := s.githubCredentialClass(ctx, orgID); err != nil {
		if errors.Is(err, ErrUnknownGitHubCredentialClass) {
			settingsOrgLog.Error("unknown github credential class; refusing to bind a pat", "org", orgID)
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "this workspace's GitHub credential is managed in a way this version doesn't recognize — don't bind a token here",
				"field": "pat",
			})
			return
		}
		internalError(w, "github-access", err)
		return
	}
	if app, err := s.githubApps.GetForOrgSystem(ctx, orgID); err != nil {
		internalError(w, "github-access", err)
		return
	} else if app != nil {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "this workspace uses a GitHub App — use the switch flow",
			"field": "pat",
		})
		return
	}

	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		if err := integrations.Save(ctx, tx.Secrets, orgID, auth.Credentials{
			GitHubURL: baseURL, GitHubPAT: pat,
		}); err != nil {
			return fmt.Errorf("store credential: %w", err)
		}
		// The org credential's OWN login, so the resolver can stamp the org
		// commit-author identity on delegated-agent commits.
		if err := persistOrgGitHubLogin(ctx, tx, orgID, ghUser.Login); err != nil {
			return fmt.Errorf("persist org github login: %w", err)
		}
		orgSet, err := tx.Orgs.GetSettings(ctx, orgID)
		if err != nil {
			return fmt.Errorf("load org settings: %w", err)
		}
		orgSet.GitHubBaseURL = baseURL
		if err := tx.Orgs.UpdateSettings(ctx, orgID, orgSet); err != nil {
			return fmt.Errorf("save org settings: %w", err)
		}
		// The org is now in the PAT credential system. Written here, in the same
		// transaction as the token itself, so the class can never outlive or
		// precede the credential it describes — a crash between two separate
		// writes would leave the class lying. UpdateSettings above does not carry
		// it (it deliberately doesn't own the column), hence the separate call.
		if err := tx.Orgs.SetGitHubCredentialClass(ctx, orgID, domain.GitHubCredentialClassPAT); err != nil {
			return fmt.Errorf("set github credential class: %w", err)
		}
		return tx.AccessChangeLog.Record(ctx, orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialSet,
			DetailJSON: accessDetailCredentialNamed(
				domain.CredentialKindGitHubPAT, baseURL, ghUser.Login),
		})
	}); err != nil {
		settingsOrgLog.Error("github pat bind failed", "org", orgID, "error", err)
		internalError(w, "github-access", err)
		return
	}
	release() // idempotent; the defer stays as the early-return safety net

	s.kickGitHubChanged(r, orgID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "connected", "login": ghUser.Login})
}

// handleGitHubPATDelete unbinds the org's GitHub bot PAT — the disconnect.
// Idempotent: disconnecting an org that has nothing bound is a 200 with no
// audit row (a removal that removed nothing isn't an access change).
//
// The base URL goes with it ONLY when no App registration remains. Host and
// credential are the two halves of "this workspace is connected to GitHub", so
// a plain disconnect clears both — leaving a host behind would render a
// connected-looking, credential-less GitHub card. But an App registration keeps
// using that host: a staged App coexists with a live PAT during a PAT→App
// switch, and the resolver's base lookup falls through org_settings → the
// github_url secret → github.com, so clearing both while an App row survives
// would silently re-point a GHES org's App at github.com. Wrong host, no error.
// With an App present the disconnect touches the credential only, and the App's
// own teardown (discard / switch-to-pat) owns the host from there.
//
// Per-user GitHub identity (PAT_2) is deliberately untouched: it's owned by its
// own surface, and disconnecting the org's access doesn't unmake the fact that
// a user is @login on that host.
//
// DELETE /api/orgs/{org_id}/github/access/pat
func (s *Server) handleGitHubPATDelete(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	// Serialize against App registration for this org, and hold it across the
	// read AND the write. The read decides whether the host survives, so a
	// registration landing in between would have us clear the host out from
	// under a brand-new App — the same broken state as the unconditional clear,
	// arrived at by a race instead. There's no network call in this handler, so
	// the critical section costs nothing to widen this far.
	release, err := s.acquireKeyedLock(ctx, &s.githubAppRegMu, githubAppRegRMWLockSalt, orgID)
	if err != nil {
		internalError(w, "github-access", err)
		return
	}
	defer release()

	// Staged OR active — either way a registration is still pointing at the
	// host. A read failure must not be mistaken for "no App", which would clear
	// the host, so it aborts rather than defaulting.
	app, err := s.githubApps.GetForOrgSystem(ctx, orgID)
	if err != nil {
		internalError(w, "github-access", err)
		return
	}
	keepHost := app != nil

	// The credential class is deliberately untouched by this handler. It names
	// which credential SYSTEM the org is in, not whether that system currently
	// holds a token: an org that unbinds its PAT is still a PAT org with nothing
	// bound, and an org keeping a registration (keepHost) is still a BYO-App org.
	// Neither disconnect moves the org between systems.
	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		creds, _ := integrations.Load(ctx, tx.Secrets, orgID)
		had := creds.GitHubPAT != ""
		orgSet, err := tx.Orgs.GetSettings(ctx, orgID)
		if err != nil {
			return fmt.Errorf("load org settings: %w", err)
		}
		prevHost := orgSet.GitHubBaseURL
		if keepHost {
			// Drop the token alone. ClearGitHub would take the github_url
			// secret too, which is the resolver's second fallback.
			if _, err := tx.Secrets.Delete(ctx, orgID, integrations.KeyGitHubPAT); err != nil {
				return fmt.Errorf("clear credential: %w", err)
			}
		} else {
			if err := integrations.ClearGitHub(ctx, tx.Secrets, orgID); err != nil {
				return fmt.Errorf("clear credential: %w", err)
			}
			orgSet.GitHubBaseURL = ""
			if err := tx.Orgs.UpdateSettings(ctx, orgID, orgSet); err != nil {
				return fmt.Errorf("save org settings: %w", err)
			}
		}
		if !had {
			return nil
		}
		return tx.AccessChangeLog.Record(ctx, orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialRemoved,
			DetailJSON:  accessDetailCredential(domain.CredentialKindGitHubPAT, prevHost),
		})
	}); err != nil {
		internalError(w, "github-access", err)
		return
	}
	release() // idempotent; the defer stays as the early-return safety net

	s.kickGitHubChanged(r, orgID)
	writeJSON(w, http.StatusOK, disconnectedResponse("github"))
}

// handleJiraCredentialDelete unbinds the org's Jira service credential — the
// mirror of the PUT that handleJiraConnect serves. It clears the stored
// credential AND the base URL column in ONE transaction; that pairing is the
// whole reason this route exists, since the frontend previously had to fire
// DELETE /api/integrations/jira and then a second sparse org-settings save to
// clear the column, with a real error path for "credentials were removed, but
// clearing the saved URL failed".
//
// Per-user Jira access (the user's own stored credential + derived identity)
// is deliberately left intact — it's custodied under a separate per-user secret
// key and cleared only by its own surface.
//
// DELETE /api/orgs/{org_id}/jira/access/credential
func (s *Server) handleJiraCredentialDelete(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	var had bool
	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		creds, _ := integrations.Load(ctx, tx.Secrets, orgID)
		had = creds.JiraPAT != "" || creds.JiraAPIToken != ""
		orgSet, err := tx.Orgs.GetSettings(ctx, orgID)
		if err != nil {
			return fmt.Errorf("load org settings: %w", err)
		}
		prevHost := orgSet.JiraBaseURL
		if err := integrations.ClearJira(ctx, tx.Secrets, orgID); err != nil {
			return fmt.Errorf("clear credential: %w", err)
		}
		orgSet.JiraBaseURL = ""
		if err := tx.Orgs.UpdateSettings(ctx, orgID, orgSet); err != nil {
			return fmt.Errorf("save org settings: %w", err)
		}
		if !had {
			return nil
		}
		return tx.AccessChangeLog.Record(ctx, orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialRemoved,
			DetailJSON:  accessDetailCredential(domain.CredentialKindJiraOrg, auditJiraHost(prevHost)),
		})
	}); err != nil {
		internalError(w, "jira-access", err)
		return
	}

	// Stop the Jira poller and drop the in-memory client so it doesn't keep
	// polling with a credential that no longer exists.
	if s.onJiraChanged != nil {
		s.MarkJiraRestarted(ctx, orgID)
		go s.onJiraChanged(orgID)
	}
	writeJSON(w, http.StatusOK, disconnectedResponse("jira"))
}

// disconnectedResponse is the unbind reply, carrying the local-mode env-overlay
// caveat when it applies: TRIAGE_FACTORY_* vars shadow the stored credential on
// read, so the delete genuinely succeeded but the value keeps surfacing until
// the operator unsets the var. Saying so beats reporting a clean disconnect the
// user can see isn't one. Multi mode has no overlay.
func disconnectedResponse(integration string) map[string]any {
	resp := map[string]any{"status": "disconnected"}
	if runmode.Current() != runmode.ModeLocal {
		return resp
	}
	for _, e := range auth.EnvProvided() {
		if e != integration {
			continue
		}
		resp["warning"] = "env vars still supply this credential — unset them in your shell to fully disconnect"
		resp["env_provided"] = []string{integration}
		break
	}
	return resp
}

// kickGitHubChanged re-dues polling under a changed GitHub credential. Jira is
// marked restarted alongside it because the GitHub restart path rebuilds both
// pollers; skipping the mark would let Jira carry over a stale snapshot.
func (s *Server) kickGitHubChanged(r *http.Request, orgID string) {
	if s.onGitHubChanged == nil {
		return
	}
	s.MarkJiraRestarted(r.Context(), orgID)
	go s.onGitHubChanged(orgID)
}

// badRequestField is a 400 that names the offending input, matching the
// {error, field} shape the credential surfaces already return on a 422 so the
// frontend can highlight one control either way.
func badRequestField(w http.ResponseWriter, msg, field string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg, "field": field})
}

// recordOrgCredentialClear writes the credential_removed rows for a bulk
// "clear all tokens" sweep — one per credential that was actually stored.
func recordOrgCredentialClear(ctx context.Context, tx db.TxStores, orgID, userID string, creds auth.Credentials) error {
	var kinds []string
	if creds.GitHubPAT != "" {
		kinds = append(kinds, domain.CredentialKindGitHubPAT)
	}
	if creds.JiraPAT != "" || creds.JiraAPIToken != "" {
		kinds = append(kinds, domain.CredentialKindJiraOrg)
	}
	return recordCredentialRemovals(ctx, tx, orgID, userID, kinds)
}
