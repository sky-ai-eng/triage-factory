package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// eventHandlerStore is the Postgres impl of db.EventHandlerStore.
//
// # Pool split
//
//   - app   — tf_app, RLS-active. Every CRUD method runs here.
//   - admin — supabase_admin, BYPASSRLS. Seed runs here because the
//     event_handlers_insert RLS policy gates on
//     creator_user_id = tf.current_user_id() and on team write; boot-time
//     Seed has no JWT claims and would otherwise fail the WITH CHECK on
//     every shipped row.
//
// Inside WithTx both fields point at the same *sql.Tx, and inTx is
// true. Seed inside WithTx is rejected: escaping to the admin pool
// would break the caller's transaction scope (matches PromptStore).
type eventHandlerStore struct {
	app   queryer
	admin queryer
	inTx  bool
}

func newEventHandlerStore(app, admin queryer) db.EventHandlerStore {
	return &eventHandlerStore{app: app, admin: admin}
}

// newTxEventHandlerStore composes a tx-bound EventHandlerStore for
// WithTx / NewForTx. Both pools collapse onto the caller's tx; inTx=true
// makes Seed refuse rather than silently bypass the tx scope.
func newTxEventHandlerStore(tx queryer) db.EventHandlerStore {
	return &eventHandlerStore{app: tx, admin: tx, inTx: true}
}

var _ db.EventHandlerStore = (*eventHandlerStore)(nil)

// pgEventHandlerColumns is the row's projection, shared by every read and
// every RETURNING.
const pgEventHandlerColumns = `id, mode, event_type, scope_predicate_json::text, source,
       team_id, applies_to_unowned,
       name, default_priority, sort_order,
       blueprint_id, breaker_threshold, min_autonomy_suitability,
       user_modified, created_at, updated_at,
       COALESCE(creator_user_id::text, '')`

func (s *eventHandlerStore) Seed(ctx context.Context, orgID, teamID string, blueprintIDsBySlug map[string]string) error {
	if s.inTx {
		return errors.New("postgres event_handlers: Seed must not be called inside WithTx; call stores.EventHandlers.Seed directly")
	}
	if teamID == "" {
		return errors.New("postgres event_handlers Seed: teamID required (shipped handlers are team-scoped; caller threads the target team — local mode passes runmode.LocalDefaultTeamID, multi mode passes the new team's UUID on team creation)")
	}
	var inserted int64
	for _, h := range db.ShippedEventHandlers {
		// The id is a random UUID per team copy and h.ID is the system_slug;
		// re-seed dedupes on (org_id, team_id, system_slug) via ON CONFLICT.
		blueprintID, resolved := db.ResolveShippedBlueprint(h, blueprintIDsBySlug)
		plan := db.PlanHandlerSync(h, blueprintID, resolved, db.TeamHandlerRow{})
		n, err := insertShippedHandlerPG(ctx, s.admin, orgID, teamID, h, plan, `ON CONFLICT (org_id, team_id, system_slug) DO NOTHING`)
		if err != nil {
			return fmt.Errorf("seed event_handler %s: %w", h.ID, err)
		}
		inserted += n
	}
	dbPgLog.Info("seeded event_handlers", "inserted", inserted, "existed", int64(len(db.ShippedEventHandlers))-inserted)
	return nil
}

