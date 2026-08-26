package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/promptseed"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
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

// setupModelPicks answers whether this org has made the two model choices setup
// requires of it: the background-jobs model, and the default model of the team
// setup configures. They are reported separately because they are two different
// screens — one org-scoped, one team-scoped — and the resume point has to name
// the one that is actually missing.
//
// MULTI ONLY, and this is the one recorded mode asymmetry of the model surface
// rather than drift. TF ships no fallback model, so a multi org that skipped
// either pick has background jobs that silently never run and a team whose every
// unpinned step refuses at dispatch — neither of which says anything at the
// moment it is decided. Local pre-fills both from its dialect's column defaults
// and never blocks on them, so asking there would gate a first run on a question
// nobody was asked.
//
// The team is the org's default (oldest) team — the same one the wizard's team
// section resolves and configures, so the answer is about the row the founder
// was actually shown. Read under the caller's claims like everything else on
// this route; a caller outside that team reads the schema defaults, which are
// populated, so the gate errs toward letting them in rather than bouncing
// somebody through a wizard step they cannot even see.
func setupModelPicks(ctx context.Context, tx db.TxStores, orgID string) (orgPick, teamPick bool, err error) {
	if runmode.Current() != runmode.ModeMulti {
		return true, true, nil
	}
	orgSet, err := tx.Orgs.GetSettings(ctx, orgID)
	if err != nil {
		return false, false, fmt.Errorf("load org settings for the setup gate: %w", err)
	}
	orgPick = strings.TrimSpace(orgSet.BackgroundJobsModel) != ""

	teamID, err := tx.Teams.GetDefaultForOrg(ctx, orgID)
	if err != nil {
		return false, false, fmt.Errorf("resolve the default team for the setup gate: %w", err)
	}
	if teamID == "" {
		// A teamless org is a bootstrap bug, not an unfinished pick. Nothing
		// here can be chosen yet, and reporting the org incomplete would send
		// the founder to a wizard whose team section has nothing to address.
		return orgPick, true, nil
	}
	teamSet, err := tx.Teams.GetSettings(ctx, teamID)
	if err != nil {
		return false, false, fmt.Errorf("load team settings for the setup gate: %w", err)
	}
	return orgPick, strings.TrimSpace(teamSet.DefaultModel) != "", nil
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
		creds       auth.Credentials
		credsErr    error
		repoCount   int
		githubReady bool
		orgModel    bool
		teamModel   bool
	)
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		creds, credsErr = integrations.Load(r.Context(), tx.Secrets, orgID)
		var e error
		repoCount, e = tx.Repos.CountConfigured(r.Context(), orgID)
		if e != nil {
			return e
		}
		if orgModel, teamModel, e = setupModelPicks(r.Context(), tx, orgID); e != nil {
			return e
		}
		// GitHub access can be satisfied by a registered GitHub App (the
		// multi-mode path) rather than a PAT, so the setup-complete gate must
		// count an App as "GitHub configured." Best-effort here, unlike the
		// availability read that shares the derivation: this route answers 200
		// with configured=false on its own faults, so a failed probe leaves the
		// PAT signal standing rather than turning a hiccup into a founder sent
		// back through setup.
		ready, ge := integrations.GitHubReady(r.Context(), tx.Orgs, tx.GitHubApps, orgID, creds)
		if ge != nil {
			setupLog.Warn("github access probe failed; github-configured gate falls back to the pat signal", "org", orgID, "error", ge)
			githubReady = creds.GitHubPAT != ""
			return nil
		}
		githubReady = ready
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
	// App; the env overlay folds into creds.GitHubPAT), the org has brought at
	// least one repo into the registry, and — in multi mode, where nothing is
	// pre-filled — both model picks are made. ReplaceForTeam writes the registry
	// row in the same tx it records the team's tracked repos, so repoCount is a
	// durable signal here — it doesn't lag behind the (async) profiling pass.
	// It counts the registry rather than the tracked set on purpose: a founder
	// who has finished setup and later untracks everything has still finished
	// setup, and bouncing them back through it would be a regression, not a
	// reminder. Jira stays optional. setup_step tells the gate which configure
	// screen an incomplete founder resumes on.
	setupComplete := githubReady && orgModel && teamModel && repoCount >= 1
	// The order is the wizard's own: the org's credential and jobs model, then
	// the team's repos, then the team's default model. Each arm names the screen
	// its missing input lives on, so a founder resumes where the work is.
	setupStep := "done"
	switch {
	case !githubReady, !orgModel:
		setupStep = "org"
	case repoCount == 0, !teamModel:
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

// ensureLocalOrgProvisioned idempotently provisions the local-mode tenant —
// the runmode.LocalDefault* sentinel rows plus the shipped
// agent/prompts/blueprints/handlers — by running the shared
// db.BootstrapLocalOrg chain when needed. It is the common core of the
// "Start your factory" action (createLocalOrg) and the headless bootstrap
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
