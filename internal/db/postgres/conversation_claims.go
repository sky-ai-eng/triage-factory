package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// pgClaimColumns is the projection of one claims row into domain.Claim, in the
// order db.ScanClaim reads it. The point read and the RETURNING of the claim
// writes that answer with a domain.Claim both use it, so the write shape
// cannot drift from the read shape. cred_pubkey is left out: Claim never
// serializes it.
const pgClaimColumns = `id::text, org_id::text, conversation_id::text, executor_id, boot_epoch,
	COALESCE(phase, ''), claimed_at, released_at, COALESCE(outcome, ''), COALESCE(error, ''),
	duration_ms, num_turns, plugins_loaded::text, created_at`

// RecordClaimPluginsLoadedSystem is keyed on the claim id alone (org bound as
// defense in depth) with no released_at predicate: the wrapper's report can
// arrive as the engagement ends, and a later report on the same engagement
// replaces an earlier one.
func (s *conversationStore) RecordClaimPluginsLoadedSystem(ctx context.Context, orgID, claimID string, report domain.PluginsLoaded) (*domain.Claim, error) {
	// A non-uuid id names no claim; Postgres would reject the bind with a
	// 22P02 for what is really a miss.
	if _, err := uuid.Parse(claimID); err != nil {
		return nil, nil
	}
	encoded, err := db.MarshalPluginsLoadedColumn(report)
	if err != nil {
		return nil, fmt.Errorf("marshal claims.plugins_loaded: %w", err)
	}
	c, err := db.ScanClaim(s.admin.QueryRowContext(ctx, `
		UPDATE claims SET plugins_loaded = $1::jsonb
		WHERE org_id = $2 AND id = $3
		RETURNING `+pgClaimColumns, encoded, orgID, claimID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("record claim plugins_loaded: %w", err)
	}
	return &c, nil
}

func (s *conversationStore) ListClaimsForConversation(ctx context.Context, orgID, conversationID string) ([]domain.Claim, error) {
	if _, err := uuid.Parse(conversationID); err != nil {
		return []domain.Claim{}, nil
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+pgClaimColumns+`
		FROM claims WHERE org_id = $1 AND conversation_id = $2
		ORDER BY claimed_at, id
	`, orgID, conversationID)
	if err != nil {
		return nil, fmt.Errorf("list claims for conversation: %w", err)
	}
	defer rows.Close()
	out := []domain.Claim{}
	for rows.Next() {
		c, err := db.ScanClaim(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan claim: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
