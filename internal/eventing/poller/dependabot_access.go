package poller

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// dependabotProbesPerCycle caps the probes one org's cycle makes across all
// of its credential passes, so a large tracked set is probed over several
// cycles rather than with a request for every repository at once. The
// repositories never probed go first, then the longest-standing answers.
const dependabotProbesPerCycle = 25

// dependabotProbe records, per tracked repository, whether the org's GitHub
// credential can read its Dependabot alerts. One is made per org cycle. Each
// credential pass adds the repositories it reads, and the probes run once,
// after every pass has refreshed, so they never delay a refresh.
type dependabotProbe struct {
	m      *Manager
	orgID  string
	host   string
	passes []dependabotPass
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
	targets, err := p.load(ctx)
	if err != nil {
		githubLog.WarnContext(ctx, "read dependabot access answers failed; none probed this cycle", "org", p.orgID, "error", err)
		return
	}

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
			return
		}
		owner, repo, _ := strings.Cut(d.name, "/")
		access, err := d.client.DependabotAlertsAccess(ctx, owner, repo)
		if err != nil {
			var rl *ghclient.ErrRateLimited
			if !errors.As(err, &rl) {
				githubLog.Log(ctx, upstream.LogLevel(err, slog.LevelWarn), "dependabot alerts access probe failed; the rest wait for the next cycle", "org", p.orgID, "repo", d.name, "error", err)
			}
			return
		}
		stored, err := p.m.DependabotAccess.RecordSystem(ctx, p.orgID, d.target.repositoryID, access)
		if err != nil {
			githubLog.ErrorContext(ctx, "record dependabot alerts access failed", "org", p.orgID, "repo", d.name, "error", err)
			continue
		}
		if d.target.answer == nil || d.target.answer.Access != stored.Access {
			githubLog.InfoContext(ctx, "dependabot alerts access", "org", p.orgID, "repo", d.name, "access", stored.Access)
		}
	}
}

// dependabotTarget is a tracked repository and the answer recorded for it,
// if any.
type dependabotTarget struct {
	repositoryID string
	answer       *domain.RepositoryDependabotAccess
}

// load reads the org's tracked repositories on the host and the answers
// recorded for them, keyed by owner/repo.
func (p *dependabotProbe) load(ctx context.Context) (map[string]dependabotTarget, error) {
	tracked, _, err := p.m.repos.ListTrackedSystem(ctx, p.orgID, p.host, db.Unwindowed)
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
	out := make(map[string]dependabotTarget, len(tracked))
	for _, r := range tracked {
		out[r.Slug()] = dependabotTarget{repositoryID: r.ID, answer: byRepo[r.ID]}
	}
	return out, nil
}

// checkedAt is when the target's answer was recorded, zero when it has none,
// which orders a never-probed repository first.
func checkedAt(t dependabotTarget) time.Time {
	if t.answer == nil {
		return time.Time{}
	}
	return t.answer.CheckedAt
}
