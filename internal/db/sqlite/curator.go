package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// curatorStore is the SQLite impl of db.CuratorStore. SQL bodies are
// lifted verbatim from internal/db/curator.go +
// internal/db/curator_pending_context.go (which retain their
// *sql.DB-only signatures for the handler-side cancel/list/cleanup
// paths still tracked by SKY-253). The only behavioral changes are
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

func (s *curatorStore) CreateRequest(ctx context.Context, orgID, projectID, creatorUserID, userInput string) (string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	id := uuid.New().String()
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO curator_requests (id, project_id, status, user_input, created_at, creator_user_id)
		VALUES (?, ?, 'queued', ?, ?, ?)
	`, id, projectID, userInput, time.Now().UTC(), creatorUserID)
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *curatorStore) GetRequest(ctx context.Context, orgID, id string) (*domain.CuratorRequest, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row := s.q.QueryRowContext(ctx, `
		SELECT id, project_id, status, user_input, error_msg,
		       cost_usd, duration_ms, num_turns,
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
	res, err := s.q.ExecContext(ctx, `
		UPDATE curator_requests
		SET status = ?, error_msg = ?, cost_usd = ?, duration_ms = ?, num_turns = ?, finished_at = ?
		WHERE id = ? AND status NOT IN ('done', 'cancelled', 'failed')
	`, status, nullIfEmpty(errMsg), costUSD, durationMs, numTurns, time.Now().UTC(), id)
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
	res, err := s.q.ExecContext(ctx, `
		UPDATE curator_requests
		SET status = 'cancelled', error_msg = ?, finished_at = ?
		WHERE id = ? AND status NOT IN ('done', 'cancelled', 'failed')
	`, nullIfEmpty(errMsg), time.Now().UTC(), id)
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
	res, err := s.q.ExecContext(ctx, `
		UPDATE curator_requests
		SET status = 'cancelled',
		    error_msg = COALESCE(error_msg, 'process restarted'),
		    finished_at = COALESCE(finished_at, ?)
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
