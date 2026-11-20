package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// eventHandlerStore is the SQLite impl of db.EventHandlerStore. The
// SQLite schema carries org_id structurally, so every
// method filters by org_id (matching the Postgres impl's WHERE pattern)
// rather than the older assertLocalOrg convention the predecessor
// stores used.
type eventHandlerStore struct {
	q queryer
}

func newEventHandlerStore(q queryer) db.EventHandlerStore {
	return &eventHandlerStore{q: q}
}

var _ db.EventHandlerStore = (*eventHandlerStore)(nil)

// sqliteEventHandlerColumns mirrors the Postgres projection so scan helpers
// stay aligned.
const sqliteEventHandlerColumns = `id, mode, event_type, scope_predicate_json, source,
       team_id, applies_to_unowned,
       name, default_priority, sort_order,
       blueprint_id, breaker_threshold, min_autonomy_suitability,
       user_modified, created_at, updated_at,
       creator_user_id`

func (s *eventHandlerStore) Seed(ctx context.Context, orgID, teamID string, blueprintIDsBySlug map[string]string) error {
	if teamID == "" {
		return fmt.Errorf("sqlite event_handlers Seed: teamID required (shipped handlers are team-scoped; local mode passes runmode.LocalDefaultTeamID)")
	}
	var inserted int64

	// Shipped predicates are inserted as written. The GitHub handlers' empty
	// `author_id_in` and the Jira and Linear handlers' empty `assignee_in`
	// match everyone; owner routing scopes them at event time, resolving the
	// PR author's (or the issue assignee's) team through the owning-team
	// ladder, so a user who connects a source after the seed is scoped the
	// same way.
	for _, h := range db.ShippedEventHandlers {
		blueprintID, resolved := db.ResolveShippedBlueprint(h, blueprintIDsBySlug)
		plan := db.PlanHandlerSync(h, blueprintID, resolved, db.TeamHandlerRow{})
		n, err := insertShippedHandlerSQLite(ctx, s.q, orgID, teamID, h, plan, `INSERT OR IGNORE`)
		if err != nil {
			return fmt.Errorf("seed event_handler %s: %w", h.ID, err)
		}
		inserted += n
	}
	dbLog.Info("seeded event_handlers", "inserted", inserted, "existed", int64(len(db.ShippedEventHandlers))-inserted)
	return nil
}

