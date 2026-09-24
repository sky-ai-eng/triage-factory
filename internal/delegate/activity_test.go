package delegate

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/inference"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// bifrostStreamIdleTimeout is bifrost's schemas.DefaultStreamIdleTimeoutInSeconds,
// by value: the per-chunk idle timeout bifrost closes a stream on, which has
// to fire before the provider bound so a stream that went quiet mid-response
// surfaces as the transient error the retry loop handles.
const bifrostStreamIdleTimeout = 120 * time.Second

// TestActivityTimings holds the order the watchdog's bounds have to keep.
// Nothing reads them from the environment, so this is where a change to one
// without the others is caught.
func TestActivityTimings(t *testing.T) {
	if providerByteDeadline <= bifrostStreamIdleTimeout {
		t.Errorf("provider bound %s is not above bifrost's stream-idle timeout %s, so the watchdog would stop a stream bifrost was about to retry",
			providerByteDeadline, bifrostStreamIdleTimeout)
	}
	if toolSocketDeadline <= toolCallDeadline {
		t.Errorf("tool socket deadline %s is not above the tool-call bound %s, so a slow tool would fail the conversation as a dead socket",
			toolSocketDeadline, toolCallDeadline)
	}
	if permissionPromptDeadline >= stallIdleLimit {
		t.Errorf("permission prompt deadline %s is not below the idle limit %s", permissionPromptDeadline, stallIdleLimit)
	}
	if permissionPromptDeadline+backstopMargin >= stallIdleLimit {
		t.Errorf("the permission operation's deadline %s is not below the idle limit %s", permissionPromptDeadline+backstopMargin, stallIdleLimit)
	}
	// The bring-up waits: each operation deadline sits past the wait's own
	// bound, so the bound fires first.
	if sidecarOpDeadline <= 60*time.Second {
		t.Errorf("sidecar operation deadline %s is not above the broker's 60s call timeout", sidecarOpDeadline)
	}
	if fetchPROpDeadline <= 30*time.Second {
		t.Errorf("fetch_pr operation deadline %s is not above the GitHub client's 30s timeout", fetchPROpDeadline)
	}
	// An injected tool bound keeps the socket above it.
	if got := (activityTimings{toolCall: time.Second}).toolSocket(); got <= time.Second {
		t.Errorf("toolSocket for an injected 1s tool bound = %s, want above it", got)
	}
}

// stallRecorder collects what a tracker's watchdog decided.
type stallRecorder struct {
	mu     sync.Mutex
	causes []stallCause
	fired  chan struct{}
}

func newStallRecorder() *stallRecorder {
	return &stallRecorder{fired: make(chan struct{}, 8)}
}

func (r *stallRecorder) stalled(c stallCause) {
	r.mu.Lock()
	r.causes = append(r.causes, c)
	r.mu.Unlock()
	r.fired <- struct{}{}
}

func (r *stallRecorder) seen() []stallCause {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]stallCause(nil), r.causes...)
}

func (r *stallRecorder) waitFired(t *testing.T, within time.Duration) stallCause {
	t.Helper()
	select {
	case <-r.fired:
	case <-time.After(within):
		t.Fatalf("the watchdog did not fire within %s", within)
	}
	got := r.seen()
	return got[len(got)-1]
}

func (r *stallRecorder) assertQuietFor(t *testing.T, d time.Duration, why string) {
	t.Helper()
	select {
	case <-r.fired:
		t.Fatalf("the watchdog fired: %s (causes %+v)", why, r.seen())
	case <-time.After(d):
	}
}

func TestActivityTracker_IdleWithNothingInFlightFiresOnce(t *testing.T) {
	rec := newStallRecorder()
	a := newActivityTracker(40*time.Millisecond, rec.stalled)
	defer a.stop()

	cause := rec.waitFired(t, 5*time.Second)
	if cause.op != "" {
		t.Errorf("idle stall named op %q, want none", cause.op)
	}
	if cause.elapsed < 40*time.Millisecond {
		t.Errorf("idle stall after %s, want at least the 40ms limit", cause.elapsed)
	}
	// Decided once: activity after the stall does not re-arm it.
	a.touch()
	rec.assertQuietFor(t, 150*time.Millisecond, "a second time after the stall was decided")
	if n := len(rec.seen()); n != 1 {
		t.Errorf("stalls = %d, want 1", n)
	}
}

