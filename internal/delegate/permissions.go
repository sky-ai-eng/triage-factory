// The browser tool-permission round-trip. When a run is spawned with the
// handler this file builds, every canUseTool prompt the agent raises is
// recorded, surfaced to the browser as a `permission_request` WS event, and the
// agent's turn is parked until the user answers via ResolvePermission (the
// POST .../permissions/{id} endpoint) — or a bounded timeout denies it. The
// broker (s.permPending) is in-memory only and keyed by the tool_use id of the
// call being gated, so a prompt names the same call the transcript does.
//
// Two halves, and the split is load-bearing. The BROKER is the mechanism: the
// parked goroutine, the 1-buffered channel, the deregister-then-drain ordering
// and the timers are what actually unblock the agent, and nothing here drives
// it off a table. The conversation_permissions ROW is the record: it gives a
// live prompt an address (GET .../permissions, so a refresh, a second tab, or a
// cold board load can reconstruct what a fire-once frame told exactly one
// listener) and gives every resolution an audit trail with a machine-readable
// reason — an approve used to leave no trace at all, and a deny survived only as
// prose in a tool_result, where "a human refused this" and "nobody was watching"
// look the same. Every write here is best-effort: failing to record a prompt
// must never become a reason to fail to ask it.
//
// Both runLiveAndDrive call sites (the initial run and the resume) drive
// through this same streaming-input path — direct spawns and gVisor-jailed
// runs alike (the sandbox's bidirectional stdio channel is validated
// end-to-end, so runOneShot is a vestigial fallback, not the production
// sandbox path). What differs by call site is which permission handler answers
// the prompt: an unjailed spawn gets this file's BrowserPermissionHandler; a
// gVisor-jailed one gets AutoApprovePermissionHandler instead — see
// run.go/resume.go for the agentproc.WillSandbox() branch.
//
// This whole file is the SDK runtime's, reached only by a conversation carrying
// that ratchet. Nothing equivalent exists for the native engine, and not
// because it was skipped: prompting presupposes an off-allowlist call to decide,
// and there is no such thing there. The tools it advertises ARE its boundary,
// and a name outside them is dispatched into the jail like any other and
// answered "unknown tool".

package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// The two terminal deny messages a surfaced prompt can resolve to without a
// user answer. They're kept distinct so the absent path (nobody could answer)
// is separable from the present-but-unanswered path (someone was watching but
// never clicked) in run transcripts and downstream tooling.
//
// The text is written for the agent, which reads it as the tool result and has
// no other information about why the call failed. A bare "no operator
// available" reads like a transient service error, so agents retry the same
// call — and burn the rest of the run doing it. Each message therefore says
// what happened (a human gate, not a tool fault), how long it waited, that
// retrying is futile, and what to do instead.

// permDenyNoOperator is the deny reason for a prompt nobody could have
// answered: no answer-capable, focused tab was open in the run's org for the
// whole grace window.
func permDenyNoOperator(grace time.Duration) string {
	return "This tool call required permission from a human, but nobody is watching this run — " +
		"there was no operator on the board or the run's page to approve it. " +
		"The request waited " + humanWait(grace) + " for someone to appear and was then denied automatically. " +
		"This is not a failure of the tool, and retrying it will be denied the same way for as long as the run is unattended. " +
		"Find another way to make progress with the tools you already have. " +
		"If there is genuinely no alternative, stop and state plainly what you needed permission for and why, " +
		"so a human can approve it and resume the run."
}

// permDenyTimedOut is the deny reason for a prompt that was surfaced and left
// unanswered for the full permission window — either someone was present the
// whole time and never clicked, or presence kept re-arming the wait.
func permDenyTimedOut(full time.Duration) string {
	return "This tool call required permission from a human. The request was surfaced for approval but " +
		"nobody answered within " + humanWait(full) + ", so it was denied automatically. " +
		"This is not a failure of the tool, and retrying it will most likely time out the same way. " +
		"Find another way to make progress with the tools you already have. " +
		"If there is genuinely no alternative, stop and state plainly what you needed permission for and why, " +
		"so a human can approve it and resume the run."
}

