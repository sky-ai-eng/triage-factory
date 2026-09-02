package poller

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/reporename"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"github.com/sky-ai-eng/triage-factory/internal/tracker"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Manager manages the lifecycle of polling loops, allowing them to be
// stopped and restarted when credentials or config change.
type Manager struct {
	database *sql.DB
	pub      tracker.Publisher // the durable ingestor in production; *eventbus.Bus in tests
	// tracker dependencies are held instead of a single Tracker instance
	// because each poll cycle constructs one Tracker per active org —
	// orgID is a per-tracker construction parameter, not a per-call
	// argument. See the per-org loops in runGitHubCycle / runJiraCycle.
	tasks    db.TaskStore
	entities db.EntityStore
	// eventQueue is the durable outbox the tracker's diff arms write
	// their transitions to, in the same transaction as the snapshot CAS
	// those transitions were diffed against. Held here only to construct
	// the per-org Tracker — the poller itself never touches the queue.
	eventQueue   db.EventQueueStore
	users        db.UsersStore            // source of the local user's host-scoped GitHub identity
	repos        db.RepositoryStore       // configured-repo names for GitHub poller startup
	orgs         db.OrgsStore             // enumerate active orgs at each poll tick + per-org settings (GitHub/Jira base URLs, poll intervals)
	jiraRules    db.JiraStatusRulesStore  // per-team Jira project rules; discovery polls the org-wide union (every team's rules)
	githubGroups db.TeamGitHubGroupsStore // GitHub-team → TF-team mappings; reconciled (stale-team prune) each GitHub cycle
	secrets      db.SecretStore           // integration creds via SecretStore (keychain in local, vault in multi)
	apps         db.GitHubAppsStore       // per-org App installations, read per cycle to fan the poll out across them
	resolver     ghclient.Resolver        // per-cycle, per-installation GitHub client resolution (App installation token → PAT)

	// ReconcileGrant refreshes the org's App-installation mirror — which
	// installations exist, how wide each grant is, and which repositories each
	// reaches — from GitHub, before this cycle reads any of it. nil disables
	// the pass (tests, and any embedder that hasn't wired it).
	//
	// It runs inside the poll cycle rather than off the poll-completion
	// sentinel every other background pass hangs from, for a reason the
	// sentinel cannot serve: a cycle that finds no installations reports
	// degraded and returns WITHOUT emitting a completion, so a subscriber would
	// never fire for exactly the org whose mirror is broken. Running here also
	// means the installations this cycle is about to fan out over are the ones
	// GitHub reports right now, not the ones it reported a cycle ago.
	//
	// Leader gating comes from the poller itself: in multi mode the scheduler
	// runs only on the brain-lease holder, and in local mode it always runs.
	// That is the same gate the tracker, the drain worker and the sweeper sit
	// behind, so no second mechanism is introduced for this one pass.
	ReconcileGrant func(ctx context.Context, orgID string) error

	// RefreshManagedInstallations refreshes the installation set of every
	// workspace on the deployment's shared GitHub App, from one listing. nil
	// disables the pass (tests, and any embedder that hasn't wired it).
	//
	// It is a deployment singleton rather than a per-org call like
	// ReconcileGrant, because under a shared key the listing is the same answer
	// whoever asks — so it runs at most ONCE per cycle, ahead of the first due
	// org's poll, and not at all on a wake where no org is due. That keeps the
	// shared rate budget's spend independent of the tenant count and puts the
	// refresh before ReconcileGrant reads each org's installations, so a lifted
	// suspension or a renamed account is already on the row that read sees.
	//
	// Same leader gating as ReconcileGrant: the scheduler that calls it runs
	// only on the brain-lease holder in multi mode, so a standby never lists.
	RefreshManagedInstallations func(ctx context.Context) error

	// EventSources reads the org's declared per-source policy — today, whether
	// an org admin has paused a source. nil disables the skip (tests, and any
	// embedder that hasn't wired it), which costs API calls and nothing else:
	// the router drops a paused source's events at the single funnel every
	// producer crosses, so a poller that forgets to skip wastes a request
	// rather than leaking an event.
	EventSources db.OrgEventSourceStore

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

	// dashboardBackfillInflight collapses concurrent dashboard-history
	// backfill kicks for the same (org, user, host) within this process so a
	// burst of dashboard opens triggers at most one search pass (TFAC-396).
	// Keys are released when the backfill finishes; the durable
	// dashboard_backfilled_at marker is what prevents re-running across
	// processes / restarts. Zero value is ready to use.
	dashboardBackfillInflight sync.Map

	// cursorMu guards ghCursor, the per-org GitHub round-robin repo cursor
	// (TFAC-571). In-memory only for v1 (per the ticket's explicit
	// allowance) — a restart loses the cursor and the next cycle starts
	// from the head, same as today. Key is orgID; value is the full-name
	// ("owner/repo") of the next repo to refresh, or absent/"" for "start
	// at the head" (no cursor set, or the last cycle fully wrapped).
	cursorMu sync.Mutex
	ghCursor map[string]string

	// heartbeatMu guards lastGithubTick/lastJiraTick — stamped once per
	// wake of runGitHubCycle/runJiraCycle regardless of whether any org
	// was due that wake (TFAC-573). Distinct from nextPoll/dueMu: nextPoll
	// only gets a slot for orgs actually polled, so an org roster of zero
	// (or every org not-yet-due) would otherwise look identical to a dead
	// loop. This is the /readyz hard "is the base-tick loop alive" check.
	heartbeatMu    sync.Mutex
	lastGithubTick time.Time
	lastJiraTick   time.Time

	// pollSuccessMu guards lastGithubSuccess/lastJiraSuccess — per-org
	// timestamp of the last poll that completed a RefreshGitHub/RefreshJira
	// call without error (TFAC-573). This is the /readyz soft signal: it
	// proves an org's poll actually completed, where the heartbeat above
	// only proves the loop woke.
	pollSuccessMu     sync.Mutex
	lastGithubSuccess map[string]time.Time
	lastJiraSuccess   map[string]time.Time
}

