package routing

import (
	"context"
	"errors"
	"fmt"
	"time"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// tryAutoDelegate decides whether a matched (task, trigger) fires now or
// queues. Order of checks: breaker (per-(entity,prompt)) → task gate
// (per-task, auto-only) → fire or enqueue.
//
// Breaker is a hard skip — a tripped breaker means the user has work to
// investigate before more conversations land on this entity-prompt pair.
// Queueing past it would just stack stale firings the user didn't ask for. It stays
// entity-scoped on purpose: repeated failures of one prompt against one
// entity are the signal, whichever task carried them.
//
// The task gate is the serialization point: at most one auto conversation in
// flight per task, whichever trigger it came from. A sibling task on the same
// entity has its own gate and fires independently. If the gate is closed
// (this task's own conversation is live, or older firings are queued for FIFO
// fairness), the firing folds into the live conversation or enqueues onto
// pending_firings instead of being dropped silently.
//
// Returns fired=true iff this call actually committed the bot to the task —
// an immediate fireDelegate success, or a new row landed in pending_firings
// (queued because the task was busy; the commitment is real even though
// the conversation hasn't started yet). Every other exit — already claimed by
// another team, the bot disabled for the team, no agent bootstrapped, the
// breaker tripped, a duplicate already queued, or a replay hitting
// ErrAlreadyFired — returns (false, nil): nothing new happened on this call,
// and nothing should.
//
// A non-nil error is the third outcome, and it means something else: a
// dependency this decision rests on failed, so whether the trigger should
// have fired is unknown. The caller propagates it to the queue worker, which
// replays the whole event; the fences (the (event, trigger) replay fence, the
// one-active-auto-run index, the pending_firings pending-unique) make that
// replay a no-op for anything that already committed. The line between the
// two is whether a retry could come out differently — a state a replay would
// find unchanged is a skip, not an error, or the event burns its attempt
// budget and parks over a standing condition.
func (r *Router) tryAutoDelegate(ctx context.Context, orgID string, task *domain.Task, trigger domain.EventHandler, entityID string, triggeringEventID string, actingTeamID string) (fired bool, err error) {
	return r.tryAutoDelegateTrackingInjection(ctx, orgID, task, trigger, entityID, triggeringEventID, actingTeamID, nil)
}

// tryAutoDelegateTrackingInjection is tryAutoDelegate with an optional output
// for the one conversation that consumed this event through the same-task
// additive path. The routing-disposition subscriber uses that identity to
// suppress only the informational duplicate while leaving every task/rule
// write on the normal router path.
func (r *Router) tryAutoDelegateTrackingInjection(ctx context.Context, orgID string, task *domain.Task, trigger domain.EventHandler, entityID string, triggeringEventID string, actingTeamID string, injectedConversationID *string) (fired bool, err error) {
	// Exclusive claim: one task, one owner. If the bot has already
	// claimed this task on behalf of a different team (an earlier
	// matched team won the CAS), this team's trigger must not pile on a
	// second conversation against the same situation — that is the cross-team
	// duplication the one-task model exists to prevent. A trigger whose
	// acting team IS the current owner still proceeds, so multiple
	// prompts one team configured on the same event all run.
	if task.ClaimedByAgentID != "" && actingTeamID != "" && teamIDValue(task) != actingTeamID {
		routerLog.Info("auto-trigger skipped: task already claimed by the bot for another team",
			"task_id", task.ID, "claimed_team", teamIDValue(task), "acting_team", actingTeamID)
		return false, nil
	}
	// Resolve the org's agent ONCE here. It's the single source for three
	// consumers that must agree: the bot-disabled-team gate, the blueprint
	// run's actor (frozen onto blueprint_runs.actor_agent_id via DelegateOpts),
	// and the task's claim (the AgentClaimStamp each commitment carries).
	// Resolving once guarantees conversations.actor_agent_id and
	// tasks.claimed_by_agent_id are the same id with no second lookup to drift,
	// and it's available at step-0 enqueue — which is what lets the claim ride
	// the blueprint-run insert's own transaction instead of following it as a
	// separate write.
	//
	// Nil r.agents is pre-D-Claims test wiring: skip the gate, leave agentID
	// empty (the blueprint run records no actor, the claim stamp is the zero
	// value and every store skips it) — preserving the "proceed with
	// auto-fire" degrade. Production always wires it.
	var agentID string
	if r.agents != nil {
		a, err := r.agents.GetForOrgSystem(ctx, orgID)
		if err != nil {
			routerLog.Warn("auto-trigger deferred: agent lookup failed", "error", err)
			return false, fmt.Errorf("agent lookup: %w", err)
		}
		if a != nil {
			agentID = a.ID
		}

		// Bot-disabled-team gate. If the task's team has the bot
		// turned off in team_agents.enabled, the auto-trigger is a no-op
		// — the task is already in the team queue (created by HandleEvent
		// upstream); a human will delegate it later if they want a
		// conversation. Skip silently rather than firing on a disabled team.
		// Requires team_agents too; nil (older test wiring) degrades to "proceed".
		if r.teamAgents != nil {
			if a == nil {
				// No bootstrapped agent — bootstrap is now fatal at
				// startup, so this shouldn't reach us in practice.
				// Log + bail rather than crashing the goroutine. Not an
				// error: nothing about replaying the event bootstraps an
				// agent, so retrying would spend the event's attempts and
				// park it over a startup-time condition.
				routerLog.Warn("auto-trigger skipped: no agent bootstrapped", "task_id", task.ID)
				return false, nil
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
			ta, err := r.teamAgents.GetForTeamSystem(ctx, orgID, teamID, a.ID)
			if err != nil {
				routerLog.Warn("auto-trigger deferred: team_agents lookup failed", "task_id", task.ID, "error", err)
				return false, fmt.Errorf("team_agents lookup: %w", err)
			}
			if ta == nil || !ta.Enabled {
				routerLog.Info("auto-trigger skipped: bot disabled for team", "task_id", task.ID, "team", teamID)
				return false, nil
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
	breakerPromptID := r.breakerPromptID(ctx, orgID, trigger.BlueprintID)
	failures, err := r.tasks.CountConsecutiveFailedConversationsSystem(ctx, orgID, entityID, breakerPromptID)
	if err != nil {
		routerLog.Error("breaker query failed", "entity", entityID, "prompt", breakerPromptID, "error", err)
		return false, fmt.Errorf("breaker query: %w", err)
	}
	if failures >= breakerThreshold {
		routerLog.Info("breaker tripped",
			"entity", entityID, "prompt", breakerPromptID, "failures", failures, "threshold", breakerThreshold)
		// Look up prompt name for the toast — opportunistic, falls back to a
		// generic message if the lookup fails since the breaker trip itself
		// is the load-bearing signal. One toast per trip (happens rarely).
		promptName := ""
		if p, perr := r.prompts.GetSystem(ctx, orgID, breakerPromptID); perr == nil && p != nil {
			promptName = p.Name
		}
		if promptName == "" {
			promptName = "prompt"
		}
		toast.Warning(r.ws, orgID, fmt.Sprintf("Auto-delegation paused: %s tripped the breaker (%d consecutive failures on this entity)", promptName, failures))
		return false, nil
	}

	// Per-task gate. Closed if an auto conversation is active on THIS TASK, or
	// any pending_firings rows are already queued for it (FIFO fairness).
	// Compose the gate from its two halves: ConversationStore owns the
	// conversation-shaped predicate, PendingFiringsStore owns the queue-shaped
	// one. The gate opens only when neither side blocks.
	//
	// The task is the unit, not the entity. A task is one situation needing
	// attention — that is what its (entity, event type, discriminator) dedup
	// key means — so two situations on one pull request may each have an
	// agent working. Gating on the entity conflated them, and once a
	// conversation could park indefinitely (a stop freezes its blueprint
	// 'running' by design) one such conversation held the gate shut for every
	// other situation on that entity, with nothing left to reopen it.
	//
	// The active-conversation read resolves the conversation's ID rather than
	// a bool: a busy gate is the additive-injection path, and folding the new
	// event into the conversation needs the conversation.
	activeConversationID, err := r.conversations.ActiveAutoConversationIDForTaskSystem(ctx, orgID, task.ID)
	if err != nil {
		routerLog.Error("task gate active-conversation query failed", "task_id", task.ID, "error", err)
		return false, fmt.Errorf("task gate active-conversation query: %w", err)
	}
	hasActive := activeConversationID != ""
	hasPending := false
	if !hasActive {
		hasPending, err = r.firings.HasPendingForTask(ctx, orgID, task.ID)
		if err != nil {
			routerLog.Error("task gate pending query failed", "task_id", task.ID, "error", err)
			return false, fmt.Errorf("task gate pending query: %w", err)
		}
	}
	if hasActive || hasPending {
		// A busy gate now always means THIS task's own conversation is live,
		// so absorption is the default rather than a same-task special case:
		// fold the event into the conversation instead of deferring a second
		// one. Only a firing with no live conversation to fold into (hasPending
		// with the conversation already gone) still defers.
		if hasActive {
			// The claim rides whichever durable write the injection makes —
			// folding an event into the live conversation is the bot taking
			// responsibility for this task exactly as a fresh fire or a
			// deferral is. The conversation belongs to this same task, so the
			// standing claim is normally already the bot's and the stamp is a
			// race-safe no-op; it moves only when the task was user-requeued
			// while its conversation stayed live, which this new event
			// re-commits.
			if r.tryAdditiveInjection(ctx, orgID, entityID, activeConversationID, task, trigger, triggeringEventID, claimStamp(agentID, actingTeamID)) {
				if injectedConversationID != nil {
					*injectedConversationID = activeConversationID
				}
				return false, nil
			}
			// The conversation went terminal between the gate read and the
			// injection attempt, or the injection couldn't be delivered durably — fall
			// through to the normal deferral so the firing is never silently
			// dropped.
		}
		return r.enqueueBusyFiring(ctx, orgID, entityID, task, trigger, triggeringEventID, actingTeamID, agentID)
	}

	// Consolidate the owner team to the acting team BEFORE firing. An
	// auto-fired conversation inherits conversations.team_id from tasks.team_id
	// at insert, and the claim (which also consolidates the owner) lands inside
	// that same insert — so without this, a conversation fired by a team other
	// than the creation-time owner (e.g. the owner had auto-delegation disabled
	// and a lower-priority team is firing) would read the stale owner as it
	// writes its own row. Owner-only update, no claim touch: if the fire fails
	// the task is owned by the team that tried, unclaimed — not a phantom
	// claim. Skipped when the acting team already is the owner (the common
	// path, and same-team multi-prompt where an active conversation may exist).
	if actingTeamID != "" && actingTeamID != teamIDValue(task) {
		if updated, err := r.tasks.SetOwnerTeamSystem(ctx, orgID, task.ID, actingTeamID); err != nil {
			routerLog.Error("failed to consolidate owner team before fire", "task_id", task.ID, "error", err)
		} else {
			task.TeamID = updated.TeamID
		}
	}
	// The claim rides the fenced blueprint-run insert's transaction (see
	// db.AgentClaimStamp): the blueprint-run row and "the bot owns this task"
	// are one durable write, so there is no window where the conversation is
	// live and the board still shows the task free. A stamp refusal — a user
	// claiming mid-fire — does not roll the blueprint run back; the human wins
	// the claim, the conversation still runs, and the drain path's
	// claim_changed guard keeps later firings off it.
	if _, err := r.fireDelegate(ctx, orgID, task, trigger, triggeringEventID, agentID, claimStamp(agentID, actingTeamID)); err != nil {
		// A replayed event (at-least-once queue) whose first blueprint run
		// already committed hits the (event, trigger) fence and comes back
		// as ErrAlreadyFired. Clean skip — the original blueprint run + its
		// claim stand, so we must NOT re-stamp the claim or log an error.
		if errors.Is(err, delegate.ErrAlreadyFired) {
			routerLog.Info("auto-delegate skipped: event already fired this trigger (replay)",
				"task_id", task.ID, "trigger", trigger.ID, "event_id", triggeringEventID)
			return false, nil
		}
		// The gate read said idle, but a DIFFERENT (event, trigger) went
		// active on this task before our insert — the one-active index
		// is the authority, the gate just a fast-path. The
		// intent is still valid: defer it onto pending_firings exactly as
		// the gate's busy branch would have, instead of dropping it.
		if errors.Is(err, delegate.ErrTaskBusy) {
			routerLog.Info("auto-delegate deferred: task went busy under the fire (gate race)",
				"entity", entityID, "task_id", task.ID, "trigger", trigger.ID, "event_id", triggeringEventID)
			return r.enqueueBusyFiring(ctx, orgID, entityID, task, trigger, triggeringEventID, actingTeamID, agentID)
		}
		routerLog.Error("fire failed", "task_id", task.ID, "trigger", trigger.ID, "error", err)
		return false, fmt.Errorf("fire delegate: %w", err)
	}
	// The blueprint run and its claim are committed. Sync what the router
	// still holds in memory from the row itself, and broadcast if the claim
	// moved.
	r.syncClaimAfterCommit(ctx, orgID, task, agentID)
	return true, nil
}

// enqueueBusyFiring defers a valid firing onto pending_firings because the
// task is busy, and commits the task's agent claim on success. Called
// from both places that discover busyness: the gate's fast-path read, and
// the ErrTaskBusy loser of the fenced insert (the gate race — the DB
// index is the authority, the gate an optimization). Returns true when the
// intent was queued and the claim stamped; (false, nil) when it collapsed
// onto an already-queued (task, trigger) duplicate (whose own enqueue already
// stamped the claim); (false, err) when the enqueue itself failed — the
// deferral is the firing's last durable record, so losing it loses the intent
// entirely, and the caller replays instead. The pending-unique makes that
// replay collapse rather than double-queue.
func (r *Router) enqueueBusyFiring(ctx context.Context, orgID, entityID string, task *domain.Task, trigger domain.EventHandler, triggeringEventID, actingTeamID, agentID string) (bool, error) {
	// System-actor firing rows have no human author. Empty user
	// here lets the Postgres impl's COALESCE walk to the org-
	// owner fallback (creator_user_id is NOT NULL but the table
	// has no separate "actor" column); SQLite ignores the column
	// entirely.
	//
	// The claim rides the insert's transaction: a queued firing commits the
	// bot to this task just as a fired blueprint run does, so a failed enqueue
	// leaves no phantom claim and a landed one is never claim-less. The store skips the
	// stamp on the collapse path, where the already-queued duplicate's own
	// enqueue made the commitment.
	inserted, claimed, err := r.firings.Enqueue(ctx, orgID, "", entityID, task.ID, trigger.ID, triggeringEventID, claimStamp(agentID, actingTeamID))
	if err != nil {
		routerLog.Error("enqueue firing failed",
			"entity", entityID, "task_id", task.ID, "trigger", trigger.ID, "error", err)
		return false, fmt.Errorf("enqueue firing: %w", err)
	}
	if !inserted {
		routerLog.Debug("firing collapsed, duplicate already queued",
			"entity", entityID, "task_id", task.ID, "trigger", trigger.ID)
		return false, nil
	}
	routerLog.Info("queued firing, task busy",
		"entity", entityID, "task_id", task.ID, "trigger", trigger.ID)
	r.claimCommitted(orgID, task, actingTeamID, agentID, claimed)
	return true, nil
}

// tryAdditiveInjection folds a firing into its task's already-active auto
// conversation via the cross-pod-aware injection seam, instead of deferring a
// second conversation onto pending_firings. conversationID is that
// conversation's id, resolved by the caller from
// the firing's own task — so it belongs to that task by construction, with
// no separate ownership check to make. Returns true when the caller should
// treat the firing as handled; false when the caller must fall through to
// the normal deferral so the firing is never silently dropped.
//
// The one fall-through is StageOrDeliverAdditiveEvent reporting
// InjectNotDelivered — dropped outright (no live process anywhere, and the
// durable append itself failed), or durably staged onto a conversation
// that's since gone fully terminal (a staged row only flushes on the
// conversation's next resume, so one that can never resume would silently
// lose it). Neither case has a durable row to fall back on, so this
// defers.
//
// InjectDeliveredRemote is distinct from the other "handled" outcomes: a
// live remote executor now owns recording task_events 'injected' (or
// compensating with a pending_firing enqueue if the conversation turns out
// dead by apply time) — this method must NOT record it here, or a
// slow/failed remote apply could leave a duplicate or premature bookkeeping
// row. The claim travels with the signal for the same reason: the owner
// stamps it inside whichever of those two writes it makes. The stamp this
// method does on that branch is the immediate, broadcast-carrying half — if
// it fails, the owner's coupled write is what makes the claim durable, so
// the commitment can no longer outlive it.
func (r *Router) tryAdditiveInjection(ctx context.Context, orgID, entityID, conversationID string, task *domain.Task, trigger domain.EventHandler, triggeringEventID string, claim dbpkg.AgentClaimStamp) bool {
	// Best-effort: an empty metadataJSON still renders a body naming the
	// event type alone, so a lookup failure degrades rather than drops the
	// injection.
	metadataJSON, err := r.events.GetMetadataSystem(ctx, orgID, triggeringEventID)
	if err != nil {
		metadataJSON = ""
	}
	body := domain.AdditiveEventInjection(trigger.EventType, metadataJSON)

	outcome := r.spawner.StageOrDeliverAdditiveEvent(ctx, orgID, conversationID, trigger.EventType, body,
		domain.NoteProvenance{EventID: triggeringEventID, EventType: trigger.EventType},
		delegate.AdditiveFiringRef{
			EntityID:          entityID,
			TaskID:            task.ID,
			TriggerID:         trigger.ID,
			TriggeringEventID: triggeringEventID,
			TaskClaim:         claim,
		})
	switch outcome {
	case delegate.InjectNotDelivered:
		return false
	case delegate.InjectDeliveredRemote:
		routerLog.Info("handed additive event to a live remote executor via conversation_signals",
			"entity", entityID, "task_id", task.ID, "trigger", trigger.ID, "conversation", conversationID, "event_type", trigger.EventType)
		r.stampAgentClaim(ctx, orgID, task, claim)
		return true
	default: // InjectDeliveredLocal, InjectStagedResumable
		// The mark and the claim are one durable step, so a failure loses both
		// together rather than committing the fold against a task the board
		// still shows as free. The injection itself is already delivered
		// (or durably staged), so this stays a handled outcome either way —
		// falling through to the deferral would fire a second blueprint run for
		// an event the agent has already been handed.
		if claimed, err := r.tasks.MarkEventInjectedSystem(ctx, orgID, task.ID, triggeringEventID, claim); err != nil {
			routerLog.Error("failed to mark injected task_event", "task_id", task.ID, "conversation", conversationID, "error", err)
		} else {
			r.claimCommitted(orgID, task, claim.ActingTeamID, claim.AgentID, claimed)
		}
		routerLog.Info("injected additive event into active conversation",
			"entity", entityID, "task_id", task.ID, "trigger", trigger.ID, "conversation", conversationID, "event_type", trigger.EventType)
		return true
	}
}

// claimStamp packages the claim the three commitment points hand to their
// store call. agentID is the org agent resolved ONCE by tryAutoDelegate — the
// same id frozen onto blueprint_runs.actor_agent_id — so the claim and the
// blueprint run's execution attribution can't drift.
//
// Empty agentID (the seam between db init and agent bootstrap, or test wiring
// with no agents store) yields the zero stamp, team included. The team is
// dropped rather than carried because ActingTeamID only ever means
// "consolidate the owner as part of this claim" — with no agent there is no
// claim to consolidate under, so a team-only value names a write that will
// never happen. Every store would skip it either way; what a partial value
// would actually cost is honesty on the cross-pod inject path, where the two
// fields ride the signal payload as omitempty JSON and a lone
// claim_acting_team_id would describe a claim the producer never made.
func claimStamp(agentID, actingTeamID string) dbpkg.AgentClaimStamp {
	if agentID == "" {
		return dbpkg.AgentClaimStamp{}
	}
	return dbpkg.AgentClaimStamp{AgentID: agentID, ActingTeamID: actingTeamID}
}

// claimCommitted reconciles the router with a claim stamp that has already
// committed inside its engagement's transaction: it updates the in-memory
// task — the cross-team guard reads ClaimedByAgentID when this same event's
// next matched trigger comes round — and broadcasts task_claimed so the Board
// re-renders its per-claim lanes.
//
// claimed=false is the store's three-way refusal: a user beat the bot to the
// claim (don't steal), the bot already owns it (idempotent no-op), or the task
// is terminal (sticky claim past close). All three mean "nothing moved", so
// both the in-memory write and the broadcast are correctly skipped; the
// commitment they rode still stands. We don't disambiguate at the log level —
// the store doesn't surface which guard tripped, and re-reading just to log it
// is wasted I/O.
func (r *Router) claimCommitted(orgID string, task *domain.Task, actingTeamID, agentID string, claimed bool) {
	if !claimed {
		if agentID != "" {
			routerLog.Debug("agent claim stamp was a no-op (user owns it, bot already owns it, or task is terminal)", "task_id", task.ID)
		}
		return
	}
	task.ClaimedByAgentID = agentID
	// A landed stamp proves claimed_by_user_id was NULL at commit time — the
	// store's guard requires it — so a non-empty user claim on this struct is
	// a read that has since gone stale (a requeue between load and stamp).
	// Leaving it would put both claim columns on one in-memory task, a state
	// the DB's XOR forbids and the re-derive guard at rederive.go:70 reads as
	// "someone owns this" for the wrong reason. syncClaimAfterCommit, the
	// fire-path sibling, already lands both fields from its read-back.
	task.ClaimedByUserID = ""
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

// syncClaimAfterCommit reads back the claim that rode the fenced blueprint-run
// insert. The fire path is the one commitment whose store call the router
// doesn't make itself — it goes through the spawner — so the outcome comes
// from the row rather than a return value. This is a read of the claim column,
// not a derivation from conversation liveness: a user who won the race
// mid-fire reads back as the owner and neither the in-memory task nor the
// Board is told otherwise.
//
// Best-effort. A failed read leaves the router's in-memory copy stale for the
// rest of this event's trigger loop, which is what a failed stamp used to do
// on every fire; the committed row is already correct either way.
func (r *Router) syncClaimAfterCommit(ctx context.Context, orgID string, task *domain.Task, agentID string) {
	if agentID == "" {
		return
	}
	fresh, err := r.tasks.GetSystem(ctx, orgID, task.ID)
	if err != nil || fresh == nil {
		routerLog.Warn("could not read back the task claim committed with the blueprint run", "task_id", task.ID, "error", err)
		return
	}
	moved := task.ClaimedByAgentID != agentID && fresh.ClaimedByAgentID == agentID
	task.ClaimedByAgentID = fresh.ClaimedByAgentID
	task.ClaimedByUserID = fresh.ClaimedByUserID
	task.TeamID = fresh.TeamID
	if moved {
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
}

// stampAgentClaim is the standalone claim write, now reduced to the one
// commitment the router cannot ride a transaction with: an additive event
// handed to a live REMOTE executor. That pod owns the durable write (the
// 'injected' mark, or the compensating pending_firing if the conversation is
// gone by apply time) and stamps the claim inside it — the signal payload
// carries the claim for exactly that. This call is the local, immediate half, so the Board
// sees the claim now instead of one cross-pod hop later; if it fails, the
// owner's coupled write still makes it durable.
func (r *Router) stampAgentClaim(ctx context.Context, orgID string, task *domain.Task, claim dbpkg.AgentClaimStamp) {
	if claim.AgentID == "" {
		return
	}
	ok, err := r.tasks.StampAgentClaimIfUnclaimedSystem(ctx, orgID, task.ID, claim.AgentID, claim.ActingTeamID)
	if err != nil {
		routerLog.Error("failed to stamp agent claim", "task_id", task.ID, "error", err)
		return
	}
	r.claimCommitted(orgID, task, claim.ActingTeamID, claim.AgentID, ok)
}

// fireDelegate transitions the task to delegated status, broadcasts the
// change, then fires the spawner. Returns the blueprint-run ID on success —
// used by DrainTask to record which blueprint run a queued firing
// materialized into.
//
// triggeringEventID is the event instance driving this fire:
// the immediate path passes tryAutoDelegate's event id, the drain path
// passes the pending firing's. It threads into DelegateOpts so the
// blueprint-run insert is fenced on (triggering_event_id, trigger_id); a
// replayed event whose first blueprint run already committed surfaces as
// delegate.ErrAlreadyFired, which both callers treat as a clean skip
// rather than a duplicate fire.
//
// actorAgentID is the executing bot, resolved once by the caller — the immediate
// path passes the agent it resolved up front (and stamps the same id as the
// claim); the drain path passes the firing's already-stamped task claim. It's
// frozen onto blueprint_runs.actor_agent_id at mint and inherited by every step.
//
// claim is the task claim to write inside the blueprint-run insert's own
// transaction, and is deliberately NOT derived from actorAgentID. The immediate path passes
// one: this fire is the commitment, so the claim must land with it. The drain
// path passes the zero stamp: its firing's claim was committed by the enqueue
// that queued it, and attemptDrainOne has just re-validated that the claim is
// still the bot's — re-stamping here would silently re-impose a claim a user
// cleared by requeueing in the interim.
func (r *Router) fireDelegate(ctx context.Context, orgID string, task *domain.Task, trigger domain.EventHandler, triggeringEventID, actorAgentID string, claim dbpkg.AgentClaimStamp) (string, error) {
	// The handoff point, and the last thing this trace can see: what the
	// spawner enqueues is claimed minutes later, by another process, and up
	// to five times — so the engagement gets its own root and the
	// blueprint_run.id stamped below is what joins the two. A link would
	// promise a 1:1 relationship that does not exist.
	ctx, span := tracer.Start(ctx, "route.delegate", trace.WithAttributes(telemetry.TaskID(task.ID)))
	defer span.End()

	if r.spawner == nil {
		return "", fmt.Errorf("spawner not configured")
	}

	// No status flip here. Previously we transitioned to
	// status='delegated' for UI feedback + dedup. Now the
	// responsibility axis is the claim columns: the blueprint-run insert writes
	// claimed_by_agent_id in its own transaction and the caller
	// broadcasts task_claimed, which is what the Board now listens
	// for. Status stays 'queued' until a genuine lifecycle move (done
	// / dismissed / snoozed). Dedup is unaffected — the partial unique
	// index gates on status NOT IN ('done', 'dismissed'), so a
	// queued+claimed task still matches.
	routerLog.Info("auto-delegating task",
		"task_id", task.ID, "trigger", trigger.ID, "blueprint", trigger.BlueprintID)

	// Re-read task to get entity-joined display fields the spawner needs.
	fresh, err := r.tasks.GetSystem(ctx, orgID, task.ID)
	if err != nil || fresh == nil {
		if err != nil {
			return "", fmt.Errorf("re-read task: %w", err)
		}
		return "", fmt.Errorf("task %s disappeared before spawn", task.ID)
	}

	blueprintRunID, err := r.spawner.Delegate(*fresh, delegate.DelegateOpts{
		OrgID:               orgID,
		ExplicitBlueprintID: trigger.BlueprintID,
		TriggerType:         "event",
		TriggerID:           trigger.ID,
		TriggeringEventID:   triggeringEventID,
		ActorAgentID:        actorAgentID,
		TaskClaim:           claim,
	})
	if err != nil {
		// Post-B+: nothing to revert status-wise (status stayed 'queued').
		// The claim didn't land either — it rides the blueprint-run insert, so a
		// failure that never committed a blueprint run never committed a claim.
		// This leaves the task in a clean unclaimed-queued state, which is
		// correct.
		//
		// The two anticipated refusals — a replay hitting the fence, a task
		// that went busy under the fire — are outcomes both callers handle
		// by design, so they name themselves instead of colouring the trace
		// red and diluting "traces with errors".
		switch {
		case errors.Is(err, delegate.ErrAlreadyFired):
			span.SetAttributes(telemetry.Outcome("already_fired"))
		case errors.Is(err, delegate.ErrTaskBusy):
			span.SetAttributes(telemetry.Outcome("task_busy"))
		default:
			span.SetStatus(codes.Error, "delegate")
		}
		return "", err
	}
	span.SetAttributes(telemetry.BlueprintRunID(blueprintRunID))
	routerLog.InfoContext(ctx, "started blueprint run for task", "blueprint_run", blueprintRunID, "task_id", task.ID)
	return blueprintRunID, nil
}

// DrainTask is the spawner's hook into the per-task firing queue.
// Called when an auto conversation terminates on the task (any terminal
// status). A completed conversation that left an unresolved artifact still
// counts as terminal here — the artifact is an async sidecar, so it releases
// the task lock and doesn't block downstream processing.
//
// Pops the task's pending firings in FIFO order, validates each against
// current state (task still active? trigger still enabled? breaker still
// under threshold?), and fires the first valid one. Stale firings are
// soft-deleted with a skip_reason and the loop continues. At most one
// firing actually fires per drain — that blueprint run becomes the new
// in-flight for the task and gates further drains naturally. A sibling task on the
// same entity has its own queue and its own drain; neither waits on the
// other.
func (r *Router) DrainTask(orgID, taskID string) {
	// The drain is its own unit of work rather than its caller's: the
	// spawner hooks it off a conversation's terminal (a goroutine whose context
	// is already unwinding) and the sweeper off a periodic tick. Both need
	// every pop to reach a terminal mark — a firing abandoned mid-drain sits
	// in 'draining' until the staleness sweep notices — so the drain holds a
	// detached context instead of borrowing one that is about to die.
	ctx := context.Background()

	// Serialize drains per task. Without this, a fast-terminating conversation
	// fired by an earlier drain can spawn a second DrainTask goroutine
	// that pops the same pending_firings row before the first drain
	// transitions it out of 'pending' — leading to duplicate fireDelegate
	// calls. The MarkFired/MarkSkipped guards on
	// status='pending' protect the row's own mutation but cannot un-fire
	// the duplicate blueprint run. This mutex closes the window: the second
	// drain blocks until the first releases, by which point the firing has
	// landed in a terminal status and the second drain's pop returns the
	// next row (or nothing).
	mu := r.taskDrainLock(taskID)
	mu.Lock()
	defer mu.Unlock()

	for {
		firing, err := r.firings.PopForTask(ctx, orgID, taskID)
		if err != nil {
			routerLog.Error("drain pop failed", "task_id", taskID, "error", err)
			return
		}
		if firing == nil {
			return // queue empty
		}

		blueprintRunID, skipReason, transientErr := r.attemptDrainOne(ctx, orgID, firing)
		if transientErr != nil {
			// Transient failure (DB read, Delegate). PopForTask already
			// claimed this row into 'draining', so release it
			// back to 'pending' rather than leaving it stuck — marking
			// 'skipped_stale' here would permanently drop a queued intent
			// over a temporary problem, and a 'draining' row left
			// unresolved is invisible to HasPendingForTask /
			// ListTasksWithPending and would never be retried. The
			// periodic sweeper or the next conversation terminal will retry
			// once released.
			if err := r.firings.Release(ctx, orgID, firing.ID); err != nil {
				routerLog.Error("release firing after transient drain error failed",
					"firing_id", firing.ID, "task_id", taskID, "error", err)
			}
			if errors.Is(transientErr, errDrainTaskBusy) {
				// Routine gate race, not a failure: another conversation went
				// active between the pop and the fire; the release above
				// re-queues the intent for the busy conversation's own terminal
				// drain.
				routerLog.Info("drain deferred: task busy, firing released for retry",
					"firing_id", firing.ID, "task_id", taskID)
			} else {
				routerLog.Warn("drain transient error, released firing for retry",
					"firing_id", firing.ID, "task_id", taskID, "error", transientErr)
			}
			return
		}
		if blueprintRunID != "" {
			if err := r.firings.MarkFired(ctx, orgID, firing.ID, blueprintRunID); err != nil {
				// Durability race: the blueprint run was created (side-effect
				// committed inside the spawner goroutine) but the UPDATE
				// that records the firing→blueprint-run association failed.
				//
				// Roll the side-effect chain back in reverse: tear down
				// the blueprint run we just spawned — every step of it, since
				// the firing that minted it is being undone — then revert the
				// task to 'queued' so the limbo state (task=delegated + no
				// live blueprint run) is not externally visible. Mirrors what
				// fireDelegate already does when spawner.Delegate itself
				// fails.
				//
				// PopForTask already claimed this row into 'draining'
				// — release it back to 'pending' so a later
				// drain retries it fresh, mirroring the transientErr
				// branch above. Without this the row is stuck in
				// 'draining' forever: PopForTask only ever claims
				// 'pending' rows, and HasPendingForTask /
				// ListTasksWithPending don't see 'draining' rows
				// either, so nothing would ever pick it up again.
				routerLog.Error("mark firing fired failed, rolling back: tearing down blueprint run + reverting task to queued",
					"firing_id", firing.ID, "blueprint_run", blueprintRunID, "error", err)
				// Addressed by blueprint run, which is what Delegate returned
				// and the only id this path holds: its steps' conversations are
				// minted below it, so the teardown resolves them itself.
				if r.spawner != nil {
					cerr := r.spawner.StopBlueprintRun(orgID, blueprintRunID, delegate.StopCauseFiringReverted)
					switch {
					case cerr == nil:
					case errors.Is(cerr, delegate.ErrBlueprintRunConcluded):
						// The run beat the rollback to a terminal of its own,
						// so the thing this teardown exists to prevent — a run
						// still executing under a task that no longer claims it
						// — cannot happen. Recorded rather than silent because
						// an auto run concluding inside the mark-fired window
						// is rare enough to be worth seeing next to the failure
						// above.
						routerLog.Warn("tear down blueprint run after mark-fired failure: run had already concluded, nothing left to stop",
							"firing_id", firing.ID, "blueprint_run", blueprintRunID, "error", cerr)
					default:
						// The rest of the rollback lands regardless, which is
						// what makes this the bad outcome rather than a partial
						// one: the task goes back to 'queued' and the firing
						// back to 'pending' under a blueprint run that, as far
						// as anything here knows, is still executing for
						// nobody.
						routerLog.Error("tear down blueprint run after mark-fired failure: rollback may have left a live run with no task claiming it",
							"firing_id", firing.ID, "blueprint_run", blueprintRunID, "error", cerr)
					}
				}
				r.revertTaskStatus(ctx, orgID, firing.TaskID, "queued")
				if rerr := r.firings.Release(ctx, orgID, firing.ID); rerr != nil {
					routerLog.Error("release firing after mark-fired failure failed",
						"firing_id", firing.ID, "error", rerr)
				}
			}
			return // one fire per drain — the new blueprint run gates the rest
		}
		// Skipped or fire failed; record reason and continue draining.
		if err := r.firings.MarkSkipped(ctx, orgID, firing.ID, skipReason); err != nil {
			// Same stuck-in-'draining' risk as the MarkFired branch above:
			// the skip decision itself is definitive (attemptDrainOne only
			// reaches here with a non-empty skipReason), but persisting it
			// failed, so release the claim rather than strand the row.
			routerLog.Error("mark firing skipped failed", "firing_id", firing.ID, "skip_reason", skipReason, "error", err)
			if rerr := r.firings.Release(ctx, orgID, firing.ID); rerr != nil {
				routerLog.Error("release firing after mark-skipped failure failed",
					"firing_id", firing.ID, "error", rerr)
			}
			return
		}
		routerLog.Debug("skipped firing", "firing_id", firing.ID, "task_id", taskID, "skip_reason", skipReason)
	}
}

// RunDrainSweeper periodically attempts to drain every task that has at
// least one pending firing. The sweeper is the safety net for stuck
// queues: a firing left in 'pending' after a transient validation/fire
// error needs *some* drain to retry it, and the natural trigger
// (notifyDrainer from an auto-conversation terminal) only fires when an auto
// conversation is actively terminating. If nothing's terminating — task has
// no active conversations and no events arrive — the queue would otherwise
// sit indefinitely.
//
// Cadence is 30s by default; tuneable via interval. Each tick lists
// tasks with pending firings (cheap — partial index) and calls
// DrainTask on each. DrainTask's per-task mutex makes the sweeper
// safe to run alongside event-triggered drains: if a drain is already
// running for a task, the sweeper's call blocks then re-pops, which
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
			// scorer pattern. In local mode this collapses to N=1
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

// drainingStaleAfter is how old a 'draining' claim must be before the
// sweeper treats its drainer as dead and requeues the firing. A drain is
// a handful of DB round-trips (spawner.Delegate is a pure enqueue — no
// process spawn, no network beyond Postgres), so minutes of staleness is
// a crashed drainer, not a slow one; generous enough that a live drain
// can never be stolen mid-flight.
const drainingStaleAfter = 2 * time.Minute

// sweepOrg drains every task in a single org that has at least one
// pending firing. Factored out of RunDrainSweeper so the per-org loop
// reads as one statement and per-org errors don't bail the whole
// cycle.
func (r *Router) sweepOrg(ctx context.Context, orgID string) {
	// Crash recovery first: requeue firings stuck in 'draining' past the
	// staleness cutoff (their drainer died between pop and resolve), so
	// the pass below sees and drains them like any queued intent.
	if n, err := r.firings.RequeueStaleDraining(ctx, orgID, time.Now().UTC().Add(-drainingStaleAfter)); err != nil {
		routerLog.Error("drain sweeper: requeue stale draining failed", "org", orgID, "error", err)
	} else if n > 0 {
		routerLog.Warn("drain sweeper: requeued firings orphaned in 'draining' (drainer died mid-drain?)",
			"count", n, "org", orgID)
	}

	ids, err := r.firings.ListTasksWithPending(ctx, orgID)
	if err != nil {
		routerLog.Error("drain sweeper: list tasks with pending failed", "org", orgID, "error", err)
		return
	}
	for _, tid := range ids {
		// Skip a task whose own conversation is still live — its terminal will
		// drain the queue. Scoped to the task, so one task parked indefinitely no
		// longer makes the sweeper step over every sibling task's queue on
		// the same entity, which is how a stopped conversation used to halt
		// triage for a whole pull request.
		active, err := r.conversations.HasActiveAutoConversationForTaskSystem(ctx, orgID, tid)
		if err != nil {
			routerLog.Error("drain sweeper: active-check failed", "task_id", tid, "org", orgID, "error", err)
			continue
		}
		if active {
			continue
		}
		r.DrainTask(orgID, tid)
	}
}

// attemptDrainOne validates a popped firing against current state and
// fires it if everything still holds. Three outcomes:
//
//   - (blueprintRunID, "", nil)         — fire succeeded; caller marks 'fired'.
//   - ("", skipReason, nil)    — definitive "no longer relevant"; caller
//     marks 'skipped_stale'. Reserved for: task_closed (done /
//     dismissed / snoozed — task isn't drain-eligible on the
//     lifecycle axis), trigger_disabled, breaker_tripped,
//     claim_changed (a user took the task over or
//     requeued it after the firing was enqueued, so the bot's
//     original commitment is no longer current; drainer must not
//     fire a phantom bot conversation against a now-user-claimed task).
//   - ("", "", err)            — transient failure (DB read, fire-time);
//     caller leaves the firing in 'pending' state and bails the drain
//     loop. The periodic sweeper or next conversation terminal will retry.
//
// Validation reads from live tables, not from the firing row, so the
// drainer reflects the world *now* — invalidation falls out for free
// from the close cascade and trigger config.
//
// We classify Delegate errors as transient too: even when spawner.Delegate
// refuses (rate-limited GitHub, missing creds, worktree race), the firing
// intent is still valid and worth retrying. The breaker handles the
// "actually broken, repeated failure" case via conversation-level failure
// counts — but only once we've started enough conversations to trip it.
// Until then, retry.
func (r *Router) attemptDrainOne(ctx context.Context, orgID string, firing *domain.PendingFiring) (blueprintRunID, skipReason string, transientErr error) {
	task, err := r.tasks.GetSystem(ctx, orgID, firing.TaskID)
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
	// the task over, producing a phantom bot conversation on a now-user-
	// claimed task.
	if task.ClaimedByAgentID == "" {
		return "", domain.PendingFiringSkipClaimChanged, nil
	}

	trigger, err := r.handlers.GetSystem(ctx, orgID, firing.TriggerID)
	if err != nil {
		return "", "", fmt.Errorf("trigger lookup: %w", err)
	}
	if trigger == nil || trigger.Kind != domain.EventHandlerKindTrigger || !trigger.Enabled {
		return "", domain.PendingFiringSkipTriggerDisabled, nil
	}

	breakerThreshold := derefIntDefault(trigger.BreakerThreshold, 0)
	failures, err := r.tasks.CountConsecutiveFailedConversationsSystem(ctx, orgID, firing.EntityID, r.breakerPromptID(ctx, orgID, trigger.BlueprintID))
	if err != nil {
		return "", "", fmt.Errorf("breaker query: %w", err)
	}
	if failures >= breakerThreshold {
		return "", domain.PendingFiringSkipBreakerTripped, nil
	}

	// The actor is the agent that already claimed this task (guaranteed non-empty
	// by the claim guard above) — the drain re-fires the same bot's commitment, so
	// the new blueprint_run's frozen actor matches the standing task claim. No
	// claim stamp rides this insert: the claim is already the bot's (the guard
	// above just read it), and re-writing it would be the one shape that turns a
	// user's requeue-with-a-live-conversation back into a bot claim.
	id, err := r.fireDelegate(ctx, orgID, task, *trigger, firing.TriggeringEventID, task.ClaimedByAgentID, dbpkg.AgentClaimStamp{})
	if err != nil {
		// The blueprint run for this (event, trigger) already exists — a
		// prior drain attempt fired it (process died before MarkFired), or
		// the immediate path did before this firing was popped. Definitive
		// "no longer relevant": mark skipped_stale so the firing doesn't
		// retry forever. The existing blueprint run gates / drains the entity.
		if errors.Is(err, delegate.ErrAlreadyFired) {
			return "", domain.PendingFiringSkipAlreadyFired, nil
		}
		// A different (event, trigger) went active on the task between
		// this drain's pop and the fenced insert (a fresh immediate fire,
		// or a racing drainer). NOT a skip: the queued intent is still
		// valid — surface it transient-shaped so the caller releases the
		// firing back to 'pending'; the busy conversation's own terminal drain
		// (or the sweeper) retries it.
		if errors.Is(err, delegate.ErrTaskBusy) {
			return "", "", errDrainTaskBusy
		}
		return "", "", fmt.Errorf("fire delegate: %w", err)
	}
	return id, "", nil
}

// errDrainTaskBusy marks the drain-time task-busy race for DrainTask's
// transient branch: same release-and-retry handling, but logged as routine
// (Info) rather than as a Warn-worthy transient failure.
var errDrainTaskBusy = errors.New("routing: task busy at drain fire; firing released for retry")

// revertTaskStatus moves a task's lifecycle axis back to the given
// status and broadcasts the change so the frontend doesn't get stuck
// showing a stale state. Claim cols are intentionally left alone —
// The three axes (lifecycle / claim / conversations) are
// orthogonal, and this helper only touches lifecycle.
//
// The only caller today is the mark-fired-failure rollback path in
// DrainTask: a blueprint run was successfully spawned but the UPDATE that
// records the firing→blueprint-run association failed. The recovery flow
// cancels it, leaves the firing in 'pending' so the next drain
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
func (r *Router) revertTaskStatus(ctx context.Context, orgID, taskID, status string) {
	if _, err := r.tasks.SetStatusSystem(ctx, orgID, taskID, status); err != nil {
		routerLog.Error("failed to revert task status", "task_id", taskID, "status", status, "error", err)
		return
	}
	r.ws.Broadcast(websocket.Event{
		Type:  "task_updated",
		OrgID: orgID,
		Data:  map[string]any{"task_id": taskID, "status": status},
	})
}
