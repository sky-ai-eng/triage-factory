package routing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
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
//
// Returns fired=true iff this call actually committed the bot to the task —
// an immediate fireDelegate success, or a new row landed in pending_firings
// (queued because the entity was busy; the commitment is real even though
// the run hasn't started yet). Every other exit — already claimed by
// another team, a store/lookup error, the bot disabled for the team, the
// breaker tripped, a duplicate already queued, or a replay hitting
// ErrAlreadyFired — returns false: nothing new happened on this call.
func (r *Router) tryAutoDelegate(orgID string, task *domain.Task, trigger domain.EventHandler, entityID string, triggeringEventID string, actingTeamID string) (fired bool) {
	// Exclusive claim: one task, one owner. If the bot has already
	// claimed this task on behalf of a different team (an earlier
	// matched team won the CAS), this team's trigger must not pile on a
	// second run against the same situation — that is the cross-team
	// duplication the one-task model exists to prevent. A trigger whose
	// acting team IS the current owner still proceeds, so multiple
	// prompts one team configured on the same event all run.
	if task.ClaimedByAgentID != "" && actingTeamID != "" && teamIDValue(task) != actingTeamID {
		routerLog.Info("auto-trigger skipped: task already claimed by the bot for another team",
			"task_id", task.ID, "claimed_team", teamIDValue(task), "acting_team", actingTeamID)
		return false
	}
	// Resolve the org's agent ONCE here. It's the single source for three
	// consumers that must agree: the bot-disabled-team gate, the run's actor
	// (frozen onto blueprint_runs.actor_agent_id via DelegateOpts), and the
	// task's claim (stampAgentClaim). Resolving once — instead of re-deriving in
	// stampAgentClaim as before — guarantees runs.actor_agent_id and
	// tasks.claimed_by_agent_id are the same id with no second lookup to drift,
	// and it's available at step-0 enqueue even though the claim isn't stamped
	// until after fireDelegate returns.
	//
	// Nil r.agents is pre-D-Claims test wiring: skip the gate, leave agentID
	// empty (the run records no actor, stampAgentClaim no-ops) — preserving the
	// "proceed with auto-fire" degrade. Production always wires it.
	var agentID string
	if r.agents != nil {
		a, err := r.agents.GetForOrgSystem(context.Background(), orgID)
		if err != nil {
			routerLog.Warn("auto-trigger skipped: agent lookup failed", "error", err)
			return false
		}
		if a != nil {
			agentID = a.ID
		}

		// Bot-disabled-team gate. If the task's team has the bot
		// turned off in team_agents.enabled, the auto-trigger is a no-op
		// — the task is already in the team queue (created by HandleEvent
		// upstream); a human will swipe-delegate later if they want a
		// run. Skip silently rather than firing on a disabled team. Requires
		// team_agents too; nil (older test wiring) degrades to "proceed".
		if r.teamAgents != nil {
			if a == nil {
				// No bootstrapped agent — bootstrap is now fatal at
				// startup, so this shouldn't reach us in practice.
				// Log + bail rather than crashing the goroutine.
				routerLog.Warn("auto-trigger skipped: no agent bootstrapped", "task_id", task.ID)
				return false
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
				routerLog.Warn("auto-trigger skipped: team_agents lookup failed", "task_id", task.ID, "error", err)
				return false
			}
			if ta == nil || !ta.Enabled {
				routerLog.Info("auto-trigger skipped: bot disabled for team", "task_id", task.ID, "team", teamID)
				return false
			}
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
		routerLog.Error("breaker query failed", "entity", entityID, "prompt", breakerPromptID, "error", err)
		return false
	}
	if failures >= breakerThreshold {
		routerLog.Info("breaker tripped",
			"entity", entityID, "prompt", breakerPromptID, "failures", failures, "threshold", breakerThreshold)
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
		return false
	}

	// Per-entity gate. Closed if any auto run is active on the entity OR
	// any pending_firings rows are already queued (FIFO fairness).
	// Compose the per-entity firing gate from its two halves:
	// AgentRunStore owns the runs-shaped predicate, PendingFiringsStore
	// owns the queue-shaped one. canFire = neither side blocks.
	gateCtx := context.Background()
	hasActive, err := r.agentRuns.HasActiveAutoRunForEntitySystem(gateCtx, orgID, entityID)
	if err != nil {
		routerLog.Error("entity gate active-run query failed", "entity", entityID, "error", err)
		return false
	}
	hasPending := false
	if !hasActive {
		hasPending, err = r.firings.HasPendingForEntity(gateCtx, orgID, entityID)
		if err != nil {
			routerLog.Error("entity gate pending query failed", "entity", entityID, "error", err)
			return false
		}
	}
	if hasActive || hasPending {
		// Additive events (TFAC-594): a second firing of a type declared
		// Additive against an entity with an already-active auto run is a
		// follow-up to the conversation in progress, not a request for a
		// second one — inject into the live run via the staged-injection
		// seam instead of deferring. hasPending-only (no active run) always
		// keeps the deferral: injection needs a live target to fold into.
		if hasActive && events.AdditiveFor(trigger.EventType) {
			if r.tryAdditiveInjection(gateCtx, orgID, entityID, task, trigger, triggeringEventID) {
				// The gate is entity-wide, not task-scoped (see
				// HasActiveAutoRunForEntitySystem's doc comment) — the
				// active run's task may differ from THIS task (e.g. a
				// ci_check_failed-derived task owns the live run while a
				// separate slack:mention-derived task on the same entity
				// additively fires). Commit the claim on task here
				// exactly like the deferral path does post-Enqueue: the
				// bot has taken responsibility for this task by folding
				// its event into the live conversation, even though the
				// run belongs to another task. A same-task repeat firing
				// makes this a no-op (bot already owns it).
				r.stampAgentClaim(orgID, task, actingTeamID, agentID)
				return
			}
			// Resolution failed, or the active run went terminal in the
			// race between the gate read and the injection attempt — fall
			// through to the normal deferral so the firing is never
			// silently dropped.
		}
		// System-actor firing rows have no human author. Empty user
		// here lets the Postgres impl's COALESCE walk to the org-
		// owner fallback (creator_user_id is NOT NULL but the table
		// has no separate "actor" column); SQLite ignores the column
		// entirely.
		inserted, err := r.firings.Enqueue(context.Background(), orgID, "", entityID, task.ID, trigger.ID, triggeringEventID)
		if err != nil {
			routerLog.Error("enqueue firing failed",
				"entity", entityID, "task_id", task.ID, "trigger", trigger.ID, "error", err)
			return false
		}
		if !inserted {
			routerLog.Debug("firing collapsed, duplicate already queued",
				"entity", entityID, "task_id", task.ID, "trigger", trigger.ID)
			return false
		}
		routerLog.Info("queued firing, entity busy",
			"entity", entityID, "task_id", task.ID, "trigger", trigger.ID)
		// Pending firing landed in the queue, commit the task to the
		// org's agent. Stamp here (after EnqueuePendingFiring
		// succeeded) so a failed enqueue doesn't leave a phantom claim
		// on an otherwise queued task.
		r.stampAgentClaim(orgID, task, actingTeamID, agentID)
		return true
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
			routerLog.Error("failed to consolidate owner team before fire", "task_id", task.ID, "error", err)
		} else {
			task.TeamID = teamIDPtr(actingTeamID)
		}
	}
	if _, err := r.fireDelegate(orgID, task, trigger, triggeringEventID, agentID); err != nil {
		// A replayed event (at-least-once queue) whose first run
		// already committed hits the (event, trigger) fence and comes back
		// as ErrAlreadyFired. Clean skip — the original run + its claim
		// stand, so we must NOT re-stamp the claim or log an error.
		if errors.Is(err, delegate.ErrAlreadyFired) {
			routerLog.Info("auto-delegate skipped: event already fired this trigger (replay)",
				"task_id", task.ID, "trigger", trigger.ID, "event_id", triggeringEventID)
			return false
		}
		routerLog.Error("fire failed", "task_id", task.ID, "trigger", trigger.ID, "error", err)
		return false
	}
	// fireDelegate succeeded (run inserted + spawner
	// goroutine launched) — stamp the agent claim. Done AFTER success
	// so a fireDelegate that fails + reverts to status='queued'
	// doesn't leave a phantom bot claim on a task that's back in the
	// human-triage queue.
	r.stampAgentClaim(orgID, task, actingTeamID, agentID)
	return true
}

