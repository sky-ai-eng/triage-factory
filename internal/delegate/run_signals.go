// Cross-pod run control (TFAC-585): RunController's intended second
// implementation. When the local process registry misses a control
// request (interrupt/steer/cancel/permission), crossPodController resolves
// the run's executor via runs.executor_id + instance-registry liveness,
// inserts a row on the conversation_signals outbox, NOTIFYs tf_ctl, and waits (with
// a bounded timeout) for the owning executor's apply loop to apply it
// through its own local RunController and ack. See
// docs/for-agents/specs/horizontal-scaling/README.md §5.2.
//
// This file also carries StageOrDeliverAdditiveEvent, the cross-pod-aware
// successor TFAC-594's additive-injection gate calls: the `inject` signal
// kind exists because that gate's live path is a process-local getProc
// hit the router (a brain component) can't reach for a run executing on a
// remote executor.
//
// Every mechanism here is dormant unless SetRunSignals wires it — always
// nil in local mode (conversation_signals is Postgres-only) and in every test that
// doesn't opt in, which is what keeps s.controller as the plain
// inProcessController and every cross-pod branch below unreached.

package delegate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/pgnotify"
)

// tfCtlChannel is the shared, kind-discriminated control-plane doorbell
// channel: control pods NOTIFY {"kind":"new",...} to wake an owner's
// apply loop, and owners NOTIFY {"kind":"ack",...} to wake a control
// pod's ack wait — see docs/for-agents/specs/horizontal-scaling/README.md §5. This
// package only PUBLISHES here (notifyCtl); consumption is internal/app's
// single per-pod tf_ctl listener (internal/app/ctl.go), which routes
// new/ack payloads to HandleCtlNotification below alongside the channel's
// other kinds (wsbackplane's "kick", ctlbus's "trigger"/"pollsoon") — one
// LISTEN connection serves every tf_ctl consumer on the pod, per spec
// §5's connection budget. New kinds must stay disjoint across all three
// publishers.
const tfCtlChannel = "tf_ctl"

// instanceStaleThreshold bounds how old an instance registry heartbeat may
// be before resolveLiveOwner treats that executor as gone rather than a
// viable signal target. Deliberately generous relative to the ~4s
// heartbeat cadence (RunInstanceHeartbeat) — this is a routing decision
// ("is it worth trying"), not the fleet reaper's dead-executor
// determination (TFAC-586 owns that stricter fencing deadline); a false
// "still live" here just costs a timed-out signal wait, never corrupts
// state.
const instanceStaleThreshold = 15 * time.Second

// defaultAckPollInterval is the waiting control pod's backstop poll
// alongside its tf_ctl ack-NOTIFY wait — "NOTIFY is a doorbell, never the
// only path" (spec §5).
const defaultAckPollInterval = 250 * time.Millisecond

// DefaultSignalAckTimeout is the interrupt/steer reply-leg timeout — how
// long the control pod waits for the owning executor to ack before giving
// up (504). Env-tunable via TF_SIGNAL_ACK_TIMEOUT (ParseSignalAckTimeout).
const DefaultSignalAckTimeout = 5 * time.Second

// DefaultPermissionAckTimeout is the permission reply-leg timeout — longer
// than the other kinds because the browser is synchronously blocked on
// this response (spec's reply-leg contract table). Not env-tunable
// separately; TF_SIGNAL_ACK_TIMEOUT governs interrupt/steer only.
const DefaultPermissionAckTimeout = 10 * time.Second

// ParseSignalAckTimeout interprets the TF_SIGNAL_ACK_TIMEOUT env value.
// Empty → the default. Non-positive or unparseable → the default plus an
// error the caller logs (a bad value must not brick boot).
func ParseSignalAckTimeout(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultSignalAckTimeout, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return DefaultSignalAckTimeout, fmt.Errorf(
			"invalid TF_SIGNAL_ACK_TIMEOUT %q (want a positive duration like \"5s\"); using default %s",
			raw, DefaultSignalAckTimeout)
	}
	return d, nil
}

