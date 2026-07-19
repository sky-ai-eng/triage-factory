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

// eventHandlerStore is the unified Postgres impl of db.EventHandlerStore.
// Replaces taskRuleStore + triggerStore from the prior stores.
//
// Per-kind fields are nullable on the column level; the per-kind CHECK
// constraints on event_handlers enforce the shape pair (rule populates
// name/default_priority/sort_order, trigger populates blueprint_id/
// breaker_threshold/min_autonomy_suitability). This impl branches on
// the row's Kind where the SQL diverges.
//
// # Pool split (unchanged from the predecessor stores)
//
//   - app   — tf_app, RLS-active. Every CRUD method runs here.
//   - admin — supabase_admin, BYPASSRLS. Seed runs here because the
//     event_handlers_insert / event_handlers_update RLS policies gate
//     on creator_user_id = tf.current_user_id() (or admin-on-org for
//     UPDATE); boot-time Seed has no JWT claims and would otherwise
//     fail the WITH CHECK on every shipped row.
//
// Inside WithTx both fields point at the same *sql.Tx, and inTx is
// true. Seed inside WithTx is rejected: escaping to the admin pool
// would break the caller's transaction scope (matches PromptStore +
// the predecessor stores).
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

// pgEventHandlerColumns mirrors the unified row. Per-kind nullable
// columns (name + default_priority + sort_order for rules; blueprint_id
// + breaker_threshold + min_autonomy_suitability for triggers) are
// scanned via sql.Null* and mapped to the domain type's pointer fields.
const pgEventHandlerColumns = `id, kind, event_type, scope_predicate_json::text, enabled, source,
       team_id, applies_to_unowned,
       name, default_priority, sort_order,
       blueprint_id, breaker_threshold, min_autonomy_suitability,
       user_modified, created_at, updated_at`

