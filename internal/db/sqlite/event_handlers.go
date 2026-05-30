package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// eventHandlerStore is the SQLite impl of db.EventHandlerStore. Post-
// SKY-269 the SQLite schema carries org_id structurally, so every
// method filters by org_id (matching the Postgres impl's WHERE pattern)
// rather than the older assertLocalOrg convention the predecessor
// stores used.
//
// Per-kind CHECK constraints on event_handlers enforce the shape
// pair at the DB level; this store branches on Kind where the SQL
// diverges (Create / Update / Seed write different column sets per
// kind).
// users is plumbed in so Seed can read users.github_username through the
// canonical store (gets any forward-compat behavior added there for
// free) rather than duplicating the SELECT here.
type eventHandlerStore struct {
	q     queryer
	users db.UsersStore
}

func newEventHandlerStore(q queryer, users db.UsersStore) db.EventHandlerStore {
	return &eventHandlerStore{q: q, users: users}
}

var _ db.EventHandlerStore = (*eventHandlerStore)(nil)

// sqliteEventHandlerColumns mirrors the Postgres projection so scan
// helpers stay aligned. Per-kind nullable columns scan into sql.Null*
// and map to the domain pointer fields.
const sqliteEventHandlerColumns = `id, kind, event_type, scope_predicate_json, enabled, source,
       team_id,
       name, default_priority, sort_order,
       prompt_id, breaker_threshold, min_autonomy_suitability,
       created_at, updated_at`