func NewManager(database *sql.DB, pub tracker.Publisher, users db.UsersStore, tasks db.TaskStore, entities db.EntityStore, repos db.RepositoryStore, eventQueue db.EventQueueStore, orgs db.OrgsStore, jiraRules db.JiraStatusRulesStore, githubGroups db.TeamGitHubGroupsStore, secrets db.SecretStore, apps db.GitHubAppsStore, resolver ghclient.Resolver) *Manager {
	return &Manager{
		database:     database,
		pub:          pub,
		tasks:        tasks,
		entities:     entities,
		eventQueue:   eventQueue,
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
	return tracker.New(m.database, m.pub, m.tasks, m.entities, m.repos, m.eventQueue, orgID)
}

// sourceDisabled reports whether this cycle must not run, and the span outcome
// that says why. It is not an optimization: a source an org admin turned off
// makes zero API calls, which is a promise measurable from the other end — an
// operator watching their own Jira Data Center is entitled to see the traffic
// go to nothing, and a cycle that polls anyway is a broken switch, not a
// wasted call.
//
// The GitHub cycle does not call this, and it is not an omission: GitHub has no
// off switch (eventsource.Disableable), so the answer there is a constant and a
// check would read as a suppression that can happen.
//
// So an unreadable policy skips. The two mistakes are not symmetric: skipping
// wrongly costs one cycle of latency on a source that is polled again at the
// next tick, while polling wrongly puts load on somebody's instance precisely
// when this deployment's database is already unhealthy. The outcomes stay
// distinct so a skipped cycle never has to be read as a deliberate pause.
func (m *Manager) sourceDisabled(ctx context.Context, source, orgID string) (skip bool, outcome string) {
	if m.EventSources == nil {
		return false, ""
	}
	kinds, err := m.EventSources.ListDisabledSystem(ctx, orgID)
	if err != nil {
		pollerLog.ErrorContext(ctx, "read event-source policy failed; skipping this cycle rather than polling a source that may be turned off",
			"source", source, "org", orgID, "error", err)
		return true, "policy_unreadable"
	}
	return slices.Contains(kinds, source), "disabled"
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
			githubLog.Warn("read settings for reviewer resolver failed; falling back to github.com host", "org", orgID, "error", err)
		} else {
			// Resolve to the effective host (empty → the deployment default) so the
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

// RestartJira stops and restarts only the Jira polling loop. Runs in both
// modes — startJira reads service creds through the claims-free system door,
// so the restarted ticker polls multi-mode tenants just like local.
func (m *Manager) RestartJira() {
	m.mu.Lock()
	if m.jiraStop != nil {
		close(m.jiraStop)
		m.jiraStop = nil
		jiraLog.Info("tracker stopped")
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
		pollerLog.Warn("load org settings failed; falling back to defaults", "org", orgID, "error", err)
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

// PollGitHubOnce runs one synchronous GitHub poll cycle for a single org,
// bypassing the scheduler entirely — no due gate, no slot reservation, no
// ticker. It is the driver entrypoint for the poll-scale benchmark harness
// (internal/pollbench), which needs to time discrete cycles against a fake
// GitHub server; in-package tests call runGitHubCycleForOrg directly for the
// same reason. Not used by the production loop: server code goes through
// startGitHub's scheduled cycles (or PollSoon to re-due an org) so per-org
// cadence and the anti-stampede scheduling stay intact.
func (m *Manager) PollGitHubOnce(ctx context.Context, orgID string) {
	m.runGitHubCycleForOrg(ctx, orgID)
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
		pollerLog.Warn("list jira rules (org union) failed", "org", orgID, "error", err)
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
		githubLog.Info("tracker stopped")
	}
	if m.jiraStop != nil {
		close(m.jiraStop)
		m.jiraStop = nil
		jiraLog.Info("tracker stopped")
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
		m.runGitHubCycle(stop)

		ticker := time.NewTicker(basePollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.runGitHubCycle(stop)
			case <-stop:
				return
			}
		}
	}()

	githubLog.Info("tracker started (per-org cadence resolved each wake)", "base_tick", basePollInterval)
}

// runGitHubCycle enumerates active orgs and dispatches per-org GitHub polling
// for the orgs whose configured interval has elapsed (others are skipped this
// wake). Each polled org's next slot is reserved before the poll using its
// freshly-read, clamped interval, so an interval change applies from the next
// poll without a restart. Per-org failures are logged and reported via
// OnError but do not abort the remaining orgs in the cycle — a transient
// failure on org A shouldn't starve orgs B..N of polls.
func (m *Manager) runGitHubCycle(stop <-chan struct{}) {
	m.stampGitHubHeartbeat()
	// The trace root for everything a tick does. Fresh per cycle: the
	// ticker that fires this has no caller, so there is no parent to
	// inherit and inventing one would join unrelated cycles into a trace
	// that never ends.
	ctx, span := tracer.Start(context.Background(), "poll.github",
		trace.WithAttributes(telemetry.Source("github")))
	defer span.End()

	now := time.Now()
	orgIDs, err := m.orgs.ListActiveSystem(ctx)
	if err != nil {
		span.SetStatus(codes.Error, "list active orgs")
		githubLog.ErrorContext(ctx, "list active orgs failed", "error", err)
		m.reportError("github", "", err)
		return
	}
	polled := 0
	defer func() { span.SetAttributes(telemetry.Count(polled)) }()

	// Once per cycle, lazily: a wake where nothing is due spends nothing. Best
	// effort like the per-org grant pass — a listing that fails leaves every
	// managed row exactly as it was, so a stale set is the worst outcome and
	// never a reason to skip the polls the cycle came for.
	managedRefreshed := false
	refreshManaged := func() {
		if managedRefreshed || m.RefreshManagedInstallations == nil {
			return
		}
		managedRefreshed = true
		if err := m.RefreshManagedInstallations(ctx); err != nil {
			githubLog.WarnContext(ctx, "managed installation set refresh failed", "error", err)
		}
	}

	for _, orgID := range orgIDs {
		// Stop starting NEW per-org batches the moment this loop is torn down
		// (a lease demotion or RestartAll closes stop) — otherwise a demoted
		// holder keeps polling the rest of the fleet, double-spending the API
		// budget and emitting poll-complete sentinels that would kick a fresh
		// scoring cycle on a pod that no longer holds the brain.
		select {
		case <-stop:
			// Torn down mid-sweep. Without this the span is just a cycle
			// with a low org count, indistinguishable from a quiet one.
			span.SetAttributes(telemetry.Outcome("stopped"))
			return
		default:
		}
		if !m.pollDue("github", orgID, now) {
			continue
		}
		interval := clampPollInterval(m.loadOrgSettings(ctx, orgID).GitHubPollInterval)
		m.schedulePoll("github", orgID, now.Add(interval))
		polled++
		refreshManaged()
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
	// Per-org child of the cycle root — and a root in its own right when
	// PollGitHubOnce calls this directly, which needs no special handling.
	ctx, span := tracer.Start(ctx, "poll.github.org",
		trace.WithAttributes(telemetry.Source("github"), telemetry.OrgID(orgID)))
	defer span.End()

	// Refresh what the App can reach before reading any of it, and BEFORE the
	// tracked-set gate below: an org that tracks nothing is precisely the org
	// where "the App can reach repositories nobody asked for" is largest, so
	// skipping the pass there would blind the finding in the case that needs it
	// most. Best-effort — a mirror TF fails to refresh is one it refreshes next
	// cycle, and never a reason to skip the poll the caller actually came for.
	// The reconcile leaves the previous answer in place on any failure, so a
	// stale mirror is the worst outcome here.
	if m.ReconcileGrant != nil {
		if err := m.ReconcileGrant(ctx, orgID); err != nil {
			githubLog.WarnContext(ctx, "installation mirror reconcile failed", "org", orgID, "error", err)
		}
	}

	repos, err := m.repos.ListTrackedNamesSystem(ctx, orgID)
	if err != nil {
		span.SetStatus(codes.Error, "load configured repos")
		githubLog.ErrorContext(ctx, "load configured repos failed", "org", orgID, "error", err)
		return
	}
	if len(repos) == 0 {
		// Anticipated: an org that tracks nothing. Same posture as the
		// Jira twin's unconfigured skip — an outcome, never an error.
		span.SetAttributes(telemetry.Outcome("no_repos"))
		m.setGitHubCursor(orgID, "") // tracked set emptied — drop any stale cursor
		return
	}

	// TFAC-571: rotate the repo list to start at this org's round-robin
	// cursor (the resume point saved by a prior cycle that got cut short by
	// ErrRateLimited), so a large tracked set doesn't starve the repos at
	// the tail — without this, every cycle restarts from index 0 and repos
	// early in the (stable) list get refreshed every time while repos late
	// in the list may never complete a first refresh.
	repos = rotateFromCursor(repos, m.githubCursor(orgID))

	// Reconcile the GitHub-team mappings against the live team set — the
	// deletion floor's "periodic refresh" trigger, independent of the
	// poll-dispatch path below so it runs whether the org polls via App
	// or PAT.
	m.reconcileGitHubGroups(ctx, orgID, repos)

	isLocal := runmode.Current() == runmode.ModeLocal

	// GitHub access is strictly either/or (TFAC-328): only an ACTIVE App
	// registration engages the per-installation App path. A staged App
	// (registered but active=false during a PAT→App switch) keeps the PAT live,
	// so it polls via the PAT path directly — never poll a staged App. Its
	// tier-1 mint is gated off (resolver skips inactive apps), so entering the
	// App path with one would fail every mint and, pre-TFAC-328, stumble into
	// the PAT fallback. orgHasRegisteredApp is the existing Active-gated probe.
	appActive := m.orgHasRegisteredApp(ctx, orgID)
	if !appActive {
		// No App, or a staged App → the PAT is the live credential.
		m.pollGitHubPAT(ctx, orgID, repos, isLocal)
		return
	}

	// Already reconciled against GET /app/installations at the top of this
	// cycle, in BOTH modes. It used to be reconciled here and only when
	// isLocal, on the reasoning that webhooks can't reach a laptop — which left
	// multi mode converging on deliveries alone, and GitHub never re-sends one
	// it failed to deliver. An org that missed its `created` delivery then read
	// as installed on no accounts forever: degraded below, cycle skipped, repo
	// picker blank, with nothing on a timer to correct it.
	installs, err := m.apps.ListInstallationsForOrgSystem(ctx, orgID)
	if err != nil {
		githubLog.Warn("list installations failed", "org", orgID, "error", err)
	}

	// Active App installed on no accounts. Under XOR there is no PAT to fall
	// back to, so this is a degraded-health condition, not a silent skip:
	// surface it and skip the cycle rather than polling as some other identity.
	if len(installs) == 0 {
		degraded := errors.New("github app is active but installed on no accounts")
		span.SetStatus(codes.Error, "app installed on no accounts")
		githubLog.ErrorContext(ctx, "skipping cycle", "org", orgID, "error", degraded)
		m.reportError("github", orgID, degraded)
		return
	}

	// App path: poll each installation over the intersection of the
	// configured set with that installation's repo grant. Token errors are
	// isolated per installation — a failed mint on A must not starve B/C.
	//
	// anyFunctional tracks whether at least one installation yielded a usable
	// installation token. ListInstallationRepos is installation-token-only, so
	// a successful call proves we hold one. App tokens have no "me", so the
	// resolver is store-backed (host from org_settings). Built once per org
	// cycle — host is stable across the per-installation loop below.
	resolver := m.reviewerResolver(ctx, orgID, "", nil)

	// unresolvedOwners collects the account logins of installations whose
	// client/grant couldn't be resolved this cycle (ClientFor or
	// ListInstallationRepos failed) — TFAC-571 follow-up. Those repos were
	// never reached at all, the same as a rate-limited repo that never got
	// dispatched, so the cursor computed below must not treat this cycle as
	// a full wrap just because a LATER installation happened to succeed.
	unresolvedOwners := make(map[string]bool)
	var rateLimitResumeFrom string
	var rateLimitErr *ghclient.ErrRateLimited

	covered := make(map[string]bool, len(repos))
	anyFunctional := false
	for _, inst := range installs {
		client, cerr := m.resolver.ClientFor(ctx, orgID, inst.AccountLogin)
		if cerr != nil {
			githubLog.Error("resolve client for installation failed", "org", orgID, "installation", inst.InstallationID, "account", inst.AccountLogin, "error", cerr)
			m.reportError("github", orgID, cerr)
			unresolvedOwners[strings.ToLower(inst.AccountLogin)] = true
			continue
		}
		grant, gerr := client.ListInstallationRepos(ctx)
		if gerr != nil {
			githubLog.Error("list installation repos failed", "org", orgID, "account", inst.AccountLogin, "error", gerr)
			m.reportError("github", orgID, gerr)
			unresolvedOwners[strings.ToLower(inst.AccountLogin)] = true
			continue
		}
		anyFunctional = true
		// Rename detection, before the intersection rather than after it: a
		// renamed repository is precisely the one the intersection can no
		// longer see, because TF still spells it the old way and GitHub
		// answers with the new one. The grant carries every repo's id
		// alongside its current name, so the condition — same id, different
		// name — is readable here with no request of its own, and over the
		// WHOLE grant rather than the tracked subset.
		if renamed := m.applyRepoRenames(ctx, orgID, grant); renamed > 0 {
			repos = m.reloadConfiguredRepos(ctx, orgID, repos)
		}
		scoped := intersectConfigured(repos, grant, covered)
		if len(scoped) == 0 {
			continue
		}
		// The grant response carries GitHub's repository id for every repo in
		// it, so this is where a tracked repo's id is cheapest to learn: no
		// request of its own, and it covers the whole tracked set rather than
		// whatever the profiler reaches before its TTL. Only NULLs are filled,
		// so it is a no-op on every cycle after the first. Best-effort — an
		// id TF fails to record is one it records next cycle, and never a
		// reason to skip the poll the caller actually came for.
		m.recordRepoIDs(ctx, orgID, scoped, grant)
		// App tokens have no "me" — drop the username axis for discovery
		// (Sharp edge 2). Predicates still match per-PR fields downstream.
		_, resumeFrom, rerr := m.trackerForOrg(orgID).RefreshGitHub(ctx, client, "", scoped, resolver)
		if rerr != nil {
			githubLog.Error("tracker error", "org", orgID, "installation", inst.AccountLogin, "error", rerr)
			m.reportError("github", orgID, rerr)
		} else {
			m.stampGitHubSuccess(orgID)
		}
		var rl *ghclient.ErrRateLimited
		if errors.As(rerr, &rl) {
			// TFAC-571: budget's known exhausted for this credential — stop
			// cleanly rather than trying the remaining installations against
			// the same exhausted client budget. Cursor placement is decided
			// once, below, alongside any unresolved-installation gap.
			rateLimitResumeFrom, rateLimitErr = resumeFrom, rl
			break
		}
	}

	// TFAC-571: decide the cycle's actual resume point once, after every
	// installation this cycle could reach has run. A rate-limited fan-out
	// wins (it's the more specific signal — RefreshGitHub already pinned it
	// to the exact repo that needs a retry). Otherwise, if any installation
	// couldn't even be resolved this cycle, the cursor must not reset to ""
	// (implying a full wrap) just because a later installation succeeded —
	// resume at the earliest currently-configured repo that installation
	// would own (matched by account-login prefix, same as
	// reconcileGitHubGroups) and isn't already covered by a DIFFERENT,
	// successful installation. A repo genuinely outside every installation's
	// grant (the "not in any app installation grant" warning below) is a
	// permanent gap, not a transient one, so it's excluded here — only
	// unresolved-owner repos get cursor priority.
	resumeFrom := rateLimitResumeFrom
	if resumeFrom == "" && len(unresolvedOwners) > 0 {
		for _, r := range repos {
			if covered[r] {
				continue
			}
			owner, _, _ := strings.Cut(r, "/")
			if unresolvedOwners[strings.ToLower(owner)] {
				resumeFrom = r
				break
			}
		}
	}
	m.recordGitHubCursor(orgID, resumeFrom, rateLimitErr)

	// No installation produced a usable installation token (every mint/list
	// failed). Under XOR there is no PAT behind these failures to fall back to
	// — the previous cross-mode fallback would silently do nothing (no PAT) or,
	// worse, act as a leaked PAT if one ever crept back in. Surface degraded
	// health and skip rather than masking it.
	if !anyFunctional {
		degraded := errors.New("github app is active but no installation produced a usable token")
		span.SetStatus(codes.Error, "no usable installation token")
		githubLog.ErrorContext(ctx, "skipping cycle", "org", orgID, "error", degraded)
		m.reportError("github", orgID, degraded)
		return
	}

	// Configured repos that no functional installation grants — the App isn't
	// installed on them. Log once so the gap is visible without being an error.
	for _, r := range repos {
		if !covered[r] {
			githubLog.Warn("configured repo not in any app installation grant; skipping", "org", orgID, "repo", r)
		}
	}
}

// reconcileGitHubGroups is the GitHub-team-deletion reconcile floor — the
// "periodic refresh" trigger the reconcile lifecycle describes, run every
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
				githubGroupsLog.Warn("resolve client for owner failed; skipping reconcile", "org", orgID, "owner", owner, "error", err)
			}
			continue
		}
		teams, err := client.ListOrgTeams(ctx, owner)
		if err != nil {
			githubGroupsLog.Warn("list teams for owner failed; skipping reconcile", "org", orgID, "owner", owner, "error", err)
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
			githubGroupsLog.Warn("reconcile prune failed", "org", orgID, "owner", owner, "error", err)
		} else if n > 0 {
			githubGroupsLog.Info("pruned stale mappings for deleted github teams", "org", orgID, "owner", owner, "pruned", n)
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
	// The PAT path is the whole body of one branch of runGitHubCycleForOrg,
	// so it records onto that caller's poll.github.org span rather than
	// opening a child that would only ever duplicate it. A no-op span when
	// there is no caller (tests), which needs no guard.
	span := trace.SpanFromContext(ctx)

	// No target account, and none to give: the credential this path resolves is
	// the org PAT, which is account-agnostic, and the cycle it feeds polls the
	// whole configured set across every tracked owner. Should the org's App turn
	// active between the caller's gate and this call, resolution answers as the
	// App does — its one installation, or an ambiguity error past that, never a
	// guessed account.
	client, err := m.resolver.ClientFor(ctx, orgID, "")
	if err != nil {
		if errors.Is(err, ghclient.ErrNoGitHubCredentials) {
			span.SetAttributes(telemetry.Outcome("unconfigured"))
			return // not configured for GitHub — silent skip
		}
		span.SetStatus(codes.Error, "resolve pat client")
		githubLog.ErrorContext(ctx, "resolve pat client failed", "org", orgID, "error", err)
		m.reportError("github", orgID, err)
		return
	}

	var username string
	var userTeams []string
	if isLocal {
		// Identity is host-scoped: resolve the org's GitHub host
		// from org_settings, then read the local user's login for that
		// (user, host) pair. Boot-time goroutine with no JWT claims → the
		// `...System` admin-pool variants.
		orgSet, serr := m.orgs.GetSettingsSystem(ctx, orgID)
		if serr != nil {
			span.SetStatus(codes.Error, "read org settings")
			githubLog.ErrorContext(ctx, "read org settings failed", "org", orgID, "error", serr)
			return
		}
		username, err = m.users.GetGitHubLoginSystem(ctx, runmode.LocalDefaultUserID, orgSet.GitHubBaseURL)
		if err != nil {
			span.SetStatus(codes.Error, "read github identity")
			githubLog.ErrorContext(ctx, "read github identity failed", "org", orgID, "error", err)
			return
		}
		if teams, terr := client.ListMyTeams(ctx); terr != nil {
			githubLog.Warn("list teams failed; team-based review requests will be missed this cycle", "org", orgID, "error", terr)
		} else {
			userTeams = teams
		}
	}

	resolver := m.reviewerResolver(ctx, orgID, username, userTeams)
	_, resumeFrom, rerr := m.trackerForOrg(orgID).RefreshGitHub(ctx, client, username, repos, resolver)
	if rerr != nil {
		span.SetStatus(codes.Error, "refresh")
		githubLog.ErrorContext(ctx, "tracker error", "org", orgID, "error", rerr)
		m.reportError("github", orgID, rerr)
	} else {
		m.stampGitHubSuccess(orgID)
	}
	var rl *ghclient.ErrRateLimited
	errors.As(rerr, &rl)
	m.recordGitHubCursor(orgID, resumeFrom, rl)
}

// orgHasRegisteredApp reports whether the org's live GitHub credential is a
// GitHub App — its own, or the deployment's shared one. It is the poll cycle's
// App-vs-PAT dispatch gate (and, downstream of that, what keeps a PAT org from
// paying a no-op installation backfill round-trip each cycle).
//
// The first question is the org's credential CLASS, not whether an
// org_github_apps row exists: "no row" is the shape of a PAT org and would
// equally be the shape of an org riding a deployment-level shared App, whose
// installations this probe would then never fan out over.
//
// False does NOT mean "poll with a PAT" — it means "don't fan out per
// installation". The credential itself is chosen one layer down, by
// resolver.ClientFor in pollGitHubPAT, which reads the class again and REFUSES
// an unknown or unreadable one (ErrUnknownCredentialClass) instead of falling
// to a PAT. That error isn't ErrNoGitHubCredentials, so the cycle reports
// degraded health and polls nothing. So the error returns below are not a
// fail-open toward PAT, even though a false is what they produce: no path from
// here can poll an org under a credential system it isn't in. They are still
// logged, because the cycle they cost is worth seeing.
func (m *Manager) orgHasRegisteredApp(ctx context.Context, orgID string) bool {
	if m.apps == nil {
		return false
	}
	if m.orgs == nil {
		// Wiring fault, not an org state: NewManager always supplies the store.
		// Loud, because the consequence is a cycle that polls as a PAT.
		githubLog.Error("orgs store not wired; cannot resolve credential class, polling as PAT", "org", orgID)
		return false
	}
	set, err := m.orgs.GetSettingsSystem(ctx, orgID)
	if err != nil {
		githubLog.Warn("read org settings failed; polling as PAT this cycle", "org", orgID, "error", err)
		return false
	}
	switch set.GitHubCredentialClass {
	case domain.GitHubCredentialClassPAT:
		return false
	case domain.GitHubCredentialClassBYOApp:
		app, err := m.apps.GetForOrgSystem(ctx, orgID)
		if err != nil {
			githubLog.Warn("read app registration failed", "org", orgID, "error", err)
			return false
		}
		// A staged App (active=false, mid PAT→App switch) is a BYO-App org whose
		// live credential is still the PAT — the class and the Active bit
		// answering their two different questions.
		return app != nil && app.Active
	case domain.GitHubCredentialClassManagedApp:
		// The class is the whole answer here, and there is no second read to
		// make: an org riding the deployment App holds no registration row, so
		// there is no Active bit that could stage it behind a PAT — the staged
		// window the BYO arm reads exists only for an org that owns its key.
		//
		// Whether the deployment App is usable is not this gate's question
		// either. Fanning out per installation is what the class decides; the
		// credential is chosen one layer down by the resolver, which refuses a
		// managed org outright when the shared App is unconfigured or fails its
		// preflight. The cycle then reports degraded and polls nothing, which is
		// the correct outcome and not one a false here could produce — false
		// would poll as a PAT the workspace never chose.
		return true
	default:
		githubLog.Warn("unknown github credential class; polling as PAT this cycle",
			"org", orgID, "class", set.GitHubCredentialClass)
		return false
	}
}

// intersectConfigured returns the configured repos reachable through one
// installation's grant, marking each in covered so the caller can report
// configured repos that no installation grants. Matching is case-insensitive
// on the "owner/repo" slug (GitHub logins are case-insensitive).
// recordRepoIDs writes GitHub's repository id onto the tracked repos in
// scoped, taking the ids from the grant listing this cycle already fetched.
//
// It is the poller's half of repository identity: a slug is what a repository
// is called and the id is what it is, and rename detection can only key on the
// half that a rename does not move. Filling ids only when a repository is
// profiled would leave every repo undetectable for the length of the profile
// TTL; filling them here closes that to one poll cycle at no request cost.
//
// Scoped to the intersection rather than the whole grant on purpose: a repo
// nobody tracks has no row, so writing it would either no-op or (worse) invite
// a create path in the poller.
func (m *Manager) recordRepoIDs(ctx context.Context, orgID string, scoped []string, grant []ghclient.UserRepo) {
	if m.repos == nil {
		return
	}
	byName := make(map[string]string, len(grant))
	for _, g := range grant {
		if id := g.ExternalID(); id != "" {
			byName[strings.ToLower(g.FullName)] = id
		}
	}
	refs := make([]domain.RepoRef, 0, len(scoped))
	for _, slug := range scoped {
		id := byName[strings.ToLower(slug)]
		if id == "" {
			continue
		}
		owner, repo, ok := strings.Cut(slug, "/")
		if !ok {
			continue
		}
		refs = append(refs, domain.RepoRef{
			Source: domain.RepoSourceGitHub, Owner: owner, Repo: repo, ExternalID: id,
		})
	}
	if len(refs) == 0 {
		return
	}
	filled, err := m.repos.FillMissingExternalIDsSystem(ctx, orgID, refs)
	if err != nil {
		githubLog.WarnContext(ctx, "record repository ids failed", "org", orgID, "error", err)
		return
	}
	if filled > 0 {
		githubLog.InfoContext(ctx, "recorded repository ids", "org", orgID, "filled", filled)
	}
}

// applyRepoRenames reconciles the tracked repositories' slugs against the
// names GitHub used in this grant listing, returning how many moved.
//
// Scoped to the whole grant, unlike recordRepoIDs above: filling an id needs a
// row to fill and so follows the tracked intersection, but detecting a rename
// needs the observation the intersection has already dropped. A grant entry
// for a repository TF does not track matches no stored identity and costs one
// map lookup.
func (m *Manager) applyRepoRenames(ctx context.Context, orgID string, grant []ghclient.UserRepo) int {
	if m.repos == nil {
		return 0
	}
	observed := make([]domain.RepoRef, 0, len(grant))
	for _, g := range grant {
		id := g.ExternalID()
		if id == "" {
			continue
		}
		owner, repo, ok := strings.Cut(g.FullName, "/")
		if !ok {
			continue
		}
		observed = append(observed, domain.RepoRef{
			Source: domain.RepoSourceGitHub, Owner: owner, Repo: repo, ExternalID: id,
		})
	}
	return reporename.Apply(ctx, m.repos, m.resolver, githubLog, orgID, observed)
}

// reloadConfiguredRepos re-reads the tracked set after a rename moved a slug
// inside it, so this cycle's intersection matches the repository under its new
// name instead of skipping it until the next one. Re-rotated to the same
// cursor the first read was, so the round-robin position survives.
func (m *Manager) reloadConfiguredRepos(ctx context.Context, orgID string, current []string) []string {
	refreshed, err := m.repos.ListTrackedNamesSystem(ctx, orgID)
	if err != nil {
		githubLog.WarnContext(ctx, "re-read configured repos after a rename failed", "org", orgID, "error", err)
		return current
	}
	return rotateFromCursor(refreshed, m.githubCursor(orgID))
}

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

// githubCursor returns the org's saved round-robin repo cursor (TFAC-571) —
// the full-name ("owner/repo") of the next repo to refresh, or "" if none is
// set (never polled, or the last cycle fully wrapped the list).
func (m *Manager) githubCursor(orgID string) string {
	m.cursorMu.Lock()
	defer m.cursorMu.Unlock()
	return m.ghCursor[orgID]
}

// setGitHubCursor records the org's round-robin repo cursor. An empty repo
// clears it (full wrap completed, or the tracked set is now empty) rather
// than storing a meaningless "resume at the head" entry.
func (m *Manager) setGitHubCursor(orgID, repo string) {
	m.cursorMu.Lock()
	defer m.cursorMu.Unlock()
	if repo == "" {
		delete(m.ghCursor, orgID)
		return
	}
	if m.ghCursor == nil {
		m.ghCursor = make(map[string]string)
	}
	m.ghCursor[orgID] = repo
}

// recordGitHubCursor applies one GitHub cycle's outcome to the org's
// round-robin cursor. resumeFrom == "" means the cycle fully wrapped the
// repo list — the cursor resets to the head for next cycle. A non-empty
// resumeFrom means it didn't: either ErrRateLimited cut a fan-out short
// partway through the list (rl != nil — see Tracker.RefreshGitHub), or, in
// the multi-installation App path, an installation's client/grant couldn't
// even be resolved this cycle so its repos were never reached (rl == nil —
// see the unresolvedOwners handling in runGitHubCycleForOrg). Either way the
// cursor is saved at resumeFrom so next cycle prioritizes it. Only the
// rate-limited case additionally logs at INFO and pulls the org's next
// scheduled poll in to rl.ResumeAt, so the cursor gets picked back up as
// soon as the budget frees rather than waiting out a full normal poll
// interval (which may be much longer, or — if shorter — would just hit the
// same exhaustion again); the unresolved-installation case has no known
// resume time, so it just waits for the org's ordinary cadence.
func (m *Manager) recordGitHubCursor(orgID, resumeFrom string, rl *ghclient.ErrRateLimited) {
	m.setGitHubCursor(orgID, resumeFrom)
	if rl == nil {
		return
	}
	githubLog.Info("github poll cycle interrupted by rate limit; resuming repo cursor next cycle",
		"org", orgID, "resume_repo", resumeFrom, "resume_at", rl.ResumeAt)
	m.schedulePoll("github", orgID, rl.ResumeAt)
}

// rotateFromCursor returns repos rotated so iteration starts at cursor's
// position and wraps around — TFAC-571's round-robin fairness mechanism, so
// a large tracked set doesn't starve the repos at the tail of the list when
// a cycle can't finish (rate budget exhausted, restart, GHES flake).
//
// Churn-tolerant by construction: repos are matched by full name, not index,
// so an insertion/removal elsewhere in the tracked set doesn't shift the
// resume point. If cursor is empty or its repo is no longer present (removed
// from the tracked set between cycles), rotation is a no-op — resume at the
// head rather than panicking or skipping the list.
func rotateFromCursor(repos []string, cursor string) []string {
	if cursor == "" {
		return repos
	}
	idx := -1
	for i, r := range repos {
		if r == cursor {
			idx = i
			break
		}
	}
	if idx <= 0 {
		return repos
	}
	out := make([]string, 0, len(repos))
	out = append(out, repos[idx:]...)
	out = append(out, repos[:idx]...)
	return out
}

// startJira launches the Jira tracking loop: ONE goroutine that wakes every
// basePollInterval; runJiraCycle then polls the active orgs whose interval
// has elapsed, resolving each tenant's Jira creds + project rules + base URL
// + interval inside the loop. Orgs without a connected Jira integration (no
// PAT, no URL, no rules) are silently skipped each cycle, so adding/removing
// a tenant's Jira config doesn't need a poller restart.
//
// Runs in both modes, mirroring startGitHub. The per-org loop reads every
// secret through the claims-free system door (integrations.LoadSystem,
// resolver.ForSystem), so it works without a request JWT: in multi mode those
// reads run on the system/admin pool (trusting the passed orgID, no
// current_org_id() check); in local mode they forward to the keychain. A
// configured multi-mode tenant therefore mints Jira tasks in the background
// without waiting for an interactive request.
func (m *Manager) startJira() {
	stop := make(chan struct{})
	m.mu.Lock()
	m.jiraStop = stop
	m.mu.Unlock()

	go func() {
		// Initial poll. Same cold-boot-vs-cadence semantics as startGitHub.
		m.runJiraCycle(stop)

		ticker := time.NewTicker(basePollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.runJiraCycle(stop)
			case <-stop:
				return
			}
		}
	}()

	jiraLog.Info("tracker started (per-org cadence resolved each wake)", "base_tick", basePollInterval)
}

