package poller

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
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
// It does the same for each repository no team tracks that still holds an
// active vulnerable package, so the tasks on its packages close when the
// alerts are fixed, as those on a pull request in an untracked repository
// close when it merges. One is made per org cycle. Each credential pass adds
// the repositories it reads, and the probes and listings run once, after every
// pass has refreshed, so they never delay a refresh.
type dependabotProbe struct {
	m      *Manager
	orgID  string
	host   string
	passes []dependabotPass

	// What run read, for listAlerts: the repositories with their answers,
	// each updated by the probe that answered it.
	targets map[string]dependabotTarget
}

// dependabotPass is one credential, the tracked repositories its pass reads,
// and everything it can read (an App installation's grant, or every
// repository for a personal token), which decides the untracked repositories
// it lists.
type dependabotPass struct {
	client *ghclient.Client
	repos  []string
	reach  tracker.GitHubReach
}

// newDependabotProbe returns the cycle's probe, or nil when the Manager holds
// no store to record answers in (tests and embedders that haven't wired it).
func (m *Manager) newDependabotProbe(orgID, host string) *dependabotProbe {
	if m.DependabotAccess == nil {
		return nil
	}
	return &dependabotProbe{m: m, orgID: orgID, host: host}
}

// add queues a credential pass: client, the tracked repos it reads, and its
// reach. A pass with no tracked repository still lists the untracked ones its
// reach holds. nil-safe.
func (p *dependabotProbe) add(client *ghclient.Client, repos []string, reach tracker.GitHubReach) {
	if p == nil {
		return
	}
	p.passes = append(p.passes, dependabotPass{client: client, repos: repos, reach: reach})
}

// run probes the repositories a pass covers whose answer is missing or older
// than domain.DependabotAccessRecheck, and records each answer GitHub gives.
// The first request that gets no answer ends the cycle's probes: a rate
// limit, a refused or revoked credential, or GitHub failing would meet every
// remaining request too, so the next cycle tries again. nil-safe.
func (p *dependabotProbe) run(ctx context.Context) {
	if p == nil || len(p.passes) == 0 {
		return
	}
	targets, err := p.load(ctx)
	if err != nil {
		githubLog.WarnContext(ctx, "read dependabot access answers failed; none probed this cycle", "org", p.orgID, "error", err)
		return
	}
	p.targets = targets
	p.probe(ctx, targets)
}

// probe asks GitHub for the answers that are due and records them, updating
// targets with each answer stored. It stops at the first request that gets no
// answer.
func (p *dependabotProbe) probe(ctx context.Context, targets map[string]dependabotTarget) {
	now := time.Now()
	var due []string
	for _, name := range slices.Sorted(maps.Keys(targets)) {
		t := targets[name]
		if t.pass < 0 || (t.answer != nil && now.Sub(t.answer.CheckedAt) < domain.DependabotAccessRecheck) {
			continue
		}
		due = append(due, name)
	}
	slices.SortStableFunc(due, func(a, b string) int {
		return checkedAt(targets[a]).Compare(checkedAt(targets[b]))
	})
	if len(due) > dependabotProbesPerCycle {
		due = due[:dependabotProbesPerCycle]
	}

	for _, name := range due {
		if ctx.Err() != nil {
			return
		}
		t := targets[name]
		access, err := p.passes[t.pass].client.DependabotAlertsAccess(ctx, t.repository.Owner, t.repository.Repo)
		if err != nil {
			var rl *ghclient.ErrRateLimited
			if !errors.As(err, &rl) {
				githubLog.Log(ctx, upstream.LogLevel(err, slog.LevelWarn), "dependabot alerts access probe failed; the rest wait for the next cycle", "org", p.orgID, "repo", name, "error", err)
			}
			return
		}
		stored, err := p.m.DependabotAccess.RecordSystem(ctx, p.orgID, t.repository.ID, access)
		if err != nil {
			githubLog.ErrorContext(ctx, "record dependabot alerts access failed", "org", p.orgID, "repo", name, "error", err)
			continue
		}
		if t.answer == nil || t.answer.Access != stored.Access {
			githubLog.InfoContext(ctx, "dependabot alerts access", "org", p.orgID, "repo", name, "access", stored.Access)
		}
		t.answer = &stored
		targets[name] = t
	}
}