// SetSignalAckTimeout overrides the interrupt/steer reply-leg timeout.
// Tests inject a short value to drive the timeout path deterministically.
func (s *Spawner) SetSignalAckTimeout(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.signalAckTimeout = d
}

// signalTimeout returns the effective interrupt/steer ack timeout, falling
// back to the default when unset.
func (s *Spawner) signalTimeout() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.signalAckTimeout > 0 {
		return s.signalAckTimeout
	}
	return DefaultSignalAckTimeout
}

// SetRunSignals wires the cross-pod run-control outbox (multi mode only —
// callers must gate this on runmode.Current() == ModeMulti before calling;
// there is no internal mode check here, matching the other post-
// construction seams like SetJiraResolver). Installing a non-nil
// runSignals swaps s.controller for crossPodController, the RunController
// seam's second implementation; leaving it unset (the default) keeps the
// plain inProcessController, which is what makes local mode's "no
// conversation_signals writes" invariant structural rather than a runtime check.
// notifyDB is the admin pool NOTIFY rides — see pgnotify.Notify's doc
// comment on why it needs no session affinity, unlike the dedicated LISTEN
// connection (internal/pgnotify.Listener, wired separately against
// s.HandleCtlNotification).
func (s *Spawner) SetRunSignals(runSignals db.RunSignalStore, notifyDB *sql.DB) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runSignals = runSignals
	s.signalNotifyDB = notifyDB
	s.controller = crossPodController{inProcessController: inProcessController{s: s}}
}

// getController reads s.controller under lock — SetRunSignals is the only
// mutator, called once at startup, but a bare unguarded read would race it
// on the theoretical timing where a request lands mid-startup.
func (s *Spawner) getController() RunController {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.controller
}

// crossPodController wraps inProcessController with the outbox-backed
// cross-pod path: try the local process registry first (byte-identical to
// today's N=1 behavior, including its exact error), and only on a local
// miss resolve a remote owner. Cancel is NOT extended here — cancel's
// cross-pod hastening is fire-and-forget from cancel.go's Spawner.Cancel
// (the DB-only park write is already the source of
// truth), so crossPodController.Cancel is a pure passthrough to the local
// lookup.
type crossPodController struct {
	inProcessController
}

func (c crossPodController) Interrupt(ctx context.Context, runID string) error {
	err := c.inProcessController.Interrupt(ctx, runID)
	if !errors.Is(err, ErrNoLiveProcess) {
		return err
	}
	return c.s.routeControlSignal(ctx, runID, domain.RunSignalInterrupt, "")
}

func (c crossPodController) Steer(ctx context.Context, runID, text string) error {
	err := c.inProcessController.Steer(ctx, runID, text)
	if !errors.Is(err, ErrNoLiveProcess) {
		return err
	}
	return c.s.routeControlSignal(ctx, runID, domain.RunSignalSteer, text)
}

// steerPayload is the conversation_signals.payload shape for kind=steer.
type steerPayload struct {
	Text string `json:"text"`
}

// permissionPayload is the conversation_signals.payload shape for kind=permission.
type permissionPayload struct {
	ToolCallID   string         `json:"tool_call_id"`
	Behavior     string         `json:"behavior"`
	Message      string         `json:"message"`
	UpdatedInput map[string]any `json:"updated_input"`
	// DecidedBy carries the answering user across the hop so the owner — the
	// only process that writes the prompt's audit row — can attribute the
	// decision to the human who actually made it rather than to "somebody on
	// another pod".
	DecidedBy string `json:"decided_by,omitempty"`
}

// injectPayload is the conversation_signals.payload shape for kind=inject. The
// firing-ref fields ride along so the owner can compensate with a
// pending_firing enqueue if the run turns out dead by apply time (the
// "gone" ack path) — the intent must never be dropped.
type injectPayload struct {
	Producer          string `json:"producer"`
	Body              string `json:"body"`
	EntityID          string `json:"entity_id"`
	TaskID            string `json:"task_id"`
	TriggerID         string `json:"trigger_id"`
	TriggeringEventID string `json:"triggering_event_id"`
}

