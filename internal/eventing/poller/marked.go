package poller

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/tracker"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// The narrowed pass: a GitHub webhook delivery leaves refresh marks naming
// what changed (db.GitHubRefreshMarkStore), and the narrowed pass refreshes
// what an org's marks name, in the marked repositories only, seconds after the
// delivery rather than at the org's next scheduled cycle: the pull requests
// and branches a mark names (tracker.RefreshMarkedGitHub), and the Dependabot
// alerts of a repository a mark names them in
// (tracker.RefreshVulnerablePackages). The scheduled cycle honors the marks
// too, so a mark no narrowed pass reached is refreshed there.

// hintDebounce is how long after a receiver's nudge the narrowed pass runs, so
// the deliveries one change sends (a push's pull_request, push and check_suite
// events, say) are read by one pass.
const hintDebounce = 2 * time.Second

// markReadLimit is how many of an org's marks one pass reads, oldest first.
// The rest wait for the next pass; the base tick's sweep starts one within
// basePollInterval while any are left.
const markReadLimit = 500

// orgPass is one org's GitHub pass state. mu is held for the length of every
// pass over the org, scheduled or narrowed, so only one runs at a time; the
// flags are guarded by Manager.passMu.
type orgPass struct {
	mu sync.Mutex
	// pending is a narrowed pass scheduled that has not read the org's marks
	// yet, which every further nudge collapses into.
	pending bool
	// yield is the scheduled cycle finding the org mid-pass: no narrowed pass
	// starts until the scheduled cycle has run, so a busy org's nudges cannot
	// keep it waiting past the next base tick.
	yield bool
}

func (m *Manager) orgPassFor(orgID string) *orgPass {
	m.passMu.Lock()
	defer m.passMu.Unlock()
	if m.orgPasses == nil {
		m.orgPasses = make(map[string]*orgPass)
	}
	p := m.orgPasses[orgID]
	if p == nil {
		p = &orgPass{}
		m.orgPasses[orgID] = p
	}
	return p
}

// tryBeginScheduled starts the org's scheduled cycle unless a narrowed pass is
// running over it, in which case the scheduled loop moves on to the next org
// rather than wait: different orgs never wait on each other. The org stays
// due, and no narrowed pass starts before the next wake runs it.
func (m *Manager) tryBeginScheduled(orgID string) (end func(), ok bool) {
	p := m.orgPassFor(orgID)
	locked := p.mu.TryLock()
	m.passMu.Lock()
	p.yield = !locked
	m.passMu.Unlock()
	if !locked {
		return nil, false
	}
	return p.mu.Unlock, true
}

// beginScheduled starts the org's scheduled cycle once no narrowed pass is
// running over it.
func (m *Manager) beginScheduled(orgID string) (end func()) {
	p := m.orgPassFor(orgID)
	p.mu.Lock()
	m.passMu.Lock()
	p.yield = false
	m.passMu.Unlock()
	return p.mu.Unlock
}

// HintGitHub asks for a narrowed pass over orgID's refresh marks, about
// hintDebounce from now. It is the receivers' nudge, relayed to the brain:
// nudges that land before the pass reads the marks collapse into it, and one
// that lands while it runs schedules the next. A nudge while the GitHub loop is
// not running (a standby, or a stopped poller) does nothing: the marks are
// stored, and the loop's sweep or its scheduled cycle reads them once it runs.
func (m *Manager) HintGitHub(orgID string) {
	m.scheduleMarkedPass(orgID, hintDebounce)
}

func (m *Manager) scheduleMarkedPass(orgID string, after time.Duration) {
	if m.RefreshMarks == nil || orgID == "" || !m.githubRunning() {
		return
	}
	p := m.orgPassFor(orgID)
	m.passMu.Lock()
	if p.pending {
		m.passMu.Unlock()
		return
	}
	p.pending = true
	m.passMu.Unlock()
	time.AfterFunc(after, func() { m.runMarkedPass(orgID) })
}

