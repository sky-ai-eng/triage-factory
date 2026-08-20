package postgres

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// stagedInjectionStore is the Postgres impl of db.StagedInjectionStore — the
// durable "stage for next resume" agent-injection queue (TFAC-501), backed
// by undelivered messages rows (role='user',
// subtype='injection:system-note') rather than a side table — one transcript
// canon. Wired against the ADMIN (BYPASSRLS) pool in postgres.New: both the
// producer (an eventbus subscriber) and the consumer (a detached resume
// goroutine) run without JWT claims, so an app-pool/RLS statement would see
// nothing. org_id stays bound in every WHERE/INSERT clause as defense in
// depth.
type stagedInjectionStore struct{ admin queryer }

func newStagedInjectionStore(admin queryer) db.StagedInjectionStore {
	return &stagedInjectionStore{admin: admin}
}

var _ db.StagedInjectionStore = (*stagedInjectionStore)(nil)

// stagedInjectionSubtype discriminates the staged-note rows from every other
// undelivered user message (the resume pending-input row keeps subtype
// 'text').
const stagedInjectionSubtype = "injection:system-note"

func (s *stagedInjectionStore) AppendSystem(ctx context.Context, orgID string, n domain.StagedInjection) (domain.StagedInjection, error) {
	meta, err := json.Marshal(map[string]string{"producer": n.Producer})
	if err != nil {
		return domain.StagedInjection{}, err
	}
	// created_at is left to the column DEFAULT now(). RETURNING projects the
	// same columns flushPendingInput's SELECT does, so the freshly-inserted
	// row and a later flush of it map through the same stagedInjectionFromPending.
	var r pendingRow
	if err := s.admin.QueryRowContext(ctx, `
		INSERT INTO messages (org_id, conversation_id, role, content, subtype, metadata, delivered)
		VALUES ($1::uuid, $2::uuid, 'user', $3, $4, $5::jsonb, false)
		RETURNING id, org_id::text, conversation_id::text, content,
		          COALESCE(user_id::text, ''), COALESCE(metadata::text, ''), created_at
	`, orgID, n.ConversationID, n.Body, stagedInjectionSubtype, string(meta)).Scan(
		&r.ID, &r.OrgID, &r.ConvID, &r.Content, &r.UserID, &r.Metadata, &r.CreatedAt,
	); err != nil {
		return domain.StagedInjection{}, err
	}
	return stagedInjectionFromPending(r), nil
}

func (s *stagedInjectionStore) FlushPendingSystem(ctx context.Context, orgID, conversationID string) ([]domain.StagedInjection, error) {
	// The shared flush primitive, narrowed to this store's subtype: one
	// definition of "consume a conversation's pending rows" serves every
	// producer, so a note is delivered exactly once even under a resume race
	// and withdrawn rows stay out by the same rule the claim predicate uses.
	flushed, err := flushPendingInput(ctx, s.admin, orgID, conversationID, stagedInjectionSubtype)
	if err != nil {
		return nil, err
	}
	return stagedInjectionsFromPending(flushed), nil
}

func (s *stagedInjectionStore) DeleteSystem(ctx context.Context, orgID, id string) error {
	rowID, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		// A non-numeric id can only be a pre-refactor uuid that no longer
		// names any row — the contract is a silent no-op.
		return nil
	}
	// Retire, don't delete (see the interface doc): the executor's tf_system
	// role holds no DELETE on messages. delivered stays false — withdrawn
	// means the note never happened, and a delivered stamp would make it read
	// as transcript history; window_state='inactive' is what keeps it out of
	// the flush, assembly, and the display reads. The delivered = false guard
	// keeps this from retiring an already-flushed row a racing resume just
	// consumed.
	_, err = s.admin.ExecContext(ctx, `
		UPDATE messages SET window_state = 'inactive'
		WHERE org_id = $1::uuid AND id = $2 AND subtype = $3 AND delivered = false
	`, orgID, rowID, stagedInjectionSubtype)
	return err
}

// stagedInjectionFromPending maps one messages row (whether just inserted by
// AppendSystem or claimed by a flush) onto the StagedInjection shape: the row
// id becomes the decimal ID, content the Body, and the producer tag is
// unpacked from metadata. The two writers share this mapper so a fresh
// AppendSystem row and that same row read back by a later flush can never
// disagree about what a staged injection is.
func stagedInjectionFromPending(r pendingRow) domain.StagedInjection {
	n := domain.StagedInjection{
		ID:             strconv.FormatInt(r.ID, 10),
		ConversationID: r.ConvID,
		OrgID:          r.OrgID,
		Body:           r.Content,
		CreatedAt:      r.CreatedAt,
	}
	if r.Metadata != "" {
		var meta struct {
			Producer string `json:"producer"`
		}
		_ = json.Unmarshal([]byte(r.Metadata), &meta)
		n.Producer = meta.Producer
	}
	return n
}

// stagedInjectionsFromPending maps flushed pending rows onto the
// StagedInjection shape via stagedInjectionFromPending.
func stagedInjectionsFromPending(flushed []pendingRow) []domain.StagedInjection {
	var out []domain.StagedInjection
	for _, r := range flushed {
		out = append(out, stagedInjectionFromPending(r))
	}
	return out
}
