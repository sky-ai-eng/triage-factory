package routing

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// tryAutoDelegate decides whether a matched (task, trigger) fires now or
// queues. Order of checks: breaker (per-(entity,prompt)) → entity gate
// (per-entity, auto-only) → fire or enqueue.
//
// Breaker is a hard skip — a tripped breaker means the user has work to
// investigate before more runs land on this entity-prompt pair. Queueing
// past it would just stack stale firings the user didn't ask for.
//
// Entity gate is the per-entity serialization point: at most one auto run
// in flight per entity, regardless of which task/trigger it came from. If
// the gate is closed (active auto run, or older firings already queued
// for FIFO fairness), the firing enqueues onto pending_firings instead of
// being dropped silently.
func (r *Router) tryAutoDelegate(orgID string, task *domain.Task, trigger domain.EventHandler, entityID string, triggeringEventID string, actingTeamID string) {
	// Exclusive claim: one task, one owner. If the bot has already
	// claimed this task on behalf of a different team (an earlier
	// matched team won the CAS), this team's trigger must not pile on a
	// second run against the same situation — that is the cross-team
	// duplication the one-task model exists to prevent. A trigger whose
	// acting team IS the current owner still proceeds, so multiple
	// prompts one team configured on the same event all run.
	if task.ClaimedByAgentID != "" && actingTeamID != "" && teamIDValue(task) != actingTeamID {
		log.Printf("[router] auto-trigger skipped on task %s: already claimed by the bot for team %s (acting team %s lost the claim)", task.ID, teamIDValue(task), actingTeamID)
		return
	}
	// SKY-261 bot-disabled-team gate. If the task's team has the bot
	// turned off in team_agents.enabled, the auto-trigger is a no-op
	// — the task is already in the team queue (created by HandleEvent
	// upstream); a human will swipe-delegate later if they want a
	// run. Skip silently rather than firing on a disabled team.
	//
	// The gate requires BOTH stores (agents to resolve the org's
	// agent, teamAgents to read the enabled flag). If either is nil
	// — pre-D-Claims test wiring that didn't thread the new stores
	// — the gate degrades to "bot enabled check unavailable, proceed
	// with auto-fire," which preserves the pre-SKY-261 behavior.
	// Production server.New / main.go always passes both.
	if r.teamAgents != nil && r.agents != nil {
		a, err := r.agents.GetForOrgSystem(context.Background(), orgID)
		if err != nil {
			log.Printf("[router] auto-trigger skipped: agent lookup: %v", err)
			return
		}
		if a == nil {
			// No bootstrapped agent — bootstrap is now fatal at
			// startup, so this shouldn't reach us in practice.
			// Log + bail rather than crashing the goroutine.
			log.Printf("[router] auto-trigger skipped on task %s: no agent bootstrapped", task.ID)
			return
		}
		// Read the bot-enabled flag for the FIRING team — the team
		// whose trigger routed the bot here — not the task's owner
		// team. One task is now visible to many teams; the gate must
		// read the acting team's own team_agents row so a two-team org
		// where team B disabled the bot doesn't auto-fire on team B by
		// reading team A's flag. Fall back to the task's owner team,
		// then the local sentinel, when the caller didn't supply one.
		teamID := actingTeamID
		if teamID == "" {
			teamID = teamIDValue(task)
		}
		if teamID == "" {
			teamID = runmode.LocalDefaultTeamID
		}
		ta, err := r.teamAgents.GetForTeamSystem(context.Background(), orgID, teamID, a.ID)
		if err != nil {
			log.Printf("[router] auto-trigger skipped on task %s: team_agents lookup: %v", task.ID, err)
			return
		}
		if ta == nil || !ta.Enabled {
			log.Printf("[router] auto-trigger skipped on task %s: bot disabled for team", task.ID)
			return
		}
	}
	// Breaker gate. trigger.BreakerThreshold is *int because the column
	// is nullable at the schema level (rule rows have NULL); kind='trigger'
	// rows are guaranteed non-nil by the per-kind CHECK constraint. The
	// breaker keys on the blueprint's first step prompt (runs are prompt-
	// keyed; for the 1-step blueprints every shipped trigger uses, that is
	// the wrapped prompt — identical to the pre-blueprint behavior).
	breakerThreshold := derefIntDefault(trigger.BreakerThreshold, 0)
	breakerPromptID := r.breakerPromptID(orgID, trigger.BlueprintID)
	failures, err := r.tasks.CountConsecutiveFailedRunsSystem(context.Background(), orgID, entityID, breakerPromptID)
	if err != nil {
		log.Printf("[router] breaker query error for entity %s prompt %s: %v", entityID, breakerPromptID, err)
		return
	}
	if failures >= breakerThreshold {
		log.Printf("[router] breaker tripped for entity %s prompt %s (%d >= %d)",
			entityID, breakerPromptID, failures, breakerThreshold)
		// Look up prompt name for the toast — opportunistic, falls back to a
		// generic message if the lookup fails since the breaker trip itself
		// is the load-bearing signal. One toast per trip (happens rarely).
		promptName := ""
		if p, perr := r.prompts.GetSystem(context.Background(), orgID, breakerPromptID); perr == nil && p != nil {
			promptName = p.Name
		}
		if promptName == "" {
			promptName = "prompt"
		}
		toast.Warning(r.ws, orgID, fmt.Sprintf("Auto-delegation paused: %s tripped the breaker (%d consecutive failures on this entity)", promptName, failures))
		return
	}

	// Per-entity gate. Closed if any auto run is active on the entity OR
	// any pending_firings rows are already queued (FIFO fairness).
	// Compose the per-entity firing gate from its two halves:
	// AgentRunStore owns the runs-shaped predicate, PendingFiringsStore
	// owns the queue-shaped one. canFire = neither side blocks.
	gateCtx := context.Background()
	hasActive, err := r.agentRuns.HasActiveAutoRunForEntitySystem(gateCtx, orgID, entityID)
	if err != nil {
		log.Printf("[router] entity gate active-run query error for %s: %v", entityID, err)
		return
	}
	hasPending := false
	if !hasActive {
		hasPending, err = r.firings.HasPendingForEntity(gateCtx, orgID, entityID)
		if err != nil {
			log.Printf("[router] entity gate pending query error for %s: %v", entityID, err)
			return
		}
	}
	if hasActive || hasPending {
		// System-actor firing rows have no human author. Empty user
		// here lets the Postgres impl's COALESCE walk to the org-
		// owner fallback (creator_user_id is NOT NULL but the table
		// has no separate "actor" column); SQLite ignores the column
		// entirely.
		inserted, err := r.firings.Enqueue(context.Background(), orgID, "", entityID, task.ID, trigger.ID, triggeringEventID)
		if err != nil {
			log.Printf("[router] enqueue firing failed (entity %s task %s trigger %s): %v",
				entityID, task.ID, trigger.ID, err)
			return
		}
		if inserted {
			log.Printf("[router] queued firing on entity %s (task %s, trigger %s) — entity busy",
				entityID, task.ID, trigger.ID)
			// SKY-261 D-Claims: pending firing landed in the queue,
			// commit the task to the org's agent. Stamp here (after
			// EnqueuePendingFiring succeeded) so a failed enqueue
			// doesn't leave a phantom claim on an otherwise queued
			// task. !inserted means a duplicate firing already in the
			// queue — the original enqueue already stamped, no re-
			// stamp needed.
			r.stampAgentClaim(orgID, task, actingTeamID)
		} else {
			log.Printf("[router] firing collapsed on entity %s (task %s, trigger %s) — duplicate already queued",
				entityID, task.ID, trigger.ID)
		}
		return
	}

	// Consolidate the owner team to the acting team BEFORE firing. An
	// auto-fired run inherits runs.team_id from tasks.team_id at insert,
	// and the claim (which also consolidates the owner) lands only after
	// the fire succeeds — so without this, a run fired by a team other
	// than the creation-time owner (e.g. the owner had auto-delegation
	// disabled and a lower-priority team is firing) would be attributed
	// to the stale owner. Owner-only update, no claim touch: if the fire
	// fails the task is owned by the team that tried, unclaimed — not a
	// phantom claim. Skipped when the acting team already is the owner
	// (the common path, and same-team multi-prompt where an active run
	// may exist).
	if actingTeamID != "" && actingTeamID != teamIDValue(task) {
		if err := r.tasks.SetOwnerTeamSystem(context.Background(), orgID, task.ID, actingTeamID); err != nil {
			log.Printf("[router] failed to consolidate owner team on task %s before fire: %v", task.ID, err)
		} else {
			task.TeamID = teamIDPtr(actingTeamID)
		}
	}
	if _, err := r.fireDelegate(orgID, task, trigger, triggeringEventID); err != nil {
		// SKY-424: a replayed event (at-least-once queue) whose first run
		// already committed hits the (event, trigger) fence and comes back
		// as ErrAlreadyFired. Clean skip — the original run + its claim
		// stand, so we must NOT re-stamp the claim or log an error.
		if errors.Is(err, delegate.ErrAlreadyFired) {
			log.Printf("[router] auto-delegate skipped for task %s (trigger %s): event %s already fired this trigger (replay)",
				task.ID, trigger.ID, triggeringEventID)
			return
		}
		log.Printf("[router] fire failed for task %s (trigger %s): %v", task.ID, trigger.ID, err)
		return
	}
	// SKY-261 D-Claims: fireDelegate succeeded (run inserted + spawner
	// goroutine launched) — stamp the agent claim. Done AFTER success
	// so a fireDelegate that fails + reverts to status='queued'
	// doesn't leave a phantom bot claim on a task that's back in the
	// human-triage queue.
	r.stampAgentClaim(orgID, task, actingTeamID)
}