// tryAdditiveInjection folds an additive event into the entity's already-
// active auto run via the staged-injection seam, instead of deferring a
// second run onto pending_firings (TFAC-594). Returns true when the caller
// should treat the firing as handled (delivered live, or durably staged
// onto a resumable run); false when the caller must fall through to the
// normal deferral so the firing is never silently dropped. Three distinct
// cases fall through:
//
//   - the active run couldn't be resolved to an ID (the busy-gate read
//     raced the run going terminal);
//   - StageOrDeliverInjectionResult reports the injection was dropped
//     outright (delivered=false, staged=false — no live process, AND the
//     durable append itself failed: store unwired or a transient error).
//     There's no durable row to fall back on here, regardless of the run's
//     current status, so this always defers;
//   - it was durably staged (staged=true) but the run has since gone fully
//     terminal (not parked/open) — a staged row only flushes on the run's
//     next resume, so a run that can never resume would silently lose it.
func (r *Router) tryAdditiveInjection(ctx context.Context, orgID, entityID string, task *domain.Task, trigger domain.EventHandler, triggeringEventID string) bool {
	runID, err := r.agentRuns.ActiveAutoRunIDForEntitySystem(ctx, orgID, entityID)
	if err != nil || runID == "" {
		return false
	}

	// Best-effort: an empty metadataJSON still renders a body naming the
	// event type alone, so a lookup failure degrades rather than drops the
	// injection.
	metadataJSON, err := r.events.GetMetadataSystem(ctx, orgID, triggeringEventID)
	if err != nil {
		metadataJSON = ""
	}
	body := domain.AdditiveEventInjection(trigger.EventType, metadataJSON)

	delivered, staged := r.spawner.StageOrDeliverInjectionResult(orgID, runID, trigger.EventType, body)
	if !delivered {
		if !staged {
			// Dropped outright — nothing durable to flush later. Never
			// treat this as handled no matter the run's status.
			return false
		}
		run, rerr := r.agentRuns.GetSystem(ctx, orgID, runID)
		if rerr != nil || run == nil || !runIsResumable(run.Status, run.Outcome) {
			return false
		}
	}

	if err := r.tasks.RecordEventSystem(ctx, orgID, task.ID, triggeringEventID, "injected"); err != nil {
		routerLog.Error("failed to record injected task_event", "task_id", task.ID, "run_id", runID, "error", err)
	}
	routerLog.Info("injected additive event into active run",
		"entity", entityID, "task_id", task.ID, "trigger", trigger.ID, "run_id", runID, "event_type", trigger.EventType)
	return true
}

