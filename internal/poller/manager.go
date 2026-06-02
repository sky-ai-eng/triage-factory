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
	pub      tracker.Publisher // SKY-414: the durable ingestor in production; *eventbus.Bus in tests
	// tracker dependencies are held instead of a single Tracker instance
	// because each poll cycle constructs one Tracker per active org —
	// orgID is a per-tracker construction parameter, not a per-call
	// argument. See the per-org loops in runGitHubCycle / runJiraCycle.
	tasks        db.TaskStore
	entities     db.EntityStore
	users        db.UsersStore            // source of the local user's host-scoped GitHub identity (SKY-396)
	repos        db.RepoStore             // configured-repo names for GitHub poller startup
	orgs         db.OrgsStore             // enumerate active orgs at each poll tick + per-org settings (GitHub/Jira base URLs, poll intervals)
	jiraRules    db.JiraStatusRulesStore  // per-team Jira project rules; discovery polls the org-wide union (every team's rules)
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

	// dueMu guards nextPoll, the scheduler clock. Each source runs ONE
	// base-tick loop (every basePollInterval) that polls an org only once
	// its own configured interval has elapsed, so orgs keep individual
	// cadences without a goroutine each. Key is "source/orgID"; value is
	// the earliest time that org is next eligible. Guarded because a
	// Restart can briefly overlap an old and a new poll goroutine.
	dueMu    sync.Mutex
	nextPoll map[string]time.Time
}

