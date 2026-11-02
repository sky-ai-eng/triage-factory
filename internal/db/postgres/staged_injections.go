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

// withdrawProducerNotesSQL withdraws the notes producer $4 staged in org $1
// that are still waiting, under any of the staged subtypes ($3), as
// DeleteSystem withdraws one: delivered stays false and the row leaves the
// window. Each scope below adds which conversations, named by $2.
const withdrawProducerNotesSQL = `
	UPDATE messages SET window_state = 'inactive'
	 WHERE org_id = $1 AND NOT delivered AND role = 'user'
	   AND subtype = ANY($3::text[]) AND metadata->>'producer' = $4`

// withdrawProducerNotesForTaskSQL scopes withdrawProducerNotesSQL to task
// $2's conversations. The task store's clears of a held close run it in
// their own statements, since a hold cleared without its close makes its
// notes false.
const withdrawProducerNotesForTaskSQL = withdrawProducerNotesSQL + `
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

func (s *stagedInjectionStore) WithdrawPendingSystem(ctx context.Context, orgID, conversationID, producer string) error {
	if _, err := s.admin.ExecContext(ctx, withdrawProducerNotesSQL+`
	   AND conversation_id = $2`, orgID, conversationID, domain.StagedInjectionSubtypes, producer); err != nil {
		return fmt.Errorf("withdraw %s notes: %w", producer, err)
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
	// Nothing was withdrawn. delivered only ever flips to true, so reading it
	// after the update cannot disagree with what stopped the update.
	var consumed bool
	err = s.admin.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM messages
			WHERE org_id = $1::uuid AND id = $2 AND subtype = ANY($3::text[]) AND delivered = true
		)
	`, orgID, rowID, domain.StagedInjectionSubtypes).Scan(&consumed)
	return consumed, err
}

func (s *stagedInjectionStore) HasNoteForEventSystem(ctx context.Context, orgID, conversationID, producer, eventID string) (bool, error) {
	var told bool
	err := s.admin.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM messages
			WHERE org_id = $1::uuid AND conversation_id = $2::uuid
			  AND role = 'user' AND subtype = ANY($3::text[])
			  AND metadata->>'producer' = $4 AND metadata->>'event_id' = $5
			  AND (delivered OR window_state = 'active')
		)
	`, orgID, conversationID, domain.StagedInjectionSubtypes, producer, eventID).Scan(&told)
	return told, err
}
