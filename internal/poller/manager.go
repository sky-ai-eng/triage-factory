package poller

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/tracker"
)

// Manager manages the lifecycle of polling loops, allowing them to be
// stopped and restarted when credentials or config change.
type Manager struct {
	database *sql.DB
	bus      *eventbus.Bus
	// tracker dependencies are held instead of a single Tracker instance
	// because each poll cycle constructs one Tracker per active org —
	// orgID is a per-tracker construction parameter, not a per-call
	// argument. See the per-org loops in runGitHubCycle / runJiraCycle.
	tasks        db.TaskStore
	entities     db.EntityStore
	users        db.UsersStore            // source of the session user's github_username
	repos        db.RepoStore             // configured-repo names for GitHub poller startup
	orgs         db.OrgsStore             // enumerate active orgs at each poll tick + per-org settings (GitHub/Jira base URLs, poll intervals)
	teams        db.TeamsStore            // resolve each org's default team for per-team Jira project rules
	jiraRules    db.JiraStatusRulesStore  // per-team Jira project rules (replaces deleted config.Jira.Projects)
	githubGroups db.TeamGitHubGroupsStore // GitHub-team → TF-team mappings; reconciled (stale-team prune) each GitHub cycle
	secrets      db.SecretStore           // integration creds via SecretStore (keychain in local, vault in multi)
	apps         db.GitHubAppsStore       // per-org App installations + local-NAT backfill for per-installation polling
	resolver     ghclient.Resolver        // per-cycle, per-installation GitHub client resolution (App installation token → PAT)

	// OnError fires when a poll cycle returns an error. Source is "github"
	// or "jira"; orgID identifies the tenant whose cycle errored (empty
	// when the failure is upstream of the per-org loop, e.g. listing
	// active orgs itself). Wired from main to a toast helper so users
	// see the failure without log-diving; nil-safe if caller doesn't
	// set it.
	OnError func(source, orgID string, err error)

	mu       sync.Mutex
	ghStop   chan struct{}
	jiraStop chan struct{}
}

func NewManager(database *sql.DB, bus *eventbus.Bus, users db.UsersStore, tasks db.TaskStore, entities db.EntityStore, repos db.RepoStore, orgs db.OrgsStore, teams db.TeamsStore, jiraRules db.JiraStatusRulesStore, githubGroups db.TeamGitHubGroupsStore, secrets db.SecretStore, apps db.GitHubAppsStore, resolver ghclient.Resolver) *Manager {
	return &Manager{
		database:     database,
		bus:          bus,
		tasks:        tasks,
		entities:     entities,
		users:        users,
		repos:        repos,
		orgs:         orgs,
		teams:        teams,
		jiraRules:    jiraRules,
		githubGroups: githubGroups,
		secrets:      secrets,
		apps:         apps,
		resolver:     resolver,
	}
}

// trackerForOrg builds a Tracker bound to the given tenant. Called
// inside the per-org loops of runGitHubCycle / runJiraCycle so each
// tracker emits events stamped with the correct OrgID and reads/
// writes entities scoped to that tenant. Construction is cheap
// (struct of method holders + store references) so per-cycle
// allocation is fine.
func (m *Manager) trackerForOrg(orgID string) *tracker.Tracker {
	return tracker.New(m.database, m.bus, m.tasks, m.entities, m.repos, orgID)
}

// reportError invokes the OnError callback if set. Centralized so adding
// behavior later (metrics, rate-limiting) has one call site. orgID
// scopes the failure to a tenant; pass empty when the failure is
// process-level (e.g. the cycle's initial ListActiveSystem itself
// errored before any per-org work began).
func (m *Manager) reportError(source, orgID string, err error) {
	if m.OnError != nil {
		m.OnError(source, orgID, err)
	}
}

