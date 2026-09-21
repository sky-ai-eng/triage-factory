package routing

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// This file is the terminal-state invariant checker: a periodic, READ-ONLY
// pass that counts violations of one invariant against stored state —
//
//	for every entity with a stored snapshot, state = 'closed' if and only if
//	the snapshot is terminal, at every commit boundary — except while a
//	terminating close for that entity is unsettled in the event queue
//
// — and records the counts on gauges. It repairs nothing. Enforcement lives
// in the poll: the tracker's Phase 3 writes state and snapshot in one
// statement, emits a close obligation for a terminal snapshot it finds on an
// entity still active, and the router closes from that obligation under the
// same guarded transaction a real merged/closed/completed transition takes.
// A periodic writer judging from a batch read is exactly what that design
// removed — it raced the live path (a reopen is two phases apart in a poll
// cycle, and a sweep firing between them cancelled work on a pull request
// someone had just reopened) — so this pass is deliberately incapable of
// writing to entities or tasks.
//
// Nonzero here means the poll's enforcement or the mint guard failed, or an
// entity nothing polls any more is carrying a terminal snapshot: worth an
// alarm, never a silent repair. Zero is the steady state.

// DefaultTerminalCheckInterval is how often the checker runs. Minutes, not
// seconds: on a healthy deployment every pass reads a handful of rows and
// records two zeros.
const DefaultTerminalCheckInterval = 5 * time.Minute

// TerminalCheckGrace is how long an active entity with a terminal snapshot
// may go unpolled before Count A counts it. The close obligation is emitted
// on the refresh AFTER the one that lost the close, so one poll cycle of lag
// is legitimate and not a violation; the grace is several cycles at any sane
// poll cadence. What outlives it is an entity no cycle visits — an untracked
// repo, a removed Jira project — which is exactly the case the count is for:
// untracking never destroys tasks (the dismiss affordance is the answer), so
// the entity is not enforced, and it IS counted.
const TerminalCheckGrace = 15 * time.Minute

// terminalCheckLogIDs caps how many ids a nonzero pass names in its log line.
const terminalCheckLogIDs = 20

// terminalInvariantGauges holds the last counts per org for the two
// observable gauges. Observable instruments are read on collection, not on
// write, so the checker records into these maps and the callback reports
// whatever is current at scrape time. Instruments are created once from the
// provider handed in — the global one in production, a manual-reader one in
// tests.
type terminalInvariantGauges struct {
	mu             sync.Mutex
	activeTerminal map[string]int64
	openOnClosed   map[string]int64
	reg            metric.Registration
}

// newTerminalInvariantGauges creates the two instruments and registers the
// callback that reports the per-org counts. A provider that cannot build
// an instrument (never the no-op provider) leaves the gauges unregistered
// and the checker still logs; instrument creation is not what the checker
// is for.
func newTerminalInvariantGauges(provider metric.MeterProvider) *terminalInvariantGauges {
	g := &terminalInvariantGauges{
		activeTerminal: map[string]int64{},
		openOnClosed:   map[string]int64{},
	}
	meter := provider.Meter("internal/routing")
	active, err := meter.Int64ObservableGauge("entity_terminal_active",
		metric.WithDescription("Active entities whose stored snapshot is terminal, past the poll grace, with no terminating close in flight"))
	if err != nil {
		lifecycleLog.Error("terminal checker: create entity_terminal_active gauge failed", "error", err)
		return g
	}
	open, err := meter.Int64ObservableGauge("tasks_open_on_closed_entity",
		metric.WithDescription("Open tasks whose entity is closed"))
	if err != nil {
		lifecycleLog.Error("terminal checker: create tasks_open_on_closed_entity gauge failed", "error", err)
		return g
	}
	reg, err := meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		g.mu.Lock()
		defer g.mu.Unlock()
		for orgID, n := range g.activeTerminal {
			o.ObserveInt64(active, n, metric.WithAttributes(telemetry.OrgID(orgID)))
		}
		for orgID, n := range g.openOnClosed {
			o.ObserveInt64(open, n, metric.WithAttributes(telemetry.OrgID(orgID)))
		}
		return nil
	}, active, open)
	if err != nil {
		lifecycleLog.Error("terminal checker: register gauge callback failed", "error", err)
		return g
	}
	g.reg = reg
	return g
}

