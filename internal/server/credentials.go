package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/promptseed"
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
			authLog.Warn("blocked ssh setup, preflight failed", "ssh_host", sshHost, "error", err)
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
		ghUser, err := auth.ValidateGitHub(r.Context(), req.GitHubURL, req.GitHubPAT)
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
		jiraUser, err := auth.ValidateJira(r.Context(), jira.DataCenterPAT(req.JiraURL, req.JiraPAT))
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

		// NOTE: this is the ORG credential (PAT_1) — the bot token TF
		// authenticates to GitHub with. It is deliberately NOT bound to the
		// configuring user's GitHub identity. Per-user identity (PAT_2 / "this
		// TF user is @login") is captured by its own surface — the setup
		// wizard's User step / the Connect gate page, writing
		// user_github_identities directly — so access and identity never get
		// conflated. The two may carry the same token value, but they are set
		// and stored independently.

		// Persist the org credential's OWN GitHub login (the login the PAT
		// authenticates as) so the resolver's OrgIdentityFor can stamp the org
		// commit-author identity on delegated-agent commits (TFAC-452). This is
		// org ACCESS metadata on the agents row, NOT user_github_identities.
		if resp.GitHub != nil {
			if err := persistOrgGitHubLogin(r.Context(), tx, orgID, resp.GitHub.Login); err != nil {
				return fmt.Errorf("persist org github login: %w", err)
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
		// Audit the org credential binds in the same tx. The setup
		// wizard requires a GitHub PAT (guarded above) and optionally carries a
		// Jira one; each records its own row with the configured host, so the
		// change-log shows the same two binds a later Settings rotation would.
		if req.GitHubPAT != "" {
			if err := tx.AccessChangeLog.Record(r.Context(), orgID, domain.AccessChange{
				ActorUserID: userID,
				Action:      domain.AccessActionCredentialSet,
				DetailJSON: accessDetailCredentialNamed(
					domain.CredentialKindGitHubPAT, req.GitHubURL, githubLoginOf(resp.GitHub)),
			}); err != nil {
				return fmt.Errorf("audit credential set: %w", err)
			}
		}
		if req.JiraPAT != "" {
			if err := tx.AccessChangeLog.Record(r.Context(), orgID, domain.AccessChange{
				ActorUserID: userID,
				Action:      domain.AccessActionCredentialSet,
				DetailJSON:  accessDetailCredential(domain.CredentialKindJiraOrg, auditJiraHost(req.JiraURL)),
			}); err != nil {
				return fmt.Errorf("audit credential set: %w", err)
			}
		}
		return nil
	}); err != nil {
		// Log the underlying wrap-chain (SQL / vault / FK errors) for
		// operator debugging, but return a stable user-facing message
		// so we don't leak Postgres internals to API clients. Mirrors
		// the pattern handleJiraConnect now uses.
		setupLog.Error("integrations setup persist failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to store credentials"})
		return
	}

	// Setup always includes GitHub — trigger full restart. Mark Jira restarted
	// synchronously so jiraPollReady flips false before the async callback
	// starts, closing a race where carry-over reads stale snapshots.
	if s.onGitHubChanged != nil {
		s.MarkJiraRestarted(r.Context(), orgID)
		go s.onGitHubChanged(orgID)
	}

	writeJSON(w, http.StatusOK, resp)
}

// githubLoginOf returns the login a validated GitHub user resolved to, or ""
// when no validation ran (nil) — the audit detail's optional "name".
func githubLoginOf(u *auth.GitHubUser) string {
	if u == nil {
		return ""
	}
	return u.Login
}

// persistOrgGitHubLogin records the org credential's OWN GitHub login on the
// agents row so the credential resolver's OrgIdentityFor PAT tier can stamp the
// org commit-author identity on delegated-agent commits. Called
// inside the same WithTx that saves the org PAT, by every org-PAT writer that
// already validated the login (handleIntegrationsSetup, the App→PAT switch, the
// settings PAT update). This is org ACCESS metadata — it deliberately does NOT
// touch user_github_identities (the per-user PAT_2 identity surface).
//
// An empty login is a no-op (the caller had no GitHub PAT in this write). A
// missing agents row (not yet bootstrapped) is skipped rather than erroring —
// the login self-heals on the next PAT re-save. A real write error propagates so
// it rolls back with the rest of the caller's tx. The App path never calls this:
// an App org's bot login (<slug>[bot]) resolves live from the registration.
func persistOrgGitHubLogin(ctx context.Context, tx db.TxStores, orgID, login string) error {
	if login == "" {
		return nil
	}
	agent, err := tx.Agents.GetForOrg(ctx, orgID)
	if err != nil {
		return fmt.Errorf("load agent for org github login: %w", err)
	}
	if agent == nil {
		return nil // not bootstrapped yet; nothing to stamp
	}
	return tx.Agents.SetGitHubOrgLogin(ctx, orgID, agent.ID, login)
}

