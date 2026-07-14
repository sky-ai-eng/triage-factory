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

func (s *curatorStore) CreateRequest(ctx context.Context, orgID, projectID, creatorUserID, homeInstanceID, userInput string) (string, error) {
	var id string
	// team_id is snapshotted from the project at creation (point-in-time, like
	// runs.team_id) so curator spend rolls into the project's team; the
	// security_invoker llm_spend view reads the column directly. Any future
	// curator INSERT MUST keep this (SELECT team_id FROM projects WHERE id = ...)
	// subquery (TFAC-476). $2 (project_id) is reused for the correlated lookup.
	// home_instance_id is the resolved curator home (spec §6.3); NULLIF maps the
	// local / role=all empty string to NULL.
	err := s.q.QueryRowContext(ctx, `
		INSERT INTO curator_requests
			(org_id, project_id, team_id, creator_user_id, status, user_input, home_instance_id)
		VALUES ($1, $2, (SELECT team_id FROM projects WHERE id = $2), $3, 'queued', $4, NULLIF($5, ''))
		RETURNING id::text
	`, orgID, projectID, creatorUserID, userInput, homeInstanceID).Scan(&id)
	if err != nil {
		return "", err
	}
	return id, nil
}

// ListQueuedRequestsForHomeSystem scans the queued turns homed to this executor
// (curator homing, spec §6.3) via the admin pool — a cross-user, cross-org
// system read, home_instance_id bound by argument. Backed by
// idx_curator_requests_homed_queued.
func (s *curatorStore) ListQueuedRequestsForHomeSystem(ctx context.Context, homeInstanceID string) ([]domain.HomedCuratorRequest, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT id::text, org_id::text, project_id::text, creator_user_id::text
		FROM curator_requests
		WHERE home_instance_id = $1 AND status = 'queued'
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

// CancelStrandedRequestsForHomeSystem flips this executor's stranded queued/
// running turns to cancelled (curator homing, spec §6.3) via the admin pool.
// Plain status flip, no token roll-up — keeps the tf_system grant a bare UPDATE
// and a cancelled-stranded turn's accounting need not be refreshed.
func (s *curatorStore) CancelStrandedRequestsForHomeSystem(ctx context.Context, homeInstanceID, errMsg string) (int, error) {
	res, err := s.admin.ExecContext(ctx, `
		UPDATE curator_requests
		SET status = 'cancelled',
		    error_msg = COALESCE(error_msg, $2),
		    finished_at = COALESCE(finished_at, now())
		WHERE home_instance_id = $1 AND status IN ('queued', 'running')
	`, homeInstanceID, errMsg)
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
	row := s.q.QueryRowContext(ctx, `
		SELECT id::text, project_id::text, status, user_input, error_msg,
		       cost_usd, duration_ms, num_turns,
		       input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
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
	return completeCuratorRequest(ctx, s.q, orgID, id, status, errMsg, costUSD, durationMs, numTurns)
}

// CompleteRequestSystem routes the terminal write through the admin
// pool for the SendMessage queue-full fallback (no JWT-claims context).
// See db.CuratorStore.CompleteRequestSystem.
func (s *curatorStore) CompleteRequestSystem(ctx context.Context, orgID, id, status, errMsg string, costUSD float64, durationMs, numTurns int) (bool, error) {
	return completeCuratorRequest(ctx, s.admin, orgID, id, status, errMsg, costUSD, durationMs, numTurns)
}

func completeCuratorRequest(ctx context.Context, q queryer, orgID, id, status, errMsg string, costUSD float64, durationMs, numTurns int) (bool, error) {
	var errBind any
	if errMsg != "" {
		errBind = errMsg
	}
	// Token columns are SET from the absolute curator_messages SUM (the
	// streaming sink wrote every message row before this terminal write) —
	// the same roll-up runs uses over run_messages. The subqueries reuse
	// the org/id binds ($6/$7); org_id scopes curator_messages for
	// defense-in-depth (and so the admin-pool BYPASSRLS path stays
	// tenant-correct). TFAC-473.
	res, err := q.ExecContext(ctx, `
		UPDATE curator_requests
		SET status = $1, error_msg = $2, cost_usd = $3, duration_ms = $4, num_turns = $5,
		    input_tokens          = (SELECT COALESCE(SUM(input_tokens), 0)          FROM curator_messages WHERE org_id = $6 AND request_id = $7),
		    output_tokens         = (SELECT COALESCE(SUM(output_tokens), 0)         FROM curator_messages WHERE org_id = $6 AND request_id = $7),
		    cache_read_tokens     = (SELECT COALESCE(SUM(cache_read_tokens), 0)     FROM curator_messages WHERE org_id = $6 AND request_id = $7),
		    cache_creation_tokens = (SELECT COALESCE(SUM(cache_creation_tokens), 0) FROM curator_messages WHERE org_id = $6 AND request_id = $7),
		    finished_at = now()
		WHERE org_id = $6 AND id = $7 AND status NOT IN ('done', 'cancelled', 'failed')
	`, status, errBind, costUSD, durationMs, numTurns, orgID, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *curatorStore) MarkRequestCancelledIfActive(ctx context.Context, orgID, id, errMsg string) (bool, error) {
	return markCuratorRequestCancelledIfActive(ctx, s.q, orgID, id, errMsg)
}

// MarkRequestCancelledIfActiveSystem routes the cancel write through the
// admin pool for the curator's system-driven cancel paths (shutdown,
// project-delete drain, SendMessage shutdown fallback). See
// db.CuratorStore.MarkRequestCancelledIfActiveSystem.
func (s *curatorStore) MarkRequestCancelledIfActiveSystem(ctx context.Context, orgID, id, errMsg string) (bool, error) {
	return markCuratorRequestCancelledIfActive(ctx, s.admin, orgID, id, errMsg)
}

func markCuratorRequestCancelledIfActive(ctx context.Context, q queryer, orgID, id, errMsg string) (bool, error) {
	var errBind any
	if errMsg != "" {
		errBind = errMsg
	}
	// Refresh the denormalized token columns from the curator_messages SUM,
	// same as completeCuratorRequest — a request cancelled mid-turn still
	// streamed (and paid for) messages, so the cache must reflect them rather
	// than strand at 0. The subqueries reuse the org/id binds ($2/$3); org_id
	// scopes curator_messages for defense-in-depth (and so the admin-pool
	// BYPASSRLS cancel paths stay tenant-correct). TFAC-473.
	res, err := q.ExecContext(ctx, `
		UPDATE curator_requests
		SET status = 'cancelled', error_msg = $1, finished_at = now(),
		    input_tokens          = (SELECT COALESCE(SUM(input_tokens), 0)          FROM curator_messages WHERE org_id = $2 AND request_id = $3),
		    output_tokens         = (SELECT COALESCE(SUM(output_tokens), 0)         FROM curator_messages WHERE org_id = $2 AND request_id = $3),
		    cache_read_tokens     = (SELECT COALESCE(SUM(cache_read_tokens), 0)     FROM curator_messages WHERE org_id = $2 AND request_id = $3),
		    cache_creation_tokens = (SELECT COALESCE(SUM(cache_creation_tokens), 0) FROM curator_messages WHERE org_id = $2 AND request_id = $3)
		WHERE org_id = $2 AND id = $3 AND status NOT IN ('done', 'cancelled', 'failed')
	`, errBind, orgID, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// QueuedRequestsForProjectSystem lists queued rows via the admin pool
// for the project-delete drain. See
// db.CuratorStore.QueuedRequestsForProjectSystem.
func (s *curatorStore) QueuedRequestsForProjectSystem(ctx context.Context, orgID, projectID string) ([]domain.CuratorRequest, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT id::text, project_id::text, status, user_input, error_msg,
		       cost_usd, duration_ms, num_turns,
		       input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
		       started_at, finished_at, created_at, creator_user_id::text
		FROM curator_requests
		WHERE org_id = $1 AND project_id = $2 AND status = 'queued'
		ORDER BY created_at ASC, id ASC
	`, orgID, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.CuratorRequest{}
	for rows.Next() {
		req, err := scanPgCuratorRequest(rows)
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
	// check fires, which is what the multi-user test
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
	// Roll up the token cache here too (correlated SUM per row) — a 'running'
	// request stranded by a crash may have streamed curator_messages, so the
	// boot sweep must reflect them rather than leave the columns at 0. Same
	// every-terminal-write invariant as the cancel paths; 'queued' rows simply
	// have no messages and sum to 0. Correlates on the unique request_id, so no
	// org scoping is needed on this cross-tenant sweep (TFAC-473).
	res, err := s.admin.ExecContext(ctx, `
		UPDATE curator_requests
		SET status = 'cancelled',
		    error_msg = COALESCE(error_msg, 'process restarted'),
		    finished_at = COALESCE(finished_at, now()),
		    input_tokens          = (SELECT COALESCE(SUM(input_tokens), 0)          FROM curator_messages WHERE request_id = curator_requests.id),
		    output_tokens         = (SELECT COALESCE(SUM(output_tokens), 0)         FROM curator_messages WHERE request_id = curator_requests.id),
		    cache_read_tokens     = (SELECT COALESCE(SUM(cache_read_tokens), 0)     FROM curator_messages WHERE request_id = curator_requests.id),
		    cache_creation_tokens = (SELECT COALESCE(SUM(cache_creation_tokens), 0) FROM curator_messages WHERE request_id = curator_requests.id)
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

func (s *curatorStore) ListRequestsByProject(ctx context.Context, orgID, projectID string) ([]domain.CuratorRequest, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT id::text, project_id::text, status, user_input, error_msg,
		       cost_usd, duration_ms, num_turns,
		       input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
		       started_at, finished_at, created_at, creator_user_id::text
		FROM curator_requests
		WHERE org_id = $1 AND project_id = $2
		ORDER BY created_at ASC, id ASC
	`, orgID, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.CuratorRequest{}
	for rows.Next() {
		req, err := scanPgCuratorRequest(rows)
		if err != nil {
			return nil, err
		}
		if req != nil {
			out = append(out, *req)
		}
	}
	return out, rows.Err()
}

func (s *curatorStore) ListMessagesByRequestIDs(ctx context.Context, orgID string, requestIDs []string) (map[string][]domain.CuratorMessage, error) {
	out := make(map[string][]domain.CuratorMessage)
	if len(requestIDs) == 0 {
		return out, nil
	}
	// request_id is a uuid column, so the id list goes through
	// pgUUIDArray (a Postgres array literal) rather than a raw
	// []string bind — same reasoning as ArtifactStore.CountByRunIDs.
	// One query, no chunking: the whole list is a single array bind.
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, request_id::text, role, content, subtype, tool_calls, tool_call_id, is_error, metadata,
		       model, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, created_at
		FROM curator_messages
		WHERE org_id = $1 AND request_id = ANY($2)
		ORDER BY created_at ASC, id ASC
	`, orgID, pgUUIDArray(requestIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		m, err := scanPgCuratorMessage(rows)
		if err != nil {
			return nil, err
		}
		out[m.RequestID] = append(out[m.RequestID], m)
	}
	return out, rows.Err()
}

func (s *curatorStore) InFlightRequestForProject(ctx context.Context, orgID, projectID string) (*domain.CuratorRequest, error) {
	row := s.q.QueryRowContext(ctx, `
		SELECT id::text, project_id::text, status, user_input, error_msg,
		       cost_usd, duration_ms, num_turns,
		       input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
		       started_at, finished_at, created_at, creator_user_id::text
		FROM curator_requests
		WHERE org_id = $1 AND project_id = $2 AND status IN ('queued', 'running')
		ORDER BY (status = 'running') DESC, created_at ASC, id ASC
		LIMIT 1
	`, orgID, projectID)
	return scanPgCuratorRequest(row)
}

// ResetForProject: the in-flight guard reads via the ADMIN pool on
// purpose — the reset tears down the project's SHARED curator session,
// and the modify/select policies are self-only, so an app-pool count
// could not see a teammate's live turn (exactly the turn the guard
// exists to protect). The read stays org+project-scoped in the WHERE.
// The deletes then run on the claims-bound queryer, so a caller only
// ever destroys their own history rows under RLS; other users' rows
// survive and the next turn starts a fresh session.
//
// Unlike the SQLite impl, the guard runs on a different connection
// than the deletes (the admin pool is never the in-flight tx), so the
// check-then-wipe is best-effort rather than atomic — acceptable
// because the curator runtime serializes same-project turns and the
// 409 is a UX courtesy, not a correctness gate.
func (s *curatorStore) ResetForProject(ctx context.Context, orgID, projectID string) error {
	var inflight int
	if err := s.admin.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM curator_requests
		 WHERE org_id = $1 AND project_id = $2 AND status IN ('queued', 'running')
	`, orgID, projectID).Scan(&inflight); err != nil {
		return fmt.Errorf("count inflight: %w", err)
	}
	if inflight > 0 {
		return db.ErrCuratorInFlight
	}
	if _, err := s.q.ExecContext(ctx, `
		UPDATE projects SET curator_session_id = NULL, updated_at = now()
		 WHERE org_id = $1 AND id = $2
	`, orgID, projectID); err != nil {
		return fmt.Errorf("clear session id: %w", err)
	}
	if _, err := s.q.ExecContext(ctx, `
		DELETE FROM curator_pending_context WHERE org_id = $1 AND project_id = $2
	`, orgID, projectID); err != nil {
		return fmt.Errorf("delete pending context: %w", err)
	}
	if _, err := s.q.ExecContext(ctx, `
		DELETE FROM curator_requests WHERE org_id = $1 AND project_id = $2
	`, orgID, projectID); err != nil {
		return fmt.Errorf("delete requests: %w", err)
	}
	return nil
}

// ImportRequest preserves the bundle row's id/status/accounting/
// timestamps verbatim. creator_user_id stamps to tf.current_user_id()
// (the importing user — the original creator is not a user here), so
// the curator_requests_modify WITH CHECK passes under the importer's
// claims. team_id snapshots from the destination project row per the
// TFAC-476 invariant ($2 reused for the correlated lookup).
func (s *curatorStore) ImportRequest(ctx context.Context, orgID string, req domain.CuratorRequest) error {
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO curator_requests (
			id, org_id, project_id, team_id, creator_user_id, status, user_input, error_msg,
			cost_usd, duration_ms, num_turns,
			input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
			started_at, finished_at, created_at
		) VALUES ($1, $3, $2, (SELECT team_id FROM projects WHERE id = $2), tf.current_user_id(),
		          $4, $5, NULLIF($6, ''), $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
	`,
		req.ID, req.ProjectID, orgID,
		req.Status, req.UserInput, req.ErrorMsg,
		req.CostUSD, req.DurationMs, req.NumTurns,
		req.InputTokens, req.OutputTokens, req.CacheReadTokens, req.CacheCreationTokens,
		timePtrAny(req.StartedAt), timePtrAny(req.FinishedAt), req.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("import curator_request %s: %w", req.ID, err)
	}
	return nil
}

// ImportPendingContext preserves consumed state + created_at, unlike
// InsertPendingContext (which queues a fresh unconsumed delta).
func (s *curatorStore) ImportPendingContext(ctx context.Context, orgID string, row domain.CuratorPendingContext) error {
	var consumedBy any
	if row.ConsumedByRequestID != "" {
		consumedBy = row.ConsumedByRequestID
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO curator_pending_context (
			org_id, creator_user_id, project_id, curator_session_id, change_type, baseline_value,
			consumed_at, consumed_by_request_id, created_at
		) VALUES ($1, tf.current_user_id(), $2, $3, $4, $5, $6, $7, $8)
	`,
		orgID, row.ProjectID, row.CuratorSessionID, row.ChangeType, row.BaselineValue,
		timePtrAny(row.ConsumedAt), consumedBy, row.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("import pending context row: %w", err)
	}
	return nil
}

// scanPgCuratorMessage mirrors the SQLite scanCuratorMessageRowSqlite
// column ordering (see ListMessagesByRequestIDs' SELECT list).
func scanPgCuratorMessage(rows *sql.Rows) (domain.CuratorMessage, error) {
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

// timePtrAny maps a *time.Time to a bind-compatible value (nil for
// NULL). Sibling of intPtrAny below.
func timePtrAny(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
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