func (s *eventHandlerStore) List(ctx context.Context, orgID string, f db.EventHandlerListFilter, opts db.ListOpts) ([]domain.EventHandler, int, error) {
	where, args := s.buildListWhere(orgID, f)

	var total int
	if err := s.app.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_handlers`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if opts.CountOnly {
		return []domain.EventHandler{}, total, nil
	}

	q := `SELECT ` + pgEventHandlerColumns + `
	      FROM event_handlers` + where + `
	      ORDER BY sort_order ASC, name ASC, id ASC`
	pageArgs := args
	if opts.Limit > 0 {
		q += fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)
		pageArgs = append(append([]any{}, args...), opts.Limit, opts.Offset)
	}
	rows, err := s.scanList(ctx, q, pageArgs)
	return rows, total, err
}

func (s *eventHandlerStore) Get(ctx context.Context, orgID, id string) (*domain.EventHandler, error) {
	return getEventHandler(ctx, s.app, orgID, id)
}

func (s *eventHandlerStore) GetSystem(ctx context.Context, orgID, id string) (*domain.EventHandler, error) {
	return getEventHandler(ctx, s.admin, orgID, id)
}

func getEventHandler(ctx context.Context, q queryer, orgID, id string) (*domain.EventHandler, error) {
	if !isValidUUID(id) {
		return nil, nil
	}
	row := q.QueryRowContext(ctx, `
		SELECT `+pgEventHandlerColumns+`
		FROM event_handlers
		WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL
	`, orgID, id)
	h, err := scanEventHandlerRowPG(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &h, nil
}

func (s *eventHandlerStore) GetActiveForEvent(ctx context.Context, orgID, eventType string) ([]domain.EventHandler, error) {
	return getActiveEventHandlers(ctx, s.app, orgID, eventType)
}

func (s *eventHandlerStore) GetActiveForEventSystem(ctx context.Context, orgID, eventType string) ([]domain.EventHandler, error) {
	return getActiveEventHandlers(ctx, s.admin, orgID, eventType)
}

// pgActiveHandler is the WHERE fragment for a handler an event can match: not
// off, live, and on a team that isn't archived. It is shared with the task
// list's handler order so that order counts exactly the handlers
// GetActiveForEvent returns.
const pgActiveHandler = `mode <> 'off' AND deleted_at IS NULL
	AND NOT EXISTS (SELECT 1 FROM teams tm WHERE tm.id = event_handlers.team_id AND tm.deleted_at IS NOT NULL)`

func getActiveEventHandlers(ctx context.Context, q queryer, orgID, eventType string) ([]domain.EventHandler, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT `+pgEventHandlerColumns+`
		FROM event_handlers
		WHERE org_id = $1 AND event_type = $2 AND `+pgActiveHandler+`
		ORDER BY sort_order ASC, id ASC
	`, orgID, eventType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectEventHandlers(rows)
}