func TestActivityTracker_OperationHoldsTheIdleArmOffUntilItsDeadline(t *testing.T) {
	rec := newStallRecorder()
	a := newActivityTracker(30*time.Millisecond, rec.stalled)
	defer a.stop()

	started := time.Now()
	a.begin("tool:bash", 250*time.Millisecond)
	rec.assertQuietFor(t, 120*time.Millisecond, "past the idle limit while a tool was in flight")

	cause := rec.waitFired(t, 5*time.Second)
	if cause.op != "tool:bash" {
		t.Errorf("stall op = %q, want tool:bash", cause.op)
	}
	if since := time.Since(started); since < 250*time.Millisecond {
		t.Errorf("stalled after %s, before the operation's 250ms deadline", since)
	}
}

func TestActivityTracker_EndReturnsToTheIdleArm(t *testing.T) {
	rec := newStallRecorder()
	a := newActivityTracker(60*time.Millisecond, rec.stalled)
	defer a.stop()

	end := a.begin("clone", time.Hour)
	time.Sleep(20 * time.Millisecond)
	end()
	if _, op := a.snapshot(); op != "" {
		t.Errorf("op after end = %q, want none", op)
	}
	cause := rec.waitFired(t, 5*time.Second)
	if cause.op != "" {
		t.Errorf("stall after the operation ended named %q, want the idle arm", cause.op)
	}
}

func TestActivityTracker_ProgressExtendsTheDeadline(t *testing.T) {
	rec := newStallRecorder()
	a := newActivityTracker(time.Hour, rec.stalled)
	defer a.stop()

	a.begin("provider", 80*time.Millisecond)
	// Four bounds' worth of chunks, each well inside the bound after the one
	// before it.
	for i := 0; i < 16; i++ {
		time.Sleep(20 * time.Millisecond)
		a.progress(80 * time.Millisecond)
	}
	select {
	case <-rec.fired:
		t.Fatalf("a stream producing a chunk every 20ms was stopped against an 80ms bound: %+v", rec.seen())
	default:
	}
	cause := rec.waitFired(t, 5*time.Second)
	if cause.op != "provider" {
		t.Errorf("stall op = %q, want provider", cause.op)
	}
}

func TestActivityTracker_EndAfterAReplacingBeginIsANoOp(t *testing.T) {
	rec := newStallRecorder()
	a := newActivityTracker(time.Hour, rec.stalled)
	defer a.stop()

	endFirst := a.begin("tool:read", time.Hour)
	a.begin("tool:bash", 80*time.Millisecond)
	endFirst()
	if _, op := a.snapshot(); op != "tool:bash" {
		t.Fatalf("op after the replaced operation's end = %q, want tool:bash still in flight", op)
	}
	if cause := rec.waitFired(t, 5*time.Second); cause.op != "tool:bash" {
		t.Errorf("stall op = %q, want the replacing tool:bash", cause.op)
	}
}

// TestActivityTracker_CheckDecidesOnTheStateItReads: Reset cannot unschedule a
// callback the runtime already dispatched, so a callback can run just after
// activity it was armed before. It has to read the state it finds, and stand
// down. The test stages that state and runs the callback by hand, because
// when the runtime dispatches a timer is not something a test can schedule.
func TestActivityTracker_CheckDecidesOnTheStateItReads(t *testing.T) {
	rec := newStallRecorder()
	a := newActivityTracker(time.Hour, rec.stalled)
	defer a.stop()

	// The state a callback was dispatched for: the last activity an idle
	// limit ago. The timer itself is an hour out, so only the explicit check
	// calls below run the decision.
	a.mu.Lock()
	a.lastActivity = time.Now().Add(-2 * time.Hour)
	a.mu.Unlock()

	// Activity lands, and the callback dispatched before it runs after it.
	a.touch()
	a.check()
	select {
	case <-rec.fired:
		t.Fatalf("a callback that ran after fresh activity stopped the engagement: %+v", rec.seen())
	default:
	}

	// The control: with no activity since, the same check decides a stall.
	a.mu.Lock()
	a.lastActivity = time.Now().Add(-2 * time.Hour)
	a.mu.Unlock()
	a.check()
	rec.waitFired(t, time.Second)
}