// runIsResumable mirrors internal/delegate's unexported resumableState
// predicate: every non-finish parked/terminal state a later resume can
// wake — `open`, or `completed` with outcome `abort`. Duplicated here
// (rather than exported from internal/delegate) to keep the routing→
// delegate dependency narrowed to the Delegator interface.
func runIsResumable(status, outcome string) bool {
	switch status {
	case "open":
		return true
	case "completed":
		return outcome == string(domain.RunOutcomeAbort)
	default:
		return false
	}
}

// stampAgentClaim writes claimed_by_agent_id on a task AND broadcasts the
// task_claimed event so listeners (Board) can re-render the
// per-claim lanes. Called from the three commitment points in
// tryAutoDelegate (post-fireDelegate success, post-EnqueuePendingFiring
// success, post-tryAdditiveInjection success). All three converge on "the
// bot has committed to this task" — including the additive-injection case,
// where the task committed to may not be the task that owns the run the
// event got folded into (the busy gate is entity-wide, not task-scoped).
//
// agentID is the org agent resolved ONCE by the caller (tryAutoDelegate) — the
// same id frozen onto the run's blueprint_run actor — so the claim and the run's
// execution attribution can't drift. Empty agentID (pre-bootstrap / test wiring
// with no agents store) leaves the claim unstamped rather than crashing, matching
// the "transient seam between db init and agent bootstrap" case in §4 of the spec.
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
func (r *Router) stampAgentClaim(orgID string, task *domain.Task, actingTeamID, agentID string) {
	if agentID == "" {
		return
	}
	ok, err := r.tasks.StampAgentClaimIfUnclaimedSystem(context.Background(), orgID, task.ID, agentID, actingTeamID)
	if err != nil {
		routerLog.Error("failed to stamp agent claim", "task_id", task.ID, "error", err)
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
		routerLog.Debug("agent claim stamp was a no-op (user owns it, bot already owns it, or task is terminal)", "task_id", task.ID)
		return
	}
	task.ClaimedByAgentID = agentID
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
			"claimed_by_agent_id": agentID,
			"claimed_by_user_id":  "",
		},
	})
}

