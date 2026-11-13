package poller

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/tracker"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// dependabotProbesPerCycle caps the probes one org's cycle makes across all
// of its credential passes, so a large tracked set is probed over several
// cycles rather than with a request for every repository at once. The
// repositories never probed go first, then the longest-standing answers.
const dependabotProbesPerCycle = 25

// dependabotProbe records, per tracked repository, whether the org's GitHub
// credential can read its Dependabot alerts, and then lists the open alerts of
// each repository whose answer is readable (tracker.RefreshVulnerablePackages).
// One is made per org cycle. Each credential pass adds the repositories it
// reads, and the probes and listings run once, after every pass has
// refreshed, so they never delay a refresh.
type dependabotProbe struct {
	m      *Manager
	orgID  string
	host   string
	passes []dependabotPass

	// What run read and answered, for listAlerts: the tracked repositories
	// with their answers, the untracked repositories with a listing, and
	// whether every probe due got an answer.
	targets   map[string]dependabotTarget
	untracked []string
	answered  bool
}

// dependabotPass is one credential and the repositories it reads.
type dependabotPass struct {
	client *ghclient.Client
	repos  []string
}

// newDependabotProbe returns the cycle's probe, or nil when the Manager holds
// no store to record answers in (tests and embedders that haven't wired it).
func (m *Manager) newDependabotProbe(orgID, host string) *dependabotProbe {
	if m.DependabotAccess == nil {
		return nil
	}
	return &dependabotProbe{m: m, orgID: orgID, host: host}
}

// add queues repos for probing under client. nil-safe.
func (p *dependabotProbe) add(client *ghclient.Client, repos []string) {
	if p == nil || len(repos) == 0 {
		return
	}
	p.passes = append(p.passes, dependabotPass{client: client, repos: repos})
}

// run probes the tracked repositories the passes added whose answer is
// missing or older than domain.DependabotAccessRecheck, and records each
// answer GitHub gives. The first request that gets no answer ends the cycle's
// probes: a rate limit, a refused or revoked credential, or GitHub failing
// would meet every remaining request too, so the next cycle tries again.
// nil-safe.
func (p *dependabotProbe) run(ctx context.Context) {
	if p == nil || len(p.passes) == 0 {
		return
	}
	targets, untracked, err := p.load(ctx)
	if err != nil {
		githubLog.WarnContext(ctx, "read dependabot access answers failed; none probed this cycle", "org", p.orgID, "error", err)
		return
	}
	p.targets, p.untracked = targets, untracked
	p.answered = p.probe(ctx, targets)
}

// probe asks GitHub for the answers that are due and records them, updating
// targets with each answer stored. It reports false when a request got no
// answer.
func (p *dependabotProbe) probe(ctx context.Context, targets map[string]dependabotTarget) bool {
	now := time.Now()
	type due struct {
		name   string
		client *ghclient.Client
		target dependabotTarget
	}
	var dueRepos []due
	seen := map[string]bool{}
	for _, pass := range p.passes {
		for _, name := range pass.repos {
			t, ok := targets[name]
			if !ok || seen[name] {
				continue
			}
			seen[name] = true
			if t.answer != nil && now.Sub(t.answer.CheckedAt) < domain.DependabotAccessRecheck {
				continue
			}
			dueRepos = append(dueRepos, due{name: name, client: pass.client, target: t})
		}
	}
	slices.SortStableFunc(dueRepos, func(a, b due) int {
		return checkedAt(a.target).Compare(checkedAt(b.target))
	})
	if len(dueRepos) > dependabotProbesPerCycle {
		dueRepos = dueRepos[:dependabotProbesPerCycle]
	}

	for _, d := range dueRepos {
		if ctx.Err() != nil {
			return false
		}
		owner, repo, _ := strings.Cut(d.name, "/")
		access, err := d.client.DependabotAlertsAccess(ctx, owner, repo)
		if err != nil {
			var rl *ghclient.ErrRateLimited
			if !errors.As(err, &rl) {
				githubLog.Log(ctx, upstream.LogLevel(err, slog.LevelWarn), "dependabot alerts access probe failed; the rest wait for the next cycle", "org", p.orgID, "repo", d.name, "error", err)
			}
			return false
		}
		stored, err := p.m.DependabotAccess.RecordSystem(ctx, p.orgID, d.target.repository.ID, access)
		if err != nil {
			githubLog.ErrorContext(ctx, "record dependabot alerts access failed", "org", p.orgID, "repo", d.name, "error", err)
			continue
		}
		if d.target.answer == nil || d.target.answer.Access != stored.Access {
			githubLog.InfoContext(ctx, "dependabot alerts access", "org", p.orgID, "repo", d.name, "access", stored.Access)
		}
		d.target.answer = &stored
		targets[d.name] = d.target
	}
	return true
}