// stampAgentClaim writes claimed_by_agent_id on a task using the org's
// agent row AND broadcasts the SKY-261 B+ task_claimed event so
// listeners (Board) can re-render the per-claim lanes. Called from
// the two commitment points in tryAutoDelegate (post-fireDelegate
// success, post-EnqueuePendingFiring success). Both paths converge
// on "the bot has committed to this task." Nil-safe on r.agents and
// on GetForOrg returning (nil, nil) — both leave the claim
// unstamped rather than crashing, which matches the "transient seam
// between db init and agent bootstrap" case noted in §4 of the spec.
//
// Uses StampAgentClaimIfUnclaimed (race-safe variant) instead of the
// unconditional setter: if a user claims the same task while the
// trigger is mid-fire, the user-claim wins and the bot silently
// loses the race. The drain path's claim_changed guard then skips
// any pending firing for this task on the next pop — so the auto-
// trigger commitment never lands, which is the right outcome when
// the human said "I'll handle this." Same-agent rewrites also
// short-circuit here, avoiding redundant task_claimed broadcasts on
// flows that call stampAgentClaim twice in quick succession.
func (r *Router) stampAgentClaim(orgID string, task *domain.Task, actingTeamID string) {
	if r.agents == nil {
		return
	}
	a, err := r.agents.GetForOrgSystem(context.Background(), orgID)
	if err != nil {
		log.Printf("[router] agent lookup failed for task %s claim stamp: %v", task.ID, err)
		return
	}
	if a == nil {
		return
	}
	ok, err := r.tasks.StampAgentClaimIfUnclaimedSystem(context.Background(), orgID, task.ID, a.ID, actingTeamID)
	if err != nil {
		log.Printf("[router] failed to stamp agent claim on task %s: %v", task.ID, err)
		return
	}
	if !ok {
		// StampAgentClaimIfUnclaimed returns ok=false for any of
		// three reasons: a user beat the bot to the claim (don't
		// steal), the bot already owns it (idempotent no-op), or
		// the task is terminal (done/dismissed — sticky claim past
		// close, refuse new mutations). All three legitimately
		// produce "skip the broadcast"; we don't disambiguate at
		// the log level because the helper doesn't surface the
		// reason and re-reading just to log is wasted I/O. If a
		// future debug session needs the breakdown, the helper can
		// grow a tri-state return or the caller can re-read the
		// task — neither is load-bearing for correctness.
		log.Printf("[router] agent claim stamp on task %s was a no-op (user owns it, bot already owns it, or task is terminal)", task.ID)
		return
	}
	task.ClaimedByAgentID = a.ID
	if actingTeamID != "" {
		// Mirror the store's owner-consolidation so the shared task
		// object reflects the new owning team for later iterations.
		task.TeamID = teamIDPtr(actingTeamID)
	}
	r.ws.Broadcast(websocket.Event{
		Type:  "task_claimed",
		OrgID: orgID,
		Data: map[string]any{
			"task_id":             task.ID,
			"claimed_by_agent_id": a.ID,
			"claimed_by_user_id":  "",
		},
	})
}