// RestartAll stops all polling loops and restarts any that are fully
// configured. orgID identifies the tenant whose credentials drive the
// restart — in local mode that's runmode.LocalDefaultOrgID; in multi
// mode this signature lets a future per-org Manager loop call Restart
// per active org. The poller cycles themselves still iterate active
// orgs internally for the per-org tracker dispatch — orgID here is
// the credential-resolution scope (whose PAT do we boot the client
// with), not the polling scope.
func (m *Manager) RestartAll(ctx context.Context, orgID string) {
	m.stopAll()

	orgSet := m.loadOrgSettings(ctx, orgID)

	// GitHub polling resolves per-org credentials per cycle, per
	// installation inside runGitHubCycle (App installation token → PAT),
	// so the only thing startGitHub needs from the trigger org is the tick
	// interval — orgSet.GitHubPollInterval is the process-global cadence.
	m.startGitHub(orgSet.GitHubPollInterval)
	// Jira polling resolves per-org settings/creds/rules inside each
	// cycle, so the only thing startJira needs from the trigger org
	// is the tick interval — orgSet.JiraPollInterval is the process-
	// global cadence (per-org poll cadence is future work).
	m.startJira(orgSet.JiraPollInterval)
}

// RestartGitHub stops and restarts only the GitHub polling loop.
func (m *Manager) RestartGitHub(ctx context.Context, orgID string) {
	m.mu.Lock()
	if m.ghStop != nil {
		close(m.ghStop)
		m.ghStop = nil
		log.Println("[github] tracker stopped")
	}
	m.mu.Unlock()

	orgSet := m.loadOrgSettings(ctx, orgID)
	m.startGitHub(orgSet.GitHubPollInterval)
}

// RestartJira stops and restarts only the Jira polling loop.
func (m *Manager) RestartJira(ctx context.Context, orgID string) {
	m.mu.Lock()
	if m.jiraStop != nil {
		close(m.jiraStop)
		m.jiraStop = nil
		log.Println("[jira] tracker stopped")
	}
	m.mu.Unlock()

	orgSet := m.loadOrgSettings(ctx, orgID)
	m.startJira(orgSet.JiraPollInterval)
}

// loadOrgSettings reads the org's settings or falls back to
// domain.DefaultOrgSettings() on any error. The store already
// returns DefaultOrgSettings() on sql.ErrNoRows; this wrapper covers
// real read errors (transient DB hiccup, RLS in unexpected contexts)
// that would otherwise silently leave orgSet as the Go zero value —
// PollInterval=0 would then trip the `< 10s → 1m` clamp inside start*
// and quietly change the poll cadence to a different value than the
// schema default. Logging + explicit fallback makes the failure
// observable and the behavior deterministic.
func (m *Manager) loadOrgSettings(ctx context.Context, orgID string) domain.OrgSettings {
	orgSet, err := m.orgs.GetSettingsSystem(ctx, orgID)
	if err != nil {
		log.Printf("[poller] load org settings for %s: %v (falling back to defaults)", orgID, err)
		return domain.DefaultOrgSettings()
	}
	return orgSet
}

// loadJiraRules pulls the per-team Jira status rules for the org's
// default team. Local mode collapses to N=1 (the synthetic sentinel
// team); multi-mode per-org Jira project configuration is a future
// concern that follows the same per-team grain. Empty list on error.
func (m *Manager) loadJiraRules(ctx context.Context, orgID string) []domain.JiraProjectStatusRules {
	if m.teams == nil || m.jiraRules == nil {
		return nil
	}
	teamID, err := m.teams.GetDefaultForOrgSystem(ctx, orgID)
	if err != nil || teamID == "" {
		if err != nil {
			log.Printf("[poller] org %s: resolve default team: %v", orgID, err)
		}
		return nil
	}
	rules, err := m.jiraRules.ListForTeamSystem(ctx, teamID)
	if err != nil {
		log.Printf("[poller] org %s team %s: list jira rules: %v", orgID, teamID, err)
		return nil
	}
	return rules
}

// StopAll stops all running polling loops without restarting.
func (m *Manager) StopAll() {
	m.stopAll()
}

// Restart is a convenience alias for RestartAll.
func (m *Manager) Restart(ctx context.Context, orgID string) {
	m.RestartAll(ctx, orgID)
}

func (m *Manager) stopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.ghStop != nil {
		close(m.ghStop)
		m.ghStop = nil
		log.Println("[github] tracker stopped")
	}
	if m.jiraStop != nil {
		close(m.jiraStop)
		m.jiraStop = nil
		log.Println("[jira] tracker stopped")
	}
}

