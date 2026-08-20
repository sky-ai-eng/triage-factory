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

// permissionStore is the Postgres impl of db.PermissionStore. Split-pool like
// artifactStore: ListPending runs on the app pool under RLS (the
// conversation_permissions_select policy composes through the conversation,
// mirroring claims), every write on the admin pool — tf_app holds no
// INSERT/UPDATE grant, and every writer is a delegate goroutine with no
// JWT-claims context to run under anyway.
//
// Nothing writes here in a multi deployment today: multi mints
// runtime='native', which has no permission gate, and sandboxed SDK runs
// auto-approve. The arm exists because the store is one dual-dialect contract
// with one conformance suite.
type permissionStore struct {
	q     queryer
	admin queryer
}

func newPermissionStore(q, admin queryer) db.PermissionStore {
	return &permissionStore{q: q, admin: admin}
}

var _ db.PermissionStore = (*permissionStore)(nil)

// pgPermissionColumns is the SELECT list scanned by scanPGPermissionRow.
// uuids are cast to text so the scan targets stay plain strings, matching
// pgConversationColumns.
const pgPermissionColumns = `
	id::text, org_id::text, conversation_id::text, COALESCE(claim_id::text, ''),
	message_id, tool_call_id, tool_name, input_json, COALESCE(title, ''), state,
	COALESCE(reason, ''), requested_at, expires_at, COALESCE(decided_by::text, ''),
	decided_at, waited_ms`

func (s *permissionStore) Create(ctx context.Context, orgID string, p domain.ConversationPermission) (domain.ConversationPermission, error) {
	if p.State == "" {
		p.State = domain.PermissionStatePending
	}
	if p.State != domain.PermissionStatePending {
		return domain.ConversationPermission{}, fmt.Errorf("postgres permissions Create: state must be %q, got %q", domain.PermissionStatePending, p.State)
	}
	if p.ConversationID == "" || p.ToolCallID == "" || p.ToolName == "" {
		return domain.ConversationPermission{}, errors.New("postgres permissions Create: conversation_id, tool_call_id and tool_name are required")
	}
	if p.ClaimID == "" {
		return domain.ConversationPermission{}, errors.New("postgres permissions Create: claim_id is required on a pending prompt — ListPending derives pending against the conversation's active claim, so a NULL claim would be permanently unreachable (and ExpireForClaim keys on it too, so nothing would ever settle it)")
	}
	if p.RequestedAt.IsZero() {
		p.RequestedAt = time.Now().UTC()
	}
	// A blob that won't marshal fails the insert rather than storing NULL and
	// keeping the row — see the SQLite twin for why that direction is the
	// conservative one (the input IS what the user approves, so no row and a
	// timeout deny beats a row that renders "Allow Bash?" with no command).
	var inputJSON any
	if len(p.Input) > 0 {
		raw, err := json.Marshal(p.Input)
		if err != nil {
			return domain.ConversationPermission{}, fmt.Errorf("postgres permissions Create: marshal input: %w", err)
		}
		inputJSON = string(raw)
	}
	var expires any
	if p.ExpiresAt != nil {
		expires = p.ExpiresAt.UTC()
	}
	// The id is minted DB-side when the caller supplied none (the column's
	// gen_random_uuid default). RETURNING projects the point read's column
	// list, so the row handed back carries that id and the defaulted
	// requested_at — a caller that supplied neither has no other handle to
	// either.
	row, err := scanPGPermissionRow(s.admin.QueryRowContext(ctx, `
		INSERT INTO conversation_permissions (
			id, org_id, conversation_id, claim_id, message_id, tool_call_id,
			tool_name, input_json, title, state, requested_at, expires_at)
		VALUES (COALESCE($1::uuid, gen_random_uuid()), $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING `+pgPermissionColumns, nullUUID(p.ID), orgID, p.ConversationID, nullUUID(p.ClaimID), nullInt64Ptr(p.MessageID),
		p.ToolCallID, p.ToolName, inputJSON, nullIfEmpty(p.Title), p.State,
		p.RequestedAt.UTC(), expires))
	if err != nil {
		return domain.ConversationPermission{}, wrapAdminPoolPermErr(err, "conversation_permissions.Create")
	}
	if row == nil {
		return domain.ConversationPermission{}, sql.ErrNoRows
	}
	return *row, nil
}