// fireDelegate transitions the task to delegated status, broadcasts the
// change, then fires the spawner. Returns the run ID on success — used by
// DrainEntity to record which run a queued firing materialized into.
//
// triggeringEventID is the event instance driving this fire (SKY-424):
// the immediate path passes tryAutoDelegate's event id, the drain path
// passes the pending firing's. It threads into DelegateOpts so the run
// insert is fenced on (triggering_event_id, trigger_id); a replayed event
// whose first run already committed surfaces as delegate.ErrAlreadyFired,
// which both callers treat as a clean skip rather than a duplicate fire.
func (r *Router) fireDelegate(orgID string, task *domain.Task, trigger domain.EventHandler, triggeringEventID string) (string, error) {
	if r.spawner == nil {
		return "", fmt.Errorf("spawner not configured")
	}

	// SKY-261 B+: no status flip here. Pre-SKY-261 we transitioned to
	// status='delegated' for UI feedback + dedup. Post-B+ the
	// responsibility axis is the claim columns: stampAgentClaim
	// (called by the caller on fireDelegate success) writes
	// claimed_by_agent_id and broadcasts task_claimed, which is what
	// the Board now listens for. Status stays 'queued' until a
	// genuine lifecycle move (done / dismissed / snoozed). Dedup is
	// unaffected — the partial unique index gates on status NOT IN
	// ('done', 'dismissed'), so a queued+claimed task still matches.
	log.Printf("[router] auto-delegating task %s (trigger %s, blueprint %s)",
		task.ID, trigger.ID, trigger.BlueprintID)

	// Re-read task to get entity-joined display fields the spawner needs.
	fresh, err := r.tasks.GetSystem(context.Background(), orgID, task.ID)
	if err != nil || fresh == nil {
		if err != nil {
			return "", fmt.Errorf("re-read task: %w", err)
		}
		return "", fmt.Errorf("task %s disappeared before spawn", task.ID)
	}

	runID, err := r.spawner.Delegate(*fresh, delegate.DelegateOpts{
		OrgID:               orgID,
		ExplicitBlueprintID: trigger.BlueprintID,
		TriggerType:         "event",
		TriggerID:           trigger.ID,
		TriggeringEventID:   triggeringEventID,
	})
	if err != nil {
		// Post-B+: nothing to revert status-wise (status stayed 'queued').
		// stampAgentClaim hasn't run yet either — the caller only calls
		// it after fireDelegate returns nil. So this failure leaves the
		// task in a clean unclaimed-queued state, which is correct.
		return "", err
	}
	log.Printf("[router] started run %s for task %s", runID, task.ID)
	return runID, nil
}

