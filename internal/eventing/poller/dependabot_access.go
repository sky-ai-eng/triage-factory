package poller

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// dependabotAccessRecheck is how long a recorded answer stands before the poll
// asks GitHub again. It is how TF notices a credential gaining or losing the
// permission, or alerts being turned on or off on a repository, at a cost of
// one request per tracked repository per interval.
const dependabotAccessRecheck = 24 * time.Hour

// dependabotProbesPerPass caps the probes one credential's pass makes, so a
// large tracked set is probed over several cycles rather than with a request
// for every repository at once. The repositories never probed go first, then
// the longest-standing answers.
const dependabotProbesPerPass = 25

// dependabotProbe records, per tracked repository, whether the org's GitHub
// credential can read its Dependabot alerts. One is made per org cycle, and
// each credential pass hands it the tracked repositories that credential
// reads. It reads the tracked set and the recorded answers once, on its first
// pass.
type dependabotProbe struct {
	m     *Manager
	orgID string
	host  string
	load  func() (map[string]dependabotTarget, error)
	// rateLimited ends the cycle's probes, as a rate limit ends the cycle's
	// refreshes: the next cycle takes up the repositories this one left.
	rateLimited bool
}

// dependabotTarget is a tracked repository and the answer recorded for it,
// if any.
type dependabotTarget struct {
	repositoryID string
	answer       *domain.RepositoryDependabotAccess
}

// newDependabotProbe returns the cycle's probe, or nil when the Manager holds
// no store to record answers in (tests and embedders that haven't wired it).
func (m *Manager) newDependabotProbe(ctx context.Context, orgID, host string) *dependabotProbe {
	if m.DependabotAccess == nil {
		return nil
	}
	return &dependabotProbe{
		m:     m,
		orgID: orgID,
		host:  host,
		load: sync.OnceValues(func() (map[string]dependabotTarget, error) {
			tracked, _, err := m.repos.ListTrackedSystem(ctx, orgID, host, db.Unwindowed)
			if err != nil {
				return nil, err
			}
			answers, err := m.DependabotAccess.ListSystem(ctx, orgID, host)
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
		}),
	}
}

// run probes the repositories of repos whose answer is missing or older than
// dependabotAccessRecheck, under client, and records each answer GitHub
// gives. A failed probe or write records nothing for that repository, and the
// next cycle tries it again. nil-safe.
func (p *dependabotProbe) run(ctx context.Context, client *ghclient.Client, repos []string) {
	if p == nil || p.rateLimited {
		return
	}
	targets, err := p.load()
	if err != nil {
		githubLog.WarnContext(ctx, "read dependabot access answers failed; none probed this cycle", "org", p.orgID, "error", err)
		return
	}

	now := time.Now()
	type due struct {
		name   string
		target dependabotTarget
	}
	var dueRepos []due
	for _, name := range repos {
		t, ok := targets[name]
		if !ok {
			continue
		}
		if t.answer != nil && now.Sub(t.answer.CheckedAt) < dependabotAccessRecheck {
			continue
		}
		dueRepos = append(dueRepos, due{name: name, target: t})
	}
	slices.SortStableFunc(dueRepos, func(a, b due) int {
		return checkedAt(a.target).Compare(checkedAt(b.target))
	})
	if len(dueRepos) > dependabotProbesPerPass {
		dueRepos = dueRepos[:dependabotProbesPerPass]
	}

	for _, d := range dueRepos {
		if ctx.Err() != nil {
			return
		}
		owner, repo, _ := strings.Cut(d.name, "/")
		access, err := client.DependabotAlertsAccess(ctx, owner, repo)
		var rl *ghclient.ErrRateLimited
		if errors.As(err, &rl) {
			p.rateLimited = true
			return
		}
		if err != nil {
			githubLog.Log(ctx, upstream.LogLevel(err, slog.LevelWarn), "dependabot alerts access probe failed", "org", p.orgID, "repo", d.name, "error", err)
			continue
		}
		stored, err := p.m.DependabotAccess.RecordSystem(ctx, p.orgID, d.target.repositoryID, access)
		if err != nil {
			githubLog.ErrorContext(ctx, "record dependabot alerts access failed", "org", p.orgID, "repo", d.name, "error", err)
			continue
		}
		targets[d.name] = dependabotTarget{repositoryID: d.target.repositoryID, answer: &stored}
		if d.target.answer == nil || d.target.answer.Access != access {
			githubLog.InfoContext(ctx, "dependabot alerts access", "org", p.orgID, "repo", d.name, "access", access)
		}
	}
}

// checkedAt is when the target's answer was recorded, zero when it has none,
// which orders a never-probed repository first.
func checkedAt(t dependabotTarget) time.Time {
	if t.answer == nil {
		return time.Time{}
	}
	return t.answer.CheckedAt
}
