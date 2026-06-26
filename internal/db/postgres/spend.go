package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// spendStore is the Postgres impl of db.SpendStore — a read-only aggregation
// over the public.llm_spend view (TFAC-472).
//
// # Pool: app, RLS-active
//
// Wired against the app pool in postgres.New. The view is defined
// WITH (security_invoker = true), so SELECTing it under tf_app evaluates the
// base tables' RLS (runs/curator_requests org+team, system_llm_runs org) as the
// querying user — a team member sees their team's runs but not a sibling team's,
// with system/curator rows visible at org scope. Wiring this to admin would
// bypass that and leak cross-team spend. org_id stays in every WHERE as defense
// in depth alongside RLS.
type spendStore struct{ app queryer }

func newSpendStore(app queryer) db.SpendStore { return &spendStore{app: app} }

var _ db.SpendStore = (*spendStore)(nil)

// spendSelectCols is the view's column list in canonical order, shared by the
// scan helper so the SELECT and Scan can't drift.
const spendSelectCols = `source, source_id, org_id, team_id, category, subtype,
	creator_user_id, actor_agent_id, model, total_cost_usd,
	input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
	occurred_at`

func (s *spendStore) ListSpend(ctx context.Context, orgID string, opts domain.SpendFilter) ([]domain.SpendRow, error) {
	// Build the WHERE incrementally with $N placeholders. org_id is always
	// $1; each optional filter appends its own placeholder so the args slice
	// and the query stay in lock-step.
	var where strings.Builder
	where.WriteString(`WHERE org_id = $1`)
	args := []any{orgID}
	next := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if opts.TeamID != nil {
		where.WriteString(` AND team_id = ` + next(*opts.TeamID))
	}
	if opts.Category != nil {
		where.WriteString(` AND category = ` + next(*opts.Category))
	}
	if !opts.Since.IsZero() {
		where.WriteString(` AND occurred_at >= ` + next(opts.Since))
	}
	if !opts.Until.IsZero() {
		where.WriteString(` AND occurred_at < ` + next(opts.Until))
	}
	query := `SELECT ` + spendSelectCols + ` FROM llm_spend ` + where.String() + ` ORDER BY occurred_at DESC`
	if opts.Limit > 0 {
		query += ` LIMIT ` + next(opts.Limit)
	}

	rows, err := s.app.QueryContext(ctx, query, args...)
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

func (s *spendStore) SpendByCategory(ctx context.Context, orgID string, since time.Time) ([]domain.SpendBucket, error) {
	rows, err := s.app.QueryContext(ctx, `
		SELECT category,
		       SUM(total_cost_usd)::double precision AS cost,
		       SUM(input_tokens)          AS input_tokens,
		       SUM(output_tokens)         AS output_tokens,
		       SUM(cache_read_tokens)     AS cache_read_tokens,
		       SUM(cache_creation_tokens) AS cache_creation_tokens
		FROM llm_spend
		WHERE org_id = $1 AND occurred_at >= $2
		GROUP BY category
		ORDER BY category
	`, orgID, since)
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

// scanSpendRow decodes one llm_spend row in spendSelectCols order. The five
// nullable columns (team_id, subtype, creator_user_id, actor_agent_id, model)
// scan through sql.NullString → *string; uuid columns scan cleanly into string
// via the pgx stdlib driver.
func scanSpendRow(scan func(dst ...any) error) (domain.SpendRow, error) {
	var r domain.SpendRow
	var teamID, subtype, creatorUserID, actorAgentID, model sql.NullString
	if err := scan(
		&r.Source, &r.SourceID, &r.OrgID, &teamID, &r.Category, &subtype,
		&creatorUserID, &actorAgentID, &model, &r.TotalCostUSD,
		&r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CacheCreationTokens,
		&r.OccurredAt,
	); err != nil {
		return r, err
	}
	r.TeamID = nullStringToPtr(teamID)
	r.Subtype = nullStringToPtr(subtype)
	r.CreatorUserID = nullStringToPtr(creatorUserID)
	r.ActorAgentID = nullStringToPtr(actorAgentID)
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