func (s *eventHandlerStore) ListForBlueprint(ctx context.Context, orgID, blueprintID string) ([]domain.EventHandler, error) {
	rows, err := s.app.QueryContext(ctx, `
		SELECT `+pgEventHandlerColumns+`
		FROM event_handlers
		WHERE org_id = $1 AND blueprint_id = $2 AND deleted_at IS NULL
		ORDER BY created_at DESC, id
	`, orgID, blueprintID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectEventHandlers(rows)
}

func (s *eventHandlerStore) Create(ctx context.Context, orgID, teamID string, h domain.EventHandler) (domain.EventHandler, error) {
	if err := db.ValidateEventHandler(h); err != nil {
		return domain.EventHandler{}, err
	}
	// team_id is the acting team the handler resolved for this request —
	// no "first/any team in org" fallback. team_id is NOT NULL (the sole
	// scoping signal) and the event_handlers_insert RLS gates on
	// tf.user_can_write_team(team_id), so a real team here keeps the write
	// valid. Empty is a handler bug, so reject it rather than write an
	// invalid row.
	if teamID == "" {
		return domain.EventHandler{}, fmt.Errorf("postgres event_handlers Create: team_id required (handler must thread the resolved acting team from request context)")
	}
	// RETURNING projects the point read's column list, so the row handed back
	// carries the creator identity the COALESCE resolved, the forced source
	// and the stamped timestamps h never described.
	return scanEventHandlerRowPG(s.app.QueryRowContext(ctx, `
		INSERT INTO event_handlers
			(id, org_id, creator_user_id, team_id, mode, event_type,
			 scope_predicate_json, source, applies_to_unowned,
			 name, default_priority, sort_order,
			 blueprint_id, breaker_threshold, min_autonomy_suitability,
			 created_at, updated_at)
		VALUES (
			$1, $2,
			COALESCE(tf.current_user_id(), (SELECT owner_user_id FROM orgs WHERE id = $2)),
			$3::uuid,
			$4, $5,
			$6::jsonb, 'user', $7,
			$8, $9, $10,
			$11, $12, $13,
			now(), now()
		)
		RETURNING `+pgEventHandlerColumns,
		h.ID, orgID, teamID, h.Mode, h.EventType,
		predicateArgPG(h.ScopePredicateJSON), h.AppliesToUnowned,
		h.Name, h.DefaultPriority, h.SortOrder,
		nullIfEmpty(h.BlueprintID), h.BreakerThreshold, h.MinAutonomySuitability))
}

func (s *eventHandlerStore) Update(ctx context.Context, orgID string, h domain.EventHandler) (domain.EventHandler, error) {
	if !isValidUUID(h.ID) {
		return domain.EventHandler{}, db.ErrNoSuchEventHandler
	}
	if err := db.ValidateEventHandler(h); err != nil {
		return domain.EventHandler{}, err
	}
	cur, found, err := s.current(ctx, orgID, h.ID)
	if err != nil {
		return domain.EventHandler{}, err
	}
	if !found {
		return domain.EventHandler{}, db.ErrNoSuchEventHandler
	}
	// The write is guarded on the blueprint read here, and attaches a
	// different one only while it is live. The guard on blueprint_id is what
	// holds under READ COMMITTED: an UPDATE that waited on a concurrent
	// detach re-checks its WHERE against the detached row, while the EXISTS
	// still reads the snapshot the statement began with, where a blueprint
	// whose delete has not committed is live.
	//
	// So an attach holds the blueprint first, with the share lock
	// BlueprintStore.GetForShare takes, and decides that it attaches from cur
	// rather than from the caller's read: h composed from an older read of
	// the row can attach a blueprint the caller believes the row already
	// holds, and so does not hold itself. In a transaction the lock lasts
	// until commit; outside one it is released before the UPDATE runs.
	attaches := h.BlueprintID != "" && h.BlueprintID != cur.BlueprintID
	if attaches {
		var live int
		err := s.app.QueryRowContext(ctx,
			`SELECT 1 FROM blueprints WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL FOR SHARE`,
			orgID, h.BlueprintID).Scan(&live)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.EventHandler{}, db.ErrEventHandlerBlueprintChanged
		}
		if err != nil {
			return domain.EventHandler{}, fmt.Errorf("hold blueprint %s: %w", h.BlueprintID, err)
		}
	}
	updated, err := scanUpdatedEventHandlerPG(s.app.QueryRowContext(ctx, `
		UPDATE event_handlers
		SET mode = $1, scope_predicate_json = $2::jsonb, applies_to_unowned = $3,
		    name = $4, default_priority = $5, sort_order = $6,
		    blueprint_id = $7, breaker_threshold = $8, min_autonomy_suitability = $9,
		    user_modified = user_modified OR $10,
		    updated_at = now()
		WHERE org_id = $11 AND id = $12 AND deleted_at IS NULL
		  AND blueprint_id IS NOT DISTINCT FROM $13::text
		  AND (NOT $14::boolean OR EXISTS (SELECT 1 FROM blueprints b WHERE b.id = $7 AND b.deleted_at IS NULL))
		RETURNING `+pgEventHandlerColumns,
		h.Mode, predicateArgPG(h.ScopePredicateJSON), h.AppliesToUnowned,
		h.Name, h.DefaultPriority, h.SortOrder,
		nullIfEmpty(h.BlueprintID), h.BreakerThreshold, h.MinAutonomySuitability,
		db.HandlerContentChanged(cur, h),
		orgID, h.ID,
		nullIfEmpty(cur.BlueprintID), attaches))
	if errors.Is(err, db.ErrNoSuchEventHandler) {
		if _, still, e := s.current(ctx, orgID, h.ID); e != nil {
			return domain.EventHandler{}, e
		} else if still {
			return domain.EventHandler{}, db.ErrEventHandlerBlueprintChanged
		}
	}
	return updated, err
}