// humanWait renders a wait window for those agent-facing messages in words
// rather than as a Go duration ("2 minutes 30 seconds", not "2m30s"). Whole
// seconds are enough resolution for a bound that is minutes long in
// production; sub-second windows are only reachable from tests' injected
// timeouts, so they fall back to the Duration's own string rather than
// rounding away to "0 seconds".
func humanWait(d time.Duration) string {
	if d < time.Second {
		return d.String()
	}
	d = d.Round(time.Second)
	mins := int(d / time.Minute)
	secs := int((d % time.Minute) / time.Second)
	switch {
	case mins == 0:
		return countOf(secs, "second")
	case secs == 0:
		return countOf(mins, "minute")
	default:
		return countOf(mins, "minute") + " " + countOf(secs, "second")
	}
}

func countOf(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// defaultPresencePollInterval is how often the presence-gated wait re-checks
// whether an answer-capable, focused tab is present in the run's org. 1s is
// fine-grained enough for a 15s-default grace while staying cheap (an RLock +
// map scan over the hub's clients). Tests inject a shorter value via
// SetPresencePollInterval to drive the absent/present paths deterministically.
const defaultPresencePollInterval = time.Second

// ErrNoPendingPermission is returned by ResolvePermission when no in-flight
// request matches (orgID, conversationID, toolCallID) — it was already answered, timed
// out, or never existed. The permission endpoint maps it to 404.
var ErrNoPendingPermission = errors.New("no pending permission request")

// resolveSDKPermissionMode chooses the initial Claude Code permission posture
// for SDK-runtime delegated conversations. Multi mode is always unattended and
// always starts in auto. Local mode honors the team toggle, falling back to the
// schema default on a missing row or read failure. Native conversations never
// call this helper, so the setting cannot affect their own permission gate.
func (s *Spawner) resolveSDKPermissionMode(ctx context.Context, teamID string) string {
	if runmode.Current() == runmode.ModeMulti {
		return "auto"
	}

	ts := domain.DefaultTeamSettingsFor(runmode.Current() == runmode.ModeMulti)
	if s.teams != nil && teamID != "" {
		if loaded, err := s.teams.GetSettingsSystem(ctx, teamID); err == nil {
			ts = loaded
		} else {
			delegateLog.Warn("load team settings for auto mode failed; using default", "team", teamID, "error", err)
		}
	}
	if ts.AutoModeEnabled {
		return "auto"
	}
	return ""
}

// pendingPermission is one in-flight browser permission prompt: the 1-buffered
// channel the handler goroutine is parked on, plus the owning org so a resolve
// from another tenant can't satisfy it. The owning run is encoded in the broker
// key (see permKey), not stored here.
type pendingPermission struct {
	ch    chan agentproc.PermissionDecision
	orgID string
}

// permKey is the broker key for a pending prompt. The identifier is the SDK's
// tool_use id for the gated call, which is unique in practice — but the
// composite costs nothing and keeps each run's prompts isolated rather than
// resting on that being true across runs. The NUL separator can't appear in a
// run uuid or a tool_use id, so the composite is unambiguous.
func permKey(conversationID, toolCallID string) string {
	return conversationID + "\x00" + toolCallID
}

// permTimeout is how long a surfaced prompt waits for an answer before denying.
// It is kept strictly below idleTimeout() so a pending prompt always resolves
// before the run could idle-hibernate mid-prompt (which would tear the warm
// process down with the reader goroutine still parked in the handler). Half the
// idle window is a clean strictly-less value for any idle, including the short
// ones tests inject.
func (s *Spawner) permTimeout() time.Duration {
	return s.idleTimeout() / 2
}

// AbsentAutoDeny captures the per-run, presence-gated fast-deny policy
// (TFAC-392), resolved once at spawn time (resolveAbsentAutoDeny) and captured
// in the handler closure so there's no per-prompt DB read.
//
//   - enabled == false → today's behavior exactly: the prompt waits the full
//     permTimeout() regardless of presence (a safe no-op when the team has the
//     toggle off).
//   - enabled == true → an unattended prompt (no answer-capable, focused tab in
//     the run's org) is denied after grace instead of the full window; presence
//     re-arms the wait to the full timeout. grace is pre-clamped to
//     [1s, permTimeout()) so the absent path can never exceed the full window —
//     the "total wait < idleTimeout()" invariant holds either way.
type AbsentAutoDeny struct {
	enabled bool
	grace   time.Duration
}

// resolveAbsentAutoDeny reads the run's team settings once (admin pool — the run
// goroutine has no JWT claims) and clamps the grace to a sane range. A nil teams
// store, empty teamID, or read failure falls back to the schema defaults, so the
// handler is always built with a usable policy. Called at spawn time, not per
// prompt.
func (s *Spawner) resolveAbsentAutoDeny(ctx context.Context, teamID string) AbsentAutoDeny {
	ts := domain.DefaultTeamSettingsFor(runmode.Current() == runmode.ModeMulti)
	if s.teams != nil && teamID != "" {
		if loaded, err := s.teams.GetSettingsSystem(ctx, teamID); err == nil {
			ts = loaded
		} else {
			delegateLog.Warn("load team settings for absent-auto-deny failed; using defaults", "team", teamID, "error", err)
		}
	}
	if !ts.PermissionAbsentAutodenyEnabled {
		return AbsentAutoDeny{enabled: false}
	}
	grace := time.Duration(ts.PermissionAbsentGraceMS) * time.Millisecond
	return AbsentAutoDeny{enabled: true, grace: clampGrace(grace, s.permTimeout())}
}

// AbsentGraceMinSeconds and AbsentGraceMaxSeconds bound the unattended-prompt
// grace window the team setting exposes (the slider range in the UI and the
// clamp the settings API applies). The floor mirrors clampGrace's 1s minimum;
// the ceiling is the largest whole second strictly below the production
// permTimeout() (DefaultIdleHibernateTimeout / 2), so any value a user can pick
// round-trips through clampGrace unchanged under the default idle timeout. The
// runtime clampGrace still re-clamps against the LIVE permTimeout() as defense
// in depth (and to absorb a test-injected short idle), so these are the
// advertised UI/HTTP bounds, not the only line of defense.
const (
	AbsentGraceMinSeconds = 1
	AbsentGraceMaxSeconds = int(DefaultIdleHibernateTimeout/time.Second)/2 - 1
)

// clampGrace keeps the absent-deny grace in [1s, full) so it can neither
// collapse to an instant deny on a bad value nor invert the load-bearing
// "absent wait ≤ full ≤ permTimeout() < idleTimeout()" ordering. full is
// permTimeout() (minutes in production), so the upper guard only ever bites a
// misconfigured value; the lower floor of 1s mirrors the HTTP-layer floor.
func clampGrace(grace, full time.Duration) time.Duration {
	const minGrace = AbsentGraceMinSeconds * time.Second
	if grace < minGrace {
		grace = minGrace
	}
	if grace >= full {
		// full is unusually short (only realistic under a test's tiny injected
		// idle). Pull grace strictly below it; never return a non-positive value.
		grace = full - full/10
		if grace <= 0 {
			grace = full / 2
		}
	}
	return grace
}

// BrowserPermissionHandler returns a PermissionHandler that surfaces each
// canUseTool prompt to the browser and parks the agent's turn until the user
// answers (ResolvePermission) or the bounded wait denies it. Safe to call from
// the agentproc reader goroutine: it registers a 1-buffered channel keyed by the
// request id, records the prompt, broadcasts a `permission_request`, waits, and
// always deregisters.
//
// claimID is the engagement raising the prompt. It scopes the durable row so a
// prompt outliving its own process reads as gone (ListPending derives pending
// against the conversation's ACTIVE claim), which is what makes the read
// correct after a crash that ran no cleanup at all.
//
// absent is the presence-gated fast-deny policy (TFAC-392). With it disabled the
// wait is exactly the legacy full-permTimeout() select; with it enabled an
// unattended prompt denies after the grace window with the nobody-was-watching
// reason (permDenyNoOperator), while a present, focused board/run tab extends
// the wait to the full window. Either way the total wait never exceeds
// permTimeout() and a 200-acknowledged decision is never dropped.
func (s *Spawner) BrowserPermissionHandler(orgID, conversationID, claimID string, absent AbsentAutoDeny) agentproc.PermissionHandler {
	// The engagement's trace root, captured once when the handler is built —
	// which is during setup, while the root is still live — rather than per
	// prompt, since by the time a human is looking at a card the engagement
	// has long since been deregistered.
	engagement := s.engagementSpanContext(conversationID)
	return func(req agentproc.PermissionRequest) agentproc.PermissionDecision {
		// A permission prompt is the one place a run stops for a person, and
		// the run's own accounting deliberately discounts that wait — so
		// without this span the gap is invisible in every duration TF records.
		// Punctual and linked, like everything after agent-live.
		//
		// The tool name is NOT an attribute: the input it came with is agent
		// text, and the name alone is close enough to it that the hygiene rule
		// (opaque ids and closed enums only) says no. What the span carries is
		// how long the wait was and how it ended.
		_, span := startPunctualLinked(context.Background(), engagement, conversationID, "permission.prompt",
			telemetry.OrgID(orgID))
		decided := "denied"
		defer func() {
			span.SetAttributes(telemetry.Outcome(decided))
			span.End()
		}()

		key := permKey(conversationID, req.ToolCallID)
		ch := make(chan agentproc.PermissionDecision, 1)
		s.mu.Lock()
		s.permPending[key] = &pendingPermission{ch: ch, orgID: orgID}
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			delete(s.permPending, key)
			s.mu.Unlock()
		}()

		// full is the prompt's server-side deadline. It stays the FULL window
		// even when absent-deny may fire sooner: the card's client TTL is a
		// backstop, and an absent-deny eagerly broadcasts permission_resolved
		// to drop the card the moment it fires.
		full := s.permTimeout()

		// Record BEFORE broadcasting, so the frame is a hint pointing at
		// something that already exists: a client that refetches the instant it
		// arrives finds the prompt, and a client that never saw the frame at
		// all (refresh, second tab, cold load) finds it too.
		s.recordPermissionRequest(orgID, conversationID, claimID, full, req)

		// Hub.Broadcast is nil-receiver-safe, so no guard is needed for the
		// hub-less test spawner. The payload is deliberately just the id: the
		// prompt itself is read from
		// GET /api/agent/conversations/{id}/permissions, so the socket is a
		// refetch trigger like artifact_updated rather than the sole carrier of
		// state no other path can reach. That costs one round-trip before a
		// prompt renders, which is nothing against a human-paced, rare event.
		s.wsHub.Broadcast(websocket.Event{
			Type:           "permission_request",
			OrgID:          orgID,
			ConversationID: conversationID,
			Data:           map[string]any{"tool_call_id": req.ToolCallID},
		})

		decision, got, reason, why := s.awaitPermission(ch, orgID, conversationID, full, absent)
		if got {
			// A person answered. "allow" / "deny" come from the decision's own
			// behavior field, which is a closed vocabulary the wrapper sets.
			decided = "answered_" + decision.Behavior
			return decision
		}
		// A deadline fired (full timeout, or the absent grace). Deregister
		// BEFORE deciding, then drain once: a decision already in the buffer
		// was acknowledged with a 200 (ResolvePermission sends under s.mu while
		// the entry is present), so honor it — covering both the instant where
		// the resolve and the deadline were simultaneously ready and the gap
		// before the deferred delete. After the delete, a late resolve reliably
		// misses (404).
		s.mu.Lock()
		delete(s.permPending, key)
		s.mu.Unlock()
		select {
		case d := <-ch:
			// Buffered under s.mu while the entry was live, so the row was
			// already stamped by the resolver that acknowledged it.
			decided = "answered_" + d.Behavior
			return d
		default:
			// why is the auto-deny's reason code ('timeout' / 'absent'), the
			// distinction the whole absent-deny policy exists to make.
			if why != "" {
				decided = why
			}
			// The prompt is no longer answerable. Stamp the row with WHY —
			// 'timeout' and 'absent' are the whole reason this column exists,
			// since the agent-facing prose below says the same thing in
			// English that is free to be reworded — then tell every surface
			// showing it to drop it now instead of waiting out its own client
			// TTL (ResolvePermission already broadcasts for the answered case).
			s.resolvePermissionRow(orgID, conversationID, req.ToolCallID, domain.PermissionStateDenied, why, "")
			s.broadcastPermissionResolved(orgID, conversationID, req.ToolCallID)
			return agentproc.PermissionDecision{Behavior: "deny", Message: reason}
		}
	}
}

