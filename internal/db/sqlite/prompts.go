package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// promptStore is the SQLite impl of db.PromptStore. SQL bodies are
// ported verbatim from the pre-D2 internal/db/prompts.go +
// internal/db/prompt_stats.go; the only behavioral changes are:
//
//   - assertLocalOrg at every method entry (defends against
//     mis-configured callers passing a real UUID),
//   - context propagation on every Exec/Query (the old free-functions
//     used non-ctx variants),
//   - SeedOrUpdate's tx is opened with BeginTx so it observes ctx
//     cancellation instead of hanging on a dead caller.
//
// Both pools collapse to one *sql.DB in SQLite (no role concept).
// The seeder field is wired identically to q at construction; the
// Postgres impl is where the split actually matters.
type promptStore struct {
	q      queryer
	seeder queryer
}

func newPromptStore(app, admin queryer) db.PromptStore {
	return &promptStore{q: app, seeder: admin}
}

var _ db.PromptStore = (*promptStore)(nil)

// --- SeedOrUpdate --------------------------------------------------

func (s *promptStore) SeedOrUpdate(ctx context.Context, orgID, teamID string, p domain.Prompt) (string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	if teamID == "" {
		teamID = runmode.LocalDefaultTeamID
	}
	if p.Source == "" {
		p.Source = "system"
	}
	if p.Source != "system" {
		return "", fmt.Errorf("sqlite prompts: SeedOrUpdate only accepts Source=\"system\" (got %q); use Create or UpdateImported for non-system rows", p.Source)
	}
	if p.SystemSlug == "" {
		return "", fmt.Errorf("sqlite prompts: SeedOrUpdate requires a non-empty SystemSlug (the shipped slug to dedupe on)")
	}
	hash := shippedContentHash(p)
	now := time.Now().UTC()

	// Wrap the read-modify-write in a tx so the prompt insert/update
	// + the system_prompt_versions upsert are atomic. A mid-stream
	// failure would otherwise leave the version row stamped without
	// the corresponding prompt update applied (or vice versa) and
	// the next seed cycle would see inconsistent state.
	var resultID string
	err := inTx(ctx, s.seeder, func(q queryer) error {
		// Identity is per-team: (org_id, team_id, system_slug). A second
		// team gets its own copy (same slug, different team) and re-seeds
		// resolve the existing row by this key (SKY-380).
		var (
			existingID   string
			userModified int
		)
		switch err := q.QueryRowContext(ctx,
			`SELECT id, user_modified FROM prompts WHERE org_id = ? AND team_id = ? AND system_slug = ?`,
			orgID, teamID, p.SystemSlug,
		).Scan(&existingID, &userModified); {
		case errors.Is(err, sql.ErrNoRows):
			// Fresh insert — mint a random UUID for this team's copy.
			newID := uuid.New().String()
			if _, err := q.ExecContext(ctx, `
				INSERT INTO prompts (id, org_id, team_id, system_slug, name, body, source, usage_count, user_modified, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?)
			`, newID, orgID, teamID, p.SystemSlug, p.Name, p.Body, p.Source, now, now); err != nil {
				return err
			}
			resultID = newID
			return upsertSystemPromptVersionSQLite(ctx, q, newID, hash, now)
		case err != nil:
			return fmt.Errorf("read prompt: %w", err)
		}
		resultID = existingID

		// User-modified rows are intentional local edits — never
		// overwrite, never claim a new shipped hash was applied.
		if userModified != 0 {
			return nil
		}

		// Identical-shipped-content fast path: skip the writes so
		// prompts.updated_at + system_prompt_versions.applied_at
		// don't churn on every startup (the UI orders by updated_at).
		var priorHash sql.NullString
		if err := q.QueryRowContext(ctx,
			`SELECT content_hash FROM system_prompt_versions WHERE prompt_id = ?`, existingID,
		).Scan(&priorHash); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read prior prompt version: %w", err)
		}
		if priorHash.Valid && priorHash.String == hash {
			return nil
		}

		if _, err := q.ExecContext(ctx, `
			UPDATE prompts
			SET name = ?, body = ?, source = ?, updated_at = ?
			WHERE id = ?
		`, p.Name, p.Body, p.Source, now, existingID); err != nil {
			return err
		}
		return upsertSystemPromptVersionSQLite(ctx, q, existingID, hash, now)
	})
	if err != nil {
		return "", err
	}
	return resultID, nil
}

// shippedContentHash digests the shipped (name, body, source) triple
// with null-byte separators so rename + re-source trigger an update
// even when body is unchanged, and so distinct field combinations
// can't collide on a shared prefix ("Foo"+"" vs ""+"Foo").
func shippedContentHash(p domain.Prompt) string {
	h := sha256.Sum256([]byte(p.Name + "\x00" + p.Body + "\x00" + p.Source))
	return hex.EncodeToString(h[:])
}

