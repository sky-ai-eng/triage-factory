package routing

import (
	"context"
	"errors"
	"fmt"

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
// The task gate is the serialization point: at most one conversation in
// flight per task, whichever trigger it came from and whoever started it. A
// sibling task on the same entity has its own gate and fires independently.
// If the gate is closed (this task's own conversation is live, or older
// firings are queued for FIFO fairness), the firing folds into the live
// conversation or enqueues onto pending_firings instead of being dropped
// silently.
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
// one-active-run index, the firing kind's (task, trigger) key) make that
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

	// Per-task gate. Closed if a conversation is live on THIS TASK, or any
	// unsettled firing — ready, leased or parked — is already queued for it
	// (FIFO fairness; a parked row still holds its key). Compose the gate
	// from its two halves: ConversationStore owns the conversation-shaped
	// predicate, PendingFiringsStore owns the queue-shaped one. The gate
	// opens only when neither side blocks.
	//
	// Whatever minted the live conversation holds the gate. A task has one, and
	// a human's delegation is as much the task's live conversation as an
	// auto-fired one — so an event landing on a task a person is working gets
	// folded into their agent's context by the busy branch below, rather than
	// racing a second agent against it.
	//
	// The task is the unit, not the entity. A task is one situation needing
	// attention — that is what its (entity, event type, discriminator) dedup
	// key means — so two situations on one pull request may each have an
	// agent working. Gating on the entity conflated them, and once a
	// conversation could park indefinitely (a stop freezes its blueprint
	// 'running' by design) one such conversation held the gate shut for every
	// other situation on that entity, with nothing left to reopen it.
	//
	// The live-conversation read resolves the conversation's ID rather than
	// a bool: a busy gate is the additive-injection path, and folding the new
	// event into the conversation needs the conversation.
	activeConversationID, err := r.conversations.LiveConversationIDForTaskSystem(ctx, orgID, task.ID)
	if err != nil {
		routerLog.Error("task gate live-conversation query failed", "task_id", task.ID, "error", err)
		return false, fmt.Errorf("task gate live-conversation query: %w", err)
	}
	hasActive := activeConversationID != ""
	hasQueued := false
	if !hasActive {
		hasQueued, err = r.firings.HasUnsettledForTask(ctx, orgID, task.ID)
		if err != nil {
			routerLog.Error("task gate queued query failed", "task_id", task.ID, "error", err)
			return false, fmt.Errorf("task gate queued query: %w", err)
		}
	}
	if hasActive || hasQueued {
		// A busy gate now always means THIS task's own conversation is live,
		// so absorption is the default rather than a same-task special case:
		// fold the event into the conversation instead of deferring a second
		// one. Only a firing with no live conversation to fold into (hasQueued
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

	// The firing is one transaction (see BlueprintStore.CreateRunWithFirstStepSystem):
	// the owner consolidation, the blueprint_run row, "the bot owns this task",
	// and step 0's conversation are one durable write, so there is no window
	// where the conversation is live and the board still shows the task free
	// or owned by the wrong team.
	//
	// actingTeamID travels as the owner to consolidate to, because an
	// auto-fired conversation inherits conversations.team_id from
	// tasks.team_id at insert: a firing by a team other than the creation-time
	// owner (the owner has auto-delegation disabled and a lower-priority team
	// is firing) would otherwise open under the stale owner. The claim stamp
	// consolidates too, but cannot be relied on for it — a stamp refusal (a
	// user claiming mid-fire, or the bot already owning the task) leaves the
	// run committed and the owner unmoved. A refusal is otherwise not a
	// rollback: the human wins the claim, the conversation still runs, and the
	// firing worker's claim_changed guard keeps later firings off it.
	if _, err := r.fireDelegate(ctx, orgID, task, trigger, triggeringEventID, agentID, claimStamp(agentID, actingTeamID), actingTeamID); err != nil {
		// A replayed event (at-least-once queue) whose first blueprint run
		// already committed hits the (event, trigger) fence and comes back
		// as ErrAlreadyFired. Clean skip — the original blueprint run + its
		// claim stand, so we must NOT re-stamp the claim or log an error.
		if errors.Is(err, delegate.ErrAlreadyFired) {
			routerLog.Info("auto-delegate skipped: event already fired this trigger (replay)",
				"task_id", task.ID, "trigger", trigger.ID, "event_id", triggeringEventID)
			return false, nil
		}
		// The gate read said idle, but something else went live on this task
		// before our insert — the one-active-run index is the authority, the
		// gate just a fast-path. The intent is still valid: defer it onto
		// pending_firings exactly as the gate's busy branch would have,
		// instead of dropping it.
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
// entirely, and the caller replays instead. The kind's (task, trigger) key
// makes that replay collapse rather than double-queue.
func (r *Router) enqueueBusyFiring(ctx context.Context, orgID, entityID string, task *domain.Task, trigger domain.EventHandler, triggeringEventID, actingTeamID, agentID string) (bool, error) {
	// The claim rides the insert's transaction: a queued firing commits the
	// bot to this task just as a fired blueprint run does, so a failed enqueue
	// leaves no phantom claim and a landed one is never claim-less. The store skips the
	// stamp on the collapse path, where the already-queued duplicate's own
	// enqueue made the commitment.
	inserted, claimed, err := r.firings.Enqueue(ctx, orgID, entityID, task.ID, trigger.ID, triggeringEventID, claimStamp(agentID, actingTeamID))
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

// tryAdditiveInjection folds a firing into its task's live conversation via
// the cross-pod-aware injection seam, instead of deferring a second
// conversation onto pending_firings. Whatever minted that conversation — an
// earlier auto-fire, or a person's own delegation — it is the one the task is
// about, and the event belongs in front of it. conversationID is its id,
// resolved by the caller from the firing's own task, so it belongs to that
// task by construction with no separate ownership check to make. Returns true when the caller should
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

// syncClaimAfterCommit reads back the claim and the owner that rode the
// firing's transaction. The fire path is the one commitment whose store call
// the router doesn't make itself — it goes through the spawner — so the
// outcome comes from the row rather than a return value. This is a read of the
// claim column, not a derivation from conversation liveness: a user who won
// the race mid-fire reads back as the owner and neither the in-memory task nor
// the Board is told otherwise.
//
// Best-effort. A failed read leaves the router's in-memory copy stale for the
// rest of this event's trigger loop, which is what a failed stamp used to do
// on every fire; the committed row is already correct either way.
func (r *Router) syncClaimAfterCommit(ctx context.Context, orgID string, task *domain.Task, agentID string) {
	fresh, err := r.tasks.GetSystem(ctx, orgID, task.ID)
	if err != nil || fresh == nil {
		routerLog.Warn("could not read back the task claim committed with the blueprint run", "task_id", task.ID, "error", err)
		return
	}
	// An empty agentID is test wiring with no agents store: no claim was
	// stamped, so nothing moved and there is nothing to announce — but the
	// owner consolidation still committed, so the sync above is not skipped.
	moved := agentID != "" && task.ClaimedByAgentID != agentID && fresh.ClaimedByAgentID == agentID
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

// fireDelegate fires the spawner for a (task, trigger) and returns the
// blueprint-run ID on success — what the firing worker records as the run a
// queued firing materialized into.
//
// triggeringEventID is the event instance driving this fire:
// the immediate path passes tryAutoDelegate's event id, the firing worker
// passes the queued firing's. It threads into DelegateOpts so the
// blueprint-run insert is fenced on (triggering_event_id, trigger_id); a
// replayed event whose first blueprint run already committed surfaces as
// delegate.ErrAlreadyFired, which both callers treat as a clean skip
// rather than a duplicate fire.
//
// actorAgentID is the executing bot, resolved once by the caller — the immediate
// path passes the agent it resolved up front (and stamps the same id as the
// claim); the firing worker passes the firing's already-stamped task claim. It's
// frozen onto blueprint_runs.actor_agent_id at mint and inherited by every step.
//
// claim is the task claim to write inside the firing's own transaction, and is
// deliberately NOT derived from actorAgentID. The immediate path passes
// one: this fire is the commitment, so the claim must land with it. The firing
// worker passes the zero stamp: its firing's claim was committed by the enqueue
// that queued it, and the worker has just re-validated that the claim is
// still the bot's — re-stamping here would silently re-impose a claim a user
// cleared by requeueing in the interim.
//
// ownerTeamID is the team the task's card consolidates to inside that same
// transaction, and it is separate from the claim for the reason the claim is
// separate from the actor: a stamp refusal still commits the run, so the
// consolidation cannot ride on the stamp landing. Empty leaves the owner
// alone — the firing worker's consolidation happened at the enqueue that queued
// the firing, and re-imposing it here would fight a user's requeue the same
// way a re-stamped claim would.
func (r *Router) fireDelegate(ctx context.Context, orgID string, task *domain.Task, trigger domain.EventHandler, triggeringEventID, actorAgentID string, claim dbpkg.AgentClaimStamp, ownerTeamID string) (string, error) {
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
		OwnerTeamID:         ownerTeamID,
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
