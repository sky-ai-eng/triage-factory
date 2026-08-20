package server

import (
	"context"
	"fmt"
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/promptseed"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// persistOrgGitHubIdentity records the org credential's OWN GitHub login and
// verified primary email on the agents row so the credential resolver's
// OrgIdentityFor PAT tier can stamp delegated-agent commits. Called
// inside the same WithTx that saves the org PAT, by every org-PAT writer that
// already validated the login (the PAT bind, the App→PAT switch). This is org
// ACCESS metadata — it deliberately does NOT touch user_github_identities (the
// per-user PAT_2 identity surface).
//
// An empty login is a no-op (the caller had no GitHub PAT in this write). A
// missing agents row (not yet bootstrapped) is skipped rather than erroring —
// the login self-heals on the next PAT re-save. A real write error propagates so
// it rolls back with the rest of the caller's tx. The App path never calls this:
// an App org's bot login (<slug>[bot]) resolves live from the registration.
func persistOrgGitHubIdentity(ctx context.Context, tx db.TxStores, orgID, login, email string) error {
	if login == "" || email == "" {
		return nil
	}
	agent, err := tx.Agents.GetForOrg(ctx, orgID)
	if err != nil {
		return fmt.Errorf("load agent for org github login: %w", err)
	}
	if agent == nil {
		return nil // not bootstrapped yet; nothing to stamp
	}
	_, e := tx.Agents.SetGitHubOrgIdentity(ctx, orgID, agent.ID, login, email)
	return e
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
	// App; the env overlay folds into creds.GitHubPAT) AND the org has brought
	// at least one repo into the registry. ReplaceForTeam writes the registry
	// row in the same tx it records the team's tracked repos, so repoCount is a
	// durable signal here — it doesn't lag behind the (async) profiling pass.
	// It counts the registry rather than the tracked set on purpose: a founder
	// who has finished setup and later untracks everything has still finished
	// setup, and bouncing them back through it would be a regression, not a
	// reminder. Jira stays optional. setup_step tells the gate which configure
	// screen an incomplete founder resumes on.
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
		httpx.WriteErrors(w, http.StatusNotFound, httpx.ErrorItem{Reason: httpx.ReasonNotFound, Message: "setup/start is local-mode only; multi-mode provisions tenants on org creation"})
		return
	}

	alreadyProvisioned, err := s.ensureLocalOrgProvisioned(r.Context())
	if err != nil {
		internalError(w, "setup", fmt.Errorf("provision local workspace: %w", err))
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
