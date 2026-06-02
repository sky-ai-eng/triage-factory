package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// curatorStore is the Postgres impl of db.CuratorStore. App pool — the
// curator goroutine wraps each turn's writes in
// Tx.SyntheticClaimsWithTx with the requesting user's identity
// (curator_requests.creator_user_id read from the request row at
// dequeue time). RLS policies curator_requests_modify /
// curator_messages_modify / curator_pending_context_modify gate every
// row on (org_id = tf.current_org_id() AND creator_user_id =
// tf.current_user_id()), so a write outside synthetic claims would
// fail; a read of someone else's row would return zero rows.
//
// Holds two queryers. q is the app pool (or the in-flight *sql.Tx
// when composed from txStoresFromTx); admin is the admin pool used
// by CancelOrphanedNonTerminalRequests, which is a cross-tenant
// system-service sweep run at process startup before any JWT-claims
// context exists. Both args collapse to the same queryer for the
// NewForTx test door.
type curatorStore struct {
	q     queryer
	admin queryer
}

func newCuratorStore(q, admin queryer) db.CuratorStore {
	return &curatorStore{q: q, admin: admin}
}

var _ db.CuratorStore = (*curatorStore)(nil)

func (s *curatorStore) CreateRequest(ctx context.Context, orgID, projectID, creatorUserID, userInput string) (string, error) {
	var id string
	err := s.q.QueryRowContext(ctx, `
		INSERT INTO curator_requests
			(org_id, project_id, creator_user_id, status, user_input)
		VALUES ($1, $2, $3, 'queued', $4)
		RETURNING id::text
	`, orgID, projectID, creatorUserID, userInput).Scan(&id)
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *curatorStore) GetRequest(ctx context.Context, orgID, id string) (*domain.CuratorRequest, error) {
	row := s.q.QueryRowContext(ctx, `
		SELECT id::text, project_id::text, status, user_input, error_msg,
		       cost_usd, duration_ms, num_turns,
		       started_at, finished_at, created_at, creator_user_id::text
		FROM curator_requests
		WHERE org_id = $1 AND id = $2
	`, orgID, id)
	return scanPgCuratorRequest(row)
}

