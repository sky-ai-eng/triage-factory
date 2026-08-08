package routing

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// This file is the terminal-state reconciliation sweep: a periodic pass that
// enforces one invariant directly against stored state —
//
//	the stored snapshot is terminal  ⇒  the entity is closed and its active
//	tasks are closed
//
// It is the backstop for the close phase, not a second implementation of it.
// The close phase (close_relations.go) is event-driven and knows WHY a task
// should close; this knows only that a PR which shipped, or an issue that
// reached a done status, cannot still be open work. That is deliberately the
// smaller claim, because it is the one reconstructable from a snapshot.
//
// Why an event replay is not enough on its own. The close phase's obligation
// covers the case where the event is on the queue and a store call failed —
// retried in seconds. It cannot cover the cases where there is no row to
// retry: an event the tracker diffed but never published (the snapshot CAS
// commits before the publish, so a death in between loses the transition and
// the next cycle diffs MERGED against MERGED and produces nothing), a row
// parked 'failed' after its attempt budget, or a row orphaned 'processing' by
// a pod that was replaced rather than rebooted. In every one of those the
// entity stays 'active' forever: it is polled every cycle for a pull request
// that shipped months ago, its tasks sit in the team's queue with nothing left
// to clear them, and later events on it keep routing and minting new ones
// because the closed-entity gate reads a state that never flipped.
//
// Why this is not enough on its own either, and does not try to be: it
// reconciles the TERMINAL case only. A typed sibling close — "this
// ci_check_passed should have closed that ci_check_failed card" — is not
// reconstructable from snapshot state without re-implementing the diff engine
// as a reconciler, so it stays the close phase's job, where a fast replay is
// the right repair anyway (the entity's eventual terminal close clears every
// task regardless).

// DefaultReconcileInterval is how often the terminal-state sweep runs. Minutes,
// not seconds: this is a rare-divergence backstop, and every pass it makes on a
// healthy deployment reads nothing and writes nothing. The latency it sets is
// the repair time for a lost close, which is measured against "forever" — the
// status quo without it — not against the event queue's seconds.
const DefaultReconcileInterval = 5 * time.Minute

// reconcileBatchLimit caps how many divergent entities one org's pass repairs.
// A backlog larger than this (a long outage, a first run on a deployment that
// has been dropping closes) drains over successive passes rather than holding
// the sweep goroutine for an unbounded stretch; the candidate read is
// created_at ASC, so the oldest divergences go first and the batch is the same
// set each pass until it is repaired.
const reconcileBatchLimit = 200

// closeReasonReconciled is the close reason the sweep stamps on the tasks it
// closes. Deliberately distinct from the close phase's event-driven reasons
// ("auto_closed_by_event", "entity_closed"): those name an event that resolved
// the task, and this one had no event to name — it closed the task because the
// entity's stored state said the work was already over. Keeping them apart is
// what makes the audit trail honest about which mechanism fired, and readable
// as a signal that closes are being lost somewhere upstream.
const closeReasonReconciled = "reconciled"

// RunTerminalReconciler periodically repairs entities whose stored snapshot is
// terminal but whose row is still active, closing the entity and its active
// tasks. Per-org iteration mirrors RunDrainSweeper; returns when ctx is
// cancelled.
//
// Brain-gated in multi mode (started from the leader-elected background brain,
// like the drain sweeper), so exactly one process sweeps at a time. That is a
// courtesy rather than a correctness requirement — every write it makes is a
// guarded transition that no-ops on an already-repaired row — but two pods
// racing to close the same entity would burn duplicate reads for nothing.
func (r *Router) RunTerminalReconciler(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			orgIDs, err := r.orgs.ListActiveSystem(ctx)
			if err != nil {
				lifecycleLog.Error("terminal reconciler: list orgs failed", "error", err)
				continue
			}
			for _, orgID := range orgIDs {
				r.reconcileOrgTerminalEntities(ctx, orgID)
			}
		}
	}
}

// reconcileOrgTerminalEntities runs one org's pass. Factored out of
// RunTerminalReconciler so a per-org failure doesn't bail the whole cycle, and
// so tests can drive a single deterministic pass instead of racing a ticker.
func (r *Router) reconcileOrgTerminalEntities(ctx context.Context, orgID string) {
	doneByProject := r.jiraDoneStatusesByProject(ctx, orgID)
	candidates, err := r.entities.ListActiveTerminalCandidatesSystem(ctx, orgID, unionValues(doneByProject), reconcileBatchLimit)
	if err != nil {
		lifecycleLog.Error("terminal reconciler: list candidates failed", "org", orgID, "error", err)
		return
	}
	for i := range candidates {
		// Checked between entities and nowhere else, like the drain worker's
		// between-rows gate: a demoted holder should stop taking on new
		// repairs rather than push every remaining store call through a dead
		// context and log the failure. Each entity is independent, so a pass
		// that stops here leaves no half-state — the rest are still candidates
		// on the next holder's pass.
		if ctx.Err() != nil {
			return
		}
		r.reconcileTerminalEntity(ctx, orgID, candidates[i], doneByProject)
	}
}