// runMarkedPass runs one scheduled narrowed pass, after the org's running
// pass, if any, ends, while the GitHub loop is running. The loop is read when
// the pass is due rather than when it was scheduled, so a nudge collapsed into
// a pass scheduled before a restart still runs on the restarted loop, and one
// scheduled before a stop does not run.
func (m *Manager) runMarkedPass(orgID string) {
	p := m.orgPassFor(orgID)
	running := m.githubRunning()
	if running {
		p.mu.Lock()
		defer p.mu.Unlock()
	}
	m.passMu.Lock()
	p.pending = false
	yield := p.yield
	m.passMu.Unlock()
	if !running || yield || !m.githubRunning() {
		return
	}
	m.refreshMarkedGitHubForOrg(context.Background(), orgID)
}

// githubRunning reports whether the GitHub loop is running (startGitHub, until
// stopAll).
func (m *Manager) githubRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ghStop != nil
}

// sweepMarks starts a narrowed pass for every active org with pending marks:
// the base tick's backstop for a nudge the relay lost, so a mark waits at most
// basePollInterval for its pass. It runs on its own ticker beside the
// scheduled loop, whose wake can be held up by a long cycle over many orgs.
func (m *Manager) sweepMarks(ctx context.Context) {
	if m.RefreshMarks == nil {
		return
	}
	marked, err := m.RefreshMarks.ListMarkedOrgsSystem(ctx)
	if err != nil {
		githubLog.WarnContext(ctx, "list orgs with refresh marks failed; their marks wait for the next sweep or their scheduled cycle", "error", err)
		return
	}
	if len(marked) == 0 {
		return
	}
	active, err := m.orgs.ListActiveSystem(ctx)
	if err != nil {
		githubLog.WarnContext(ctx, "list active orgs for the refresh-mark sweep failed", "error", err)
		return
	}
	isActive := make(map[string]bool, len(active))
	for _, id := range active {
		isActive[id] = true
	}
	for _, orgID := range marked {
		if isActive[orgID] {
			m.scheduleMarkedPass(orgID, 0)
		}
	}
}

// RefreshMarkedGitHubOnce runs one narrowed pass for orgID synchronously, once
// no other pass is running over it, bypassing the debounce. It is the
// poll-scale benchmark's entry point, as PollGitHubOnce is for the scheduled
// cycle; the production loop goes through HintGitHub and the sweep.
func (m *Manager) RefreshMarkedGitHubOnce(ctx context.Context, orgID string) {
	p := m.orgPassFor(orgID)
	p.mu.Lock()
	defer p.mu.Unlock()
	m.refreshMarkedGitHubForOrg(ctx, orgID)
}

