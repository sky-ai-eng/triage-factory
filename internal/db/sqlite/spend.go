package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// spendStore is the SQLite impl of db.SpendStore — a read-only aggregation over
// the llm_spend view (TFAC-472), the UNION-ALL of conversation messages +
// system_llm_runs on the category axis. SQLite is N=1 (local mode) with no RLS,
// so unlike the Postgres impl there is no app/admin pool split — the single
// connection serves the read. assertLocalOrg pins orgID to the local sentinel.
//
// Datetime filters wrap both sides in datetime() (the codebase idiom, see
// conversation.go) so a bound time.Time compares correctly against occurred_at
// regardless of whether the underlying started_at/created_at was written by a
// Go time.Time bind or a SQL CURRENT_TIMESTAMP default.
type spendStore struct{ q queryer }

func newSpendStore(q queryer) db.SpendStore { return &spendStore{q: q} }

var _ db.SpendStore = (*spendStore)(nil)

// spendSelectCols is the view's column list in canonical order, shared by the
// scan helper so the SELECT and Scan can't drift.
const spendSelectCols = `source, source_id, org_id, team_id, category, subtype,
	creator_user_id, actor_agent_id, trigger_id, model, total_cost_usd,
	input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
	occurred_at`

// ListSpendSystem mirrors the Postgres admin-pool variant the role-gated team /
// org usage reads use (TFAC-478). SQLite is N=1 / no RLS, so the cross-team read
// is identical to ListSpend — delegate straight through.
func (s *spendStore) ListSpendSystem(ctx context.Context, orgID string, opts domain.SpendFilter) ([]domain.SpendRow, error) {
	return s.ListSpend(ctx, orgID, opts)
}

