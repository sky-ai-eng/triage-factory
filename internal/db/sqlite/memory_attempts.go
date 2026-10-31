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
	// The Postgres twin carries the reasoning. SQLite has no LATERAL, so the
	// failures since the newest generated memory are read whole rather than
	// probed per conversation; local mode's ledger is one person's. Times are
	// compared through julianday(), which reads every stored format as the
	// instant it names.
	if err := s.q.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT t.id)
		FROM conversations owed, tasks t
		WHERE `+taskMemoryOwedRowSQL("t.org_id", "t.id")+`
		  AND t.org_id = ? AND t.status NOT IN ('done', 'dismissed')
	`, orgID).Scan(&out.TasksWaiting); err != nil {
		return domain.MemoryFailing{}, err
	}

	rows, err := s.q.QueryContext(ctx, `
		WITH generated AS (
		    SELECT julianday(MAX(completed_at)) AS at
		    FROM conversation_memory_attempts
		    WHERE org_id = ? AND outcome = 'generated'
		),
		failed AS (
		    SELECT att.id, att.started_at, julianday(att.started_at) AS day,
		           att.error_kind, att.error_message
		    FROM conversations owed, tasks t, generated g, conversation_memory_attempts att
		    WHERE `+taskMemoryOwedRowSQL("t.org_id", "t.id")+`
		      AND t.org_id = ?
		      AND att.conversation_id = owed.id AND att.outcome = 'failed'
		      AND (g.at IS NULL OR julianday(att.started_at) > g.at)
		),
		ends AS (SELECT MIN(day) AS oldest, MAX(day) AS newest FROM failed),
		recent AS (
		    SELECT f.id, f.started_at, f.error_kind, f.error_message,
		           ROW_NUMBER() OVER (
		               PARTITION BY f.error_kind ORDER BY f.day DESC, f.id DESC) AS rn
		    FROM failed f, ends e
		    WHERE (e.newest - e.oldest) * 86400.0 >= ?
		      AND f.day >= e.newest - ? / 86400.0
		)
		SELECT error_kind, COALESCE(error_message, '')
		FROM recent
		WHERE rn = 1
		ORDER BY julianday(started_at) DESC, id DESC
	`, orgID, orgID, span.Seconds(), window.Seconds())
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