// refreshMarkedGitHubForOrg is the narrowed pass over orgID's refresh marks.
// It makes none of the scheduled cycle's org-wide reads: no installation,
// grant or team listing and no reconcile. Each marked repository is refreshed
// under the installation whose grant held it at the org's last scheduled
// cycle, or under the org's personal token, and a mark that cannot be routed
// that way waits for the scheduled cycle, which reads the grants afresh. It
// moves neither the round-robin cursor nor the connection state, reports no
// completed poll, and runs nothing while the org waits out a rate limit.
// After a pass that emitted events, it kicks the org's scorer, which the
// completed-poll notice kicks after a scheduled cycle.
func (m *Manager) refreshMarkedGitHubForOrg(ctx context.Context, orgID string) {
	ctx, span := tracer.Start(ctx, "poll.github.marked",
		trace.WithAttributes(telemetry.Source("github"), telemetry.OrgID(orgID)))
	defer span.End()
	if m.RefreshMarks == nil {
		return
	}
	if m.pollHeld("github", orgID, time.Now()) {
		span.SetAttributes(telemetry.Outcome("rate_limited"))
		return
	}
	orgSet, err := m.orgs.GetSettingsSystem(ctx, orgID)
	if err != nil {
		span.SetStatus(codes.Error, "read org settings")
		githubLog.ErrorContext(ctx, "read org settings for the narrowed pass failed; its marks wait", "org", orgID, "error", err)
		return
	}
	scope := domain.EntityScope("github", orgSet)
	ledger := m.readMarks(ctx, orgID, scope)
	if ledger == nil {
		span.SetAttributes(telemetry.Outcome("no_marks"))
		return
	}
	defer ledger.commit(ctx)
	repos := ledger.markedRepos()
	if len(repos) == 0 {
		span.SetAttributes(telemetry.Outcome("untracked"))
		return
	}
	alerts := m.markedAlertListings(ctx, orgID, scope, ledger, repos)
	repos = slices.DeleteFunc(repos, func(r domain.Repository) bool {
		_, listed := alerts[r.ID]
		return !listed && !ledger.pullRequestsMarked(r.ID)
	})
	if len(repos) == 0 {
		span.SetAttributes(telemetry.Outcome("alerts_unlisted"))
		return
	}
	// A host that is not answering costs the first request its retries and
	// the rest one attempt each, as in the scheduled cycle.
	ctx = upstream.WithFailFast(ctx)

	events := 0
	if !m.orgHasRegisteredApp(ctx, orgID) {
		client, self, resolver, ok := m.markedPATPass(ctx, orgID, orgSet)
		if !ok {
			span.SetAttributes(telemetry.Outcome("no_credential"))
			return
		}
		n, _ := m.runMarked(ctx, orgID, scope, client, self, repos, alerts, resolver, ledger)
		events += n
	} else {
		installs, err := m.apps.ListInstallationsOnHostSystem(ctx, orgID, scope)
		if err != nil {
			span.SetStatus(codes.Error, "list installations")
			githubLog.WarnContext(ctx, "list installations for the narrowed pass failed; its marks wait", "org", orgID, "error", err)
			return
		}
		known := m.knownGrantsOf(orgID)
		resolver := m.reviewerResolver(ctx, orgID, domain.GitHubAccount{}, nil)
		covered := make(map[string]bool, len(repos))
		for _, inst := range installs {
			reach, ok := known[inst.InstallationID]
			if !ok {
				continue
			}
			var handed []domain.Repository
			for _, r := range repos {
				if !covered[r.ID] && reach.Holds(r) {
					handed = append(handed, r)
					covered[r.ID] = true
				}
			}
			if len(handed) == 0 {
				continue
			}
			client, err := m.resolver.ClientForInstallation(ctx, orgID, inst)
			if err != nil {
				// Debug: the scheduled cycle resolves the same client and
				// reports its failure once a cycle, where a narrowed pass
				// would repeat it on every sweep while the marks wait.
				githubLog.DebugContext(ctx, "resolve client for the narrowed pass failed; its marks wait", "org", orgID, "installation", inst.InstallationID, "error", err)
				continue
			}
			n, err := m.runMarked(ctx, orgID, scope, client, domain.GitHubAccount{}, handed, alerts, resolver, ledger)
			events += n
			// The scheduled cycle's rule: a rate limit stops the fan-out
			// rather than spend the other installations' budgets finding the
			// same thing.
			var rl *ghclient.ErrRateLimited
			if errors.As(err, &rl) {
				break
			}
		}
	}
	span.SetAttributes(telemetry.Count(events))
	if events > 0 && m.TriggerScorer != nil {
		m.TriggerScorer(orgID)
	}
}

// runMarked refreshes what the marks on repos name under client: a
// RefreshMarkedGitHub over the repositories a mark names pull requests or a
// branch in (runNarrowed), then a listing of the alerts of each one alerts
// holds (runNarrowedAlerts). A rate limit on the first skips the second. The
// error joins both passes' errors.
func (m *Manager) runMarked(ctx context.Context, orgID, scope string, client *ghclient.Client, self domain.GitHubAccount, repos []domain.Repository, alerts map[string]tracker.AlertListing, resolver tracker.ReviewerResolver, ledger *markLedger) (int, error) {
	var prs []domain.Repository
	var listings []tracker.AlertListing
	for _, r := range repos {
		if ledger.pullRequestsMarked(r.ID) {
			prs = append(prs, r)
		}
		if l, ok := alerts[r.ID]; ok {
			listings = append(listings, l)
		}
	}
	events := 0
	var prErr error
	if len(prs) > 0 {
		var n int
		n, prErr = m.runNarrowed(ctx, orgID, scope, client, self, prs, resolver, ledger)
		events += n
		var rl *ghclient.ErrRateLimited
		if errors.As(prErr, &rl) {
			return events, prErr
		}
	}
	if len(listings) == 0 {
		return events, prErr
	}
	n, alertsErr := m.runNarrowedAlerts(ctx, orgID, scope, client, listings, ledger)
	return events + n, errors.Join(prErr, alertsErr)
}