// TestActivityTracker_ABlockedCallerDoesNotDelayTheTimer is the failure the
// watchdog's own timer exists for: the goroutine that began an operation is
// blocked and will never report its end, and the stall still lands.
func TestActivityTracker_ABlockedCallerDoesNotDelayTheTimer(t *testing.T) {
	rec := newStallRecorder()
	a := newActivityTracker(time.Hour, rec.stalled)
	defer a.stop()

	block := make(chan struct{})
	defer close(block)
	go func() {
		end := a.begin("provider", 60*time.Millisecond)
		<-block
		end()
	}()
	if cause := rec.waitFired(t, 5*time.Second); cause.op != "provider" {
		t.Errorf("stall op = %q, want provider", cause.op)
	}
}

func TestActivityTracker_StopEndsTheWatchdog(t *testing.T) {
	rec := newStallRecorder()
	a := newActivityTracker(30*time.Millisecond, rec.stalled)
	a.stop()
	rec.assertQuietFor(t, 120*time.Millisecond, "after the engagement ended")
}

func TestActivityTracker_SnapshotAndANilTracker(t *testing.T) {
	a := newActivityTracker(time.Hour, func(stallCause) {})
	defer a.stop()
	time.Sleep(20 * time.Millisecond)
	idle, op := a.snapshot()
	if idle < 20*time.Millisecond || op != "" {
		t.Errorf("snapshot = (%s, %q), want at least 20ms idle and no op", idle, op)
	}
	a.begin("rehydrate", time.Hour)
	if idle, op := a.snapshot(); idle > 20*time.Millisecond || op != "rehydrate" {
		t.Errorf("snapshot after begin = (%s, %q), want fresh activity and rehydrate", idle, op)
	}

	// Every method is a no-op on a nil tracker.
	var none *activityTracker
	none.begin("x", time.Second)()
	none.progress(time.Second)
	none.touch()
	none.stop()
	if idle, op := none.snapshot(); idle != 0 || op != "" {
		t.Errorf("nil snapshot = (%s, %q), want (0, \"\")", idle, op)
	}
}

func TestStallOpLabel(t *testing.T) {
	for op, want := range map[string]string{
		"":                     "idle",
		"provider":             "provider",
		"tool:bash":            "tool",
		"tool:mcp:weird:name":  "tool",
		"awaiting_credentials": "awaiting_credentials",
	} {
		if got := stallOpLabel(op); got != want {
			t.Errorf("stallOpLabel(%q) = %q, want %q", op, got, want)
		}
	}
}

func TestStopParkReason(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errStalled)
	if got := stopParkReason(ctx); got != domain.ParkReasonStalled {
		t.Errorf("stopParkReason(stalled) = %q, want stalled", got)
	}
	// A stall is a stop, not a fence.
	if leaseFenced(ctx) {
		t.Error("leaseFenced matches errStalled; a stalled holder still owns its claim and parks through it")
	}
	child, cancelChild := context.WithCancel(ctx)
	defer cancelChild()
	if got := stopParkReason(child); got != domain.ParkReasonStalled {
		t.Errorf("stopParkReason on a child of the stalled claim context = %q, want stalled", got)
	}
	plain, cancelPlain := context.WithCancel(context.Background())
	cancelPlain()
	if got := stopParkReason(plain); got != domain.ParkReasonUserCancelled {
		t.Errorf("stopParkReason(cancel) = %q, want user_cancelled", got)
	}
}

