package curator

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// turnSink translates agentproc stream events into messages rows +
// websocket pushes for one in-flight turn. One sink per agentproc.Run call
// (constructed on each dispatch in the per-project goroutine). Every row is
// stamped with the requesting user (turn attribution) and the turn's claim
// (per-engagement accounting — the release's token SUM keys on it); the
// broadcast carries the wire CuratorMessage shape with request_id = the
// turn id, so the frontend's rendering is unchanged.
//
// Session id capture: only the very first turn in a conversation's
// lifetime sees a fresh init event with a new session_id. Subsequent
// turns resume against the persisted id so the init they emit
// re-broadcasts the same id. The sync.Once guard keeps us from
// writing the same value twice per turn even though SetSDKSession is
// idempotent at the DB layer.
type turnSink struct {
	curator        *Curator
	projectID      string
	conversationID string
	requestID      string
	claimID        string
	orgID          string
	creatorUserID  string

	// sessionOnce guards the OnSession write so a future concurrent
	// ParseLine wouldn't double-persist if the underlying stream
	// parser ever changed. sync.Once gives the guarantee in one
	// primitive — the body runs exactly once across all callers,
	// even if two concurrent OnSession invocations land at the same
	// time. agentproc drives the sink from one goroutine today, but
	// the protection is cheap and matches how the delegate sink
	// handles the same hazard.
	sessionOnce  sync.Once
	sessionErr   error
	sessionErrMu sync.Mutex

	// cancelTurn kills this turn's subprocess. Fired when the DB refuses a
	// write because the turn's claim was released — the only way to stop an
	// agent whose output no longer has anywhere to go.
	cancelTurn context.CancelFunc

	// fenced records that refusal, for the dispatch loop to read once
	// agentproc returns: it must then release nothing and revert nothing,
	// because a successor engagement owns the conversation.
	fenced atomic.Bool
}

func newTurnSink(c *Curator, projectID, conversationID, requestID, claimID, orgID, creatorUserID string, cancelTurn context.CancelFunc) *turnSink {
	return &turnSink{
		curator:        c,
		projectID:      projectID,
		conversationID: conversationID,
		requestID:      requestID,
		claimID:        claimID,
		orgID:          orgID,
		creatorUserID:  creatorUserID,
		cancelTurn:     cancelTurn,
	}
}

// tripFence reacts to a refused write: kill the turn and record that this
// engagement is out. Nothing else — the successor owns the conversation's
// disposition, so this turn releases no claim and reverts no context.
//
// Error level on purpose: a fence trip means the cooperative self-fence
// failed to stop this executor in time, which is a fleet incident.
func (s *turnSink) tripFence(err error) {
	if s.fenced.Swap(true) {
		return
	}
	curatorLog.Error("claim fence tripped — this executor no longer owns the curator conversation; abandoning the turn without writing",
		"request", s.requestID, "conversation", s.conversationID, "claim_id", s.claimID, "org_id", s.orgID, "error", err)
	if s.cancelTurn != nil {
		s.cancelTurn()
	}
}

// fenceTripped reports whether this turn was fenced out mid-stream.
func (s *turnSink) fenceTripped() bool { return s.fenced.Load() }

// OnSession persists the captured session_id on the conversation the
// first time it's observed in the turn's lifetime. Subsequent resumes
// within the same conversation re-emit the same id and the sync.Once
// short-circuits the redundant write.
func (s *turnSink) OnSession(sessionID string) error {
	// The session-id update is part of this user's turn — wrap in
	// synthetic claims so multi-mode RLS attributes the bookkeeping
	// write to the same identity as the message writes. Background
	// ctx is fine here: this fires from the agentproc sink, not from
	// a cancellable msgCtx, and the write should land even if the
	// dispatch is being torn down.
	//
	// Errors from the first attempt are captured under sessionErrMu
	// and returned to every subsequent caller so a transient failure
	// on the first OnSession invocation isn't swallowed by a later
	// duplicate-call no-op.
	s.sessionOnce.Do(func() {
		ctx := context.Background()
		err := s.curator.stores.Tx.SyntheticClaimsWithTx(ctx, s.orgID, s.creatorUserID, func(ts db.TxStores) error {
			return ts.Curator.SetSDKSession(ctx, s.orgID, s.conversationID, sessionID)
		})
		if err != nil {
			s.sessionErrMu.Lock()
			s.sessionErr = fmt.Errorf("persist curator session_id: %w", err)
			s.sessionErrMu.Unlock()
		}
	})
	s.sessionErrMu.Lock()
	defer s.sessionErrMu.Unlock()
	return s.sessionErr
}