// runNarrowed runs one RefreshMarkedGitHub over repos and records the outcome
// in ledger: the marks on repos naming pull requests or a branch are honored
// when the pass completes. A rate limit holds the org's GitHub polls, narrowed
// and scheduled, until it resets.
func (m *Manager) runNarrowed(ctx context.Context, orgID, scope string, client *ghclient.Client, self domain.GitHubAccount, repos []domain.Repository, resolver tracker.ReviewerResolver, ledger *markLedger) (int, error) {
	slugs := make([]string, len(repos))
	for i, r := range repos {
		slugs[i] = r.Slug()
	}
	n, err := m.trackerForOrg(orgID).WithRefreshMarks(ledger.marks()).RefreshMarkedGitHub(ctx, scope, client, self, slugs, resolver)
	if err != nil {
		githubLog.Log(ctx, upstream.LogLevel(err, slog.LevelError), "narrowed pass failed; its marks wait for the next pass", "org", orgID, "error", err)
		m.reportError("github", orgID, err)
		var rl *ghclient.ErrRateLimited
		if errors.As(err, &rl) {
			m.holdPoll("github", orgID, rl.ResumeAt)
		}
		return n, err
	}
	ledger.honor(repos)
	return n, nil
}

// runNarrowedAlerts lists the alerts of the repositories listings hold, each
// one marked, under client, and records the outcome in ledger: the alert mark
// on each repository the listing finished is honored, a repository whose
// listing GitHub refused or answered unusably included, since the scheduled
// cycle lists it as long as its recorded answer says the alerts are readable.
// A rate limit holds the org's GitHub polls, as in runNarrowed.
func (m *Manager) runNarrowedAlerts(ctx context.Context, orgID, scope string, client *ghclient.Client, listings []tracker.AlertListing, ledger *markLedger) (int, error) {
	n, finished, err := m.trackerForOrg(orgID).RefreshVulnerablePackages(ctx, scope, client, listings)
	ledger.honorAlerts(listings[:finished])
	if err != nil {
		var rl *ghclient.ErrRateLimited
		if errors.As(err, &rl) {
			m.holdPoll("github", orgID, rl.ResumeAt)
		} else {
			githubLog.Log(ctx, upstream.LogLevel(err, slog.LevelWarn), "narrowed dependabot alerts listing failed; its marks wait for the next pass", "org", orgID, "error", err)
		}
	}
	return n, err
}

// markedAlertListings is the listing a narrowed pass makes for each of repos
// whose alerts a mark names, by registry row id: marked, with the repository's
// recorded answer. A mark on a repository whose alerts the poll does not list,
// with an answer that says they cannot be read or none, names nothing a pass
// refreshes and is cleared. A failed read of the answers lists no alerts this
// pass and leaves their marks for the next.
func (m *Manager) markedAlertListings(ctx context.Context, orgID, scope string, ledger *markLedger, repos []domain.Repository) map[string]tracker.AlertListing {
	var marked []domain.Repository
	for _, r := range repos {
		if ledger.alertsMarked(r.ID) {
			marked = append(marked, r)
		}
	}
	if len(marked) == 0 {
		return nil
	}
	if m.DependabotAccess == nil {
		for _, r := range marked {
			ledger.dropUnlistedAlerts(r.ID)
		}
		return nil
	}
	answers, err := m.DependabotAccess.ListSystem(ctx, orgID, scope)
	if err != nil {
		githubLog.ErrorContext(ctx, "read dependabot access answers for the narrowed pass failed; its alert marks wait", "org", orgID, "error", err)
		return nil
	}
	byRepo := make(map[string]domain.RepositoryDependabotAccess, len(answers))
	for _, a := range answers {
		byRepo[a.RepositoryID] = a
	}
	out := make(map[string]tracker.AlertListing, len(marked))
	for _, r := range marked {
		a, ok := byRepo[r.ID]
		if !ok || a.Access != domain.DependabotAlertsReadable {
			ledger.dropUnlistedAlerts(r.ID)
			continue
		}
		out[r.ID] = tracker.AlertListing{Repository: r, Access: a, Marked: true}
	}
	return out
}