func (s *eventHandlerStore) Seed(ctx context.Context, orgID, teamID string) error {
	if teamID == "" {
		return fmt.Errorf("sqlite event_handlers Seed: teamID required (SKY-295: shipped rules are team-scoped; local mode passes runmode.LocalDefaultTeamID)")
	}
	now := time.Now().UTC()
	var inserted int64

	// SKY-264: ShippedEventHandlers carry `author_in: []` as a placeholder
	// where the rule scopes to "my events." In local mode we know exactly
	// who "me" is — the synthetic user's github_username — so substitute
	// it into the empty allowlist at seed time. INSERT OR IGNORE below
	// means this only takes effect on first install / clean-slate;
	// existing installs preserve whatever the user has saved via the
	// Settings UI.
	//
	// Empty username (user hasn't connected GitHub yet) leaves the
	// allowlist empty — the rule fires for everyone, which is the
	// match-all default. User can edit the rule once they connect.
	// Best-effort: a missing row (fresh install pre-bootstrap) returns
	// empty and substituteLocalGitHubIdentity degrades to leaving the
	// placeholder allowlist empty.
	localGitHubUsername, _ := s.users.GetGitHubUsername(ctx, runmode.LocalDefaultUserID)

	// SKY-270: same shape for Jira. The shipped jira-assigned and
	// jira-became-atomic rules ship with `assignee_in: []` placeholders;
	// fill them with the local user's Atlassian account ID so the rule
	// matches without manual setup. Empty value (Jira not connected yet)
	// leaves the allowlist empty → rule matches every assignee, matching
	// the GitHub side's degrade-cleanly default.
	localJiraAccountID, _, _ := s.users.GetJiraIdentity(ctx, runmode.LocalDefaultUserID)

	for _, h := range db.ShippedEventHandlers {
		predStr := substituteLocalGitHubIdentity(h.Predicate, localGitHubUsername)
		predStr = substituteLocalJiraIdentity(predStr, localJiraAccountID)
		var pred any
		if predStr != "" {
			pred = predStr
		}

		// SKY-295: system rows materialize as visibility='team' with
		// team_id=teamID. The event_handlers_system_has_no_creator
		// CHECK still allows creator_user_id NULL when source='system',
		// and team_visibility_requires_team is satisfied because team_id
		// is non-NULL.
		switch h.Kind {
		case domain.EventHandlerKindRule:
			res, err := s.q.ExecContext(ctx, `
				INSERT OR IGNORE INTO event_handlers
					(id, org_id, team_id, creator_user_id, visibility, kind, event_type,
					 scope_predicate_json, enabled, source,
					 name, default_priority, sort_order,
					 created_at, updated_at)
				VALUES (?, ?, ?, NULL, 'team', 'rule', ?,
				        ?, 1, 'system',
				        ?, ?, ?,
				        ?, ?)
			`, h.ID, orgID, teamID, h.EventType,
				pred,
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
			// Shipped triggers ship disabled (project convention —
			// users opt in or replace).
			res, err := s.q.ExecContext(ctx, `
				INSERT OR IGNORE INTO event_handlers
					(id, org_id, team_id, creator_user_id, visibility, kind, event_type,
					 scope_predicate_json, enabled, source,
					 prompt_id, breaker_threshold, min_autonomy_suitability,
					 created_at, updated_at)
				VALUES (?, ?, ?, NULL, 'team', 'trigger', ?,
				        ?, 0, 'system',
				        ?, ?, ?,
				        ?, ?)
			`, h.ID, orgID, teamID, h.EventType,
				pred,
				h.PromptID, h.BreakerThreshold, h.MinAutonomySuitability,
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
	log.Printf("[db] seeded %d new event_handlers (%d already existed)", inserted, int64(len(db.ShippedEventHandlers))-inserted)
	return nil
}

func (s *eventHandlerStore) List(ctx context.Context, orgID string, kind string) ([]domain.EventHandler, error) {
	q := `SELECT ` + sqliteEventHandlerColumns + ` FROM event_handlers WHERE org_id = ?`
	args := []any{orgID}
	if kind != "" {
		q += ` AND kind = ?`
		args = append(args, kind)
	}
	q += `
	      ORDER BY kind ASC,
	               CASE WHEN kind = 'rule' THEN sort_order ELSE 0 END ASC,
	               CASE WHEN kind = 'rule' THEN name ELSE '' END ASC,
	               created_at DESC`
	rows, err := s.q.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectEventHandlersSQLite(rows)
}

func (s *eventHandlerStore) Get(ctx context.Context, orgID, id string) (*domain.EventHandler, error) {
	row := s.q.QueryRowContext(ctx, `
		SELECT `+sqliteEventHandlerColumns+`
		FROM event_handlers
		WHERE org_id = ? AND id = ?
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

func (s *eventHandlerStore) GetEnabledForEvent(ctx context.Context, orgID, eventType string) ([]domain.EventHandler, error) {
	// Rules-before-triggers order (same as Postgres impl) — preserves
	// the pre-unification observable shape.
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+sqliteEventHandlerColumns+`
		FROM event_handlers
		WHERE org_id = ? AND event_type = ? AND enabled = 1
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

func (s *eventHandlerStore) ListForPrompt(ctx context.Context, orgID, promptID string) ([]domain.EventHandler, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+sqliteEventHandlerColumns+`
		FROM event_handlers
		WHERE org_id = ? AND prompt_id = ? AND kind = 'trigger'
		ORDER BY created_at DESC
	`, orgID, promptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectEventHandlersSQLite(rows)
}

func (s *eventHandlerStore) Create(ctx context.Context, orgID, teamID string, h domain.EventHandler) error {
	if err := db.ValidateEventHandlerForCreate(&h); err != nil {
		return err
	}
	_ = teamID // local mode is single-team; rows pin LocalDefaultTeamID below
	var pred any
	if h.ScopePredicateJSON != nil {
		pred = *h.ScopePredicateJSON
	}
	now := time.Now().UTC()

	// user-source rows are team-scoped with team_id = LocalDefaultTeamID
	// + visibility='team'. SQLite has one team in local mode (the
	// sentinel) so the acting team is always that team — the teamID the
	// handler threads is ignored here. creator_user_id is required for
	// source='user' rows by the event_handlers_system_has_no_creator
	// CHECK.
	switch h.Kind {
	case domain.EventHandlerKindRule:
		_, err := s.q.ExecContext(ctx, `
			INSERT INTO event_handlers
				(id, org_id, team_id, creator_user_id, visibility, kind, event_type,
				 scope_predicate_json, enabled, source,
				 name, default_priority, sort_order,
				 created_at, updated_at)
			VALUES (?, ?, ?, ?, 'team', 'rule', ?,
			        ?, ?, 'user',
			        ?, ?, ?,
			        ?, ?)
		`, h.ID, orgID, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID, h.EventType,
			pred, h.Enabled,
			h.Name, derefFloat(h.DefaultPriority), derefInt(h.SortOrder),
			now, now)
		return err

	case domain.EventHandlerKindTrigger:
		_, err := s.q.ExecContext(ctx, `
			INSERT INTO event_handlers
				(id, org_id, team_id, creator_user_id, visibility, kind, event_type,
				 scope_predicate_json, enabled, source,
				 prompt_id, breaker_threshold, min_autonomy_suitability,
				 created_at, updated_at)
			VALUES (?, ?, ?, ?, 'team', 'trigger', ?,
			        ?, ?, 'user',
			        ?, ?, ?,
			        ?, ?)
		`, h.ID, orgID, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID, h.EventType,
			pred, h.Enabled,
			h.PromptID, derefInt(h.BreakerThreshold), derefFloat(h.MinAutonomySuitability),
			now, now)
		return err
	}
	return fmt.Errorf("sqlite event_handlers Create: unknown kind %q", h.Kind)
}

func (s *eventHandlerStore) Update(ctx context.Context, orgID string, h domain.EventHandler) error {
	if err := db.ValidateEventHandlerForCreate(&h); err != nil {
		return err
	}
	var pred any
	if h.ScopePredicateJSON != nil {
		pred = *h.ScopePredicateJSON
	}
	now := time.Now().UTC()

	switch h.Kind {
	case domain.EventHandlerKindRule:
		_, err := s.q.ExecContext(ctx, `
			UPDATE event_handlers
			SET scope_predicate_json = ?, enabled = ?,
			    name = ?, default_priority = ?, sort_order = ?,
			    updated_at = ?
			WHERE org_id = ? AND id = ? AND kind = 'rule'
		`, pred, h.Enabled,
			h.Name, derefFloat(h.DefaultPriority), derefInt(h.SortOrder),
			now, orgID, h.ID)
		return err

	case domain.EventHandlerKindTrigger:
		_, err := s.q.ExecContext(ctx, `
			UPDATE event_handlers
			SET scope_predicate_json = ?, enabled = ?,
			    breaker_threshold = ?, min_autonomy_suitability = ?,
			    updated_at = ?
			WHERE org_id = ? AND id = ? AND kind = 'trigger'
		`, pred, h.Enabled,
			derefInt(h.BreakerThreshold), derefFloat(h.MinAutonomySuitability),
			now, orgID, h.ID)
		return err
	}
	return fmt.Errorf("sqlite event_handlers Update: unknown kind %q", h.Kind)
}

func (s *eventHandlerStore) SetEnabled(ctx context.Context, orgID, id string, enabled bool) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE event_handlers SET enabled = ?, updated_at = ? WHERE org_id = ? AND id = ?
	`, enabled, time.Now().UTC(), orgID, id)
	return err
}

func (s *eventHandlerStore) Delete(ctx context.Context, orgID, id string) error {
	_, err := s.q.ExecContext(ctx, `DELETE FROM event_handlers WHERE org_id = ? AND id = ?`, orgID, id)
	return err
}

func (s *eventHandlerStore) Reorder(ctx context.Context, orgID string, ids []string) error {
	return inTx(ctx, s.q, func(q queryer) error {
		now := time.Now().UTC()
		for i, id := range ids {
			if _, err := q.ExecContext(ctx, `
				UPDATE event_handlers SET sort_order = ?, updated_at = ?
				WHERE org_id = ? AND id = ? AND kind = 'rule'
			`, i, now, orgID, id); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *eventHandlerStore) Promote(ctx context.Context, orgID string, id string, t domain.EventHandler) error {
	if t.Kind != domain.EventHandlerKindTrigger {
		return errors.New("sqlite event_handlers Promote: target kind must be 'trigger'")
	}
	if t.PromptID == "" || t.BreakerThreshold == nil || t.MinAutonomySuitability == nil {
		return errors.New("sqlite event_handlers Promote: trigger fields required")
	}
	var pred any
	if t.ScopePredicateJSON != nil {
		pred = *t.ScopePredicateJSON
	}
	// Single UPDATE flips kind, clears rule-only columns, populates
	// trigger-only. The per-kind CHECK constraints validate atomically.
	res, err := s.q.ExecContext(ctx, `
		UPDATE event_handlers
		SET kind = 'trigger',
		    prompt_id = ?, breaker_threshold = ?, min_autonomy_suitability = ?,
		    name = NULL, default_priority = NULL, sort_order = NULL,
		    scope_predicate_json = ?,
		    updated_at = ?
		WHERE org_id = ? AND id = ? AND kind = 'rule'
	`, t.PromptID, *t.BreakerThreshold, *t.MinAutonomySuitability,
		pred, time.Now().UTC(),
		orgID, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("sqlite event_handlers Promote: row not found or not a rule")
	}
	return nil
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
		promptID      sql.NullString
		breakerNS     sql.NullInt64
		minAutonomyNS sql.NullFloat64
	)
	if err := scanFn(
		&h.ID, &h.Kind, &h.EventType, &pred, &h.Enabled, &h.Source,
		&teamID,
		&nameNS, &defPriority, &sortOrder,
		&promptID, &breakerNS, &minAutonomyNS,
		&h.CreatedAt, &h.UpdatedAt,
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
	if promptID.Valid {
		h.PromptID = promptID.String
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

// substituteLocalGitHubIdentity rewrites a seed-time predicate so empty
// allowlist fields (`author_in: []`, `reviewer_in: []`, `commenter_in: []`)
// become single-entry allowlists scoped to the local user. Local-mode
// callers pass `localUser` = users.github_username for the synthetic
// LocalDefaultUserID; multi-mode never calls this (Postgres Seed inserts
// the shipped predicates verbatim).
//
// Pre-filled allowlists (non-empty arrays) are left untouched — that's
// the user's customization and Seed's INSERT OR IGNORE shouldn't
// stomp it. Malformed predicate JSON falls through unchanged; the
// downstream matcher already fails-closed on undecodable predicates.
func substituteLocalGitHubIdentity(predJSON, localUser string) string {
	return substituteEmptyAllowlists(predJSON, localUser, "author_in", "reviewer_in", "commenter_in")
}

// substituteLocalJiraIdentity is the Jira analog of
// substituteLocalGitHubIdentity. Shipped Jira-scoped rules carry
// `assignee_in: []` placeholders; this fills them with the local user's
// jira_account_id at seed time. Same semantics as the GitHub helper —
// non-empty arrays are user-customizations and preserved; missing keys
// stay missing; malformed JSON falls through unchanged.
//
// Reporter / commenter allowlists are not currently shipped on any
// system handler (they're predicate-only — exposed in the rule editor
// for user-authored rules), so this helper covers `assignee_in` only.
// Adding more keys here is the right extension if reporter / commenter
// system handlers ever ship.
func substituteLocalJiraIdentity(predJSON, localAccountID string) string {
	return substituteEmptyAllowlists(predJSON, localAccountID, "assignee_in")
}

// substituteEmptyAllowlists is the shared body for the GitHub/Jira
// substitution helpers. For each named key, if the predicate has the
// key set to an empty array, replace it with `[identity]`. Pre-filled
// arrays are preserved; missing keys are ignored; malformed predicate
// JSON falls through unchanged.
func substituteEmptyAllowlists(predJSON, identity string, keys ...string) string {
	if predJSON == "" || identity == "" {
		return predJSON
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(predJSON), &fields); err != nil {
		return predJSON
	}
	changed := false
	for _, key := range keys {
		raw, ok := fields[key]
		if !ok {
			continue
		}
		var arr []string
		if err := json.Unmarshal(raw, &arr); err != nil {
			// Field exists but isn't a string slice — leave it alone,
			// the validator will reject the row anyway.
			continue
		}
		if len(arr) != 0 {
			// User-customized; preserve.
			continue
		}
		substituted, err := json.Marshal([]string{identity})
		if err != nil {
			continue
		}
		fields[key] = substituted
		changed = true
	}
	if !changed {
		return predJSON
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return predJSON
	}
	return string(out)
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
