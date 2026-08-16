package delegate

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// runSink adapts an agentproc invocation to the delegate's storage:
// session ids land on conversations.sdk_session_id, parsed messages land in
// messages, and both fan out to the websocket so the UI can
// react in real time.
//
// One sink per agentproc.Run call (initial invocation + each resume
// own a fresh sink). The runID is captured at construction so every
// row + broadcast is keyed correctly even when the spawner is
// servicing many runs concurrently.
//
// triggerType + creatorUserID encode the run's identity for the
// per-message pool routing: manual runs wrap each insert in a
// synthetic-claims tx so the writes pass RLS under tf_app; event-
// triggered runs (no user identity) route to the admin-pool System
// methods. The long-running stream rules out a goroutine-lifetime
// tx — SyntheticClaimsWithTx scopes JWT claims to one Postgres
// connection's transaction, which the agent subprocess would stream
// past on the next OnMessage.
type runSink struct {
	spawner       *Spawner
	orgID         string
	runID         string
	claimID       string
	triggerType   string
	creatorUserID string

	// fenced records that the DB refused a transcript write because this
	// engagement's claim is no longer live — a successor owns the
	// conversation. Read by runAgent, which then abandons the run without
	// writing a terminal, so the flag has to outlive the stream that set it.
	fenced atomic.Bool

	// sessionDelivered suppresses repeated OnSession handling within
	// this runSink instance. Some streams can emit system/init more
	// than once for the same session_id; while SetSession is
	// idempotent at the DB layer, skipping duplicate handling also
	// avoids an extra running-status broadcast from the same stream.
	// Because each agentproc.Run call gets a fresh sink, this does
	// not deduplicate across separate resume invocations.
	sessionDelivered bool
}

func newRunSink(s *Spawner, orgID, runID, claimID, triggerType, creatorUserID string) *runSink {
	return &runSink{
		spawner:       s,
		orgID:         orgID,
		runID:         runID,
		claimID:       claimID,
		triggerType:   triggerType,
		creatorUserID: creatorUserID,
	}
}

// OnSession persists the captured session_id and re-broadcasts the
// running status so the UI re-fetches the run row and picks up
// SessionID. The "Take over" button is gated on session id presence;
// without this nudge it stays hidden until the next status flip
// (often "running" → terminal), which is too late to be useful.
//
// The three arms are OnMessage's, for the same reason: an engagement writes
// as itself whatever the run's trigger type, and only a writer with no claim
// falls back to the pool split. The fence matters more here than anywhere
// else in this file. sdk_session_id is the resume coordinate, so a zombie's
// late init lands on the successor's row and is not discovered until the next
// resume tries to reconnect to a session that died with this process. A
// refusal therefore DISCARDS this id — there is no unfenced retry, because
// the write succeeding is precisely the harm.
func (k *runSink) OnSession(sessionID string) error {
	if k.sessionDelivered {
		return nil
	}
	k.sessionDelivered = true
	bgCtx := context.Background()
	switch {
	case k.claimID != "":
		if err := k.spawner.agentRuns.SetSessionForClaimSystem(bgCtx, k.orgID, k.runID, k.claimID, sessionID); err != nil {
			if errors.Is(err, db.ErrClaimReleased) {
				k.tripFence(err)
				return err
			}
			return fmt.Errorf("persist session_id: %w", err)
		}
	case k.triggerType == "manual":
		if err := k.spawner.tx.SyntheticClaimsWithTx(bgCtx, k.orgID, k.creatorUserID, func(ts db.TxStores) error {
			return ts.Conversations.SetSession(bgCtx, k.orgID, k.runID, sessionID)
		}); err != nil {
			return fmt.Errorf("persist session_id: %w", err)
		}
	default:
		if err := k.spawner.agentRuns.SetSessionSystem(bgCtx, k.orgID, k.runID, sessionID); err != nil {
			return fmt.Errorf("persist session_id: %w", err)
		}
	}
	k.spawner.broadcastRunUpdate(k.orgID, k.runID, "running")
	return nil
}

// OnMessage inserts the parsed assistant/tool message into
// messages and pushes it onto the websocket. Per-row failures
// are returned to agentproc, which logs and continues — losing one
// row is preferable to abandoning the run.
//
// The one failure that is not per-row is the fence: a refused write means
// this engagement no longer owns the conversation, and every row after it
// would interleave with a successor's. That one abandons the run.
func (k *runSink) OnMessage(msg *domain.Message) error {
	bgCtx := context.Background()
	var id int64
	switch {
	case k.claimID != "":
		// The engagement writes as itself. Manual and event-triggered runs
		// converge here: attribution rides on the claim, not on which pool
		// the statement runs through.
		i, ierr := k.spawner.agentRuns.InsertMessageForClaimSystem(bgCtx, k.orgID, k.claimID, msg)
		if ierr != nil {
			if errors.Is(ierr, db.ErrClaimReleased) {
				k.tripFence(ierr)
				return ierr
			}
			return fmt.Errorf("insert message: %w", ierr)
		}
		id = i
	case k.triggerType == "manual":
		if err := k.spawner.tx.SyntheticClaimsWithTx(bgCtx, k.orgID, k.creatorUserID, func(ts db.TxStores) error {
			i, ierr := ts.Conversations.InsertMessage(bgCtx, k.orgID, msg)
			if ierr != nil {
				return ierr
			}
			id = i
			return nil
		}); err != nil {
			return fmt.Errorf("insert message: %w", err)
		}
	default:
		i, ierr := k.spawner.agentRuns.InsertMessageSystem(bgCtx, k.orgID, msg)
		if ierr != nil {
			return fmt.Errorf("insert message: %w", ierr)
		}
		id = i
	}
	msg.ID = int(id)
	k.spawner.broadcastMessage(k.orgID, k.runID, msg)
	return nil
}

// tripFence reacts to a refused write: kill the agent, and leave the
// conversation entirely alone. The successor is mid-flight on it, so there is
// nothing this engagement may still say about it — no terminal status, no
// failure row, no claim release. Killing the process is the only action left,
// and stopping the stream is what keeps the rest of this turn's output from
// piling up behind an insert that will never succeed.
//
// Logged at error level on purpose: reaching here means the cooperative
// self-fence did not stop this executor in time, which is a fleet incident
// worth waking someone for, not a per-row hiccup.
func (k *runSink) tripFence(err error) {
	if k.fenced.Swap(true) {
		return
	}
	delegateLog.Error("claim fence tripped — this executor no longer owns the conversation; abandoning the run without writing",
		"run_id", k.runID, "claim_id", k.claimID, "org_id", k.orgID, "error", err)
	// Best-effort: no live handle means the process is already gone, which
	// is the outcome this call was after.
	k.spawner.getController().Cancel(k.runID)
}

// fenceTripped reports whether this engagement was fenced out mid-stream.
func (k *runSink) fenceTripped() bool { return k.fenced.Load() }

// Compile-time check that runSink satisfies the agentproc.Sink
// contract.
var _ agentproc.Sink = (*runSink)(nil)