func (s *permissionStore) Resolve(ctx context.Context, orgID, conversationID, toolCallID, state, reason, decidedBy string) (*domain.ConversationPermission, error) {
	if !domain.ValidPermissionState(state) || state == domain.PermissionStatePending {
		return nil, fmt.Errorf("postgres permissions Resolve: %q is not a terminal state", state)
	}
	if !domain.ValidPermissionReason(reason) {
		return nil, fmt.Errorf("postgres permissions Resolve: unknown reason %q", reason)
	}
	// Only a pending row is written: the first resolution wins and a racing
	// second one is a no-op (RETURNING hands back no row — nil is the guard
	// declining, not an error), matching the broker's own
	// deregister-then-drain. waited_ms is computed against the STORED
	// requested_at in the same statement, so the number never straddles two
	// clocks.
	row, err := scanPGPermissionRow(s.admin.QueryRowContext(ctx, `
		UPDATE conversation_permissions
		SET state = $1, reason = $2, decided_by = $3, decided_at = $4,
		    waited_ms = GREATEST(0, ROUND(EXTRACT(EPOCH FROM ($4::timestamptz - requested_at)) * 1000))::int
		WHERE org_id = $5 AND conversation_id = $6 AND tool_call_id = $7 AND state = 'pending'
		RETURNING `+pgPermissionColumns, state, nullIfEmpty(reason), nullUUID(decidedBy), time.Now().UTC(),
		orgID, conversationID, toolCallID))
	if err != nil {
		return nil, wrapAdminPoolPermErr(err, "conversation_permissions.Resolve")
	}
	return row, nil
}

func (s *permissionStore) ListPending(ctx context.Context, orgID, conversationID string) ([]domain.ConversationPermission, error) {
	// Pending is derived, not trusted: the claim_id equality against the
	// conversation's live claim is what makes a prompt from a crashed process
	// read as gone without anything having written its expiry. A conversation
	// with no active claim matches nothing (the subquery is NULL, and NULL
	// equality is never true), which is the intended answer.
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+pgPermissionColumns+`
		FROM conversation_permissions
		WHERE org_id = $1 AND conversation_id = $2 AND state = 'pending'
		  AND claim_id = (SELECT id FROM claims
		                  WHERE conversation_id = $2 AND released_at IS NULL)
		ORDER BY requested_at ASC, id ASC
	`, orgID, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPGPermissions(rows)
}

func (s *permissionStore) Get(ctx context.Context, orgID, conversationID, toolCallID string) (*domain.ConversationPermission, error) {
	return scanPGPermissionRow(s.q.QueryRowContext(ctx, `
		SELECT `+pgPermissionColumns+`
		FROM conversation_permissions
		WHERE org_id = $1 AND conversation_id = $2 AND tool_call_id = $3
	`, orgID, conversationID, toolCallID))
}

func (s *permissionStore) ExpireForClaim(ctx context.Context, orgID, claimID string) error {
	if claimID == "" {
		return nil
	}
	_, err := s.admin.ExecContext(ctx, `
		UPDATE conversation_permissions
		SET state = $1, reason = $2, decided_at = $3,
		    waited_ms = GREATEST(0, ROUND(EXTRACT(EPOCH FROM ($3::timestamptz - requested_at)) * 1000))::int
		WHERE org_id = $4 AND claim_id = $5 AND state = 'pending'
	`, domain.PermissionStateExpired, domain.PermissionReasonClaimLost,
		time.Now().UTC(), orgID, claimID)
	if err != nil {
		return wrapAdminPoolPermErr(err, "conversation_permissions.ExpireForClaim")
	}
	return nil
}

// scanPGPermissionRow decodes one conversation_permissions row from the
// shared pgPermissionColumns projection, off either a *sql.Row (a RETURNING
// or point read) or a *sql.Rows mid-iteration. (nil, nil) on sql.ErrNoRows —
// the shape Resolve's guarded UPDATE needs (nil is the guard declining, not
// an error) and ListPending's loop below shares.
func scanPGPermissionRow(row interface{ Scan(dest ...any) error }) (*domain.ConversationPermission, error) {
	var p domain.ConversationPermission
	var messageID, waited sql.NullInt64
	var inputJSON sql.NullString
	var expiresAt, decidedAt sql.NullTime
	err := row.Scan(&p.ID, &p.OrgID, &p.ConversationID, &p.ClaimID, &messageID,
		&p.ToolCallID, &p.ToolName, &inputJSON, &p.Title, &p.State, &p.Reason,
		&p.RequestedAt, &expiresAt, &p.DecidedBy, &decidedAt, &waited)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if messageID.Valid {
		v := messageID.Int64
		p.MessageID = &v
	}
	if inputJSON.Valid && inputJSON.String != "" {
		// Unlike the write side, an unreadable blob HERE keeps the row: the
		// prompt already exists and is parking an agent, so hiding it is the
		// failure this table exists to prevent. Logged rather than swallowed
		// — it means the column was corrupted or written by something other
		// than Create.
		if err := json.Unmarshal([]byte(inputJSON.String), &p.Input); err != nil {
			dbPgLog.Warn("permission input_json unreadable; surfacing the prompt without its input",
				"conversation", p.ConversationID, "tool_call_id", p.ToolCallID, "error", err)
			p.Input = nil
		}
	}
	if expiresAt.Valid {
		t := expiresAt.Time
		p.ExpiresAt = &t
	}
	if decidedAt.Valid {
		t := decidedAt.Time
		p.DecidedAt = &t
	}
	p.WaitedMs = intPtrFromNull(waited)
	return &p, nil
}

func scanPGPermissions(rows *sql.Rows) ([]domain.ConversationPermission, error) {
	var out []domain.ConversationPermission
	for rows.Next() {
		p, err := scanPGPermissionRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}
