package server

import (
	"context"
	"os"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// Headless bootstrap (TFAC-411). When TF_HEADLESS is set in local mode, the
// server provisions the local tenant and seeds tracked repos + Jira config +
// the operator's identity entirely from environment variables, so a
// keychain-less / browser-less install reaches setup_complete with no manual
// setup. This is the single-user local-mode experience — NOT the multi-tenant
// deployment.
//
// Invariants:
//   - Local mode only. The app gates the call on a.local() && HeadlessEnabled();
//     none of these env vars can reach a multi-mode org.
//   - One-time seed. Everything below is applied only on the boot that
//     provisions the tenant, and each item additionally
//     guards on its target being empty, so a restart never overwrites config
//     the operator later edited in the UI. The DB is authoritative once
//     populated; the env is the initial seed, not a competing store.
//   - Warn, don't crash. A missing/invalid credential or incomplete Jira config
//     logs a WARN and skips the affected piece; it never aborts boot. (The only
//     fatal at boot is the separate TF_SECRET_ENCRYPTION_KEY check.)

// Headless env var names. The bot/access credentials come through the existing
// internal/auth overlay (TRIAGE_FACTORY_GITHUB_URL / _GITHUB_BOT_PAT / _JIRA_URL
// and either _JIRA_BOT_PAT or _JIRA_EMAIL + _JIRA_API_TOKEN); the vars below are
// the headless-only seed + identity inputs.
const (
	envHeadless      = "TF_HEADLESS"
	envRepos         = "TRIAGE_FACTORY_REPOS"
	envGitHubUserPAT = "TRIAGE_FACTORY_GITHUB_USER_PAT"
	// The operator's Jira identity token: a PAT on Data Center, an API token on
	// Cloud. Cloud authenticates the token together with the account email,
	// which is the second var.
	envJiraUserPAT          = "TRIAGE_FACTORY_JIRA_USER_PAT"
	envJiraUserEmail        = "TRIAGE_FACTORY_JIRA_USER_EMAIL"
	envJiraProjects         = "TRIAGE_FACTORY_JIRA_PROJECTS"
	envJiraPickupStatuses   = "TRIAGE_FACTORY_JIRA_PICKUP_STATUSES"
	envJiraInProgressStatus = "TRIAGE_FACTORY_JIRA_INPROGRESS_STATUS"
	// Optional: the status that names work awaiting human review. A deployment
	// that leaves it unset is completely configured — the rule arms nothing —
	// so this is deliberately not part of jiraComplete.
	envJiraInReviewStatus = "TRIAGE_FACTORY_JIRA_INREVIEW_STATUS"
	envJiraDoneStatus     = "TRIAGE_FACTORY_JIRA_DONE_STATUS"
	envCloneProtocol      = "TRIAGE_FACTORY_CLONE_PROTOCOL"
)

// headlessSeedVars are the bootstrap-only env vars — read solely by the
// headless bootstrap. (The credential overlay vars are NOT here: they apply on
// every secret read regardless of TF_HEADLESS, so they're never "orphaned".)
var headlessSeedVars = []string{
	envRepos, envGitHubUserPAT, envJiraUserPAT, envJiraUserEmail, envJiraProjects,
	envJiraPickupStatuses, envJiraInProgressStatus, envJiraInReviewStatus,
	envJiraDoneStatus, envCloneProtocol,
}

// HeadlessEnabled reports whether TF_HEADLESS requests the env-driven bootstrap.
func HeadlessEnabled() bool { return strings.TrimSpace(os.Getenv(envHeadless)) != "" }

// WarnIfHeadlessSeedVarsOrphaned logs one WARN when bootstrap-only seed vars are
// set but TF_HEADLESS is unset — so an operator who forgot the trigger isn't
// left wondering why nothing was provisioned. No-op when TF_HEADLESS is set.
func WarnIfHeadlessSeedVarsOrphaned() {
	if HeadlessEnabled() {
		return
	}
	var present []string
	for _, k := range headlessSeedVars {
		if strings.TrimSpace(os.Getenv(k)) != "" {
			present = append(present, k)
		}
	}
	if len(present) > 0 {
		headlessLog.Warn("headless seed env vars are set but TF_HEADLESS is unset; ignoring them — set TF_HEADLESS=1 to enable headless provisioning", "vars", present)
	}
}

// headlessConfig is the parsed headless input apart from the org credentials,
// which come through the overlay-backed secret store.
type headlessConfig struct {
	repos                []domain.TeamGitHubRepo
	githubUserPAT        string
	jiraUserPAT          string
	jiraUserEmail        string
	jiraProjects         []string
	jiraPickupStatuses   []string
	jiraInProgressStatus string
	jiraInReviewStatus   string
	jiraDoneStatus       string
	cloneProtocol        string // "https" (default) or "ssh"
}

// jiraIntent reports whether the operator expressed any intent to configure
// Jira (so an incomplete config is worth a WARN rather than a silent skip).
func (c headlessConfig) jiraIntent() bool {
	return len(c.jiraProjects) > 0 || len(c.jiraPickupStatuses) > 0 ||
		c.jiraInProgressStatus != "" || c.jiraInReviewStatus != "" ||
		c.jiraDoneStatus != "" || c.jiraUserPAT != "" || c.jiraUserEmail != ""
}

// jiraComplete reports whether every field the Jira status model requires is
// present (projects + a non-empty pickup set + in-progress + done). The org
// credential is checked separately by the caller. The in-review status is
// optional and absent here on purpose: it arms nothing, so a deployment that
// never sets it is fully configured.
func (c headlessConfig) jiraComplete() bool {
	return len(c.jiraProjects) > 0 && len(c.jiraPickupStatuses) > 0 &&
		c.jiraInProgressStatus != "" && c.jiraDoneStatus != ""
}

func loadHeadlessConfig() headlessConfig {
	return headlessConfig{
		repos:                parseRepos(os.Getenv(envRepos)),
		githubUserPAT:        strings.TrimSpace(os.Getenv(envGitHubUserPAT)),
		jiraUserPAT:          strings.TrimSpace(os.Getenv(envJiraUserPAT)),
		jiraUserEmail:        strings.TrimSpace(os.Getenv(envJiraUserEmail)),
		jiraProjects:         parseCSV(os.Getenv(envJiraProjects)),
		jiraPickupStatuses:   parseCSV(os.Getenv(envJiraPickupStatuses)),
		jiraInProgressStatus: strings.TrimSpace(os.Getenv(envJiraInProgressStatus)),
		jiraInReviewStatus:   strings.TrimSpace(os.Getenv(envJiraInReviewStatus)),
		jiraDoneStatus:       strings.TrimSpace(os.Getenv(envJiraDoneStatus)),
		cloneProtocol:        parseCloneProtocol(os.Getenv(envCloneProtocol)),
	}
}

// parseCloneProtocol normalizes the clone-protocol override. Headless pins
// https by default because a headless box (Docker, CI, bare server) usually has
// no SSH agent or keys loaded, and the bot PAT — used over https — is the
// credential it's guaranteed to have. An operator who HAS an SSH agent on the
// box can set TRIAGE_FACTORY_CLONE_PROTOCOL=ssh to clone over SSH instead. An
// unrecognized value warns and falls back to https rather than failing boot.
func parseCloneProtocol(raw string) string {
	switch v := strings.ToLower(strings.TrimSpace(raw)); v {
	case "", "https":
		return "https"
	case "ssh":
		return "ssh"
	default:
		headlessLog.Warn("ignoring invalid TRIAGE_FACTORY_CLONE_PROTOCOL (want ssh|https); using https", "value", raw)
		return "https"
	}
}

// parseCSV splits a comma-separated value, trimming whitespace and dropping
// empties. Returns nil for an empty/blank input.
func parseCSV(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// parseRepos parses a CSV of "owner/repo" entries into TeamGitHubRepo rows.
// Malformed entries (missing owner or repo) are dropped with a WARN rather than
// failing the whole bootstrap.
func parseRepos(raw string) []domain.TeamGitHubRepo {
	var out []domain.TeamGitHubRepo
	for _, entry := range parseCSV(raw) {
		owner, repo, ok := strings.Cut(entry, "/")
		owner, repo = strings.TrimSpace(owner), strings.TrimSpace(repo)
		if !ok || owner == "" || repo == "" {
			headlessLog.Warn("ignoring malformed TRIAGE_FACTORY_REPOS entry (want owner/repo)", "entry", entry)
			continue
		}
		out = append(out, domain.TeamGitHubRepo{Owner: owner, Repo: repo})
	}
	return out
}

// jiraIdentitySeed is the validated per-user Jira credential the bootstrap will
// persist — computed (with its network validation) before the write tx.
type jiraIdentitySeed struct {
	host        string
	envelope    string
	accountID   string
	displayName string
	// source is the identity row's source marker, the value the HTTP bind
	// writes for the same scheme.
	source string
}

// RunHeadlessBootstrap provisions the local tenant and seeds repos + Jira +
// identity from environment variables. Local-mode only; the caller gates on
// HeadlessEnabled(). Idempotent and never-overwriting (see the package doc).
// Returns an error only for an unexpected failure the caller should log; every
// "couldn't fully configure" case is handled in-band with a WARN.
func (s *Server) RunHeadlessBootstrap(ctx context.Context) error {
	// Defense in depth: the headless seed only ever touches the LocalDefault*
	// sentinels, and the caller already gates on local mode — but a hard guard
	// here makes it impossible for this to mutate a multi-mode org no matter who
	// calls it.
	if runmode.Current() != runmode.ModeLocal {
		return nil
	}

	cfg := loadHeadlessConfig()

	// 1. Bot/access GitHub credential — read via the overlay-aware secret store
	//    (TRIAGE_FACTORY_GITHUB_URL + _GITHUB_BOT_PAT supply it in headless).
	creds, err := integrations.LoadSystem(ctx, s.secrets, runmode.LocalDefaultOrgID)
	if err != nil {
		return err
	}
	if creds.GitHubURL == "" || creds.GitHubPAT == "" {
		headlessLog.Warn("TF_HEADLESS set but no GitHub bot credentials in env; skipping bootstrap (set TRIAGE_FACTORY_GITHUB_URL and TRIAGE_FACTORY_GITHUB_BOT_PAT)")
		return nil
	}
	ghWeb, okHost := resolveGitHubHost(creds.GitHubURL)
	if !okHost {
		headlessLog.Warn("GitHub URL is misconfigured; skipping bootstrap", "url", creds.GitHubURL)
		return nil
	}
	// 2. If the tenant is already fully provisioned there's nothing to seed —
	//    return BEFORE the bot-credential network call. Otherwise a steady-state
	//    restart (TF_HEADLESS still set) would make an outbound GitHub request on
	//    every boot, and an expired bot token would log an alarming "skipping
	//    bootstrap" even though the bootstrap had nothing to do. Read-only probe,
	//    no provisioning side effect.
	if provisioned, perr := s.localOrgProvisioned(ctx); perr != nil {
		return perr
	} else if provisioned {
		headlessLog.Info("tenant already provisioned; headless bootstrap left existing config untouched")
		return nil
	}

	// 3. Validate the bot credential BEFORE provisioning — don't stand up an org
	//    around a token the host rejects. Validating first means fixing the token
	//    and restarting recovers cleanly; provisioning an empty tenant first would
	//    trip the never-overwrite gate and the seed would never run.
	botIdentity, err := auth.CaptureGitHubIdentity(ctx, creds.GitHubURL, creds.GitHubPAT)
	if err != nil {
		headlessLog.Warn("GitHub bot credential failed validation; skipping bootstrap", "host", ghWeb, "error", err)
		return nil
	}

	// 4. Pre-tx network validations for the per-user identities, so the write tx
	//    holds only DB writes (mirrors the HTTP handlers' validate-then-write).
	//    We're here only when not yet provisioned (fresh or crash-recovery), so
	//    these always run.
	var githubIdentity auth.GitHubUser
	switch {
	case cfg.githubUserPAT != "":
		ghUser, verr := validateGitHubIdentityPAT(ctx, ghWeb, cfg.githubUserPAT)
		if verr != nil {
			headlessLog.Warn("TRIAGE_FACTORY_GITHUB_USER_PAT failed validation; GitHub identity not bound (you'll be asked to Connect)", "host", ghWeb, "error", verr)
		} else {
			githubIdentity = ghUser
		}
	default:
		// The GitHub identity gate is a hard redirect in local mode, so a
		// provision without this token lands the operator on the Connect page
		// rather than the app. Warn so a half-configured headless deploy is
		// diagnosable instead of mysteriously gated.
		headlessLog.Warn("TRIAGE_FACTORY_GITHUB_USER_PAT is unset; GitHub identity won't be bound and you'll be prompted to Connect in the UI. Set it (often the same value as the bot PAT) for a no-browser boot.")
	}

	// Jira readiness: an org credential usable for the host's deployment plus a
	// complete status config. The deployment is the resolver's own call
	// (DeploymentForMarker): an env-only setup stores no marker, so the host
	// shape decides — a *.atlassian.net host is Cloud and reads the email + API
	// token, any other host is Data Center and reads the PAT.
	jiraHost, jiraHostOK := resolveJiraHost(creds.JiraURL)
	jiraDeployment := jira.DeploymentForMarker(jira.AuthMethod(creds.JiraAuthMethod), jiraHost)
	_, jiraCredOK := integrations.JiraSystemConfig(creds)
	jiraReady := jiraCredOK && cfg.jiraComplete()
	// "Intent to use Jira" includes the org credential, not just the headless
	// seed vars — so an operator who set the Jira credentials but forgot the
	// project/status config still gets told why Jira wasn't configured, rather
	// than silently discovering an empty Jira page.
	jiraIntended := creds.JiraURL != "" || creds.JiraPAT != "" || creds.JiraEmail != "" ||
		creds.JiraAPIToken != "" || cfg.jiraIntent()
	if jiraIntended && !jiraReady {
		if creds.JiraURL != "" && !jiraHostOK {
			headlessLog.Warn("Jira URL is misconfigured; skipping Jira setup", "url", creds.JiraURL)
		} else {
			// The deployment is named when the host resolved, so an operator who
			// set a Cloud host with the Data Center PAT (or the reverse) can see
			// which credential the host asked for.
			var args []any
			if jiraHostOK {
				args = append(args, "host", jiraHost, "deployment", jiraDeployment)
			}
			headlessLog.Warn("Jira config is incomplete; skipping Jira setup (need TRIAGE_FACTORY_JIRA_URL, the service credential for its deployment — _JIRA_BOT_PAT for Data Center, _JIRA_EMAIL + _JIRA_API_TOKEN for Cloud — and _JIRA_PROJECTS + _JIRA_PICKUP_STATUSES + _JIRA_INPROGRESS_STATUS + _JIRA_DONE_STATUS)", args...)
		}
	}

	var jiraIdentity *jiraIdentitySeed
	if jiraReady {
		if cfg.jiraUserPAT != "" {
			jiraIdentity = s.validateJiraIdentity(ctx, jiraHost, jiraDeployment, cfg.jiraUserEmail, cfg.jiraUserPAT)
		} else {
			// Same gate problem as GitHub: configuring Jira projects without a
			// per-user token leaves the operator stuck at the Jira Connect page.
			headlessLog.Warn("Jira is configured but TRIAGE_FACTORY_JIRA_USER_PAT is unset; Jira identity won't be bound and you'll be prompted to connect Jira in the UI. Set it for a no-browser boot.")
		}
	}

	// 5. Provision the tenant (idempotent), then seed in one tx. Reached only when
	//    not already provisioned, so the seed always runs; the per-item "only if
	//    empty" guards below are belt-and-suspenders against a partial prior provision.
	// The alreadyProvisioned bool is intentionally discarded: the read-only probe
	// at step 2 already returned early if it were true, so here it's always false.
	if _, err := s.ensureLocalOrgProvisioned(ctx); err != nil {
		return err
	}

	var (
		seededRepos        int
		seededJiraProjects int
		boundGitHubLogin   string
		boundJiraName      string
	)
	if err := s.tx.WithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(tx db.TxStores) error {
		// org-settings host URLs + clone protocol. The freshly-provisioned row
		// carries empty base URLs; the clone protocol is pinned outright rather
		// than left to the row's default, because a typical headless box has no
		// SSH agent. TRIAGE_FACTORY_CLONE_PROTOCOL overrides it for a box that
		// does have SSH set up.
		orgSet, gerr := tx.Orgs.GetSettings(ctx, runmode.LocalDefaultOrgID)
		if gerr != nil {
			return gerr
		}
		if orgSet.GitHubBaseURL == "" {
			orgSet.GitHubBaseURL = creds.GitHubURL
		}
		if jiraReady && orgSet.JiraBaseURL == "" {
			orgSet.JiraBaseURL = creds.JiraURL
		}
		orgSet.GitHubCloneProtocol = cfg.cloneProtocol
		if _, uerr := tx.Orgs.UpdateSettings(ctx, runmode.LocalDefaultOrgID, orgSet); uerr != nil {
			return uerr
		}
		if uerr := persistOrgGitHubIdentity(ctx, tx, runmode.LocalDefaultOrgID, botIdentity.Login, botIdentity.PrimaryEmail); uerr != nil {
			return uerr
		}

		// Tracked repos (only if the team tracks none yet).
		if len(cfg.repos) > 0 {
			existing, lerr := tx.TeamGitHubRepos.ListForTeam(ctx, runmode.LocalDefaultTeamID)
			if lerr != nil {
				return lerr
			}
			if len(existing) == 0 {
				if rerr := tx.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, cfg.repos); rerr != nil {
					return rerr
				}
				seededRepos = len(cfg.repos)
			}
		}

		// Jira projects + global status rules (only if the team has no rules yet).
		if jiraReady {
			existing, lerr := tx.JiraStatusRules.ListForTeam(ctx, runmode.LocalDefaultTeamID)
			if lerr != nil {
				return lerr
			}
			if len(existing) == 0 {
				teamSet, terr := tx.Teams.GetSettings(ctx, runmode.LocalDefaultTeamID)
				if terr != nil {
					return terr
				}
				teamSet.JiraProjects = cfg.jiraProjects
				if _, uerr := tx.Teams.UpdateSettings(ctx, runmode.LocalDefaultTeamID, teamSet); uerr != nil {
					return uerr
				}
				if rerr := tx.JiraStatusRules.ReplaceForTeam(ctx, runmode.LocalDefaultTeamID, headlessJiraRules(cfg)); rerr != nil {
					return rerr
				}
				seededJiraProjects = len(cfg.jiraProjects)
			}
		}

		// GitHub identity (only if none bound for this user+host yet).
		if githubIdentity.Login != "" {
			cur, gierr := tx.Users.GetGitHubLogin(ctx, runmode.LocalDefaultUserID, ghWeb)
			if gierr != nil {
				return gierr
			}
			if cur == "" {
				if uerr := tx.Users.UpsertGitHubIdentity(ctx, runmode.LocalDefaultUserID, ghWeb, githubIdentity.Login, githubIdentity.UserID(), githubIdentity.PrimaryEmail, "pat"); uerr != nil {
					return uerr
				}
				boundGitHubLogin = githubIdentity.Login
			}
		}

		// Jira identity (only if none bound for this user+host yet).
		if jiraIdentity != nil {
			acct, _, jierr := tx.Users.GetJiraIdentity(ctx, runmode.LocalDefaultUserID, jiraIdentity.host)
			if jierr != nil {
				return jierr
			}
			if acct == "" {
				if perr := tx.Secrets.PutUser(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, jiraTokenKey(jiraIdentity.host), jiraIdentity.envelope, "Jira user access token"); perr != nil {
					return perr
				}
				if uerr := tx.Users.UpsertJiraIdentity(ctx, runmode.LocalDefaultUserID, jiraIdentity.host, jiraIdentity.accountID, jiraIdentity.displayName, jiraIdentity.source); uerr != nil {
					return uerr
				}
				boundJiraName = jiraIdentity.displayName
			}
		}
		return nil
	}); err != nil {
		return err
	}

	// Deterministic field order (slog reads the slice positionally).
	args := []any{"github_host", ghWeb}
	if seededRepos > 0 {
		args = append(args, "repos", seededRepos)
	}
	if seededJiraProjects > 0 {
		args = append(args, "jira_projects", seededJiraProjects)
	}
	if boundGitHubLogin != "" {
		args = append(args, "github_identity", boundGitHubLogin)
	}
	if boundJiraName != "" {
		args = append(args, "jira_identity", boundJiraName)
	}
	headlessLog.Info("headless bootstrap complete", args...)
	return nil
}

