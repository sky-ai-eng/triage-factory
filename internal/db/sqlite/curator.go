package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// curatorStore is the SQLite impl of db.CuratorStore. SQL bodies are
// lifted verbatim from internal/db/curator.go +
// internal/db/curator_pending_context.go (which retain their
// *sql.DB-only signatures for the handler-side cancel/list/cleanup
// paths). The only behavioral changes are
// the orgID assertion at each entry and the ctx-aware database/sql
// methods so the per-turn SyntheticClaimsWithTx wrap binds the
// store to the in-flight tx.
//
// orgID + creatorUserID parameters are bound for signature parity
// with the Postgres impl. SQLite has no auth concept, but the
// columns exist (defaulted to LocalDefaultOrgID / LocalDefaultUserID
// at the schema level) so we bind the values on Create paths so a
// future test can switch a SQLite install into a multi-user shape
// without ripping the wiring out.
type curatorStore struct{ q queryer }

func newCuratorStore(q queryer) db.CuratorStore { return &curatorStore{q: q} }

var _ db.CuratorStore = (*curatorStore)(nil)

func (s *curatorStore) CreateRequest(ctx context.Context, orgID, projectID, creatorUserID, homeInstanceID, userInput string) (string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	id := uuid.New().String()
	// team_id is snapshotted from the project at creation (point-in-time, like
	// runs.team_id) so curator spend rolls into the project's team; the llm_spend
	// view reads the column directly. Any future curator INSERT MUST keep this
	// (SELECT team_id FROM projects WHERE id = ?) subquery — projectID is bound
	// twice for the correlated lookup (TFAC-476). home_instance_id is NULL in
	// local mode (empty homeInstanceID → NULLIF): N=1 never forwards or homes.
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO curator_requests (id, project_id, team_id, status, user_input, created_at, creator_user_id, home_instance_id)
		VALUES (?, ?, (SELECT team_id FROM projects WHERE id = ?), 'queued', ?, ?, ?, NULLIF(?, ''))
	`, id, projectID, projectID, userInput, time.Now().UTC(), creatorUserID, homeInstanceID)
	if err != nil {
		return "", err
	}
	return id, nil
}

// ListQueuedRequestsForHomeSystem is inert in SQLite (N=1 never homes to a
// remote executor), but implemented for interface + conformance symmetry.
func (s *curatorStore) ListQueuedRequestsForHomeSystem(ctx context.Context, homeInstanceID string) ([]domain.HomedCuratorRequest, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, org_id, project_id, creator_user_id
		FROM curator_requests
		WHERE home_instance_id = ? AND status = 'queued'
		ORDER BY created_at ASC, id ASC
	`, homeInstanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.HomedCuratorRequest
	for rows.Next() {
		var r domain.HomedCuratorRequest
		if err := rows.Scan(&r.ID, &r.OrgID, &r.ProjectID, &r.CreatorUserID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CancelStrandedRequestsForHomeSystem is inert in SQLite (N=1 uses the global
// CancelOrphanedNonTerminalRequests boot sweep), but implemented for interface
// symmetry. Rolls the denormalized token columns up from the curator_messages
// SUM, same as every other terminal write (CompleteRequest /
// MarkRequestCancelledIfActive / CancelOrphanedNonTerminalRequests): a 'running'
// turn stranded on a dead home may have streamed (and paid for) messages before
// the home died, so cancelling it must reflect that usage rather than strand
// llm_spend at 0 (TFAC-473). Correlated on curator_requests.id (bulk update).
func (s *curatorStore) CancelStrandedRequestsForHomeSystem(ctx context.Context, homeInstanceID, errMsg string) (int, error) {
	res, err := s.q.ExecContext(ctx, `
		UPDATE curator_requests
		SET status = 'cancelled',
		    error_msg = COALESCE(error_msg, ?),
		    finished_at = COALESCE(finished_at, ?),
		    input_tokens          = (SELECT COALESCE(SUM(input_tokens), 0)          FROM curator_messages WHERE request_id = curator_requests.id),
		    output_tokens         = (SELECT COALESCE(SUM(output_tokens), 0)         FROM curator_messages WHERE request_id = curator_requests.id),
		    cache_read_tokens     = (SELECT COALESCE(SUM(cache_read_tokens), 0)     FROM curator_messages WHERE request_id = curator_requests.id),
		    cache_creation_tokens = (SELECT COALESCE(SUM(cache_creation_tokens), 0) FROM curator_messages WHERE request_id = curator_requests.id)
		WHERE home_instance_id = ? AND status IN ('queued', 'running')
	`, errMsg, time.Now().UTC(), homeInstanceID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

func (s *curatorStore) GetRequest(ctx context.Context, orgID, id string) (*domain.CuratorRequest, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row := s.q.QueryRowContext(ctx, `
		SELECT id, project_id, status, user_input, error_msg,
		       cost_usd, duration_ms, num_turns,
		       input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
		       started_at, finished_at, created_at, creator_user_id
		FROM curator_requests WHERE id = ?
	`, id)
	return scanCuratorRequestWithUser(row)
}

func (s *curatorStore) MarkRequestRunning(ctx context.Context, orgID, id string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	res, err := s.q.ExecContext(ctx, `
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

func (s *curatorStore) CompleteRequest(ctx context.Context, orgID, id, status, errMsg string, costUSD float64, durationMs, numTurns int) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	// Token columns are SET from the absolute curator_messages SUM (the
	// streaming sink wrote every message row before this terminal write) —
	// the same roll-up runs uses over run_messages. TFAC-473.
	res, err := s.q.ExecContext(ctx, `
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

func (s *curatorStore) MarkRequestCancelledIfActive(ctx context.Context, orgID, id, errMsg string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	// Refresh the denormalized token columns from the curator_messages SUM,
	// same as CompleteRequest — a request cancelled mid-turn still streamed
	// (and paid for) messages, so the cache must reflect them rather than
	// strand at 0 (TFAC-473).
	res, err := s.q.ExecContext(ctx, `
		UPDATE curator_requests
		SET status = 'cancelled', error_msg = ?, finished_at = ?,
		    input_tokens          = (SELECT COALESCE(SUM(input_tokens), 0)          FROM curator_messages WHERE request_id = ?),
		    output_tokens         = (SELECT COALESCE(SUM(output_tokens), 0)         FROM curator_messages WHERE request_id = ?),
		    cache_read_tokens     = (SELECT COALESCE(SUM(cache_read_tokens), 0)     FROM curator_messages WHERE request_id = ?),
		    cache_creation_tokens = (SELECT COALESCE(SUM(cache_creation_tokens), 0) FROM curator_messages WHERE request_id = ?)
		WHERE id = ? AND status NOT IN ('done', 'cancelled', 'failed')
	`, nullIfEmpty(errMsg), time.Now().UTC(), id, id, id, id, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// MarkRequestCancelledIfActiveSystem and CompleteRequestSystem are the
// admin-pool variants the curator's system-cancel paths call. SQLite is
// single-tenant with no RLS, so there is no separate admin connection —
// they delegate to the claims-equivalent base methods. TFAC-64.
func (s *curatorStore) MarkRequestCancelledIfActiveSystem(ctx context.Context, orgID, id, errMsg string) (bool, error) {
	return s.MarkRequestCancelledIfActive(ctx, orgID, id, errMsg)
}

func (s *curatorStore) CompleteRequestSystem(ctx context.Context, orgID, id, status, errMsg string, costUSD float64, durationMs, numTurns int) (bool, error) {
	return s.CompleteRequest(ctx, orgID, id, status, errMsg, costUSD, durationMs, numTurns)
}

// QueuedRequestsForProjectSystem lists queued rows for the
// project-delete drain. orgID is asserted local; the query keys on
// project_id (single tenant). TFAC-64.
func (s *curatorStore) QueuedRequestsForProjectSystem(ctx context.Context, orgID, projectID string) ([]domain.CuratorRequest, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, project_id, status, user_input, error_msg,
		       cost_usd, duration_ms, num_turns,
		       input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
		       started_at, finished_at, created_at, creator_user_id
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
		req, err := scanCuratorRequestWithUser(rows)
		if err != nil {
			return nil, err
		}
		if req != nil {
			out = append(out, *req)
		}
	}
	return out, rows.Err()
}

func (s *curatorStore) InsertMessage(ctx context.Context, orgID string, msg *domain.CuratorMessage) (int64, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return 0, err
	}
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
	result, err := s.q.ExecContext(ctx, `
		INSERT INTO curator_messages (request_id, role, content, subtype, tool_calls, tool_call_id, is_error, metadata, model, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		msg.RequestID, msg.Role, msg.Content, msg.Subtype,
		toolCallsJSON, nullStrSqlite(msg.ToolCallID), msg.IsError, metadataJSON,
		nullStrSqlite(msg.Model), nullIntSqlite(msg.InputTokens), nullIntSqlite(msg.OutputTokens),
		nullIntSqlite(msg.CacheReadTokens), nullIntSqlite(msg.CacheCreationTokens),
		msg.CreatedAt,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (s *curatorStore) DeleteMessagesBySubtype(ctx context.Context, orgID, requestID, subtype string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		DELETE FROM curator_messages
		 WHERE request_id = ? AND subtype = ?
	`, requestID, subtype)
	return err
}

// ConsumePendingContext claims pending rows and reads project state
// atomically. When invoked against an outer tx (the curator goroutine's
// SyntheticClaimsWithTx wrap), the outer tx is the locking boundary —
// inTx detects the *sql.Tx and runs fn directly. When invoked against
// *sql.DB (no caller today, but future non-goroutine paths may use
// it), inTx opens a short-lived tx itself so the locking-order
// invariant (UPDATE first → RESERVED lock) is preserved either way.
func (s *curatorStore) ConsumePendingContext(ctx context.Context, orgID, projectID, requestID string) (*domain.Project, []domain.CuratorPendingContext, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, nil, err
	}
	var (
		project *domain.Project
		out     []domain.CuratorPendingContext
	)
	err := inTx(ctx, s.q, func(tx queryer) error {
		now := time.Now().UTC()
		// FIRST statement: the UPDATE. Forces RESERVED lock
		// acquisition before any read, closing the consume-vs-PATCH
		// race documented on the package-level helper.
		if _, err := tx.ExecContext(ctx, `
			UPDATE curator_pending_context
			   SET consumed_at = ?, consumed_by_request_id = ?
			 WHERE project_id = ?
			   AND curator_session_id = (SELECT curator_session_id FROM projects WHERE id = ?)
			   AND consumed_at IS NULL
		`, now, requestID, projectID, projectID); err != nil {
			return fmt.Errorf("claim pending rows: %w", err)
		}

		p, err := scanCuratorProject(tx.QueryRowContext(ctx, `
			SELECT id, name, description, curator_session_id, pinned_repos, jira_project_key, linear_project_key, spec_authorship_blueprint_id, created_at, updated_at
			FROM projects WHERE id = ?
		`, projectID))
		if err != nil {
			return fmt.Errorf("read project: %w", err)
		}
		if p == nil {
			// Project vanished — UPDATE's subquery returned NULL so
			// it claimed nothing. Return cleanly; the caller surfaces
			// this as a request failure.
			out = []domain.CuratorPendingContext{}
			return nil
		}
		project = p

		rows, err := tx.QueryContext(ctx, `
			SELECT id, project_id, curator_session_id, change_type, baseline_value,
			       consumed_at, consumed_by_request_id, created_at
			  FROM curator_pending_context
			 WHERE consumed_by_request_id = ?
			 ORDER BY created_at ASC, id ASC
		`, requestID)
		if err != nil {
			return fmt.Errorf("read claimed rows: %w", err)
		}
		defer rows.Close()

		out = []domain.CuratorPendingContext{}
		for rows.Next() {
			row, err := scanPendingContextRow(rows)
			if err != nil {
				return err
			}
			out = append(out, row)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, nil, err
	}
	return project, out, nil
}

func (s *curatorStore) FinalizePendingContext(ctx context.Context, orgID, requestID string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		DELETE FROM curator_pending_context
		 WHERE consumed_by_request_id = ?
	`, requestID)
	return err
}

func (s *curatorStore) RevertPendingContext(ctx context.Context, orgID, requestID string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	return inTx(ctx, s.q, func(tx queryer) error {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM curator_pending_context
			 WHERE consumed_at IS NULL
			   AND (project_id, curator_session_id, change_type) IN (
			       SELECT project_id, curator_session_id, change_type
			         FROM curator_pending_context
			        WHERE consumed_by_request_id = ?
			   )
		`, requestID); err != nil {
			return fmt.Errorf("merge pending rows: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE curator_pending_context
			   SET consumed_at = NULL, consumed_by_request_id = NULL
			 WHERE consumed_by_request_id = ?
		`, requestID); err != nil {
			return fmt.Errorf("revert pending rows: %w", err)
		}
		return nil
	})
}

func (s *curatorStore) InsertPendingContext(ctx context.Context, orgID, projectID, sessionID, changeType, baselineJSON string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO curator_pending_context
			(project_id, curator_session_id, change_type, baseline_value)
		VALUES (?, ?, ?, ?)
		ON CONFLICT DO NOTHING
	`, projectID, sessionID, changeType, baselineJSON)
	return err
}

func (s *curatorStore) ListPendingContext(ctx context.Context, orgID, projectID string) ([]domain.CuratorPendingContext, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, project_id, curator_session_id, change_type, baseline_value,
		       consumed_at, consumed_by_request_id, created_at
		  FROM curator_pending_context
		 WHERE project_id = ?
		 ORDER BY created_at ASC, id ASC
	`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.CuratorPendingContext{}
	for rows.Next() {
		row, err := scanPendingContextRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *curatorStore) DeletePendingContextForSession(ctx context.Context, orgID, projectID, sessionID string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		DELETE FROM curator_pending_context
		 WHERE project_id = ? AND curator_session_id = ?
	`, projectID, sessionID)
	return err
}

func (s *curatorStore) CancelOrphanedNonTerminalRequests(ctx context.Context) (int, error) {
	// Roll up the token cache here too (correlated SUM per row) — a 'running'
	// request stranded by a crash may have streamed curator_messages, so the
	// boot sweep must reflect them rather than leave the columns at 0. Same
	// every-terminal-write invariant as the cancel paths; 'queued' rows simply
	// have no messages and sum to 0 (TFAC-473).
	res, err := s.q.ExecContext(ctx, `
		UPDATE curator_requests
		SET status = 'cancelled',
		    error_msg = COALESCE(error_msg, 'process restarted'),
		    finished_at = COALESCE(finished_at, ?),
		    input_tokens          = (SELECT COALESCE(SUM(input_tokens), 0)          FROM curator_messages WHERE request_id = curator_requests.id),
		    output_tokens         = (SELECT COALESCE(SUM(output_tokens), 0)         FROM curator_messages WHERE request_id = curator_requests.id),
		    cache_read_tokens     = (SELECT COALESCE(SUM(cache_read_tokens), 0)     FROM curator_messages WHERE request_id = curator_requests.id),
		    cache_creation_tokens = (SELECT COALESCE(SUM(cache_creation_tokens), 0) FROM curator_messages WHERE request_id = curator_requests.id)
		WHERE status IN ('queued', 'running')
	`, time.Now().UTC())
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

func (s *curatorStore) ListRequestsByProject(ctx context.Context, orgID, projectID string) ([]domain.CuratorRequest, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, project_id, status, user_input, error_msg,
		       cost_usd, duration_ms, num_turns,
		       input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
		       started_at, finished_at, created_at, creator_user_id
		FROM curator_requests
		WHERE project_id = ?
		ORDER BY created_at ASC, id ASC
	`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.CuratorRequest{}
	for rows.Next() {
		req, err := scanCuratorRequestWithUser(rows)
		if err != nil {
			return nil, err
		}
		if req != nil {
			out = append(out, *req)
		}
	}
	return out, rows.Err()
}

// ListMessagesByRequestIDs chunks the IN-list at 500 ids — SQLite's
// default SQLITE_LIMIT_VARIABLE_NUMBER is 999 on older builds, so 500
// stays comfortably inside; per-project chat counts are practically far
// below the chunk size.
func (s *curatorStore) ListMessagesByRequestIDs(ctx context.Context, orgID string, requestIDs []string) (map[string][]domain.CuratorMessage, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	out := make(map[string][]domain.CuratorMessage)
	if len(requestIDs) == 0 {
		return out, nil
	}
	const chunkSize = 500
	for start := 0; start < len(requestIDs); start += chunkSize {
		end := min(start+chunkSize, len(requestIDs))
		chunk := requestIDs[start:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, id := range chunk {
			placeholders[i] = "?"
			args[i] = id
		}
		rows, err := s.q.QueryContext(ctx, `
			SELECT `+sqliteCuratorMessageColumns+`
			FROM curator_messages
			WHERE request_id IN (`+strings.Join(placeholders, ",")+`)
			ORDER BY created_at ASC, id ASC
		`, args...)
		if err != nil {
			return nil, err
		}
		if err := func() error {
			defer rows.Close()
			for rows.Next() {
				m, err := scanCuratorMessageRowSqlite(rows)
				if err != nil {
					return err
				}
				out[m.RequestID] = append(out[m.RequestID], m)
			}
			return rows.Err()
		}(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *curatorStore) InFlightRequestForProject(ctx context.Context, orgID, projectID string) (*domain.CuratorRequest, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row := s.q.QueryRowContext(ctx, `
		SELECT id, project_id, status, user_input, error_msg,
		       cost_usd, duration_ms, num_turns,
		       input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
		       started_at, finished_at, created_at, creator_user_id
		FROM curator_requests
		WHERE project_id = ? AND status IN ('queued', 'running')
		ORDER BY (status = 'running') DESC, created_at ASC, id ASC
		LIMIT 1
	`, projectID)
	return scanCuratorRequestWithUser(row)
}

// ResetForProject runs the in-flight guard + the three deletes through
// inTx so the check-then-wipe is atomic whether the store is bound to
// an outer WithTx tx or a bare *sql.DB — a concurrent SendMessage
// cannot slip a new request into the gap.
func (s *curatorStore) ResetForProject(ctx context.Context, orgID, projectID string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	return inTx(ctx, s.q, func(tx queryer) error {
		var inflight int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM curator_requests
			 WHERE project_id = ? AND status IN ('queued', 'running')
		`, projectID).Scan(&inflight); err != nil {
			return fmt.Errorf("count inflight: %w", err)
		}
		if inflight > 0 {
			return db.ErrCuratorInFlight
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE projects SET curator_session_id = NULL, updated_at = ?
			 WHERE id = ?
		`, time.Now().UTC(), projectID); err != nil {
			return fmt.Errorf("clear session id: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM curator_pending_context WHERE project_id = ?
		`, projectID); err != nil {
			return fmt.Errorf("delete pending context: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM curator_requests WHERE project_id = ?
		`, projectID); err != nil {
			return fmt.Errorf("delete requests: %w", err)
		}
		return nil
	})
}

// ImportRequest preserves the bundle row's id/status/accounting/
// timestamps verbatim; team_id snapshots from the destination project
// (TFAC-476 — every curator INSERT keeps the correlated subquery).
func (s *curatorStore) ImportRequest(ctx context.Context, orgID string, req domain.CuratorRequest) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO curator_requests (
			id, project_id, team_id, status, user_input, error_msg,
			cost_usd, duration_ms, num_turns,
			input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
			started_at, finished_at, created_at, creator_user_id
		) VALUES (?, ?, (SELECT team_id FROM projects WHERE id = ?), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		req.ID, req.ProjectID, req.ProjectID,
		req.Status, req.UserInput, nullStrSqlite(req.ErrorMsg),
		req.CostUSD, req.DurationMs, req.NumTurns,
		req.InputTokens, req.OutputTokens, req.CacheReadTokens, req.CacheCreationTokens,
		nullTimeSqlite(req.StartedAt), nullTimeSqlite(req.FinishedAt), req.CreatedAt,
		req.CreatorUserID,
	)
	if err != nil {
		return fmt.Errorf("import curator_request %s: %w", req.ID, err)
	}
	return nil
}

// ImportPendingContext preserves consumed state + created_at, unlike
// InsertPendingContext (which queues a fresh unconsumed delta).
func (s *curatorStore) ImportPendingContext(ctx context.Context, orgID string, row domain.CuratorPendingContext) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	var consumedBy any
	if row.ConsumedByRequestID != "" {
		consumedBy = row.ConsumedByRequestID
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO curator_pending_context (
			project_id, curator_session_id, change_type, baseline_value,
			consumed_at, consumed_by_request_id, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)
	`,
		row.ProjectID, row.CuratorSessionID, row.ChangeType, row.BaselineValue,
		nullTimeSqlite(row.ConsumedAt), consumedBy, row.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("import pending context row: %w", err)
	}
	return nil
}

// sqliteCuratorMessageColumns + scanCuratorMessageRowSqlite mirror the
// package-level curatorMessageColumns/scanCuratorMessageRow pair (the
// legacy raw helpers) — duplicated locally because those are unexported
// in package db and this dialect impl consumes db only through its
// exported interfaces.
const sqliteCuratorMessageColumns = `
	id, request_id, role, content, subtype, tool_calls, tool_call_id, is_error, metadata,
	model, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, created_at
`

func scanCuratorMessageRowSqlite(rows *sql.Rows) (domain.CuratorMessage, error) {
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

func nullTimeSqlite(t *time.Time) sql.NullTime {
	if t == nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: *t, Valid: true}
}

// nullStrSqlite + nullIntSqlite mirror the package-level helpers used
// by the legacy curator package-level INSERTs. Duplicated locally to
// avoid an import cycle on the package-db helpers from inside a
// dialect impl that already depends on package db only via its
// exported interface.
func nullStrSqlite(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func nullIntSqlite(p *int) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*p), Valid: true}
}

// scanCuratorRequestWithUser reads a curator_requests row including
// creator_user_id. Returns (nil, nil) on ErrNoRows.
func scanCuratorRequestWithUser(row interface {
	Scan(dest ...any) error
}) (*domain.CuratorRequest, error) {
	var (
		req        domain.CuratorRequest
		errMsg     sql.NullString
		startedAt  sql.NullTime
		finishedAt sql.NullTime
		userID     string
	)
	err := row.Scan(
		&req.ID, &req.ProjectID, &req.Status, &req.UserInput, &errMsg,
		&req.CostUSD, &req.DurationMs, &req.NumTurns,
		&req.InputTokens, &req.OutputTokens, &req.CacheReadTokens, &req.CacheCreationTokens,
		&startedAt, &finishedAt, &req.CreatedAt, &userID,
	)
	if errors.Is(err, sql.ErrNoRows) {
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
	req.CreatorUserID = userID
	return &req, nil
}

// scanCuratorProject reads a project row used by ConsumePendingContext.
// Duplicates scanSqliteProjectRow inline to avoid coupling the curator
// impl to the project-store internals (both stores hit the same
// columns, but the lifetimes are independent — projects.go may add
// columns without curator following).
func scanCuratorProject(row interface {
	Scan(dest ...any) error
}) (*domain.Project, error) {
	var (
		p               domain.Project
		sessionID       sql.NullString
		jiraKey         sql.NullString
		linearKey       sql.NullString
		specBlueprintID sql.NullString
		pinnedJSON      string
		createdAt       time.Time
		updatedAt       time.Time
	)
	err := row.Scan(
		&p.ID, &p.Name, &p.Description, &sessionID, &pinnedJSON,
		&jiraKey, &linearKey, &specBlueprintID,
		&createdAt, &updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.CuratorSessionID = sessionID.String
	p.JiraProjectKey = jiraKey.String
	p.LinearProjectKey = linearKey.String
	p.SpecAuthorshipBlueprintID = specBlueprintID.String
	p.CreatedAt = createdAt
	p.UpdatedAt = updatedAt
	if pinnedJSON == "" {
		p.PinnedRepos = []string{}
	} else if err := json.Unmarshal([]byte(pinnedJSON), &p.PinnedRepos); err != nil {
		return nil, fmt.Errorf("unmarshal pinned_repos: %w", err)
	}
	if p.PinnedRepos == nil {
		p.PinnedRepos = []string{}
	}
	return &p, nil
}

// scanPendingContextRow reads a curator_pending_context row. Mirrors
// the package-level scanPendingContext helper.
func scanPendingContextRow(scanner interface {
	Scan(dest ...any) error
}) (domain.CuratorPendingContext, error) {
	var (
		row        domain.CuratorPendingContext
		consumedAt sql.NullTime
		consumedBy sql.NullString
	)
	if err := scanner.Scan(
		&row.ID, &row.ProjectID, &row.CuratorSessionID, &row.ChangeType,
		&row.BaselineValue, &consumedAt, &consumedBy, &row.CreatedAt,
	); err != nil {
		return domain.CuratorPendingContext{}, err
	}
	if consumedAt.Valid {
		t := consumedAt.Time
		row.ConsumedAt = &t
	}
	row.ConsumedByRequestID = consumedBy.String
	return row, nil
}