// DrainEntity is the spawner's hook into the per-entity firing queue.
// Called when an auto run terminates on the entity (any terminal status,
// including pending_approval per the SKY-189 design — pending_approval
// releases the entity lock so user deliberation doesn't block downstream
// processing).
//
// Pops pending firings in FIFO order, validates each against current
// state (task still active? trigger still enabled? breaker still under
// threshold?), and fires the first valid one. Stale firings are
// soft-deleted with a skip_reason and the loop continues. At most one
// firing actually fires per drain — that run becomes the new in-flight
// for the entity and gates further drains naturally.
func (r *Router) DrainEntity(orgID, entityID string) {
	// Serialize drains per entity. Without this, a fast-terminating run
	// fired by an earlier drain can spawn a second DrainEntity goroutine
	// that pops the same pending_firings row before the first drain
	// transitions it out of 'pending' — leading to duplicate fireDelegate
	// calls. The MarkPendingFiringFired/Skipped guards on
	// status='pending' protect the row's own mutation but cannot un-fire
	// the duplicate run. This mutex closes the window: the second drain
	// blocks until the first releases, by which point the firing has
	// landed in a terminal status and the second drain's pop returns the
	// next row (or nothing).
	mu := r.entityDrainLock(entityID)
	mu.Lock()
	defer mu.Unlock()

	for {
		firing, err := r.firings.PopForEntity(context.Background(), orgID, entityID)
		if err != nil {
			log.Printf("[router] drain pop error for entity %s: %v", entityID, err)
			return
		}
		if firing == nil {
			return // queue empty
		}

		runID, skipReason, transientErr := r.attemptDrainOne(orgID, firing)
		if transientErr != nil {
			// Transient failure (DB read, Delegate). Leave the firing in
			// 'pending' state and bail the drain loop — marking
			// 'skipped_stale' here would permanently drop a queued intent
			// over a temporary problem. The periodic sweeper or the next
			// run-terminal will retry.
			log.Printf("[router] drain transient error on firing %d (entity %s): %v — leaving pending for retry",
				firing.ID, entityID, transientErr)
			return
		}
		if runID != "" {
			if err := r.firings.MarkFired(context.Background(), orgID, firing.ID, runID); err != nil {
				// Durability race: the run was created (side-effect
				// committed inside the spawner goroutine) but the UPDATE
				// that records the firing→run association failed. The
				// firing row is still 'pending' — a later DrainEntity
				// would pop the same row and fire again, duplicating.
				//
				// Roll the side-effect chain back in reverse: Cancel the
				// run we just spawned, then revert the task to 'queued'
				// so the limbo state (task=delegated + no live run) is
				// not externally visible. Mirrors what fireDelegate
				// already does when spawner.Delegate itself fails. The
				// firing stays 'pending' and the next drain re-fires
				// fresh; until then the task reads as queued, which is
				// honest.
				log.Printf("[router] mark firing %d fired (run %s) failed: %v — rolling back: cancelling run + reverting task to queued",
					firing.ID, runID, err)
				if r.spawner != nil {
					if cerr := r.spawner.Cancel(orgID, runID, ""); cerr != nil {
						log.Printf("[router] cancel run %s after mark-fired failure: %v — run may already be terminal, drain still triggers from its defer",
							runID, cerr)
					}
				}
				r.revertTaskStatus(orgID, firing.TaskID, "queued")
			}
			return // one fire per drain — the new run gates the rest
		}
		// Skipped or fire failed; record reason and continue draining.
		if err := r.firings.MarkSkipped(context.Background(), orgID, firing.ID, skipReason); err != nil {
			log.Printf("[router] mark firing %d skipped (%s): %v", firing.ID, skipReason, err)
			return
		}
		log.Printf("[router] skipped firing %d on entity %s: %s", firing.ID, entityID, skipReason)
	}
}