// OnMessage inserts the parsed assistant or tool message into the
// conversation's transcript and broadcasts it to the websocket so the open
// project page paints it as it arrives. Per-row failures are returned to
// agentproc which logs and continues.
func (s *turnSink) OnMessage(msg *domain.Message) error {
	row := &domain.Message{
		ConversationID:      s.conversationID,
		UserID:              s.creatorUserID,
		ClaimID:             s.claimID,
		Role:                msg.Role,
		Subtype:             msg.Subtype,
		Content:             msg.Content,
		ToolCalls:           msg.ToolCalls,
		ToolCallID:          msg.ToolCallID,
		IsError:             msg.IsError,
		Metadata:            msg.Metadata,
		Model:               msg.Model,
		InputTokens:         msg.InputTokens,
		OutputTokens:        msg.OutputTokens,
		CacheReadTokens:     msg.CacheReadTokens,
		CacheCreationTokens: msg.CacheCreationTokens,
		CreatedAt:           msg.CreatedAt,
		DurationMs:          msg.DurationMs,
		Reasoning:           msg.Reasoning,
		ContentBlocks:       msg.ContentBlocks,
	}
	// A curator turn is an executor engagement like any delegated run, so its
	// transcript writes name their own claim and are refused once that claim
	// is released — a turn whose executor was reaped must not keep writing
	// into a conversation the successor has picked up. The row still carries
	// the requesting user for turn attribution; what changed is that
	// ownership, not RLS, is what authorizes the write.
	ctx := context.Background()
	id, err := s.curator.stores.Conversations.InsertMessageForClaimSystem(ctx, s.orgID, s.claimID, row)
	if err != nil {
		if errors.Is(err, db.ErrClaimReleased) {
			s.tripFence(err)
		}
		return fmt.Errorf("insert curator message: %w", err)
	}
	row.ID = int(id)
	wire := row.ToDTO()
	s.curator.broadcastMessage(s.orgID, s.projectID, wire)
	return nil
}

// Compile-time check that turnSink satisfies the agentproc.Sink contract.
var _ agentproc.Sink = (*turnSink)(nil)

// broadcastMessage pushes a transcript row onto the websocket as the shared
// `message` event. Empty hub is tolerated (test harnesses construct curators
// without a hub). orgID scopes the event to the owning tenant so the hub's
// per-connection filter routes it only to clients authed against that org —
// leaks of curator transcripts across tenants would surface another team's
// prompts and responses.
func (c *Curator) broadcastMessage(orgID, projectID string, msg domain.MessageDTO) {
	if c.wsHub == nil {
		return
	}
	c.wsHub.Broadcast(websocket.Event{
		Type:           "message",
		OrgID:          orgID,
		ProjectID:      projectID,
		ConversationID: msg.ConversationID,
		Data:           msg,
	})
}

// broadcastRequestUpdate pushes a status transition for a turn as the shared
// `conversation_update` event. Frontend uses this to flip the UI from
// "queued" → "running" → terminal without re-fetching history.
func (c *Curator) broadcastRequestUpdate(orgID, projectID, requestID, status string) {
	if c.wsHub == nil {
		return
	}
	c.wsHub.Broadcast(websocket.Event{
		Type:      "conversation_update",
		OrgID:     orgID,
		ProjectID: projectID,
		Data: map[string]string{
			"request_id": requestID,
			"status":     status,
		},
	})
}