// recordPermissionRequest writes the durable 'pending' row for a freshly
// surfaced prompt. Best-effort by construction: a store that isn't wired, or a
// write that fails, costs the prompt its address (a refresh won't rebuild it)
// but must never cost the agent its question — the broker has already parked
// and the browser is about to be told, so returning early here would strand the
// run for the full window over a bookkeeping failure.
func (s *Spawner) recordPermissionRequest(orgID, conversationID, claimID string, full time.Duration, req agentproc.PermissionRequest) {
	if s.permissions == nil {
		return
	}
	now := time.Now().UTC()
	expires := now.Add(full)
	_, err := s.permissions.Create(context.Background(), orgID, domain.ConversationPermission{
		ConversationID: conversationID,
		ClaimID:        claimID,
		ToolCallID:     req.ToolCallID,
		ToolName:       req.ToolName,
		Input:          req.Input,
		Title:          req.Title,
		State:          domain.PermissionStatePending,
		RequestedAt:    now,
		ExpiresAt:      &expires,
	})
	if err != nil {
		delegateLog.Warn("record pending tool permission failed; prompt is live but not reloadable",
			"conversation", conversationID, "tool_call_id", req.ToolCallID, "error", err)
	}
}

// resolvePermissionRow stamps a prompt's terminal on its durable row and
// returns what the store settled: the row when this call's decision landed,
// nil when the store has nothing wired, the write failed (logged here — same
// best-effort posture as the insert, since the broker has already decided and
// the agent is already unblocked by the time this runs, so a failure loses an
// audit row rather than changing what happened), or the guard declined
// because the prompt wasn't pending. A caller that only needs the
// best-effort record (the auto-deny paths below) can discard the return
// value; resolvePermissionLocal's HTTP-driven path uses it to answer with
// the row it actually settled rather than asserting one. decidedBy is set
// only for a human answer.
func (s *Spawner) resolvePermissionRow(orgID, conversationID, toolCallID, state, reason, decidedBy string) *domain.ConversationPermission {
	if s.permissions == nil {
		return nil
	}
	resolved, err := s.permissions.Resolve(context.Background(), orgID, conversationID, toolCallID, state, reason, decidedBy)
	if err != nil {
		delegateLog.Warn("record tool permission resolution failed",
			"conversation", conversationID, "tool_call_id", toolCallID, "state", state, "reason", reason, "error", err)
		return nil
	}
	return resolved
}

