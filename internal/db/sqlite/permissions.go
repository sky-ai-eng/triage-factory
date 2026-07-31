package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// permissionStore is the SQLite impl of db.PermissionStore. This is the arm
// production actually uses: the browser permission round-trip is reached only
// by an unsandboxed local SDK run (multi mints runtime='native', which has no
// permission gate, and sandboxed SDK runs auto-approve).
type permissionStore struct{ q queryer }

func newPermissionStore(q queryer) db.PermissionStore {
	return &permissionStore{q: q}
}

var _ db.PermissionStore = (*permissionStore)(nil)

// permissionColumns is the SELECT list scanned by scanPermissions.
const permissionColumns = `
	id, org_id, conversation_id, COALESCE(claim_id, ''), message_id,
	tool_call_id, tool_name, input_json, COALESCE(title, ''), state,
	COALESCE(reason, ''), requested_at, expires_at, COALESCE(decided_by, ''),
	decided_at, waited_ms`

func (s *permissionStore) Create(ctx context.Context, orgID string, p *domain.ConversationPermission) error {
	if p == nil {
		return errors.New("sqlite permissions Create: nil permission")
	}
	if p.State == "" {
		p.State = domain.PermissionStatePending
	}
	if p.State != domain.PermissionStatePending {
		return fmt.Errorf("sqlite permissions Create: state must be %q, got %q", domain.PermissionStatePending, p.State)
	}
	if p.ConversationID == "" || p.ToolCallID == "" || p.ToolName == "" {
		return errors.New("sqlite permissions Create: conversation_id, tool_call_id and tool_name are required")
	}
	if p.ID == "" {
		p.ID = uuid.New().String()
	}
	if p.RequestedAt.IsZero() {
		p.RequestedAt = time.Now().UTC()
	}
	// The input is stored as the JSON the agent asked to run with, so a
	// reconstructed prompt shows the real call rather than the SDK's rendered
	// sentence.
	//
	// A blob that won't marshal fails the insert rather than storing NULL and
	// keeping the row, and the direction is deliberate. The input IS what the
	// user approves — a prompt that renders "Allow Bash?" with no command
	// invites a blind yes — so refusing to write a row we can't describe means
	// the prompt never surfaces and the agent takes the timeout deny instead.
	// That fails closed: the tool doesn't run. Storing the half-row fails open.
	// The branch is unreachable in practice either way (Input is decoded from
	// the wrapper's own JSON, so it is marshalable by construction), which is
	// exactly why it should take the conservative side rather than earn a
	// degradation path.
	var inputJSON any
	if len(p.Input) > 0 {
		raw, err := json.Marshal(p.Input)
		if err != nil {
			return fmt.Errorf("sqlite permissions Create: marshal input: %w", err)
		}
		inputJSON = string(raw)
	}
	var expires any
	if p.ExpiresAt != nil {
		expires = p.ExpiresAt.UTC()
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO conversation_permissions (
			id, org_id, conversation_id, claim_id, message_id, tool_call_id,
			tool_name, input_json, title, state, requested_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, p.ID, orgID, p.ConversationID, nullIfEmpty(p.ClaimID), sqliteNullInt64(p.MessageID),
		p.ToolCallID, p.ToolName, inputJSON, nullIfEmpty(p.Title), p.State,
		p.RequestedAt.UTC(), expires)
	return err
}

func (s *permissionStore) Resolve(ctx context.Context, orgID, conversationID, toolCallID, state, reason, decidedBy string) error {
	if !domain.ValidPermissionState(state) || state == domain.PermissionStatePending {
		return fmt.Errorf("sqlite permissions Resolve: %q is not a terminal state", state)
	}
	if !domain.ValidPermissionReason(reason) {
		return fmt.Errorf("sqlite permissions Resolve: unknown reason %q", reason)
	}
	now := time.Now().UTC()
	// The wait is computed in Go rather than in the UPDATE because SQLite
	// can't do the arithmetic here: the driver stores a time.Time in Go's own
	// String() layout, which julianday()/strftime() don't parse — they'd
	// silently yield NULL rather than erroring. Reading the stored
	// requested_at back and subtracting keeps the measurement anchored to the
	// row's own timestamp, which is the property that matters.
	requestedAt, ok, err := s.pendingRequestedAt(ctx, orgID, conversationID, toolCallID)
	if err != nil || !ok {
		// Not pending — already resolved, or never recorded. Not an error:
		// this is a best-effort record alongside a broker that has already
		// decided.
		return err
	}
	// Only a pending row is written: the first resolution wins and a racing
	// second one is a no-op, matching the broker's own deregister-then-drain
	// (a decision that lands in the same instant a deadline fires is honored by
	// one path and dropped by the other) — which is also why losing the read
	// above to a racing resolve is harmless, since this UPDATE then matches
	// nothing.
	_, err = s.q.ExecContext(ctx, `
		UPDATE conversation_permissions
		SET state = ?, reason = ?, decided_by = ?, decided_at = ?, waited_ms = ?
		WHERE org_id = ? AND conversation_id = ? AND tool_call_id = ? AND state = 'pending'
	`, state, nullIfEmpty(reason), nullIfEmpty(decidedBy), now, waitedMs(requestedAt, now),
		orgID, conversationID, toolCallID)
	return err
}

// pendingRequestedAt reads one still-pending prompt's requested_at.
// ok=false means there is nothing pending to resolve.
func (s *permissionStore) pendingRequestedAt(ctx context.Context, orgID, conversationID, toolCallID string) (time.Time, bool, error) {
	var requestedAt time.Time
	err := s.q.QueryRowContext(ctx, `
		SELECT requested_at FROM conversation_permissions
		WHERE org_id = ? AND conversation_id = ? AND tool_call_id = ? AND state = 'pending'
	`, orgID, conversationID, toolCallID).Scan(&requestedAt)
	if err == sql.ErrNoRows {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return requestedAt, true, nil
}

// waitedMs is how long a prompt stood open, floored at zero: a clock that
// stepped backwards between the two stamps is a measurement problem, and a
// negative wait would read as a decision made before the question was asked.
func waitedMs(requestedAt, decidedAt time.Time) int64 {
	ms := decidedAt.Sub(requestedAt).Milliseconds()
	if ms < 0 {
		return 0
	}
	return ms
}

func (s *permissionStore) ListPending(ctx context.Context, orgID, conversationID string) ([]domain.ConversationPermission, error) {
	// Pending is derived, not trusted: the claim_id equality against the
	// conversation's live claim is what makes a prompt from a crashed process
	// read as gone without anything having written its expiry. A conversation
	// with no active claim matches nothing (the subquery is NULL, and NULL
	// equality is never true), which is the intended answer.
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+permissionColumns+`
		FROM conversation_permissions
		WHERE org_id = ? AND conversation_id = ? AND state = 'pending'
		  AND claim_id = (SELECT id FROM claims
		                  WHERE conversation_id = ? AND released_at IS NULL)
		ORDER BY requested_at ASC, id ASC
	`, orgID, conversationID, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPermissions(rows)
}

func (s *permissionStore) ExpireForClaim(ctx context.Context, orgID, claimID string) error {
	if claimID == "" {
		return nil
	}
	now := time.Now().UTC()
	// Row-at-a-time for the same reason Resolve is: the per-row wait can't be
	// computed in SQL against a Go-formatted timestamp. A released engagement
	// has at most a handful of open prompts (usually zero — this is the tidy-up
	// path, and the read that matters derives the same answer from the claim
	// without it), so the loop costs nothing.
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, requested_at FROM conversation_permissions
		WHERE org_id = ? AND claim_id = ? AND state = 'pending'
	`, orgID, claimID)
	if err != nil {
		return err
	}
	type openPrompt struct {
		id          string
		requestedAt time.Time
	}
	var open []openPrompt
	for rows.Next() {
		var p openPrompt
		if err := rows.Scan(&p.id, &p.requestedAt); err != nil {
			rows.Close()
			return err
		}
		open = append(open, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range open {
		if _, err := s.q.ExecContext(ctx, `
			UPDATE conversation_permissions
			SET state = ?, reason = ?, decided_at = ?, waited_ms = ?
			WHERE id = ? AND state = 'pending'
		`, domain.PermissionStateExpired, domain.PermissionReasonClaimLost,
			now, waitedMs(p.requestedAt, now), p.id); err != nil {
			return err
		}
	}
	return nil
}

func scanPermissions(rows *sql.Rows) ([]domain.ConversationPermission, error) {
	var out []domain.ConversationPermission
	for rows.Next() {
		var p domain.ConversationPermission
		var messageID, waited sql.NullInt64
		var inputJSON sql.NullString
		var expiresAt, decidedAt sql.NullTime
		if err := rows.Scan(&p.ID, &p.OrgID, &p.ConversationID, &p.ClaimID, &messageID,
			&p.ToolCallID, &p.ToolName, &inputJSON, &p.Title, &p.State, &p.Reason,
			&p.RequestedAt, &expiresAt, &p.DecidedBy, &decidedAt, &waited); err != nil {
			return nil, err
		}
		if messageID.Valid {
			v := messageID.Int64
			p.MessageID = &v
		}
		if inputJSON.Valid && inputJSON.String != "" {
			// Unlike the write side, an unreadable blob HERE keeps the row: the
			// prompt already exists and is parking an agent, so hiding it is the
			// failure this table exists to prevent. It can only happen if the
			// column was corrupted or written by something other than Create, so
			// it's logged rather than swallowed.
			if err := json.Unmarshal([]byte(inputJSON.String), &p.Input); err != nil {
				dbLog.Warn("permission input_json unreadable; surfacing the prompt without its input",
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
		out = append(out, p)
	}
	return out, rows.Err()
}