// listAlerts runs after run. It first retires the packages of every
// repository whose answer says its alerts can no longer be read
// (tracker.RetireUnreadableVulnerablePackages), then lists the open alerts of
// every repository a pass covers whose answer is readable, under that pass's
// credential (tracker.RefreshVulnerablePackages). It lists even when a probe
// got no answer, since that probe's repository may be the only one failing,
// and a failure that would meet every request ends the listings at their
// first one. nil-safe.
func (p *dependabotProbe) listAlerts(ctx context.Context) {
	if p == nil || p.targets == nil {
		return
	}
	tr := p.m.trackerForOrg(p.orgID)
	names := slices.Sorted(maps.Keys(p.targets))
	var answered []tracker.AlertListing
	for _, name := range names {
		if t := p.targets[name]; t.answer != nil {
			answered = append(answered, t.listing())
		}
	}
	tr.RetireUnreadableVulnerablePackages(ctx, p.host, answered)

	for i, pass := range p.passes {
		var listings []tracker.AlertListing
		for _, name := range names {
			t := p.targets[name]
			if t.pass == i && t.answer != nil && t.answer.Access == domain.DependabotAlertsReadable {
				listings = append(listings, t.listing())
			}
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

// forgetUntrackedAlertListings forgets the ETag of every repository of the
// org the poll no longer lists on host (RepositoryDependabotAccessStore's
// ClearUntrackedAlertListingsSystem), so a repository tracked again is listed
// in full. The cycle runs it before any of its gates, because a repository
// leaves the tracked set when a team untracks it and also when the org moves
// to another GitHub host, and an org that has just moved tracks nothing there
// yet. A write that fails is made again next cycle.
func (m *Manager) forgetUntrackedAlertListings(ctx context.Context, orgID, host string) {
	if m.DependabotAccess == nil || host == "" {
		return
	}
	n, err := m.DependabotAccess.ClearUntrackedAlertListingsSystem(ctx, orgID, host)
	if err != nil {
		githubLog.ErrorContext(ctx, "forget the dependabot alert listings of untracked repositories failed", "org", orgID, "error", err)
		return
	}
	if n > 0 {
		githubLog.InfoContext(ctx, "forgot the dependabot alert listings of untracked repositories", "org", orgID, "count", n)
	}
}

// dependabotTarget is a repository the probe covers, the answer recorded for
// it, if any, and the index of the pass whose credential probes and lists it:
// for a tracked repository the first pass that reads it, for an untracked one
// the first whose reach holds it, and -1 when no pass does.
type dependabotTarget struct {
	repository domain.Repository
	answer     *domain.RepositoryDependabotAccess
	untracked  bool
	pass       int
}

func (t dependabotTarget) listing() tracker.AlertListing {
	return tracker.AlertListing{Repository: t.repository, Access: *t.answer, Untracked: t.untracked}
}

// load reads the org's tracked repositories on the host, the untracked ones
// that hold an active vulnerable package, and the answers recorded for them,
// keyed by owner/repo, each with the pass that covers it.
func (p *dependabotProbe) load(ctx context.Context) (map[string]dependabotTarget, error) {
	tracked, _, err := p.m.repos.ListTrackedSystem(ctx, p.orgID, p.host, db.Unwindowed)
	if err != nil {
		return nil, err
	}
	held, err := p.m.repos.ListHeldUntrackedSystem(ctx, p.orgID, p.host)
	if err != nil {
		return nil, err
	}
	packages, err := p.m.entities.ListActiveKindSystem(ctx, p.orgID, domain.ProviderGitHub, p.host, domain.EntityKindVulnerablePackage)
	if err != nil {
		return nil, err
	}
	answers, err := p.m.DependabotAccess.ListSystem(ctx, p.orgID, p.host)
	if err != nil {
		return nil, err
	}
	byRepo := make(map[string]*domain.RepositoryDependabotAccess, len(answers))
	for i := range answers {
		byRepo[answers[i].RepositoryID] = &answers[i]
	}
	holding := make(map[string]bool, len(packages))
	for _, e := range packages {
		holding[e.RepositoryID] = true
	}
	out := make(map[string]dependabotTarget, len(tracked))
	for _, r := range tracked {
		out[r.Slug()] = dependabotTarget{repository: r, answer: byRepo[r.ID], pass: p.trackedPass(r.Slug())}
	}
	for _, r := range held {
		if holding[r.ID] {
			out[r.Slug()] = dependabotTarget{repository: r, answer: byRepo[r.ID], untracked: true, pass: p.reachPass(r)}
		}
	}
	return out, nil
}

// trackedPass is the index of the first pass that reads the tracked
// repository named name, -1 when none does.
func (p *dependabotProbe) trackedPass(name string) int {
	return slices.IndexFunc(p.passes, func(pass dependabotPass) bool { return slices.Contains(pass.repos, name) })
}

// reachPass is the index of the first pass whose reach holds r, -1 when none
// does.
func (p *dependabotProbe) reachPass(r domain.Repository) int {
	return slices.IndexFunc(p.passes, func(pass dependabotPass) bool { return pass.reach.Holds(r) })
}

// checkedAt is when the target's answer was recorded, zero when it has none,
// which orders a never-probed repository first.
func checkedAt(t dependabotTarget) time.Time {
	if t.answer == nil {
		return time.Time{}
	}
	return t.answer.CheckedAt
}
