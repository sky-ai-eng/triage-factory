package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// promptStore is the SQLite impl of db.PromptStore. SQL bodies are
// ported verbatim from the pre-D2 internal/db/prompts.go +
// internal/db/prompt_stats.go; the only behavioral changes are
// assertLocalOrg at every method entry (defends against mis-configured
// callers passing a real UUID) and context propagation on every
// Exec/Query (the old free-functions used non-ctx variants).
//
// SQLite has no role concept, so the two pools the Postgres impl keeps
// distinct (app vs admin) collapse to one *sql.DB here.
type promptStore struct {
	q queryer
}

func newPromptStore(q queryer) db.PromptStore {
	return &promptStore{q: q}
}

var _ db.PromptStore = (*promptStore)(nil)

// --- CRUD ----------------------------------------------------------

// List ignores teamID: local mode is single-team, so every prompt
// already belongs to the sole team and the multi-team narrowing the
// param expresses is a no-op here.
func (s *promptStore) List(ctx context.Context, orgID string, _ string, opts db.ListOpts) ([]domain.Prompt, int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM prompts WHERE hidden = 0 AND deleted_at IS NULL
	`).Scan(&total); err != nil {
		return nil, 0, err
	}
	query := `
		SELECT id, name, body, source, allowed_tools, model, usage_count, team_id, system_slug, created_at, updated_at
		FROM prompts WHERE hidden = 0 AND deleted_at IS NULL ORDER BY updated_at DESC, id`
	args := []any{}
	if opts.Limit > 0 {
		query += ` LIMIT ? OFFSET ?`
		args = append(args, opts.Limit, opts.Offset)
	}
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var prompts []domain.Prompt
	for rows.Next() {
		p, err := scanPromptRowSQLite(rows.Scan)
		if err != nil {
			return nil, 0, err
		}
		prompts = append(prompts, p)
	}
	return prompts, total, rows.Err()
}

// Get is request-facing: it filters deleted_at IS NULL so a soft-deleted prompt
// reads as absent. GetSystem omits the filter for in-flight runs / past-run
// timelines.
func (s *promptStore) Get(ctx context.Context, orgID string, id string) (*domain.Prompt, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	p, err := scanPromptRowSQLite(s.q.QueryRowContext(ctx, `
		SELECT id, name, body, source, allowed_tools, model, usage_count, team_id, system_slug, created_at, updated_at
		FROM prompts WHERE id = ? AND deleted_at IS NULL
	`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// scanPromptRowSQLite decodes a prompts row in the canonical column order
// (id … team_id, system_slug, created_at, updated_at). system_slug is
// nullable (user prompts); team_id is NOT NULL.
func scanPromptRowSQLite(scanFn func(dst ...any) error) (domain.Prompt, error) {
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

// GetSystem omits the deleted_at filter so soft-deleted prompts still resolve
// for in-flight runs and past-run timelines (mirrors the admin-pool variant in
// Postgres).
func (s *promptStore) GetSystem(ctx context.Context, orgID string, id string) (*domain.Prompt, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	p, err := scanPromptRowSQLite(s.q.QueryRowContext(ctx, `
		SELECT id, name, body, source, allowed_tools, model, usage_count, team_id, system_slug, created_at, updated_at
		FROM prompts WHERE id = ?
	`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// GetBySystemSlug resolves a shipped prompt by slug. SQLite is single-team
// in production, so the slug alone is unique; a non-empty teamID further
// scopes it (used by the per-team-divergence conformance path). org_id is
// in the WHERE for consistency with the store's other reads (assertLocalOrg
// already gates it).
func (s *promptStore) GetBySystemSlug(ctx context.Context, orgID, teamID, systemSlug string) (*domain.Prompt, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	q := `
		SELECT id, name, body, source, allowed_tools, model, usage_count, team_id, system_slug, created_at, updated_at
		FROM prompts WHERE org_id = ? AND system_slug = ? AND deleted_at IS NULL`
	args := []any{orgID, systemSlug}
	if teamID != "" {
		q += ` AND team_id = ?`
		args = append(args, teamID)
	}
	p, err := scanPromptRowSQLite(s.q.QueryRowContext(ctx, q, args...).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// Create inserts a prompt row scoped to the local sentinel team. The
// creator_user_id is derived from p.Source rather than taken from the
// caller, which is a deliberate divergence from the Postgres impl:
//
//   - Postgres Create derives creator_user_id from tf.current_user_id()
//     (request context). System prompts there are seeded by
//     ShippedDefaultsStore.SeedShippedIntoTeam on the admin pool — the
//     prompts_system_has_no_creator CHECK rejects source='system' rows
//     that come through this Create path.
//   - SQLite Create handles both source='system' (system tests +
//     curator skill seeds) and source∈('user','imported') in one
//     entry point because local mode has no request context to
//     derive identity from and tests reach into Create directly.
//
// Local-mode call sites today: the prompts HTTP handler (source='user')
// and skills.ImportAll (source='imported'). Tests that need a
// source='system' row also use this method. The override keeps
// those three call paths converging on a single Create rather than
// adding a per-source variant or forcing the handler to wire team_id
// from outside. Every prompt is team-owned (no visibility column).
func (s *promptStore) Create(ctx context.Context, orgID, teamID string, p domain.Prompt) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_ = teamID // local mode is single-team; the row pins LocalDefaultTeamID below
	now := time.Now().UTC()
	var creatorUserID any = runmode.LocalDefaultUserID
	if p.Source == "system" {
		creatorUserID = nil
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO prompts (id, name, body, source, allowed_tools, model, usage_count, team_id, creator_user_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?)
	`, p.ID, p.Name, p.Body, p.Source, p.AllowedTools, p.Model, runmode.LocalDefaultTeamID, creatorUserID, now, now)
	return err
}

