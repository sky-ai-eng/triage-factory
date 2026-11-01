package sqlite

import (
	"context"
	"database/sql"
	"sort"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// stagedInjectionStore is the SQLite impl of db.StagedInjectionStore, backed
// by undelivered messages rows (role='user', subtype 'injection:event' or
// 'injection:system-note') rather than a side table — one transcript canon. SQLite is single-tenant (local mode, N=1) with no RLS — org_id
// exists for parity with the Postgres baseline and every caller passes
// LocalDefaultOrgID (asserted at each entry). The `...System` method names
// mirror the interface; in SQLite there's one connection, so System is
// identical to a plain call.
type stagedInjectionStore struct{ q queryer }

func newStagedInjectionStore(q queryer) db.StagedInjectionStore { return &stagedInjectionStore{q: q} }

var _ db.StagedInjectionStore = (*stagedInjectionStore)(nil)

func (s *stagedInjectionStore) AppendSystem(ctx context.Context, orgID string, n domain.StagedInjection) (domain.StagedInjection, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.StagedInjection{}, err
	}
	if err := db.RequireStagedInjectionSubtype(n.Subtype); err != nil {
		return domain.StagedInjection{}, err
	}
	meta, err := db.StagedInjectionMetadata(n)
	if err != nil {
		return domain.StagedInjection{}, err
	}
	// RETURNING projects the same columns flushPendingInput's SELECT does, so
	// the freshly-inserted row and a later flush of it map through the same
	// db.StagedInjectionFromPending mapper.
	var (
		r        db.PendingRow
		userID   sql.NullString
		metadata sql.NullString
	)
	if err := s.q.QueryRowContext(ctx, `
		INSERT INTO messages (org_id, conversation_id, role, content, subtype, metadata, delivered)
		VALUES (?, ?, 'user', ?, ?, ?, 0)
		RETURNING id, org_id, conversation_id, content, subtype, user_id, metadata, created_at
	`, orgID, n.ConversationID, n.Body, n.Subtype, meta).Scan(
		&r.ID, &r.OrgID, &r.ConvID, &r.Content, &r.Subtype, &userID, &metadata, &r.CreatedAt,
	); err != nil {
		return domain.StagedInjection{}, err
	}
	r.UserID, r.Metadata = userID.String, metadata.String
	return db.StagedInjectionFromPending(r), nil
}

func (s *stagedInjectionStore) FlushPendingSystem(ctx context.Context, orgID, conversationID string) ([]domain.StagedInjection, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	// The shared flush primitive, narrowed to this store's subtypes: one
	// definition of "consume a conversation's pending rows" serves every
	// producer.
	flushed, err := flushPendingInput(ctx, s.q, orgID, conversationID, domain.StagedInjectionSubtypes)
	if err != nil {
		return nil, err
	}
	out := db.StagedInjectionsFromPending(flushed)
	// UPDATE … RETURNING does not honor ORDER BY in SQLite, so sort by the
	// monotonic row id to restore oldest-first — the order the bundled
	// block reads in.
	sort.SliceStable(out, func(i, j int) bool { return stagedInjectionIDLess(out[i].ID, out[j].ID) })
	return out, nil
}

func (s *stagedInjectionStore) DeleteSystem(ctx context.Context, orgID, id string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	rowID, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		// A non-numeric id can only be a pre-refactor uuid that no longer
		// names any row — the contract is a silent no-op.
		return false, nil
	}
	subtypes := strings.TrimRight(strings.Repeat("?, ", len(domain.StagedInjectionSubtypes)), ", ")
	args := []any{orgID, rowID}
	for _, st := range domain.StagedInjectionSubtypes {
		args = append(args, st)
	}
	// Retire, don't delete (see the interface doc). delivered stays 0 —
	// withdrawn means the note never happened, and a delivered stamp would
	// make it read as transcript history; window_state='inactive' is what
	// keeps it out of the flush, assembly, and the display reads. The
	// delivered = 0 guard keeps this from retiring an already-flushed row a
	// racing resume just consumed.
	res, err := s.q.ExecContext(ctx, `
		UPDATE messages SET window_state = 'inactive'
		WHERE org_id = ? AND id = ? AND delivered = 0 AND subtype IN (`+subtypes+`)
	`, args...)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n > 0 {
		return false, err
	}
	// Nothing was withdrawn. delivered only ever flips to 1, so reading it
	// after the update cannot disagree with what stopped the update.
	var consumed bool
	err = s.q.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM messages
			WHERE org_id = ? AND id = ? AND delivered = 1 AND subtype IN (`+subtypes+`)
		)
	`, args...).Scan(&consumed)
	return consumed, err
}

func (s *stagedInjectionStore) HasNoteForEventSystem(ctx context.Context, orgID, conversationID, producer, eventID string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	subtypes := strings.TrimRight(strings.Repeat("?, ", len(domain.StagedInjectionSubtypes)), ", ")
	args := []any{orgID, conversationID}
	for _, st := range domain.StagedInjectionSubtypes {
		args = append(args, st)
	}
	args = append(args, producer, eventID)
	var told bool
	err := s.q.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM messages
			WHERE org_id = ? AND conversation_id = ?
			  AND role = 'user' AND subtype IN (`+subtypes+`)
			  AND json_extract(metadata, '$.producer') = ? AND json_extract(metadata, '$.event_id') = ?
			  AND (delivered = 1 OR window_state = 'active')
		)
	`, args...).Scan(&told)
	return told, err
}

// stagedInjectionIDLess orders decimal message-row ids numerically (a
// lexical sort would put "10" before "9").
func stagedInjectionIDLess(a, b string) bool {
	ai, _ := strconv.ParseInt(a, 10, 64)
	bi, _ := strconv.ParseInt(b, 10, 64)
	return ai < bi
}