// ExpirePermissionsForClaim marks an engagement's still-open prompts expired
// when it releases its claim, so the audit history reads as settled rather than
// leaving questions that look live forever.
//
// Best-effort on purpose, and NOTHING depends on it landing: ListPending
// derives pending against the conversation's active claim, so a released
// claim's prompts stop being pending whether or not this ever ran. That is
// exactly what makes the arrangement survive the crash it exists for — a
// process that dies mid-prompt runs no cleanup by definition.
func (s *Spawner) ExpirePermissionsForClaim(orgID, claimID string) {
	if s.permissions == nil || claimID == "" {
		return
	}
	if err := s.permissions.ExpireForClaim(context.Background(), orgID, claimID); err != nil {
		delegateLog.Warn("expire pending tool permissions for released claim failed",
			"claim_id", claimID, "error", err)
	}
}

// awaitPermission blocks until the prompt is answered or a deadline fires. It
// returns (answer, true, "", "") when a decision arrives on ch — the caller
// returns its decision verbatim — and (zero, false, reason, why) when a
// deadline elapses, where reason is the agent-facing deny message the caller
// uses after its deregister-then-drain and why is the machine-readable
// domain.PermissionReason* for the audit row. The two are deliberately
// separate: the prose is written for an agent and free to be reworded, the
// reason is a stored enum nothing should have to parse English to recover.
//
// With absent.enabled false this is the exact legacy select: ch vs the full
// window. With it enabled the wait polls presence on a short ticker and tracks
// two deadlines: the full window (always), and the grace clock (only while no
// answer-capable, focused tab is present in the run's org). The effective
// deadline is whichever is sooner; presence flipping to present re-arms the wait
// to the full window (disarming graceTimer), and flipping back restarts the
// grace clock. The grace deadline never exceeds the full window (clampGrace), so the
// total wait is bounded by permTimeout() in every branch.
func (s *Spawner) awaitPermission(ch chan agentproc.PermissionDecision, orgID, conversationID string, full time.Duration, absent AbsentAutoDeny) (agentproc.PermissionDecision, bool, string, string) {
	if !absent.enabled {
		select {
		case d := <-ch:
			return d, true, "", ""
		case <-time.After(full):
			return agentproc.PermissionDecision{}, false, permDenyTimedOut(full), domain.PermissionReasonTimeout
		}
	}

	// Explicit timers fire the deadlines exactly (no up-to-one-tick overshoot):
	//   - fullTimer is the hard ceiling, armed for the whole window so the total
	//     wait never exceeds permTimeout() — the load-bearing "< idleTimeout()"
	//     invariant holds precisely, not within a poll interval.
	//   - graceTimer is the absent deadline, armed only while unattended. The
	//     ticker's sole job is to re-read presence and arm/disarm + reset this
	//     timer on the present↔absent edges, so a present→absent flip restarts
	//     the grace clock from that moment.
	fullTimer := time.NewTimer(full)
	defer fullTimer.Stop()
	ticker := time.NewTicker(s.presencePollInterval())
	defer ticker.Stop()

	present := s.presentFor(orgID, conversationID)
	graceTimer := time.NewTimer(absent.grace)
	defer graceTimer.Stop()
	if present {
		// Present at prompt time: disarm the grace clock until/unless the tab
		// leaves. clampGrace keeps grace < full, so a fresh-absent run's grace
		// deadline is always strictly inside the full window.
		stopTimer(graceTimer)
	}

	for {
		select {
		case d := <-ch:
			return d, true, "", ""
		case <-fullTimer.C:
			// Watched-but-unanswered for the whole window (or grace never fired
			// because presence kept re-arming) — the legacy timeout.
			return agentproc.PermissionDecision{}, false, permDenyTimedOut(full), domain.PermissionReasonTimeout
		case <-graceTimer.C:
			// Only armed while unattended, so reaching it means genuinely nobody
			// could answer within the grace. The reported wait is the grace
			// window, which is measured from the moment the last operator left,
			// not from when the prompt was raised.
			return agentproc.PermissionDecision{}, false, permDenyNoOperator(absent.grace), domain.PermissionReasonAbsent
		case <-ticker.C:
			now := s.presentFor(orgID, conversationID)
			switch {
			case now && !present:
				// became present: stop the absent clock, re-arm to full window.
				// If graceTimer.C was simultaneously ready and select happened to
				// pick this branch, stopTimer drains that pending fire — a "grace
				// elapsed exactly as someone arrived" resolves as still-attended,
				// which is the right call.
				stopTimer(graceTimer)
			case !now && present:
				// became absent: restart the grace clock from now.
				resetTimer(graceTimer, absent.grace)
			}
			present = now
		}
	}
}

