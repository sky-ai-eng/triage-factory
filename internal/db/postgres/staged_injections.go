package postgres

import (
	"context"
	"fmt"
	"strconv"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// stagedInjectionStore is the Postgres impl of db.StagedInjectionStore — the
// durable "stage for next resume" agent-injection queue (TFAC-501), backed
// by undelivered messages rows (role='user', subtype 'injection:event' or
// 'injection:system-note') rather than a side table — one transcript canon. Wired against the ADMIN (BYPASSRLS) pool in postgres.New: both the
// producer (an eventbus subscriber) and the consumer (a detached resume
// goroutine) run without JWT claims, so an app-pool/RLS statement would see
// nothing. org_id stays bound in every WHERE/INSERT clause as defense in
// depth.
type stagedInjectionStore struct{ admin queryer }

func newStagedInjectionStore(admin queryer) db.StagedInjectionStore {
	return &stagedInjectionStore{admin: admin}
}

var _ db.StagedInjectionStore = (*stagedInjectionStore)(nil)

func (s *stagedInjectionStore) AppendSystem(ctx context.Context, orgID string, n domain.StagedInjection) (domain.StagedInjection, error) {
	if err := db.RequireStagedInjectionSubtype(n.Subtype); err != nil {
		return domain.StagedInjection{}, err
	}
	meta, err := db.StagedInjectionMetadata(n)
	if err != nil {
		return domain.StagedInjection{}, err
	}
	// created_at is left to the column DEFAULT now(). RETURNING projects the
	// same columns flushPendingInput's SELECT does, so the freshly-inserted
	// row and a later flush of it map through db.StagedInjectionFromPending.
	var r db.PendingRow
	if err := s.admin.QueryRowContext(ctx, `
		INSERT INTO messages (org_id, conversation_id, role, content, subtype, metadata, delivered)
		VALUES ($1::uuid, $2::uuid, 'user', $3, $4, $5::jsonb, false)
		RETURNING id, org_id::text, conversation_id::text, content, subtype,
		          COALESCE(user_id::text, ''), COALESCE(metadata::text, ''), created_at
	`, orgID, n.ConversationID, n.Body, n.Subtype, meta).Scan(
		&r.ID, &r.OrgID, &r.ConvID, &r.Content, &r.Subtype, &r.UserID, &r.Metadata, &r.CreatedAt,
	); err != nil {
		return domain.StagedInjection{}, err
	}
	return db.StagedInjectionFromPending(r), nil
}

func (s *stagedInjectionStore) FlushPendingSystem(ctx context.Context, orgID, conversationID string) ([]domain.StagedInjection, error) {
	// The shared flush primitive, narrowed to this store's subtypes: one
	// definition of "consume a conversation's pending rows" serves every
	// producer, so a note is delivered exactly once even under a resume race
	// and withdrawn rows stay out by the same rule the claim predicate uses.
	flushed, err := flushPendingInput(ctx, s.admin, orgID, conversationID, domain.StagedInjectionSubtypes)
	if err != nil {
		return nil, err
	}
	return db.StagedInjectionsFromPending(flushed), nil
}

// withdrawProducerNotesForTaskSQL withdraws the notes producer $4 staged on
// task $2's conversations, in org $1, that are still waiting, under any of
// the staged subtypes ($3), as DeleteSystem withdraws one: delivered stays
// false and the row leaves the window. The task store's clears of a held
// close run it in their own statements, since a hold cleared without its
// close makes its notes false.
const withdrawProducerNotesForTaskSQL = `
	UPDATE messages SET window_state = 'inactive'
	 WHERE org_id = $1 AND NOT delivered AND role = 'user'
	   AND subtype = ANY($3::text[]) AND metadata->>'producer' = $4
	   AND conversation_id IN (SELECT c.id FROM conversations c WHERE c.org_id = $1 AND c.task_id = $2)`

func (s *stagedInjectionStore) RestorePendingSystem(ctx context.Context, orgID, conversationID string, ids []string) error {
	rowIDs := make([]int64, 0, len(ids))
	for _, id := range ids {
		if n, err := strconv.ParseInt(id, 10, 64); err == nil {
			rowIDs = append(rowIDs, n)
		}
	}
	if len(rowIDs) == 0 {
		return nil
	}
	if _, err := s.admin.ExecContext(ctx, `
		UPDATE messages SET delivered = false
		WHERE org_id = $1::uuid AND conversation_id = $2::uuid AND id = ANY($3::bigint[])
		  AND delivered = true AND window_state = 'active'
		  AND role = 'user' AND subtype = ANY($4::text[])
	`, orgID, conversationID, rowIDs, domain.StagedInjectionSubtypes); err != nil {
		return fmt.Errorf("restore staged injections: %w", err)
	}
	return nil
}

func (s *stagedInjectionStore) DeleteSystem(ctx context.Context, orgID, id string) (bool, error) {
	rowID, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		// A non-numeric id can only be a pre-refactor uuid that no longer
		// names any row — the contract is a silent no-op.
		return false, nil
	}
	// Retire, don't delete (see the interface doc): the executor's tf_system
	// role holds no DELETE on messages. delivered stays false — withdrawn
	// means the note never happened, and a delivered stamp would make it read
	// as transcript history; window_state='inactive' is what keeps it out of
	// the flush, assembly, and the display reads. The delivered = false guard
	// keeps this from retiring an already-flushed row a racing resume just
	// consumed.
	res, err := s.admin.ExecContext(ctx, `
		UPDATE messages SET window_state = 'inactive'
		WHERE org_id = $1::uuid AND id = $2 AND subtype = ANY($3::text[]) AND delivered = false
	`, orgID, rowID, domain.StagedInjectionSubtypes)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n > 0 {
		return false, err
	}
	// Nothing was withdrawn: report whether a reader had flushed the row.
	var consumed bool
	err = s.admin.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM messages
			WHERE org_id = $1::uuid AND id = $2 AND subtype = ANY($3::text[]) AND delivered = true
		)
	`, orgID, rowID, domain.StagedInjectionSubtypes).Scan(&consumed)
	return consumed, err
}

func (s *stagedInjectionStore) HasNoteForTaskCloseSystem(ctx context.Context, orgID, conversationID, taskClose string) (bool, error) {
	var told bool
	err := s.admin.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM messages
			WHERE org_id = $1::uuid AND conversation_id = $2::uuid
			  AND role = 'user' AND subtype = ANY($3::text[])
			  AND metadata->>'task_close' = $4
			  AND (delivered OR window_state = 'active')
		)
	`, orgID, conversationID, domain.StagedInjectionSubtypes, taskClose).Scan(&told)
	return told, err
}