// validateJiraIdentity validates the operator's per-user Jira token against the
// org's Jira host and returns the credential envelope + identity to persist.
// The scheme follows the org's deployment, not the variables set: ForUser
// refuses a credential whose scheme doesn't match the deployment, so binding
// one would leave an identity row whose token never resolves. Cloud is Basic
// auth over the account email + API token, Data Center a PAT on its own.
// Returns nil (with a WARN) on any failure — Jira identity is best-effort.
func (s *Server) validateJiraIdentity(ctx context.Context, host string, deployment jira.Deployment, email, token string) *jiraIdentitySeed {
	var (
		cfg    jira.Config
		cred   jira.UserCredential
		source string
	)
	if deployment == jira.DeploymentCloud {
		if email == "" {
			headlessLog.Warn("Jira is Cloud but TRIAGE_FACTORY_JIRA_USER_EMAIL is unset; a Cloud API token authenticates with its account email, so Jira identity not bound", "host", host)
			return nil
		}
		cfg = jira.CloudAPIToken(host, email, token)
		cred = jira.UserCredential{Method: jira.AuthMethodCloudAPIToken, Email: email, Token: token}
		source = string(jira.AuthMethodCloudAPIToken)
	} else {
		if email != "" {
			headlessLog.Warn("ignoring TRIAGE_FACTORY_JIRA_USER_EMAIL: the Jira host is Data Center, where the identity token is a PAT used on its own", "host", host)
		}
		cfg = jira.DataCenterPAT(host, token)
		cred = jira.UserCredential{Method: jira.AuthMethodDCPAT, Token: token}
		source = "pat"
	}
	jiraUser, err := auth.ValidateJira(ctx, cfg)
	if err != nil {
		headlessLog.Warn("TRIAGE_FACTORY_JIRA_USER_PAT failed validation; Jira identity not bound", "host", host, "deployment", deployment, "error", err)
		return nil
	}
	if jiraUser.StableID() == "" {
		headlessLog.Warn("Jira returned no account for TRIAGE_FACTORY_JIRA_USER_PAT; Jira identity not bound", "host", host)
		return nil
	}
	envelope, err := jira.MarshalUserCredential(cred)
	if err != nil {
		headlessLog.Warn("could not encode Jira user credential; Jira identity not bound", "error", err)
		return nil
	}
	return &jiraIdentitySeed{
		host:        host,
		envelope:    envelope,
		accountID:   jiraUser.StableID(),
		displayName: jiraUser.DisplayName,
		source:      source,
	}
}