// ctlNotification is the tf_ctl NOTIFY payload envelope — a pure doorbell
// (never the data): "new" wakes the target's apply loop to rescan;
// "ack" wakes a waiting control pod to re-check AckStatus.
type ctlNotification struct {
	Kind string `json:"kind"` // "new" | "ack"
	ID   int64  `json:"id"`
}

// routeControlSignal is the shared cross-pod path for interrupt/steer: no
// live process here, so resolve a live remote owner, insert the signal,
// and wait for its ack up to the reply-leg timeout. Returns
// ErrNoLiveProcess (409) when no live owner exists anywhere or the owner
// acks anything other than ok — gone (lost the race) or stale (e.g. a
// malformed payload, which should never happen same-version but must not
// be reported as success across a rolling deploy) both count as "did not
// apply" — ErrSignalAckTimeout (504) on a timed-out wait.
func (s *Spawner) routeControlSignal(ctx context.Context, runID string, kind domain.RunSignalKind, text string) error {
	orgID, err := s.agentRuns.LookupOrgForRunSystem(ctx, runID)
	if err != nil || orgID == "" {
		return fmt.Errorf("%s run %s: %w", kind, runID, ErrNoLiveProcess)
	}
	var payload string
	if kind == domain.RunSignalSteer {
		b, merr := json.Marshal(steerPayload{Text: text})
		if merr != nil {
			return fmt.Errorf("%s run %s: marshal payload: %w", kind, runID, merr)
		}
		payload = string(b)
	}
	waitCtx, cancel := context.WithTimeout(ctx, s.signalTimeout())
	defer cancel()
	result, live, err := s.sendSignalAndAwaitAck(waitCtx, orgID, runID, kind, payload)
	if err != nil {
		return fmt.Errorf("%s run %s: %w", kind, runID, err)
	}
	if !live || result != domain.RunSignalAckOK {
		return fmt.Errorf("%s run %s: %w", kind, runID, ErrNoLiveProcess)
	}
	return nil
}

// signalCancelBestEffort hastens a live remote kill for a run this pod
// doesn't own locally — fire-and-forget, per the reply-leg contract's
// cancel row: the caller's DB-only park write is already
// the source of truth and already works cross-pod, so a failure or
// timeout here is never surfaced. No-op when runSignals isn't wired
// (local mode, or multi mode before/without SetRunSignals).
func (s *Spawner) signalCancelBestEffort(orgID, runID, executorID string) {
	s.mu.Lock()
	runSignals := s.runSignals
	s.mu.Unlock()
	if runSignals == nil || s.instances == nil || executorID == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		inst, err := s.instances.Get(ctx, executorID)
		if err != nil || inst == nil || time.Since(inst.LastHeartbeatAt) > instanceStaleThreshold {
			return
		}
		id, err := runSignals.Insert(ctx, orgID, runID, domain.RunSignalCancel, "", executorID)
		if err != nil {
			delegateLog.Warn("cancel hastening signal insert failed (DB-only cancel already recorded)", "run", runID, "error", err)
			return
		}
		if err := s.notifyCtl(ctx, "new", id); err != nil {
			delegateLog.Warn("notify tf_ctl for cancel hastening signal failed; the owner's backstop scan still finds it", "signal_id", id, "error", err)
		}
	}()
}

// resolveLiveOwner reports the executor id owning runID's live process,
// and whether that executor's instance-registry heartbeat is fresh enough
// to be worth signaling. false covers every reason a remote signal can't
// help: no runSignals/instances wired, the run has no executor_id, or the
// stamped executor's heartbeat is stale.
func (s *Spawner) resolveLiveOwner(ctx context.Context, orgID, runID string) (executorID string, ok bool) {
	if s.instances == nil {
		return "", false
	}
	run, err := s.agentRuns.GetSystem(ctx, orgID, runID)
	if err != nil || run == nil || run.ExecutorID == "" {
		return "", false
	}
	inst, err := s.instances.Get(ctx, run.ExecutorID)
	if err != nil || inst == nil {
		return "", false
	}
	if time.Since(inst.LastHeartbeatAt) > instanceStaleThreshold {
		return "", false
	}
	return run.ExecutorID, true
}