// runJiraCycle enumerates active orgs and dispatches a per-org RefreshJira for
// the orgs whose configured interval has elapsed (others are skipped this
// wake). Each org's creds + rules + base URL + interval are resolved inside
// the loop so two tenants with different Jira PATs / project configurations
// don't share state; the next slot is reserved from the freshly-read interval
// once settings load. Orgs not configured for Jira are skipped silently;
// per-org failures are logged + reported via OnError but do not abort the
// remaining orgs in the cycle.
func (m *Manager) runJiraCycle(stop <-chan struct{}) {
	m.stampJiraHeartbeat()
	// Fresh trace root per tick, for the reasons in runGitHubCycle.
	ctx, span := tracer.Start(context.Background(), "poll.jira",
		trace.WithAttributes(telemetry.Source("jira")))
	defer span.End()

	now := time.Now()
	orgIDs, err := m.orgs.ListActiveSystem(ctx)
	if err != nil {
		span.SetStatus(codes.Error, "list active orgs")
		jiraLog.ErrorContext(ctx, "list active orgs failed", "error", err)
		m.reportError("jira", "", err)
		return
	}
	polled := 0
	defer func() { span.SetAttributes(telemetry.Count(polled)) }()
	// ForSystem is the canonical constructor for the org/bot service
	// client. The poller is read-only (discovery), so it's bot-attributed by
	// design; routing through the resolver keeps system-client construction in
	// one place alongside the per-user write path.
	sysResolver := jiraclient.NewResolver(m.secrets, m.orgs)
	for _, orgID := range orgIDs {
		// Stop starting NEW per-org batches once this loop is torn down (a
		// lease demotion or RestartJira closes stop) — see runGitHubCycle.
		select {
		case <-stop:
			// See runGitHubCycle.
			span.SetAttributes(telemetry.Outcome("stopped"))
			return
		default:
		}
		if !m.pollDue("jira", orgID, now) {
			continue
		}
		polled++
		m.runJiraCycleForOrg(ctx, sysResolver, orgID, now)
	}
	m.prunePoll("jira", orgIDs)
}

