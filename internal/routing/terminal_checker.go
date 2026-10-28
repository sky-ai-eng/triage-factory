package routing

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
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

// TerminalInvariantGauges holds the last counts per org for the two
// observable gauges. Observable instruments are read on collection, not on
// write, so each pass records into the map and the callback reports whatever
// is current at scrape time.
//
// It has workmetrics.DepthObserver's lifecycle: the brain builds one on every
// acquisition and closes it on demotion, so a pod that lost the lease stops
// reporting instead of exporting its last counts beside the holder's.
type TerminalInvariantGauges struct {
	mu     sync.Mutex
	counts map[string]terminalCounts // org → its counts at the last pass that read them
	reg    metric.Registration
}

// terminalCounts is one org's Count A and Count B.
type terminalCounts struct {
	activeTerminal, openOnClosed int64
}

// NewTerminalInvariantGauges creates the two instruments and registers the
// callback that reports the per-org counts. A provider that cannot build
// an instrument (never the no-op provider) leaves the gauges unregistered
// and the checker still logs; instrument creation is not what the checker
// is for.
func NewTerminalInvariantGauges(provider metric.MeterProvider) *TerminalInvariantGauges {
	g := &TerminalInvariantGauges{counts: map[string]terminalCounts{}}
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
		for orgID, c := range g.counts {
			attrs := metric.WithAttributes(telemetry.OrgID(orgID))
			o.ObserveInt64(active, c.activeTerminal, attrs)
			o.ObserveInt64(open, c.openOnClosed, attrs)
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

// Close unregisters the gauge callback, so a closed gauge set stops
// reporting, and it has done so when it returns. See
// workmetrics.DepthObserver.Close for why the brain calls this itself on
// demotion rather than relying on RunTerminalInvariantChecker's deferred
// call. Safe to call more than once, and while the checker is still running.
func (g *TerminalInvariantGauges) Close() {
	g.mu.Lock()
	reg := g.reg
	g.reg = nil
	g.mu.Unlock()
	if reg != nil {
		if err := reg.Unregister(); err != nil {
			lifecycleLog.Error("terminal checker: unregister gauge callback failed", "error", err)
		}
	}
}

// record replaces the reported counts with one pass's. An org in failed
// keeps the counts it had, if it had any, because a zero the pass did not
// establish is worse than a stale number; every other org not in counts
// drops out.
func (g *TerminalInvariantGauges) record(counts map[string]terminalCounts, failed []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, orgID := range failed {
		if prev, ok := g.counts[orgID]; ok {
			counts[orgID] = prev
		}
	}
	g.counts = counts
}

// RunTerminalInvariantChecker periodically counts the entities the poll's
// enforcement should have closed and did not, and the tasks left open on
// closed entities, per org, logging each nonzero count and recording every
// count on g. Returns when ctx is cancelled, closing g for a caller that did
// not already.
//
// Brain-gated (started from the leader-elected background brain, like the
// firing worker), so exactly one process counts and reports at a time. The
// pass writes nothing, so two counting would be harmless; two reporting is
// not, because a demoted pod's last counts would sit beside the holder's
// with nothing to tell them apart. That is why g is the brain's to build
// and close rather than the router's.
func (r *Router) RunTerminalInvariantChecker(ctx context.Context, interval time.Duration, g *TerminalInvariantGauges) {
	defer g.Close()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.checkTerminalInvariant(ctx, g)
		}
	}
}

// checkTerminalInvariant runs one pass over every active org and records its
// counts on g, replacing the previous pass's: an org the list no longer
// returns (deleted, archived) stops reporting rather than holding its last
// count forever, and an org whose reads failed keeps its previous counts. A
// failed org list or a pass cut short by ctx records nothing, since neither
// established which orgs are active.
func (r *Router) checkTerminalInvariant(ctx context.Context, g *TerminalInvariantGauges) {
	orgIDs, err := r.orgs.ListActiveSystem(ctx)
	if err != nil {
		lifecycleLog.Error("terminal checker: list orgs failed", "error", err)
		return
	}
	counts := make(map[string]terminalCounts, len(orgIDs))
	var failed []string
	for _, orgID := range orgIDs {
		// Checked between orgs and nowhere else, like the drain worker's
		// between-rows gate: a demoted holder should stop taking on new
		// passes rather than push every remaining read through a dead
		// context and log the failure.
		if ctx.Err() != nil {
			return
		}
		activeTerminal, openOnClosed, ok := r.checkOrgTerminalInvariant(ctx, orgID)
		if !ok {
			failed = append(failed, orgID)
			continue
		}
		counts[orgID] = terminalCounts{activeTerminal: int64(activeTerminal), openOnClosed: int64(openOnClosed)}
	}
	g.record(counts, failed)
}