func (s *Server) handleIntegrationsStatus(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	// First-run detection keys on tenant existence, not GitHub creds.
	// "configured" now means "a provisioned tenant exists" — the local
	// shim hands every request the LocalDefault* org id regardless of
	// whether that org row actually exists, so probe the row directly.
	// No tenant ⇒ the user hasn't run "Start your factory" yet and
	// the AuthGate routes them to the first-run screen; GitHub-creds-
	// present is now a later config step, surfaced via the github/jira
	// fields below rather than gating first-run. GetOrgSystem reads org
	// metadata without claims (a cheap existence probe); in multi mode an
	// active org always exists, so configured is always true there and
	// the field is unused (multi gates via AuthContext).
	org, err := s.orgs.GetOrgSystem(r.Context(), orgID)
	if err != nil {
		setupLog.Error("integrations status tenant probe failed", "error", err)
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": false,
			"error":      "failed to load integrations status",
		})
		return
	}
	tenantExists := org != nil

	// First-run fast path: with no tenant, the github/jira/repo fields are
	// unused (they belong to the post-provision config step) and the gate
	// only needs configured=false. Short-circuit before touching the
	// keychain + repo query — the first-run screen may poll this on a loop.
	if !tenantExists {
		writeJSON(w, http.StatusOK, map[string]any{
			"configured":     false,
			"github":         false,
			"github_ready":   false,
			"jira":           false,
			"github_repos":   0,
			"env_provided":   auth.EnvProvided(),
			"setup_complete": false,
			"setup_step":     "org",
		})
		return
	}

	// SecretStore.Load + repo count both inside the same WithTx so the
	// org_secrets read sees request.jwt.claims and repos_select RLS runs
	// under the user's identity. Local mode collapses to one SQLite
	// tx with the same shape. These feed the config-step fields
	// (github/jira/github_repos), not the first-run gate.
	var (
		creds         auth.Credentials
		credsErr      error
		repoCount     int
		appRegistered bool
	)
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		creds, credsErr = integrations.Load(r.Context(), tx.Secrets, orgID)
		var e error
		repoCount, e = tx.Repos.CountConfigured(r.Context(), orgID)
		if e != nil {
			return e
		}
		// GitHub access can be satisfied by a registered GitHub App (the
		// multi-mode path) rather than a PAT, so the setup-complete gate must
		// count an App as "GitHub configured."
		//
		// Whether there is an App to count is the org's credential class, not
		// the presence of a registration row — a rowless org is a PAT org today
		// and would equally be an org on a deployment-level shared App, which
		// this gate would then report as unconfigured and send back through
		// setup it doesn't need. Best-effort throughout: any read failure, or a
		// class this build doesn't know, leaves appRegistered false and the PAT
		// signal still stands, exactly as a failed App read always has.
		orgSet, se := tx.Orgs.GetSettings(r.Context(), orgID)
		if se != nil {
			setupLog.Warn("read org settings failed; github-configured gate falls back to the pat signal", "org", orgID, "error", se)
			return nil
		}
		switch orgSet.GitHubCredentialClass {
		case domain.GitHubCredentialClassPAT:
			// No App to count; creds.GitHubPAT below is the whole signal.
		case domain.GitHubCredentialClassBYOApp:
			if app, ae := tx.GitHubApps.GetForOrg(r.Context(), orgID); ae == nil && app != nil {
				appRegistered = app.Active && app.ClientID != ""
			}
		default:
			setupLog.Warn("unknown github credential class; github-configured gate falls back to the pat signal",
				"org", orgID, "class", orgSet.GitHubCredentialClass)
		}
		return nil
	}); err != nil {
		// Status endpoint returns 200 with configured=false so the
		// frontend renders a sensible "not connected" UI even when
		// the read failed; log the underlying error server-side
		// instead of leaking it in the response body.
		setupLog.Error("integrations status read failed", "error", err)
		writeJSON(w, http.StatusOK, map[string]any{
			"configured":     false,
			"error":          "failed to load integrations status",
			"setup_complete": false,
			"setup_step":     "org",
		})
		return
	}
	if credsErr != nil {
		setupLog.Error("integrations status creds load failed", "error", credsErr)
		writeJSON(w, http.StatusOK, map[string]any{
			"configured":     false,
			"error":          "failed to load credentials",
			"setup_complete": false,
			"setup_step":     "org",
		})
		return
	}

	// Setup is complete once GitHub access is configured (PAT or registered
	// App; the env overlay folds into creds.GitHubPAT) AND the org tracks at
	// least one repo. ReplaceForTeam writes a repo_profiles skeleton row in
	// the same tx it records the team's tracked repos, so repoCount is a
	// durable signal here — it doesn't lag behind the (async) profiling pass.
	// Jira stays optional. setup_step tells the gate which configure screen
	// an incomplete founder resumes on.
	githubReady := creds.GitHubPAT != "" || appRegistered
	setupComplete := githubReady && repoCount >= 1
	setupStep := "done"
	switch {
	case !githubReady:
		setupStep = "org"
	case repoCount == 0:
		setupStep = "team"
	}

	// Jira is "connected" when a usable service credential exists for the org's
	// stored auth-method marker — Data Center PAT or Cloud email + API token.
	// JiraSystemConfig reads the marker (not key presence), so this matches the
	// client the resolver would build and a Cloud org (which has no PAT) reports
	// connected. cfg.Deployment is the authoritative Cloud-vs-DC answer (from the
	// marker), surfaced below so the frontend seeds its deployment picker without
	// re-guessing from the host shape (which is wrong for Cloud custom domains).
	jiraCfg, jiraConnected := integrations.JiraSystemConfig(creds)

	result := map[string]any{
		"configured":     tenantExists,
		"github":         creds.GitHubPAT != "",
		"github_ready":   githubReady,
		"jira":           jiraConnected,
		"github_repos":   repoCount,
		"env_provided":   auth.EnvProvided(),
		"setup_complete": setupComplete,
		"setup_step":     setupStep,
	}

	if creds.GitHubURL != "" {
		result["github_url"] = creds.GitHubURL
	}
	if creds.JiraURL != "" {
		result["jira_url"] = creds.JiraURL
	}
	if jiraConnected {
		result["jira_deployment"] = string(jiraCfg.Deployment)
	}

	writeJSON(w, http.StatusOK, result)
}