// sendSignalAndAwaitAck resolves a live remote owner for runID, inserts a
// conversation_signals row, NOTIFYs tf_ctl, and waits for an ack until ctx is done.
// live=false means no live remote owner was found at all (the run has no
// stamped executor, or its heartbeat is stale) — the caller falls back to
// its own no-live-process handling; live=true with a non-nil err means the
// owner never acked before ctx's deadline (ErrSignalAckTimeout) or the
// insert itself failed.
func (s *Spawner) sendSignalAndAwaitAck(ctx context.Context, orgID, runID string, kind domain.RunSignalKind, payload string) (result string, live bool, err error) {
	executorID, ok := s.resolveLiveOwner(ctx, orgID, runID)
	if !ok {
		return "", false, nil
	}
	id, err := s.runSignals.Insert(ctx, orgID, runID, kind, payload, executorID)
	if err != nil {
		return "", true, fmt.Errorf("insert signal: %w", err)
	}
	if err := s.notifyCtl(ctx, "new", id); err != nil {
		delegateLog.Warn("notify tf_ctl for new signal failed; the owner's backstop scan still finds it", "signal_id", id, "error", err)
	}
	result, err = s.awaitAck(ctx, id)
	return result, true, err
}

// ErrSignalAckTimeout is returned when a cross-pod control request's
// owning executor never acked before the reply-leg deadline. Callers map
// it to 504 — "run owner did not acknowledge; the run may be mid-teardown".
var ErrSignalAckTimeout = errors.New("run owner did not acknowledge the control signal")

// awaitAck blocks until signal id is acked or ctx is done, racing a
// tf_ctl ack-NOTIFY wake against a 250ms backstop poll of AckStatus —
// whichever observes the ack first wins (NOTIFY is a doorbell, never the
// data: every wake re-reads AckStatus rather than trusting the
// notification's own payload).
func (s *Spawner) awaitAck(ctx context.Context, id int64) (string, error) {
	wake := make(chan struct{}, 1)
	s.mu.Lock()
	s.ackWaiters[id] = wake
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.ackWaiters, id)
		s.mu.Unlock()
	}()

	check := func() (string, bool) {
		acked, result, err := s.runSignals.AckStatus(ctx, id)
		if err != nil {
			delegateLog.Warn("poll signal ack status failed", "signal_id", id, "error", err)
			return "", false
		}
		return result, acked
	}
	// The ack may have already landed between Insert and here (a fast
	// owner beating a slow doorbell/poll setup) — check once up front.
	if result, ok := check(); ok {
		return result, nil
	}
	poll := time.NewTicker(defaultAckPollInterval)
	defer poll.Stop()
	for {
		select {
		case <-wake:
			if result, ok := check(); ok {
				return result, nil
			}
		case <-poll.C:
			if result, ok := check(); ok {
				return result, nil
			}
		case <-ctx.Done():
			return "", ErrSignalAckTimeout
		}
	}
}

// notifyCtl publishes a tf_ctl doorbell. No-op (never an error worth
// failing the caller over) when signalNotifyDB isn't wired.
func (s *Spawner) notifyCtl(ctx context.Context, kind string, id int64) error {
	s.mu.Lock()
	notifyDB := s.signalNotifyDB
	s.mu.Unlock()
	if notifyDB == nil {
		return nil
	}
	payload, err := json.Marshal(ctlNotification{Kind: kind, ID: id})
	if err != nil {
		return err
	}
	return pgnotify.Notify(ctx, notifyDB, tfCtlChannel, string(payload))
}

// HandleCtlNotification handles this package's tf_ctl kinds, invoked by
// internal/app's unified tf_ctl dispatcher (ctl.go) for kind "new"/"ack"
// payloads. It dispatches a "new" doorbell to the signal-apply loop's
// wake channel and an "ack" doorbell to the matching in-flight awaitAck's
// wake channel, if any is registered here. Malformed payloads are logged
// and dropped — the backstop polls on both sides mean a dropped doorbell
// only delays, never loses, the underlying work.
func (s *Spawner) HandleCtlNotification(payload string) {
	var env ctlNotification
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		delegateLog.Warn("tf_ctl notification: malformed payload", "payload", payload, "error", err)
		return
	}
	switch env.Kind {
	case "new":
		select {
		case s.signalApplyWake <- struct{}{}:
		default:
		}
	case "ack":
		s.mu.Lock()
		wake, ok := s.ackWaiters[env.ID]
		s.mu.Unlock()
		if ok {
			select {
			case wake <- struct{}{}:
			default:
			}
		}
	}
}