// listAlerts runs after run: it clears the listing of each repository no team
// tracks, and lists the open alerts of every repository a pass added whose
// answer is readable, under the credential of the first pass that added it
// (tracker.RefreshVulnerablePackages). A cycle whose probes got no answer
// lists nothing, and a listing that gets none ends the cycle's listings.
// nil-safe.
func (p *dependabotProbe) listAlerts(ctx context.Context) {
	if p == nil || p.targets == nil {
		return
	}
	p.forgetListings(ctx, p.untracked)
	if !p.answered {
		return
	}
	targets := p.targets
	tr := p.m.trackerForOrg(p.orgID)
	seen := map[string]bool{}
	for _, pass := range p.passes {
		var listings []tracker.AlertListing
		for _, name := range pass.repos {
			t, ok := targets[name]
			if !ok || seen[name] || t.answer == nil || t.answer.Access != domain.DependabotAlertsReadable {
				continue
			}
			seen[name] = true
			listings = append(listings, tracker.AlertListing{Repository: t.repository, Access: *t.answer})
		}
		if len(listings) == 0 {
			continue
		}
		if _, err := tr.RefreshVulnerablePackages(ctx, p.host, pass.client, listings); err != nil {
			var rl *ghclient.ErrRateLimited
			if !errors.As(err, &rl) {
				githubLog.Log(ctx, upstream.LogLevel(err, slog.LevelWarn), "dependabot alerts listing failed; the rest wait for the next cycle", "org", p.orgID, "error", err)
			}
			return
		}
	}
}

// forgetListings clears the alert listing of each repository in untracked, so
// its next listing, once a team tracks it again, is a baseline.
func (p *dependabotProbe) forgetListings(ctx context.Context, untracked []string) {
	for _, repositoryID := range untracked {
		if _, err := p.m.DependabotAccess.ClearAlertListingSystem(ctx, p.orgID, repositoryID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			githubLog.ErrorContext(ctx, "clear dependabot alert listing of an untracked repository failed", "org", p.orgID, "repository_id", repositoryID, "error", err)
		}
	}
}

// dependabotTarget is a tracked repository and the answer recorded for it,
// if any.
type dependabotTarget struct {
	repository domain.Repository
	answer     *domain.RepositoryDependabotAccess
}

// load reads the org's tracked repositories on the host and the answers
// recorded for them, keyed by owner/repo. untracked is the repositories no
// team tracks whose alerts have a listing recorded.
func (p *dependabotProbe) load(ctx context.Context) (targets map[string]dependabotTarget, untracked []string, err error) {
	tracked, _, err := p.m.repos.ListTrackedSystem(ctx, p.orgID, p.host, db.Unwindowed)
	if err != nil {
		return nil, nil, err
	}
	answers, err := p.m.DependabotAccess.ListSystem(ctx, p.orgID, p.host)
	if err != nil {
		return nil, nil, err
	}
	byRepo := make(map[string]*domain.RepositoryDependabotAccess, len(answers))
	for i := range answers {
		byRepo[answers[i].RepositoryID] = &answers[i]
	}
	targets = make(map[string]dependabotTarget, len(tracked))
	for _, r := range tracked {
		targets[r.Slug()] = dependabotTarget{repository: r, answer: byRepo[r.ID]}
		delete(byRepo, r.ID)
	}
	for id, a := range byRepo {
		if a.AlertsBaselined() {
			untracked = append(untracked, id)
		}
	}
	slices.Sort(untracked)
	return targets, untracked, nil
}

// checkedAt is when the target's answer was recorded, zero when it has none,
// which orders a never-probed repository first.
func checkedAt(t dependabotTarget) time.Time {
	if t.answer == nil {
		return time.Time{}
	}
	return t.answer.CheckedAt
}