// startGitHub launches the GitHub tracking loop. Each tick iterates
// active orgs and, within each org, every active App installation (with a
// PAT-borrow fallback), resolving a fresh client per installation per cycle.
// Per-org repo lists and per-org credentials are resolved inside the loop so
// a new org/installation added between ticks picks up on the next cycle
// without a poller restart. Local mode collapses to N=1 (the synthetic
// sentinel org). Bounded per-org concurrency is a future optimization —
// sequential is fine given the poll period (≥1 minute baseline).
//
// Runs in both modes (the local-only gate is lifted): multi-mode
// GitHub polling is the per-org App path; local mode keeps the PAT default
// and also supports a locally-registered App via API backfill (webhooks
// don't reach local-NAT).
func (m *Manager) startGitHub(interval time.Duration) {
	if interval < 10*time.Second {
		interval = time.Minute
	}

	stop := make(chan struct{})
	m.mu.Lock()
	m.ghStop = stop
	m.mu.Unlock()

	go func() {
		// Initial poll
		m.runGitHubCycle()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.runGitHubCycle()
			case <-stop:
				return
			}
		}
	}()

	log.Printf("[github] tracker started (interval: %s, per-installation resolution per cycle)", interval)
}

// runGitHubCycle enumerates active orgs and dispatches per-org GitHub
// polling. Per-org failures are logged and reported via OnError but do not
// abort the remaining orgs in the cycle — a transient failure on org A
// shouldn't starve orgs B..N of polls.
func (m *Manager) runGitHubCycle() {
	ctx := context.Background()
	orgIDs, err := m.orgs.ListActiveSystem(ctx)
	if err != nil {
		log.Printf("[github] list active orgs: %v", err)
		m.reportError("github", "", err)
		return
	}
	for _, orgID := range orgIDs {
		m.runGitHubCycleForOrg(ctx, orgID)
	}
}