// markedPATPass is patPass for the narrowed pass: the org's personal-token
// client, the account it acts as, and the reviewer resolver, without the team
// listing. In local mode the resolver takes the user's teams from the last
// scheduled cycle that listed them, and with none listed yet there is no pass:
// a review requested from one of the user's teams would otherwise read as
// requested from nobody, and the snapshot that advanced past it would never
// emit it.
func (m *Manager) markedPATPass(ctx context.Context, orgID string, orgSet domain.OrgSettings) (*ghclient.Client, domain.GitHubAccount, tracker.ReviewerResolver, bool) {
	client, err := m.resolver.ClientFor(ctx, orgID, "")
	if err != nil {
		// Debug, as for an installation's client in the narrowed pass.
		githubLog.DebugContext(ctx, "resolve pat client for the narrowed pass failed; its marks wait", "org", orgID, "error", err)
		return nil, domain.GitHubAccount{}, nil, false
	}
	if runmode.Current() != runmode.ModeLocal {
		return client, domain.GitHubAccount{}, m.reviewerResolver(ctx, orgID, domain.GitHubAccount{}, nil), true
	}
	teams, listed := m.localTeamsOf(orgID)
	if !listed {
		return nil, domain.GitHubAccount{}, nil, false
	}
	identity, err := m.users.GetGitHubIdentitySystem(ctx, runmode.LocalDefaultUserID, orgSet.GitHubBaseURL)
	if err != nil {
		githubLog.ErrorContext(ctx, "read github identity for the narrowed pass failed; its marks wait", "org", orgID, "error", err)
		return nil, domain.GitHubAccount{}, nil, false
	}
	self := domain.GitHubAccountOf(identity)
	return client, self, m.reviewerResolver(ctx, orgID, self, teams), true
}

// pollHeld reports whether a rate-limit hold (holdPoll) on orgID's polls of
// source is still ahead of now. Unlike pollDue it never lifts the hold.
func (m *Manager) pollHeld(source, orgID string, now time.Time) bool {
	m.dueMu.Lock()
	defer m.dueMu.Unlock()
	hold, ok := m.pollHold[pollKey(source, orgID)]
	return ok && now.Before(hold)
}

// knownGrantsOf returns the reach of each of orgID's App installations, by
// installation id, as its last scheduled cycle read the grant.
func (m *Manager) knownGrantsOf(orgID string) map[string]tracker.GitHubReach {
	m.knownMu.Lock()
	defer m.knownMu.Unlock()
	return m.knownGrants[orgID]
}

// rememberGrants records the reach of each installation the scheduled cycle
// read the grant of, keeping the last known reach of each other one in installs
// (an installation the cycle could not resolve, or did not reach before a rate
// limit stopped it) and dropping every installation not in installs.
func (m *Manager) rememberGrants(orgID string, installs []domain.OrgGitHubAppInstallation, read map[string]tracker.GitHubReach) {
	m.knownMu.Lock()
	defer m.knownMu.Unlock()
	prev := m.knownGrants[orgID]
	next := make(map[string]tracker.GitHubReach, len(installs))
	for _, inst := range installs {
		if r, ok := read[inst.InstallationID]; ok {
			next[inst.InstallationID] = r
		} else if r, ok := prev[inst.InstallationID]; ok {
			next[inst.InstallationID] = r
		}
	}
	if m.knownGrants == nil {
		m.knownGrants = make(map[string]map[string]tracker.GitHubReach)
	}
	m.knownGrants[orgID] = next
}

// localTeamsOf returns the local user's teams as the last scheduled cycle
// that listed them read them, and whether one has.
func (m *Manager) localTeamsOf(orgID string) ([]string, bool) {
	m.knownMu.Lock()
	defer m.knownMu.Unlock()
	teams, ok := m.localTeams[orgID]
	return teams, ok
}

