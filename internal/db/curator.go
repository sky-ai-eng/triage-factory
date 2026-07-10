package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ErrCuratorInFlight is returned by CuratorStore.ResetForProject when a
// queued or running request exists at the moment of the reset call.
// The HTTP handler maps this to 409 so the client can prompt the
// user to cancel first.
var ErrCuratorInFlight = errors.New("curator request in flight")

// CreateCuratorRequest inserts a queued request and returns its id.
// The HTTP handler returns 202 + this id immediately; the per-project
// goroutine picks the row up and flips status to running on the next
// tick. uuid generated server-side; callers do not supply ids.
func CreateCuratorRequest(database *sql.DB, projectID, userInput string) (string, error) {
	id := uuid.New().String()
	// team_id is snapshotted from the project at creation (point-in-time, like
	// runs.team_id) so curator spend rolls into the project's team; the llm_spend
	// view reads the column directly. Any future curator INSERT MUST keep this
	// (SELECT team_id FROM projects WHERE id = ?) subquery — projectID is bound
	// twice for the correlated lookup (TFAC-476).
	_, err := database.Exec(`
		INSERT INTO curator_requests (id, project_id, team_id, status, user_input, created_at)
		VALUES (?, ?, (SELECT team_id FROM projects WHERE id = ?), 'queued', ?, ?)
	`, id, projectID, projectID, userInput, time.Now().UTC())
	if err != nil {
		return "", err
	}
	return id, nil
}