// runGitHubCycleForOrg polls one org. It resolves a GitHub client PER
// INSTALLATION (App installation tokens expire ~1h, so the client must not be
// hoisted across cycles — Sharp edge 1), maps the org's configured repo set
// onto the installation that can reach each repo, and dispatches a
// per-installation RefreshGitHub over that intersection. With no live
// installation it falls back to the org's PAT (tier 3) over the full
// configured set.
func (m *Manager) runGitHubCycleForOrg(ctx context.Context, orgID string) {
	repos, err := m.repos.ListConfiguredNamesSystem(ctx, orgID)
	if err != nil {
		log.Printf("[github] org %s: load configured repos: %v", orgID, err)
		return
	}
	if len(repos) == 0 {
		return
	}

	// Reconcile the GitHub-team mappings against the live team set — the
	// deletion floor's "periodic refresh" trigger, independent of the
	// poll-dispatch path below so it runs whether the org polls via App
	// or PAT.
	m.reconcileGitHubGroups(ctx, orgID, repos)

	isLocal := runmode.Current() == runmode.ModeLocal

	var installs []domain.OrgGitHubAppInstallation
	if m.apps != nil {
		installs, err = m.apps.ListInstallationsForOrgSystem(ctx, orgID)
		if err != nil {
			log.Printf("[github] org %s: list installations: %v", orgID, err)
		}

		// Local-NAT bonus: webhooks don't reach a local instance, so the
		// installation mirror can't be kept fresh by the webhook receiver.
		// When the org has a registered App, backfill installations from the
		// API on the cycle and re-read. No-op when there's no App.
		if isLocal && m.orgHasRegisteredApp(ctx, orgID) {
			if berr := m.apps.BackfillInstallationsFromAPI(ctx, orgID); berr != nil {
				log.Printf("[github] org %s: installation backfill: %v", orgID, berr)
			} else if installs, err = m.apps.ListInstallationsForOrgSystem(ctx, orgID); err != nil {
				log.Printf("[github] org %s: re-list installations after backfill: %v", orgID, err)
			}
		}
	}

	if len(installs) == 0 {
		m.pollGitHubPAT(ctx, orgID, repos, isLocal)
		return
	}

	// App path: poll each installation over the intersection of the
	// configured set with that installation's repo grant. Token errors are
	// isolated per installation — a failed mint on A must not starve B/C.
	//
	// anyFunctional tracks whether at least one installation yielded a usable
	// installation token. ListInstallationRepos is installation-token-only,
	// so a successful call proves we hold one; a failure can mean the
	// resolver fell back to a PAT client (mint failed but a PAT is
	// configured), which 403s on this endpoint. If NO installation is
	// functional we'd otherwise leave the org unpolled despite an available
	// PAT — so fall through to the PAT path in that case.
	covered := make(map[string]bool, len(repos))
	anyFunctional := false
	for _, inst := range installs {
		client, cerr := m.resolver.ClientFor(ctx, orgID, inst.AccountLogin)
		if cerr != nil {
			log.Printf("[github] org %s: resolve client for installation %s (%s): %v", orgID, inst.InstallationID, inst.AccountLogin, cerr)
			m.reportError("github", orgID, cerr)
			continue
		}
		grant, gerr := client.ListInstallationRepos()
		if gerr != nil {
			log.Printf("[github] org %s: list installation repos for %s: %v", orgID, inst.AccountLogin, gerr)
			m.reportError("github", orgID, gerr)
			continue
		}
		anyFunctional = true
		scoped := intersectConfigured(repos, grant, covered)
		if len(scoped) == 0 {
			continue
		}
		// App tokens have no "me" — drop the username axis for discovery
		// (Sharp edge 2). Predicates still match per-PR fields downstream.
		if _, rerr := m.trackerForOrg(orgID).RefreshGitHub(client, "", nil, scoped); rerr != nil {
			log.Printf("[github] org %s installation %s: tracker error: %v", orgID, inst.AccountLogin, rerr)
			m.reportError("github", orgID, rerr)
		}
	}

	// No installation produced a usable installation token (every mint/list
	// failed). The resolver may have a working PAT behind those failures —
	// poll the configured set through it rather than leaving the org dark.
	// ErrNoGitHubCredentials inside pollGitHubPAT means there's genuinely no
	// fallback, and it skips silently.
	if !anyFunctional {
		m.pollGitHubPAT(ctx, orgID, repos, isLocal)
		return
	}

	// Configured repos that no functional installation grants — the App isn't
	// installed on them. Log once so the gap is visible without being an
	// error. (Skipped when we fell back to PAT above, which polls them all.)
	for _, r := range repos {
		if !covered[r] {
			log.Printf("[github] org %s: configured repo %s not in any App installation grant — skipping", orgID, r)
		}
	}
}

// reconcileGitHubGroups is the GitHub-team-deletion reconcile floor — the
// "periodic refresh" trigger the SKY-369 lifecycle describes, run every
// GitHub poll cycle so team_github_groups stays fresh for the routing layer
// without depending on someone opening the settings page. For each distinct
// GitHub org behind the configured repos it fetches the live team set and
// prunes mapping rows pointing at teams that no longer exist.
//
// Credentials resolve through the same per-owner resolver the poll dispatch
// uses (App installation token → org PAT), so it's always the single
// org-level identity — the editor can only create a mapping for a team that
// identity can see, so a present team is never wrongly pruned. Non-destructive
// on uncertainty: owners with no credential or an unreadable org are skipped,
// a fetch error never prunes, and an empty team list is skipped (an empty
// result is ambiguous — a user account or zero visibility — where "delete
// everything for this org" would be wrong). A non-empty list is the org
// credential's authoritative view, so a missing slug is a real deletion.
func (m *Manager) reconcileGitHubGroups(ctx context.Context, orgID string, repos []string) {
	if m.githubGroups == nil {
		return
	}
	seen := map[string]bool{}
	for _, full := range repos {
		owner, _, ok := strings.Cut(full, "/")
		owner = strings.ToLower(strings.TrimSpace(owner))
		if !ok || owner == "" || seen[owner] {
			continue
		}
		seen[owner] = true

		client, err := m.resolver.ClientFor(ctx, orgID, owner)
		if err != nil {
			if !errors.Is(err, ghclient.ErrNoGitHubCredentials) {
				log.Printf("[github-groups] org %s: resolve client for %s: %v", orgID, owner, err)
			}
			continue
		}
		teams, err := client.ListOrgTeams(owner)
		if err != nil {
			log.Printf("[github-groups] org %s: list teams for %s: %v", orgID, owner, err)
			continue
		}
		slugs := make([]string, 0, len(teams))
		for _, t := range teams {
			if t.Slug != "" {
				slugs = append(slugs, t.Slug)
			}
		}
		if len(slugs) == 0 {
			continue
		}
		if n, err := m.githubGroups.PruneMissingSystem(ctx, orgID, owner, slugs); err != nil {
			log.Printf("[github-groups] org %s: reconcile prune for %s: %v", orgID, owner, err)
		} else if n > 0 {
			log.Printf("[github-groups] org %s: pruned %d stale mapping(s) for deleted GitHub teams under %s", orgID, n, owner)
		}
	}
}