// stopTimer halts a timer and drains a pending fire so a later Reset starts
// clean. Safe because awaitPermission is the timer's sole reader and only calls
// this from the ticker branch — a path it cannot reach after the timer already
// fired (that returns).
func stopTimer(t *time.Timer) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
}

// resetTimer re-arms a (possibly already-fired) timer to d, draining any pending
// fire first so the new deadline is measured from now.
func resetTimer(t *time.Timer, d time.Duration) {
	stopTimer(t)
	t.Reset(d)
}

// presentFor reports whether any answer-capable, focused tab in orgID is on the
// board or this run's detail page (TFAC-392). In multi mode, once
// SetPresenceChecker has wired a fleet-wide checker (TFAC-584), this
// consults it — local Hub state OR the ws_presence table — so a reviewer
// connected to a different pod than the one running this run still
// counts. Otherwise (local mode, or before wiring) it falls back to the
// hub directly, which is nil-receiver-safe (the hub-less test spawner
// reads as "nobody present").
func (s *Spawner) presentFor(orgID, conversationID string) bool {
	s.mu.Lock()
	pc := s.presence
	s.mu.Unlock()
	if pc != nil {
		return pc.PresentFor(context.Background(), orgID, conversationID)
	}
	return s.wsHub.PresentFor(orgID, conversationID)
}