func (s *spendStore) ListSpend(ctx context.Context, orgID string, opts domain.SpendFilter) ([]domain.SpendRow, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	var where strings.Builder
	where.WriteString(`WHERE org_id = ?`)
	args := []any{orgID}
	if opts.TeamID != nil {
		where.WriteString(` AND team_id = ?`)
		args = append(args, *opts.TeamID)
	}
	if opts.CreatorUserID != nil {
		where.WriteString(` AND creator_user_id = ?`)
		args = append(args, *opts.CreatorUserID)
	}
	if opts.Category != nil {
		where.WriteString(` AND category = ?`)
		args = append(args, *opts.Category)
	}
	if !opts.Since.IsZero() {
		where.WriteString(` AND datetime(occurred_at) >= datetime(?)`)
		args = append(args, opts.Since)
	}
	if !opts.Until.IsZero() {
		where.WriteString(` AND datetime(occurred_at) < datetime(?)`)
		args = append(args, opts.Until)
	}
	// ORDER BY datetime(occurred_at), not the raw column: occurred_at unions
	// started_at/created_at that may have been written either as a Go time.Time
	// or a SQL CURRENT_TIMESTAMP default (different text shapes), so normalize
	// before sorting — same reason the WHERE filters wrap both sides.
	query := `SELECT ` + spendSelectCols + ` FROM llm_spend ` + where.String() + ` ORDER BY datetime(occurred_at) DESC`
	if opts.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, opts.Limit)
	}

	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list spend: %w", err)
	}
	defer rows.Close()

	out := []domain.SpendRow{}
	for rows.Next() {
		r, err := scanSpendRow(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SpendByCategorySystem mirrors the Postgres admin-pool variant the TFAC-477
// safety cap reads. SQLite is N=1 / no RLS, so the org-wide aggregate is
// identical to SpendByCategory — delegate straight through.
func (s *spendStore) SpendByCategorySystem(ctx context.Context, orgID string, since, until time.Time) ([]domain.SpendBucket, error) {
	return s.SpendByCategory(ctx, orgID, since, until)
}

// SpendByCategorySystemForTeam mirrors the Postgres admin-pool variant the
// TFAC-482 per-team cap reads — SpendByCategorySystem narrowed to one team. The
// team_id filter excludes the org's NULL-team rows (system overhead).
// SQLite is N=1 / no RLS, so it's the same single-connection read with
// the team predicate applied.
func (s *spendStore) SpendByCategorySystemForTeam(ctx context.Context, orgID, teamID string, since, until time.Time) ([]domain.SpendBucket, error) {
	return s.spendByCategory(ctx, orgID, teamID, since, until)
}

func (s *spendStore) SpendTotalSystem(ctx context.Context, since, until time.Time) (float64, error) {
	// N=1: one org, so a cross-org total needs no org filter (and would be
	// wrong to pin to the local sentinel — there is nothing else to sum).
	var where strings.Builder
	where.WriteString(`WHERE 1=1`)
	args := []any{}
	if !since.IsZero() {
		where.WriteString(` AND datetime(occurred_at) >= datetime(?)`)
		args = append(args, since)
	}
	if !until.IsZero() {
		where.WriteString(` AND datetime(occurred_at) < datetime(?)`)
		args = append(args, until)
	}
	var total sql.NullFloat64
	err := s.q.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(total_cost_usd), 0) FROM llm_spend `+where.String(), args...).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("spend total: %w", err)
	}
	return total.Float64, nil
}

func (s *spendStore) SpendByCategory(ctx context.Context, orgID string, since, until time.Time) ([]domain.SpendBucket, error) {
	return s.spendByCategory(ctx, orgID, "", since, until)
}

// spendByCategory is the shared aggregate. A non-empty teamID adds an `AND
// team_id = teamID` filter (the per-team cap path); "" leaves it org-wide.
func (s *spendStore) spendByCategory(ctx context.Context, orgID, teamID string, since, until time.Time) ([]domain.SpendBucket, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	// Both bounds optional (see SpendFilter): drop the clause on a zero time.
	// datetime() normalizes both sides, same as ListSpend. SQLite REAL is always
	// 8-byte, so SUM accumulates in double precision natively — no cast needed
	// (the Postgres twin casts the addends to float8 to match this).
	var where strings.Builder
	where.WriteString(`WHERE org_id = ?`)
	args := []any{orgID}
	if teamID != "" {
		where.WriteString(` AND team_id = ?`)
		args = append(args, teamID)
	}
	if !since.IsZero() {
		where.WriteString(` AND datetime(occurred_at) >= datetime(?)`)
		args = append(args, since)
	}
	if !until.IsZero() {
		where.WriteString(` AND datetime(occurred_at) < datetime(?)`)
		args = append(args, until)
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT category,
		       SUM(total_cost_usd)        AS cost,
		       SUM(input_tokens)          AS input_tokens,
		       SUM(output_tokens)         AS output_tokens,
		       SUM(cache_read_tokens)     AS cache_read_tokens,
		       SUM(cache_creation_tokens) AS cache_creation_tokens
		FROM llm_spend `+where.String()+`
		GROUP BY category
		ORDER BY category
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("spend by category: %w", err)
	}
	defer rows.Close()

	out := []domain.SpendBucket{}
	for rows.Next() {
		var b domain.SpendBucket
		if err := rows.Scan(&b.Category, &b.TotalCostUSD,
			&b.InputTokens, &b.OutputTokens, &b.CacheReadTokens, &b.CacheCreationTokens); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// scanSpendRow decodes one llm_spend row in spendSelectCols order. The six
// nullable columns (team_id, subtype, creator_user_id, actor_agent_id,
// trigger_id, model) scan through sql.NullString → *string.
func scanSpendRow(scan func(dst ...any) error) (domain.SpendRow, error) {
	var r domain.SpendRow
	var teamID, subtype, creatorUserID, actorAgentID, triggerID, model sql.NullString
	if err := scan(
		&r.Source, &r.SourceID, &r.OrgID, &teamID, &r.Category, &subtype,
		&creatorUserID, &actorAgentID, &triggerID, &model, &r.TotalCostUSD,
		&r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CacheCreationTokens,
		&r.OccurredAt,
	); err != nil {
		return r, err
	}
	r.TeamID = nullStringToPtr(teamID)
	r.Subtype = nullStringToPtr(subtype)
	r.CreatorUserID = nullStringToPtr(creatorUserID)
	r.ActorAgentID = nullStringToPtr(actorAgentID)
	r.TriggerID = nullStringToPtr(triggerID)
	r.Model = nullStringToPtr(model)
	return r, nil
}

func nullStringToPtr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	v := n.String
	return &v
}