// predicateArgPG binds a nil predicate as NULL.
func predicateArgPG(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// scanUpdatedEventHandlerPG decodes an id-keyed UPDATE … RETURNING. No row
// scanned means no live handler answered the id — or that RLS hides whatever
// does — and both are db.ErrNoSuchEventHandler.
func scanUpdatedEventHandlerPG(row *sql.Row) (domain.EventHandler, error) {
	h, err := scanEventHandlerRowPG(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.EventHandler{}, db.ErrNoSuchEventHandler
	}
	return h, err
}

// current reads the content fields of the live handler id — what Update
// compares h against to decide the user_modified stamp, and the blueprint its
// write is guarded on — and whether there is one.
func (s *eventHandlerStore) current(ctx context.Context, orgID, id string) (db.TeamHandlerRow, bool, error) {
	var cur db.TeamHandlerRow
	var pred, blueprintID sql.NullString
	err := s.app.QueryRowContext(ctx, `
		SELECT scope_predicate_json::text, name, default_priority, blueprint_id, breaker_threshold, min_autonomy_suitability
		FROM event_handlers WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL
	`, orgID, id).Scan(&pred, &cur.Name, &cur.DefaultPriority, &blueprintID, &cur.BreakerThreshold, &cur.MinAutonomySuitability)
	if errors.Is(err, sql.ErrNoRows) {
		return db.TeamHandlerRow{}, false, nil
	}
	if err != nil {
		return db.TeamHandlerRow{}, false, err
	}
	cur.Predicate, cur.BlueprintID = pred.String, blueprintID.String
	return cur, true, nil
}

// Delete soft-deletes a handler by stamping deleted_at.
func (s *eventHandlerStore) Delete(ctx context.Context, orgID, id string) error {
	if !isValidUUID(id) {
		return nil
	}
	_, err := s.app.ExecContext(ctx,
		`UPDATE event_handlers SET deleted_at = now() WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL`,
		orgID, id)
	return err
}

func (s *eventHandlerStore) Reorder(ctx context.Context, orgID string, ids []string) error {
	return inTxRaw(ctx, s.app, func(tx *sql.Tx) error {
		for i, id := range ids {
			if !isValidUUID(id) {
				continue
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE event_handlers SET sort_order = $1, updated_at = now()
				WHERE org_id = $2 AND id = $3 AND deleted_at IS NULL
			`, i, orgID, id); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *eventHandlerStore) DetachBlueprint(ctx context.Context, orgID, blueprintID string) error {
	_, err := s.app.ExecContext(ctx, `
		UPDATE event_handlers
		SET blueprint_id = NULL,
		    mode = CASE WHEN mode = 'automate' THEN 'watch' ELSE mode END,
		    updated_at = now()
		WHERE org_id = $1 AND blueprint_id = $2 AND deleted_at IS NULL
	`, orgID, blueprintID)
	return err
}

// Sync brings teamID's unmodified copies of db.ShippedEventHandlers up to
// current shipped content. See db.EventHandlerStore.Sync for the normative
// rules. Runs on the admin pool (claims-less boot work, mirrors Seed); each
// handler is synced in its own admin-pool transaction so one failure doesn't
// block the rest.
func (s *eventHandlerStore) Sync(ctx context.Context, orgID, teamID string, shippedHandlers []db.ShippedEventHandler, blueprintIDsBySlug map[string]string) error {
	if s.inTx {
		return errors.New("postgres event_handlers: Sync must not be called inside WithTx; call stores.EventHandlers.Sync directly")
	}
	if teamID == "" {
		return errors.New("postgres event_handlers Sync: teamID required")
	}
	conn, ok := s.admin.(*sql.DB)
	if !ok {
		return fmt.Errorf("postgres event_handlers Sync: requires a *sql.DB admin handle, got %T", s.admin)
	}
	for _, h := range shippedHandlers {
		if err := s.syncOne(ctx, conn, orgID, teamID, h, blueprintIDsBySlug); err != nil {
			return fmt.Errorf("sync event_handler %s: %w", h.ID, err)
		}
	}
	return nil
}

func (s *eventHandlerStore) syncOne(ctx context.Context, conn *sql.DB, orgID, teamID string, h db.ShippedEventHandler, blueprintIDsBySlug map[string]string) error {
	return db.InTx(ctx, conn, func(tx *sql.Tx) error {
		row, err := loadTeamHandlerRowPG(ctx, tx, orgID, teamID, h.ID)
		if err != nil {
			return err
		}
		blueprintID, resolved := db.ResolveShippedBlueprint(h, blueprintIDsBySlug)
		plan := db.PlanHandlerSync(h, blueprintID, resolved, row)
		switch plan.Action {
		case db.HandlerInsert:
			_, err := insertShippedHandlerPG(ctx, tx, orgID, teamID, h, plan, "")
			return err
		case db.HandlerApply:
			return applyHandlerPG(ctx, tx, orgID, row.ID, plan)
		default:
			return nil // HandlerSkip / HandlerEqual: nothing written, so the commit is a no-op
		}
	})
}

// loadTeamHandlerRowPG reads the team's row for one shipped handler's
// system_slug, including a soft-deleted occupant (Deleted=true). Exists=false
// when no row.
func loadTeamHandlerRowPG(ctx context.Context, q queryer, orgID, teamID, slug string) (db.TeamHandlerRow, error) {
	var (
		row               db.TeamHandlerRow
		pred, blueprintID sql.NullString
	)
	switch err := q.QueryRowContext(ctx, `
		SELECT id, user_modified, (deleted_at IS NOT NULL),
		       scope_predicate_json::text, name, default_priority,
		       blueprint_id, breaker_threshold, min_autonomy_suitability
		FROM event_handlers WHERE org_id = $1 AND team_id = $2::uuid AND system_slug = $3
	`, orgID, teamID, slug).Scan(&row.ID, &row.UserModified, &row.Deleted, &pred, &row.Name, &row.DefaultPriority, &blueprintID, &row.BreakerThreshold, &row.MinAutonomySuitability); {
	case errors.Is(err, sql.ErrNoRows):
		return db.TeamHandlerRow{Exists: false}, nil
	case err != nil:
		return db.TeamHandlerRow{}, fmt.Errorf("read handler %s: %w", slug, err)
	}
	row.Exists = true
	row.Predicate, row.BlueprintID = pred.String, blueprintID.String
	return row, nil
}

// insertShippedHandlerPG inserts a team's copy of a shipped handler in its
// shipped mode and sort order. onConflict is Seed's ON CONFLICT clause, which
// leaves an existing copy alone, and empty for the sync, which has just read
// that no row holds the slug. It reports the rows inserted.
func insertShippedHandlerPG(ctx context.Context, q queryer, orgID, teamID string, h db.ShippedEventHandler, plan db.HandlerSyncPlan, onConflict string) (int64, error) {
	var pred any
	if plan.Predicate != "" {
		pred = plan.Predicate
	}
	now := time.Now().UTC()
	res, err := q.ExecContext(ctx, `
		INSERT INTO event_handlers
			(id, org_id, team_id, creator_user_id, mode, event_type,
			 system_slug, scope_predicate_json, source,
			 name, default_priority, sort_order,
			 blueprint_id, breaker_threshold, min_autonomy_suitability,
			 created_at, updated_at)
		VALUES (
			$1, $2, $3::uuid, NULL, $4, $5,
			$6, $7::jsonb, 'system',
			$8, $9, $10,
			$11, $12, $13,
			$14, $14
		)
		`+onConflict,
		uuid.New().String(), orgID, teamID, plan.Mode, h.EventType,
		h.ID, pred,
		plan.Name, plan.DefaultPriority, plan.SortOrder,
		nullIfEmpty(plan.BlueprintID), plan.BreakerThreshold, plan.MinAutonomySuitability,
		now)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// applyHandlerPG rewrites an existing row's content fields. Never touches mode
// or sort_order, and leaves blueprint_id alone when the plan does not write
// it.
func applyHandlerPG(ctx context.Context, q queryer, orgID, id string, plan db.HandlerSyncPlan) error {
	var pred any
	if plan.Predicate != "" {
		pred = plan.Predicate
	}
	_, err := q.ExecContext(ctx, `
		UPDATE event_handlers
		SET scope_predicate_json = $1::jsonb, name = $2, default_priority = $3,
		    blueprint_id = CASE WHEN $4::boolean THEN $5::text ELSE blueprint_id END,
		    breaker_threshold = $6, min_autonomy_suitability = $7, updated_at = now()
		WHERE org_id = $8 AND id = $9
	`, pred, plan.Name, plan.DefaultPriority,
		plan.WriteBlueprint, nullIfEmpty(plan.BlueprintID),
		plan.BreakerThreshold, plan.MinAutonomySuitability, orgID, id)
	return err
}

// buildListWhere composes the WHERE clause List's count and page share, so the
// two can't apply different filters to the same request.
func (s *eventHandlerStore) buildListWhere(orgID string, f db.EventHandlerListFilter) (string, []any) {
	args := []any{orgID}
	where := ` WHERE org_id = $1 AND deleted_at IS NULL`
	if len(f.Modes) > 0 {
		args = append(args, f.Modes)
		where += fmt.Sprintf(" AND mode = ANY($%d)", len(args))
	}
	if f.EventType != "" {
		args = append(args, f.EventType)
		where += fmt.Sprintf(" AND event_type = $%d", len(args))
	}
	if f.HasBlueprint != nil {
		if *f.HasBlueprint {
			where += " AND blueprint_id IS NOT NULL"
		} else {
			where += " AND blueprint_id IS NULL"
		}
	}
	if f.TeamID != "" {
		// Every handler is team-owned (team_id NOT NULL, no org-visible
		// tier), so this is a plain team filter. RLS still gates the row set;
		// this narrows within it.
		args = append(args, f.TeamID)
		where += fmt.Sprintf(" AND team_id = $%d", len(args))
	}
	if len(f.GatedEventTypes) > 0 {
		args = append(args, f.GatedEventTypes)
		where += fmt.Sprintf(" AND NOT (event_type = ANY($%d))", len(args))
	}
	return where, args
}

func (s *eventHandlerStore) scanList(ctx context.Context, query string, args []any) ([]domain.EventHandler, error) {
	rows, err := s.app.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectEventHandlers(rows)
}

func collectEventHandlers(rows *sql.Rows) ([]domain.EventHandler, error) {
	var out []domain.EventHandler
	for rows.Next() {
		h, err := scanEventHandlerPG(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func scanEventHandlerPG(rows *sql.Rows) (domain.EventHandler, error) {
	return scanEventHandlerFromAny(rows.Scan)
}

func scanEventHandlerRowPG(row *sql.Row) (domain.EventHandler, error) {
	return scanEventHandlerFromAny(row.Scan)
}

// scanEventHandlerFromAny is the shared row decoder.
func scanEventHandlerFromAny(scanFn func(dst ...any) error) (domain.EventHandler, error) {
	var h domain.EventHandler
	var pred, blueprintID sql.NullString
	if err := scanFn(
		&h.ID, &h.Mode, &h.EventType, &pred, &h.Source,
		&h.TeamID, &h.AppliesToUnowned,
		&h.Name, &h.DefaultPriority, &h.SortOrder,
		&blueprintID, &h.BreakerThreshold, &h.MinAutonomySuitability,
		&h.UserModified, &h.CreatedAt, &h.UpdatedAt,
		&h.CreatorUserID,
	); err != nil {
		return h, err
	}
	h.BlueprintID = blueprintID.String
	if pred.Valid {
		s := pred.String
		h.ScopePredicateJSON = &s
	}
	return h, nil
}