func (s *promptStore) Update(ctx context.Context, orgID string, id, name, body, model string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE prompts SET name = ?, body = ?, model = ?, user_modified = 1, updated_at = ? WHERE id = ?
	`, name, body, model, time.Now().UTC(), id)
	return err
}

func (s *promptStore) UpdateImported(ctx context.Context, orgID string, id, name, body, allowedTools string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE prompts SET name = ?, body = ?, allowed_tools = ?, updated_at = ? WHERE id = ?
	`, name, body, allowedTools, time.Now().UTC(), id)
	return err
}

// Delete soft-deletes: it stamps deleted_at rather than removing the row,
// so conversations.prompt_id (RESTRICT) and blueprint_steps.step_prompt_id
// (RESTRICT) FKs never fire and historical runs keep resolving the prompt
// via GetSystem.
func (s *promptStore) Delete(ctx context.Context, orgID string, id string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	res, err := s.q.ExecContext(ctx, `UPDATE prompts SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`, time.Now().UTC(), id)
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
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `UPDATE prompts SET hidden = 1 WHERE id = ?`, id)
	return err
}

func (s *promptStore) Unhide(ctx context.Context, orgID string, id string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `UPDATE prompts SET hidden = 0 WHERE id = ?`, id)
	return err
}

func (s *promptStore) CountConversationReferences(ctx context.Context, orgID, id string) (int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return 0, err
	}
	var n int
	err := s.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversations WHERE prompt_id = ?`, id).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count conversation references: %w", err)
	}
	return n, nil
}

func (s *promptStore) IncrementUsage(ctx context.Context, orgID string, id string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `UPDATE prompts SET usage_count = usage_count + 1 WHERE id = ?`, id)
	return err
}

func (s *promptStore) IncrementUsageSystem(ctx context.Context, orgID string, id string) error {
	return s.IncrementUsage(ctx, orgID, id)
}

// --- Stats ---------------------------------------------------------

// Stats reads three separate views from the conversations table. Kept as
// three statements rather than one CTE because the legacy free-
// function shape was three statements and the conformance harness
// covers both backends with identical assertions — a CTE optimization
// is a future patch, not a port. Stats lives on PromptStore (vs
// ConversationStore) because the queries key on prompt_id and the prompts
// handler is the only consumer.
func (s *promptStore) Stats(ctx context.Context, orgID string, promptID string) (*domain.PromptStats, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	stats := &domain.PromptStats{}

	// Totals — aggregate, always returns exactly one row. COALESCE on
	// the SUM(CASE…) columns because SUM over zero rows is NULL in
	// SQLite (and Postgres) and *int Scan rejects NULL — the
	// never-used-prompt path otherwise blows up the whole stats panel.
	// Per-run cost/duration derive in the inner projection (cost = the
	// messages ledger's settlement SUM, duration = the claims' telemetry
	// SUM); a run with nothing settled yields NULL there, which AVG
	// ignores — the former stored-column semantics.
	if err := s.q.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN status = 'completed' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END), 0),
			COALESCE(AVG(run_cost), 0),
			COALESCE(AVG(run_duration), 0),
			COALESCE(SUM(run_cost), 0)
		FROM (
			SELECT c.status,
			       (SELECT SUM(m.cost_usd) FROM messages m WHERE m.conversation_id = c.id)     AS run_cost,
			       (SELECT SUM(cl.duration_ms) FROM claims cl WHERE cl.conversation_id = c.id) AS run_duration
			FROM conversations c WHERE c.prompt_id = ?
		)
	`, promptID).Scan(
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

	// Last used — MAX() on an aggregate always returns one row
	// (NULL if no matches). Log + continue on scan errors so the
	// rest of the stats render rather than 500-ing the whole panel.
	var lastUsed sql.NullTime
	if err := s.q.QueryRowContext(ctx,
		`SELECT MAX(started_at) FROM conversations WHERE prompt_id = ?`, promptID,
	).Scan(&lastUsed); err != nil {
		promptStatsLog.Error("scan max started_at failed", "prompt_id", promptID, "error", err)
	}
	if lastUsed.Valid {
		formatted := lastUsed.Time.Format(time.RFC3339)
		stats.LastUsedAt = &formatted
	}

	// Runs per day (last 30 days). Build the 30-day skeleton so
	// the sparkline always renders 30 buckets even when several
	// days have zero runs.
	//
	// UTC days on both sides. SQLite's DATE() resolves started_at to a UTC
	// day whatever offset the stored value carries, so a skeleton built in
	// the process's local zone names a different day for every run within
	// the offset of midnight — and every one of those lookups misses,
	// dropping real runs out of the sparkline.
	cutoff := time.Now().UTC().AddDate(0, 0, -30).Format("2006-01-02")
	rows, err := s.q.QueryContext(ctx, `
		SELECT DATE(started_at) AS day, COUNT(*) AS cnt
		FROM conversations
		WHERE prompt_id = ? AND DATE(started_at) >= ?
		GROUP BY day ORDER BY day
	`, promptID, cutoff)
	if err != nil {
		// Non-fatal: totals + last-used are enough for a useful
		// response. Match the legacy free-function's behavior so the
		// frontend continues to render.
		return stats, nil
	}
	defer rows.Close()

	dayMap := make(map[string]int)
	for rows.Next() {
		var day string
		var cnt int
		if err := rows.Scan(&day, &cnt); err != nil {
			promptStatsLog.Error("scan runs-per-day row failed", "prompt_id", promptID, "error", err)
			continue
		}
		dayMap[day] = cnt
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