// checkOrgTerminalInvariant runs one org's reads and returns the two counts:
// Count A, active entities with a terminal snapshot past the grace and with
// no close in flight; Count B, open tasks on closed entities. Factored out of
// checkTerminalInvariant so a per-org failure doesn't bail the whole pass,
// and so tests can count a single org without gauges. A read failure on
// either count returns ok=false, and the pass keeps that org's previous
// counts rather than reporting a zero it did not establish.
func (r *Router) checkOrgTerminalInvariant(ctx context.Context, orgID string) (activeTerminal, openOnClosed int, ok bool) {
	doneByProject := r.jiraDoneStatusesByProject(ctx, orgID)
	doneByLinearTeam := r.linearDoneStatesByTeam(ctx, orgID)
	candidates, err := r.entities.ListActiveTerminalCandidatesSystem(ctx, orgID, unionValues(doneByProject), linearUnion(doneByLinearTeam), TerminalCheckGrace, 0)
	if err != nil {
		lifecycleLog.Error("terminal checker: list candidates failed", "org", orgID, "error", err)
		return 0, 0, false
	}
	var stranded []string
	for i := range candidates {
		// A Jira candidate whose status is done in some OTHER project (or a
		// Linear one done in another Linear team). The store's filters are
		// flat unions; this is the per-project / per-team recheck that keeps
		// a union from over-counting.
		if snapshotIsTerminal(candidates[i], doneByProject, doneByLinearTeam) {
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
// the status is in ITS OWN project's done set (the project the entity
// references, entities.jira_project_id), which is why the per-project map is
// threaded here rather than collapsed to a union; Linear asks the same of
// the entity's own Linear team (entities.linear_team_id). An entity whose
// project or team has no configured rules is never terminal, matching the
// tracker's behavior for one the user removed from settings while its
// entities were still active.
func snapshotIsTerminal(entity domain.Entity, doneByProject map[string][]domain.JiraStatusRef, doneByLinearTeam map[string][]domain.LinearStateRef) bool {
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
		return domain.ContainsStatus(doneByProject[entity.JiraProjectID], snap.StatusRef())
	case "linear":
		var snap domain.LinearSnapshot
		if err := json.Unmarshal([]byte(entity.SnapshotJSON), &snap); err != nil || snap.State.IsZero() {
			return false
		}
		return domain.ContainsState(doneByLinearTeam[entity.LinearTeamID], snap.StateRef())
	default:
		// Sources with no poller-owned terminal notion (Slack threads) are
		// not the checker's business — the store never surfaces them either.
		return false
	}
}

// jiraDoneStatusesByProject reads the org's per-project done statuses on its
// current Jira site, keyed by the project's jira_projects row — the one an
// issue's entity references (entities.jira_project_id), which a project key
// rename does not move — and merged across every team that tracks the
// project. The merge is set-union in
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
	settings, err := r.orgs.GetSettingsSystem(ctx, orgID)
	if err != nil {
		lifecycleLog.Error("terminal checker: read org settings for the jira site failed, skipping jira this pass", "org", orgID, "error", err)
		return out
	}
	rules, err := r.jiraRules.ListForOrgSystem(ctx, orgID, domain.EntityScope("jira", settings))
	if err != nil {
		lifecycleLog.Error("terminal checker: list jira status rules failed, skipping jira this pass", "org", orgID, "error", err)
		return out
	}
	for _, rule := range rules {
		seen := map[string]bool{}
		for _, existing := range out[rule.JiraProjectID] {
			seen[domain.JiraStatusDedupKey(existing)] = true
		}
		for _, done := range rule.DoneMembers {
			key := domain.JiraStatusDedupKey(done)
			if key != "" && !seen[key] {
				seen[key] = true
				out[rule.JiraProjectID] = append(out[rule.JiraProjectID], done)
			}
		}
	}
	return out
}

// linearDoneStatesByTeam is jiraDoneStatusesByProject for Linear: each Linear
// team's done states, set-unioned across every TF team that tracks it, in the
// order the poller's toTrackerLinearRules merges them, read from the org's
// current Linear workspace. Nil store or a read failure yields an empty map, so
// no Linear entity reads as terminal that pass.
func (r *Router) linearDoneStatesByTeam(ctx context.Context, orgID string) map[string][]domain.LinearStateRef {
	out := map[string][]domain.LinearStateRef{}
	if r.linearRules == nil {
		return out
	}
	settings, err := r.orgs.GetSettingsSystem(ctx, orgID)
	if err != nil {
		lifecycleLog.Error("terminal checker: read org settings for the linear workspace failed, skipping linear this pass", "org", orgID, "error", err)
		return out
	}
	rules, err := r.linearRules.ListForOrgSystem(ctx, orgID, domain.EntityScope("linear", settings))
	if err != nil {
		lifecycleLog.Error("terminal checker: list linear team rules failed, skipping linear this pass", "org", orgID, "error", err)
		return out
	}
	for _, rule := range rules {
		seen := map[string]bool{}
		for _, existing := range out[rule.LinearTeamID] {
			seen[domain.LinearStateDedupKey(existing)] = true
		}
		for _, done := range rule.DoneMembers {
			if key := domain.LinearStateDedupKey(done); !seen[key] {
				seen[key] = true
				out[rule.LinearTeamID] = append(out[rule.LinearTeamID], done)
			}
		}
	}
	return out
}

// linearUnion flattens the per-team done-state map into the deduplicated flat
// list the candidate read narrows on, the unionValues of Linear.
func linearUnion(byTeam map[string][]domain.LinearStateRef) []domain.LinearStateRef {
	seen := map[string]bool{}
	out := make([]domain.LinearStateRef, 0, len(byTeam))
	for _, values := range byTeam {
		for _, v := range values {
			if key := domain.LinearStateDedupKey(v); !seen[key] {
				seen[key] = true
				out = append(out, v)
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
