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
// # Pools: app (RLS-active) + admin (BYPASSRLS)
//
//   - app: ListSpend + SpendByCategory. The view is defined
//     WITH (security_invoker = true), so SELECTing it under tf_app evaluates the
//     base tables' RLS (runs/curator_requests org+team, system_llm_runs org) as
//     the querying user — a team member sees their team's runs but not a sibling
//     team's, with system/curator rows visible at org scope.
//   - admin: SpendByCategorySystem. The org-wide aggregate a claims-less system
//     caller needs — the TFAC-477 safety cap reads it from a Spawner.Delegate
//     goroutine with no tf.current_org_id(), where an app-pool read would see
//     nothing. Mirrors the app/admin split on artifactStore (ListByRun /
//     ListByRunSystem).
//
// org_id stays in every WHERE as defense in depth alongside RLS on both pools.
type spendStore struct {
	app   queryer
	admin queryer
}

func newSpendStore(app, admin queryer) db.SpendStore { return &spendStore{app: app, admin: admin} }

var _ db.SpendStore = (*spendStore)(nil)

// spendSelectCols is the view's column list in canonical order, shared by the
// scan helper so the SELECT and Scan can't drift.
const spendSelectCols = `source, source_id, org_id, team_id, category, subtype,
	creator_user_id, actor_agent_id, trigger_id, model, total_cost_usd,
	input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
	occurred_at`

func (s *spendStore) ListSpend(ctx context.Context, orgID string, opts domain.SpendFilter) ([]domain.SpendRow, error) {
	return listSpend(ctx, s.app, orgID, opts)
}

// ListSpendSystem runs listSpend on the admin pool (BYPASSRLS) so a role-gated
// team / org usage read (TFAC-478) sees spend across teams the caller may not
// belong to. org_id stays in the WHERE as defense in depth. Mirrors
// SpendByCategorySystem / ListByRunSystem.
func (s *spendStore) ListSpendSystem(ctx context.Context, orgID string, opts domain.SpendFilter) ([]domain.SpendRow, error) {
	return listSpend(ctx, s.admin, orgID, opts)
}

// listSpend is the shared row read the app- and admin-pool variants both run;
// q selects which pool (and thus whether RLS applies).
func listSpend(ctx context.Context, q queryer, orgID string, opts domain.SpendFilter) ([]domain.SpendRow, error) {
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
	if opts.CreatorUserID != nil {
		where.WriteString(` AND creator_user_id = ` + next(*opts.CreatorUserID))
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

	rows, err := q.QueryContext(ctx, query, args...)
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

func (s *spendStore) SpendByCategory(ctx context.Context, orgID string, since, until time.Time) ([]domain.SpendBucket, error) {
	return spendByCategory(ctx, s.app, orgID, "", since, until)
}

// SpendByCategorySystem runs SpendByCategory's aggregate on the admin pool
// (BYPASSRLS) so a claims-less system caller (the TFAC-477 safety cap, in a
// Spawner.Delegate goroutine) sees org-wide spend across every team. org_id
// stays in the WHERE as defense in depth. Mirrors ListByRunSystem.
func (s *spendStore) SpendByCategorySystem(ctx context.Context, orgID string, since, until time.Time) ([]domain.SpendBucket, error) {
	return spendByCategory(ctx, s.admin, orgID, "", since, until)
}

// SpendByCategorySystemForTeam narrows SpendByCategorySystem to one team with an
// `AND team_id = $teamID` predicate (TFAC-482 per-team daily spend cap). The team
// filter automatically excludes the org's NULL-team rows (system overhead +
// non-team curator), so a team cap counts only the team's own spend. Admin pool
// for the same reason as SpendByCategorySystem — the cap reads it claims-less.
func (s *spendStore) SpendByCategorySystemForTeam(ctx context.Context, orgID, teamID string, since, until time.Time) ([]domain.SpendBucket, error) {
	return spendByCategory(ctx, s.admin, orgID, teamID, since, until)
}

func (s *spendStore) SpendTotalSystem(ctx context.Context, since, until time.Time) (float64, error) {
	var where strings.Builder
	args := []any{}
	next := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if !since.IsZero() {
		where.WriteString(` AND occurred_at >= ` + next(since))
	}
	if !until.IsZero() {
		where.WriteString(` AND occurred_at < ` + next(until))
	}
	// COALESCE so no rows sums to 0, not NULL; double-precision accumulation
	// matches SpendByCategorySystem's reconciliation rationale.
	var total sql.NullFloat64
	err := s.admin.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(total_cost_usd::double precision), 0)
		FROM llm_spend WHERE 1=1`+where.String(), args...).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("spend total: %w", err)
	}
	return total.Float64, nil
}

// spendByCategory is the shared aggregate the app- and admin-pool variants both
// run; q selects which pool (and thus whether RLS applies). A non-empty teamID
// adds an `AND team_id = teamID` filter (the per-team cap path); "" leaves the
// aggregate org-wide.
func spendByCategory(ctx context.Context, q queryer, orgID, teamID string, since, until time.Time) ([]domain.SpendBucket, error) {
	// Both bounds optional (see SpendFilter): drop the clause on a zero time.
	var where strings.Builder
	where.WriteString(`WHERE org_id = $1`)
	args := []any{orgID}
	next := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if teamID != "" {
		where.WriteString(` AND team_id = ` + next(teamID))
	}
	if !since.IsZero() {
		where.WriteString(` AND occurred_at >= ` + next(since))
	}
	if !until.IsZero() {
		where.WriteString(` AND occurred_at < ` + next(until))
	}
	// SUM(total_cost_usd::double precision), not SUM(total_cost_usd): in PG,
	// total_cost_usd is `real` (float4) and SUM(real) both accumulates AND
	// returns real, so casting each addend to double precision first makes the
	// accumulation happen in float8 — matching SQLite (whose REAL is always
	// 8-byte) and minimizing rounding drift when many rows are summed for bill
	// reconciliation. (Casting the SUM *result* instead would widen an
	// already-float4-rounded value and buy nothing.)
	rows, err := q.QueryContext(ctx, `
		SELECT category,
		       SUM(total_cost_usd::double precision) AS cost,
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
// trigger_id, model) scan through sql.NullString → *string; uuid columns scan
// cleanly into string via the pgx stdlib driver.
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