// MarkCuratorRequestRunning flips a queued row to running and stamps
// started_at. Returns sql.ErrNoRows if the row is not currently queued
// (for example, it was already claimed or otherwise transitioned).
// This includes the cancel-vs-pickup race: a user fired DELETE while
// the row was queued, the goroutine sees a non-queued row when it
// dequeues, and skips it.
func MarkCuratorRequestRunning(database *sql.DB, id string) error {
	res, err := database.Exec(`
		UPDATE curator_requests
		SET status = 'running', started_at = ?
		WHERE id = ? AND status = 'queued'
	`, time.Now().UTC(), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// CompleteCuratorRequest writes a terminal status and accounting,
// but ONLY if the row is still non-terminal. Status is one of
// done | cancelled | failed. Caller passes 0 for any field that
// wasn't observed (e.g., a failure with no result event).
//
// The four token columns are SET from the curator_messages SUM in the
// same UPDATE — the streaming sink writes every per-message row before
// this terminal write, so the SUM is the turn's full token total.
// Mirrors the runs roll-up over run_messages; recovers historical
// curator spend for free since the per-message data is already on disk
// (TFAC-473).
//
// Returns true if the flip happened. The status filter is the
// single source of truth for "terminal state is final": the
// goroutine that actually ran agentproc and the cancel handler
// can race to write the row, and either order produces the same
// outcome — the first writer wins, the second is a no-op. Without
// this filter, a user-cancel that landed during agentproc.Run
// could be silently overwritten by the goroutine's natural
// completion write seconds later.
//
// Per-project goroutine is the sole caller in normal flow; the
// guard exists for the rare cancel-vs-completion interleaving.
func CompleteCuratorRequest(database *sql.DB, id, status, errMsg string, costUSD float64, durationMs, numTurns int) (bool, error) {
	res, err := database.Exec(`
		UPDATE curator_requests
		SET status = ?, error_msg = ?, cost_usd = ?, duration_ms = ?, num_turns = ?,
		    input_tokens          = (SELECT COALESCE(SUM(input_tokens), 0)          FROM curator_messages WHERE request_id = ?),
		    output_tokens         = (SELECT COALESCE(SUM(output_tokens), 0)         FROM curator_messages WHERE request_id = ?),
		    cache_read_tokens     = (SELECT COALESCE(SUM(cache_read_tokens), 0)     FROM curator_messages WHERE request_id = ?),
		    cache_creation_tokens = (SELECT COALESCE(SUM(cache_creation_tokens), 0) FROM curator_messages WHERE request_id = ?),
		    finished_at = ?
		WHERE id = ? AND status NOT IN ('done', 'cancelled', 'failed')
	`, status, nullIfEmpty(errMsg), costUSD, durationMs, numTurns,
		id, id, id, id, time.Now().UTC(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// GetCuratorRequest reads a single row, or returns (nil, nil) if not
// found. The handler returns 404 on the nil case.
func GetCuratorRequest(database *sql.DB, id string) (*domain.CuratorRequest, error) {
	row := database.QueryRow(`
		SELECT id, project_id, status, user_input, error_msg,
		       cost_usd, duration_ms, num_turns,
		       input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
		       started_at, finished_at, created_at
		FROM curator_requests WHERE id = ?
	`, id)
	return scanCuratorRequest(row)
}

// QueuedCuratorRequestsForProject returns queued rows in FIFO order.
// Used by Curator.CancelProject as a defensive sweep so a project-
// delete that races a never-picked-up queued row still flips that
// row to a terminal state before the project FK cascade fires.
// Cross-process recovery is out of scope: process-restart cancels
// every non-terminal row at startup via
// CuratorStore.CancelOrphanedNonTerminalRequests, so by the time
// anything calls this helper, only rows enqueued during the current
// process lifetime can be observed.
func QueuedCuratorRequestsForProject(database *sql.DB, projectID string) ([]domain.CuratorRequest, error) {
	rows, err := database.Query(`
		SELECT id, project_id, status, user_input, error_msg,
		       cost_usd, duration_ms, num_turns,
		       input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
		       started_at, finished_at, created_at
		FROM curator_requests
		WHERE project_id = ? AND status = 'queued'
		ORDER BY created_at ASC, id ASC
	`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.CuratorRequest{}
	for rows.Next() {
		req, err := scanCuratorRequest(rows)
		if err != nil {
			return nil, err
		}
		if req != nil {
			out = append(out, *req)
		}
	}
	return out, rows.Err()
}

// SetProjectCuratorSessionID persists the captured Claude Code
// session_id on the project row. The first message kicks off a fresh
// CC session and captures the id; subsequent messages can resume
// against the persisted session id. This helper always performs the
// UPDATE by project id; callers may choose when to invoke it.
func SetProjectCuratorSessionID(database *sql.DB, projectID, sessionID string) error {
	_, err := database.Exec(`
		UPDATE projects SET curator_session_id = ?, updated_at = ?
		WHERE id = ?
	`, sessionID, time.Now().UTC(), projectID)
	return err
}

// InsertCuratorMessage writes one stream-output row and returns its id.
// Schema mirrors run_messages so the same agent-message accumulator
// (agentproc.StreamState) emits *domain.AgentMessage values that we
// translate to the curator_messages shape via the request_id.
func InsertCuratorMessage(database *sql.DB, msg *domain.CuratorMessage) (int64, error) {
	var toolCallsJSON, metadataJSON sql.NullString
	if len(msg.ToolCalls) > 0 {
		b, err := json.Marshal(msg.ToolCalls)
		if err != nil {
			return 0, fmt.Errorf("marshal tool_calls: %w", err)
		}
		toolCallsJSON = sql.NullString{String: string(b), Valid: true}
	}
	if len(msg.Metadata) > 0 {
		b, err := json.Marshal(msg.Metadata)
		if err != nil {
			return 0, fmt.Errorf("marshal metadata: %w", err)
		}
		metadataJSON = sql.NullString{String: string(b), Valid: true}
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now().UTC()
	}
	result, err := database.Exec(`
		INSERT INTO curator_messages (request_id, role, content, subtype, tool_calls, tool_call_id, is_error, metadata, model, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		msg.RequestID, msg.Role, msg.Content, msg.Subtype,
		toolCallsJSON, nullStr(msg.ToolCallID), msg.IsError, metadataJSON,
		nullStr(msg.Model), nullInt(msg.InputTokens), nullInt(msg.OutputTokens),
		nullInt(msg.CacheReadTokens), nullInt(msg.CacheCreationTokens),
		msg.CreatedAt,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// curatorMessageColumns is the SELECT list shared by every helper
// that reads from curator_messages, so scanCuratorMessageRow stays
// tied to a single column ordering.
const curatorMessageColumns = `
	id, request_id, role, content, subtype, tool_calls, tool_call_id, is_error, metadata,
	model, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, created_at
`

// scanCuratorMessageRow reads one curator_messages row from a Rows
// cursor — shared between the per-request and batched helpers so the
// nullable-column / JSON-decoding plumbing lives in one place.
func scanCuratorMessageRow(rows *sql.Rows) (domain.CuratorMessage, error) {
	var (
		m             domain.CuratorMessage
		toolCallsJSON sql.NullString
		metadataJSON  sql.NullString
		toolCallID    sql.NullString
		model         sql.NullString
		inputTokens   sql.NullInt64
		outputTokens  sql.NullInt64
		cacheRead     sql.NullInt64
		cacheCreation sql.NullInt64
	)
	if err := rows.Scan(
		&m.ID, &m.RequestID, &m.Role, &m.Content, &m.Subtype,
		&toolCallsJSON, &toolCallID, &m.IsError, &metadataJSON,
		&model, &inputTokens, &outputTokens, &cacheRead, &cacheCreation,
		&m.CreatedAt,
	); err != nil {
		return domain.CuratorMessage{}, err
	}
	if toolCallsJSON.Valid {
		if err := json.Unmarshal([]byte(toolCallsJSON.String), &m.ToolCalls); err != nil {
			return domain.CuratorMessage{}, fmt.Errorf("unmarshal tool_calls: %w", err)
		}
	}
	if metadataJSON.Valid {
		if err := json.Unmarshal([]byte(metadataJSON.String), &m.Metadata); err != nil {
			return domain.CuratorMessage{}, fmt.Errorf("unmarshal metadata: %w", err)
		}
	}
	m.ToolCallID = toolCallID.String
	m.Model = model.String
	if inputTokens.Valid {
		v := int(inputTokens.Int64)
		m.InputTokens = &v
	}
	if outputTokens.Valid {
		v := int(outputTokens.Int64)
		m.OutputTokens = &v
	}
	if cacheRead.Valid {
		v := int(cacheRead.Int64)
		m.CacheReadTokens = &v
	}
	if cacheCreation.Valid {
		v := int(cacheCreation.Int64)
		m.CacheCreationTokens = &v
	}
	return m, nil
}

// DeleteCuratorMessagesBySubtype removes every curator_messages row
// for a request with the given subtype. Used by the curator runtime
// to drop a `context_change` audit row after a revert: the chat
// history must not show a "context noted" entry for a turn that
// never delivered the deltas.
//
// Idempotent — zero-row deletes are fine. Returns no count; the
// caller doesn't currently care.
func DeleteCuratorMessagesBySubtype(database *sql.DB, requestID, subtype string) error {
	_, err := database.Exec(`
		DELETE FROM curator_messages
		 WHERE request_id = ? AND subtype = ?
	`, requestID, subtype)
	return err
}

// ListCuratorMessagesByRequest returns the agent-side stream rows for
// a request in chronological order. Used by the websocket replay path
// and tests; the GET history handler uses the batched
// ListCuratorMessagesByRequestIDs to avoid an N+1 over the request
// list.
func ListCuratorMessagesByRequest(database *sql.DB, requestID string) ([]domain.CuratorMessage, error) {
	rows, err := database.Query(`
		SELECT `+curatorMessageColumns+`
		FROM curator_messages
		WHERE request_id = ?
		ORDER BY created_at ASC, id ASC
	`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.CuratorMessage{}
	for rows.Next() {
		m, err := scanCuratorMessageRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func scanCuratorRequest(row rowScanner) (*domain.CuratorRequest, error) {
	var (
		req        domain.CuratorRequest
		errMsg     sql.NullString
		startedAt  sql.NullTime
		finishedAt sql.NullTime
	)
	err := row.Scan(
		&req.ID, &req.ProjectID, &req.Status, &req.UserInput, &errMsg,
		&req.CostUSD, &req.DurationMs, &req.NumTurns,
		&req.InputTokens, &req.OutputTokens, &req.CacheReadTokens, &req.CacheCreationTokens,
		&startedAt, &finishedAt, &req.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	req.ErrorMsg = errMsg.String
	if startedAt.Valid {
		t := startedAt.Time
		req.StartedAt = &t
	}
	if finishedAt.Valid {
		t := finishedAt.Time
		req.FinishedAt = &t
	}
	return &req, nil
}
