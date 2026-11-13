package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// sqliteClaimColumns is the projection of one claims row into domain.Claim,
// in the order db.ScanClaim reads it. The point read and the RETURNING of the
// claim writes that answer with a domain.Claim both use it, so the write shape
// cannot drift from the read shape. cred_pubkey is left out: Claim never
// serializes it.
const sqliteClaimColumns = `id, org_id, conversation_id, executor_id, boot_epoch,
	COALESCE(phase, ''), claimed_at, released_at, COALESCE(outcome, ''), COALESCE(error, ''),
	duration_ms, num_turns, plugins_loaded, created_at`

// RecordClaimPluginsLoadedSystem is keyed on the claim id alone, with no
// released_at predicate: the wrapper's report can arrive as the engagement
// ends, and a later report on the same engagement replaces an earlier one.
func (s *conversationStore) RecordClaimPluginsLoadedSystem(ctx context.Context, orgID, claimID string, report domain.PluginsLoaded) (*domain.Claim, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	encoded, err := db.MarshalPluginsLoadedColumn(report)
	if err != nil {
		return nil, fmt.Errorf("marshal claims.plugins_loaded: %w", err)
	}
	c, err := db.ScanClaim(s.q.QueryRowContext(ctx, `
		UPDATE claims SET plugins_loaded = ? WHERE id = ?
		RETURNING `+sqliteClaimColumns, encoded, claimID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("record claim plugins_loaded: %w", err)
	}
	return &c, nil
}

func (s *conversationStore) ListClaimsForConversation(ctx context.Context, orgID, conversationID string) ([]domain.Claim, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+sqliteClaimColumns+`
		FROM claims WHERE conversation_id = ?
		ORDER BY claimed_at, id
	`, conversationID)
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