func (s *curatorStore) MarkRequestRunning(ctx context.Context, orgID, id string) error {
	res, err := s.q.ExecContext(ctx, `
		UPDATE curator_requests
		SET status = 'running', started_at = now()
		WHERE org_id = $1 AND id = $2 AND status = 'queued'
	`, orgID, id)
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
	var errBind any
	if errMsg != "" {
		errBind = errMsg
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE curator_requests
		SET status = $1, error_msg = $2, cost_usd = $3, duration_ms = $4, num_turns = $5, finished_at = now()
		WHERE org_id = $6 AND id = $7 AND status NOT IN ('done', 'cancelled', 'failed')
	`, status, errBind, costUSD, durationMs, numTurns, orgID, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *curatorStore) MarkRequestCancelledIfActive(ctx context.Context, orgID, id, errMsg string) (bool, error) {
	var errBind any
	if errMsg != "" {
		errBind = errMsg
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE curator_requests
		SET status = 'cancelled', error_msg = $1, finished_at = now()
		WHERE org_id = $2 AND id = $3 AND status NOT IN ('done', 'cancelled', 'failed')
	`, errBind, orgID, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *curatorStore) InsertMessage(ctx context.Context, orgID string, msg *domain.CuratorMessage) (int64, error) {
	var toolCallsJSON, metadataJSON any
	if len(msg.ToolCalls) > 0 {
		b, err := json.Marshal(msg.ToolCalls)
		if err != nil {
			return 0, fmt.Errorf("marshal tool_calls: %w", err)
		}
		toolCallsJSON = string(b)
	}
	if len(msg.Metadata) > 0 {
		b, err := json.Marshal(msg.Metadata)
		if err != nil {
			return 0, fmt.Errorf("marshal metadata: %w", err)
		}
		metadataJSON = string(b)
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now().UTC()
	}
	// creator_user_id resolves from tf.current_user_id() set by
	// SyntheticClaimsWithTx — same pattern every other app-pool
	// store INSERT uses (projects, prompts, event_handlers, ...).
	// The curator_messages_modify RLS WITH CHECK then gates on
	// creator_user_id = tf.current_user_id(), so caller-supplied
	// userID and policy-asserted current_user_id can't diverge.
	// curator_messages.creator_user_id is NOT NULL with no DEFAULT
	// in the Postgres baseline, so this binding is load-bearing —
	// omitting it lets the INSERT fail RLS before the NOT NULL
	// check fires, which is what the SKY-298 multi-user test
	// originally tripped on.
	var id int64
	err := s.q.QueryRowContext(ctx, `
		INSERT INTO curator_messages
			(org_id, creator_user_id, request_id, role, content, subtype, tool_calls, tool_call_id,
			 is_error, metadata, model, input_tokens, output_tokens,
			 cache_read_tokens, cache_creation_tokens, created_at)
		VALUES ($1, tf.current_user_id(), $2, $3, $4, $5, $6::jsonb, NULLIF($7, ''), $8,
		        $9::jsonb, NULLIF($10, ''), $11, $12, $13, $14, $15)
		RETURNING id
	`,
		orgID, msg.RequestID, msg.Role, msg.Content, msg.Subtype,
		toolCallsJSON, msg.ToolCallID, msg.IsError, metadataJSON,
		msg.Model, intPtrAny(msg.InputTokens), intPtrAny(msg.OutputTokens),
		intPtrAny(msg.CacheReadTokens), intPtrAny(msg.CacheCreationTokens),
		msg.CreatedAt,
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

func (s *curatorStore) DeleteMessagesBySubtype(ctx context.Context, orgID, requestID, subtype string) error {
	_, err := s.q.ExecContext(ctx, `
		DELETE FROM curator_messages
		 WHERE org_id = $1 AND request_id = $2 AND subtype = $3
	`, orgID, requestID, subtype)
	return err
}

// ConsumePendingContext claims pending rows and reads project state
// atomically. Composes against the outer SyntheticClaimsWithTx tx
// when invoked through TxStores, or opens a short tx via inTx when
// called against a *sql.DB. Postgres locking is row-level by default,
// so the UPDATE-first ordering that matters for SQLite (RESERVED-vs-
// SHARED lock) is moot here — the impl preserves the ordering for
// symmetry with the SQLite store so the two backends behave the same
// way under interleaved PATCHes.
func (s *curatorStore) ConsumePendingContext(ctx context.Context, orgID, projectID, requestID string) (*domain.Project, []domain.CuratorPendingContext, error) {
	var (
		project *domain.Project
		out     []domain.CuratorPendingContext
	)
	err := inTx(ctx, s.q, func(tx queryer) error {
		if _, err := tx.ExecContext(ctx, `
			UPDATE curator_pending_context
			   SET consumed_at = now(), consumed_by_request_id = $1
			 WHERE org_id = $2
			   AND project_id = $3
			   AND curator_session_id = (
			       SELECT curator_session_id FROM projects WHERE org_id = $2 AND id = $3
			   )
			   AND consumed_at IS NULL
		`, requestID, orgID, projectID); err != nil {
			return fmt.Errorf("claim pending rows: %w", err)
		}

		p, err := scanPgCuratorProject(tx.QueryRowContext(ctx, `
			SELECT id::text, name, description, curator_session_id,
			       pinned_repos, jira_project_key, linear_project_key,
			       spec_authorship_blueprint_id, created_at, updated_at
			FROM projects
			WHERE org_id = $1 AND id = $2
		`, orgID, projectID))
		if err != nil {
			return fmt.Errorf("read project: %w", err)
		}
		if p == nil {
			out = []domain.CuratorPendingContext{}
			return nil
		}
		project = p

		rows, err := tx.QueryContext(ctx, `
			SELECT id, project_id::text, curator_session_id, change_type, baseline_value,
			       consumed_at, consumed_by_request_id::text, created_at
			  FROM curator_pending_context
			 WHERE org_id = $1 AND consumed_by_request_id = $2
			 ORDER BY created_at ASC, id ASC
		`, orgID, requestID)
		if err != nil {
			return fmt.Errorf("read claimed rows: %w", err)
		}
		defer rows.Close()

		out = []domain.CuratorPendingContext{}
		for rows.Next() {
			row, err := scanPgPendingContext(rows)
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
	_, err := s.q.ExecContext(ctx, `
		DELETE FROM curator_pending_context
		 WHERE org_id = $1 AND consumed_by_request_id = $2
	`, orgID, requestID)
	return err
}

func (s *curatorStore) RevertPendingContext(ctx context.Context, orgID, requestID string) error {
	return inTx(ctx, s.q, func(tx queryer) error {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM curator_pending_context
			 WHERE org_id = $1
			   AND consumed_at IS NULL
			   AND (project_id, curator_session_id, change_type) IN (
			       SELECT project_id, curator_session_id, change_type
			         FROM curator_pending_context
			        WHERE org_id = $1 AND consumed_by_request_id = $2
			   )
		`, orgID, requestID); err != nil {
			return fmt.Errorf("merge pending rows: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE curator_pending_context
			   SET consumed_at = NULL, consumed_by_request_id = NULL
			 WHERE org_id = $1 AND consumed_by_request_id = $2
		`, orgID, requestID); err != nil {
			return fmt.Errorf("revert pending rows: %w", err)
		}
		return nil
	})
}

// InsertPendingContext queues a coalesced context-change delta.
// creator_user_id binds via tf.current_user_id() — the same pattern
// every other app-pool curator INSERT uses (curator_messages,
// curator_requests). The ON CONFLICT target names the partial-unique
// index columns explicitly with its predicate so Postgres matches the
// idx_curator_pending_context_one_pending_per_type index for the
// upsert resolution.
func (s *curatorStore) InsertPendingContext(ctx context.Context, orgID, projectID, sessionID, changeType, baselineJSON string) error {
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO curator_pending_context
			(org_id, creator_user_id, project_id, curator_session_id, change_type, baseline_value)
		VALUES ($1, tf.current_user_id(), $2, $3, $4, $5)
		ON CONFLICT (project_id, curator_session_id, change_type)
		WHERE consumed_at IS NULL
		DO NOTHING
	`, orgID, projectID, sessionID, changeType, baselineJSON)
	return err
}

func (s *curatorStore) ListPendingContext(ctx context.Context, orgID, projectID string) ([]domain.CuratorPendingContext, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, project_id::text, curator_session_id, change_type, baseline_value,
		       consumed_at, consumed_by_request_id::text, created_at
		  FROM curator_pending_context
		 WHERE org_id = $1 AND project_id = $2
		 ORDER BY created_at ASC, id ASC
	`, orgID, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.CuratorPendingContext{}
	for rows.Next() {
		row, err := scanPgPendingContext(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *curatorStore) DeletePendingContextForSession(ctx context.Context, orgID, projectID, sessionID string) error {
	_, err := s.q.ExecContext(ctx, `
		DELETE FROM curator_pending_context
		 WHERE org_id = $1 AND project_id = $2 AND curator_session_id = $3
	`, orgID, projectID, sessionID)
	return err
}

// CancelOrphanedNonTerminalRequests is the boot-time cross-tenant
// sweep. Routes through the admin pool because the caller is
// main.go's startup path with no JWT-claims context — RLS would
// otherwise refuse the UPDATE. org_id is intentionally omitted: the
// sweep affects every tenant whose chat was in-flight at the moment
// of restart, documented as intentional (single-pod multi-mode in
// v1; per-pod sharding is the only thing that would let us scope
// narrower, and it doesn't exist yet).
func (s *curatorStore) CancelOrphanedNonTerminalRequests(ctx context.Context) (int, error) {
	res, err := s.admin.ExecContext(ctx, `
		UPDATE curator_requests
		SET status = 'cancelled',
		    error_msg = COALESCE(error_msg, 'process restarted'),
		    finished_at = COALESCE(finished_at, now())
		WHERE status IN ('queued', 'running')
	`)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// intPtrAny maps an *int to a bind-compatible value (nil for NULL,
// int otherwise). Postgres-side variant of the package-db nullInt
// helper — kept local to avoid widening the package-db helpers'
// exported surface.
func intPtrAny(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func scanPgCuratorRequest(row interface {
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

func scanPgCuratorProject(row interface {
	Scan(dest ...any) error
}) (*domain.Project, error) {
	var (
		p               domain.Project
		sessionID       sql.NullString
		jiraKey         sql.NullString
		linearKey       sql.NullString
		specBlueprintID sql.NullString
		pinnedJSON      []byte
	)
	err := row.Scan(
		&p.ID, &p.Name, &p.Description, &sessionID, &pinnedJSON,
		&jiraKey, &linearKey, &specBlueprintID,
		&p.CreatedAt, &p.UpdatedAt,
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
	if len(pinnedJSON) == 0 {
		p.PinnedRepos = []string{}
	} else if err := json.Unmarshal(pinnedJSON, &p.PinnedRepos); err != nil {
		return nil, fmt.Errorf("unmarshal pinned_repos: %w", err)
	}
	if p.PinnedRepos == nil {
		p.PinnedRepos = []string{}
	}
	return &p, nil
}

func scanPgPendingContext(scanner interface {
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
