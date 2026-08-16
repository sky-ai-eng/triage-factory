package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/curator"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// curatorHandler serves the per-project curator chat endpoints. runtime is read
// through a getter so the handler always sees the current curator runtime,
// which is wired onto the server after construction.
//
// All DB access goes through tx (WithTx → tx.Curator) except the two claim
// writes (cancel of a running turn), which route through the admin-pool
// System door on curatorStore — tf_app cannot write claims, and the handler
// has already located the turn under the caller's own RLS view (the
// conversations private-visibility arm scopes history/cancel/reset to the
// requesting user's rows).
type curatorHandler struct {
	tx           db.TxRunner
	curatorStore db.CuratorStore
	ws           *websocket.Hub
	runtime      func() *curator.Curator
}

// Curator chat endpoints. Three operations the Projects
// page needs:
//
//   - POST .../messages   queue a user turn → 202 + {request_id}
//   - GET  .../messages   chat history (turns + their messages)
//   - DELETE .../in-flight cancel the active turn → 204
//
// All three are scoped to a single project. The runtime in
// internal/curator owns concurrency: cross-project messages run in
// parallel; same-project messages queue serially behind one CC
// subprocess at a time.
//
// The turn id on the wire (request_id) is the turn's user-message id
// rendered as a decimal string.

// writeCuratorUnavailable answers the curator routes on a deployment whose
// process runs no curator runtime — configuration, not a request fault, so it
// takes the same 409 NOT_CONFIGURED every other unconfigured subsystem gives.
func writeCuratorUnavailable(w http.ResponseWriter) {
	writeNotConfigured(w, "the curator is not configured on this deployment")
}

type curatorSendRequest struct {
	Content string `json:"content"`
}

type curatorSendResponse struct {
	RequestID string `json:"request_id"`
}

// curatorRequestJSON is the wire shape for a Curator chat turn — the frozen
// contract the frontend renders. The server synthesizes it from the live
// conversation's rows: a plain user message starts a turn, the claim its
// following rows carry supplies status + accounting, and the rows between it
// and the next user message are its agent-side stream.
type curatorRequestJSON struct {
	domain.CuratorRequest
	Messages []domain.MessageDTO `json:"messages"`
}

func (ch *curatorHandler) handleCuratorSend(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	cur := ch.runtime()
	if cur == nil {
		writeCuratorUnavailable(w)
		return
	}
	projectID, ok := projectIDOr404(w, r)
	if !ok {
		return
	}
	var project *domain.Project
	if err := ch.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		project, e = tx.Projects.Get(r.Context(), orgID, projectID)
		return e
	}); err != nil {
		internalError(w, "curator", err)
		return
	}
	if project == nil {
		notFound(w, "project")
		return
	}

	var req curatorSendRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	content := strings.TrimSpace(req.Content)
	if content == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonMissingField, Message: "content is required", Field: "content",
		})
		return
	}

	// Pass the requesting user's identity explicitly so the per-project
	// goroutine attributes every per-turn write accordingly. Both org
	// and user come from the request context — sentinel in local mode
	// via the shim, real values in multi mode.
	requestID, err := cur.SendMessage(r.Context(), projectID, orgID, userID, content)
	if err != nil {
		// A capless control pod with no eligible executor can't run the turn
		// anywhere — surface it as 503 (retryable once an executor is back),
		// not a 500.
		if errors.Is(err, curator.ErrNoCuratorExecutor) {
			httpx.WriteErrors(w, http.StatusServiceUnavailable, httpx.ErrorItem{
				Reason: httpx.ReasonUpstreamUnavailable, Message: err.Error(),
			})
			return
		}
		internalError(w, "curator", err)
		return
	}
	writeJSON(w, http.StatusAccepted, curatorSendResponse{RequestID: requestID})
}

