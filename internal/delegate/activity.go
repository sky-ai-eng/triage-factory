package delegate

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The stall watchdog's timings. Each bound is the deadline of one operation
// an engagement can block on, and stallIdleLimit is how long an engagement
// may go with nothing in flight and nothing persisted. They are constants for
// the reason the claim-lease timings are: the ordering between them is a
// correctness property, and TestActivityTimings holds it.
const (
	// Nothing in flight and nothing persisted for this long is a stall.
	stallIdleLimit = 10 * time.Minute

	// A provider attempt with no byte for this long is over its bound.
	// Bifrost's own stream-idle timeout (120s) fires first and surfaces as a
	// transient error the retry loop handles; this is the backstop for a
	// stream that never produced a first byte, which nothing else bounds.
	providerByteDeadline = 150 * time.Second

	// One tool call. The native tool socket's deadline is set above this
	// (toolSocketDeadline), so a slow tool is a stall the watchdog reports,
	// not a socket error that fails the conversation.
	toolCallDeadline   = 30 * time.Minute
	toolSocketDeadline = toolCallDeadline + time.Minute

	// Clone, fetch, snapshot rehydrate: one workspace operation.
	workspaceOpDeadline = 10 * time.Minute

	// A permission prompt waiting on a person (SDK runtime only).
	permissionPromptDeadline = 150 * time.Second

	// A database write made with no caller context (the SDK sink, the
	// memory mirror check).
	detachedWriteDeadline = 30 * time.Second
)

// Operations that already carry a bound of their own — the bring-up waits,
// a permission prompt, a cold resume's wait on a snapshot — are tracked with a
// deadline past it, so their own bound fires first and surfaces as what it is
// (a failed bring-up, a denied prompt), and the watchdog is only ever the
// backstop.
const (
	// backstopMargin is how far past such a wait's own bound its operation
	// deadline sits.
	backstopMargin = 30 * time.Second

	// sidecarOpDeadline covers one round trip of the credential sidecar's
	// bring-up. The cap-broker bounds each call it serves at 60 seconds
	// (cmd/capbroker's callTimeout), and that is the bound this backs up.
	sidecarOpDeadline = 60*time.Second + backstopMargin

	// fetchPROpDeadline covers the pull-request fetch, which goes through the
	// GitHub client's 30-second timeout.
	fetchPROpDeadline = 60 * time.Second

	// stallIntentTimeout bounds the stall's intent write. It runs on a fresh
	// context because the engagement's own is about to be cancelled, and it
	// is bounded because the watchdog must go on to cancel whatever happens
	// to the write.
	stallIntentTimeout = 10 * time.Second
)

// stopParkReason is the reason a park of a cancelled engagement records: the
// stall's when the watchdog stopped it, a person's otherwise. A pending stop
// intent overrides it in the store, so this only decides the reason when the
// intent write failed or the cancel arrived with none.
func stopParkReason(ctx context.Context) domain.ParkReason {
	if errors.Is(context.Cause(ctx), errStalled) {
		return domain.ParkReasonStalled
	}
	return domain.ParkReasonUserCancelled
}

// stallCause is what the watchdog decided on: the operation that outlived its
// deadline and how long it had been in flight, or, with no operation, how
// long the engagement had been idle.
type stallCause struct {
	op      string
	elapsed time.Duration
}

// activityTracker is one engagement's record of what it is doing. It
// lives in the executor process for the engagement's lifetime; nothing
// about it is durable except what the claim renewal copies out.
//
// Every method is nil-safe, so a caller with no tracker registered (a test
// fixture, a path outside a claim) reports into nothing.
type activityTracker struct {
	mu           sync.Mutex
	idleLimit    time.Duration
	lastActivity time.Time // monotonic
	op           *inflightOp
	stalled      func(reason stallCause)
	timer        *time.Timer
	// decided is set once the watchdog has stopped the engagement, and
	// stopped once the engagement has ended: either way nothing re-arms.
	decided, stopped bool
}

type inflightOp struct {
	name     string    // "provider", "tool:<name>", "clone", "rehydrate", "permission", ...
	started  time.Time // monotonic
	deadline time.Time // monotonic; extended by progress for provider streams
}

// newActivityTracker starts an engagement's watchdog with nothing in flight,
// counting the start itself as activity.
func newActivityTracker(idleLimit time.Duration, stalled func(stallCause)) *activityTracker {
	a := &activityTracker{
		idleLimit:    idleLimit,
		lastActivity: time.Now(),
		stalled:      stalled,
	}
	a.timer = time.AfterFunc(idleLimit, a.check)
	return a
}

