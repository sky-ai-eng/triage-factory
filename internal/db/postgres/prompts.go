package postgres

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
)

// promptStore is the Postgres impl of db.PromptStore.
//
// # Pool split
//
// Most methods run on the app pool (tf_app, RLS-active). SeedOrUpdate
// is the exception — system_prompt_versions has INSERT/UPDATE/DELETE
// REVOKE'd from tf_app per D3, so the sidecar write can ONLY happen
// on the admin pool. The impl holds both pools at construction and
// picks per-method.
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
	inTx  bool // when constructed inside WithTx, both fields point at the same *sql.Tx
}

func newPromptStore(app, admin queryer) db.PromptStore {
	return &promptStore{app: app, admin: admin}
}

func newTxPromptStore(tx queryer) db.PromptStore {
	// Inside a tx, SeedOrUpdate cannot escape to the admin pool —
	// it would break tx semantics and bypass the caller's WithTx
	// scope. SeedOrUpdate inside a tx-bound store returns an error
	// (see method body); the only production caller is the startup
	// seeder, which runs outside any tx.
	return &promptStore{app: tx, admin: tx, inTx: true}
}

var _ db.PromptStore = (*promptStore)(nil)

// --- SeedOrUpdate --------------------------------------------------

func (s *promptStore) SeedOrUpdate(ctx context.Context, orgID, teamID string, p domain.Prompt) (string, error) {
	if s.inTx {
		// system_prompt_versions writes need the admin pool;
		// inside WithTx we have an app-pool tx. Escaping to the
		// admin pool would silently bypass the caller's tx scope.
		// The startup seeder is the only legit caller and runs
		// outside WithTx.
		return "", errors.New("postgres prompts: SeedOrUpdate must not be called inside WithTx; call stores.Prompts.SeedOrUpdate directly")
	}
	if teamID == "" {
		return "", errors.New("postgres prompts: SeedOrUpdate requires team_id (prompts are team-scoped; seed one copy per team)")
	}
	if p.Source == "" {
		p.Source = "system"
	}
	if p.Source != "system" {
		return "", fmt.Errorf("postgres prompts: SeedOrUpdate only accepts Source=\"system\" (got %q); use Create or UpdateImported for non-system rows", p.Source)
	}
	if p.SystemSlug == "" {
		return "", fmt.Errorf("postgres prompts: SeedOrUpdate requires a non-empty SystemSlug (the shipped slug to dedupe on)")
	}
	hash := shippedContentHash(p)
	now := time.Now().UTC()

	// Open a tx on the admin pool so the prompt + version row writes
	// commit atomically. Admin bypasses RLS so no JWT claim plumbing
	// is needed — supabase_admin sees every row.
	conn, ok := s.admin.(*sql.DB)
	if !ok {
		return "", fmt.Errorf("postgres prompts: SeedOrUpdate requires a *sql.DB admin handle, got %T", s.admin)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin admin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Identity is per-team: (org_id, team_id, system_slug). A second team
	// gets its own copy (same slug, different team_id); re-seeds resolve
	// the existing row by this key (SKY-380).
	var (
		existingID   string
		userModified bool
	)
	switch err := tx.QueryRowContext(ctx,
		`SELECT id, user_modified FROM prompts WHERE org_id = $1 AND team_id = $2 AND system_slug = $3`,
		orgID, teamID, p.SystemSlug,
	).Scan(&existingID, &userModified); {
	case errors.Is(err, sql.ErrNoRows):
		// Fresh insert — mint a random UUID for this team's copy. Shipped
		// system prompts have no human author (prompts_system_has_no_creator
		// pins source='system' ↔ creator_user_id NULL); they're team-owned
		// like every prompt (no visibility column).
		newID := uuid.New().String()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO prompts (id, org_id, team_id, system_slug, creator_user_id, name, body, source, usage_count, user_modified, created_at, updated_at)
			VALUES ($1, $2, $3::uuid, $4, NULL, $5, $6, $7, 0, FALSE, $8, $8)
		`, newID, orgID, teamID, p.SystemSlug, p.Name, p.Body, p.Source, now); err != nil {
			return "", fmt.Errorf("insert prompt: %w", err)
		}
		if err := upsertSystemPromptVersionPG(ctx, tx, orgID, newID, hash, now); err != nil {
			return "", err
		}
		if err := tx.Commit(); err != nil {
			return "", err
		}
		return newID, nil
	case err != nil:
		return "", fmt.Errorf("read prompt: %w", err)
	}

	if userModified {
		return existingID, tx.Commit()
	}

	var priorHash sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT content_hash FROM system_prompt_versions WHERE org_id = $1 AND prompt_id = $2`, orgID, existingID,
	).Scan(&priorHash); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("read prior prompt version: %w", err)
	}
	if priorHash.Valid && priorHash.String == hash {
		return existingID, tx.Commit()
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE prompts
		SET name = $1, body = $2, source = $3, updated_at = $4
		WHERE org_id = $5 AND id = $6
	`, p.Name, p.Body, p.Source, now, orgID, existingID); err != nil {
		return "", fmt.Errorf("update prompt: %w", err)
	}
	if err := upsertSystemPromptVersionPG(ctx, tx, orgID, existingID, hash, now); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return existingID, nil
}

// shippedContentHash digests the shipped (name, body, source) triple
// with null-byte separators. Identical to the SQLite helper — kept
// in this package too because Go's package-private visibility forbids
// the SQLite version reaching here, and duplicating 4 lines beats
// pulling a "shared internals" package for one helper.
func shippedContentHash(p domain.Prompt) string {
	h := sha256.Sum256([]byte(p.Name + "\x00" + p.Body + "\x00" + p.Source))
	return hex.EncodeToString(h[:])
}

func upsertSystemPromptVersionPG(ctx context.Context, q queryer, orgID, promptID, hash string, now time.Time) error {
	// applied_at only bumps when content_hash actually changed —
	// the WHERE on the conflict branch makes identical-hash
	// reseeds a no-op even for legacy rows that fall through to
	// this upsert (the seed body's own short-circuit would also
	// have prevented this for non-legacy rows).
	if _, err := q.ExecContext(ctx, `
		INSERT INTO system_prompt_versions (org_id, prompt_id, content_hash, applied_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (org_id, prompt_id) DO UPDATE SET
			content_hash = EXCLUDED.content_hash,
			applied_at = EXCLUDED.applied_at
		WHERE system_prompt_versions.content_hash <> EXCLUDED.content_hash
	`, orgID, promptID, hash, now); err != nil {
		return fmt.Errorf("upsert system prompt version: %w", err)
	}
	return nil
}

// --- CRUD ----------------------------------------------------------

func (s *promptStore) List(ctx context.Context, orgID string, teamID string) ([]domain.Prompt, error) {
	args := []any{orgID}
	q := `SELECT id, name, body, source, allowed_tools, model, usage_count, team_id, system_slug, created_at, updated_at
		FROM prompts WHERE org_id = $1 AND hidden = FALSE AND deleted_at IS NULL`
	if teamID != "" {
		// Prompts page narrowed to one team: that team's prompts. Every
		// prompt is team-scoped post-SKY-380 (no org-visible tier), so this
		// is a plain team filter. RLS still gates what the caller may see.
		args = append(args, teamID)
		q += fmt.Sprintf(" AND team_id = $%d", len(args))
	}
	q += ` ORDER BY updated_at DESC`
	rows, err := s.app.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var prompts []domain.Prompt
	for rows.Next() {
		p, err := scanPromptRowPG(rows.Scan)
		if err != nil {
			return nil, err
		}
		prompts = append(prompts, p)
	}
	return prompts, rows.Err()
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
// depth (SKY-380).
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
// nullable (user prompts); team_id is NOT NULL post-SKY-380. Note kind is
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
	// SeedOrUpdate on the admin pool, NOT through this method —
	// shipped rows have creator_user_id NULL + visibility='org' per
	// prompts_system_has_no_creator, which neither branch above
	// supports. Don't read this fallback as a deploy-time path.
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

// Delete soft-deletes: it stamps deleted_at rather than removing the row, so
// runs.prompt_id (RESTRICT) and blueprint_steps.step_prompt_id (RESTRICT) FKs
// never fire and historical runs keep resolving the prompt via GetSystem.
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

func (s *promptStore) CountRunReferences(ctx context.Context, orgID, id string) (int, error) {
	var n int
	err := s.app.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM runs WHERE org_id = $1 AND prompt_id = $2`, orgID, id,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count run references: %w", err)
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
	if err := s.app.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN status = 'completed' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END), 0),
			COALESCE(AVG(total_cost_usd), 0),
			COALESCE(AVG(duration_ms), 0)::bigint,
			COALESCE(SUM(total_cost_usd), 0)
		FROM runs WHERE org_id = $1 AND prompt_id = $2
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
		`SELECT MAX(started_at) FROM runs WHERE org_id = $1 AND prompt_id = $2`, orgID, promptID,
	).Scan(&lastUsed); err != nil {
		log.Printf("[prompt_stats] failed to scan MAX(started_at) for %s: %v", promptID, err)
	}
	if lastUsed.Valid {
		formatted := lastUsed.Time.Format(time.RFC3339)
		stats.LastUsedAt = &formatted
	}

	cutoff := time.Now().AddDate(0, 0, -30).Format("2006-01-02")
	rows, err := s.app.QueryContext(ctx, `
		SELECT started_at::date AS day, COUNT(*) AS cnt
		FROM runs
		WHERE org_id = $1 AND prompt_id = $2 AND started_at::date >= $3::date
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
			log.Printf("[prompt_stats] failed to scan runs-per-day row for %s: %v", promptID, err)
			continue
		}
		dayMap[day.Format("2006-01-02")] = cnt
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
