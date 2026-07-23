package delegate

import (
	"context"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// runSink adapts an agentproc invocation to the delegate's storage:
// session ids land on runs.session_id, parsed messages land in
// run_messages, and both fan out to the websocket so the UI can
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
	triggerType   string
	creatorUserID string

	// sessionDelivered suppresses repeated OnSession handling within
	// this runSink instance. Some streams can emit system/init more
	// than once for the same session_id; while SetAgentRunSession is
	// idempotent at the DB layer, skipping duplicate handling also
	// avoids an extra running-status broadcast from the same stream.
	// Because each agentproc.Run call gets a fresh sink, this does
	// not deduplicate across separate resume invocations.
	sessionDelivered bool
}

func newRunSink(s *Spawner, orgID, runID, triggerType, creatorUserID string) *runSink {
	return &runSink{
		spawner:       s,
		orgID:         orgID,
		runID:         runID,
		triggerType:   triggerType,
		creatorUserID: creatorUserID,
	}
}

// OnSession persists the captured session_id and re-broadcasts the
// running status so the UI re-fetches the run row and picks up
// SessionID. The "Take over" button is gated on session id presence;
// without this nudge it stays hidden until the next status flip
// (often "running" → terminal), which is too late to be useful.
func (k *runSink) OnSession(sessionID string) error {
	if k.sessionDelivered {
		return nil
	}
	k.sessionDelivered = true
	bgCtx := context.Background()
	if k.triggerType == "manual" {
		if err := k.spawner.tx.SyntheticClaimsWithTx(bgCtx, k.orgID, k.creatorUserID, func(ts db.TxStores) error {
			return ts.AgentRuns.SetSession(bgCtx, k.orgID, k.runID, sessionID)
		}); err != nil {
			return fmt.Errorf("persist session_id: %w", err)
		}
	} else if err := k.spawner.agentRuns.SetSessionSystem(bgCtx, k.orgID, k.runID, sessionID); err != nil {
		return fmt.Errorf("persist session_id: %w", err)
	}
	k.spawner.broadcastRunUpdate(k.orgID, k.runID, "running")
	return nil
}

// OnMessage inserts the parsed assistant/tool message into
// run_messages and pushes it onto the websocket. Per-row failures
// are returned to agentproc, which logs and continues — losing one
// row is preferable to abandoning the run.
func (k *runSink) OnMessage(msg *domain.AgentMessage) error {
	bgCtx := context.Background()
	var id int64
	if k.triggerType == "manual" {
		if err := k.spawner.tx.SyntheticClaimsWithTx(bgCtx, k.orgID, k.creatorUserID, func(ts db.TxStores) error {
			i, ierr := ts.AgentRuns.InsertMessage(bgCtx, k.orgID, msg)
			if ierr != nil {
				return ierr
			}
			id = i
			return nil
		}); err != nil {
			return fmt.Errorf("insert message: %w", err)
		}
	} else {
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

// Compile-time check that runSink satisfies the agentproc.Sink
// contract.
var _ agentproc.Sink = (*runSink)(nil)