func (s *eventHandlerStore) Seed(ctx context.Context, orgID, teamID string, blueprintIDsBySlug map[string]string) error {
	if s.inTx {
		return errors.New("postgres event_handlers: Seed must not be called inside WithTx; call stores.EventHandlers.Seed directly")
	}
	if teamID == "" {
		return errors.New("postgres event_handlers Seed: teamID required (shipped handlers are team-scoped; caller threads the target team — local mode passes runmode.LocalDefaultTeamID, multi mode passes the new team's UUID on team creation)")
	}
	now := time.Now().UTC()
	var inserted int64
	for _, h := range db.ShippedEventHandlers {
		var pred any
		if h.Predicate != "" {
			pred = h.Predicate
		}

		// System rows materialize as visibility='team' with team_id=teamID.
		// event_handlers_system_has_no_creator allows creator_user_id NULL
		// on source='system'. The id is a random UUID per team copy and h.ID
		// is the system_slug; re-seed dedupes on (org_id, team_id,
		// system_slug) via ON CONFLICT.
		switch h.Kind {
		case domain.EventHandlerKindRule:
			res, err := s.admin.ExecContext(ctx, `
				INSERT INTO event_handlers
					(id, org_id, team_id, creator_user_id, kind, event_type,
					 system_slug, scope_predicate_json, enabled, source,
					 name, default_priority, sort_order,
					 created_at, updated_at)
				VALUES (
					$1, $2, $3::uuid, NULL, 'rule', $4,
					$5, $6::jsonb, TRUE, 'system',
					$7, $8, $9,
					$10, $10
				)
				ON CONFLICT (org_id, team_id, system_slug) DO NOTHING
			`, uuid.New().String(), orgID, teamID, h.EventType,
				h.ID, pred, h.Name, h.DefaultPriority, h.SortOrder, now)
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
			// than write a dangling reference (the same-team FK would reject
			// it anyway).
			blueprintID, ok := blueprintIDsBySlug[h.BlueprintID]
			if !ok || blueprintID == "" {
				return fmt.Errorf("seed event_handler trigger %s: blueprint slug %q not found in blueprintIDsBySlug (seed prompts before handlers)", h.ID, h.BlueprintID)
			}
			// Trigger: blueprint_id + breaker_threshold +
			// min_autonomy_suitability populated; rule-only columns NULL.
			// Shipped triggers ship disabled (project convention —
			// users opt in).
			res, err := s.admin.ExecContext(ctx, `
				INSERT INTO event_handlers
					(id, org_id, team_id, creator_user_id, kind, event_type,
					 system_slug, scope_predicate_json, enabled, source,
					 blueprint_id, breaker_threshold, min_autonomy_suitability,
					 created_at, updated_at)
				VALUES (
					$1, $2, $3::uuid, NULL, 'trigger', $4,
					$5, $6::jsonb, FALSE, 'system',
					$7, $8, $9,
					$10, $10
				)
				ON CONFLICT (org_id, team_id, system_slug) DO NOTHING
			`, uuid.New().String(), orgID, teamID, h.EventType,
				h.ID, pred, blueprintID, h.BreakerThreshold, h.MinAutonomySuitability, now)
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
	dbPgLog.Info("seeded event_handlers", "inserted", inserted, "existed", int64(len(db.ShippedEventHandlers))-inserted)
	return nil
}

func (s *eventHandlerStore) List(ctx context.Context, orgID string, kind string, teamID string) ([]domain.EventHandler, error) {
	query, args := s.buildListQuery(orgID, kind, teamID)
	return s.scanList(ctx, query, args)
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

// GetBySystemSlug resolves a team's copy of a shipped handler by its stable
// system_slug. Returns (nil, nil) when the team has no copy or its copy is
// soft-deleted.
func (s *eventHandlerStore) GetBySystemSlug(ctx context.Context, orgID, teamID, systemSlug string) (*domain.EventHandler, error) {
	if teamID == "" {
		return nil, errors.New("postgres event_handlers GetBySystemSlug: teamID required")
	}
	row := s.app.QueryRowContext(ctx, `
		SELECT `+pgEventHandlerColumns+`
		FROM event_handlers
		WHERE org_id = $1 AND team_id = $2::uuid AND system_slug = $3 AND deleted_at IS NULL
	`, orgID, teamID, systemSlug)
	h, err := scanEventHandlerRowPG(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &h, nil
}

func (s *eventHandlerStore) GetEnabledForEvent(ctx context.Context, orgID, eventType string) ([]domain.EventHandler, error) {
	return getEnabledEventHandlers(ctx, s.app, orgID, eventType)
}

func (s *eventHandlerStore) GetEnabledForEventSystem(ctx context.Context, orgID, eventType string) ([]domain.EventHandler, error) {
	return getEnabledEventHandlers(ctx, s.admin, orgID, eventType)
}

func getEnabledEventHandlers(ctx context.Context, q queryer, orgID, eventType string) ([]domain.EventHandler, error) {
	// kind ordering: 'rule' < 'trigger' alphabetically, so a plain
	// ORDER BY kind ASC keeps the rules-before-triggers invariant
	// that the router relies on (same observable behavior as the
	// pre-unification two-phase loop). sort_order then breaks ties
	// among rules; created_at DESC orders triggers.
	rows, err := q.QueryContext(ctx, `
		SELECT `+pgEventHandlerColumns+`
		FROM event_handlers
		WHERE org_id = $1 AND event_type = $2 AND enabled = TRUE AND deleted_at IS NULL
		ORDER BY kind ASC,
		         CASE WHEN kind = 'rule' THEN sort_order ELSE 0 END ASC,
		         created_at DESC
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
		WHERE org_id = $1 AND blueprint_id = $2 AND kind = 'trigger' AND deleted_at IS NULL
		ORDER BY created_at DESC
	`, orgID, blueprintID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectEventHandlers(rows)
}

func (s *eventHandlerStore) Create(ctx context.Context, orgID, teamID string, h domain.EventHandler) error {
	if err := db.ValidateEventHandlerForCreate(&h); err != nil {
		return err
	}
	var pred any
	if h.ScopePredicateJSON != nil {
		pred = *h.ScopePredicateJSON
	}

	// team_id is the acting team the handler resolved for this request —
	// no "first/any team in org" fallback. team_id is NOT NULL (the sole
	// scoping signal; no visibility column) and the
	// event_handlers_insert RLS gates on tf.user_in_team(team_id), so a
	// real team here keeps the write valid. Empty is a handler bug, so
	// reject it rather than write an invalid row.
	if teamID == "" {
		return fmt.Errorf("postgres event_handlers Create: team_id required (handler must thread the resolved acting team from request context)")
	}
	switch h.Kind {
	case domain.EventHandlerKindRule:
		_, err := s.app.ExecContext(ctx, `
			INSERT INTO event_handlers
				(id, org_id, creator_user_id, team_id, kind, event_type,
				 scope_predicate_json, enabled, source, applies_to_unowned,
				 name, default_priority, sort_order,
				 created_at, updated_at)
			VALUES (
				$1, $2,
				COALESCE(tf.current_user_id(), (SELECT owner_user_id FROM orgs WHERE id = $2)),
				$3::uuid,
				'rule', $4,
				$5::jsonb, $6, 'user', $7,
				$8, $9, $10,
				now(), now()
			)
		`, h.ID, orgID, teamID, h.EventType, pred, h.Enabled, h.AppliesToUnowned,
			h.Name, derefFloat(h.DefaultPriority), derefInt(h.SortOrder))
		return err

	case domain.EventHandlerKindTrigger:
		_, err := s.app.ExecContext(ctx, `
			INSERT INTO event_handlers
				(id, org_id, creator_user_id, team_id, kind, event_type,
				 scope_predicate_json, enabled, source, applies_to_unowned,
				 blueprint_id, breaker_threshold, min_autonomy_suitability,
				 created_at, updated_at)
			VALUES (
				$1, $2,
				COALESCE(tf.current_user_id(), (SELECT owner_user_id FROM orgs WHERE id = $2)),
				$3::uuid,
				'trigger', $4,
				$5::jsonb, $6, 'user', $7,
				$8, $9, $10,
				now(), now()
			)
		`, h.ID, orgID, teamID, h.EventType, pred, h.Enabled, h.AppliesToUnowned,
			h.BlueprintID, derefInt(h.BreakerThreshold), derefFloat(h.MinAutonomySuitability))
		return err
	}
	return fmt.Errorf("postgres event_handlers Create: unknown kind %q", h.Kind)
}

func (s *eventHandlerStore) Update(ctx context.Context, orgID string, h domain.EventHandler) error {
	if !isValidUUID(h.ID) {
		return nil
	}
	if err := db.ValidateEventHandlerForCreate(&h); err != nil {
		return err
	}
	var pred any
	if h.ScopePredicateJSON != nil {
		pred = *h.ScopePredicateJSON
	}

	contentChanged, err := s.contentChanged(ctx, orgID, h)
	if err != nil {
		return err
	}

	switch h.Kind {
	case domain.EventHandlerKindRule:
		_, err := s.app.ExecContext(ctx, `
			UPDATE event_handlers
			SET scope_predicate_json = $1::jsonb, enabled = $2, applies_to_unowned = $3,
			    name = $4, default_priority = $5, sort_order = $6,
			    user_modified = user_modified OR $7,
			    updated_at = now()
			WHERE org_id = $8 AND id = $9 AND kind = 'rule' AND deleted_at IS NULL
		`, pred, h.Enabled, h.AppliesToUnowned, h.Name,
			derefFloat(h.DefaultPriority), derefInt(h.SortOrder),
			contentChanged,
			orgID, h.ID)
		return err

	case domain.EventHandlerKindTrigger:
		// blueprint_id is immutable on trigger update — change requires
		// RetargetBlueprint.
		_, err := s.app.ExecContext(ctx, `
			UPDATE event_handlers
			SET scope_predicate_json = $1::jsonb, enabled = $2, applies_to_unowned = $3,
			    breaker_threshold = $4, min_autonomy_suitability = $5,
			    user_modified = user_modified OR $6,
			    updated_at = now()
			WHERE org_id = $7 AND id = $8 AND kind = 'trigger' AND deleted_at IS NULL
		`, pred, h.Enabled, h.AppliesToUnowned,
			derefInt(h.BreakerThreshold), derefFloat(h.MinAutonomySuitability),
			contentChanged,
			orgID, h.ID)
		return err
	}
	return fmt.Errorf("postgres event_handlers Update: unknown kind %q", h.Kind)
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
	err := s.app.QueryRowContext(ctx, `
		SELECT scope_predicate_json::text, name, default_priority, breaker_threshold, min_autonomy_suitability
		FROM event_handlers WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL
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

func (s *eventHandlerStore) SetEnabled(ctx context.Context, orgID, id string, enabled bool) error {
	if !isValidUUID(id) {
		return nil
	}
	_, err := s.app.ExecContext(ctx, `
		UPDATE event_handlers SET enabled = $1, updated_at = now() WHERE org_id = $2 AND id = $3 AND deleted_at IS NULL
	`, enabled, orgID, id)
	return err
}

// Delete removes a handler. A system_slug row (shipped copy) soft-deletes so
// its (org_id, team_id, system_slug) slot stays occupied for the sync's
// never-resurrect rule; a user-created row (no system_slug) hard-deletes.
func (s *eventHandlerStore) Delete(ctx context.Context, orgID, id string) error {
	if !isValidUUID(id) {
		return nil
	}
	var systemSlug sql.NullString
	switch err := s.app.QueryRowContext(ctx,
		`SELECT system_slug FROM event_handlers WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL`, orgID, id,
	).Scan(&systemSlug); {
	case errors.Is(err, sql.ErrNoRows):
		return nil // already gone; Delete is idempotent
	case err != nil:
		return err
	}
	if systemSlug.Valid && systemSlug.String != "" {
		_, err := s.app.ExecContext(ctx,
			`UPDATE event_handlers SET deleted_at = now() WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL`,
			orgID, id)
		return err
	}
	_, err := s.app.ExecContext(ctx, `DELETE FROM event_handlers WHERE org_id = $1 AND id = $2`, orgID, id)
	return err
}

func (s *eventHandlerStore) RetargetBlueprint(ctx context.Context, orgID, id, newBlueprintID string) error {
	if !isValidUUID(id) || !isValidUUID(newBlueprintID) {
		return nil
	}
	_, err := s.app.ExecContext(ctx, `
		UPDATE event_handlers SET blueprint_id = $1, user_modified = TRUE, updated_at = now()
		WHERE org_id = $2 AND id = $3 AND kind = 'trigger' AND deleted_at IS NULL
	`, newBlueprintID, orgID, id)
	return err
}

func (s *eventHandlerStore) Reorder(ctx context.Context, orgID string, ids []string) error {
	return s.runInTx(ctx, func(tx *sql.Tx) error {
		for i, id := range ids {
			if !isValidUUID(id) {
				continue
			}
			// kind='rule' filter ensures trigger IDs in the list are
			// silently skipped — sort_order is rule-only by CHECK
			// constraint and a trigger row's sort_order is NULL.
			if _, err := tx.ExecContext(ctx, `
				UPDATE event_handlers SET sort_order = $1, updated_at = now()
				WHERE org_id = $2 AND id = $3 AND kind = 'rule' AND deleted_at IS NULL
			`, i, orgID, id); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *eventHandlerStore) Promote(ctx context.Context, orgID string, id string, t domain.EventHandler) error {
	if !isValidUUID(id) {
		return errors.New("postgres event_handlers Promote: invalid id")
	}
	if t.Kind != domain.EventHandlerKindTrigger {
		return errors.New("postgres event_handlers Promote: target kind must be 'trigger'")
	}
	if t.BlueprintID == "" || t.BreakerThreshold == nil || t.MinAutonomySuitability == nil {
		return errors.New("postgres event_handlers Promote: trigger fields required (blueprint_id, breaker_threshold, min_autonomy_suitability)")
	}
	var pred any
	if t.ScopePredicateJSON != nil {
		pred = *t.ScopePredicateJSON
	}
	// Single UPDATE: clear rule-only fields, populate trigger-only,
	// flip kind. The per-kind CHECK constraints validate atomically —
	// any mid-state would fail the rule_shape or trigger_shape check. A kind
	// change is always a user modification — stamp unconditionally.
	res, err := s.app.ExecContext(ctx, `
		UPDATE event_handlers
		SET kind = 'trigger',
		    blueprint_id = $1, breaker_threshold = $2, min_autonomy_suitability = $3,
		    name = NULL, default_priority = NULL, sort_order = NULL,
		    scope_predicate_json = $4::jsonb,
		    user_modified = TRUE,
		    updated_at = now()
		WHERE org_id = $5 AND id = $6 AND kind = 'rule' AND deleted_at IS NULL
	`, t.BlueprintID, *t.BreakerThreshold, *t.MinAutonomySuitability,
		pred, orgID, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("postgres event_handlers Promote: row not found or not a rule")
	}
	return nil
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
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin admin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	row, err := loadTeamHandlerRowPG(ctx, tx, orgID, teamID, h.ID)
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
		return nil // nothing written; rollback is a no-op
	case db.HandlerInsert:
		if err := insertHandlerPG(ctx, tx, orgID, teamID, h, plan); err != nil {
			return err
		}
	case db.HandlerApply:
		if err := applyHandlerPG(ctx, tx, orgID, row.ID, h.Kind, plan); err != nil {
			return err
		}
	default:
		return nil
	}
	return tx.Commit()
}

// loadTeamHandlerRowPG reads the team's row for one shipped handler's
// system_slug, including a soft-deleted occupant (Deleted=true). Exists=false
// when no row.
func loadTeamHandlerRowPG(ctx context.Context, q queryer, orgID, teamID, slug string) (db.TeamHandlerRow, error) {
	var (
		id, blueprintID string
		mod, del        bool
		pred, name      sql.NullString
		defPriority     sql.NullFloat64
		breaker         sql.NullInt64
		minAutonomy     sql.NullFloat64
	)
	switch err := q.QueryRowContext(ctx, `
		SELECT id, user_modified, (deleted_at IS NOT NULL),
		       scope_predicate_json::text, name, default_priority,
		       COALESCE(blueprint_id, ''), breaker_threshold, min_autonomy_suitability
		FROM event_handlers WHERE org_id = $1 AND team_id = $2::uuid AND system_slug = $3
	`, orgID, teamID, slug).Scan(&id, &mod, &del, &pred, &name, &defPriority, &blueprintID, &breaker, &minAutonomy); {
	case errors.Is(err, sql.ErrNoRows):
		return db.TeamHandlerRow{Exists: false}, nil
	case err != nil:
		return db.TeamHandlerRow{}, fmt.Errorf("read handler %s: %w", slug, err)
	}
	row := db.TeamHandlerRow{Exists: true, Deleted: del, UserModified: mod, ID: id, BlueprintID: blueprintID}
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

// insertHandlerPG inserts a brand-new team copy of a shipped handler,
// honoring the shipped enabled convention (rules enabled, triggers disabled)
// — matches Seed.
func insertHandlerPG(ctx context.Context, q queryer, orgID, teamID string, h db.ShippedEventHandler, plan db.HandlerSyncPlan) error {
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
			VALUES (
				$1, $2, $3::uuid, NULL, 'rule', $4,
				$5, $6::jsonb, TRUE, 'system',
				$7, $8, $9,
				$10, $10
			)
		`, uuid.New().String(), orgID, teamID, h.EventType, h.ID, pred, plan.Name, plan.DefaultPriority, plan.SortOrder, now)
		return err
	case domain.EventHandlerKindTrigger:
		_, err := q.ExecContext(ctx, `
			INSERT INTO event_handlers
				(id, org_id, team_id, creator_user_id, kind, event_type,
				 system_slug, scope_predicate_json, enabled, source,
				 blueprint_id, breaker_threshold, min_autonomy_suitability,
				 created_at, updated_at)
			VALUES (
				$1, $2, $3::uuid, NULL, 'trigger', $4,
				$5, $6::jsonb, FALSE, 'system',
				$7, $8, $9,
				$10, $10
			)
		`, uuid.New().String(), orgID, teamID, h.EventType, h.ID, pred, plan.BlueprintID, plan.BreakerThreshold, plan.MinAutonomySuitability, now)
		return err
	}
	return fmt.Errorf("postgres event_handlers Sync: insert: unknown kind %q", h.Kind)
}

// applyHandlerPG rewrites an existing row's diverging content fields. Never
// touches enabled or sort_order.
func applyHandlerPG(ctx context.Context, q queryer, orgID, id string, kind string, plan db.HandlerSyncPlan) error {
	var pred any
	if plan.Predicate != "" {
		pred = plan.Predicate
	}
	switch kind {
	case domain.EventHandlerKindRule:
		_, err := q.ExecContext(ctx, `
			UPDATE event_handlers
			SET scope_predicate_json = $1::jsonb, name = $2, default_priority = $3, updated_at = now()
			WHERE org_id = $4 AND id = $5
		`, pred, plan.Name, plan.DefaultPriority, orgID, id)
		return err
	case domain.EventHandlerKindTrigger:
		_, err := q.ExecContext(ctx, `
			UPDATE event_handlers
			SET scope_predicate_json = $1::jsonb, blueprint_id = $2, breaker_threshold = $3, min_autonomy_suitability = $4, updated_at = now()
			WHERE org_id = $5 AND id = $6
		`, pred, plan.BlueprintID, plan.BreakerThreshold, plan.MinAutonomySuitability, orgID, id)
		return err
	}
	return fmt.Errorf("postgres event_handlers Sync: apply: unknown kind %q", kind)
}

// buildListQuery composes the WHERE for List, with optional kind filter.
// kind="" returns both kinds.
func (s *eventHandlerStore) buildListQuery(orgID, kind, teamID string) (string, []any) {
	args := []any{orgID}
	q := `SELECT ` + pgEventHandlerColumns + `
	      FROM event_handlers
	      WHERE org_id = $1 AND deleted_at IS NULL`
	if kind != "" {
		args = append(args, kind)
		q += fmt.Sprintf(" AND kind = $%d", len(args))
	}
	if teamID != "" {
		// Prompts page narrowed to one team: that team's handlers. Every
		// handler is team-owned (team_id NOT NULL, no org-visible tier), so
		// this is a plain team filter. RLS still gates the row set; this
		// narrows within it.
		args = append(args, teamID)
		q += fmt.Sprintf(" AND team_id = $%d", len(args))
	}
	// Order: rules first (sort_order ASC, name ASC), then triggers
	// (created_at DESC). Same shape as the predecessor stores' List
	// methods so handler-level callers get identical ordering.
	q += `
	      ORDER BY kind ASC,
	               CASE WHEN kind = 'rule' THEN sort_order ELSE 0 END ASC,
	               CASE WHEN kind = 'rule' THEN name ELSE '' END ASC,
	               created_at DESC`
	return q, args
}

func (s *eventHandlerStore) scanList(ctx context.Context, query string, args []any) ([]domain.EventHandler, error) {
	rows, err := s.app.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectEventHandlers(rows)
}

func (s *eventHandlerStore) runInTx(ctx context.Context, fn func(*sql.Tx) error) error {
	switch v := s.app.(type) {
	case *sql.Tx:
		return fn(v)
	case *sql.DB:
		tx, err := v.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := fn(tx); err != nil {
			return err
		}
		return tx.Commit()
	default:
		return errors.New("postgres event_handlers: unexpected queryer type")
	}
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

// scanEventHandlerFromAny is the shared row decoder. Per-kind nullable
// columns scan into sql.Null* / sql.NullString then map onto the
// domain pointer fields; rule rows have NULL trigger-only fields and
// vice versa.
func scanEventHandlerFromAny(scanFn func(dst ...any) error) (domain.EventHandler, error) {
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
	// TriggerType is not stored in event_handlers (v1 was always
	// 'event'); set it on read so downstream code that still inspects
	// h.TriggerType behaves identically to the pre-unification shape.
	if h.Kind == domain.EventHandlerKindTrigger {
		h.TriggerType = domain.TriggerTypeEvent
	}
	return h, nil
}

// derefFloat / derefInt unwrap nullable domain fields for INSERTs that
// have already passed db.ValidateEventHandlerForCreate (guaranteed non-nil
// for the kind's required fields).
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