// RunDrainSweeper periodically attempts to drain every entity that has at
// least one pending firing. The sweeper is the safety net for stuck
// queues: a firing left in 'pending' after a transient validation/fire
// error needs *some* drain to retry it, and the natural trigger
// (notifyDrainer from an auto-run terminal) only fires when an auto run
// is actively terminating. If nothing's terminating — entity has no
// active runs and no events arrive — the queue would otherwise sit
// indefinitely.
//
// Cadence is 30s by default; tuneable via interval. Each tick lists
// entities with pending firings (cheap — partial index) and calls
// DrainEntity on each. DrainEntity's per-entity mutex makes the sweeper
// safe to run alongside event-triggered drains: if a drain is already
// running for an entity, the sweeper's call blocks then re-pops, which
// is fine. Empty queues are no-ops.
//
// Returns when ctx is cancelled. Caller is responsible for the lifetime
// (typically a goroutine started from main, cancelled at shutdown).
func (r *Router) RunDrainSweeper(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Per-org iteration mirrors the established poller /
			// classifier pattern. In local mode this collapses to N=1
			// over the sentinel org (the only row in orgs); in multi
			// mode it fans across every active tenant. OrgsStore is
			// a required NewRouter parameter — if it were nil the
			// dereference below would panic, which is the right
			// behavior for a wiring bug at startup.
			orgIDs, err := r.orgs.ListActiveSystem(ctx)
			if err != nil {
				log.Printf("[router] drain sweeper: list orgs error: %v", err)
				continue
			}
			for _, orgID := range orgIDs {
				r.sweepOrg(ctx, orgID)
			}
		}
	}
}

