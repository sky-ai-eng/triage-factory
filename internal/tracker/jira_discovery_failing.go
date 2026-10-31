package tracker

import (
	"context"
	"slices"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// jiraDiscoveryDebounce is how many cycles in a row Jira must reject a
// project's discovery query before the teams arming it are told: fifteen
// minutes at the default poll interval. A rejection is Jira's answer about the
// query itself, so it seldom flaps, and three cycles covers a team that is
// already mid-edit — a status deleted in Jira while its rules are being
// changed to match — without leaving a project silent for long.
const jiraDiscoveryDebounce = 3

// jiraDiscoveryOutcome is what one cycle learned about one project's discovery.
// The values are ordered by how much they say: a project's outcome is the
// highest among its queries'.
type jiraDiscoveryOutcome int

const (
	// jiraDiscoveryAnswered: every query for the project returned, a salvaged
	// one included.
	jiraDiscoveryAnswered jiraDiscoveryOutcome = iota
	// jiraDiscoveryUnanswered: a query was rate-limited or never answered,
	// which is no evidence about the project either way.
	jiraDiscoveryUnanswered
	// jiraDiscoveryRejected: Jira rejected a query for the project, and the
	// salvage did not fix it.
	jiraDiscoveryRejected
)

func jiraDiscoveryOutcomeOf(err error) jiraDiscoveryOutcome {
	if err == nil {
		return jiraDiscoveryAnswered
	}
	if class, _ := upstream.ClassOf(err); class == upstream.Rejected {
		return jiraDiscoveryRejected
	}
	return jiraDiscoveryUnanswered
}

// JiraDiscoveryStreaks counts, per org and jira_projects row, the cycles in a
// row Jira has rejected the project's discovery. A Tracker lives for one
// cycle, so the poller keeps one of these for its lifetime and hands it to
// every Tracker it builds (WithJiraDiscoveryStreaks).
//
// It is memory only. A restart or a new lease holder starts every count at
// zero, which delays a raise and resolves nothing: a project still short of
// the debounce is kept by the Sync, not resolved, so an open occurrence
// survives until the count reaches it again. The zero value is ready to use.
type JiraDiscoveryStreaks struct {
	mu    sync.Mutex
	byOrg map[string]map[string]int
}

// observe records one cycle's outcome for each project the org polled, keyed
// by jira_projects row, and sorts them into what the Sync raises and what it
// keeps; an answered project is in neither, so its occurrence resolves. A
// project the cycle did not poll is forgotten.
func (s *JiraDiscoveryStreaks) observe(orgID string, outcomes map[string]jiraDiscoveryOutcome) (failing, keep []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.byOrg[orgID]
	next := map[string]int{}
	for id, outcome := range outcomes {
		switch outcome {
		case jiraDiscoveryRejected:
			next[id] = prev[id] + 1
			if next[id] >= jiraDiscoveryDebounce {
				failing = append(failing, id)
			} else {
				keep = append(keep, id)
			}
		case jiraDiscoveryUnanswered:
			if n := prev[id]; n > 0 {
				next[id] = n
			}
			keep = append(keep, id)
		}
	}
	s.reset(orgID, next)
	slices.Sort(failing)
	slices.Sort(keep)
	return failing, keep
}

func (s *JiraDiscoveryStreaks) forget(orgID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reset(orgID, nil)
}

func (s *JiraDiscoveryStreaks) reset(orgID string, counts map[string]int) {
	if len(counts) == 0 {
		delete(s.byOrg, orgID)
		return
	}
	if s.byOrg == nil {
		s.byOrg = map[string]map[string]int{}
	}
	s.byOrg[orgID] = counts
}

// WithJiraDiscoveryStreaks has the tracker count Jira discovery rejections in
// s, which outlives it, and returns the tracker.
func (t *Tracker) WithJiraDiscoveryStreaks(s *JiraDiscoveryStreaks) *Tracker {
	t.jiraStreaks = s
	return t
}

// discoveryStreaks is the tracker's counts: the shared ones when the poller
// handed them over, else the tracker's own.
func (t *Tracker) discoveryStreaks() *JiraDiscoveryStreaks {
	if t.jiraStreaks == nil {
		t.jiraStreaks = &JiraDiscoveryStreaks{}
	}
	return t.jiraStreaks
}

// NoJiraDiscovery is the cycle of an org with no armed project on its Jira
// site: no project's discovery can be failing, so every open
// jira.discovery_failing occurrence resolves and the counts start over.
func (t *Tracker) NoJiraDiscovery(ctx context.Context) {
	t.discoveryStreaks().forget(t.orgID)
	kinds.JiraDiscoveryFailing.Sync(ctx, t.orgID, nil)
}

// jiraProjectParams names each jira_projects row in ids with its key, for the
// notification's copy.
func jiraProjectParams(projects JiraRules, ids []string) []kinds.JiraProject {
	out := make([]kinds.JiraProject, 0, len(ids))
	for _, id := range ids {
		p := kinds.JiraProject{JiraProjectID: id}
		if rule := projects.ForProject(id); rule != nil {
			p.ProjectKey = rule.Key
		}
		out = append(out, p)
	}
	return out
}
