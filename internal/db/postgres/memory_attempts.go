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

func (s *memoryAttemptStore) FailingSystem(ctx context.Context, orgID string, streak int) (domain.MemoryFailing, error) {
	if streak < 1 {
		return domain.MemoryFailing{}, db.ErrInvalidMemoryFailingStreak
	}
	var out domain.MemoryFailing
	// The owing condition is taskMemoryOwedRowSQL, the same words the task
	// read's memory_pending and the sweep are written in, so "waiting" here is
	// the wait those two report.
	if err := s.admin.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT t.id)
		FROM conversations owed, tasks t
		WHERE `+taskMemoryOwedRowSQL("t.org_id", "t.id")+`
		  AND t.org_id = $1 AND t.status NOT IN ('done', 'dismissed')
	`, orgID).Scan(&out.TasksWaiting); err != nil {
		return domain.MemoryFailing{}, wrapAdminPoolPermErr(err, "conversation_memory_attempts.FailingSystem")
	}

	// The LATERAL takes each owing conversation's newest completed attempts in
	// idx_conversation_memory_attempts_conversation's order, so the read is
	// `streak` rows per owing conversation however long an outage has run, and
	// idx_conversation_memory_attempts_generated answers the newest generated
	// memory without reading the org's history.
	rows, err := s.admin.QueryContext(ctx, `
		WITH generated AS (
		    SELECT MAX(completed_at) AS at
		    FROM conversation_memory_attempts
		    WHERE org_id = $1 AND outcome = 'generated'
		),
		newest AS (
		    SELECT owed.id AS conversation_id, att.id, att.started_at, att.error_kind, att.error_message,
		           (att.outcome = 'failed' AND (g.at IS NULL OR att.completed_at > g.at)) AS failing
		    FROM conversations owed, tasks t, generated g,
		    LATERAL (
		        SELECT a.id, a.started_at, a.completed_at, a.outcome, a.error_kind, a.error_message
		        FROM conversation_memory_attempts a
		        WHERE a.conversation_id = owed.id AND a.completed_at IS NOT NULL
		        ORDER BY a.started_at DESC, a.id DESC
		        LIMIT $2
		    ) att
		    WHERE `+taskMemoryOwedRowSQL("t.org_id", "t.id")+`
		      AND t.org_id = $1
		),
		stuck AS (
		    SELECT conversation_id FROM newest
		    GROUP BY conversation_id
		    HAVING COUNT(*) FILTER (WHERE failing) = $2
		)
		SELECT n.error_kind, COALESCE(n.error_message, '')
		FROM newest n JOIN stuck USING (conversation_id)
		ORDER BY n.started_at DESC, n.id DESC
	`, orgID, streak)
	if err != nil {
		return domain.MemoryFailing{}, wrapAdminPoolPermErr(err, "conversation_memory_attempts.FailingSystem")
	}
	defer rows.Close()
	var found []domain.MemoryFailingKind
	for rows.Next() {
		var k domain.MemoryFailingKind
		if err := rows.Scan(&k.Kind, &k.ErrorMessage); err != nil {
			return domain.MemoryFailing{}, err
		}
		found = append(found, k)
	}
	if err := rows.Err(); err != nil {
		return domain.MemoryFailing{}, err
	}
	out.Kinds = db.FirstOfEachMemoryFailingKind(found)
	return out, nil
}