// presencePollInterval is the absent-deny presence poll cadence, falling back to
// the default when unset. Tests inject a short value via SetPresencePollInterval.
func (s *Spawner) presencePollInterval() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.permPresencePoll > 0 {
		return s.permPresencePoll
	}
	return defaultPresencePollInterval
}

// SetPresencePollInterval overrides the absent-deny presence poll cadence. Tests
// inject a short value to drive the absent/present paths deterministically; the
// in-flight handler reads it through presencePollInterval() at wait start.
func (s *Spawner) SetPresencePollInterval(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.permPresencePoll = d
}

// broadcastPermissionResolved tells the browser a pending prompt reached a
// terminal resolution (answered or timed out) so every surface rendering it —
// the board card and the run-detail dock, or two board tabs — drops it promptly
// instead of waiting out its own client-side TTL. The client TTL stays as a
// backstop for a dropped/missed event. Hub.Broadcast is nil-receiver-safe.
func (s *Spawner) broadcastPermissionResolved(orgID, conversationID, toolCallID string) {
	s.wsHub.Broadcast(websocket.Event{
		Type:           "permission_resolved",
		OrgID:          orgID,
		ConversationID: conversationID,
		Data: map[string]any{
			"tool_call_id": toolCallID,
		},
	})
}

// AutoApprovePermissionHandler returns a PermissionHandler for gVisor-sandboxed
// delegated runs that allows every canUseTool prompt immediately instead of
// routing it through BrowserPermissionHandler's presence-gated wait.
//
// Delegated runs are unattended by design — a task-triggered run fires with no
// operator expected at the console — so that presence-gated wait almost always
// resolves via its own absent-grace deny anyway; it was friction without a
// live decision behind it. What actually bounds an off-allowlist tool
// call's blast radius in the sandboxed case is structural, not the prompt: the
// gVisor sandbox (netns egress lockdown + Property B credential-free env), the
// static --allowedTools allowlist (unchanged by this handler), and the
// enumerated agenthost RPC surface (no merge/delete/dispatch method exists to
// unlock). Local-mode runs have no gVisor boundary, so they keep routing
// through BrowserPermissionHandler — see the agentproc.WillSandbox() branch in
// run.go/resume.go.
func (s *Spawner) AutoApprovePermissionHandler(conversationID string) agentproc.PermissionHandler {
	return func(req agentproc.PermissionRequest) agentproc.PermissionDecision {
		delegateLog.Info("off-allowlist tool auto-approved in the sandbox", "conversation", conversationID, "tool_call_id", req.ToolCallID, "tool", req.ToolName)
		return agentproc.PermissionDecision{Behavior: "allow"}
	}
}