// reconcileTerminalEntity closes one divergent entity and its active tasks.
//
// Tasks first, entity second, and the entity only when every task actually
// closed. The candidate read finds ACTIVE entities, so closing the entity is
// what removes this row from the sweep's own view: flipping it while a task
// close was still failing would hide the remaining tasks from the next pass
// and strand exactly what the sweep exists to clear. Same ordering, same
// reason, as the close phase's (see HandleEvent).
func (r *Router) reconcileTerminalEntity(ctx context.Context, orgID string, entity domain.Entity, doneByProject map[string][]string) {
	if !snapshotIsTerminal(entity, doneByProject) {
		// A Jira candidate whose status is done in some OTHER project. The
		// store's Jira filter is the flat union across projects; this is the
		// per-project recheck that keeps the union from over-closing.
		return
	}

	tasks, err := r.tasks.FindActiveByEntitySystem(ctx, orgID, entity.ID)
	if err != nil {
		lifecycleLog.Error("terminal reconciler: list active tasks failed", "entity_id", entity.ID, "org", orgID, "error", err)
		return
	}
	closed, failed := 0, false
	for _, t := range tasks {
		// No closing event id: this close is not event-driven, so there is
		// none to record — closeTaskWithAudit skips the task_events row and
		// close_event_type stays NULL, matching every other non-event close.
		if err := r.closeTaskWithAudit(ctx, orgID, t.ID, "", closeReasonReconciled, ""); err != nil {
			lifecycleLog.Error("terminal reconciler: close task failed", "task_id", t.ID, "entity_id", entity.ID, "org", orgID, "error", err)
			failed = true
			continue
		}
		closed++
	}
	if failed {
		// Collect-don't-bail, same as the close phase: the siblings that can
		// close still do. But the entity stays active so the next pass finds
		// this entity again and re-attempts what is left.
		if closed > 0 {
			r.broadcastTasksUpdated(orgID)
		}
		return
	}

	if err := r.entities.CloseSystem(ctx, orgID, entity.ID); err != nil {
		lifecycleLog.Error("terminal reconciler: close entity failed", "entity_id", entity.ID, "org", orgID, "error", err)
		return
	}
	// Worth a line every time: a repair here means a close was lost upstream,
	// which is the signal this sweep exists to make visible rather than absorb.
	lifecycleLog.InfoContext(ctx, "terminal reconciler: closed entity whose snapshot was already terminal",
		"entity_id", entity.ID, "source", entity.Source, "source_id", entity.SourceID, "org", orgID, "tasks_closed", closed)
	if closed > 0 {
		r.broadcastTasksUpdated(orgID)
	}
}

// snapshotIsTerminal answers "does this entity's stored snapshot say the work
// is over?" — the same question, and the same answer, the tracker asks at
// discovery time. GitHub reads it straight off the snapshot; Jira asks whether
// the status is in ITS OWN project's done set, which is why the per-project map
// is threaded here rather than collapsed to a union. An entity whose project
// has no configured rules is never terminal, matching the tracker's behavior
// for a project the user removed from settings while its entities were still
// active.
func snapshotIsTerminal(entity domain.Entity, doneByProject map[string][]string) bool {
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
		for _, done := range doneByProject[jiraProjectKey(entity.SourceID)] {
			if done == snap.Status {
				return true
			}
		}
		return false
	default:
		// Sources with no poller-owned terminal notion (Slack threads) are
		// not the sweep's business — the store never surfaces them either.
		return false
	}
}

// jiraDoneStatusesByProject reads the org's per-project done statuses, merged
// across every team that tracks the project. The merge is set-union in
// first-seen order, matching the poller's toTrackerJiraRules exactly: entities
// are org-shared (one row per issue no matter how many teams track its
// project), so any team's notion of "done" is what closed the shared entity in
// the first place, and the reconciler has to read terminality the same way the
// emitter wrote it.
//
// Nil store (test wiring that never passes one) or a read failure yields an
// empty map: no Jira entity is then treated as terminal, so the sweep degrades
// to GitHub-only rather than guessing at a status vocabulary it couldn't load.
func (r *Router) jiraDoneStatusesByProject(ctx context.Context, orgID string) map[string][]string {
	out := map[string][]string{}
	if r.jiraRules == nil {
		return out
	}
	rules, err := r.jiraRules.ListForOrgSystem(ctx, orgID)
	if err != nil {
		lifecycleLog.Error("terminal reconciler: list jira status rules failed, skipping jira this pass", "org", orgID, "error", err)
		return out
	}
	for _, rule := range rules {
		seen := map[string]bool{}
		for _, existing := range out[rule.ProjectKey] {
			seen[existing] = true
		}
		for _, done := range rule.DoneMembers {
			if done != "" && !seen[done] {
				seen[done] = true
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
func unionValues(byKey map[string][]string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(byKey))
	for _, values := range byKey {
		for _, v := range values {
			if !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	return out
}

// jiraProjectKey extracts the project key from a Jira issue key ("SKY-123" →
// "SKY"). Jira project keys contain no hyphen, so the first segment is the key.
func jiraProjectKey(issueKey string) string {
	if i := strings.IndexByte(issueKey, '-'); i >= 0 {
		return issueKey[:i]
	}
	return issueKey
}