// begin records an operation in flight and returns its end. end records
// activity. Operations do not nest: begin while one is in flight replaces
// it, and the replaced one's end is a no-op, identity-checked.
func (a *activityTracker) begin(name string, bound time.Duration) (end func()) {
	if a == nil {
		return func() {}
	}
	now := time.Now()
	op := &inflightOp{name: name, started: now, deadline: now.Add(bound)}
	a.mu.Lock()
	a.op = op
	a.lastActivity = now
	a.rearmLocked()
	a.mu.Unlock()
	return func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.op != op {
			return
		}
		a.op = nil
		a.lastActivity = time.Now()
		a.rearmLocked()
	}
}

// progress extends the in-flight operation's deadline to now+bound and
// records activity. The provider stream calls it per chunk.
func (a *activityTracker) progress(bound time.Duration) {
	if a == nil {
		return
	}
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.op != nil {
		a.op.deadline = now.Add(bound)
	}
	a.lastActivity = now
	a.rearmLocked()
}

// touch records activity with nothing in flight: a message persisted, a
// turn sent. With an operation in flight it leaves that operation's deadline
// alone — a persisted row does not tell the watchdog the operation is still
// making progress.
func (a *activityTracker) touch() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastActivity = time.Now()
	a.rearmLocked()
}

// snapshot is what the claim renewal copies out.
func (a *activityTracker) snapshot() (idle time.Duration, op string) {
	if a == nil {
		return 0, ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.op != nil {
		op = a.op.name
	}
	return time.Since(a.lastActivity), op
}

// stop ends the watchdog with the engagement. A callback already dispatched
// reads stopped and stands down.
func (a *activityTracker) stop() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopped = true
	a.timer.Stop()
}

// rearmLocked points the one timer at whichever deadline governs now: the
// in-flight operation's, or the idle limit past the last activity.
func (a *activityTracker) rearmLocked() {
	if a.stopped || a.decided {
		return
	}
	a.timer.Reset(time.Until(a.dueLocked()))
}

func (a *activityTracker) dueLocked() time.Time {
	if a.op != nil {
		return a.op.deadline
	}
	return a.lastActivity.Add(a.idleLimit)
}

// check is the timer's callback. It decides on the state it reads under the
// lock, never on having fired: Reset cannot unschedule a callback the runtime
// has already dispatched, so a callback running just after an operation ended
// or a row landed has to find that out here and stand down.
func (a *activityTracker) check() {
	a.mu.Lock()
	if a.stopped || a.decided {
		a.mu.Unlock()
		return
	}
	now := time.Now()
	if now.Before(a.dueLocked()) {
		a.rearmLocked()
		a.mu.Unlock()
		return
	}
	var cause stallCause
	if a.op != nil {
		cause = stallCause{op: a.op.name, elapsed: now.Sub(a.op.started)}
	} else {
		cause = stallCause{elapsed: now.Sub(a.lastActivity)}
	}
	a.decided = true
	stalled := a.stalled
	a.mu.Unlock()
	stalled(cause)
}

// engineActivity is the tracker as the native loop sees it. The loop takes an
// interface rather than a dependency on this package; a nil tracker inside it
// reports into nothing, as every tracker method does.
type engineActivity struct{ a *activityTracker }

func (e engineActivity) Begin(name string, bound time.Duration) func() { return e.a.begin(name, bound) }
func (e engineActivity) Progress(bound time.Duration)                  { e.a.progress(bound) }
func (e engineActivity) Touch()                                        { e.a.touch() }

// activityTimings are the watchdog's bounds as one engagement uses them. The
// product runs on the constants above; a test sets a short value through
// setActivityTimings, and a zero field keeps that field's constant.
type activityTimings struct {
	idle         time.Duration
	providerByte time.Duration
	toolCall     time.Duration
	workspaceOp  time.Duration
	permission   time.Duration
}

// toolSocket is the native tool socket's per-call deadline, held above the
// tool call's own so the watchdog reports a slow tool before the socket
// fails it.
func (t activityTimings) toolSocket() time.Duration {
	return t.toolCall + (toolSocketDeadline - toolCallDeadline)
}

// setActivityTimings overrides the watchdog's bounds. Nothing in the running
// product calls it.
func (s *Spawner) setActivityTimings(t activityTimings) {
	s.mu.Lock()
	s.activityTimingsOverride = t
	s.mu.Unlock()
}

