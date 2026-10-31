package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// memoryAttemptStore is the Postgres impl of db.MemoryAttemptStore. Wired
// against the ADMIN pool in postgres.New: the writer is the brain's memory
// provisioner, a boot-launched goroutine with no JWT-claims context, so an
// app-pool write under the conversation_memory_attempts_all RLS policy would
// be rejected — and tf_app holds SELECT alone anyway. That org-scoped policy
// is what gates the app-pool reads; org_id stays bound in every statement as
// defense in depth.
type memoryAttemptStore struct{ admin queryer }

func newMemoryAttemptStore(admin queryer) db.MemoryAttemptStore {
	return &memoryAttemptStore{admin: admin}
}

var _ db.MemoryAttemptStore = (*memoryAttemptStore)(nil)

// pgMemoryAttemptColumns is the canonical projection of a
// conversation_memory_attempts row, in the order db.ScanMemoryAttempt reads
// them. The point read SELECTs it and both writes RETURN it, so the write
// shape cannot drift from the read shape. uuids are cast to text so the scan
// targets stay plain strings, matching pgConversationColumns.
const pgMemoryAttemptColumns = `id::text, org_id::text, conversation_id::text,
	started_at, completed_at,
	COALESCE(outcome, ''), COALESCE(error_kind, ''), COALESCE(error_message, ''),
	COALESCE(system_llm_run_id::text, ''), window_rows_total, window_rows_sent`

func (s *memoryAttemptStore) BeginAttemptSystem(ctx context.Context, orgID, conversationID string) (domain.MemoryAttempt, error) {
	a, err := db.ScanMemoryAttempt(s.admin.QueryRowContext(ctx, `
		INSERT INTO conversation_memory_attempts (org_id, conversation_id, started_at)
		VALUES ($1, $2, $3)
		RETURNING `+pgMemoryAttemptColumns,
		orgID, conversationID, time.Now().UTC()).Scan)
	if err != nil {
		return domain.MemoryAttempt{}, wrapAdminPoolPermErr(err, "conversation_memory_attempts.BeginAttemptSystem")
	}
	return a, nil
}

