package curator

import (
	"context"
	"fmt"
	"sync"

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
}

func newTurnSink(c *Curator, projectID, conversationID, requestID, claimID, orgID, creatorUserID string) *turnSink {
	return &turnSink{
		curator:        c,
		projectID:      projectID,
		conversationID: conversationID,
		requestID:      requestID,
		claimID:        claimID,
		orgID:          orgID,
		creatorUserID:  creatorUserID,
	}
}

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
		Reasoning:           msg.Reasoning,
		ContentBlocks:       msg.ContentBlocks,
	}
	// Per-message synthetic-claims wrap — each row attributes to the
	// requesting user. Short-lived tx (one INSERT) so the long-running
	// claude subprocess never holds a tx open.
	ctx := context.Background()
	var id int64
	if err := s.curator.stores.Tx.SyntheticClaimsWithTx(ctx, s.orgID, s.creatorUserID, func(ts db.TxStores) error {
		got, err := ts.Conversations.InsertMessage(ctx, s.orgID, row)
		if err != nil {
			return err
		}
		id = got
		return nil
	}); err != nil {
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
