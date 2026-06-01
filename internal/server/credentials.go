package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

type setupRequest struct {
	GitHubURL string `json:"github_url"`
	GitHubPAT string `json:"github_pat"`
	JiraURL   string `json:"jira_url"`
	JiraPAT   string `json:"jira_pat"`
	// CloneProtocol is the user's choice on the Setup wizard: "ssh"
	// (default) or "https". Empty means "use the existing config
	// value" — important because the wizard runs preflight separately
	// and may post setup multiple times during reconfiguration.
	CloneProtocol string `json:"clone_protocol"`
}

type setupResponse struct {
	GitHub *auth.GitHubUser `json:"github,omitempty"`
	Jira   *auth.JiraUser   `json:"jira,omitempty"`
}

func (s *Server) handleIntegrationsSetup(w http.ResponseWriter, r *http.Request) {
	userID := ClaimsFrom(r.Context()).Subject
	// Setup writes credentials through the SecretStore, which is
	// org-scoped — see handleSettingsPost for the multi-mode rationale.
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	var req setupRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}

	if req.GitHubURL == "" || req.GitHubPAT == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "GitHub URL and token are required"})
		return
	}

	// Multi-mode is HTTPS-only: reject an ssh selection rather than
	// store a value the clone path will ignore. SSH would need a per-org key
	// the hosted runtime has no machinery to provision, and PreflightSSH
	// (below) must never run in a container — it writes ~/.ssh/known_hosts.
	if req.CloneProtocol == "ssh" && runmode.Current() != runmode.ModeLocal {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "ssh clone protocol is not available in this deployment; use https",
			"field": "clone_protocol",
		})
		return
	}

	// Hard-block setup with SSH selected if our preflight against the
	// configured GitHub host can't authenticate. Run BEFORE the PAT
	// check so the user gets the SSH error first rather than entering
	// a valid PAT just to find out their SSH is broken on the next
	// step. The HTTPS path skips this entirely. The probe target is
	// derived from the URL the user just submitted so GHE deployments
	// see hints with their hostname, not "github.com". Local-mode only —
	// the multi-mode ssh rejection above guarantees we never reach here
	// with ssh selected outside local.
	if req.CloneProtocol == "ssh" {
		sshHost := worktree.SSHHostFromBaseURL(req.GitHubURL)
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		err := worktree.PreflightSSH(ctx, sshHost)
		cancel()
		if err != nil {
			log.Printf("[auth] blocked SSH setup against %s: preflight failed: %v", sshHost, err)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error":  fmt.Sprintf("SSH preflight against %s failed — set up your SSH key or pick HTTPS. %s", sshHost, err.Error()),
				"field":  "clone_protocol",
				"stderr": err.Error(),
			})
			return
		}
	}

	resp := setupResponse{}

	// Validate GitHub if provided
	if req.GitHubURL != "" && req.GitHubPAT != "" {
		ghUser, err := auth.ValidateGitHub(req.GitHubURL, req.GitHubPAT)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error": "GitHub: " + err.Error(),
				"field": "github",
			})
			return
		}
		resp.GitHub = ghUser
	}

	// Validate Jira if provided
	if req.JiraURL != "" && req.JiraPAT != "" {
		jiraUser, err := auth.ValidateJira(req.JiraURL, req.JiraPAT)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error": "Jira: " + err.Error(),
				"field": "jira",
			})
			return
		}
		resp.Jira = jiraUser
	}

	// One WithTx for the full persist: SecretStore (Postgres vault
	// writes need claims), org_settings (org_settings_update RLS),
	// and user_github_identities (its own user-scoped RLS) all share the
	// same claims tx so they either all commit or all roll back.
	// Local mode collapses to one SQLite tx with the same shape.
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if err := integrations.Save(r.Context(), tx.Secrets, orgID, auth.Credentials{
			GitHubURL: req.GitHubURL,
			GitHubPAT: req.GitHubPAT,
			JiraURL:   req.JiraURL,
			JiraPAT:   req.JiraPAT,
		}); err != nil {
			return fmt.Errorf("store credentials: %w", err)
		}

		// Capture the GitHub login on the users row when we validated
		// GitHub. Skip when GitHub wasn't validated (Jira-only setup)
		// — the dashboard / poller short-circuit on empty username and
		// Settings can re-capture later.
		if resp.GitHub != nil && resp.GitHub.Login != "" {
			// Bind the identity to the org's host explicitly — the host
			// the PAT just validated against (req.GitHubURL), which is the
			// same value we persist to org_settings.github_base_url below,
			// so reads keyed on (user, host) find this row.
			if err := tx.Users.UpsertGitHubIdentity(r.Context(), userID, req.GitHubURL, resp.GitHub.Login, "pat"); err != nil {
				return fmt.Errorf("persist github identity: %w", err)
			}
		}

		// Persist base URLs + clone protocol in org_settings so they
		// survive without keychain access. Read-modify-write inside
		// the same tx; the store returns DefaultOrgSettings() on a
		// missing row, so first-time setup lands a fully-populated
		// upsert rather than a partially-filled one.
		orgSet, err := tx.Orgs.GetSettings(r.Context(), orgID)
		if err != nil {
			return fmt.Errorf("load org settings: %w", err)
		}
		if req.GitHubURL != "" {
			orgSet.GitHubBaseURL = req.GitHubURL
		}
		if req.JiraURL != "" {
			orgSet.JiraBaseURL = req.JiraURL
		}
		if req.CloneProtocol == "ssh" || req.CloneProtocol == "https" {
			orgSet.GitHubCloneProtocol = req.CloneProtocol
		}
		if err := tx.Orgs.UpdateSettings(r.Context(), orgID, orgSet); err != nil {
			return fmt.Errorf("save org settings: %w", err)
		}
		return nil
	}); err != nil {
		// Log the underlying wrap-chain (SQL / vault / FK errors) for
		// operator debugging, but return a stable user-facing message
		// so we don't leak Postgres internals to API clients. Mirrors
		// the pattern handleJiraConnect now uses.
		log.Printf("[setup] handleIntegrationsSetup persist failed: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to store credentials"})
		return
	}

	// Setup always includes GitHub — trigger full restart. Mark Jira restarted
	// synchronously so jiraPollReady flips false before the async callback
	// starts, closing a race where carry-over reads stale snapshots.
	if s.onGitHubChanged != nil {
		s.MarkJiraRestarted()
		go s.onGitHubChanged(orgID)
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleIntegrationsStatus(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	// SecretStore.Load + repo count both inside the same WithTx so
	// vault_decrypt sees request.jwt.claims and repos_select RLS runs
	// under the user's identity. Local mode collapses to one SQLite
	// tx with the same shape.
	var (
		creds     auth.Credentials
		credsErr  error
		repoCount int
	)
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		creds, credsErr = integrations.Load(r.Context(), tx.Secrets, orgID)
		var e error
		repoCount, e = tx.Repos.CountConfigured(r.Context(), orgID)
		return e
	}); err != nil {
		// Status endpoint returns 200 with configured=false so the
		// frontend renders a sensible "not connected" UI even when
		// the read failed; log the underlying error server-side
		// instead of leaking it in the response body.
		log.Printf("[setup] integrations status read: %v", err)
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": false,
			"error":      "failed to load integrations status",
		})
		return
	}
	if credsErr != nil {
		log.Printf("[setup] integrations status creds load: %v", credsErr)
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": false,
			"error":      "failed to load credentials",
		})
		return
	}

	// GitHub is mandatory — configured requires GitHub creds + at least one repo
	result := map[string]any{
		"configured":   creds.GitHubPAT != "" && creds.GitHubURL != "" && repoCount > 0,
		"github":       creds.GitHubPAT != "",
		"jira":         creds.JiraPAT != "",
		"github_repos": repoCount,
		"env_provided": auth.EnvProvided(),
	}

	if creds.GitHubURL != "" {
		result["github_url"] = creds.GitHubURL
	}
	if creds.JiraURL != "" {
		result["jira_url"] = creds.JiraURL
	}

	writeJSON(w, http.StatusOK, result)
}