func upsertSystemPromptVersionSQLite(ctx context.Context, q queryer, promptID, hash string, now time.Time) error {
	// applied_at only bumps when content_hash actually changed —
	// the WHERE on the DO UPDATE branch makes this a no-op on
	// identical-hash reseeds. Important: this is the secondary
	// defense; the SeedOrUpdate body already short-circuits before
	// getting here on identical content. Both defenses matter
	// because legacy rows without a version sidecar fall through
	// to this upsert with a fresh hash, and they should not
	// re-write applied_at every startup once first written.
	if _, err := q.ExecContext(ctx, `
		INSERT INTO system_prompt_versions (prompt_id, content_hash, applied_at)
		VALUES (?, ?, ?)
		ON CONFLICT(prompt_id) DO UPDATE SET
			content_hash = excluded.content_hash,
			applied_at = excluded.applied_at
		WHERE system_prompt_versions.content_hash != excluded.content_hash
	`, promptID, hash, now); err != nil {
		return fmt.Errorf("upsert system prompt version: %w", err)
	}
	return nil
}

// --- CRUD ----------------------------------------------------------

// List ignores teamID: local mode is single-team, so every prompt
// already belongs to the sole team and the multi-team narrowing the
// param expresses is a no-op here.
func (s *promptStore) List(ctx context.Context, orgID string, _ string) ([]domain.Prompt, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, name, body, source, allowed_tools, model, usage_count, team_id, system_slug, created_at, updated_at
		FROM prompts WHERE hidden = 0 AND deleted_at IS NULL ORDER BY updated_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var prompts []domain.Prompt
	for rows.Next() {
		p, err := scanPromptRowSQLite(rows.Scan)
		if err != nil {
			return nil, err
		}
		prompts = append(prompts, p)
	}
	return prompts, rows.Err()
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
// nullable (user prompts); team_id is NOT NULL post-SKY-380.
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
//     (request context). System prompts there go through SeedOrUpdate
//     only — the prompts_system_has_no_creator CHECK rejects
//     source='system' rows that come through this path.
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

// Delete soft-deletes: it stamps deleted_at rather than removing the row, so
// runs.prompt_id (RESTRICT) and blueprint_steps.step_prompt_id (RESTRICT) FKs
// never fire and historical runs keep resolving the prompt via GetSystem.
func (s *promptStore) Delete(ctx context.Context, orgID string, id string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `UPDATE prompts SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`, time.Now().UTC(), id)
	return err
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

func (s *promptStore) CountRunReferences(ctx context.Context, orgID, id string) (int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return 0, err
	}
	var n int
	err := s.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE prompt_id = ?`, id).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count run references: %w", err)
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

// Stats reads three separate views from the runs table. Kept as
// three statements rather than one CTE because the legacy free-
// function shape was three statements and the conformance harness
// covers both backends with identical assertions — a CTE optimization
// is a future patch, not a port. Stats lives on PromptStore (vs
// RunStore) because the queries key on prompt_id and the prompts
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
	if err := s.q.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN status = 'completed' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END), 0),
			COALESCE(AVG(total_cost_usd), 0),
			COALESCE(AVG(duration_ms), 0),
			COALESCE(SUM(total_cost_usd), 0)
		FROM runs WHERE prompt_id = ?
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
		`SELECT MAX(started_at) FROM runs WHERE prompt_id = ?`, promptID,
	).Scan(&lastUsed); err != nil {
		log.Printf("[prompt_stats] failed to scan MAX(started_at) for %s: %v", promptID, err)
	}
	if lastUsed.Valid {
		formatted := lastUsed.Time.Format(time.RFC3339)
		stats.LastUsedAt = &formatted
	}

	// Runs per day (last 30 days). Build the 30-day skeleton so
	// the sparkline always renders 30 buckets even when several
	// days have zero runs.
	cutoff := time.Now().AddDate(0, 0, -30).Format("2006-01-02")
	rows, err := s.q.QueryContext(ctx, `
		SELECT DATE(started_at) AS day, COUNT(*) AS cnt
		FROM runs
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
			log.Printf("[prompt_stats] failed to scan runs-per-day row for %s: %v", promptID, err)
			continue
		}
		dayMap[day] = cnt
	}
	if err := rows.Err(); err != nil {
		log.Printf("[prompt_stats] runs-per-day iteration error for %s: %v", promptID, err)
	}

	for i := 29; i >= 0; i-- {
		d := time.Now().AddDate(0, 0, -i).Format("2006-01-02")
		stats.RunsPerDay = append(stats.RunsPerDay, domain.DayCount{Date: d, Count: dayMap[d]})
	}
	return stats, nil
}