// runJiraCycleForOrg polls one org: settings → next-slot reservation →
// credentials → rules → a single RefreshJira over the merged project set.
// Split out of runJiraCycle's loop to mirror runGitHubCycleForOrg, so both
// sources have one per-org unit to hang a span (and a future retry policy)
// on. Every failure here is terminal for THIS org only — the caller moves
// on to the next one.
func (m *Manager) runJiraCycleForOrg(ctx context.Context, sysResolver jiraclient.Resolver, orgID string, now time.Time) {
	ctx, span := tracer.Start(ctx, "poll.jira.org",
		trace.WithAttributes(telemetry.Source("jira"), telemetry.OrgID(orgID)))
	defer span.End()

	orgSet, oerr := m.orgs.GetSettingsSystem(ctx, orgID)
	if oerr != nil {
		span.SetStatus(codes.Error, "load settings")
		jiraLog.ErrorContext(ctx, "load settings failed", "org", orgID, "error", oerr)
		m.reportError("jira", orgID, oerr)
		return // leave unscheduled → retry next base tick (interval unknown)
	}
	// Reserve the next slot from the freshly-read interval BEFORE loading
	// creds/rules below. The asymmetry is deliberate: a settings-read
	// failure (above) leaves the org unscheduled so it retries at the next
	// base tick (we don't know its interval); a creds/rules failure (below)
	// keeps this slot, so a likely-persistent auth failure backs off to the
	// org's own cadence instead of hammering every base tick.
	m.schedulePoll("jira", orgID, now.Add(clampPollInterval(orgSet.JiraPollInterval)))
	// An org admin turned Jira off, so this org's cycle makes no Jira calls at
	// all — the load an operator is watching for goes to nothing. Checked after
	// the slot reservation above so a turned-off org falls back to its own
	// cadence rather than being re-examined every base tick, and before the
	// credential load because a source nobody may call has no reason to touch
	// the secret store. Same posture as the unconfigured skip below: an
	// anticipated outcome, never an error.
	if skip, outcome := m.sourceDisabled(ctx, "jira", orgID); skip {
		span.SetAttributes(telemetry.Outcome(outcome))
		return
	}
	creds, lerr := integrations.LoadSystem(ctx, m.secrets, orgID)
	if lerr != nil {
		span.SetStatus(codes.Error, "load creds")
		jiraLog.ErrorContext(ctx, "load creds failed", "org", orgID, "error", lerr)
		m.reportError("jira", orgID, lerr)
		return
	}
	rules := m.loadJiraRules(ctx, orgID)
	// "Configured" is decided by the auth-method marker (via JiraSystemConfig),
	// not key presence: a configured org has a URL plus the scheme's secret
	// matching its jira_auth_method (DC PAT, or Cloud email + token). Reading
	// the marker keeps this gate in lockstep with the client ForSystem builds
	// below, so the two can't disagree. Skip silently when unconfigured or
	// rules are missing — adding/removing a tenant's Jira config doesn't need
	// a poller restart this way.
	if _, ok := integrations.JiraSystemConfig(creds); !ok || len(rules) == 0 {
		// An anticipated skip, not a failure: an org with no Jira config
		// is the common case, so it gets an outcome rather than an error
		// status.
		span.SetAttributes(telemetry.Outcome("unconfigured"))
		return
	}
	// Only ARMED projects can be polled: the discovery JQL is built from
	// pickup and done members, so a watched-but-unmapped project has nothing
	// to ask about. That already fell out of the merge below producing empty
	// member sets, but "the cycle ran and asked nothing" and "the cycle
	// declined to run" are different facts and only one of them is true — so
	// the skip is stated here rather than left to emerge downstream.
	projects := toTrackerJiraRules(rules)
	if len(projects) == 0 {
		span.SetAttributes(telemetry.Outcome("no_armed_projects"))
		return
	}
	baseURL := orgSet.JiraBaseURL
	if baseURL == "" {
		baseURL = creds.JiraURL
	}
	// creds load above gates configuration + supplies the baseURL fallback;
	// ForSystem (reading the same secrets, routed Cloud-vs-DC by the stored
	// auth-method marker) builds the authenticated client. The gate
	// guarantees a credential is present, so ForSystem won't report
	// ErrNoJiraSystemCredential here — handle errors defensively.
	client, cerr := sysResolver.ForSystem(ctx, orgID)
	if cerr != nil {
		span.SetStatus(codes.Error, "resolve system client")
		jiraLog.ErrorContext(ctx, "resolve system client failed", "org", orgID, "error", cerr)
		m.reportError("jira", orgID, cerr)
		return
	}
	if _, err := m.trackerForOrg(orgID).RefreshJira(ctx, client, baseURL, projects); err != nil {
		span.SetStatus(codes.Error, "refresh")
		jiraLog.ErrorContext(ctx, "tracker error", "org", orgID, "error", err)
		m.reportError("jira", orgID, err)
		return
	}
	m.stampJiraSuccess(orgID)
}