// List ignores teamID: local mode is single-team, so the multi-team
// narrowing the param expresses is a no-op here.
func (s *eventHandlerStore) List(ctx context.Context, orgID string, f db.EventHandlerListFilter, opts db.ListOpts) ([]domain.EventHandler, int, error) {
	where := ` WHERE org_id = ? AND deleted_at IS NULL`
	args := []any{orgID}
	if len(f.Modes) > 0 {
		where += ` AND mode IN (` + placeholders(len(f.Modes)) + `)`
		for _, m := range f.Modes {
			args = append(args, m)
		}
	}
	if f.EventType != "" {
		where += ` AND event_type = ?`
		args = append(args, f.EventType)
	}
	if f.HasBlueprint != nil {
		if *f.HasBlueprint {
			where += ` AND blueprint_id IS NOT NULL`
		} else {
			where += ` AND blueprint_id IS NULL`
		}
	}
	if len(f.GatedEventTypes) > 0 {
		where += ` AND event_type NOT IN (` + placeholders(len(f.GatedEventTypes)) + `)`
		for _, et := range f.GatedEventTypes {
			args = append(args, et)
		}
	}

	var total int
	if err := s.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_handlers`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if opts.CountOnly {
		return []domain.EventHandler{}, total, nil
	}

	q := `SELECT ` + sqliteEventHandlerColumns + ` FROM event_handlers` + where + `
	      ORDER BY sort_order ASC, name ASC, id ASC`
	pageArgs := args
	if opts.Limit > 0 {
		q += ` LIMIT ? OFFSET ?`
		pageArgs = append(append([]any{}, args...), opts.Limit, opts.Offset)
	}
	rows, err := s.q.QueryContext(ctx, q, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out, err := collectEventHandlersSQLite(rows)
	return out, total, err
}

// placeholders renders n comma-separated ? markers.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func (s *eventHandlerStore) Get(ctx context.Context, orgID, id string) (*domain.EventHandler, error) {
	row := s.q.QueryRowContext(ctx, `
		SELECT `+sqliteEventHandlerColumns+`
		FROM event_handlers
		WHERE org_id = ? AND id = ? AND deleted_at IS NULL
	`, orgID, id)
	h, err := scanEventHandlerRowSQLite(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &h, nil
}

// sqliteActiveHandler is the WHERE fragment for a handler an event can match:
// not off, live, and on a team that isn't archived. It is shared with the
// task list's handler order so that order counts exactly the handlers
// GetActiveForEvent returns.
const sqliteActiveHandler = `mode <> 'off' AND deleted_at IS NULL
	AND NOT EXISTS (SELECT 1 FROM teams tm WHERE tm.id = event_handlers.team_id AND tm.deleted_at IS NOT NULL)`

func (s *eventHandlerStore) GetActiveForEvent(ctx context.Context, orgID, eventType string) ([]domain.EventHandler, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+sqliteEventHandlerColumns+`
		FROM event_handlers
		WHERE org_id = ? AND event_type = ? AND `+sqliteActiveHandler+`
		ORDER BY sort_order ASC, id ASC
	`, orgID, eventType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectEventHandlersSQLite(rows)
}

func (s *eventHandlerStore) ListForBlueprint(ctx context.Context, orgID, blueprintID string) ([]domain.EventHandler, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+sqliteEventHandlerColumns+`
		FROM event_handlers
		WHERE org_id = ? AND blueprint_id = ? AND deleted_at IS NULL
		ORDER BY created_at DESC, id
	`, orgID, blueprintID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectEventHandlersSQLite(rows)
}

func (s *eventHandlerStore) Create(ctx context.Context, orgID, teamID string, h domain.EventHandler) (domain.EventHandler, error) {
	if err := db.ValidateEventHandler(h); err != nil {
		return domain.EventHandler{}, err
	}
	_ = teamID // local mode is single-team; rows pin LocalDefaultTeamID below
	now := time.Now().UTC()

	// user-source rows are team-scoped with team_id = LocalDefaultTeamID.
	// SQLite has one team in local mode (the sentinel) so the acting team
	// is always that team — the teamID the handler threads is ignored
	// here. creator_user_id is required for source='user' rows by the
	// event_handlers_system_has_no_creator CHECK.
	//
	// RETURNING projects the point read's column list, so the row handed back
	// carries the pinned team, the forced source and the stamped timestamps h
	// never described.
	return scanEventHandlerRowSQLite(s.q.QueryRowContext(ctx, `
		INSERT INTO event_handlers
			(id, org_id, team_id, creator_user_id, mode, event_type,
			 scope_predicate_json, source, applies_to_unowned,
			 name, default_priority, sort_order,
			 blueprint_id, breaker_threshold, min_autonomy_suitability,
			 created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?,
		        ?, 'user', ?,
		        ?, ?, ?,
		        ?, ?, ?,
		        ?, ?)
		RETURNING `+sqliteEventHandlerColumns,
		h.ID, orgID, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID, h.Mode, h.EventType,
		predicateArg(h.ScopePredicateJSON), h.AppliesToUnowned,
		h.Name, h.DefaultPriority, h.SortOrder,
		nullIfEmpty(h.BlueprintID), h.BreakerThreshold, h.MinAutonomySuitability,
		now, now))
}

func (s *eventHandlerStore) Update(ctx context.Context, orgID string, h domain.EventHandler) (domain.EventHandler, error) {
	if err := db.ValidateEventHandler(h); err != nil {
		return domain.EventHandler{}, err
	}
	contentChanged, err := s.contentChanged(ctx, orgID, h)
	if err != nil {
		return domain.EventHandler{}, err
	}
	return scanUpdatedEventHandlerSQLite(s.q.QueryRowContext(ctx, `
		UPDATE event_handlers
		SET mode = ?, scope_predicate_json = ?, applies_to_unowned = ?,
		    name = ?, default_priority = ?, sort_order = ?,
		    blueprint_id = ?, breaker_threshold = ?, min_autonomy_suitability = ?,
		    user_modified = user_modified OR ?,
		    updated_at = ?
		WHERE org_id = ? AND id = ? AND deleted_at IS NULL
		RETURNING `+sqliteEventHandlerColumns,
		h.Mode, predicateArg(h.ScopePredicateJSON), h.AppliesToUnowned,
		h.Name, h.DefaultPriority, h.SortOrder,
		nullIfEmpty(h.BlueprintID), h.BreakerThreshold, h.MinAutonomySuitability,
		contentChanged,
		time.Now().UTC(), orgID, h.ID))
}

// predicateArg binds a nil predicate as NULL.
func predicateArg(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// scanUpdatedEventHandlerSQLite decodes an id-keyed UPDATE … RETURNING. No row
// scanned means no live handler answered the id, which is
// db.ErrNoSuchEventHandler.
func scanUpdatedEventHandlerSQLite(row *sql.Row) (domain.EventHandler, error) {
	h, err := scanEventHandlerRowSQLite(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.EventHandler{}, db.ErrNoSuchEventHandler
	}
	return h, err
}

// contentChanged reports whether h's content fields differ from the row's
// current values — the user_modified stamping signal for Update. mode and
// sort_order are deliberately excluded (never content). A missing/deleted row
// reports false; the caller's UPDATE affects 0 rows either way.
func (s *eventHandlerStore) contentChanged(ctx context.Context, orgID string, h domain.EventHandler) (bool, error) {
	var cur db.TeamHandlerRow
	var pred, blueprintID sql.NullString
	err := s.q.QueryRowContext(ctx, `
		SELECT scope_predicate_json, name, default_priority, blueprint_id, breaker_threshold, min_autonomy_suitability
		FROM event_handlers WHERE org_id = ? AND id = ? AND deleted_at IS NULL
	`, orgID, h.ID).Scan(&pred, &cur.Name, &cur.DefaultPriority, &blueprintID, &cur.BreakerThreshold, &cur.MinAutonomySuitability)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	cur.Predicate, cur.BlueprintID = pred.String, blueprintID.String
	return db.HandlerContentChanged(cur, h), nil
}

// Delete soft-deletes a handler by stamping deleted_at.
func (s *eventHandlerStore) Delete(ctx context.Context, orgID, id string) error {
	_, err := s.q.ExecContext(ctx,
		`UPDATE event_handlers SET deleted_at = ? WHERE org_id = ? AND id = ? AND deleted_at IS NULL`,
		time.Now().UTC(), orgID, id)
	return err
}

func (s *eventHandlerStore) Reorder(ctx context.Context, orgID string, ids []string) error {
	return inTx(ctx, s.q, func(q queryer) error {
		now := time.Now().UTC()
		for i, id := range ids {
			if _, err := q.ExecContext(ctx, `
				UPDATE event_handlers SET sort_order = ?, updated_at = ?
				WHERE org_id = ? AND id = ? AND deleted_at IS NULL
			`, i, now, orgID, id); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *eventHandlerStore) DetachBlueprint(ctx context.Context, orgID, blueprintID string) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE event_handlers
		SET blueprint_id = NULL,
		    mode = CASE WHEN mode = 'automate' THEN 'watch' ELSE mode END,
		    updated_at = ?
		WHERE org_id = ? AND blueprint_id = ? AND deleted_at IS NULL
	`, time.Now().UTC(), orgID, blueprintID)
	return err
}

