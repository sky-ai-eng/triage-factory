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
// the llm_spend view (TFAC-472), the UNION-ALL of runs + curator_requests +
// system_llm_runs on the category axis. SQLite is N=1 (local mode) with no RLS,
// so unlike the Postgres impl there is no app/admin pool split — the single
// connection serves the read. assertLocalOrg pins orgID to the local sentinel.
//
// Datetime filters wrap both sides in datetime() (the codebase idiom, see
// agentrun.go) so a bound time.Time compares correctly against occurred_at
// regardless of whether the underlying started_at/created_at was written by a
// Go time.Time bind or a SQL CURRENT_TIMESTAMP default.
type spendStore struct{ q queryer }

func newSpendStore(q queryer) db.SpendStore { return &spendStore{q: q} }

var _ db.SpendStore = (*spendStore)(nil)

// spendSelectCols is the view's column list in canonical order, shared by the
// scan helper so the SELECT and Scan can't drift.
const spendSelectCols = `source, source_id, org_id, team_id, category, subtype,
	creator_user_id, actor_agent_id, model, total_cost_usd,
	input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
	occurred_at`

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

func (s *spendStore) SpendByCategory(ctx context.Context, orgID string, since time.Time) ([]domain.SpendBucket, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT category,
		       SUM(total_cost_usd)        AS cost,
		       SUM(input_tokens)          AS input_tokens,
		       SUM(output_tokens)         AS output_tokens,
		       SUM(cache_read_tokens)     AS cache_read_tokens,
		       SUM(cache_creation_tokens) AS cache_creation_tokens
		FROM llm_spend
		WHERE org_id = ? AND datetime(occurred_at) >= datetime(?)
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
// scan through sql.NullString → *string.
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