func (s *memoryAttemptStore) CompleteAttemptSystem(ctx context.Context, orgID, attemptID string,
	outcome domain.MemoryAttemptOutcome, errKind domain.MemoryAttemptErrorKind,
	errMsg, systemLLMRunID string, rowsTotal, rowsSent int,
) (domain.MemoryAttempt, error) {
	if err := db.ValidateMemoryAttemptVerdict(outcome, errKind, errMsg); err != nil {
		return domain.MemoryAttempt{}, err
	}
	// A non-UUID id names no row, which is what the CAS miss already means —
	// same mutating-method convention isValidUUID documents, and it keeps the
	// caller from meeting a 22P02 where it expected the sentinel.
	if !isValidUUID(attemptID) {
		return domain.MemoryAttempt{}, db.ErrNoSuchMemoryAttempt
	}
	// system_llm_run_id rides a subselect, not a direct bind: the spend-ledger
	// insert is best-effort, so an id whose row never landed must store NULL
	// rather than violate the FK. Org-scoped inside the subselect too — a
	// ledger row from another tenant is as much "not ours" as one that never
	// existed.
	a, err := db.ScanMemoryAttempt(s.admin.QueryRowContext(ctx, `
		UPDATE conversation_memory_attempts
		SET completed_at      = $1,
		    outcome           = $2,
		    error_kind        = NULLIF($3, ''),
		    error_message     = NULLIF($4, ''),
		    system_llm_run_id = (SELECT id FROM system_llm_runs WHERE id = $5 AND org_id = $6),
		    window_rows_total = $7,
		    window_rows_sent  = $8
		WHERE org_id = $6 AND id = $9 AND completed_at IS NULL
		RETURNING `+pgMemoryAttemptColumns,
		time.Now().UTC(), string(outcome), string(errKind), errMsg,
		nullUUID(systemLLMRunID), orgID, rowsTotal, rowsSent, attemptID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.MemoryAttempt{}, db.ErrNoSuchMemoryAttempt
	}
	if err != nil {
		return domain.MemoryAttempt{}, wrapAdminPoolPermErr(err, "conversation_memory_attempts.CompleteAttemptSystem")
	}
	return a, nil
}

func (s *memoryAttemptStore) NewestAttemptForConversationSystem(ctx context.Context, orgID, conversationID string) (*domain.MemoryAttempt, error) {
	if !isValidUUID(conversationID) {
		return nil, nil
	}
	// The id tiebreaker is not decoration: two attempts on one conversation
	// can share a started_at, and without it "the newest" is whichever row the
	// planner happened to reach first.
	a, err := db.ScanMemoryAttempt(s.admin.QueryRowContext(ctx, `
		SELECT `+pgMemoryAttemptColumns+`
		FROM conversation_memory_attempts
		WHERE org_id = $1 AND conversation_id = $2
		ORDER BY started_at DESC, id DESC
		LIMIT 1
	`, orgID, conversationID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, wrapAdminPoolPermErr(err, "conversation_memory_attempts.NewestAttemptForConversationSystem")
	}
	return &a, nil
}

func (s *memoryAttemptStore) FailingSystem(ctx context.Context, orgID string, span, window time.Duration) (domain.MemoryFailing, error) {
	if span < 0 || window <= 0 {
		return domain.MemoryFailing{}, db.ErrInvalidMemoryFailingWindow
	}
	var out domain.MemoryFailing
	// "Waiting" is the task read's memory_pending, in the same words.
	if err := s.admin.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM tasks t
		WHERE t.org_id = $1 AND t.status NOT IN ('done', 'dismissed')
		  AND `+taskMemoryPendingSQL("t.org_id", "t.id")+`
	`, orgID).Scan(&out.TasksWaiting); err != nil {
		return domain.MemoryFailing{}, wrapAdminPoolPermErr(err, "conversation_memory_attempts.FailingSystem")
	}

	// Every read below is bounded however long an outage has run: the first
	// and newest failure are one probe each per owing conversation on
	// idx_conversation_memory_attempts_conversation, the window is a range on
	// it, and idx_conversation_memory_attempts_generated answers the newest
	// generated memory without reading the org's history. An org that has
	// never generated one measures from -infinity rather than through an OR,
	// so the bound stays a range the probes can seek to.
	rows, err := s.admin.QueryContext(ctx, `
		WITH generated AS (
		    SELECT COALESCE(MAX(completed_at), '-infinity') AS at
		    FROM conversation_memory_attempts
		    WHERE org_id = $1 AND outcome = 'generated'
		),
		owing AS (
		    SELECT owed.id
		    FROM conversations owed, tasks t
		    WHERE `+taskMemoryOwedRowSQL("t.org_id", "t.id")+`
		      AND t.org_id = $1
		),
		ends AS (
		    SELECT MIN(first.started_at) AS oldest, MAX(last.started_at) AS newest
		    FROM owing o, generated g,
		    LATERAL (
		        SELECT a.started_at FROM conversation_memory_attempts a
		        WHERE a.conversation_id = o.id AND a.outcome = 'failed'
		          AND a.started_at > g.at
		        ORDER BY a.started_at ASC, a.id ASC
		        LIMIT 1
		    ) first,
		    LATERAL (
		        SELECT a.started_at FROM conversation_memory_attempts a
		        WHERE a.conversation_id = o.id AND a.outcome = 'failed'
		          AND a.started_at > g.at
		        ORDER BY a.started_at DESC, a.id DESC
		        LIMIT 1
		    ) last
		),
		recent AS (
		    SELECT a.id, a.started_at, a.error_kind, a.error_message,
		           ROW_NUMBER() OVER (
		               PARTITION BY a.error_kind ORDER BY a.started_at DESC, a.id DESC) AS rn
		    FROM owing o, generated g, ends e, conversation_memory_attempts a
		    WHERE a.conversation_id = o.id AND a.outcome = 'failed'
		      AND a.started_at > g.at
		      AND e.newest - e.oldest >= $2::float8 * interval '1 second'
		      AND a.started_at >= e.newest - $3::float8 * interval '1 second'
		)
		SELECT error_kind, COALESCE(error_message, '')
		FROM recent
		WHERE rn = 1
		ORDER BY started_at DESC, id DESC
	`, orgID, span.Seconds(), window.Seconds())
	if err != nil {
		return domain.MemoryFailing{}, wrapAdminPoolPermErr(err, "conversation_memory_attempts.FailingSystem")
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
