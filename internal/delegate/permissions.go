// The browser tool-permission round-trip. When a run is spawned with the
// handler this file builds, every canUseTool prompt the agent raises is
// surfaced to the browser as a `permission_request` WS event and the agent's
// turn is parked until the user answers via ResolvePermission (the
// POST .../permissions/{id} endpoint) — or a bounded timeout denies it. The
// broker (s.permPending) is in-memory only and keyed by the SDK-generated
// request id.
//
// Both runLiveAndDrive call sites (the initial run and the resume) drive
// through this same streaming-input path — local direct runs and multi-mode
// gVisor-sandboxed runs alike (the sandbox's bidirectional stdio channel is
// validated end-to-end, so runOneShot is a vestigial fallback, not the
// production sandbox path). What differs by call site is which permission
// handler answers the prompt: local-mode runs get this file's
// BrowserPermissionHandler; gVisor-sandboxed delegated runs get
// AutoApprovePermissionHandler instead — see run.go/resume.go for the
// agentproc.WillSandbox() branch.

package delegate

import (
	"context"
	"errors"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// The two terminal deny messages a surfaced prompt can resolve to without a
// user answer. They're kept distinct so the absent path (nobody could answer)
// is separable from the present-but-unanswered path (someone was watching but
// never clicked) in run transcripts and downstream tooling (TFAC-392).
const (
	msgPermTimedOut = "permission request timed out"
	msgNoOperator   = "no operator available"
)

// defaultPresencePollInterval is how often the presence-gated wait re-checks
// whether an answer-capable, focused tab is present in the run's org. 1s is
// fine-grained enough for a 15s-default grace while staying cheap (an RLock +
// map scan over the hub's clients). Tests inject a shorter value via
// SetPresencePollInterval to drive the absent/present paths deterministically.
const defaultPresencePollInterval = time.Second

// ErrNoPendingPermission is returned by ResolvePermission when no in-flight
// request matches (orgID, runID, requestID) — it was already answered, timed
// out, or never existed. The permission endpoint maps it to 404.
var ErrNoPendingPermission = errors.New("no pending permission request")

// pendingPermission is one in-flight browser permission prompt: the 1-buffered
// channel the handler goroutine is parked on, plus the owning org so a resolve
// from another tenant can't satisfy it. The owning run is encoded in the broker
// key (see permKey), not stored here.
type pendingPermission struct {
	ch    chan agentproc.PermissionDecision
	orgID string
}

// permKey is the broker key for a pending prompt. The wrapper's request_id is
// only unique within a single run process (`perm-N` from a per-process
// counter), so two concurrent runs can collide on the same id — keying by
// (runID, requestID) keeps each run's prompts isolated. The NUL separator can't
// appear in a run uuid or a `perm-N` id, so the composite is unambiguous.
func permKey(runID, requestID string) string {
	return runID + "\x00" + requestID
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
	ts := domain.DefaultTeamSettings()
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
// request id, broadcasts a `permission_request`, waits, and always deregisters.
//
// absent is the presence-gated fast-deny policy (TFAC-392). With it disabled the
// wait is exactly the legacy full-permTimeout() select; with it enabled an
// unattended prompt denies after the grace window with reason "no operator
// available", while a present, focused board/run tab extends the wait to the
// full window. Either way the total wait never exceeds permTimeout() and a
// 200-acknowledged decision is never dropped.
func (s *Spawner) BrowserPermissionHandler(orgID, runID string, absent AbsentAutoDeny) agentproc.PermissionHandler {
	return func(req agentproc.PermissionRequest) agentproc.PermissionDecision {
		key := permKey(runID, req.RequestID)
		ch := make(chan agentproc.PermissionDecision, 1)
		s.mu.Lock()
		s.permPending[key] = &pendingPermission{ch: ch, orgID: orgID}
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			delete(s.permPending, key)
			s.mu.Unlock()
		}()

		// Hub.Broadcast is nil-receiver-safe, so no guard is needed for the
		// hub-less test spawner. timeout_ms is the prompt's server-side
		// deadline, sent relative so the client derives its dismiss TTL from
		// the payload instead of mirroring this Go constant (and so clock
		// skew between server and browser doesn't matter). It stays the FULL
		// window even when absent-deny may fire sooner: the card's client TTL
		// is a backstop, and an absent-deny eagerly broadcasts
		// permission_resolved to drop the card the moment it fires.
		full := s.permTimeout()
		s.wsHub.Broadcast(websocket.Event{
			Type:  "permission_request",
			OrgID: orgID,
			RunID: runID,
			Data: map[string]any{
				"request_id": req.RequestID,
				"tool_name":  req.ToolName,
				"input":      req.Input,
				"timeout_ms": full.Milliseconds(),
			},
		})

		decision, got, reason := s.awaitPermission(ch, orgID, runID, full, absent)
		if got {
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
			return d
		default:
			// The prompt is no longer answerable, so tell every surface showing
			// it to drop it now instead of waiting out its own client TTL
			// (ResolvePermission already broadcasts for the user-answered case).
			s.broadcastPermissionResolved(orgID, runID, req.RequestID)
			return agentproc.PermissionDecision{Behavior: "deny", Message: reason}
		}
	}
}

// awaitPermission blocks until the prompt is answered or a deadline fires. It
// returns (decision, true, "") when a decision arrives on ch — the caller
// returns it verbatim — and (zero, false, reason) when a deadline elapses, where
// reason is the deny message the caller uses after its deregister-then-drain.
//
// With absent.enabled false this is the exact legacy select: ch vs the full
// window. With it enabled the wait polls presence on a short ticker and tracks
// two deadlines: the full window (always), and absentSince+grace (only while no
// answer-capable, focused tab is present in the run's org). The effective
// deadline is whichever is sooner; presence flipping to present re-arms the wait
// to the full window (clears absentSince), and flipping back resets the grace
// clock. The grace deadline never exceeds the full window (clampGrace), so the
// total wait is bounded by permTimeout() in every branch.
func (s *Spawner) awaitPermission(ch chan agentproc.PermissionDecision, orgID, runID string, full time.Duration, absent AbsentAutoDeny) (agentproc.PermissionDecision, bool, string) {
	if !absent.enabled {
		select {
		case d := <-ch:
			return d, true, ""
		case <-time.After(full):
			return agentproc.PermissionDecision{}, false, msgPermTimedOut
		}
	}

	// Explicit timers fire the deadlines exactly (no up-to-one-tick overshoot):
	//   - fullTimer is the hard ceiling, armed for the whole window so the total
	//     wait never exceeds permTimeout() — the load-bearing "< idleTimeout()"
	//     invariant holds precisely, not within a poll interval.
	//   - graceTimer is the absent deadline, armed only while unattended. The
	//     ticker's sole job is to re-read presence and arm/disarm + reset this
	//     timer on the present↔absent edges (so a present→absent flip restarts
	//     the grace clock from that moment, matching "absentSince resets to now").
	fullTimer := time.NewTimer(full)
	defer fullTimer.Stop()
	ticker := time.NewTicker(s.presencePollInterval())
	defer ticker.Stop()

	present := s.presentFor(orgID, runID)
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
			return d, true, ""
		case <-fullTimer.C:
			// Watched-but-unanswered for the whole window (or grace never fired
			// because presence kept re-arming) — the legacy timeout.
			return agentproc.PermissionDecision{}, false, msgPermTimedOut
		case <-graceTimer.C:
			// Only armed while unattended, so reaching it means genuinely nobody
			// could answer within the grace.
			return agentproc.PermissionDecision{}, false, msgNoOperator
		case <-ticker.C:
			now := s.presentFor(orgID, runID)
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
func (s *Spawner) presentFor(orgID, runID string) bool {
	s.mu.Lock()
	pc := s.presence
	s.mu.Unlock()
	if pc != nil {
		return pc.PresentFor(context.Background(), orgID, runID)
	}
	return s.wsHub.PresentFor(orgID, runID)
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
func (s *Spawner) broadcastPermissionResolved(orgID, runID, requestID string) {
	s.wsHub.Broadcast(websocket.Event{
		Type:  "permission_resolved",
		OrgID: orgID,
		RunID: runID,
		Data: map[string]any{
			"request_id": requestID,
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
func (s *Spawner) AutoApprovePermissionHandler(runID string) agentproc.PermissionHandler {
	return func(req agentproc.PermissionRequest) agentproc.PermissionDecision {
		delegateLog.Info("off-allowlist tool auto-approved in sandboxed run", "run", runID, "request_id", req.RequestID, "tool", req.ToolName)
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
func (s *Spawner) ResolvePermission(orgID, runID, requestID string, d agentproc.PermissionDecision) error {
	s.mu.Lock()
	p, ok := s.permPending[permKey(runID, requestID)]
	if !ok || p.orgID != orgID {
		s.mu.Unlock()
		return ErrNoPendingPermission
	}
	select {
	case p.ch <- d:
		s.mu.Unlock()
		// The decision is buffered (a real 200 promise). Tell other surfaces
		// showing this prompt to drop it now — broadcast outside s.mu so the
		// hub's own locking never nests under the broker mutex.
		s.broadcastPermissionResolved(orgID, runID, requestID)
		return nil
	default:
		// Slot already filled by a racing resolve — treat as no-longer-pending.
		s.mu.Unlock()
		return ErrNoPendingPermission
	}
}
