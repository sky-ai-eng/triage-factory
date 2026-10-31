package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// memoryAttemptStore is the SQLite impl of db.MemoryAttemptStore — the
// memory-generation attempt ledger. SQLite is single-tenant (local mode, N=1)
// with no RLS: the org_id column exists for parity with the Postgres baseline
// and every row carries LocalDefaultOrgID, which assertLocalOrg pins at every
// entry point. The Postgres impl routes through the admin pool; here the two
// pools collapse to the one connection.
type memoryAttemptStore struct{ q queryer }

func newMemoryAttemptStore(q queryer) db.MemoryAttemptStore { return &memoryAttemptStore{q: q} }

var _ db.MemoryAttemptStore = (*memoryAttemptStore)(nil)

// memoryAttemptColumns is the canonical projection of a
// conversation_memory_attempts row, in the order db.ScanMemoryAttempt reads
// them. The point read SELECTs it and both writes RETURN it, so the write shape
// cannot drift from the read shape.
const memoryAttemptColumns = `id, org_id, conversation_id, started_at, completed_at,
	COALESCE(outcome, ''), COALESCE(error_kind, ''), COALESCE(error_message, ''),
	COALESCE(system_llm_run_id, ''), window_rows_total, window_rows_sent`

func (s *memoryAttemptStore) BeginAttemptSystem(ctx context.Context, orgID, conversationID string) (domain.MemoryAttempt, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.MemoryAttempt{}, err
	}
	return db.ScanMemoryAttempt(s.q.QueryRowContext(ctx, `
		INSERT INTO conversation_memory_attempts (id, org_id, conversation_id, started_at)
		VALUES (?, ?, ?, ?)
		RETURNING `+memoryAttemptColumns,
		uuid.New().String(), orgID, conversationID, time.Now().UTC()).Scan)
}

func (s *memoryAttemptStore) CompleteAttemptSystem(ctx context.Context, orgID, attemptID string,
	outcome domain.MemoryAttemptOutcome, errKind domain.MemoryAttemptErrorKind,
	errMsg, systemLLMRunID string, rowsTotal, rowsSent int,
) (domain.MemoryAttempt, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.MemoryAttempt{}, err
	}
	if err := db.ValidateMemoryAttemptVerdict(outcome, errKind, errMsg); err != nil {
		return domain.MemoryAttempt{}, err
	}
	// system_llm_run_id rides a subselect, not a direct bind: the spend-ledger
	// insert is best-effort, so an id whose row never landed must store NULL
	// rather than violate the FK. Org-scoped inside the subselect too, for
	// parity with the Postgres statement this mirrors.
	a, err := db.ScanMemoryAttempt(s.q.QueryRowContext(ctx, `
		UPDATE conversation_memory_attempts
		SET completed_at      = ?,
		    outcome           = ?,
		    error_kind        = NULLIF(?, ''),
		    error_message     = NULLIF(?, ''),
		    system_llm_run_id = (SELECT id FROM system_llm_runs WHERE id = NULLIF(?, '') AND org_id = ?),
		    window_rows_total = ?,
		    window_rows_sent  = ?
		WHERE org_id = ? AND id = ? AND completed_at IS NULL
		RETURNING `+memoryAttemptColumns,
		time.Now().UTC(), string(outcome), string(errKind), errMsg,
		systemLLMRunID, orgID, rowsTotal, rowsSent, orgID, attemptID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.MemoryAttempt{}, db.ErrNoSuchMemoryAttempt
	}
	if err != nil {
		return domain.MemoryAttempt{}, err
	}
	return a, nil
}