func (s *stagedInjectionStore) RetireStaleAssignmentNotesSystem(ctx context.Context, orgID, conversationID string) (int, error) {
	// A note is kept only while an open task still carries its assignee, so
	// a task that closed, changed hands, went back to the queue, or is gone
	// retires it by the one NOT EXISTS. The id is compared as a uuid so the
	// lookup is the primary key's, and only where the stored value is one.
	res, err := s.admin.ExecContext(ctx, `
		UPDATE messages m SET window_state = 'inactive'
		 WHERE m.org_id = $1::uuid AND m.conversation_id = $2::uuid
		   AND NOT m.delivered AND m.window_state = 'active' AND m.role = 'user'
		   AND m.subtype = ANY($3::text[]) AND m.metadata->>'producer' = $4
		   AND (NOT EXISTS (
		            SELECT 1 FROM tasks x
		             WHERE x.org_id = m.org_id
		               AND x.id = CASE WHEN m.metadata->>'assigned_task' ~ '^[0-9a-fA-F]{8}(-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}$'
		                               THEN (m.metadata->>'assigned_task')::uuid END
		               AND x.status NOT IN ('done', 'dismissed')
		               AND m.metadata->>'assignee' = CASE
		                       WHEN x.claimed_by_agent_id IS NOT NULL THEN 'agent:' || x.claimed_by_agent_id::text
		                       WHEN x.claimed_by_user_id IS NOT NULL THEN 'user:' || x.claimed_by_user_id::text END)
		        OR EXISTS (
		            SELECT 1 FROM messages n
		             WHERE n.org_id = m.org_id AND n.conversation_id = m.conversation_id
		               AND NOT n.delivered AND n.window_state = 'active' AND n.role = 'user'
		               AND n.subtype = ANY($3::text[]) AND n.metadata->>'producer' = $4
		               AND n.metadata->>'assigned_task' = m.metadata->>'assigned_task'
		               AND n.id > m.id))
	`, orgID, conversationID, domain.StagedInjectionSubtypes, domain.StagedInjectionProducerTaskAssigned)
	if err != nil {
		return 0, fmt.Errorf("retire stale assignment notes: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}