// toTrackerJiraRules collapses the org-wide rule union into the
// tracker-local per-project view — one entry per distinct project_key, so
// a project two teams both track is polled once (entities are org-shared:
// one row per Jira issue regardless of how many teams track its project).
//
// Unarmed rows are dropped first. A team may WATCH a project before mapping
// its workflow's statuses, and such a row carries no members to build a JQL
// from; including it would put a project key in the merged view that
// contributes nothing to either query. So a project reaches the tracker only
// through a team that has actually armed it — and one team's unmapped row
// cannot dilute another team's armed one, because the merge below unions
// members and an unarmed row would contribute none.
//
// PickupMembers / DoneMembers are Jira *workflow statuses* ("To Do",
// "Backlog", "Done" — each an id plus the name captured with it), NOT people
// or teams: they drive the discovery JQL, which asks by id so a rename in Jira
// cannot silently stop matching. When several teams track the same project_key
// with divergent status sets, those sets are MERGED (set-union, first-seen order preserved): the most-permissive
// interpretation for discovery + terminal detection, so no team's
// pickup-able issue is missed and any team's notion of "done" closes the
// shared entity. Team-specific scoping lives downstream in the router
// gate, not in discovery. Kept narrow on purpose — the tracker only needs
// pickup/done statuses, not the canonicals. Input arrives ordered by
// (project_key, team_id), so the merged output is deterministic.
func toTrackerJiraRules(rules []domain.JiraProjectStatusRules) tracker.JiraRules {
	type merged struct {
		pickup, done         []domain.JiraStatusRef
		pickupSeen, doneSeen map[string]bool
	}
	byKey := map[string]*merged{}
	order := make([]string, 0, len(rules))
	addUnique := func(dst []domain.JiraStatusRef, seen map[string]bool, src []domain.JiraStatusRef) []domain.JiraStatusRef {
		for _, ref := range src {
			if key := domain.JiraStatusDedupKey(ref); !seen[key] {
				seen[key] = true
				dst = append(dst, ref)
			}
		}
		return dst
	}
	for _, p := range rules {
		if !p.Armed() {
			continue
		}
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