// handleSetupStart is the local-mode "Start your factory"
// provision action (POST /api/setup/start). On a tenant-less install it
// runs the shared BootstrapLocalOrg chain: create the synthetic tenant
// rows, then seed the org template + materialize the founder team's
// agent + prompts + blueprints + handlers — the same create→bootstrap
// path multi-mode fires on org-create, with auth skipped and identity
// auto-filled to the runmode.LocalDefault* sentinels.
//
// Non-resurrection guarantee: no-ops once the tenant is *fully*
// provisioned. "Fully provisioned" means the org row exists AND the
// agents row exists (the first step of BootstrapNewOrg). If the org
// row exists but the agents row doesn't, the binary crashed mid-provision
// after CreateLocalTenant committed but before BootstrapNewOrg ran.
// In that partial state the user hasn't performed any actions yet (the
// onboarding screen never completed), so re-running BootstrapLocalOrg is
// safe — there are no user-deleted shipped defaults to resurrect. Once
// the agent row exists, it signals "the user may have made changes" and
// the endpoint stops re-seeding to preserve those changes.
//
// Local-mode only — multi-mode provisions per signup through
// auth_provision.go and has no synthetic tenant to create.
func (s *Server) handleSetupStart(w http.ResponseWriter, r *http.Request) {
	if runmode.Current() != runmode.ModeLocal {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "setup/start is local-mode only; multi-mode provisions tenants on org creation",
		})
		return
	}

	alreadyProvisioned, err := s.ensureLocalOrgProvisioned(r.Context())
	if err != nil {
		setupLog.Error("setup/start provision failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "failed to provision local workspace",
		})
		return
	}

	// Echo the (stable sentinel) IDs to keep the response shape parallel to
	// multi-mode's org-create response and give the onboarding UI something to
	// confirm against. already_provisioned=false signals "we just provisioned
	// it (fresh install or partial-provision recovery)".
	writeJSON(w, http.StatusOK, map[string]any{
		"provisioned":         true,
		"already_provisioned": alreadyProvisioned,
		"org_id":              runmode.LocalDefaultOrgID,
		"team_id":             runmode.LocalDefaultTeamID,
	})
}