func (m *Manager) rememberLocalTeams(orgID string, teams []string) {
	m.knownMu.Lock()
	defer m.knownMu.Unlock()
	if m.localTeams == nil {
		m.localTeams = make(map[string][]string)
	}
	m.localTeams[orgID] = slices.Clone(teams)
}

// pruneKnown drops what the last scheduled cycles learned about orgs no longer
// active.
func (m *Manager) pruneKnown(activeOrgIDs []string) {
	active := make(map[string]bool, len(activeOrgIDs))
	for _, id := range activeOrgIDs {
		active[id] = true
	}
	m.knownMu.Lock()
	defer m.knownMu.Unlock()
	for orgID := range m.knownGrants {
		if !active[orgID] {
			delete(m.knownGrants, orgID)
		}
	}
	for orgID := range m.localTeams {
		if !active[orgID] {
			delete(m.localTeams, orgID)
		}
	}
}

// markLedger is one pass's account of the refresh marks it read: the ones it
// honored, which a pass over what they name in their repository completed
// after the read (its pull requests for a mark naming pull requests or a
// branch, a listing of its alerts for one naming its Dependabot alerts), and
// the ones that name nothing the poll refreshes: on a repository no team
// tracks any more, or no credential reads, or naming alerts the poll does not
// list. commit clears both kinds; every other mark stays for the next pass.
type markLedger struct {
	m     *Manager
	orgID string
	read  []domain.GitHubRefreshMark
	// tracked is every repository the org tracks on the host, by registry
	// row id.
	tracked map[string]domain.Repository
	done    map[string]bool // ids of the marks to clear
}

// readMarks reads the org's oldest pending marks and the tracked set they are
// judged against. It returns nil when there are none, or when either read
// fails, which is logged: a pass then honors no mark and clears none.
func (m *Manager) readMarks(ctx context.Context, orgID, scope string) *markLedger {
	if m.RefreshMarks == nil {
		return nil
	}
	marks, err := m.RefreshMarks.ListSystem(ctx, orgID, markReadLimit)
	if err != nil {
		githubLog.ErrorContext(ctx, "read github refresh marks failed; this pass honors none", "org", orgID, "error", err)
		return nil
	}
	if len(marks) == 0 {
		return nil
	}
	rows, _, err := m.repos.ListTrackedSystem(ctx, orgID, scope, db.Unwindowed)
	if err != nil {
		githubLog.ErrorContext(ctx, "read tracked repositories for the refresh marks failed; this pass honors none", "org", orgID, "error", err)
		return nil
	}
	tracked := make(map[string]domain.Repository, len(rows))
	for _, r := range rows {
		tracked[r.ID] = r
	}
	l := &markLedger{m: m, orgID: orgID, read: marks, tracked: tracked, done: map[string]bool{}}
	for _, mark := range marks {
		if _, ok := tracked[mark.RepositoryID]; !ok {
			l.done[mark.ID] = true
		}
	}
	return l
}

// marks is what the pass's tracker honors; nil for a nil ledger.
func (l *markLedger) marks() []domain.GitHubRefreshMark {
	if l == nil {
		return nil
	}
	return l.read
}

// pullRequestsMarked reports whether a mark the ledger read names pull
// requests or a branch in the repository whose registry row is repositoryID.
// nil-safe.
func (l *markLedger) pullRequestsMarked(repositoryID string) bool {
	return l.marked(repositoryID, isPullRequestTarget)
}

// alertsMarked reports whether a mark the ledger read names the Dependabot
// alerts of the repository whose registry row is repositoryID. nil-safe.
func (l *markLedger) alertsMarked(repositoryID string) bool {
	return l.marked(repositoryID, domain.GitHubRefreshTarget.DependabotAlerts)
}

func (l *markLedger) marked(repositoryID string, kind func(domain.GitHubRefreshTarget) bool) bool {
	if l == nil {
		return false
	}
	return slices.ContainsFunc(l.read, func(mark domain.GitHubRefreshMark) bool {
		return mark.RepositoryID == repositoryID && kind(mark.Target)
	})
}

// isPullRequestTarget reports whether a target names pull requests or a
// branch, which a pass over the repository's pull requests refreshes.
func isPullRequestTarget(t domain.GitHubRefreshTarget) bool {
	return !t.DependabotAlerts()
}

