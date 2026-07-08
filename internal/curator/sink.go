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

// requestSink translates agentproc stream events into curator_messages
// rows + websocket pushes for one in-flight request. One sink per
// agentproc.Run call (constructed on each message dispatch in the
// per-project goroutine). The projectID and requestID are captured at
// construction so the broadcasts are keyed correctly when many
// projects' Curators are streaming simultaneously.
//
// Session id capture: only the very first message in a project's
// lifetime sees a fresh init event with a new session_id. Subsequent
// requests resume against the captured id so the init they emit
// re-broadcasts the same id. The sync.Once guard keeps us from
// writing the same value twice per request even though
// SetCuratorSessionID is idempotent at the DB layer.
type requestSink struct {
	curator       *Curator
	projectID     string
	requestID     string
	orgID         string
	creatorUserID string

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

func newRequestSink(c *Curator, projectID, requestID, orgID, creatorUserID string) *requestSink {
	return &requestSink{
		curator:       c,
		projectID:     projectID,
		requestID:     requestID,
		orgID:         orgID,
		creatorUserID: creatorUserID,
	}
}

// OnSession persists the captured session_id on the project row the
// first time it's observed in the request's lifetime. Subsequent
// resumes within the same project re-emit the same id and the
// persisted-flag short-circuits the redundant write.
func (s *requestSink) OnSession(sessionID string) error {
	// The session-id update is part of this user's turn — wrap in
	// synthetic claims so multi-mode RLS attributes the bookkeeping
	// write to the same identity as the message writes. Background
	// ctx is fine here: this fires from the agentproc sink, not from
	// a cancellable msgCtx, and the write should land even if the
	// dispatch is being torn down.
	//
	// sync.Once ensures the write happens at most once across
	// concurrent callers — agentproc is single-threaded today but
	// this protection is what the comment on sessionOnce promises.
	// Errors from the first attempt are captured under sessionErrMu
	// and returned to every subsequent caller so a transient failure
	// on the first OnSession invocation isn't swallowed by a later
	// duplicate-call no-op.
	s.sessionOnce.Do(func() {
		ctx := context.Background()
		err := s.curator.stores.Tx.SyntheticClaimsWithTx(ctx, s.orgID, s.creatorUserID, func(ts db.TxStores) error {
			return ts.Projects.SetCuratorSessionID(ctx, s.orgID, s.projectID, sessionID)
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

// OnMessage inserts the parsed assistant or tool message into
// curator_messages and broadcasts it to the websocket so the open
// project page paints it as it arrives. Per-row failures are
// returned to agentproc which logs and continues.
func (s *requestSink) OnMessage(msg *domain.AgentMessage) error {
	curatorMsg := &domain.CuratorMessage{
		RequestID:           s.requestID,
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
	}
	// Per-message synthetic-claims wrap — each row attributes to the
	// requesting user. Short-lived tx (one INSERT) so the long-running
	// claude subprocess never holds a tx open.
	ctx := context.Background()
	var id int64
	if err := s.curator.stores.Tx.SyntheticClaimsWithTx(ctx, s.orgID, s.creatorUserID, func(ts db.TxStores) error {
		got, err := ts.Curator.InsertMessage(ctx, s.orgID, curatorMsg)
		if err != nil {
			return err
		}
		id = got
		return nil
	}); err != nil {
		return fmt.Errorf("insert curator message: %w", err)
	}
	curatorMsg.ID = int(id)
	s.curator.broadcastMessage(s.orgID, s.projectID, curatorMsg)
	return nil
}

// Compile-time check that requestSink satisfies the agentproc.Sink
// contract.
var _ agentproc.Sink = (*requestSink)(nil)

// broadcastMessage pushes a CuratorMessage onto the websocket. Empty
// hub is tolerated (test harnesses construct curators without a hub).
// orgID scopes the event to the owning tenant so the hub's per-
// connection filter routes it only to clients authed against that
// org — leaks of curator transcripts across tenants would surface
// another team's prompts and responses.
func (c *Curator) broadcastMessage(orgID, projectID string, msg *domain.CuratorMessage) {
	if c.wsHub == nil {
		return
	}
	c.wsHub.Broadcast(websocket.Event{
		Type:      "curator_message",
		OrgID:     orgID,
		ProjectID: projectID,
		Data:      msg,
	})
}

// broadcastRequestUpdate pushes a status transition for a request.
// Frontend uses this to flip the UI from "queued" → "running" →
// terminal without re-fetching the request row.
func (c *Curator) broadcastRequestUpdate(orgID, projectID, requestID, status string) {
	if c.wsHub == nil {
		return
	}
	c.wsHub.Broadcast(websocket.Event{
		Type:      "curator_request_update",
		OrgID:     orgID,
		ProjectID: projectID,
		Data: map[string]string{
			"request_id": requestID,
			"status":     status,
		},
	})
}