// TestRenewClaimLease_CopiesTheTrackersActivity: each renewal carries the
// engagement's idle and in-flight operation to the claim row.
func TestRenewClaimLease_CopiesTheTrackersActivity(t *testing.T) {
	fake := &fakeRenewalStore{}
	s := leaseTestSpawner(t, fake, 20*time.Millisecond, time.Second, 90*time.Second)
	conv := leaseTestConversation()
	stop := s.startActivityTracker(conv, func(error) {})
	defer stop()
	s.activityFor(conv.ID).begin("tool:bash", time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.renewClaimLease(ctx, conv, time.Now(), func(error) {}) }()
	deadline := time.After(5 * time.Second)
	for len(fake.seen()) < 2 {
		select {
		case <-deadline:
			t.Fatal("no renewals in 5s")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
	for i, c := range fake.seen() {
		if c.op != "tool:bash" {
			t.Errorf("renewal %d op = %q, want tool:bash", i, c.op)
		}
		if c.idle < 0 || c.idle > time.Minute {
			t.Errorf("renewal %d idle = %s, want the tracker's small positive reading", i, c.idle)
		}
	}
}

// stallCounter swaps the stall counter for one on a manual reader and returns
// a reader of its value for op.
func stallCounter(t *testing.T) func(op string) int64 {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	prev := engagementStalls.Swap(newEngagementStallStats(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))
	t.Cleanup(func() { engagementStalls.Store(prev) })
	read := func(op string) int64 {
		t.Helper()
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect metrics: %v", err)
		}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != "engagements.stalled" {
					continue
				}
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("engagements.stalled is %T, want an int64 sum", m.Data)
				}
				for _, dp := range sum.DataPoints {
					if v, ok := dp.Attributes.Value(attribute.Key("op")); ok && v.AsString() == op {
						return dp.Value
					}
				}
			}
		}
		return 0
	}
	// The count lands after the cancel the test observes, so a reader waits
	// briefly for it rather than racing the watchdog's goroutine.
	return func(op string) int64 {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			if v := read(op); v != 0 || time.Now().After(deadline) {
				return v
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// stallFixture is one claimed native conversation with a registered tracker
// whose stall cancels claimCtx, as dispatchClaimedConversation wires it.
type stallFixture struct {
	s              *Spawner
	database       *sql.DB
	conversationID string
	claimID        string
	task           domain.Task
	claimCtx       context.Context
}

func newStallFixture(t *testing.T, conversationID string, timings activityTimings) stallFixture {
	t.Helper()
	database := newDelegateTestDB(t)
	seedConversation(t, database, conversationID, "", "")
	claimID := markEngaged(t, database, conversationID)
	markNative(t, database, conversationID)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-5")
	s.setActivityTimings(timings)

	var taskID string
	if err := database.QueryRow(`SELECT task_id FROM conversations WHERE id = ?`, conversationID).Scan(&taskID); err != nil {
		t.Fatalf("read task_id: %v", err)
	}
	claimCtx, fence := context.WithCancelCause(context.Background())
	t.Cleanup(func() { fence(nil) })
	stop := s.startActivityTracker(&domain.Conversation{ID: conversationID, OrgID: runmode.LocalDefaultOrgID, ClaimID: claimID}, fence)
	t.Cleanup(stop)
	return stallFixture{s: s, database: database, conversationID: conversationID, claimID: claimID, task: loadTask(t, s, taskID), claimCtx: claimCtx}
}

// assertParkedStalled reads the conversation and its claim back.
func (f stallFixture) assertParkedStalled(t *testing.T) {
	t.Helper()
	var status, parkReason string
	var intent bool
	if err := f.database.QueryRow(
		`SELECT COALESCE(status, ''), COALESCE(park_reason, ''), stop_requested_at IS NOT NULL FROM conversations WHERE id = ?`, f.conversationID,
	).Scan(&status, &parkReason, &intent); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if status != "open" || parkReason != string(domain.ParkReasonStalled) || intent {
		t.Errorf("conversation = (%q, %q, intent %v), want (open, stalled, cleared)", status, parkReason, intent)
	}
	var outcome string
	if err := f.database.QueryRow(`SELECT COALESCE(outcome, '') FROM claims WHERE id = ?`, f.claimID).Scan(&outcome); err != nil {
		t.Fatalf("read claim: %v", err)
	}
	if outcome != "cancelled" {
		t.Errorf("claim outcome = %q, want cancelled (the park is deliberate)", outcome)
	}
}

// silentProvider never sends a byte: it returns only when its context ends,
// the way a request blocked on a black-holed endpoint does.
type silentProvider struct{ calls atomic.Int32 }

func (p *silentProvider) Stream(ctx context.Context, _ inference.Request) (*inference.Completion, error) {
	p.calls.Add(1)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (f stallFixture) runNative(t *testing.T, provider agentloop.Provider, tools agentloop.ToolHost) agentloop.Result {
	t.Helper()
	transcript := newNativeTranscript(f.s, runmode.LocalDefaultOrgID, f.conversationID, f.claimID)
	if _, err := transcript.Insert(context.Background(), runmode.LocalDefaultOrgID, pendingUserInput(f.conversationID, runmode.LocalDefaultUserID, "do the work")); err != nil {
		t.Fatalf("seed the mission: %v", err)
	}
	timings := f.s.resolvedActivityTimings()
	engine := &agentloop.Engine{
		Transcript:  transcript,
		Credentials: staticGateCredentials{provider: provider},
		Tools:       tools,
		Activity:    engineActivity{f.s.activityFor(f.conversationID)},
		ActivityBounds: agentloop.ActivityBounds{
			Provider: timings.providerByte,
			Tool:     timings.toolCall,
		},
		Retry: agentloop.RetryPolicy{Sleep: func(context.Context, time.Duration) error { return nil }},
	}
	return engine.Run(f.claimCtx, agentloop.Params{
		OrgID:          runmode.LocalDefaultOrgID,
		ConversationID: f.conversationID,
		Model:          "claude-sonnet-4-5",
		SystemPrompt:   "system",
		HasBlueprint:   true,
	})
}

// TestNativeStall_ProviderWithNoFirstByteParksStalled: a provider attempt
// that never produces a byte is stopped at the provider bound, and the
// conversation parks open as stalled rather than failing or retrying.
func TestNativeStall_ProviderWithNoFirstByteParksStalled(t *testing.T) {
	stalls := stallCounter(t)
	f := newStallFixture(t, "r-stall-provider", activityTimings{idle: time.Hour, providerByte: 100 * time.Millisecond})

	provider := &silentProvider{}
	result := f.runNative(t, provider, &recordingToolHost{})
	if result.Kind != agentloop.ResultCancelled {
		t.Fatalf("result = %v (err %v), want cancelled", result.Kind, result.Err)
	}
	if cause := context.Cause(f.claimCtx); !errors.Is(cause, errStalled) {
		t.Fatalf("claim context cause = %v, want errStalled", cause)
	}
	if n := provider.calls.Load(); n != 1 {
		t.Errorf("provider attempts = %d, want 1 — a stall stops the engagement, it does not retry", n)
	}

	if fenced := f.s.recordNativeResult(f.claimCtx, runmode.LocalDefaultOrgID, f.conversationID, f.task,
		runConfig{orgID: runmode.LocalDefaultOrgID, claimID: f.claimID},
		"", "", "manual", runmode.LocalDefaultUserID, time.Now(), result, nil); fenced {
		t.Fatal("the stalled engagement reported a fence; it still holds its claim")
	}
	f.assertParkedStalled(t)
	if got := stalls("provider"); got != 1 {
		t.Errorf("engagements.stalled{op=provider} = %d, want 1", got)
	}
}

// blockingToolHost holds a call until the engagement's context ends, then
// fails it as the transport would once the jail is torn down.
type blockingToolHost struct{ ctx context.Context }

func (h blockingToolHost) Call(string, map[string]any) (agentloop.ToolOutcome, error) {
	<-h.ctx.Done()
	return agentloop.ToolOutcome{}, errors.New("agentloop: read tool response: use of closed network connection")
}

func (blockingToolHost) Close() error { return nil }

// TestNativeStall_BlockedToolParksStalledNotFailed: a tool call past its
// bound is a stall. The transport error the call returns once the jail goes
// is the stop's doing, so the conversation parks rather than failing as a
// dead tool host.
func TestNativeStall_BlockedToolParksStalledNotFailed(t *testing.T) {
	stalls := stallCounter(t)
	f := newStallFixture(t, "r-stall-tool", activityTimings{idle: time.Hour, toolCall: 100 * time.Millisecond})

	provider := &scriptedGateProvider{turns: []gateTurn{{calls: []domain.ToolCall{bashCall("c1", "make test")}}}}
	result := f.runNative(t, provider, blockingToolHost{ctx: f.claimCtx})
	if result.Kind != agentloop.ResultCancelled {
		t.Fatalf("result = %v (err %v), want cancelled", result.Kind, result.Err)
	}

	if fenced := f.s.recordNativeResult(f.claimCtx, runmode.LocalDefaultOrgID, f.conversationID, f.task,
		runConfig{orgID: runmode.LocalDefaultOrgID, claimID: f.claimID},
		"", "", "manual", runmode.LocalDefaultUserID, time.Now(), result, nil); fenced {
		t.Fatal("the stalled engagement reported a fence; it still holds its claim")
	}
	if got := storedStatus(t, f.database, f.conversationID); got == "failed" {
		t.Fatal("a slow tool failed the conversation; it should park as a stall")
	}
	f.assertParkedStalled(t)
	if got := stalls("tool"); got != 1 {
		t.Errorf("engagements.stalled{op=tool} = %d, want 1", got)
	}
}

// sdkStallRun drives the SDK driver the way runLiveAndDrive does, over a fake
// process whose stream the test feeds through the activity sink.
type sdkStallRun struct {
	sink *activitySink
	proc *fakeLiveProc
	done chan liveOutcome
}

func (f stallFixture) startSDK(t *testing.T) sdkStallRun {
	t.Helper()
	proc := newFakeLiveProc("sess-" + f.conversationID)
	sink := newActivitySink(agentproc.NoopSink{}, nil, f.s.activityFor(f.conversationID), f.s.resolvedActivityTimings())
	done := make(chan liveOutcome, 1)
	go func() {
		done <- f.s.driveLiveConversation(f.claimCtx, liveParkContext{
			orgID: runmode.LocalDefaultOrgID, conversationID: f.conversationID, claimID: f.claimID,
			runtime: domain.ConversationRuntimeSDK,
		}, proc, make(chan *agentproc.Result))
	}()
	return sdkStallRun{sink: sink, proc: proc, done: done}
}

// park settles the stop the way runAgent's cancelled arm does.
func (f stallFixture) parkLikeRunAgent(t *testing.T) {
	t.Helper()
	if f.s.parkConversationOpen(f.claimCtx, liveParkContext{
		orgID: runmode.LocalDefaultOrgID, conversationID: f.conversationID, claimID: f.claimID,
		reason: db.ParkStopped(stopParkReason(f.claimCtx), ""), runtime: domain.ConversationRuntimeSDK,
	}, "") {
		t.Fatal("the stall park was fenced; the engagement still holds its claim")
	}
}

// TestSDKStall_ARunningToolIsNotIdle is the case the retired idle timer got
// wrong: a tool call running past the idle limit emits nothing, and it is
// work. It is stopped only once it outlives its own bound.
func TestSDKStall_ARunningToolIsNotIdle(t *testing.T) {
	stalls := stallCounter(t)
	f := newStallFixture(t, "r-sdk-tool", activityTimings{idle: 60 * time.Millisecond, toolCall: 400 * time.Millisecond})
	run := f.startSDK(t)

	run.sink.OnLine()
	run.sink.OnToolUse("toolu_1", "Bash")

	select {
	case out := <-run.done:
		t.Fatalf("the driver returned %+v while a tool was running inside its bound", out)
	case <-time.After(250 * time.Millisecond):
	}
	if f.claimCtx.Err() != nil {
		t.Fatalf("the engagement was stopped at %v, past the idle limit but inside the tool bound", context.Cause(f.claimCtx))
	}

	var out liveOutcome
	select {
	case out = <-run.done:
	case <-time.After(5 * time.Second):
		t.Fatal("a tool past its bound was never stopped")
	}
	if !errors.Is(context.Cause(f.claimCtx), errStalled) || out.err == nil {
		t.Fatalf("driver outcome = %+v, cause %v; want the cancel arm under errStalled", out, context.Cause(f.claimCtx))
	}
	if !run.proc.wasClosed() {
		t.Error("the stopped driver left the process running")
	}
	f.parkLikeRunAgent(t)
	f.assertParkedStalled(t)
	if got := stalls("tool"); got != 1 {
		t.Errorf("engagements.stalled{op=tool} = %d, want 1", got)
	}
}

// TestSDKStall_ASilentProcessIsStoppedByTheIdleArm: a process that says
// nothing more after its first line, with nothing in flight, is a stall.
func TestSDKStall_ASilentProcessIsStoppedByTheIdleArm(t *testing.T) {
	stalls := stallCounter(t)
	f := newStallFixture(t, "r-sdk-idle", activityTimings{idle: 80 * time.Millisecond})
	run := f.startSDK(t)
	run.sink.OnLine()

	select {
	case <-run.done:
	case <-time.After(5 * time.Second):
		t.Fatal("a silent process was never stopped")
	}
	if !errors.Is(context.Cause(f.claimCtx), errStalled) {
		t.Fatalf("cause = %v, want errStalled", context.Cause(f.claimCtx))
	}
	f.parkLikeRunAgent(t)
	f.assertParkedStalled(t)
	if got := stalls("idle"); got != 1 {
		t.Errorf("engagements.stalled{op=idle} = %d, want 1", got)
	}
}

// TestSDKStall_TheResumeParksAsAStall: a resume runs the same driver under
// the same engagement's tracker, so the one thing it builds for itself — the
// park its cancelled arm writes — has to name the stall too.
func TestSDKStall_TheResumeParksAsAStall(t *testing.T) {
	f := newStallFixture(t, "r-sdk-resume", activityTimings{idle: 80 * time.Millisecond})
	run := f.startSDK(t)
	run.sink.OnLine()
	select {
	case <-run.done:
	case <-time.After(5 * time.Second):
		t.Fatal("a silent resumed process was never stopped")
	}
	conv := &domain.Conversation{ID: f.conversationID, OrgID: runmode.LocalDefaultOrgID, ClaimID: f.claimID, Runtime: domain.ConversationRuntimeSDK}
	park := resumeParkContext(f.claimCtx, runmode.LocalDefaultOrgID, conv, runmode.LocalDefaultUserID)
	if park.reason.Reason != domain.ParkReasonStalled {
		t.Fatalf("resume park reason = %q, want stalled", park.reason.Reason)
	}
	if f.s.markConversationOpen(f.claimCtx, park) {
		t.Fatal("the resume's stall park was fenced")
	}
	f.assertParkedStalled(t)
}

// TestActivitySink_ToolBookkeeping pins how the stream's tool calls map onto
// operations: a result ends its call, a permission prompt replaces the call
// while a person decides and gives it back afterwards, and a turn end ends
// whatever the turn left without a result.
func TestActivitySink_ToolBookkeeping(t *testing.T) {
	a := newActivityTracker(time.Hour, func(stallCause) {})
	defer a.stop()
	sink := newActivitySink(agentproc.NoopSink{}, nil, a, activityTimings{toolCall: time.Hour, permission: time.Hour})
	op := func() string { _, o := a.snapshot(); return o }

	sink.OnToolUse("t1", "Write")
	if got := op(); got != "tool:Write" {
		t.Fatalf("op after tool_use = %q, want tool:Write", got)
	}
	done := sink.OnPermission("t1")
	if got := op(); got != "permission" {
		t.Fatalf("op during the prompt = %q, want permission", got)
	}
	done()
	if got := op(); got != "tool:Write" {
		t.Fatalf("op after an answered prompt = %q, want the approved tool back in flight", got)
	}
	sink.OnToolResult("t1")
	if got := op(); got != "" {
		t.Fatalf("op after the result = %q, want none", got)
	}

	// A prompt for a call whose tool_use has not been read yet gives nothing
	// back; the tool_use begins it when it arrives.
	done = sink.OnPermission("t2")
	done()
	if got := op(); got != "" {
		t.Fatalf("op after a prompt for an unread call = %q, want none", got)
	}

	// Parallel calls: the oldest pending call names the operation, one stays
	// in flight while any is pending whichever order the results come in, and
	// the turn end clears what is left.
	sink.OnToolUse("p1", "Read")
	sink.OnToolUse("p2", "Grep")
	sink.OnToolUse("p3", "Bash")
	if got := op(); got != "tool:Read" {
		t.Fatalf("op with three calls pending = %q, want the oldest, tool:Read", got)
	}
	sink.OnToolResult("p2")
	if got := op(); got != "tool:Read" {
		t.Fatalf("op after a later call returned first = %q, want tool:Read", got)
	}
	sink.OnToolResult("p1")
	if got := op(); got != "tool:Bash" {
		t.Fatalf("op after the oldest returned = %q, want tool:Bash", got)
	}
	sink.OnTurnEnd()
	if got := op(); got != "" {
		t.Fatalf("op after the turn end = %q, want none", got)
	}
	if len(sink.pending) != 0 || len(sink.toolNames) != 0 {
		t.Errorf("the turn end left tool bookkeeping behind: %v %v", sink.pending, sink.toolNames)
	}
}

// TestActivitySink_ACallOutlivingItsParallelSiblingIsNotIdle: a result that
// arrives while another call is still running leaves that call in flight, so
// the watchdog holds to the tool bound rather than the idle limit. The later
// call returns first here, the order in which the most recent begin is not
// the call still running.
func TestActivitySink_ACallOutlivingItsParallelSiblingIsNotIdle(t *testing.T) {
	rec := newStallRecorder()
	a := newActivityTracker(60*time.Millisecond, rec.stalled)
	defer a.stop()
	sink := newActivitySink(agentproc.NoopSink{}, nil, a, activityTimings{toolCall: 400 * time.Millisecond})

	sink.OnToolUse("p1", "Bash")
	sink.OnToolUse("p2", "Read")
	sink.OnToolResult("p2")

	rec.assertQuietFor(t, 250*time.Millisecond, "a call still running past the idle limit")
	if got := rec.waitFired(t, 5*time.Second); got.op != "tool:Bash" {
		t.Fatalf("stall op = %q, want the call still running, tool:Bash", got.op)
	}
}

// TestSDKStall_AnUnansweredPromptIsDeniedNotStalled: the prompt's own timeout
// is the bound that decides an unanswered prompt. The watchdog's operation
// sits past it, so the agent is told no and carries on rather than being
// stopped.
func TestSDKStall_AnUnansweredPromptIsDeniedNotStalled(t *testing.T) {
	f := newStallFixture(t, "r-sdk-perm", activityTimings{idle: time.Hour, permission: 80 * time.Millisecond})
	sink := newActivitySink(agentproc.NoopSink{}, nil, f.s.activityFor(f.conversationID), f.s.resolvedActivityTimings())
	handler := f.s.BrowserPermissionHandler(runmode.LocalDefaultOrgID, f.conversationID, f.claimID, AbsentAutoDeny{})

	done := sink.OnPermission("toolu_perm")
	decision := handler(agentproc.PermissionRequest{ToolCallID: "toolu_perm", ToolName: "Bash"})
	done()

	if decision.Behavior != "deny" {
		t.Fatalf("unanswered prompt = %q, want the timeout's deny", decision.Behavior)
	}
	if f.claimCtx.Err() != nil {
		t.Fatalf("the watchdog stopped the engagement (%v) at the prompt's own timeout; the deny should land first", context.Cause(f.claimCtx))
	}
	if _, op := f.s.activityFor(f.conversationID).snapshot(); op != "" {
		t.Errorf("op after the prompt = %q, want none in flight", op)
	}
}
