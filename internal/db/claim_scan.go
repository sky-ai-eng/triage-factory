package db

import (
	"database/sql"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ScanClaim decodes one claims row, projected as the dialect stores'
// claim columns, into domain.Claim:
//
//	id, org_id, conversation_id, executor_id, boot_epoch, phase, claimed_at,
//	released_at, outcome, error, duration_ms, num_turns, plugins_loaded,
//	created_at
//
// phase, outcome and error arrive as empty strings when NULL and plugins_loaded as JSON
// text, so the two dialects differ only in the SQL that produces them.
func ScanClaim(scan func(dest ...any) error) (domain.Claim, error) {
	var (
		c                    domain.Claim
		releasedAt           sql.NullTime
		durationMS, numTurns sql.NullInt64
		pluginsLoaded        sql.NullString
	)
	if err := scan(
		&c.ID, &c.OrgID, &c.ConversationID, &c.ExecutorID, &c.BootEpoch,
		&c.Phase, &c.ClaimedAt, &releasedAt, &c.Outcome, &c.Error,
		&durationMS, &numTurns, &pluginsLoaded, &c.CreatedAt,
	); err != nil {
		return domain.Claim{}, err
	}
	if releasedAt.Valid {
		v := releasedAt.Time
		c.ReleasedAt = &v
	}
	if durationMS.Valid {
		v := int(durationMS.Int64)
		c.DurationMs = &v
	}
	if numTurns.Valid {
		v := int(numTurns.Int64)
		c.NumTurns = &v
	}
	loaded, err := UnmarshalPluginsLoadedColumn(pluginsLoaded, "claims.plugins_loaded")
	if err != nil {
		return domain.Claim{}, err
	}
	c.PluginsLoaded = loaded
	return c, nil
}