func (g *terminalInvariantGauges) record(orgID string, activeTerminal, openOnClosed int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.activeTerminal[orgID] = int64(activeTerminal)
	g.openOnClosed[orgID] = int64(openOnClosed)
}

// gauges returns the router's gauge set, creating it from the global meter
// provider on first use. A test wires its own through setTerminalGauges
// before the first pass.
func (r *Router) gauges() *terminalInvariantGauges {
	r.terminalGaugesOnce.Do(func() {
		if r.terminalGauges == nil {
			r.terminalGauges = newTerminalInvariantGauges(otel.GetMeterProvider())
		}
	})
	return r.terminalGauges
}

// setTerminalGauges wires a gauge set built on a caller's meter provider, so
// a test can read the counts back through a manual reader. Call before the
// first pass; a later call is ignored.
func (r *Router) setTerminalGauges(g *terminalInvariantGauges) {
	r.terminalGaugesOnce.Do(func() { r.terminalGauges = g })
}

// RunTerminalInvariantChecker periodically counts the entities the poll's
// enforcement should have closed and did not, and the tasks left open on
// closed entities, per org, logging and recording each nonzero count.
// Returns when ctx is cancelled.
//
// Brain-gated in multi mode (started from the leader-elected background
// brain, like the firing worker), so exactly one process counts at a time:
// a courtesy rather than a correctness requirement, since the pass writes
// nothing, but two pods reporting the same org's gauge would only race each
// other for the same number.
func (r *Router) RunTerminalInvariantChecker(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			orgIDs, err := r.orgs.ListActiveSystem(ctx)
			if err != nil {
				lifecycleLog.Error("terminal checker: list orgs failed", "error", err)
				continue
			}
			for _, orgID := range orgIDs {
				// Checked between orgs and nowhere else, like the drain
				// worker's between-rows gate: a demoted holder should stop
				// taking on new passes rather than push every remaining read
				// through a dead context and log the failure.
				if ctx.Err() != nil {
					return
				}
				r.checkOrgTerminalInvariant(ctx, orgID)
			}
		}
	}
}

// checkOrgTerminalInvariant runs one org's pass and returns the two counts:
// Count A, active entities with a terminal snapshot past the grace and with
// no close in flight; Count B, open tasks on closed entities. Factored out of
// RunTerminalInvariantChecker so a per-org failure doesn't bail the whole
// cycle, and so tests can drive a single deterministic pass instead of
// racing a ticker. A read failure on either count leaves that gauge at its
// previous value rather than reporting a zero the pass did not establish.
func (r *Router) checkOrgTerminalInvariant(ctx context.Context, orgID string) (activeTerminal, openOnClosed int, ok bool) {
	doneByProject := r.jiraDoneStatusesByProject(ctx, orgID)
	candidates, err := r.entities.ListActiveTerminalCandidatesSystem(ctx, orgID, unionValues(doneByProject), TerminalCheckGrace, 0)
	if err != nil {
		lifecycleLog.Error("terminal checker: list candidates failed", "org", orgID, "error", err)
		return 0, 0, false
	}
	var stranded []string
	for i := range candidates {
		// A Jira candidate whose status is done in some OTHER project. The
		// store's Jira filter is the flat union across projects; this is the
		// per-project recheck that keeps the union from over-counting.
		if snapshotIsTerminal(candidates[i], doneByProject) {
			stranded = append(stranded, candidates[i].ID)
		}
	}
	openTasks, err := r.tasks.ListOpenOnClosedEntitiesSystem(ctx, orgID)
	if err != nil {
		lifecycleLog.Error("terminal checker: list open tasks on closed entities failed", "org", orgID, "error", err)
		return 0, 0, false
	}

	if len(stranded) > 0 {
		lifecycleLog.WarnContext(ctx, "terminal checker: active entities carry a terminal snapshot with no close in flight; the poll's enforcement did not reach them",
			"org", orgID, "count", len(stranded), "entity_ids", capIDs(stranded))
	}
	if len(openTasks) > 0 {
		lifecycleLog.WarnContext(ctx, "terminal checker: open tasks on closed entities; a close missed them or a mint bypassed the guard",
			"org", orgID, "count", len(openTasks), "task_ids", capIDs(openTasks))
	}
	r.gauges().record(orgID, len(stranded), len(openTasks))
	return len(stranded), len(openTasks), true
}