// pollGitHubPAT runs the tier-3 PAT-borrow path for an org with no live App
// installation. The resolver returns the org's PAT client; ErrNoGitHubCredentials
// means the org simply isn't configured for GitHub and is skipped silently.
//
// In local mode the poller acts as the lone local user, so it reads their
// github_username + team memberships to drive the user-perspective dashboard
// backfill and team-based review-request detection. Multi-mode PAT fallback
// has no local sentinel user, so it passes no username (org-wide REST
// discovery doesn't need one; dashboard history is local/PAT-only).
func (m *Manager) pollGitHubPAT(ctx context.Context, orgID string, repos []string, isLocal bool) {
	client, err := m.resolver.ClientFor(ctx, orgID, "")
	if err != nil {
		if errors.Is(err, ghclient.ErrNoGitHubCredentials) {
			return // not configured for GitHub — silent skip
		}
		log.Printf("[github] org %s: resolve PAT client: %v", orgID, err)
		m.reportError("github", orgID, err)
		return
	}

	var username string
	var userTeams []string
	if isLocal {
		username, err = m.users.GetGitHubUsernameSystem(ctx, runmode.LocalDefaultUserID)
		if err != nil {
			log.Printf("[github] org %s: read users.github_username: %v", orgID, err)
			return
		}
		if teams, terr := client.ListMyTeams(); terr != nil {
			log.Printf("[github] org %s: list teams: %v (team-based review requests will be missed this cycle)", orgID, terr)
		} else {
			userTeams = teams
		}
	}

	if _, err := m.trackerForOrg(orgID).RefreshGitHub(client, username, userTeams, repos); err != nil {
		log.Printf("[github] org %s: tracker error: %v", orgID, err)
		m.reportError("github", orgID, err)
	}
}

// orgHasRegisteredApp reports whether the org has an active GitHub App
// registration. Used to gate the local-NAT installation backfill so orgs
// without an App don't pay a no-op API round-trip each cycle.
func (m *Manager) orgHasRegisteredApp(ctx context.Context, orgID string) bool {
	if m.apps == nil {
		return false
	}
	app, err := m.apps.GetForOrgSystem(ctx, orgID)
	if err != nil {
		log.Printf("[github] org %s: read App registration: %v", orgID, err)
		return false
	}
	return app != nil && app.Active
}

// intersectConfigured returns the configured repos reachable through one
// installation's grant, marking each in covered so the caller can report
// configured repos that no installation grants. Matching is case-insensitive
// on the "owner/repo" slug (GitHub logins are case-insensitive).
func intersectConfigured(configured []string, grant []ghclient.UserRepo, covered map[string]bool) []string {
	granted := make(map[string]bool, len(grant))
	for _, g := range grant {
		granted[strings.ToLower(g.FullName)] = true
	}
	var out []string
	for _, r := range configured {
		if granted[strings.ToLower(r)] {
			out = append(out, r)
			covered[r] = true
		}
	}
	return out
}