// sweepOrg drains every entity in a single org that has at least one
// pending firing. Factored out of RunDrainSweeper so the per-org loop
// reads as one statement and per-org errors don't bail the whole
// cycle.
func (r *Router) sweepOrg(ctx context.Context, orgID string) {
	ids, err := r.firings.ListEntitiesWithPending(ctx, orgID)
	if err != nil {
		log.Printf("[router] drain sweeper: list error for org %s: %v", orgID, err)
		return
	}
	for _, eid := range ids {
		active, err := r.agentRuns.HasActiveAutoRunForEntitySystem(ctx, orgID, eid)
		if err != nil {
			log.Printf("[router] drain sweeper: active-check error for %s (org %s): %v", eid, orgID, err)
			continue
		}
		if active {
			continue
		}
		r.DrainEntity(orgID, eid)
	}
}

// attemptDrainOne validates a popped firing against current state and
// fires it if everything still holds. Three outcomes:
//
//   - (runID, "", nil)         — fire succeeded; caller marks 'fired'.
//   - ("", skipReason, nil)    — definitive "no longer relevant"; caller
//     marks 'skipped_stale'. Reserved for: task_closed (done /
//     dismissed / snoozed — task isn't drain-eligible on the
//     lifecycle axis), trigger_disabled, breaker_tripped,
//     claim_changed (SKY-261 B+: a user took the task over or
//     requeued it after the firing was enqueued, so the bot's
//     original commitment is no longer current; drainer must not
//     fire a phantom bot run against a now-user-claimed task).
//   - ("", "", err)            — transient failure (DB read, fire-time);
//     caller leaves the firing in 'pending' state and bails the drain
//     loop. The periodic sweeper or next run-terminal will retry.
//
// Validation reads from live tables, not from the firing row, so the
// drainer reflects the world *now* — invalidation falls out for free
// from the close cascade and trigger config.
//
// We classify Delegate errors as transient too: even when spawner.Delegate
// refuses (rate-limited GitHub, missing creds, worktree race), the firing
// intent is still valid and worth retrying. The breaker handles the
// "actually broken, repeated failure" case via run-level failure counts —
// but only once we've started enough runs to trip it. Until then, retry.
func (r *Router) attemptDrainOne(orgID string, firing *domain.PendingFiring) (runID, skipReason string, transientErr error) {
	task, err := r.tasks.GetSystem(context.Background(), orgID, firing.TaskID)
	if err != nil {
		return "", "", fmt.Errorf("task lookup: %w", err)
	}
	// SKY-261 B+: status='snoozed' belongs on the lifecycle-skip axis,
	// not the claim axis. A bot-claimed task that gets snoozed (e.g.,
	// the user said "wait until Tuesday") shouldn't fire a queued
	// drain when the entity slot opens — the snooze itself is a "do
	// not act" signal on the lifecycle axis. Grouped with
	// done/dismissed under task_closed because all three mean "the
	// task is not currently drain-eligible." A snooze wake-on-bump
	// will create a NEW event → new firing if the trigger still
	// matches; the deferred firing is the wrong path to wake it.
	if task == nil || task.Status == "done" || task.Status == "dismissed" || task.Status == "snoozed" {
		return "", domain.PendingFiringSkipTaskClosed, nil
	}

	// SKY-261 B+: drain only fires if the bot's claim still holds.
	// User-claim (claimed_by_user_id set) or requeue (both cleared)
	// invalidates the original commitment. Without this check, a
	// pending firing would fire even after the user explicitly took
	// the task over, producing a phantom bot run on a now-user-
	// claimed task.
	if task.ClaimedByAgentID == "" {
		return "", domain.PendingFiringSkipClaimChanged, nil
	}

	trigger, err := r.handlers.GetSystem(context.Background(), orgID, firing.TriggerID)
	if err != nil {
		return "", "", fmt.Errorf("trigger lookup: %w", err)
	}
	if trigger == nil || trigger.Kind != domain.EventHandlerKindTrigger || !trigger.Enabled {
		return "", domain.PendingFiringSkipTriggerDisabled, nil
	}

	breakerThreshold := derefIntDefault(trigger.BreakerThreshold, 0)
	failures, err := r.tasks.CountConsecutiveFailedRunsSystem(context.Background(), orgID, firing.EntityID, r.breakerPromptID(orgID, trigger.BlueprintID))
	if err != nil {
		return "", "", fmt.Errorf("breaker query: %w", err)
	}
	if failures >= breakerThreshold {
		return "", domain.PendingFiringSkipBreakerTripped, nil
	}

	id, err := r.fireDelegate(orgID, task, *trigger, firing.TriggeringEventID)
	if err != nil {
		// SKY-424: the run for this (event, trigger) already exists — a
		// prior drain attempt fired it (process died before MarkFired), or
		// the immediate path did before this firing was popped. Definitive
		// "no longer relevant": mark skipped_stale so the firing doesn't
		// retry forever. The existing run gates / drains the entity.
		if errors.Is(err, delegate.ErrAlreadyFired) {
			return "", domain.PendingFiringSkipAlreadyFired, nil
		}
		return "", "", fmt.Errorf("fire delegate: %w", err)
	}
	return id, "", nil
}