// InjectOutcome states how StageOrDeliverAdditiveEvent handled an additive
// event for a run — TFAC-594's additive-injection gate branches on it.
type InjectOutcome int

const (
	// InjectNotDelivered means the caller must fall through to its own
	// deferral (enqueueBusyFiring) — nothing durable exists to recover the
	// intent from.
	InjectNotDelivered InjectOutcome = iota
	// InjectDeliveredLocal means the event was steered into this
	// process's own live handle for the run — the caller should record
	// the task_event 'injected' itself, exactly as before TFAC-585.
	InjectDeliveredLocal
	// InjectDeliveredRemote means a conversation_signals row was handed to a live
	// remote executor — the OWNER records 'injected' (or compensates with
	// a pending_firing on "gone") once delivery is confirmed, so the
	// caller must NOT record it here.
	InjectDeliveredRemote
	// InjectStagedResumable means the event was durably staged AND the
	// run is currently resumable — the caller should record the
	// task_event 'injected' itself (the staged row will flush on the
	// run's next resume), exactly as before TFAC-585.
	InjectStagedResumable
)

// AdditiveFiringRef carries the (entity, task, trigger, triggering event)
// identity an inject signal's payload needs so the OWNER can compensate
// with a pending_firing enqueue if the run turns out dead by apply time
// (the ticket's "gone ack enqueues the pending_firing itself" decision).
type AdditiveFiringRef struct {
	EntityID          string
	TaskID            string
	TriggerID         string
	TriggeringEventID string
}

// StageOrDeliverAdditiveEvent is StageOrDeliverInjectionResult's cross-pod-
// aware successor for TFAC-594's additive-injection gate specifically
// (every other staged-injection producer — e.g. the new-commits notifier —
// keeps calling StageOrDeliverInjection/-Result unchanged; this method
// exists only because the routed additive gate needs the "handed to a
// live remote executor" outcome its callers must NOT re-record locally).
//
// Decision order: local live process (byte-identical to the pre-TFAC-585
// path) → a live remote owner (insert an `inject` conversation_signals row,
// fire-and-forget — never waited on, per the reply-leg contract) → the
// existing staged/resumable fallback.
func (s *Spawner) StageOrDeliverAdditiveEvent(ctx context.Context, orgID, runID, producer, body string, firing AdditiveFiringRef) InjectOutcome {
	if body == "" {
		return InjectNotDelivered
	}
	if s.getProc(runID) != nil {
		s.deliverInjectionLive(orgID, runID, domain.WrapSystemNote(body))
		return InjectDeliveredLocal
	}
	s.mu.Lock()
	runSignals := s.runSignals
	s.mu.Unlock()
	if runSignals != nil {
		if executorID, ok := s.resolveLiveOwner(ctx, orgID, runID); ok {
			payload, merr := json.Marshal(injectPayload{
				Producer: producer, Body: body,
				EntityID: firing.EntityID, TaskID: firing.TaskID,
				TriggerID: firing.TriggerID, TriggeringEventID: firing.TriggeringEventID,
			})
			if merr != nil {
				delegateLog.Warn("marshal inject payload failed; falling back to the staged queue", "run", runID, "error", merr)
			} else if id, ierr := runSignals.Insert(ctx, orgID, runID, domain.RunSignalInject, string(payload), executorID); ierr != nil {
				delegateLog.Warn("insert inject signal failed; falling back to the staged queue", "run", runID, "error", ierr)
			} else {
				if nerr := s.notifyCtl(ctx, "new", id); nerr != nil {
					delegateLog.Warn("notify tf_ctl for inject signal failed; the owner's backstop scan still finds it", "signal_id", id, "error", nerr)
				}
				return InjectDeliveredRemote
			}
		}
	}
	delivered, staged, stagedID := s.stageOrDeliverInjection(orgID, runID, producer, body)
	if delivered {
		return InjectDeliveredLocal // race: getProc missed above, live by the time stageOrDeliverInjection ran
	}
	if !staged {
		return InjectNotDelivered
	}
	run, err := s.agentRuns.GetSystem(ctx, orgID, runID)
	if err != nil || run == nil || !resumableState(run.Status, run.Outcome) {
		// The row staged above is now orphaned: the caller falls through to
		// the normal deferral (enqueueBusyFiring), and a staged row nothing
		// will ever flush is a permanent leak — worse, a double-delivery
		// hazard if this "non-resumable" run is later boot-recovered and
		// resumed (the staged row would flush into it while the pending
		// firing also spawns a fresh run). Best-effort delete: a leaked row
		// is the pre-existing status quo, never worth failing the deferral
		// over.
		if stagedID != "" && s.stagedInjections != nil {
			if derr := s.stagedInjections.DeleteSystem(ctx, orgID, stagedID); derr != nil {
				delegateLog.Warn("delete orphaned staged injection failed", "run", runID, "staged_id", stagedID, "error", derr)
			}
		}
		return InjectNotDelivered
	}
	return InjectStagedResumable
}