// headlessJiraRules expands the single global status mapping into one
// JiraProjectStatusRules row per tracked project (the accepted v1
// simplification vs. per-project parity). The table's CHECK constraints want
// members and canonical together on each write target — single values satisfy
// them, and an unset in-review status leaves both of its columns empty, which
// satisfies the constraint the other way.
//
// The statuses come from the operator's environment as NAMES, so the rows land
// name-only, with no status ids. That is a supported stored shape: the poller
// matches on the name and the ids fill if the team ever saves the rules through
// the API. Resolving ids here would mean a Jira round trip inside boot, for a
// path whose whole point is provisioning without one.
func headlessJiraRules(cfg headlessConfig) []domain.JiraProjectStatusRules {
	named := func(names ...string) []domain.JiraStatusRef {
		refs := make([]domain.JiraStatusRef, 0, len(names))
		for _, n := range names {
			refs = append(refs, domain.JiraStatusRef{Name: n})
		}
		return refs
	}
	var inReviewMembers []domain.JiraStatusRef
	var inReviewCanonical domain.JiraStatusRef
	if cfg.jiraInReviewStatus != "" {
		inReviewMembers = named(cfg.jiraInReviewStatus)
		inReviewCanonical = domain.JiraStatusRef{Name: cfg.jiraInReviewStatus}
	}
	rules := make([]domain.JiraProjectStatusRules, 0, len(cfg.jiraProjects))
	for _, key := range cfg.jiraProjects {
		rules = append(rules, domain.JiraProjectStatusRules{
			ProjectKey:          key,
			PickupMembers:       named(cfg.jiraPickupStatuses...),
			InProgressMembers:   named(cfg.jiraInProgressStatus),
			InProgressCanonical: domain.JiraStatusRef{Name: cfg.jiraInProgressStatus},
			InReviewMembers:     inReviewMembers,
			InReviewCanonical:   inReviewCanonical,
			DoneMembers:         named(cfg.jiraDoneStatus),
			DoneCanonical:       domain.JiraStatusRef{Name: cfg.jiraDoneStatus},
		})
	}
	return rules
}