func NewManager(database *sql.DB, pub tracker.Publisher, users db.UsersStore, tasks db.TaskStore, entities db.EntityStore, repos db.RepoStore, orgs db.OrgsStore, jiraRules db.JiraStatusRulesStore, githubGroups db.TeamGitHubGroupsStore, secrets db.SecretStore, apps db.GitHubAppsStore, resolver ghclient.Resolver) *Manager {
	return &Manager{
		database:     database,
		pub:          pub,
		tasks:        tasks,
		entities:     entities,
		users:        users,
		repos:        repos,
		orgs:         orgs,
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
	return tracker.New(m.database, m.pub, m.tasks, m.entities, m.repos, orgID)
}

// reviewerResolver builds the per-cycle TF-known reviewer resolver.
// A non-empty username is the local/PAT "session user" signal → resolve
// against that one login + their team memberships, preserving today's N=1
// behavior. Every other path (multi mode, or local+App where there's no
// session login) resolves against the stores, host from org_settings.
func (m *Manager) reviewerResolver(ctx context.Context, orgID, username string, userTeams []string) tracker.ReviewerResolver {
	if username != "" {
		return tracker.NewLocalReviewerResolver(username, userTeams)
	}
	host := ""
	if m.orgs != nil {
		if orgSet, err := m.orgs.GetSettingsSystem(ctx, orgID); err != nil {
			log.Printf("[github] org %s: read settings for reviewer resolver: %v", orgID, err)
		} else {
			// Resolve to the effective host (empty → github.com) so the
			// reverse identity lookup matches rows captured under the
			// canonical host (e.g. the OAuth login-claim's literal github.com).
			host = db.EffectiveGitHubHost(orgSet.GitHubBaseURL)
		}
	}
	return tracker.NewStoreReviewerResolver(ctx, orgID, host, m.users, m.githubGroups)
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

// RestartAll stops all polling loops and restarts them. There are no
// parameters: the poll cycles fan out over every active org internally
// (runGitHubCycle / runJiraCycle → OrgsStore.ListActiveSystem) and poll each
// org at its own configured cadence (see runGitHubCycle's due gate), so this
// is mode-agnostic — local mode is N=1 (the sentinel org), multi mode is N
// active tenants. The lifecycle is neither per-org nor per-interval; start*
// just spins up the shared base-tick loop. It is SCHEDULE-NEUTRAL: orgs
// already on a cadence keep their slots, so a restart doesn't re-poll the
// fleet. Callers that want a changed org polled now use PollSoon to re-due
// that org specifically — never a fleet-wide reset (the GHES/GHEC stampede).
func (m *Manager) RestartAll() {
	m.stopAll()
	m.startGitHub()
	m.startJira()
}

// RestartJira stops and restarts only the Jira polling loop. Multi-mode
// Jira polling is gated off inside startJira until per-org system creds
// land, so the restart is a no-op past the stop in multi mode.
func (m *Manager) RestartJira() {
	m.mu.Lock()
	if m.jiraStop != nil {
		close(m.jiraStop)
		m.jiraStop = nil
		log.Println("[jira] tracker stopped")
	}
	m.mu.Unlock()

	m.startJira()
}

// loadOrgSettings reads the org's settings or falls back to
// domain.DefaultOrgSettings() on any error. The store already
// returns DefaultOrgSettings() on sql.ErrNoRows; this wrapper covers
// real read errors (transient DB hiccup, RLS in unexpected contexts)
// that would otherwise silently leave orgSet as the Go zero value —
// PollInterval=0 would then be floored to basePollInterval by
// clampPollInterval, quietly changing the cadence away from the schema
// default. Logging + explicit fallback makes the failure observable and
// the behavior deterministic.
func (m *Manager) loadOrgSettings(ctx context.Context, orgID string) domain.OrgSettings {
	orgSet, err := m.orgs.GetSettingsSystem(ctx, orgID)
	if err != nil {
		log.Printf("[poller] load org settings for %s: %v (falling back to defaults)", orgID, err)
		return domain.DefaultOrgSettings()
	}
	return orgSet
}

// basePollInterval is the scheduler's wake granularity and the floor for any
// per-org interval. Each source runs ONE loop that wakes this often and polls
// the orgs whose own configured interval has elapsed — so per-org cadence is
// honored at this resolution without a goroutine per org. Every active org is
// re-listed each wake, so this is also the minimum org-roster refresh.
const basePollInterval = 30 * time.Second

// clampPollInterval floors a configured interval at basePollInterval: a
// cadence finer than the base tick can't be honored (the loop never wakes
// that often), and a zero value (unset / read error) must not collapse to
// "poll every tick".
func clampPollInterval(d time.Duration) time.Duration {
	if d < basePollInterval {
		return basePollInterval
	}
	return d
}

// pollDue reports whether orgID is eligible for a poll of source at now. An
// org with no recorded slot (never polled, pruned, or PollSoon'd) is due.
//
// pollDue + schedulePoll are check-then-act under two separate dueMu
// acquisitions, not an atomic CAS. The only place two cyclers run concurrently
// is a LOCAL restart bounce (multi never restarts the loop — see PollSoon);
// there N=1, and the worst case is the sentinel polled twice in quick
// succession, which the tracker's snapshot-diff dedups to zero duplicate
// events. Nothing resets the schedule mid-restart, so an org is never skipped.
func (m *Manager) pollDue(source, orgID string, now time.Time) bool {
	m.dueMu.Lock()
	defer m.dueMu.Unlock()
	next, ok := m.nextPoll[pollKey(source, orgID)]
	return !ok || !now.Before(next)
}

// pollKey is the nextPoll map key for a (source, org) pair. The separator is
// NUL, which can't appear in a source name ("github"/"jira") or a UUID orgID,
// so the key is unambiguous and prunePoll's prefix split is exact.
func pollKey(source, orgID string) string { return source + "\x00" + orgID }

// schedulePoll records the earliest time orgID is next eligible for a poll of
// source.
func (m *Manager) schedulePoll(source, orgID string, at time.Time) {
	m.dueMu.Lock()
	defer m.dueMu.Unlock()
	if m.nextPoll == nil {
		m.nextPoll = make(map[string]time.Time)
	}
	m.nextPoll[pollKey(source, orgID)] = at
}

// prunePoll drops scheduler slots for orgs no longer in the active set, so
// the map doesn't grow unbounded as orgs churn and a deactivated→reactivated
// org isn't held back by a stale future slot. Called once per cycle with the
// freshly-listed active orgs.
func (m *Manager) prunePoll(source string, activeOrgIDs []string) {
	active := make(map[string]bool, len(activeOrgIDs))
	for _, id := range activeOrgIDs {
		active[id] = true
	}
	prefix := source + "\x00"
	m.dueMu.Lock()
	defer m.dueMu.Unlock()
	for key := range m.nextPoll {
		if strings.HasPrefix(key, prefix) && !active[strings.TrimPrefix(key, prefix)] {
			delete(m.nextPoll, key)
		}
	}
}

// PollSoon makes orgID immediately eligible for the next poll of source by
// dropping its scheduler slot — and ONLY its slot. The running loop picks it
// up on its next wake (≤ basePollInterval). This is the targeted, load-safe
// alternative to restarting the process-global loop on a config change:
// clearing every slot would re-poll every tenant at once, stampeding shared
// GHES/GHEC API budgets — the exact thing per-org intervals exist to prevent.
// Deleting a missing key (or from a nil map) is a no-op, so this is safe
// before the first poll and harmless if the loop isn't running yet.
func (m *Manager) PollSoon(source, orgID string) {
	m.dueMu.Lock()
	defer m.dueMu.Unlock()
	delete(m.nextPoll, pollKey(source, orgID))
}

// loadJiraRules pulls the UNION of every team's per-project Jira status
// rules across the org (not just the default team's), so a non-default
// team's Jira config is discovered and polled too. toTrackerJiraRules
// then merges the rows per project_key. Local mode collapses to N=1 (the
// single default team), where the union equals what the poller read
// before. Empty list on error.
func (m *Manager) loadJiraRules(ctx context.Context, orgID string) []domain.JiraProjectStatusRules {
	if m.jiraRules == nil {
		return nil
	}
	rules, err := m.jiraRules.ListForOrgSystem(ctx, orgID)
	if err != nil {
		log.Printf("[poller] org %s: list jira rules (org union): %v", orgID, err)
		return nil
	}
	return rules
}

// StopAll stops all running polling loops without restarting.
func (m *Manager) StopAll() {
	m.stopAll()
}

// Restart is a convenience alias for RestartAll.
func (m *Manager) Restart() {
	m.RestartAll()
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

// startGitHub launches the GitHub tracking loop: ONE goroutine that wakes
// every basePollInterval and, each wake, polls the active orgs whose own
// configured interval has elapsed (runGitHubCycle's due gate). Per-org repo
// lists, credentials, and intervals are resolved inside the loop, so a new
// org/installation/interval added between wakes picks up on the next wake
// without a poller restart. Local mode collapses to N=1 (the synthetic
// sentinel org). Bounded per-org concurrency is a future optimization —
// sequential is fine at this cadence.
//
// Runs in both modes (the local-only gate is lifted): multi-mode
// GitHub polling is the per-org App path; local mode keeps the PAT default
// and also supports a locally-registered App via API backfill (webhooks
// don't reach local-NAT).
func (m *Manager) startGitHub() {
	stop := make(chan struct{})
	m.mu.Lock()
	m.ghStop = stop
	m.mu.Unlock()

	go func() {
		// Initial poll. Orgs with no slot yet (cold boot: all of them) are due
		// and polled once; orgs already on a cadence keep it, so a restart
		// doesn't re-poll the fleet. PollSoon re-dues a single org on demand.
		m.runGitHubCycle()

		ticker := time.NewTicker(basePollInterval)
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

	log.Printf("[github] tracker started (base tick %s, per-org cadence resolved each wake)", basePollInterval)
}

// runGitHubCycle enumerates active orgs and dispatches per-org GitHub polling
// for the orgs whose configured interval has elapsed (others are skipped this
// wake). Each polled org's next slot is reserved before the poll using its
// freshly-read, clamped interval, so an interval change applies from the next
// poll without a restart. Per-org failures are logged and reported via
// OnError but do not abort the remaining orgs in the cycle — a transient
// failure on org A shouldn't starve orgs B..N of polls.
func (m *Manager) runGitHubCycle() {
	ctx := context.Background()
	now := time.Now()
	orgIDs, err := m.orgs.ListActiveSystem(ctx)
	if err != nil {
		log.Printf("[github] list active orgs: %v", err)
		m.reportError("github", "", err)
		return
	}
	for _, orgID := range orgIDs {
		if !m.pollDue("github", orgID, now) {
			continue
		}
		interval := clampPollInterval(m.loadOrgSettings(ctx, orgID).GitHubPollInterval)
		m.schedulePoll("github", orgID, now.Add(interval))
		m.runGitHubCycleForOrg(ctx, orgID)
	}
	m.prunePoll("github", orgIDs)
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
	// App tokens have no "me", so the resolver is store-backed (host from
	// org_settings). Built once per org cycle — host is stable across the
	// per-installation loop below.
	resolver := m.reviewerResolver(ctx, orgID, "", nil)

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
		if _, rerr := m.trackerForOrg(orgID).RefreshGitHub(client, "", scoped, resolver); rerr != nil {
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
// host-scoped GitHub login + team memberships to drive the user-perspective dashboard
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
		// Identity is host-scoped (SKY-396): resolve the org's GitHub host
		// from org_settings, then read the local user's login for that
		// (user, host) pair. Boot-time goroutine with no JWT claims → the
		// `...System` admin-pool variants.
		orgSet, serr := m.orgs.GetSettingsSystem(ctx, orgID)
		if serr != nil {
			log.Printf("[github] org %s: read org settings: %v", orgID, serr)
			return
		}
		username, err = m.users.GetGitHubLoginSystem(ctx, runmode.LocalDefaultUserID, orgSet.GitHubBaseURL)
		if err != nil {
			log.Printf("[github] org %s: read github identity: %v", orgID, err)
			return
		}
		if teams, terr := client.ListMyTeams(); terr != nil {
			log.Printf("[github] org %s: list teams: %v (team-based review requests will be missed this cycle)", orgID, terr)
		} else {
			userTeams = teams
		}
	}

	resolver := m.reviewerResolver(ctx, orgID, username, userTeams)
	if _, err := m.trackerForOrg(orgID).RefreshGitHub(client, username, repos, resolver); err != nil {
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

// startJira launches the Jira tracking loop: ONE goroutine that wakes every
// basePollInterval; runJiraCycle then polls the active orgs whose interval
// has elapsed, resolving each tenant's Jira creds + project rules + base URL
// + interval inside the loop. Orgs without a connected Jira integration (no
// PAT, no URL, no rules) are silently skipped each cycle, so adding/removing
// a tenant's Jira config doesn't need a poller restart.
//
// Gated to local mode (the gate startGitHub used to share). The per-org loop
// shape is correct, but SecretStore.Get in Postgres requires
// request.jwt.claims (vault_* enforces org_id == tf.current_org_id()), and
// the poller goroutine has no claims context. Multi-mode Jira polling needs
// either a SystemGet-style SecretStore variant or per-org
// SyntheticClaimsWithTx routing. Until then, multi-mode tenants don't get
// background Jira polling; their data refreshes on the next interactive flow.
//
// TODO: multi-mode Jira polling — add system-mode SecretStore
// access path (SKY-347 / D11 follow-up) then drop the gate below.
func (m *Manager) startJira() {
	if runmode.Current() != runmode.ModeLocal {
		log.Println("[jira] tracker not started — multi-mode Jira polling requires per-org system credentials (see TODO in startJira)")
		return
	}

	stop := make(chan struct{})
	m.mu.Lock()
	m.jiraStop = stop
	m.mu.Unlock()

	go func() {
		// Initial poll. Same cold-boot-vs-cadence semantics as startGitHub.
		m.runJiraCycle()

		ticker := time.NewTicker(basePollInterval)
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

	log.Printf("[jira] tracker started (base tick %s, per-org cadence resolved each wake)", basePollInterval)
}

// runJiraCycle enumerates active orgs and dispatches a per-org RefreshJira for
// the orgs whose configured interval has elapsed (others are skipped this
// wake). Each org's creds + rules + base URL + interval are resolved inside
// the loop so two tenants with different Jira PATs / project configurations
// don't share state; the next slot is reserved from the freshly-read interval
// once settings load. Orgs not configured for Jira are skipped silently;
// per-org failures are logged + reported via OnError but do not abort the
// remaining orgs in the cycle.
func (m *Manager) runJiraCycle() {
	ctx := context.Background()
	now := time.Now()
	orgIDs, err := m.orgs.ListActiveSystem(ctx)
	if err != nil {
		log.Printf("[jira] list active orgs: %v", err)
		m.reportError("jira", "", err)
		return
	}
	for _, orgID := range orgIDs {
		if !m.pollDue("jira", orgID, now) {
			continue
		}
		orgSet, oerr := m.orgs.GetSettingsSystem(ctx, orgID)
		if oerr != nil {
			log.Printf("[jira] org %s: load settings: %v", orgID, oerr)
			m.reportError("jira", orgID, oerr)
			continue // leave unscheduled → retry next base tick (interval unknown)
		}
		// Reserve the next slot from the freshly-read interval BEFORE loading
		// creds/rules below. The asymmetry is deliberate: a settings-read
		// failure (above) leaves the org unscheduled so it retries at the next
		// base tick (we don't know its interval); a creds/rules failure (below)
		// keeps this slot, so a likely-persistent auth failure backs off to the
		// org's own cadence instead of hammering every base tick.
		m.schedulePoll("jira", orgID, now.Add(clampPollInterval(orgSet.JiraPollInterval)))
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
	m.prunePoll("jira", orgIDs)
}

// toTrackerJiraRules collapses the org-wide rule union into the
// tracker-local per-project view — one entry per distinct project_key, so
// a project two teams both track is polled once (entities are org-shared:
// one row per Jira issue regardless of how many teams track its project).
//
// PickupMembers / DoneMembers are Jira *workflow status names* ("To Do",
// "Backlog", "Done"), NOT people or teams — they drive the discovery JQL
// (which statuses count as available-for-pickup vs terminal). When several
// teams track the same project_key with divergent status sets, those sets
// are MERGED (set-union, first-seen order preserved): the most-permissive
// interpretation for discovery + terminal detection, so no team's
// pickup-able issue is missed and any team's notion of "done" closes the
// shared entity. Team-specific scoping lives downstream in the router
// gate, not in discovery. Kept narrow on purpose — the tracker only needs
// pickup/done statuses, not the canonicals. Input arrives ordered by
// (project_key, team_id), so the merged output is deterministic.
func toTrackerJiraRules(rules []domain.JiraProjectStatusRules) tracker.JiraRules {
	type merged struct {
		pickup, done         []string
		pickupSeen, doneSeen map[string]bool
	}
	byKey := map[string]*merged{}
	order := make([]string, 0, len(rules))
	addUnique := func(dst []string, seen map[string]bool, src []string) []string {
		for _, s := range src {
			if !seen[s] {
				seen[s] = true
				dst = append(dst, s)
			}
		}
		return dst
	}
	for _, p := range rules {
		m := byKey[p.ProjectKey]
		if m == nil {
			m = &merged{pickupSeen: map[string]bool{}, doneSeen: map[string]bool{}}
			byKey[p.ProjectKey] = m
			order = append(order, p.ProjectKey)
		}
		m.pickup = addUnique(m.pickup, m.pickupSeen, p.PickupMembers)
		m.done = addUnique(m.done, m.doneSeen, p.DoneMembers)
	}
	out := make(tracker.JiraRules, 0, len(order))
	for _, key := range order {
		m := byKey[key]
		out = append(out, tracker.JiraProjectRules{
			Key:           key,
			PickupMembers: m.pickup,
			DoneMembers:   m.done,
		})
	}
	return out
}
