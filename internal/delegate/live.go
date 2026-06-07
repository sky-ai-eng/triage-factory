// Live-run execution: every run executes as a long-lived agentproc.LiveRun
// (a streaming-input process you can message/interrupt) rather than a
// one-shot blocking call. The driver here turns one live invocation into a
// terminal disposition — a turn-terminal result, an idle hibernation, or a
// process exit — and feeds the shared post-stream branching (processCompletion)
// exactly as the one-shot path did.
//
// Two execution backends share one disposition shape (liveOutcome): the
// LiveRun driver (local, the warm/steerable path) and the one-shot fallback
// (multi-mode sandbox, where streaming-input isn't wired through gVisor yet).
// runAgent and ResumeWithMessage pick the backend via
// agentproc.InteractiveSupported() and branch only on the shared outcome.

package delegate

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
)

// liveProc is the slice of *agentproc.LiveRun the driver loop needs. Pulled
// out as an interface so driveLiveRun is unit-testable with a fake process
// (no subprocess) — the real *agentproc.LiveRun satisfies it.
type liveProc interface {
	Done() <-chan struct{}
	Result() *agentproc.Result
	SessionID() string
	Stderr() string
	Err() error
	Close() error
}

// liveParkContext carries the identity an idle hibernation needs to snapshot
// the workspace and park the run to awaiting_input.
type liveParkContext struct {
	orgID         string
	runID         string
	taskID        string
	namespace     string // blueprint_run_id — the snapshot/worktree key
	claudeCwd     string
	triggerType   string
	creatorUserID string
}

// liveOutcome is the disposition of one agent invocation, produced
// identically by the LiveRun driver and the one-shot fallback so runAgent /
// ResumeWithMessage branch on a single shape:
//
//   - result set        → a turn produced its terminal envelope; the caller
//     runs processCompletion (which owns yield→park, the gate, and finalize).
//   - hibernated true    → the live process went idle and was parked to
//     awaiting_input (snapshot written, status flipped); the caller returns
//     dormant, keeping the warm worktree.
//   - err set, no result → the process errored/was cancelled before any
//     terminal result; the caller routes through handleCancelled / failRun.
type liveOutcome struct {
	result     *agentproc.Result
	sessionID  string
	stderr     string
	hibernated bool
	err        error
}

// liveRunSpec bundles everything runLiveAndDrive needs to spawn, register,
// drive, and (on idle) hibernate one live agent invocation.
type liveRunSpec struct {
	park        liveParkContext
	opts        agentproc.RunOptions
	perms       agentproc.PermissionHandler
	sink        agentproc.Sink
	idleTimeout time.Duration // <=0 disables idle hibernation (e.g. the gate's bounded resume)
}

// runLiveAndDrive starts an interactive agent process for the run, registers
// it in the process registry so control ops can reach it, stamps executor
// ownership, and drives it to a terminal result or an idle hibernation. The
// process is closed and the handle deregistered by the time this returns.
// Shared by the initial run path and the resume path so every run executes
// uniformly as a LiveRun.
func (s *Spawner) runLiveAndDrive(ctx context.Context, spec liveRunSpec) liveOutcome {
	// Buffered so the reader goroutine's OnResult / activity callbacks never
	// block on a driver that's momentarily not selecting (both use a
	// non-blocking send, but a buffer keeps the common case lock-free).
	results := make(chan *agentproc.Result, 8)
	activity := make(chan struct{}, 64)

	spec.opts.OnResult = func(r *agentproc.Result) {
		// The driver consumes only the FIRST result and then closes the
		// process, so a full buffer here means later results arrived for a run
		// we've already decided to terminate — dropping them is intentional,
		// not lossy. A future multi-turn driver (P3 steering, which keeps the
		// process warm past a conversational turn) MUST revisit this drop.
		select {
		case results <- r:
		default:
		}
	}
	sink := newActivitySink(spec.sink, activity)

	lr, err := agentproc.RunInteractive(ctx, spec.opts, sink, spec.perms)
	if err != nil {
		return liveOutcome{err: err}
	}
	s.registerProc(spec.park.orgID, spec.park.runID, lr)
	defer s.deregisterProc(spec.park.runID)
	// Stamp run→executor ownership now the process is live (N=1 instance id;
	// the lease layer horizontal scaling adds builds on this column).
	s.stampExecutor(spec.park.orgID, spec.park.runID)

	out := s.driveLiveRun(ctx, spec.park, lr, results, activity, spec.idleTimeout)
	// Capture the final session id / stderr off the (now-closed) process for
	// the caller's completion + failure paths.
	out.sessionID = lr.SessionID()
	out.stderr = lr.Stderr()
	return out
}