// --- Owner-side signal-apply loop ---

// DefaultSignalApplyScanInterval is the apply loop's backstop scan cadence
// — the slow poll that guarantees a dropped tf_ctl doorbell only delays
// delivery, never loses it.
const DefaultSignalApplyScanInterval = 5 * time.Second

// RunSignalApplyLoop is the owner's half of the cross-pod control seam: it
// scans conversation_signals for unacked rows targeting this executor and applies
// each through the local (never cross-pod — see applySignal) RunController,
// acking with a result. One goroutine, deliberately serial (§5.2: "do not
// parallelize" — signal volume is human-action-scale). No-op when
// runSignals isn't wired (local mode, or a role that never called
// SetRunSignals).
func (s *Spawner) RunSignalApplyLoop(ctx context.Context, scanInterval time.Duration) {
	if s.runSignals == nil {
		return
	}
	s.drainSignals(ctx)
	scan := time.NewTicker(scanInterval)
	defer scan.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.signalApplyWake:
			s.drainSignals(ctx)
		case <-scan.C:
			s.drainSignals(ctx)
		}
	}
}

// drainSignals scans + applies every unacked signal targeting this
// executor, oldest id first (which, within one run, is the required apply
// order — see the store's doc comment).
func (s *Spawner) drainSignals(ctx context.Context) {
	if s.IdentityFenced() || s.PartitionFenced() {
		// A fenced instance must not apply signals — the same gate the claim
		// loop uses (dispatch.go). Identity-fenced: this process's identity is
		// superseded, so acting on a run races the replacement. Partition-
		// fenced: it already killed its live sandboxes and its rows may have
		// been requeued, so a late apply would double-write against the new
		// owner. The next successful heartbeat clears the partition fence and
		// the scan resumes.
		return
	}
	executorID, _ := s.executorIdentity()
	if executorID == "" {
		return
	}
	sigs, err := s.runSignals.ListUnackedForTarget(ctx, executorID)
	if err != nil {
		delegateLog.Warn("scan unacked conversation_signals failed; retrying on the next tick", "error", err)
		return
	}
	for _, sig := range sigs {
		s.applySignal(ctx, sig)
	}
}

// applySignal delivers one signal, then acks the result — with exactly-once
// delivery per process. If a signal was already delivered but its ack write
// didn't land (leaving it in the unacked scan), the next drain re-acks with
// the same result instead of re-delivering: re-delivery is harmless for
// cancel/interrupt/permission (idempotent), but a re-sent steer or a
// re-injected note would reach the agent twice, and inject's gone-
// compensation would double-enqueue a pending_firing. signalApplied is
// pruned on a successful ack (the row then leaves the unacked scan anyway).
func (s *Spawner) applySignal(ctx context.Context, sig domain.RunSignal) {
	if r, ok := s.signalApplied.Load(sig.ID); ok {
		if s.ackSignal(ctx, sig.ID, r.(string)) {
			s.signalApplied.Delete(sig.ID)
		}
		return
	}
	result := s.deliverSignal(ctx, sig)
	s.signalApplied.Store(sig.ID, result)
	if s.ackSignal(ctx, sig.ID, result) {
		s.signalApplied.Delete(sig.ID)
	}
}