// DELETE /api/integrations — clears all integration credentials (GitHub
// + Jira) via SecretStore. Used by the Settings "Clear All Tokens"
// flow when the user wants a fresh slate. Granular per-integration
// clears live on subpaths (e.g. DELETE /api/integrations/jira).
//
// Env-overlay UX: if any of the four well-known integration secrets
// are supplied by TRIAGE_FACTORY_* env vars (local mode only —
// multi-mode has no env overlay), SecretStore.Delete returns ok=false
// and the value continues to surface on the next Get. Surface that to
// the user instead of silently lying that the clear succeeded.
func (s *Server) handleIntegrationsClear(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		return integrations.Clear(r.Context(), tx.Secrets, orgID)
	}); err != nil {
		internalError(w, "auth", err)
		return
	}
	resp := map[string]any{"status": "cleared"}
	if runmode.Current() == runmode.ModeLocal {
		if envs := auth.EnvProvided(); len(envs) > 0 {
			resp["warning"] = fmt.Sprintf("env vars (%v) still supply credentials — unset them in your shell to fully clear", envs)
			resp["env_provided"] = envs
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// DELETE /api/integrations/jira — clears Jira credentials only,
// preserving GitHub. Counterpart to the collection-level clear at
// DELETE /api/integrations. See the env-overlay note on
// handleIntegrationsClear for the warning shape.
func (s *Server) handleIntegrationsDeleteJira(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		return integrations.ClearJira(r.Context(), tx.Secrets, orgID)
	}); err != nil {
		internalError(w, "auth", err)
		return
	}
	// Stop the Jira poller and clear the in-memory client so it doesn't
	// keep polling with stale credentials.
	if s.onJiraChanged != nil {
		s.MarkJiraRestarted()
		go s.onJiraChanged(orgID)
	}
	resp := map[string]any{"status": "cleared"}
	if runmode.Current() == runmode.ModeLocal {
		envs := auth.EnvProvided()
		for _, e := range envs {
			if e == "jira" {
				resp["warning"] = "env vars (TRIAGE_FACTORY_JIRA_URL/PAT) still supply this credential — unset them in your shell to fully clear"
				resp["env_provided"] = []string{"jira"}
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