// fireDelegate transitions the task to delegated status, broadcasts the
// change, then fires the spawner. Returns the run ID on success — used by
// DrainEntity to record which run a queued firing materialized into.
//
// triggeringEventID is the event instance driving this fire:
// the immediate path passes tryAutoDelegate's event id, the drain path
// passes the pending firing's. It threads into DelegateOpts so the run
// insert is fenced on (triggering_event_id, trigger_id); a replayed event
// whose first run already committed surfaces as delegate.ErrAlreadyFired,
// which both callers treat as a clean skip rather than a duplicate fire.
//
// actorAgentID is the executing bot, resolved once by the caller — the immediate
// path passes the agent it resolved up front (and stamps the same id as the
// claim); the drain path passes the firing's already-stamped task claim. It's
// frozen onto blueprint_runs.actor_agent_id at mint and inherited by every step.
func (r *Router) fireDelegate(orgID string, task *domain.Task, trigger domain.EventHandler, triggeringEventID, actorAgentID string) (string, error) {
	if r.spawner == nil {
		return "", fmt.Errorf("spawner not configured")
	}

	// No status flip here. Previously we transitioned to
	// status='delegated' for UI feedback + dedup. Now the
	// responsibility axis is the claim columns: stampAgentClaim
	// (called by the caller on fireDelegate success) writes
	// claimed_by_agent_id and broadcasts task_claimed, which is what
	// the Board now listens for. Status stays 'queued' until a
	// genuine lifecycle move (done / dismissed / snoozed). Dedup is
	// unaffected — the partial unique index gates on status NOT IN
	// ('done', 'dismissed'), so a queued+claimed task still matches.
	routerLog.Info("auto-delegating task",
		"task_id", task.ID, "trigger", trigger.ID, "blueprint", trigger.BlueprintID)

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
		ActorAgentID:        actorAgentID,
	})
	if err != nil {
		// Post-B+: nothing to revert status-wise (status stayed 'queued').
		// stampAgentClaim hasn't run yet either — the caller only calls
		// it after fireDelegate returns nil. So this failure leaves the
		// task in a clean unclaimed-queued state, which is correct.
		return "", err
	}
	routerLog.Info("started run for task", "run_id", runID, "task_id", task.ID)
	return runID, nil
}