// ResolvePermission delivers a decision to a pending permission request,
// unblocking the handler goroutine parked on it. The send is non-blocking (the
// channel is 1-buffered with a single receiver). A request that isn't pending —
// or whose pending entry was registered for a different org — is
// ErrNoPendingPermission for the caller to map to 404. The org check here is a
// broker-level backstop; the permission endpoint additionally authorizes the
// run under RLS (team-level) before calling this.
//
// The send happens under s.mu, atomic with the pending-presence check. That
// makes a nil return (the endpoint's 200) a real promise: the decision was
// buffered while the entry was registered, and the handler's timeout path —
// which deregisters under the same mutex before its final drain — is
// guaranteed to observe it. A 200-acknowledged decision is never dropped.
// ResolvePermission answers a pending browser permission prompt, routing
// through the same local-first / cross-pod seam as Interrupt/Steer
// (TFAC-585): resolvePermissionLocal handles the case this process holds
// the broker entry; on a local miss where this process also holds no live
// handle for the run at all (getProc), it resolves a live remote owner and
// signals it, waiting up to DefaultPermissionAckTimeout. A local miss
// where THIS process DOES own the run's live process (the tool_call_id
// itself just isn't/no-longer pending — already answered, timed out, or
// never existed) is genuinely stale/not-found, never a routing question,
// so it short-circuits before ever trying remote.
// decidedBy is the answering user, stamped onto the audit row so an approval
// names who granted it — the fact the old arrangement recorded nowhere at
// all. The returned permission is the durable row this call's decision
// settled — nil when it isn't available locally (the cross-pod routed path:
// the row was written on the remote owner, not this process) or the store
// isn't wired; a nil error always means the broker delivered the decision to
// the agent regardless, since that promise is independent of the audit
// write.
func (s *Spawner) ResolvePermission(orgID, conversationID, toolCallID, decidedBy string, d agentproc.PermissionDecision) (*domain.ConversationPermission, error) {
	resolved, err := s.resolvePermissionLocal(orgID, conversationID, toolCallID, decidedBy, d)
	if err == nil || !errors.Is(err, ErrNoPendingPermission) {
		return resolved, err
	}
	if s.getProc(conversationID) != nil {
		return nil, ErrNoPendingPermission
	}
	s.mu.Lock()
	conversationSignals := s.conversationSignals
	s.mu.Unlock()
	if conversationSignals == nil {
		return nil, ErrNoPendingPermission
	}
	return nil, s.routePermission(orgID, conversationID, toolCallID, decidedBy, d)
}

