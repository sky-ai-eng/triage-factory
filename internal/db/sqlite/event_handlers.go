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
//
// Per-kind CHECK constraints on event_handlers enforce the shape
// pair at the DB level; this store branches on Kind where the SQL
// diverges (Create / Update / Seed write different column sets per
// kind).
type eventHandlerStore struct {
	q queryer
}

func newEventHandlerStore(q queryer) db.EventHandlerStore {
	return &eventHandlerStore{q: q}
}

var _ db.EventHandlerStore = (*eventHandlerStore)(nil)

// sqliteEventHandlerColumns mirrors the Postgres projection so scan
// helpers stay aligned. Per-kind nullable columns scan into sql.Null*
// and map to the domain pointer fields.
const sqliteEventHandlerColumns = `id, kind, event_type, scope_predicate_json, enabled, source,
       team_id, applies_to_unowned,
       name, default_priority, sort_order,
       blueprint_id, breaker_threshold, min_autonomy_suitability,
       user_modified, created_at, updated_at`

func (s *eventHandlerStore) Seed(ctx context.Context, orgID, teamID string, blueprintIDsBySlug map[string]string) error {
	if teamID == "" {
		return fmt.Errorf("sqlite event_handlers Seed: teamID required (shipped handlers are team-scoped; local mode passes runmode.LocalDefaultTeamID)")
	}
	now := time.Now().UTC()
	var inserted int64

	// Shipped GitHub rules ship `author_in: []` (match-all) verbatim — the
	// host-dependent seed-time rewrite that scoped them to the local user's
	// login is GONE. Author-centric owner routing scopes these at event time
	// instead (resolving the PR author's team via the owning-team ladder),
	// which also fixes the "connected GitHub after the seed" gap the
	// seed-once rewrite had. The author_in field + matcher stay, as an
	// optional per-member filter for user-authored rules.
	//
	// Shipped Jira rules now ship `assignee_in: []` (match-all) verbatim too —
	// the seed-time rewrite that scoped jira-assigned / jira-became-atomic to
	// the local user's Atlassian account id is GONE, along with the org-host
	// read it needed. Assignee-centric owner routing scopes these at event time
	// instead (resolving the issue assignee's team via the owning-team ladder),
	// matching the GitHub fix. The assignee_in field + matcher stay, as an
	// optional per-member filter for user-authored rules.
	for _, h := range db.ShippedEventHandlers {
		var pred any
		if h.Predicate != "" {
			pred = h.Predicate
		}

		// System rows materialize as visibility='team' with team_id=teamID.
		// The event_handlers_system_has_no_creator CHECK allows
		// creator_user_id NULL when source='system'. The id is a random
		// UUID per team copy and h.ID is the system_slug; re-seed dedupes on
		// the (org_id, team_id, system_slug) unique index via INSERT OR
		// IGNORE.
		switch h.Kind {
		case domain.EventHandlerKindRule:
			res, err := s.q.ExecContext(ctx, `
				INSERT OR IGNORE INTO event_handlers
					(id, org_id, team_id, creator_user_id, kind, event_type,
					 system_slug, scope_predicate_json, enabled, source,
					 name, default_priority, sort_order,
					 created_at, updated_at)
				VALUES (?, ?, ?, NULL, 'rule', ?,
				        ?, ?, 1, 'system',
				        ?, ?, ?,
				        ?, ?)
			`, uuid.New().String(), orgID, teamID, h.EventType,
				h.ID, pred,
				h.Name, h.DefaultPriority, h.SortOrder,
				now, now)
			if err != nil {
				return fmt.Errorf("seed event_handler rule %s: %w", h.ID, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			inserted += n

		case domain.EventHandlerKindTrigger:
			// Resolve the trigger's blueprint slug to this team's blueprint-copy
			// UUID (phase-2 of the two-phase seed). A missing entry means
			// the caller didn't seed the prompts first — fail loudly rather
			// than write a dangling reference.
			blueprintID, ok := blueprintIDsBySlug[h.BlueprintID]
			if !ok || blueprintID == "" {
				return fmt.Errorf("seed event_handler trigger %s: blueprint slug %q not found in blueprintIDsBySlug (seed prompts before handlers)", h.ID, h.BlueprintID)
			}
			// Shipped triggers ship disabled (project convention —
			// users opt in or replace).
			res, err := s.q.ExecContext(ctx, `
				INSERT OR IGNORE INTO event_handlers
					(id, org_id, team_id, creator_user_id, kind, event_type,
					 system_slug, scope_predicate_json, enabled, source,
					 blueprint_id, breaker_threshold, min_autonomy_suitability,
					 created_at, updated_at)
				VALUES (?, ?, ?, NULL, 'trigger', ?,
				        ?, ?, 0, 'system',
				        ?, ?, ?,
				        ?, ?)
			`, uuid.New().String(), orgID, teamID, h.EventType,
				h.ID, pred,
				blueprintID, h.BreakerThreshold, h.MinAutonomySuitability,
				now, now)
			if err != nil {
				return fmt.Errorf("seed event_handler trigger %s: %w", h.ID, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			inserted += n

		default:
			return fmt.Errorf("seed event_handler %s: unknown kind %q", h.ID, h.Kind)
		}
	}
	dbLog.Info("seeded event_handlers", "inserted", inserted, "existed", int64(len(db.ShippedEventHandlers))-inserted)
	return nil
}

// List ignores teamID: local mode is single-team, so the multi-team
// narrowing the param expresses is a no-op here.
func (s *eventHandlerStore) List(ctx context.Context, orgID string, f db.EventHandlerListFilter, opts db.ListOpts) ([]domain.EventHandler, int, error) {
	// f.TeamID is ignored — local mode is single-team.
	where := ` WHERE org_id = ? AND deleted_at IS NULL`
	args := []any{orgID}
	if f.Kind != "" {
		where += ` AND kind = ?`
		args = append(args, f.Kind)
	}
	if len(f.GatedEventTypes) > 0 {
		placeholders := make([]string, len(f.GatedEventTypes))
		for i, et := range f.GatedEventTypes {
			placeholders[i] = "?"
			args = append(args, et)
		}
		where += ` AND event_type NOT IN (` + strings.Join(placeholders, ",") + `)`
	}

	var total int
	if err := s.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_handlers`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := `SELECT ` + sqliteEventHandlerColumns + ` FROM event_handlers` + where + `
	      ORDER BY kind ASC,
	               CASE WHEN kind = 'rule' THEN sort_order ELSE 0 END ASC,
	               CASE WHEN kind = 'rule' THEN name ELSE '' END ASC,
	               created_at DESC, id`
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

// GetBySystemSlug resolves a team's copy of a shipped handler by its stable
// system_slug. Returns (nil, nil) when the team has no copy or its copy is
// soft-deleted.
func (s *eventHandlerStore) GetBySystemSlug(ctx context.Context, orgID, teamID, systemSlug string) (*domain.EventHandler, error) {
	row := s.q.QueryRowContext(ctx, `
		SELECT `+sqliteEventHandlerColumns+`
		FROM event_handlers
		WHERE org_id = ? AND team_id = ? AND system_slug = ? AND deleted_at IS NULL
	`, orgID, teamID, systemSlug)
	h, err := scanEventHandlerRowSQLite(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &h, nil
}

func (s *eventHandlerStore) GetEnabledForEvent(ctx context.Context, orgID, eventType string) ([]domain.EventHandler, error) {
	// Rules-before-triggers order (same as Postgres impl) — preserves
	// the pre-unification observable shape.
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+sqliteEventHandlerColumns+`
		FROM event_handlers
		WHERE org_id = ? AND event_type = ? AND enabled = 1 AND deleted_at IS NULL
		ORDER BY kind ASC,
		         CASE WHEN kind = 'rule' THEN sort_order ELSE 0 END ASC,
		         created_at DESC
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
		WHERE org_id = ? AND blueprint_id = ? AND kind = 'trigger' AND deleted_at IS NULL
		ORDER BY created_at DESC
	`, orgID, blueprintID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectEventHandlersSQLite(rows)
}

func (s *eventHandlerStore) Create(ctx context.Context, orgID, teamID string, h domain.EventHandler) (domain.EventHandler, error) {
	if err := db.ValidateEventHandlerForCreate(&h); err != nil {
		return domain.EventHandler{}, err
	}
	_ = teamID // local mode is single-team; rows pin LocalDefaultTeamID below
	var pred any
	if h.ScopePredicateJSON != nil {
		pred = *h.ScopePredicateJSON
	}
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
	switch h.Kind {
	case domain.EventHandlerKindRule:
		return scanEventHandlerRowSQLite(s.q.QueryRowContext(ctx, `
			INSERT INTO event_handlers
				(id, org_id, team_id, creator_user_id, kind, event_type,
				 scope_predicate_json, enabled, source, applies_to_unowned,
				 name, default_priority, sort_order,
				 created_at, updated_at)
			VALUES (?, ?, ?, ?, 'rule', ?,
			        ?, ?, 'user', ?,
			        ?, ?, ?,
			        ?, ?)
			RETURNING `+sqliteEventHandlerColumns,
			h.ID, orgID, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID, h.EventType,
			pred, h.Enabled, h.AppliesToUnowned,
			h.Name, derefFloat(h.DefaultPriority), derefInt(h.SortOrder),
			now, now))

	case domain.EventHandlerKindTrigger:
		return scanEventHandlerRowSQLite(s.q.QueryRowContext(ctx, `
			INSERT INTO event_handlers
				(id, org_id, team_id, creator_user_id, kind, event_type,
				 scope_predicate_json, enabled, source, applies_to_unowned,
				 blueprint_id, breaker_threshold, min_autonomy_suitability,
				 created_at, updated_at)
			VALUES (?, ?, ?, ?, 'trigger', ?,
			        ?, ?, 'user', ?,
			        ?, ?, ?,
			        ?, ?)
			RETURNING `+sqliteEventHandlerColumns,
			h.ID, orgID, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID, h.EventType,
			pred, h.Enabled, h.AppliesToUnowned,
			h.BlueprintID, derefInt(h.BreakerThreshold), derefFloat(h.MinAutonomySuitability),
			now, now))
	}
	return domain.EventHandler{}, fmt.Errorf("sqlite event_handlers Create: unknown kind %q", h.Kind)
}

func (s *eventHandlerStore) Update(ctx context.Context, orgID string, h domain.EventHandler) (domain.EventHandler, error) {
	if err := db.ValidateEventHandlerForCreate(&h); err != nil {
		return domain.EventHandler{}, err
	}
	var pred any
	if h.ScopePredicateJSON != nil {
		pred = *h.ScopePredicateJSON
	}
	now := time.Now().UTC()

	contentChanged, err := s.contentChanged(ctx, orgID, h)
	if err != nil {
		return domain.EventHandler{}, err
	}

	switch h.Kind {
	case domain.EventHandlerKindRule:
		return scanUpdatedEventHandlerSQLite(s.q.QueryRowContext(ctx, `
			UPDATE event_handlers
			SET scope_predicate_json = ?, enabled = ?, applies_to_unowned = ?,
			    name = ?, default_priority = ?, sort_order = ?,
			    user_modified = user_modified OR ?,
			    updated_at = ?
			WHERE org_id = ? AND id = ? AND kind = 'rule' AND deleted_at IS NULL
			RETURNING `+sqliteEventHandlerColumns,
			pred, h.Enabled, h.AppliesToUnowned,
			h.Name, derefFloat(h.DefaultPriority), derefInt(h.SortOrder),
			contentChanged,
			now, orgID, h.ID))

	case domain.EventHandlerKindTrigger:
		return scanUpdatedEventHandlerSQLite(s.q.QueryRowContext(ctx, `
			UPDATE event_handlers
			SET scope_predicate_json = ?, enabled = ?, applies_to_unowned = ?,
			    breaker_threshold = ?, min_autonomy_suitability = ?,
			    user_modified = user_modified OR ?,
			    updated_at = ?
			WHERE org_id = ? AND id = ? AND kind = 'trigger' AND deleted_at IS NULL
			RETURNING `+sqliteEventHandlerColumns,
			pred, h.Enabled, h.AppliesToUnowned,
			derefInt(h.BreakerThreshold), derefFloat(h.MinAutonomySuitability),
			contentChanged,
			now, orgID, h.ID))
	}
	return domain.EventHandler{}, fmt.Errorf("sqlite event_handlers Update: unknown kind %q", h.Kind)
}

// scanUpdatedEventHandlerSQLite decodes an id-keyed UPDATE … RETURNING. No row
// scanned means no live handler answered the id under this statement's kind
// filter, which is db.ErrNoSuchEventHandler — the answer these writes used to
// give silently as zero rows affected.
func scanUpdatedEventHandlerSQLite(row *sql.Row) (domain.EventHandler, error) {
	h, err := scanEventHandlerRowSQLite(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.EventHandler{}, db.ErrNoSuchEventHandler
	}
	return h, err
}

// contentChanged reports whether h's content fields (scope_predicate_json,
// plus name/default_priority for a rule or breaker_threshold/
// min_autonomy_suitability for a trigger) differ from the row's current
// values — the user_modified stamping signal for Update. enabled and
// sort_order are deliberately excluded (never content). A missing/deleted row
// reports false; the caller's UPDATE affects 0 rows either way.
func (s *eventHandlerStore) contentChanged(ctx context.Context, orgID string, h domain.EventHandler) (bool, error) {
	var (
		predNS        sql.NullString
		nameNS        sql.NullString
		defPriority   sql.NullFloat64
		breakerNS     sql.NullInt64
		minAutonomyNS sql.NullFloat64
	)
	err := s.q.QueryRowContext(ctx, `
		SELECT scope_predicate_json, name, default_priority, breaker_threshold, min_autonomy_suitability
		FROM event_handlers WHERE org_id = ? AND id = ? AND deleted_at IS NULL
	`, orgID, h.ID).Scan(&predNS, &nameNS, &defPriority, &breakerNS, &minAutonomyNS)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var newPred string
	if h.ScopePredicateJSON != nil {
		newPred = *h.ScopePredicateJSON
	}
	if !db.PredicateJSONEqual(predNS.String, newPred) {
		return true, nil
	}
	switch h.Kind {
	case domain.EventHandlerKindRule:
		return nameNS.String != h.Name || defPriority.Float64 != derefFloat(h.DefaultPriority), nil
	case domain.EventHandlerKindTrigger:
		return int(breakerNS.Int64) != derefInt(h.BreakerThreshold) || minAutonomyNS.Float64 != derefFloat(h.MinAutonomySuitability), nil
	}
	return false, nil
}

func (s *eventHandlerStore) SetEnabled(ctx context.Context, orgID, id string, enabled bool) (domain.EventHandler, error) {
	return scanUpdatedEventHandlerSQLite(s.q.QueryRowContext(ctx, `
		UPDATE event_handlers SET enabled = ?, updated_at = ? WHERE org_id = ? AND id = ? AND deleted_at IS NULL
		RETURNING `+sqliteEventHandlerColumns,
		enabled, time.Now().UTC(), orgID, id))
}

// Delete removes a handler. A system_slug row (shipped copy) soft-deletes so
// its (org_id, team_id, system_slug) slot stays occupied for the sync's
// never-resurrect rule; a user-created row (no system_slug) hard-deletes.
func (s *eventHandlerStore) Delete(ctx context.Context, orgID, id string) error {
	var systemSlug sql.NullString
	switch err := s.q.QueryRowContext(ctx,
		`SELECT system_slug FROM event_handlers WHERE org_id = ? AND id = ? AND deleted_at IS NULL`, orgID, id,
	).Scan(&systemSlug); {
	case errors.Is(err, sql.ErrNoRows):
		return nil // already gone; Delete is idempotent
	case err != nil:
		return err
	}
	if systemSlug.Valid && systemSlug.String != "" {
		_, err := s.q.ExecContext(ctx,
			`UPDATE event_handlers SET deleted_at = ? WHERE org_id = ? AND id = ? AND deleted_at IS NULL`,
			time.Now().UTC(), orgID, id)
		return err
	}
	_, err := s.q.ExecContext(ctx, `DELETE FROM event_handlers WHERE org_id = ? AND id = ?`, orgID, id)
	return err
}

func (s *eventHandlerStore) RetargetBlueprint(ctx context.Context, orgID, id, newBlueprintID string) (domain.EventHandler, error) {
	return scanUpdatedEventHandlerSQLite(s.q.QueryRowContext(ctx, `
		UPDATE event_handlers SET blueprint_id = ?, user_modified = 1, updated_at = ?
		WHERE org_id = ? AND id = ? AND kind = 'trigger' AND deleted_at IS NULL
		RETURNING `+sqliteEventHandlerColumns,
		newBlueprintID, time.Now().UTC(), orgID, id))
}

func (s *eventHandlerStore) Reorder(ctx context.Context, orgID string, ids []string) error {
	return inTx(ctx, s.q, func(q queryer) error {
		now := time.Now().UTC()
		for i, id := range ids {
			if _, err := q.ExecContext(ctx, `
				UPDATE event_handlers SET sort_order = ?, updated_at = ?
				WHERE org_id = ? AND id = ? AND kind = 'rule' AND deleted_at IS NULL
			`, i, now, orgID, id); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *eventHandlerStore) Promote(ctx context.Context, orgID string, id string, t domain.EventHandler) (domain.EventHandler, error) {
	if t.Kind != domain.EventHandlerKindTrigger {
		return domain.EventHandler{}, errors.New("sqlite event_handlers Promote: target kind must be 'trigger'")
	}
	if t.BlueprintID == "" || t.BreakerThreshold == nil || t.MinAutonomySuitability == nil {
		return domain.EventHandler{}, errors.New("sqlite event_handlers Promote: trigger fields required")
	}
	var pred any
	if t.ScopePredicateJSON != nil {
		pred = *t.ScopePredicateJSON
	}
	// Single UPDATE flips kind, clears rule-only columns, populates
	// trigger-only. The per-kind CHECK constraints validate atomically. A
	// kind change is always a user modification — stamp unconditionally.
	// The row is where a caller sees the cleared rule-only columns; t
	// describes only the trigger half. No row scanned means the id named no
	// live rule, which is what the rows-affected probe here used to report.
	return scanUpdatedEventHandlerSQLite(s.q.QueryRowContext(ctx, `
		UPDATE event_handlers
		SET kind = 'trigger',
		    blueprint_id = ?, breaker_threshold = ?, min_autonomy_suitability = ?,
		    name = NULL, default_priority = NULL, sort_order = NULL,
		    scope_predicate_json = ?,
		    user_modified = 1,
		    updated_at = ?
		WHERE org_id = ? AND id = ? AND kind = 'rule' AND deleted_at IS NULL
		RETURNING `+sqliteEventHandlerColumns,
		t.BlueprintID, *t.BreakerThreshold, *t.MinAutonomySuitability,
		pred, time.Now().UTC(),
		orgID, id))
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

		var resolvedBP string
		var bpOK bool
		if h.Kind == domain.EventHandlerKindTrigger {
			resolvedBP, bpOK = blueprintIDsBySlug[h.BlueprintID]
		}

		plan := db.PlanHandlerSync(h, resolvedBP, bpOK, row)
		switch plan.Action {
		case db.HandlerSkip, db.HandlerEqual:
			return nil
		case db.HandlerInsert:
			return insertHandlerSQLite(ctx, q, orgID, teamID, h, plan)
		case db.HandlerApply:
			return applyHandlerSQLite(ctx, q, orgID, row.ID, h.Kind, plan)
		}
		return nil
	})
}

// loadTeamHandlerRowSQLite reads the team's row for one shipped handler's
// system_slug, including a soft-deleted occupant (Deleted=true). Exists=false
// when no row.
func loadTeamHandlerRowSQLite(ctx context.Context, q queryer, orgID, teamID, slug string) (db.TeamHandlerRow, error) {
	var (
		id, blueprintID string
		mod, del        int
		pred, name      sql.NullString
		defPriority     sql.NullFloat64
		breaker         sql.NullInt64
		minAutonomy     sql.NullFloat64
	)
	switch err := q.QueryRowContext(ctx, `
		SELECT id, user_modified, CASE WHEN deleted_at IS NULL THEN 0 ELSE 1 END,
		       scope_predicate_json, name, default_priority,
		       COALESCE(blueprint_id, ''), breaker_threshold, min_autonomy_suitability
		FROM event_handlers WHERE org_id = ? AND team_id = ? AND system_slug = ?
	`, orgID, teamID, slug).Scan(&id, &mod, &del, &pred, &name, &defPriority, &blueprintID, &breaker, &minAutonomy); {
	case errors.Is(err, sql.ErrNoRows):
		return db.TeamHandlerRow{Exists: false}, nil
	case err != nil:
		return db.TeamHandlerRow{}, fmt.Errorf("read handler %s: %w", slug, err)
	}
	row := db.TeamHandlerRow{Exists: true, Deleted: del != 0, UserModified: mod != 0, ID: id, BlueprintID: blueprintID}
	if pred.Valid {
		row.Predicate = pred.String
	}
	if name.Valid {
		row.Name = name.String
	}
	if defPriority.Valid {
		row.DefaultPriority = defPriority.Float64
	}
	if breaker.Valid {
		row.BreakerThreshold = int(breaker.Int64)
	}
	if minAutonomy.Valid {
		row.MinAutonomySuitability = minAutonomy.Float64
	}
	return row, nil
}

// insertHandlerSQLite inserts a brand-new team copy of a shipped handler,
// honoring the shipped enabled convention (rules enabled, triggers disabled)
// — matches Seed.
func insertHandlerSQLite(ctx context.Context, q queryer, orgID, teamID string, h db.ShippedEventHandler, plan db.HandlerSyncPlan) error {
	now := time.Now().UTC()
	var pred any
	if plan.Predicate != "" {
		pred = plan.Predicate
	}
	switch h.Kind {
	case domain.EventHandlerKindRule:
		_, err := q.ExecContext(ctx, `
			INSERT INTO event_handlers
				(id, org_id, team_id, creator_user_id, kind, event_type,
				 system_slug, scope_predicate_json, enabled, source,
				 name, default_priority, sort_order,
				 created_at, updated_at)
			VALUES (?, ?, ?, NULL, 'rule', ?,
			        ?, ?, 1, 'system',
			        ?, ?, ?,
			        ?, ?)
		`, uuid.New().String(), orgID, teamID, h.EventType,
			h.ID, pred,
			plan.Name, plan.DefaultPriority, plan.SortOrder,
			now, now)
		return err
	case domain.EventHandlerKindTrigger:
		_, err := q.ExecContext(ctx, `
			INSERT INTO event_handlers
				(id, org_id, team_id, creator_user_id, kind, event_type,
				 system_slug, scope_predicate_json, enabled, source,
				 blueprint_id, breaker_threshold, min_autonomy_suitability,
				 created_at, updated_at)
			VALUES (?, ?, ?, NULL, 'trigger', ?,
			        ?, ?, 0, 'system',
			        ?, ?, ?,
			        ?, ?)
		`, uuid.New().String(), orgID, teamID, h.EventType,
			h.ID, pred,
			plan.BlueprintID, plan.BreakerThreshold, plan.MinAutonomySuitability,
			now, now)
		return err
	}
	return fmt.Errorf("sqlite event_handlers Sync: insert: unknown kind %q", h.Kind)
}

// applyHandlerSQLite rewrites an existing row's diverging content fields.
// Never touches enabled or sort_order.
func applyHandlerSQLite(ctx context.Context, q queryer, orgID, id string, kind string, plan db.HandlerSyncPlan) error {
	now := time.Now().UTC()
	var pred any
	if plan.Predicate != "" {
		pred = plan.Predicate
	}
	switch kind {
	case domain.EventHandlerKindRule:
		_, err := q.ExecContext(ctx, `
			UPDATE event_handlers
			SET scope_predicate_json = ?, name = ?, default_priority = ?, updated_at = ?
			WHERE org_id = ? AND id = ?
		`, pred, plan.Name, plan.DefaultPriority, now, orgID, id)
		return err
	case domain.EventHandlerKindTrigger:
		_, err := q.ExecContext(ctx, `
			UPDATE event_handlers
			SET scope_predicate_json = ?, blueprint_id = ?, breaker_threshold = ?, min_autonomy_suitability = ?, updated_at = ?
			WHERE org_id = ? AND id = ?
		`, pred, plan.BlueprintID, plan.BreakerThreshold, plan.MinAutonomySuitability, now, orgID, id)
		return err
	}
	return fmt.Errorf("sqlite event_handlers Sync: apply: unknown kind %q", kind)
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
	var (
		pred          sql.NullString
		teamID        sql.NullString
		nameNS        sql.NullString
		defPriority   sql.NullFloat64
		sortOrder     sql.NullInt64
		blueprintID   sql.NullString
		breakerNS     sql.NullInt64
		minAutonomyNS sql.NullFloat64
	)
	if err := scanFn(
		&h.ID, &h.Kind, &h.EventType, &pred, &h.Enabled, &h.Source,
		&teamID, &h.AppliesToUnowned,
		&nameNS, &defPriority, &sortOrder,
		&blueprintID, &breakerNS, &minAutonomyNS,
		&h.UserModified, &h.CreatedAt, &h.UpdatedAt,
	); err != nil {
		return h, err
	}
	if teamID.Valid {
		h.TeamID = teamID.String
	}
	if pred.Valid {
		s := pred.String
		h.ScopePredicateJSON = &s
	}
	if nameNS.Valid {
		h.Name = nameNS.String
	}
	if defPriority.Valid {
		v := defPriority.Float64
		h.DefaultPriority = &v
	}
	if sortOrder.Valid {
		v := int(sortOrder.Int64)
		h.SortOrder = &v
	}
	if blueprintID.Valid {
		h.BlueprintID = blueprintID.String
	}
	if breakerNS.Valid {
		v := int(breakerNS.Int64)
		h.BreakerThreshold = &v
	}
	if minAutonomyNS.Valid {
		v := minAutonomyNS.Float64
		h.MinAutonomySuitability = &v
	}
	if h.Kind == domain.EventHandlerKindTrigger {
		h.TriggerType = domain.TriggerTypeEvent
	}
	return h, nil
}

func derefFloat(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
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

func (s *eventHandlerStore) GetEnabledForEventSystem(ctx context.Context, orgID, eventType string) ([]domain.EventHandler, error) {
	return s.GetEnabledForEvent(ctx, orgID, eventType)
}