// startJira launches the Jira tracking loop. The outer goroutine
// just drives the tick; runJiraCycle resolves per-org Jira creds +
// project rules + base URL inside the per-org loop so each tenant
// is polled with its own configuration. Orgs without a connected
// Jira integration (no PAT, no URL, no rules) are silently skipped
// each cycle so adding/removing tenants doesn't need a poller
// restart.
//
// Gated to local mode (matching startGitHub). The per-org loop
// shape is correct but SecretStore.Get in Postgres requires
// request.jwt.claims (vault_* enforces org_id ==
// tf.current_org_id()), and the poller goroutine has no claims
// context. Multi-mode Jira polling needs either a SystemGet-style
// SecretStore variant or per-org SyntheticClaimsWithTx routing.
// Until then, multi-mode tenants don't get background polling;
// their data refreshes on the next interactive flow.
//
// interval is process-global (per-org cadence is future work); in
// local mode N=1 so the triggering org's interval IS the global
// interval.
//
// TODO: multi-mode Jira polling — add system-mode SecretStore
// access path (SKY-347 / D11 follow-up) then drop the gate below.
func (m *Manager) startJira(interval time.Duration) {
	if runmode.Current() != runmode.ModeLocal {
		log.Println("[jira] tracker not started — multi-mode Jira polling requires per-org system credentials (see TODO in startJira)")
		return
	}
	if interval < 10*time.Second {
		interval = time.Minute
	}

	stop := make(chan struct{})
	m.mu.Lock()
	m.jiraStop = stop
	m.mu.Unlock()

	go func() {
		// Initial poll
		m.runJiraCycle()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.runJiraCycle()
			case <-stop:
				return
			}
		}
	}()

	log.Printf("[jira] tracker started (interval: %s, per-org config resolved each cycle)", interval)
}

// runJiraCycle enumerates active orgs and dispatches a per-org
// RefreshJira. Each org's creds + rules + base URL are resolved
// inside the loop so two tenants with different Jira PATs / project
// configurations don't share state. Orgs not configured for Jira
// are skipped silently; per-org failures are logged + reported via
// OnError but do not abort the remaining orgs in the cycle.
func (m *Manager) runJiraCycle() {
	ctx := context.Background()
	orgIDs, err := m.orgs.ListActiveSystem(ctx)
	if err != nil {
		log.Printf("[jira] list active orgs: %v", err)
		m.reportError("jira", "", err)
		return
	}
	for _, orgID := range orgIDs {
		orgSet, oerr := m.orgs.GetSettingsSystem(ctx, orgID)
		if oerr != nil {
			log.Printf("[jira] org %s: load settings: %v", orgID, oerr)
			m.reportError("jira", orgID, oerr)
			continue
		}
		creds, lerr := integrations.Load(ctx, m.secrets, orgID)
		if lerr != nil {
			log.Printf("[jira] org %s: load creds: %v", orgID, lerr)
			m.reportError("jira", orgID, lerr)
			continue
		}
		rules := m.loadJiraRules(ctx, orgID)
		if creds.JiraPAT == "" || creds.JiraURL == "" || len(rules) == 0 {
			// Not configured for Jira (or rules missing). Skip
			// silently — adding/removing a tenant's Jira config
			// doesn't need a poller restart this way.
			continue
		}
		baseURL := orgSet.JiraBaseURL
		if baseURL == "" {
			baseURL = creds.JiraURL
		}
		client := jiraclient.NewClient(creds.JiraURL, creds.JiraPAT)
		projects := toTrackerJiraRules(rules)
		if _, err := m.trackerForOrg(orgID).RefreshJira(client, baseURL, projects); err != nil {
			log.Printf("[jira] org %s: tracker error: %v", orgID, err)
			m.reportError("jira", orgID, err)
		}
	}
}

// toTrackerJiraRules converts the domain per-project rule slice into
// the tracker-local view. Kept narrow on purpose — the tracker package
// only needs pickup/done members, not the canonicals.
func toTrackerJiraRules(rules []domain.JiraProjectStatusRules) tracker.JiraRules {
	out := make(tracker.JiraRules, 0, len(rules))
	for _, p := range rules {
		out = append(out, tracker.JiraProjectRules{
			Key:           p.ProjectKey,
			PickupMembers: p.PickupMembers,
			DoneMembers:   p.DoneMembers,
		})
	}
	return out
}