// resolvePermissionLocal is ResolvePermission's original N=1 body: resolve
// the in-memory broker entry and buffer the decision.
func (s *Spawner) resolvePermissionLocal(orgID, conversationID, toolCallID, decidedBy string, d agentproc.PermissionDecision) (*domain.ConversationPermission, error) {
	s.mu.Lock()
	p, ok := s.permPending[permKey(conversationID, toolCallID)]
	if !ok || p.orgID != orgID {
		s.mu.Unlock()
		return nil, ErrNoPendingPermission
	}
	select {
	case p.ch <- d:
		s.mu.Unlock()
		// The decision is buffered (a real 200 promise), so this is the moment
		// the answer became true — stamp the row here rather than on the parked
		// goroutine, and stamp it BEFORE the broadcast: the frame is a refetch
		// trigger, so a sibling tab that reacts instantly must find the prompt
		// already gone rather than racing a write it can't see.
		state := domain.PermissionStateDenied
		if d.Behavior == "allow" {
			state = domain.PermissionStateAllowed
		}
		resolved := s.resolvePermissionRow(orgID, conversationID, toolCallID, state, domain.PermissionReasonUser, decidedBy)
		// Tell other surfaces showing this prompt to drop it now — broadcast
		// outside s.mu so the hub's own locking never nests under the broker
		// mutex.
		s.broadcastPermissionResolved(orgID, conversationID, toolCallID)
		return resolved, nil
	default:
		// Slot already filled by a racing resolve — treat as no-longer-pending.
		s.mu.Unlock()
		return nil, ErrNoPendingPermission
	}
}

// routePermission is ResolvePermission's cross-pod path: marshal the
// decision onto a conversation_signals row targeting the run's live remote owner
// and wait for the ack, up to DefaultPermissionAckTimeout (longer than
// interrupt/steer's TF_SIGNAL_ACK_TIMEOUT — the browser is synchronously
// blocked on this response). A "gone" or "stale" ack both map to
// ErrNoPendingPermission: gone means no live process to answer (the same
// user-facing outcome as never having been pending), stale means the
// tool_call_id was already resolved on the owner.
func (s *Spawner) routePermission(orgID, conversationID, toolCallID, decidedBy string, d agentproc.PermissionDecision) error {
	payload, err := json.Marshal(permissionPayload{
		ToolCallID:   toolCallID,
		Behavior:     d.Behavior,
		Message:      d.Message,
		UpdatedInput: d.UpdatedInput,
		DecidedBy:    decidedBy,
	})
	if err != nil {
		return fmt.Errorf("marshal permission payload: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), DefaultPermissionAckTimeout)
	defer cancel()
	result, live, err := s.sendSignalAndAwaitAck(ctx, orgID, conversationID, domain.ConversationSignalPermission, string(payload))
	if err != nil {
		return err
	}
	if !live || result == domain.ConversationSignalAckGone || result == domain.ConversationSignalAckStale {
		return ErrNoPendingPermission
	}
	return nil
}