// Sync brings teamID's unmodified copies of db.ShippedEventHandlers up to
// current shipped content. See db.EventHandlerStore.Sync for the normative
// rules. Each handler is synced in its own transaction so one failure doesn't
// block the rest.
func (s *eventHandlerStore) Sync(ctx context.Context, orgID, teamID string, shippedHandlers []db.ShippedEventHandler, blueprintIDsBySlug map[string]string) error {
	if teamID == "" {
		return errors.New("sqlite event_handlers Sync: teamID required")
	}
	for _, h := range shippedHandlers {
		if err := s.syncOne(ctx, orgID, teamID, h, blueprintIDsBySlug); err != nil {
			return fmt.Errorf("sync event_handler %s: %w", h.ID, err)
		}
	}
	return nil
}

func (s *eventHandlerStore) syncOne(ctx context.Context, orgID, teamID string, h db.ShippedEventHandler, blueprintIDsBySlug map[string]string) error {
	return inTx(ctx, s.q, func(q queryer) error {
		row, err := loadTeamHandlerRowSQLite(ctx, q, orgID, teamID, h.ID)
		if err != nil {
			return err
		}
		blueprintID, resolved := db.ResolveShippedBlueprint(h, blueprintIDsBySlug)
		plan := db.PlanHandlerSync(h, blueprintID, resolved, row)
		switch plan.Action {
		case db.HandlerInsert:
			_, err := insertShippedHandlerSQLite(ctx, q, orgID, teamID, h, plan, `INSERT`)
			return err
		case db.HandlerApply:
			return applyHandlerSQLite(ctx, q, orgID, row.ID, plan)
		}
		return nil
	})
}

// loadTeamHandlerRowSQLite reads the team's row for one shipped handler's
// system_slug, including a soft-deleted occupant (Deleted=true). Exists=false
// when no row.
func loadTeamHandlerRowSQLite(ctx context.Context, q queryer, orgID, teamID, slug string) (db.TeamHandlerRow, error) {
	var (
		row               db.TeamHandlerRow
		mod, del          int
		pred, blueprintID sql.NullString
	)
	switch err := q.QueryRowContext(ctx, `
		SELECT id, user_modified, CASE WHEN deleted_at IS NULL THEN 0 ELSE 1 END,
		       scope_predicate_json, name, default_priority,
		       blueprint_id, breaker_threshold, min_autonomy_suitability
		FROM event_handlers WHERE org_id = ? AND team_id = ? AND system_slug = ?
	`, orgID, teamID, slug).Scan(&row.ID, &mod, &del, &pred, &row.Name, &row.DefaultPriority, &blueprintID, &row.BreakerThreshold, &row.MinAutonomySuitability); {
	case errors.Is(err, sql.ErrNoRows):
		return db.TeamHandlerRow{Exists: false}, nil
	case err != nil:
		return db.TeamHandlerRow{}, fmt.Errorf("read handler %s: %w", slug, err)
	}
	row.Exists, row.Deleted, row.UserModified = true, del != 0, mod != 0
	row.Predicate, row.BlueprintID = pred.String, blueprintID.String
	return row, nil
}