func (s *memoryAttemptStore) NewestAttemptForConversationSystem(ctx context.Context, orgID, conversationID string) (*domain.MemoryAttempt, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	// The id tiebreaker is not decoration: two attempts on one conversation
	// can share a started_at, and without it "the newest" is whichever row the
	// planner happened to reach first.
	a, err := db.ScanMemoryAttempt(s.q.QueryRowContext(ctx, `
		SELECT `+memoryAttemptColumns+`
		FROM conversation_memory_attempts
		WHERE org_id = ? AND conversation_id = ?
		ORDER BY started_at DESC, id DESC
		LIMIT 1
	`, orgID, conversationID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *memoryAttemptStore) FailingSystem(ctx context.Context, orgID string, span, window time.Duration) (domain.MemoryFailing, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.MemoryFailing{}, err
	}
	if span < 0 || window <= 0 {
		return domain.MemoryFailing{}, db.ErrInvalidMemoryFailingWindow
	}
	var out domain.MemoryFailing
	if err := s.q.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM tasks t
		WHERE t.org_id = ? AND t.status NOT IN ('done', 'dismissed')
		  AND `+taskMemoryPendingSQL("t.org_id", "t.id")+`
	`, orgID).Scan(&out.TasksWaiting); err != nil {
		return domain.MemoryFailing{}, err
	}

	// The Postgres twin carries the reasoning, and this is the same read:
	// correlated subqueries make its per-conversation probes, and the window
	// is a range on idx_conversation_memory_attempts_conversation, so the
	// cost does not grow with the outage. The CROSS JOINs fix the join order
	// that range needs, since SQLite plans a CROSS JOIN's left side as the
	// outer loop, and the unary + keeps the bound at the newest generated
	// memory out of the index, so the range starts at the window instead.
	// Attempt times compare as their stored text, which sorts in instant order
	// because every one is written from a UTC time; julianday() measures only
	// the two ends.
	rows, err := s.q.QueryContext(ctx, `
		WITH generated AS (
		    SELECT COALESCE(MAX(completed_at), '') AS at
		    FROM conversation_memory_attempts
		    WHERE org_id = ? AND outcome = 'generated'
		),
		owing AS MATERIALIZED (
		    SELECT owed.id
		    FROM conversations owed, tasks t
		    WHERE `+taskMemoryOwedRowSQL("t.org_id", "t.id")+`
		      AND t.org_id = ?
		),
		probes AS (
		    SELECT
		        (SELECT a.started_at FROM conversation_memory_attempts a
		         WHERE a.conversation_id = o.id AND a.started_at > g.at AND a.outcome = 'failed'
		         ORDER BY a.started_at ASC, a.id ASC
		         LIMIT 1) AS first,
		        (SELECT a.started_at FROM conversation_memory_attempts a
		         WHERE a.conversation_id = o.id AND a.started_at > g.at AND a.outcome = 'failed'
		         ORDER BY a.started_at DESC, a.id DESC
		         LIMIT 1) AS last
		    FROM generated g CROSS JOIN owing o
		),
		ends AS (
		    SELECT MIN(first) AS oldest, MAX(last) AS newest FROM probes
		),
		bounds AS (
		    SELECT (julianday(newest) - julianday(oldest)) * 86400.0 AS spanned,
		           strftime('%Y-%m-%d %H:%M:%f', julianday(newest) - ? / 86400.0) AS since
		    FROM ends
		),
		recent AS (
		    SELECT a.id, a.started_at, a.error_kind, a.error_message,
		           ROW_NUMBER() OVER (
		               PARTITION BY a.error_kind ORDER BY a.started_at DESC, a.id DESC) AS rn
		    FROM bounds b CROSS JOIN generated g CROSS JOIN owing o
		         CROSS JOIN conversation_memory_attempts a
		    WHERE b.spanned >= ?
		      AND a.conversation_id = o.id AND a.started_at >= b.since AND +a.started_at > g.at
		      AND a.outcome = 'failed'
		)
		SELECT error_kind, COALESCE(error_message, '')
		FROM recent
		WHERE rn = 1
		ORDER BY started_at DESC, id DESC
	`, orgID, orgID, window.Seconds(), span.Seconds())
	if err != nil {
		return domain.MemoryFailing{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var k domain.MemoryFailingKind
		if err := rows.Scan(&k.Kind, &k.ErrorMessage); err != nil {
			return domain.MemoryFailing{}, err
		}
		out.Kinds = append(out.Kinds, k)
	}
	if err := rows.Err(); err != nil {
		return domain.MemoryFailing{}, err
	}
	return out, nil
}