// deliverSignal performs one signal's side effects through the LOCAL
// RunController (inProcessController — never s.controller/crossPodController:
// this process IS the intended owner by construction of the scan's target
// filter, so a local miss means the run is genuinely gone here, not "try
// another pod") and returns the ack result. It must be called at most once
// per signal per process (applySignal's signalApplied guard enforces this),
// because steer/inject delivery and inject's gone-compensation are not
// idempotent.
func (s *Spawner) deliverSignal(ctx context.Context, sig domain.RunSignal) string {
	local := inProcessController{s: s}
	switch sig.Kind {
	case domain.RunSignalCancel:
		if local.Cancel(sig.RunID) {
			return domain.RunSignalAckOK
		}
		return domain.RunSignalAckGone
	case domain.RunSignalInterrupt:
		return ackResultForControlErr(local.Interrupt(ctx, sig.RunID))
	case domain.RunSignalSteer:
		var p steerPayload
		if err := json.Unmarshal([]byte(sig.Payload), &p); err != nil {
			delegateLog.Warn("apply steer signal: malformed payload", "signal_id", sig.ID, "error", err)
			return domain.RunSignalAckStale
		}
		return ackResultForControlErr(local.Steer(ctx, sig.RunID, p.Text))
	case domain.RunSignalPermission:
		return s.deliverPermissionSignal(sig)
	case domain.RunSignalInject:
		return s.deliverInjectSignal(ctx, sig)
	default:
		delegateLog.Warn("apply signal: unknown kind", "signal_id", sig.ID, "kind", sig.Kind)
		return domain.RunSignalAckStale
	}
}

// ackResultForControlErr maps an Interrupt/Steer error to an ack_result:
// nil -> ok, "no live process here" -> gone, anything else -> gone too
// (the ack vocabulary has no separate "apply error" value; the waiting
// control pod treats gone identically to ErrNoLiveProcess/409 either way,
// and the real error is logged at the apply site).
func ackResultForControlErr(err error) string {
	if err == nil {
		return domain.RunSignalAckOK
	}
	if !errors.Is(err, ErrNoLiveProcess) {
		delegateLog.Warn("apply control signal: unexpected error", "error", err)
	}
	return domain.RunSignalAckGone
}

// deliverPermissionSignal resolves a cross-pod permission decision through
// this process's own permission broker and returns the ack result. A
// stale/no-longer-pending prompt (already answered, timed out, or never
// existed here) returns 'stale' — the idempotency contract for a duplicate
// permission answer.
func (s *Spawner) deliverPermissionSignal(sig domain.RunSignal) string {
	var p permissionPayload
	if err := json.Unmarshal([]byte(sig.Payload), &p); err != nil {
		delegateLog.Warn("apply permission signal: malformed payload", "signal_id", sig.ID, "error", err)
		return domain.RunSignalAckStale
	}
	err := s.resolvePermissionLocal(sig.OrgID, sig.RunID, p.ToolCallID, p.DecidedBy, agentproc.PermissionDecision{
		Behavior:     p.Behavior,
		Message:      p.Message,
		UpdatedInput: p.UpdatedInput,
	})
	switch {
	case err == nil:
		return domain.RunSignalAckOK
	case errors.Is(err, ErrNoPendingPermission):
		return domain.RunSignalAckStale
	default:
		delegateLog.Warn("apply permission signal failed", "signal_id", sig.ID, "error", err)
		return domain.RunSignalAckStale
	}
}