// capIDs renders at most terminalCheckLogIDs ids for a log line.
func capIDs(ids []string) []string {
	if len(ids) > terminalCheckLogIDs {
		return ids[:terminalCheckLogIDs]
	}
	return ids
}

// snapshotIsTerminal answers "does this entity's stored snapshot say the work
// is over?" — the same question, and the same answer, the tracker asks at
// discovery time. GitHub reads it straight off the snapshot; Jira asks whether
// the status is in ITS OWN project's done set, which is why the per-project map
// is threaded here rather than collapsed to a union. An entity whose project
// has no configured rules is never terminal, matching the tracker's behavior
// for a project the user removed from settings while its entities were still
// active.
func snapshotIsTerminal(entity domain.Entity, doneByProject map[string][]domain.JiraStatusRef) bool {
	switch entity.Source {
	case "github":
		var snap domain.PRSnapshot
		if err := json.Unmarshal([]byte(entity.SnapshotJSON), &snap); err != nil {
			return false
		}
		return snap.Merged || snap.State == "CLOSED" || snap.State == "MERGED"
	case "jira":
		var snap domain.JiraSnapshot
		if err := json.Unmarshal([]byte(entity.SnapshotJSON), &snap); err != nil || snap.Status == "" {
			return false
		}
		return domain.ContainsStatus(doneByProject[jiraProjectKey(entity.SourceID)], snap.StatusRef())
	default:
		// Sources with no poller-owned terminal notion (Slack threads) are
		// not the checker's business — the store never surfaces them either.
		return false
	}
}

// jiraDoneStatusesByProject reads the org's per-project done statuses, merged
// across every team that tracks the project. The merge is set-union in
// first-seen order, matching the poller's toTrackerJiraRules exactly: entities
// are org-shared (one row per issue no matter how many teams track its
// project), so any team's notion of "done" is what the poll would have closed
// the shared entity on, and the checker has to read terminality the same way.
//
// Nil store (test wiring that never passes one) or a read failure yields an
// empty map: no Jira entity is then treated as terminal, so the pass degrades
// to GitHub-only rather than guessing at a status vocabulary it couldn't load.
func (r *Router) jiraDoneStatusesByProject(ctx context.Context, orgID string) map[string][]domain.JiraStatusRef {
	out := map[string][]domain.JiraStatusRef{}
	if r.jiraRules == nil {
		return out
	}
	rules, err := r.jiraRules.ListForOrgSystem(ctx, orgID)
	if err != nil {
		lifecycleLog.Error("terminal checker: list jira status rules failed, skipping jira this pass", "org", orgID, "error", err)
		return out
	}
	for _, rule := range rules {
		seen := map[string]bool{}
		for _, existing := range out[rule.ProjectKey] {
			seen[domain.JiraStatusDedupKey(existing)] = true
		}
		for _, done := range rule.DoneMembers {
			key := domain.JiraStatusDedupKey(done)
			if key != "" && !seen[key] {
				seen[key] = true
				out[rule.ProjectKey] = append(out[rule.ProjectKey], done)
			}
		}
	}
	return out
}

// unionValues flattens the per-project done-status map into the deduplicated
// flat list the candidate read narrows on. It is only ever a superset filter —
// snapshotIsTerminal re-checks each row against its own project — so collapsing
// the per-project structure here is safe in the one direction it is used.
func unionValues(byKey map[string][]domain.JiraStatusRef) []domain.JiraStatusRef {
	seen := map[string]bool{}
	out := make([]domain.JiraStatusRef, 0, len(byKey))
	for _, values := range byKey {
		for _, v := range values {
			key := domain.JiraStatusDedupKey(v)
			if key != "" && !seen[key] {
				seen[key] = true
				out = append(out, v)
			}
		}
	}
	return out
}

// jiraProjectKey extracts the project key from a Jira issue key ("PROJ-123" →
// "PROJ"). Jira project keys contain no hyphen, so the first segment is the key.
func jiraProjectKey(issueKey string) string {
	if i := strings.IndexByte(issueKey, '-'); i >= 0 {
		return issueKey[:i]
	}
	return issueKey
}