// curatorHistoryRows bounds the curator transcript read to its most recent
// rows. **This is a declared cap, not pagination**, and the difference is
// deliberate: the response is not a list of messages, it is a list of
// synthesized *turns* — a user row plus the agent rows that followed it —
// so a page boundary in the middle of a turn would hand a client half a turn
// and no way to ask for the other half. Bounding the tail instead truncates
// only the oldest turn, which is what "the last N messages of a chat" already
// means. Sized to match the delegated transcript's own bound
// (transcriptPageSize) so the two chat surfaces hold the same amount of
// history.
const curatorHistoryRows = transcriptPageSize

func (ch *curatorHandler) handleCuratorHistory(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	projectID, ok := projectIDOr404(w, r)
	if !ok {
		return
	}
	var (
		project  *domain.Project
		messages []domain.Message
		claims   []domain.Claim
	)
	if err := ch.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		project, e = tx.Projects.Get(r.Context(), orgID, projectID)
		if e != nil || project == nil {
			return e
		}
		// Self-only under Postgres RLS: the private-visibility arm scopes
		// the conversation (and its rows) to the requesting creator.
		conv, e := tx.Curator.GetLiveConversation(r.Context(), orgID, projectID, userID)
		if e != nil || conv == nil {
			return e
		}
		messages, e = tx.Curator.ListConversationMessages(r.Context(), orgID, conv.ID, curatorHistoryRows)
		if e != nil {
			return e
		}
		claims, e = tx.Curator.ListClaims(r.Context(), orgID, conv.ID)
		return e
	}); err != nil {
		internalError(w, "curator", err)
		return
	}
	if project == nil {
		notFound(w, "project")
		return
	}

	writeJSON(w, http.StatusOK, synthesizeCuratorTurns(projectID, messages, claims))
}

// synthesizeCuratorTurns rebuilds the legacy turn wire shape from a curator
// conversation's rows: each plain user message (blank subtype) starts a turn whose
// status is 'queued' while the row is undelivered and otherwise derives from
// the claim its following rows carry (active → running; released outcome
// completed/cancelled/failed → done/cancelled/failed) — falling back to the
// claim stamped on the user row itself when nothing streamed, so a turn that
// never produced agent rows (a dead-lettered one especially) still renders
// its real terminal state instead of a fabricated 'done'. No claim anywhere
// → done. The rows between a user message and the next one are the turn's
// agent-side stream; injection rows are internal plumbing and never reach
// the wire (their accounting — none today — would still fold into the turn
// sums below). Cost and tokens are SUMs over the turn's rows: messages is
// the money/token ledger, and the terminal lump lands on the turn's last
// row (the user row itself when nothing streamed). Duration/turns still
// come from the claim's telemetry.
func synthesizeCuratorTurns(projectID string, messages []domain.Message, claims []domain.Claim) []curatorRequestJSON {
	claimByID := make(map[string]domain.Claim, len(claims))
	for _, c := range claims {
		claimByID[c.ID] = c
	}

	out := make([]curatorRequestJSON, 0)
	var cur *curatorRequestJSON
	var curClaimID string
	var curUserClaimID string
	var curQueued bool

	flush := func() {
		if cur == nil {
			return
		}
		claimID := curClaimID
		if claimID == "" {
			claimID = curUserClaimID
		}
		applyCuratorTurnStatus(cur, curQueued, claimID, claimByID)
		out = append(out, *cur)
		cur = nil
		curClaimID = ""
		curUserClaimID = ""
		curQueued = false
	}
	accumulate := func(m *domain.Message) {
		if cur == nil {
			return
		}
		if m.CostUSD != nil {
			cur.CostUSD += *m.CostUSD
		}
		if m.InputTokens != nil {
			cur.InputTokens += *m.InputTokens
		}
		if m.OutputTokens != nil {
			cur.OutputTokens += *m.OutputTokens
		}
		if m.CacheReadTokens != nil {
			cur.CacheReadTokens += *m.CacheReadTokens
		}
		if m.CacheCreationTokens != nil {
			cur.CacheCreationTokens += *m.CacheCreationTokens
		}
	}

	for i := range messages {
		m := messages[i]
		if strings.HasPrefix(m.Subtype, "injection:") {
			accumulate(&m)
			continue
		}
		if m.Role == "user" && m.Subtype == "" {
			flush()
			cur = &curatorRequestJSON{
				CuratorRequest: domain.CuratorRequest{
					ID:            strconv.Itoa(m.ID),
					ProjectID:     projectID,
					UserInput:     m.Content,
					CreatorUserID: m.UserID,
					CreatedAt:     m.CreatedAt,
				},
				Messages: []domain.MessageDTO{},
			}
			curQueued = m.Delivered != nil && !*m.Delivered
			curUserClaimID = m.ClaimID
			accumulate(&m)
			continue
		}
		if cur == nil {
			continue
		}
		if curClaimID == "" && m.ClaimID != "" {
			curClaimID = m.ClaimID
		}
		accumulate(&m)
		cur.Messages = append(cur.Messages, m.ToDTO())
	}
	flush()
	return out
}