// resolvedActivityTimings is the bounds this spawner's engagements run
// under, the constants wherever no override is set.
func (s *Spawner) resolvedActivityTimings() activityTimings {
	s.mu.Lock()
	t := s.activityTimingsOverride
	s.mu.Unlock()
	orDefault := func(d, def time.Duration) time.Duration {
		if d <= 0 {
			return def
		}
		return d
	}
	return activityTimings{
		idle:         orDefault(t.idle, stallIdleLimit),
		providerByte: orDefault(t.providerByte, providerByteDeadline),
		toolCall:     orDefault(t.toolCall, toolCallDeadline),
		workspaceOp:  orDefault(t.workspaceOp, workspaceOpDeadline),
		permission:   orDefault(t.permission, permissionPromptDeadline),
	}
}

// startActivityTracker registers an engagement's tracker beside its trace
// root, so the transcript writers and the stream sink reach it by
// conversation id without it being threaded through every signature. The
// returned stop deregisters it and ends its watchdog.
//
// fence is the claim context's cancel, the same handle the lease loop holds.
func (s *Spawner) startActivityTracker(conv *domain.Conversation, fence context.CancelCauseFunc) (stop func()) {
	a := newActivityTracker(s.resolvedActivityTimings().idle, func(cause stallCause) {
		s.stallEngagement(conv, fence, cause)
	})
	s.mu.Lock()
	s.activity[conv.ID] = a
	s.mu.Unlock()
	return func() {
		a.stop()
		s.mu.Lock()
		if s.activity[conv.ID] == a {
			delete(s.activity, conv.ID)
		}
		s.mu.Unlock()
	}
}

// activityFor is the tracker of the engagement driving conversationID in
// this process, or nil — which every tracker method accepts.
func (s *Spawner) activityFor(conversationID string) *activityTracker {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activity[conversationID]
}

// stallEngagement stops an engagement the watchdog found stalled. The intent
// goes first, so an executor that dies mid-stop still leaves a record the
// settlement parks as stalled; the cancel runs whatever the write did, since
// the holder parks with stopParkReason on the cause alone.
func (s *Spawner) stallEngagement(conv *domain.Conversation, fence context.CancelCauseFunc, cause stallCause) {
	if s.conversations != nil {
		ctx, cancel := context.WithTimeout(context.Background(), stallIntentTimeout)
		if _, err := s.conversations.RequestStopSystem(ctx, conv.OrgID, conv.ID, "", "", domain.ParkReasonStalled); err != nil {
			dispatchLog.Warn("recording the stall's stop intent failed; stopping the engagement on its cause alone",
				"conversation", conv.ID, "claim", conv.ClaimID, "error", err)
		}
		cancel()
	}
	fence(errStalled)

	if cause.op != "" {
		dispatchLog.Warn("engagement stalled: an operation outlived its deadline; stopping it",
			"conversation", conv.ID, "claim", conv.ClaimID, "op", cause.op, "in_flight", cause.elapsed)
	} else {
		dispatchLog.Warn("engagement stalled: idle past its limit with nothing in flight; stopping it",
			"conversation", conv.ID, "claim", conv.ClaimID, "idle", cause.elapsed)
	}
	recordEngagementStall(cause.op)
}

// stallOpLabel is an operation name cut at its first colon, so every tool
// counts under "tool" and the label stays a closed set. The idle arm has no
// operation and counts as "idle".
func stallOpLabel(op string) string {
	if op == "" {
		return "idle"
	}
	if i := strings.IndexByte(op, ':'); i >= 0 {
		return op[:i]
	}
	return op
}

// engagementStalls owns this package's stall counter. Package-level because a
// metric instrument is process-global by nature; tests swap it for one backed
// by a manual reader, which is why it is swapped atomically: a watchdog
// callback can still be counting when a test restores the previous one.
var engagementStalls atomic.Pointer[engagementStallStats]

func init() {
	engagementStalls.Store(newEngagementStallStats(otel.GetMeterProvider()))
}

type engagementStallStats struct {
	stalled metric.Int64Counter
}

// newEngagementStallStats builds the instrument against mp — production passes
// the global provider, which telemetry.Init installs. An instrument-creation
// error can only be a programmer error (an invalid name), and the API hands
// back a usable no-op alongside it, so it is logged rather than propagated.
func newEngagementStallStats(mp metric.MeterProvider) *engagementStallStats {
	stalled, err := mp.Meter("internal/delegate").Int64Counter("engagements.stalled",
		metric.WithDescription("Engagements the stall watchdog stopped, by the operation in flight (idle when none was)."))
	if err != nil {
		dispatchLog.Warn("engagement stall counter setup failed", "error", err)
	}
	return &engagementStallStats{stalled: stalled}
}

func recordEngagementStall(op string) {
	stats := engagementStalls.Load()
	if stats == nil || stats.stalled == nil {
		return
	}
	stats.stalled.Add(context.Background(), 1, metric.WithAttributes(attribute.String("op", stallOpLabel(op))))
}
