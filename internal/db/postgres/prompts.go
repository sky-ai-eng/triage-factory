package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// promptStore is the Postgres impl of db.PromptStore.
//
// # Pool split
//
// Most methods run on the app pool (tf_app, RLS-active). The ...System reads
// (GetSystem, IncrementUsageSystem) route through the admin pool for the
// claims-less delegation goroutines. The impl holds both pools at construction
// and picks per-method.
//
// # Composite PK + RLS
//
// prompts has PRIMARY KEY (org_id, id) and UNIQUE (id, org_id) (the
// second exists so child tables like projects can use composite FKs
// targeting prompts). Every method includes org_id in WHERE clauses
// as defense in depth alongside RLS — if RLS were ever misconfigured
// or bypassed the org filter still applies.
//
// # Type mappings vs SQLite
//
//   - hidden / user_modified are BOOLEAN here vs INTEGER 0/1 in SQLite.
//     Reads scan into bool; the wire shape (JSON) is identical.
//   - created_at / updated_at are TIMESTAMPTZ; time.Time scans cleanly.
//   - DATE() doesn't exist as a function — use `started_at::date`.
type promptStore struct {
	app   queryer
	admin queryer
}

func newPromptStore(app, admin queryer) db.PromptStore {
	return &promptStore{app: app, admin: admin}
}

func newTxPromptStore(tx queryer) db.PromptStore {
	// Inside a WithTx both pools collapse to the caller's *sql.Tx; the
	// ...System reads run against it rather than escaping to the admin pool.
	return &promptStore{app: tx, admin: tx}
}

var _ db.PromptStore = (*promptStore)(nil)

// --- CRUD ----------------------------------------------------------