// driveLiveRun is the select loop that resolves a live process into one of
// three terminal dispositions. The idle timer resets on every stream
// activity, so a slow-but-working agent (constant tool/text output) never
// hibernates — only a genuinely quiet process does. idleTimeout<=0 disables
// hibernation entirely (the gate's bounded resume always produces a result).
//
// The idle window is armed at entry (process spawn). The first stream event —
// typically system/init, sub-second — resets it, so idleTimeout is effectively
// the grace a *no-output* process gets before hibernating; keep it well above
// agent-startup latency (the 5-min default is; a tiny injected value will
// hibernate before the first turn, which is exactly what the idle test leans
// on).
//
// Pulled out from runLiveAndDrive so it can be driven with a fake proc +
// hand-fed channels in tests, without spawning a subprocess.
func (s *Spawner) driveLiveRun(ctx context.Context, park liveParkContext, proc liveProc, results <-chan *agentproc.Result, activity <-chan struct{}, idleTimeout time.Duration) liveOutcome {
	idle, idleC := newIdleTimer(idleTimeout)
	if idle != nil {
		defer idle.Stop()
	}

	for {
		select {
		case <-ctx.Done():
			// Hard cancel: the registered ctx cancel SIGKILLed the process.
			// Close (idempotent) and surface the ctx error so the caller routes
			// through its cancelled path.
			_ = proc.Close()
			return liveOutcome{err: ctx.Err()}

		case r := <-results:
			// A turn produced its terminal envelope. Close the process so the
			// session is free for processCompletion's gate to resume against,
			// and hand the result back.
			_ = proc.Close()
			return liveOutcome{result: r}

		case <-activity:
			resetIdleTimer(idle, idleTimeout)

		case <-idleC:
			// Quiet past the threshold with no terminal result — hibernate to a
			// durable resume. Reuses awaiting_input; no new status.
			_ = proc.Close()
			s.hibernatePark(park, proc.SessionID())
			return liveOutcome{hibernated: true}

		case <-proc.Done():
			// The process exited on its own (crash, or a Close from elsewhere).
			// Hand back whatever terminal result was folded, else the error.
			return liveOutcome{result: proc.Result(), err: proc.Err()}
		}
	}
}

// hibernatePark parks an idle live run to awaiting_input: snapshot the
// workspace (the cold-resume backstop), flip the status under a race guard,
// and nudge the board + UI. The mirror of persistYield's tail minus the
// yield_request message — idle hibernation isn't a yield, it just goes
// dormant on the same ResumeAfterYield path that a message or autonomous
// continuation later wakes.
func (s *Spawner) hibernatePark(park liveParkContext, sessionID string) {
	bgCtx := context.Background()

	// Snapshot BEFORE the flip: once dormant the run can resume on a host
	// without the warm worktree, so the blob must exist by the time the
	// status commits. Best-effort — the kept warm worktree is the fast path.
	if err := s.snapshotWorkspace(bgCtx, park.orgID, park.namespace, park.claudeCwd, sessionID); err != nil {
		log.Printf("[delegate] warning: snapshot workspace for run %s before idle hibernation: %v", park.runID, err)
	}

	var flipped bool
	var flipErr error
	if park.triggerType == "manual" {
		flipErr = s.tx.SyntheticClaimsWithTx(bgCtx, park.orgID, park.creatorUserID, func(ts db.TxStores) error {
			f, e := ts.AgentRuns.MarkAwaitingInput(bgCtx, park.orgID, park.runID)
			flipped = f
			return e
		})
	} else {
		flipped, flipErr = s.agentRuns.MarkAwaitingInputSystem(bgCtx, park.orgID, park.runID)
	}
	if flipErr != nil {
		log.Printf("[delegate] warning: mark awaiting_input for run %s on idle hibernation: %v", park.runID, flipErr)
		return
	}
	if !flipped {
		// A racing terminal flip (cancel/takeover) won — leave its status; the
		// snapshot it didn't need is dropped by that path's terminal cleanup.
		return
	}
	s.broadcastRunUpdate(park.orgID, park.runID, "awaiting_input")
	s.recomputeTaskBoardColumn(park.orgID, park.taskID)
	toast.Info(s.wsHub, park.orgID, fmt.Sprintf("Run %s hibernated while idle — resumes on the next message", shortRunID(park.runID)))
}

// runOneShot wraps the blocking one-shot agentproc.Run into the shared
// liveOutcome shape. The fallback backend for hosts where interactive runs
// aren't supported yet (multi-mode gVisor sandbox); behavior is byte-for-byte
// the historical one-shot path.
func (s *Spawner) runOneShot(ctx context.Context, opts agentproc.RunOptions, sink agentproc.Sink) liveOutcome {
	outcome, err := agentproc.Run(ctx, opts, sink)
	out := liveOutcome{err: err}
	if outcome != nil {
		out.result = outcome.Result
		out.sessionID = outcome.SessionID
		out.stderr = outcome.Stderr
	}
	return out
}

// newIdleTimer returns a started idle timer and its fire channel, or
// (nil, nil) when hibernation is disabled (d<=0) — a nil channel never
// selects, so the driver's idle case is simply unreachable then.
func newIdleTimer(d time.Duration) (*time.Timer, <-chan time.Time) {
	if d <= 0 {
		return nil, nil
	}
	t := time.NewTimer(d)
	return t, t.C
}

// resetIdleTimer re-arms an idle timer on stream activity, draining a
// concurrently-fired tick so the next idle window is the full timeout. A nil
// timer (hibernation disabled) is a no-op.
func resetIdleTimer(t *time.Timer, d time.Duration) {
	if t == nil {
		return
	}
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// activitySink decorates a Sink so the driver's idle timer resets on every
// stream message — the signal that the agent is actively working. The
// non-blocking bump means a wedged driver never back-pressures the reader
// goroutine that owns the sink.
type activitySink struct {
	inner    agentproc.Sink
	activity chan<- struct{}
}

func newActivitySink(inner agentproc.Sink, activity chan<- struct{}) activitySink {
	return activitySink{inner: inner, activity: activity}
}

func (a activitySink) OnSession(id string) error {
	a.bump()
	return a.inner.OnSession(id)
}

func (a activitySink) OnMessage(m *domain.AgentMessage) error {
	a.bump()
	return a.inner.OnMessage(m)
}

func (a activitySink) bump() {
	select {
	case a.activity <- struct{}{}:
	default:
	}
}

// Compile-time check that activitySink satisfies the Sink contract.
var _ agentproc.Sink = activitySink{}