// applyCuratorTurnStatus stamps one synthesized turn's status, times, and
// the claim's duration/turns telemetry. Cost and tokens are NOT read here —
// the caller already summed them from the turn's message rows.
func applyCuratorTurnStatus(turn *curatorRequestJSON, queued bool, claimID string, claimByID map[string]domain.Claim) {
	if queued {
		turn.Status = "queued"
		return
	}
	c, ok := claimByID[claimID]
	if claimID == "" || !ok {
		// A delivered turn with no attributed claim: nothing streamed and the
		// engagement left no trace — render it settled rather than inventing
		// a phantom in-flight state.
		turn.Status = "done"
		return
	}
	started := c.ClaimedAt
	turn.StartedAt = &started
	turn.FinishedAt = c.ReleasedAt
	turn.ErrorMsg = c.Error
	if c.DurationMs != nil {
		turn.DurationMs = *c.DurationMs
	}
	if c.NumTurns != nil {
		turn.NumTurns = *c.NumTurns
	}
	if c.ReleasedAt == nil {
		turn.Status = "running"
		return
	}
	switch c.Outcome {
	case "cancelled":
		turn.Status = "cancelled"
	case "failed":
		turn.Status = "failed"
	default:
		turn.Status = "done"
	}
}

// handleCuratorCancel terminates the active in-flight turn. Returns
// 404 if there's no queued or running turn for the requesting user on
// this project — the frontend uses that to clear stale "cancelling…"
// state when the agent finished between the user's click and the
// request landing.
func (ch *curatorHandler) handleCuratorCancel(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	cur := ch.runtime()
	if cur == nil {
		writeCuratorUnavailable(w)
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	projectID, ok := projectIDOr404(w, r)
	if !ok {
		return
	}
	var (
		project  *domain.Project
		inFlight *db.CuratorInFlightTurn
	)
	if err := ch.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		project, e = tx.Projects.Get(r.Context(), orgID, projectID)
		if e != nil || project == nil {
			return e
		}
		// Self-only under Postgres RLS: a user locates (and cancels)
		// their own in-flight turn, not a teammate's.
		inFlight, e = tx.Curator.InFlightTurn(r.Context(), orgID, projectID, userID)
		return e
	}); err != nil {
		internalError(w, "curator", err)
		return
	}
	if project == nil {
		notFound(w, "project")
		return
	}
	if inFlight == nil {
		notFound(w, "in-flight curator request")
		return
	}
	requestID := strconv.FormatInt(inFlight.MessageID, 10)

	// Two-step: tell the runtime to fire the cancel ctx (kills the CC
	// subprocess if one is running, and rings the cross-pod doorbell so a
	// homed executor's session does the same), then settle the turn at the
	// DB level as the backstop for a dropped doorbell.
	cur.Cancel(orgID, projectID)
	if inFlight.Running {
		// A running turn cancels by releasing its claim. Admin-pool System
		// door — claims have no app-pool write — with authorization already
		// established by the RLS-scoped lookup above. The session goroutine
		// writes the same release when it observes ctx.Err(); the
		// released_at IS NULL filter makes the second write a no-op, and
		// whichever writer flips broadcasts.
		flipped, err := ch.curatorStore.ReleaseActiveTurnSystem(r.Context(), orgID, inFlight.ConversationID, "cancelled", "user cancelled", 0, 0, 0)
		if err != nil {
			internalError(w, "curator", err)
			return
		}
		if flipped {
			ch.broadcastRequestUpdate(orgID, projectID, requestID, "cancelled")
		}
	} else {
		// A queued turn never entered context — cancelling it is deleting
		// the undelivered message row.
		var deleted bool
		if err := ch.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			var e error
			deleted, e = tx.Curator.DeleteQueuedTurn(r.Context(), orgID, inFlight.ConversationID, inFlight.MessageID)
			return e
		}); err != nil {
			internalError(w, "curator", err)
			return
		}
		if deleted {
			ch.broadcastRequestUpdate(orgID, projectID, requestID, "cancelled")
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// broadcastRequestUpdate mirrors the runtime's status broadcast for the
// handler-side cancel writes, keeping the wire event identical.
func (ch *curatorHandler) broadcastRequestUpdate(orgID, projectID, requestID, status string) {
	if ch.ws == nil {
		return
	}
	ch.ws.Broadcast(websocket.Event{
		Type:      "conversation_update",
		OrgID:     orgID,
		ProjectID: projectID,
		Data: map[string]string{
			"request_id": requestID,
			"status":     status,
		},
	})
}

// handleCuratorReset archives the requesting user's live curator
// conversation so their next message starts a brand-new conversation (and
// with it a fresh Claude Code session). Useful when the allowlist or the
// envelope template change — `--resume` binds those flags to the original
// session, so existing sessions don't pick up new permissions until reset.
// Also handy for nuking a confused conversation without deleting the whole
// project. Deliberately per-creator: a teammate's conversation is their own
// and survives.
//
// 409 with a clear hint if the user's turn is in flight — caller should
// cancel first. The DB op + the WS broadcast are decoupled because a
// failed broadcast (e.g. hub panicked) shouldn't roll back the archive.
//
// The broadcast is fired only when a conversation was actually archived
// (ArchiveLiveConversation returns the archived id, empty when there was
// nothing live to reset), it is scoped to the requesting user (curator
// conversations are per-creator, so a teammate's live chat must not clear),
// and it carries the archived conversation id so a client that has already
// begun a fresh conversation ignores the stale reset instead of wiping it.
func (ch *curatorHandler) handleCuratorReset(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	projectID, ok := projectIDOr404(w, r)
	if !ok {
		return
	}
	var project *domain.Project
	if err := ch.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		project, e = tx.Projects.Get(r.Context(), orgID, projectID)
		return e
	}); err != nil {
		internalError(w, "curator", err)
		return
	}
	if project == nil {
		notFound(w, "project")
		return
	}

	var archivedID string
	if err := ch.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		archivedID, e = tx.Curator.ArchiveLiveConversation(r.Context(), orgID, projectID, userID)
		return e
	}); err != nil {
		if errors.Is(err, db.ErrCuratorInFlight) {
			conflict(w, "in-flight curator request — cancel it before resetting")
			return
		}
		internalError(w, "curator", err)
		return
	}

	if ch.ws != nil && archivedID != "" {
		ch.ws.Broadcast(websocket.Event{
			Type:           "conversation_reset",
			OrgID:          orgID,
			UserID:         userID,
			ProjectID:      projectID,
			ConversationID: archivedID,
			Data:           map[string]string{"conversation_id": archivedID},
		})
	}
	w.WriteHeader(http.StatusNoContent)
}