// deliverInjectSignal delivers a cross-pod additive-event injection into
// this process's live handle for the run, recording the task_event
// 'injected' exactly as the local path does, and returns the ack result. If
// the run is no longer live here by apply time, it returns 'gone' AND
// enqueues the pending_firing itself (from the payload's firing-ref fields)
// so the additive event's intent survives — the entity-busy-style loser rule
// the ticket specifies. Not idempotent (live delivery + the gone-path
// enqueue), so applySignal guards it against re-delivery.
func (s *Spawner) deliverInjectSignal(ctx context.Context, sig domain.RunSignal) string {
	var p injectPayload
	if err := json.Unmarshal([]byte(sig.Payload), &p); err != nil {
		delegateLog.Warn("apply inject signal: malformed payload", "signal_id", sig.ID, "error", err)
		return domain.RunSignalAckStale
	}
	if s.getProc(sig.RunID) == nil {
		if s.pendingFirings != nil && p.EntityID != "" && p.TaskID != "" && p.TriggerID != "" && p.TriggeringEventID != "" {
			if _, err := s.pendingFirings.Enqueue(ctx, sig.OrgID, "", p.EntityID, p.TaskID, p.TriggerID, p.TriggeringEventID); err != nil {
				delegateLog.Error("inject signal gone-compensation: enqueue pending_firing failed", "run", sig.RunID, "task_id", p.TaskID, "error", err)
			}
		}
		return domain.RunSignalAckGone
	}
	s.deliverInjectionLive(sig.OrgID, sig.RunID, domain.WrapSystemNote(p.Body))
	if s.tasks != nil && p.TaskID != "" && p.TriggeringEventID != "" {
		if err := s.tasks.MarkEventInjectedSystem(ctx, sig.OrgID, p.TaskID, p.TriggeringEventID); err != nil {
			delegateLog.Error("mark injected task_event for cross-pod inject failed", "task_id", p.TaskID, "run_id", sig.RunID, "error", err)
		}
	}
	return domain.RunSignalAckDelivered
}

// ackSignal writes the ack and best-effort NOTIFYs tf_ctl so a waiting
// control pod's ack-NOTIFY path wakes immediately instead of falling back
// to its 250ms poll. Reports whether the ack write itself landed — a false
// return leaves the signal in the unacked scan, so applySignal keeps its
// delivered-marker to re-ack (never re-deliver) on the next drain.
func (s *Spawner) ackSignal(ctx context.Context, id int64, result string) bool {
	if err := s.runSignals.Ack(ctx, id, result); err != nil {
		delegateLog.Error("ack run_signal failed", "signal_id", id, "result", result, "error", err)
		return false
	}
	if err := s.notifyCtl(ctx, "ack", id); err != nil {
		delegateLog.Warn("notify tf_ctl for ack failed; the waiter's backstop poll still finds it", "signal_id", id, "error", err)
	}
	return true
}

// --- Acked-signal purge reaper ---

// DefaultRunSignalPurgeInterval is how often the reaper sweeps.
const DefaultRunSignalPurgeInterval = time.Hour

// DefaultRunSignalPurgeAge is the audit-convenience retention window for
// acked signal rows (decision log #5, env-tunable via
// TF_RUN_SIGNAL_PURGE_AFTER — ParseRunSignalPurgeAge).
const DefaultRunSignalPurgeAge = 24 * time.Hour

// ParseRunSignalPurgeAge interprets the TF_RUN_SIGNAL_PURGE_AFTER env
// value. Empty → the default. Non-positive or unparseable → the default
// plus an error the caller logs.
func ParseRunSignalPurgeAge(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultRunSignalPurgeAge, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return DefaultRunSignalPurgeAge, fmt.Errorf(
			"invalid TF_RUN_SIGNAL_PURGE_AFTER %q (want a positive duration like \"24h\"); using default %s",
			raw, DefaultRunSignalPurgeAge)
	}
	return d, nil
}

// RunSignalPurgeReaper periodically deletes acked conversation_signals rows older
// than age — the 24h audit-convenience window. No-op when runSignals
// isn't wired.
func (s *Spawner) RunSignalPurgeReaper(ctx context.Context, interval, age time.Duration) {
	if s.runSignals == nil {
		return
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			n, err := s.runSignals.PurgeAcked(ctx, age)
			if err != nil {
				delegateLog.Warn("purge acked conversation_signals failed", "error", err)
				continue
			}
			if n > 0 {
				delegateLog.Info("purged acked conversation_signals", "count", n)
			}
		}
	}
}