// insertShippedHandlerSQLite inserts a team's copy of a shipped handler in its
// shipped mode and sort order. verb is INSERT for the sync, which has just
// read that no row holds the slug, and INSERT OR IGNORE for Seed, which
// leaves an existing copy alone. It reports the rows inserted.
func insertShippedHandlerSQLite(ctx context.Context, q queryer, orgID, teamID string, h db.ShippedEventHandler, plan db.HandlerSyncPlan, verb string) (int64, error) {
	now := time.Now().UTC()
	var pred any
	if plan.Predicate != "" {
		pred = plan.Predicate
	}
	res, err := q.ExecContext(ctx, verb+` INTO event_handlers
			(id, org_id, team_id, creator_user_id, mode, event_type,
			 system_slug, scope_predicate_json, source,
			 name, default_priority, sort_order,
			 blueprint_id, breaker_threshold, min_autonomy_suitability,
			 created_at, updated_at)
		VALUES (?, ?, ?, NULL, ?, ?,
		        ?, ?, 'system',
		        ?, ?, ?,
		        ?, ?, ?,
		        ?, ?)
	`, uuid.New().String(), orgID, teamID, plan.Mode, h.EventType,
		h.ID, pred,
		plan.Name, plan.DefaultPriority, plan.SortOrder,
		nullIfEmpty(plan.BlueprintID), plan.BreakerThreshold, plan.MinAutonomySuitability,
		now, now)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// applyHandlerSQLite rewrites an existing row's content fields. Never touches
// mode or sort_order, and leaves blueprint_id alone when the plan does not
// write it.
func applyHandlerSQLite(ctx context.Context, q queryer, orgID, id string, plan db.HandlerSyncPlan) error {
	var pred any
	if plan.Predicate != "" {
		pred = plan.Predicate
	}
	_, err := q.ExecContext(ctx, `
		UPDATE event_handlers
		SET scope_predicate_json = ?, name = ?, default_priority = ?,
		    blueprint_id = CASE WHEN ? THEN ? ELSE blueprint_id END,
		    breaker_threshold = ?, min_autonomy_suitability = ?, updated_at = ?
		WHERE org_id = ? AND id = ?
	`, pred, plan.Name, plan.DefaultPriority,
		plan.WriteBlueprint, nullIfEmpty(plan.BlueprintID),
		plan.BreakerThreshold, plan.MinAutonomySuitability, time.Now().UTC(), orgID, id)
	return err
}

func collectEventHandlersSQLite(rows *sql.Rows) ([]domain.EventHandler, error) {
	var out []domain.EventHandler
	for rows.Next() {
		h, err := scanEventHandlerSQLite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func scanEventHandlerSQLite(rows *sql.Rows) (domain.EventHandler, error) {
	return scanEventHandlerFromAnySQLite(rows.Scan)
}

func scanEventHandlerRowSQLite(row *sql.Row) (domain.EventHandler, error) {
	return scanEventHandlerFromAnySQLite(row.Scan)
}

func scanEventHandlerFromAnySQLite(scanFn func(dst ...any) error) (domain.EventHandler, error) {
	var h domain.EventHandler
	var pred, blueprintID, creator sql.NullString
	if err := scanFn(
		&h.ID, &h.Mode, &h.EventType, &pred, &h.Source,
		&h.TeamID, &h.AppliesToUnowned,
		&h.Name, &h.DefaultPriority, &h.SortOrder,
		&blueprintID, &h.BreakerThreshold, &h.MinAutonomySuitability,
		&h.UserModified, &h.CreatedAt, &h.UpdatedAt,
		&creator,
	); err != nil {
		return h, err
	}
	h.CreatorUserID = creator.String
	h.BlueprintID = blueprintID.String
	if pred.Valid {
		s := pred.String
		h.ScopePredicateJSON = &s
	}
	return h, nil
}

// --- Admin-pool variants ---
//
// SQLite has one connection; the router's eventbus subscriber goroutine
// reaches these via the same path as the app variants. Wrappers exist
// for signature parity with the Postgres impl, which routes admin calls
// through BYPASSRLS.

func (s *eventHandlerStore) GetSystem(ctx context.Context, orgID, id string) (*domain.EventHandler, error) {
	return s.Get(ctx, orgID, id)
}

func (s *eventHandlerStore) GetActiveForEventSystem(ctx context.Context, orgID, eventType string) ([]domain.EventHandler, error) {
	return s.GetActiveForEvent(ctx, orgID, eventType)
}