func (s *promptStore) List(ctx context.Context, orgID string, teamID string, opts db.ListOpts) ([]domain.Prompt, int, error) {
	args := []any{orgID}
	where := ` WHERE org_id = $1 AND hidden = FALSE AND deleted_at IS NULL`
	if teamID != "" {
		// Prompts page narrowed to one team: that team's prompts. Every
		// prompt is team-scoped (no org-visible tier), so this
		// is a plain team filter. RLS still gates what the caller may see.
		args = append(args, teamID)
		where += fmt.Sprintf(" AND team_id = $%d", len(args))
	}

	var total int
	if err := s.app.QueryRowContext(ctx, `SELECT COUNT(*) FROM prompts`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := `SELECT id, name, body, source, allowed_tools, model, usage_count, team_id, system_slug, created_at, updated_at
		FROM prompts` + where + ` ORDER BY updated_at DESC, id`
	pageArgs := args
	if opts.Limit > 0 {
		q += fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)
		pageArgs = append(append([]any{}, args...), opts.Limit, opts.Offset)
	}
	rows, err := s.app.QueryContext(ctx, q, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var prompts []domain.Prompt
	for rows.Next() {
		p, err := scanPromptRowPG(rows.Scan)
		if err != nil {
			return nil, 0, err
		}
		prompts = append(prompts, p)
	}
	return prompts, total, rows.Err()
}

// Get is request-facing: it filters deleted_at IS NULL. GetSystem (admin pool)
// omits the filter so soft-deleted prompts still resolve for in-flight runs and
// past-run timelines.
func (s *promptStore) Get(ctx context.Context, orgID string, id string) (*domain.Prompt, error) {
	return getPrompt(ctx, s.app, orgID, id, false)
}

func (s *promptStore) GetSystem(ctx context.Context, orgID string, id string) (*domain.Prompt, error) {
	return getPrompt(ctx, s.admin, orgID, id, true)
}

// GetBySystemSlug resolves a team's copy of a shipped prompt by slug. Runs
// on the app pool (RLS-gated); org + team are in the WHERE for defense in
// depth.
func (s *promptStore) GetBySystemSlug(ctx context.Context, orgID, teamID, systemSlug string) (*domain.Prompt, error) {
	if teamID == "" {
		return nil, errors.New("postgres prompts: GetBySystemSlug requires team_id")
	}
	p, err := scanPromptRowPG(s.app.QueryRowContext(ctx, `
		SELECT id, name, body, source, allowed_tools, model, usage_count, team_id, system_slug, created_at, updated_at
		FROM prompts WHERE org_id = $1 AND team_id = $2 AND system_slug = $3 AND deleted_at IS NULL
	`, orgID, teamID, systemSlug).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func getPrompt(ctx context.Context, q queryer, orgID, id string, includeDeleted bool) (*domain.Prompt, error) {
	query := `
		SELECT id, name, body, source, allowed_tools, model, usage_count, team_id, system_slug, created_at, updated_at
		FROM prompts WHERE org_id = $1 AND id = $2`
	if !includeDeleted {
		query += ` AND deleted_at IS NULL`
	}
	p, err := scanPromptRowPG(q.QueryRowContext(ctx, query, orgID, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// scanPromptRowPG decodes a prompts row in the canonical column order
// (id … team_id, system_slug, created_at, updated_at). system_slug is
// nullable (user prompts); team_id is NOT NULL. Note kind is
// not selected here (the PG read path never populated it).
func scanPromptRowPG(scanFn func(dst ...any) error) (domain.Prompt, error) {
	var p domain.Prompt
	var systemSlug sql.NullString
	if err := scanFn(&p.ID, &p.Name, &p.Body, &p.Source, &p.AllowedTools, &p.Model, &p.UsageCount, &p.TeamID, &systemSlug, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return p, err
	}
	if systemSlug.Valid {
		p.SystemSlug = systemSlug.String
	}
	return p, nil
}

func (s *promptStore) Create(ctx context.Context, orgID, teamID string, p domain.Prompt) error {
	// creator_user_id is NOT NULL. Two execution contexts:
	//   - Production request path: WithTx has set request.jwt.claims,
	//     so tf.current_user_id() returns the caller's UUID. That's
	//     the audit identity, the value RLS prompts_insert checks
	//     against (creator_user_id = tf.current_user_id()), and the
	//     value persisted on the row.
	//   - Tests configured with the admin pool as s.app (BYPASSRLS):
	//     no JWT claims are set, so tf.current_user_id() is NULL.
	//     RLS is bypassed entirely so the policy's creator-eq-caller
	//     check doesn't run; the COALESCE fallback exists purely to
	//     satisfy the column-level NOT NULL constraint by stamping
	//     orgs.owner_user_id. The row reads "founder created this,"
	//     which is the natural attribution when no user is on the
	//     call.
	//
	// System / deploy-time seeding of shipped prompts goes through
	// ShippedDefaultsStore.SeedShippedIntoTeam on the admin pool, NOT through
	// this method — shipped rows have creator_user_id NULL per
	// prompts_system_has_no_creator, which neither branch above supports.
	// Don't read this fallback as a deploy-time path.
	//
	// team_id is the acting team the handler resolved for this request —
	// no "first/any team in org" fallback. visibility defaults to 'team',
	// so the team_visibility_requires_team CHECK forces team_id non-NULL;
	// a real team here keeps both the CHECK and the prompts_insert RLS
	// (tf.user_in_team) satisfied. Empty is a handler bug (it must thread
	// the resolved team), so reject it rather than write an invalid row.
	if teamID == "" {
		return fmt.Errorf("postgres prompts Create: team_id required (handler must thread the resolved acting team from request context)")
	}
	_, err := s.app.ExecContext(ctx, `
		INSERT INTO prompts (id, org_id, creator_user_id, team_id, name, body, source, allowed_tools, model, usage_count, created_at, updated_at)
		VALUES ($1, $2,
			COALESCE(tf.current_user_id(), (SELECT owner_user_id FROM orgs WHERE id = $2)),
			$3::uuid,
			$4, $5, $6, $7, $8, 0, now(), now())
	`, p.ID, orgID, teamID, p.Name, p.Body, p.Source, p.AllowedTools, p.Model)
	return err
}

func (s *promptStore) Update(ctx context.Context, orgID string, id, name, body, model string) error {
	_, err := s.app.ExecContext(ctx, `
		UPDATE prompts SET name = $1, body = $2, model = $3, user_modified = TRUE, updated_at = now()
		WHERE org_id = $4 AND id = $5
	`, name, body, model, orgID, id)
	return err
}

func (s *promptStore) UpdateImported(ctx context.Context, orgID string, id, name, body, allowedTools string) error {
	_, err := s.app.ExecContext(ctx, `
		UPDATE prompts SET name = $1, body = $2, allowed_tools = $3, updated_at = now()
		WHERE org_id = $4 AND id = $5
	`, name, body, allowedTools, orgID, id)
	return err
}

// Delete soft-deletes: it stamps deleted_at rather than removing the row,
// so conversations.prompt_id (RESTRICT) and blueprint_steps.step_prompt_id
// (RESTRICT) FKs never fire and historical runs keep resolving the prompt
// via GetSystem.
func (s *promptStore) Delete(ctx context.Context, orgID string, id string) error {
	res, err := s.app.ExecContext(ctx, `UPDATE prompts SET deleted_at = now() WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL`, orgID, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("prompt %s not found or already deleted", id)
	}
	return nil
}

func (s *promptStore) Hide(ctx context.Context, orgID string, id string) error {
	_, err := s.app.ExecContext(ctx, `UPDATE prompts SET hidden = TRUE WHERE org_id = $1 AND id = $2`, orgID, id)
	return err
}

func (s *promptStore) Unhide(ctx context.Context, orgID string, id string) error {
	_, err := s.app.ExecContext(ctx, `UPDATE prompts SET hidden = FALSE WHERE org_id = $1 AND id = $2`, orgID, id)
	return err
}

func (s *promptStore) CountConversationReferences(ctx context.Context, orgID, id string) (int, error) {
	var n int
	err := s.app.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM conversations WHERE org_id = $1 AND prompt_id = $2`, orgID, id,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count conversation references: %w", err)
	}
	return n, nil
}

func (s *promptStore) IncrementUsage(ctx context.Context, orgID string, id string) error {
	return incrementPromptUsage(ctx, s.app, orgID, id)
}

func (s *promptStore) IncrementUsageSystem(ctx context.Context, orgID string, id string) error {
	return incrementPromptUsage(ctx, s.admin, orgID, id)
}

func incrementPromptUsage(ctx context.Context, q queryer, orgID, id string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE prompts SET usage_count = usage_count + 1
		WHERE org_id = $1 AND id = $2
	`, orgID, id)
	return err
}

// --- Stats ---------------------------------------------------------

// Stats mirrors the SQLite impl's three-query shape so the
// conformance harness can assert against identical results across
// both backends. Differences vs SQLite:
//
//   - `DATE(started_at)` becomes `started_at::date` (Postgres has no
//     DATE() function by default; the cast does the same thing).
//   - org_id is included in every WHERE for RLS defense-in-depth.
//
// Like SQLite, the three queries are intentionally separate rather
// than a single CTE — a CTE refactor is a future optimization, not a
// port. If we change it, both backends move together.
func (s *promptStore) Stats(ctx context.Context, orgID string, promptID string) (*domain.PromptStats, error) {
	stats := &domain.PromptStats{}

	// COALESCE on the SUM(CASE…) columns because SUM over zero rows
	// is NULL in Postgres and *int Scan rejects NULL — the
	// never-used-prompt path otherwise blows up the whole stats panel.
	// Per-run cost/duration derive in the inner projection (cost = the
	// messages ledger's settlement SUM, duration = the claims' telemetry
	// SUM); a run with nothing settled yields NULL there, which AVG
	// ignores — the former stored-column semantics.
	if err := s.app.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN status = 'completed' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END), 0),
			COALESCE(AVG(run_cost), 0),
			COALESCE(AVG(run_duration), 0)::bigint,
			COALESCE(SUM(run_cost), 0)
		FROM (
			SELECT c.status,
			       (SELECT SUM(m.cost_usd) FROM messages m WHERE m.conversation_id = c.id AND m.org_id = c.org_id) AS run_cost,
			       (SELECT SUM(cl.duration_ms) FROM claims cl WHERE cl.conversation_id = c.id)                     AS run_duration
			FROM conversations c WHERE c.org_id = $1 AND c.prompt_id = $2
		) runs
	`, orgID, promptID).Scan(
		&stats.TotalRuns,
		&stats.CompletedRuns,
		&stats.FailedRuns,
		&stats.AvgCostUSD,
		&stats.AvgDurationMs,
		&stats.TotalCostUSD,
	); err != nil {
		return nil, err
	}
	if stats.TotalRuns > 0 {
		stats.SuccessRate = float64(stats.CompletedRuns) / float64(stats.TotalRuns)
	}

	var lastUsed sql.NullTime
	if err := s.app.QueryRowContext(ctx,
		`SELECT MAX(started_at) FROM conversations WHERE org_id = $1 AND prompt_id = $2`, orgID, promptID,
	).Scan(&lastUsed); err != nil {
		promptStatsLog.Error("scan max started_at failed", "prompt_id", promptID, "error", err)
	}
	if lastUsed.Valid {
		formatted := lastUsed.Time.Format(time.RFC3339)
		stats.LastUsedAt = &formatted
	}

	// UTC days on both sides. A bare started_at::date casts the timestamptz
	// through the session's TimeZone, so the day a run lands in depends on a
	// connection setting — and the skeleton below, built in the process's own
	// zone, is a third answer again. Pin both to UTC and the lookups line up.
	cutoff := time.Now().UTC().AddDate(0, 0, -30).Format("2006-01-02")
	rows, err := s.app.QueryContext(ctx, `
		SELECT (started_at AT TIME ZONE 'UTC')::date AS day, COUNT(*) AS cnt
		FROM conversations
		WHERE org_id = $1 AND prompt_id = $2
		  AND (started_at AT TIME ZONE 'UTC')::date >= $3::date
		GROUP BY day ORDER BY day
	`, orgID, promptID, cutoff)
	if err != nil {
		return stats, nil
	}
	defer rows.Close()

	dayMap := make(map[string]int)
	for rows.Next() {
		var day time.Time
		var cnt int
		if err := rows.Scan(&day, &cnt); err != nil {
			promptStatsLog.Error("scan runs-per-day row failed", "prompt_id", promptID, "error", err)
			continue
		}
		dayMap[day.Format("2006-01-02")] = cnt
	}
	if err := rows.Err(); err != nil {
		promptStatsLog.Error("runs-per-day iteration error", "prompt_id", promptID, "error", err)
	}

	for i := 29; i >= 0; i-- {
		d := time.Now().UTC().AddDate(0, 0, -i).Format("2006-01-02")
		stats.RunsPerDay = append(stats.RunsPerDay, domain.DayCount{Date: d, Count: dayMap[d]})
	}
	return stats, nil
}