// ensureLocalOrgProvisioned idempotently provisions the local-mode tenant —
// the runmode.LocalDefault* sentinel rows plus the shipped
// agent/prompts/blueprints/handlers — by running the shared
// db.BootstrapLocalOrg chain when needed. It is the common core of the
// "Start your factory" action (handleSetupStart) and the headless bootstrap
// (RunHeadlessBootstrap), so the two share one provisioning path.
//
// Returns alreadyProvisioned=true when the org AND its agents row already
// exist: BootstrapNewOrg got at least through its first step, so the user may
// have edited shipped defaults — don't re-seed (the non-resurrection guard).
// When the org row exists but the agents row doesn't, that's a
// crash-mid-provision state with no user actions yet, so re-running
// BootstrapLocalOrg is safe and reaches the same end state.
//
// Probes via the System (admin-pool) variants to match the rest of the
// bootstrap chain: provisioning-state reads are claims-free system reads.
// SQLite (the only backend this local-only path runs against) collapses both
// pools to one connection. BootstrapLocalOrg creates the synthetic sentinel
// rows and must run OUTSIDE any WithTx (admin-pool seeders).
func (s *Server) ensureLocalOrgProvisioned(ctx context.Context) (alreadyProvisioned bool, err error) {
	provisioned, err := s.localOrgProvisioned(ctx)
	if err != nil {
		return false, err
	}
	if provisioned {
		return true, nil
	}
	// Not (fully) provisioned — fresh install, or org row exists without an
	// agents row (crash-mid-provision). Re-running BootstrapLocalOrg reaches the
	// same end state either way.
	if err := db.BootstrapLocalOrg(ctx, s.allStores, promptseed.Prompts(), promptseed.Blueprints()); err != nil {
		return false, fmt.Errorf("bootstrap local org: %w", err)
	}
	return false, nil
}

// localOrgProvisioned reports whether the local tenant is *fully* provisioned —
// the org row AND its agents row both exist (the same condition
// ensureLocalOrgProvisioned treats as "already provisioned"). A read-only probe
// with no provisioning side effect, so a caller can branch on it before
// deciding whether to do work that should only happen on a real provision
// (e.g. the headless bootstrap skips its bot-credential network call and its
// seed when this is already true). Org row but no agents row reads as
// not-provisioned: that's a crash-mid-provision state the caller should
// complete. System (admin-pool) reads; SQLite collapses the pool split.
func (s *Server) localOrgProvisioned(ctx context.Context) (bool, error) {
	org, err := s.orgs.GetOrgSystem(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		return false, fmt.Errorf("tenant probe: %w", err)
	}
	if org == nil {
		return false, nil
	}
	agent, err := s.allStores.Agents.GetForOrgSystem(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		return false, fmt.Errorf("agent probe: %w", err)
	}
	return agent != nil, nil
}

// DELETE /api/integrations — clears all integration credentials (GitHub
// + Jira) via SecretStore. Used by the Settings "Clear All Tokens"
// flow when the user wants a fresh slate. Unbinding ONE credential goes
// through that credential's own resource (DELETE
// /api/orgs/{org_id}/github/access/pat, .../jira/access/credential).
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
		// Read before clearing so the audit rows name only what was really
		// bound — the vault deletes are idempotent and can't report it, and a
		// revocation the log invents is worse than one it omits.
		creds, _ := integrations.Load(r.Context(), tx.Secrets, orgID)
		if err := integrations.Clear(r.Context(), tx.Secrets, orgID); err != nil {
			return err
		}
		return recordOrgCredentialClear(r.Context(), tx, orgID, userID, creds)
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