// revertTaskStatus moves a task's lifecycle axis back to the given
// status and broadcasts the change so the frontend doesn't get stuck
// showing a stale state. Claim cols are intentionally left alone —
// after SKY-261 B+ the three axes (lifecycle / claim / runs) are
// orthogonal, and this helper only touches lifecycle.
//
// The only caller today is the mark-fired-failure rollback path in
// DrainEntity: a run was successfully spawned but the UPDATE that
// records the firing→run association failed. The recovery flow
// cancels the run, leaves the firing in 'pending' so the next drain
// retries it, and reverts the task lifecycle for FE consistency.
// Critically, the firing's *commitment* (the bot has taken this task)
// is unchanged — the next drain pass needs the bot claim to remain
// set or attemptDrainOne's ClaimedByAgentID guard would skip the
// retry as claim_changed, silently dropping the queued intent. So
// the claim col stays with the bot here; only status moves.
//
// Code paths that DO want to release the claim (user requeue, task
// completion, swipe-undo) clear the claim cols on their own — they
// don't go through this helper.
func (r *Router) revertTaskStatus(orgID, taskID, status string) {
	if err := r.tasks.SetStatusSystem(context.Background(), orgID, taskID, status); err != nil {
		log.Printf("[router] failed to revert task %s to %s: %v", taskID, status, err)
		return
	}
	r.ws.Broadcast(websocket.Event{
		Type:  "task_updated",
		OrgID: orgID,
		Data:  map[string]any{"task_id": taskID, "status": status},
	})
}