// DrainEntity is the spawner's hook into the per-entity firing queue.
// Called when an auto run terminates on the entity (any terminal status).
// A completed run that left an unresolved artifact still counts as terminal
// here — the artifact is an async sidecar, so it releases the entity
// lock and doesn't block downstream processing.
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
			routerLog.Error("drain pop failed", "entity", entityID, "error", err)
			return
		}
		if firing == nil {
			return // queue empty
		}

		runID, skipReason, transientErr := r.attemptDrainOne(orgID, firing)
		if transientErr != nil {
			// Transient failure (DB read, Delegate). PopForEntity already
			// claimed this row into 'draining' (TFAC-579), so release it
			// back to 'pending' rather than leaving it stuck — marking
			// 'skipped_stale' here would permanently drop a queued intent
			// over a temporary problem, and a 'draining' row left
			// unresolved is invisible to HasPendingForEntity /
			// ListEntitiesWithPending and would never be retried. The
			// periodic sweeper or the next run-terminal will retry once
			// released.
			if err := r.firings.Release(context.Background(), orgID, firing.ID); err != nil {
				routerLog.Error("release firing after transient drain error failed",
					"firing_id", firing.ID, "entity", entityID, "error", err)
			}
			routerLog.Warn("drain transient error, released firing for retry",
				"firing_id", firing.ID, "entity", entityID, "error", transientErr)
			return
		}
		if runID != "" {
			if err := r.firings.MarkFired(context.Background(), orgID, firing.ID, runID); err != nil {
				// Durability race: the run was created (side-effect
				// committed inside the spawner goroutine) but the UPDATE
				// that records the firing→run association failed.
				//
				// Roll the side-effect chain back in reverse: Cancel the
				// run we just spawned, then revert the task to 'queued'
				// so the limbo state (task=delegated + no live run) is
				// not externally visible. Mirrors what fireDelegate
				// already does when spawner.Delegate itself fails.
				//
				// PopForEntity already claimed this row into 'draining'
				// (TFAC-579) — release it back to 'pending' so a later
				// drain retries it fresh, mirroring the transientErr
				// branch above. Without this the row is stuck in
				// 'draining' forever: PopForEntity only ever claims
				// 'pending' rows, and HasPendingForEntity /
				// ListEntitiesWithPending don't see 'draining' rows
				// either, so nothing would ever pick it up again.
				routerLog.Error("mark firing fired failed, rolling back: cancelling run + reverting task to queued",
					"firing_id", firing.ID, "run_id", runID, "error", err)
				if r.spawner != nil {
					if cerr := r.spawner.Cancel(orgID, runID, ""); cerr != nil {
						routerLog.Warn("cancel run after mark-fired failure (run may already be terminal, drain still triggers from its defer)",
							"run_id", runID, "error", cerr)
					}
				}
				r.revertTaskStatus(orgID, firing.TaskID, "queued")
				if rerr := r.firings.Release(context.Background(), orgID, firing.ID); rerr != nil {
					routerLog.Error("release firing after mark-fired failure failed",
						"firing_id", firing.ID, "error", rerr)
				}
			}
			return // one fire per drain — the new run gates the rest
		}
		// Skipped or fire failed; record reason and continue draining.
		if err := r.firings.MarkSkipped(context.Background(), orgID, firing.ID, skipReason); err != nil {
			// Same stuck-in-'draining' risk as the MarkFired branch above:
			// the skip decision itself is definitive (attemptDrainOne only
			// reaches here with a non-empty skipReason), but persisting it
			// failed, so release the claim rather than strand the row.
			routerLog.Error("mark firing skipped failed", "firing_id", firing.ID, "skip_reason", skipReason, "error", err)
			if rerr := r.firings.Release(context.Background(), orgID, firing.ID); rerr != nil {
				routerLog.Error("release firing after mark-skipped failure failed",
					"firing_id", firing.ID, "error", rerr)
			}
			return
		}
		routerLog.Debug("skipped firing", "firing_id", firing.ID, "entity", entityID, "skip_reason", skipReason)
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
				routerLog.Error("drain sweeper: list orgs failed", "error", err)
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
		routerLog.Error("drain sweeper: list entities with pending failed", "org", orgID, "error", err)
		return
	}
	for _, eid := range ids {
		active, err := r.agentRuns.HasActiveAutoRunForEntitySystem(ctx, orgID, eid)
		if err != nil {
			routerLog.Error("drain sweeper: active-check failed", "entity", eid, "org", orgID, "error", err)
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
//     claim_changed (a user took the task over or
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
	// status='snoozed' belongs on the lifecycle-skip axis,
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

	// Drain only fires if the bot's claim still holds.
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

	// The actor is the agent that already claimed this task (guaranteed non-empty
	// by the claim guard above) — the drain re-fires the same bot's commitment, so
	// the new blueprint_run's frozen actor matches the standing task claim.
	id, err := r.fireDelegate(orgID, task, *trigger, firing.TriggeringEventID, task.ClaimedByAgentID)
	if err != nil {
		// The run for this (event, trigger) already exists — a
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
// The three axes (lifecycle / claim / runs) are
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
		routerLog.Error("failed to revert task status", "task_id", taskID, "status", status, "error", err)
		return
	}
	r.ws.Broadcast(websocket.Event{
		Type:  "task_updated",
		OrgID: orgID,
		Data:  map[string]any{"task_id": taskID, "status": status},
	})
}