// markedRepos is the tracked repositories the marks name, in the order of
// their oldest mark.
func (l *markLedger) markedRepos() []domain.Repository {
	seen := map[string]bool{}
	var out []domain.Repository
	for _, mark := range l.read {
		if r, ok := l.tracked[mark.RepositoryID]; ok && !seen[r.ID] {
			seen[r.ID] = true
			out = append(out, r)
		}
	}
	return out
}

// honor records that a pass over repos' pull requests completed, which honors
// their marks naming pull requests or a branch. A mark naming a repository's
// alerts waits for a listing of them (honorAlerts).
func (l *markLedger) honor(repos []domain.Repository) {
	if l == nil {
		return
	}
	ids := make([]string, len(repos))
	for i, r := range repos {
		ids[i] = r.ID
	}
	l.settle(ids, isPullRequestTarget)
}

// honorAlerts records that the listing of the alerts of each of listings'
// repositories finished, which honors the marks naming them. nil-safe.
func (l *markLedger) honorAlerts(listings []tracker.AlertListing) {
	if l == nil {
		return
	}
	ids := make([]string, len(listings))
	for i, listing := range listings {
		ids[i] = listing.Repository.ID
	}
	l.settle(ids, domain.GitHubRefreshTarget.DependabotAlerts)
}

// dropUnlistedAlerts records that the poll does not list the alerts of the
// repository whose registry row is repositoryID, so a mark naming them names
// nothing a pass refreshes. nil-safe.
func (l *markLedger) dropUnlistedAlerts(repositoryID string) {
	if l == nil {
		return
	}
	l.settle([]string{repositoryID}, domain.GitHubRefreshTarget.DependabotAlerts)
}

// settle records as done each mark the ledger read on a repository in ids
// whose target kind holds.
func (l *markLedger) settle(ids []string, kind func(domain.GitHubRefreshTarget) bool) {
	for _, mark := range l.read {
		if slices.Contains(ids, mark.RepositoryID) && kind(mark.Target) {
			l.done[mark.ID] = true
		}
	}
}

// honorReach records that a scheduled pass under a credential whose reach is
// reach completed: its own pull requests are those of every tracked
// repository the reach holds, whichever of them it was handed. Like honor, it
// leaves the marks naming alerts to the cycle's listing of them.
func (l *markLedger) honorReach(reach tracker.GitHubReach) {
	if l == nil {
		return
	}
	l.honor(l.held(func(r domain.Repository) bool { return reach.Holds(r) }))
}

// dropUnreached records that no credential of the org reads the tracked
// repositories none of reaches holds, so their marks name nothing any pass can
// refresh. Only a cycle that read every installation's grant knows that.
func (l *markLedger) dropUnreached(reaches []tracker.GitHubReach) {
	if l == nil {
		return
	}
	for _, r := range l.held(func(r domain.Repository) bool {
		return !slices.ContainsFunc(reaches, func(reach tracker.GitHubReach) bool { return reach.Holds(r) })
	}) {
		for _, mark := range l.read {
			if mark.RepositoryID == r.ID {
				l.done[mark.ID] = true
			}
		}
	}
}

// held is the marked tracked repositories keep holds.
func (l *markLedger) held(keep func(domain.Repository) bool) []domain.Repository {
	var out []domain.Repository
	for _, r := range l.markedRepos() {
		if keep(r) {
			out = append(out, r)
		}
	}
	return out
}

// commit clears the marks the pass honored or found naming nothing, each at
// the revision it was read at, so a delivery that landed on one since keeps
// it for the next pass.
func (l *markLedger) commit(ctx context.Context) {
	if l == nil || len(l.done) == 0 {
		return
	}
	var clear []domain.GitHubRefreshMark
	for _, mark := range l.read {
		if l.done[mark.ID] {
			clear = append(clear, mark)
		}
	}
	if _, err := l.m.RefreshMarks.ClearSystem(ctx, l.orgID, clear); err != nil {
		githubLog.WarnContext(ctx, "clear github refresh marks failed; the next pass refreshes what they name again", "org", l.orgID, "error", err)
	}
}
