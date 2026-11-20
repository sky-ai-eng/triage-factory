package db

import (
	"database/sql"
	"errors"
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The migration under test, and the one before it, whose schema the tests
// seed.
const (
	eventHandlerModes       int64 = 202611200003
	beforeEventHandlerModes int64 = 202611200002
)

// The shipped CI pair's slugs before the migration, and the slug its handler
// takes.
const (
	ciRuleSlug    = "system-rule-ci-check-failed"
	ciTriggerSlug = "system-trigger-ci-fix"
	ciHandlerSlug = "system-handler-ci-check-failed"
)

// TestMigrate_EventHandlerModes_ShippedPairs: each team's shipped rule and
// trigger for one event become one handler under the new slug, decided by
// which of the two are live and whether their predicates are equal. Every case
// is its own team's copy of the same pair, so the pairing is per team.
func TestMigrate_EventHandlerModes_ShippedPairs(t *testing.T) {
	database := openEventHandlerModesTestDB(t)

	type pairCase struct {
		name string
		// rule and trigger are the team's rows before the migration; a nil
		// one is a row the team does not have.
		rule, trigger *oldHandler
		// wantRule and wantTrigger are the rows after it; a nil one is a row
		// the migration deletes.
		wantRule, wantTrigger *newHandler
	}
	cases := []pairCase{
		{
			name:    "both_live_equal_null_predicates_trigger_enabled",
			rule:    &oldHandler{enabled: true, name: "CI failed", priority: 0.75, sortOrder: 2},
			trigger: &oldHandler{enabled: true, breaker: 3, autonomy: 0.25},
			wantTrigger: &newHandler{
				mode: domain.EventHandlerModeAutomate, slug: ciHandlerSlug,
				name: "CI failed", priority: 0.75, sortOrder: 2, breaker: 3, autonomy: 0.25,
			},
		},
		{
			name:    "both_live_predicates_differ_only_in_whitespace_only_rule_enabled",
			rule:    &oldHandler{enabled: true, predicate: `{"author_id_in":[]}`, name: "CI failed", priority: 0.75, sortOrder: 2, userModified: true},
			trigger: &oldHandler{predicate: `{ "author_id_in" : [ ] }`, breaker: 3, autonomy: 0.25},
			wantTrigger: &newHandler{
				mode: domain.EventHandlerModeWatch, slug: ciHandlerSlug, predicate: `{ "author_id_in" : [ ] }`,
				name: "CI failed", priority: 0.75, sortOrder: 2, breaker: 3, autonomy: 0.25, userModified: true,
			},
		},
		{
			name:    "both_live_equal_predicates_neither_enabled",
			rule:    &oldHandler{predicate: `{"author_id_in":[]}`, name: "CI failed", priority: 0.75, sortOrder: 2},
			trigger: &oldHandler{predicate: `{"author_id_in":[]}`, breaker: 3, autonomy: 0.25, userModified: true},
			wantTrigger: &newHandler{
				mode: domain.EventHandlerModeOff, slug: ciHandlerSlug, predicate: `{"author_id_in":[]}`,
				name: "CI failed", priority: 0.75, sortOrder: 2, breaker: 3, autonomy: 0.25, userModified: true,
			},
		},
		{
			name:    "both_live_different_predicates",
			rule:    &oldHandler{enabled: true, predicate: `{"author_id_in":[]}`, name: "CI failed", priority: 0.75, sortOrder: 2},
			trigger: &oldHandler{enabled: true, predicate: `{"author_id_in":["octocat"]}`, breaker: 3, autonomy: 0.25},
			wantRule: &newHandler{
				mode: domain.EventHandlerModeWatch, slug: ciHandlerSlug, predicate: `{"author_id_in":[]}`,
				name: "CI failed", priority: 0.75, sortOrder: 2, breaker: 4, userModified: true,
			},
			wantTrigger: &newHandler{
				mode: domain.EventHandlerModeAutomate, predicate: `{"author_id_in":["octocat"]}`,
				name: blueprintName("both_live_different_predicates"), priority: 0.5, sortOrder: 3,
				breaker: 3, autonomy: 0.25, userModified: true,
			},
		},
		{
			name:    "both_live_one_null_predicate",
			rule:    &oldHandler{enabled: true, name: "CI failed", priority: 0.75, sortOrder: 2},
			trigger: &oldHandler{predicate: `{"author_id_in":["octocat"]}`, breaker: 3, autonomy: 0.25},
			wantRule: &newHandler{
				mode: domain.EventHandlerModeWatch, slug: ciHandlerSlug,
				name: "CI failed", priority: 0.75, sortOrder: 2, breaker: 4, userModified: true,
			},
			wantTrigger: &newHandler{
				mode: domain.EventHandlerModeOff, predicate: `{"author_id_in":["octocat"]}`,
				name: blueprintName("both_live_one_null_predicate"), priority: 0.5, sortOrder: 3,
				breaker: 3, autonomy: 0.25, userModified: true,
			},
		},
		{
			name:    "only_the_rule_live",
			rule:    &oldHandler{enabled: true, name: "CI failed", priority: 0.75, sortOrder: 2},
			trigger: &oldHandler{breaker: 3, autonomy: 0.25, deleted: true},
			wantRule: &newHandler{
				mode: domain.EventHandlerModeWatch, slug: ciHandlerSlug,
				name: "CI failed", priority: 0.75, sortOrder: 2, breaker: 4,
			},
			wantTrigger: &newHandler{
				mode: domain.EventHandlerModeOff, name: blueprintName("only_the_rule_live"), priority: 0.5, sortOrder: 3,
				breaker: 3, autonomy: 0.25, deleted: true,
			},
		},
		{
			name:    "only_the_trigger_live",
			rule:    &oldHandler{enabled: true, name: "CI failed", priority: 0.75, sortOrder: 2, deleted: true},
			trigger: &oldHandler{enabled: true, breaker: 3, autonomy: 0.25},
			wantTrigger: &newHandler{
				mode: domain.EventHandlerModeAutomate, slug: ciHandlerSlug,
				name: "CI failed", priority: 0.75, sortOrder: 2, breaker: 3, autonomy: 0.25,
			},
		},
		{
			name:    "neither_live",
			rule:    &oldHandler{enabled: true, name: "CI failed", priority: 0.75, sortOrder: 2, deleted: true},
			trigger: &oldHandler{breaker: 3, autonomy: 0.25, deleted: true},
			wantTrigger: &newHandler{
				mode: domain.EventHandlerModeOff, slug: ciHandlerSlug, name: blueprintName("neither_live"), priority: 0.5,
				breaker: 3, autonomy: 0.25, deleted: true,
			},
		},
		{
			name: "neither_live_and_no_trigger",
			rule: &oldHandler{enabled: true, name: "CI failed", priority: 0.75, sortOrder: 2, deleted: true},
			wantRule: &newHandler{
				mode: domain.EventHandlerModeWatch, slug: ciHandlerSlug,
				name: "CI failed", priority: 0.75, sortOrder: 2, breaker: 4, deleted: true,
			},
		},
	}

	for _, tc := range cases {
		team := "team-" + tc.name
		seedEventHandlerModesTeam(t, database, team)
		seedOldBlueprint(t, database, team, "bp-"+tc.name, blueprintName(tc.name))
		if tc.rule != nil {
			r := *tc.rule
			r.id, r.team, r.kind, r.slug, r.eventType = "rule-"+tc.name, team, "rule", ciRuleSlug, domain.EventGitHubPRCICheckFailed
			insertOldHandler(t, database, r)
		}
		if tc.trigger != nil {
			tr := *tc.trigger
			tr.id, tr.team, tr.kind, tr.slug, tr.eventType = "trigger-"+tc.name, team, "trigger", ciTriggerSlug, domain.EventGitHubPRCICheckFailed
			tr.blueprint = "bp-" + tc.name
			insertOldHandler(t, database, tr)
		}
	}

	// A run and a queued firing of the trigger that survives both reference
	// it, which is why the trigger is the row that survives.
	survivor := "trigger-both_live_equal_null_predicates_trigger_enabled"
	execAll(t, database,
		`INSERT INTO repositories (id, org_id, owner, repo, source, host) VALUES ('r1', '`+runmode.LocalDefaultOrgID+`', 'octo', 'api', 'github', 'https://github.com')`,
		`INSERT INTO entities (id, org_id, source, scope, source_id, kind, repository_id)
			VALUES ('e1', '`+runmode.LocalDefaultOrgID+`', 'github', 'https://github.com', 'octo/api#1', 'pr', 'r1')`,
		`INSERT INTO events (id, entity_id, event_type, dedup_key) VALUES ('ev1', 'e1', 'github:pr:ci_check_failed', '')`,
		`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status) VALUES ('task1', 'e1', 'github:pr:ci_check_failed', 'ev1', 'queued')`,
		`INSERT INTO conversations (id, org_id, team_id, trigger_id, trigger_type, creator_user_id, origin, status)
			VALUES ('conv1', '`+runmode.LocalDefaultOrgID+`', 'team-both_live_equal_null_predicates_trigger_enabled', '`+survivor+`', 'event', NULL, 'interactive', 'open')`,
		`INSERT INTO pending_firings (org_id, status, max_attempts, first_enqueued_at, created_at, entity_id, task_id, trigger_id, triggering_event_id)
			VALUES ('`+runmode.LocalDefaultOrgID+`', 'ready', 3, '2026-11-01 00:00:00', '2026-11-01 00:00:00', 'e1', 'task1', '`+survivor+`', 'ev1')`,
	)

	migrateThrough(t, database, eventHandlerModes)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.rule != nil {
				assertNewHandler(t, database, "rule-"+tc.name, tc.wantRule)
			}
			if tc.trigger != nil {
				want := tc.wantTrigger
				if want != nil {
					// A former trigger keeps its blueprint.
					w := *want
					w.blueprint = "bp-" + tc.name
					want = &w
				}
				assertNewHandler(t, database, "trigger-"+tc.name, want)
			}
		})
	}

	t.Run("references_to_the_surviving_trigger", func(t *testing.T) {
		if n := queryInt(t, database, `SELECT COUNT(*) FROM conversations WHERE id = 'conv1' AND trigger_id = ?`, survivor); n != 1 {
			t.Errorf("conversation conv1 no longer references %s", survivor)
		}
		if n := queryInt(t, database, `SELECT COUNT(*) FROM pending_firings WHERE task_id = 'task1' AND trigger_id = ?`, survivor); n != 1 {
			t.Errorf("the queued firing of %s is gone or references another handler", survivor)
		}
		for _, table := range []string{"conversations", "pending_firings"} {
			if n := queryInt(t, database, `SELECT COUNT(*) FROM pragma_foreign_key_check(?)`, table); n != 0 {
				t.Errorf("%s has %d rows whose foreign keys resolve to nothing", table, n)
			}
		}
	})
}

// TestMigrate_EventHandlerModes_UnpairedShippedRows: the shipped rows with no
// partner take their new slugs.
func TestMigrate_EventHandlerModes_UnpairedShippedRows(t *testing.T) {
	database := openEventHandlerModesTestDB(t)
	team := runmode.LocalDefaultTeamID
	seedOldBlueprint(t, database, team, "bp-conflicts", "Resolve conflicts")
	rows := []struct {
		old      oldHandler
		wantSlug string
	}{
		{oldHandler{id: "review-commented", kind: "rule", slug: "system-rule-review-commented", eventType: domain.EventGitHubPRReviewCommented,
			enabled: true, name: "Reviewer commented", priority: 0.65, sortOrder: 2}, "system-handler-review-commented"},
		{oldHandler{id: "jira-became-atomic", kind: "rule", slug: "system-rule-jira-became-atomic", eventType: domain.EventJiraIssueBecameAtomic,
			enabled: true, name: "Jira became atomic", priority: 0.6, sortOrder: 5}, "system-handler-jira-became-atomic"},
		{oldHandler{id: "linear-became-atomic", kind: "rule", slug: "system-rule-linear-became-atomic", eventType: domain.EventLinearIssueBecameAtomic,
			enabled: true, name: "Linear became atomic", priority: 0.6, sortOrder: 7}, "system-handler-linear-became-atomic"},
		{oldHandler{id: "conflicts", kind: "trigger", slug: "system-trigger-conflict-resolution", eventType: domain.EventGitHubPRConflicts,
			blueprint: "bp-conflicts", breaker: 2}, "system-handler-pr-conflicts"},
	}
	for _, r := range rows {
		r.old.team = team
		insertOldHandler(t, database, r.old)
	}

	migrateThrough(t, database, eventHandlerModes)

	for _, r := range rows {
		var slug sql.NullString
		if err := database.QueryRow(`SELECT system_slug FROM event_handlers WHERE id = ?`, r.old.id).Scan(&slug); err != nil {
			t.Fatalf("read %s: %v", r.old.id, err)
		}
		if slug.String != r.wantSlug {
			t.Errorf("%s (%s) slug after the migration = %v; want %s", r.old.id, r.old.slug, slug, r.wantSlug)
		}
	}
}

// TestMigrate_EventHandlerModes_OtherRows: every row that is not a shipped
// pair takes a mode from its kind and whether it was enabled, fills the
// columns its kind left NULL, and a former trigger sorts after its team's
// rules in created_at order, a former notify rule after the triggers. The
// second team sorts on its own.
func TestMigrate_EventHandlerModes_OtherRows(t *testing.T) {
	database := openEventHandlerModesTestDB(t)
	teamA, teamB := runmode.LocalDefaultTeamID, "team-b"
	seedEventHandlerModesTeam(t, database, teamB)
	seedOldBlueprint(t, database, teamA, "bp-a", "Fix the build")
	seedOldBlueprint(t, database, teamB, "bp-b", "Review it")

	ci := domain.EventGitHubPRCICheckFailed
	rows := []oldHandler{
		{id: "rule-on", team: teamA, kind: "rule", eventType: ci, enabled: true, name: "Rule on", priority: 0.4, sortOrder: 0, createdAt: "2026-11-01 10:00:00"},
		{id: "rule-off", team: teamA, kind: "rule", eventType: ci, name: "Rule off", priority: 0.9, sortOrder: 3, createdAt: "2026-11-01 10:01:00"},
		{id: "trigger-on", team: teamA, kind: "trigger", eventType: ci, enabled: true, blueprint: "bp-a", breaker: 2, autonomy: 0.6, createdAt: "2026-11-01 10:03:00"},
		{id: "trigger-off", team: teamA, kind: "trigger", eventType: ci, blueprint: "bp-a", breaker: 5, createdAt: "2026-11-01 10:02:00"},
		{id: "notify-on", team: teamA, kind: "notify", eventType: ci, enabled: true, name: "Tell us", createdAt: "2026-11-01 09:00:00"},
		{id: "notify-off", team: teamA, kind: "notify", eventType: ci, name: "Tell us later", createdAt: "2026-11-01 10:05:00"},
		{id: "b-rule", team: teamB, kind: "rule", eventType: ci, enabled: true, name: "B rule", priority: 0.7, sortOrder: 1, createdAt: "2026-11-01 10:00:00"},
		{id: "b-trigger", team: teamB, kind: "trigger", eventType: ci, enabled: true, blueprint: "bp-b", breaker: 3, createdAt: "2026-11-01 09:30:00"},
		{id: "b-notify", team: teamB, kind: "notify", eventType: ci, enabled: true, name: "Tell B", createdAt: "2026-11-01 08:00:00"},
	}
	for _, r := range rows {
		insertOldHandler(t, database, r)
	}
	// A trigger whose blueprint row is gone: foreign keys are off for the
	// insert, as they would have to have been for the row to exist.
	execAll(t, database, `PRAGMA foreign_keys = off`)
	insertOldHandler(t, database, oldHandler{
		id: "trigger-orphan", team: teamA, kind: "trigger", eventType: ci, enabled: true,
		blueprint: "bp-gone", breaker: 4, createdAt: "2026-11-01 10:04:00",
	})
	execAll(t, database, `PRAGMA foreign_keys = on`)

	migrateThrough(t, database, eventHandlerModes)

	want := map[string]newHandler{
		"rule-on":        {mode: domain.EventHandlerModeWatch, name: "Rule on", priority: 0.4, sortOrder: 0, breaker: 4},
		"rule-off":       {mode: domain.EventHandlerModeOff, name: "Rule off", priority: 0.9, sortOrder: 3, breaker: 4},
		"trigger-off":    {mode: domain.EventHandlerModeOff, name: "Fix the build", priority: 0.5, sortOrder: 4, blueprint: "bp-a", breaker: 5},
		"trigger-on":     {mode: domain.EventHandlerModeAutomate, name: "Fix the build", priority: 0.5, sortOrder: 5, blueprint: "bp-a", breaker: 2, autonomy: 0.6},
		"trigger-orphan": {mode: domain.EventHandlerModeAutomate, name: ci, priority: 0.5, sortOrder: 6, blueprint: "bp-gone", breaker: 4},
		"notify-on":      {mode: domain.EventHandlerModeNotify, name: "Tell us", priority: 0.5, sortOrder: 7, breaker: 4},
		"notify-off":     {mode: domain.EventHandlerModeOff, name: "Tell us later", priority: 0.5, sortOrder: 8, breaker: 4},
		"b-rule":         {mode: domain.EventHandlerModeWatch, name: "B rule", priority: 0.7, sortOrder: 1, breaker: 4},
		"b-trigger":      {mode: domain.EventHandlerModeAutomate, name: "Review it", priority: 0.5, sortOrder: 2, blueprint: "bp-b", breaker: 3},
		"b-notify":       {mode: domain.EventHandlerModeNotify, name: "Tell B", priority: 0.5, sortOrder: 3, breaker: 4},
	}
	for id, w := range want {
		t.Run(id, func(t *testing.T) {
			got, ok := readNewHandler(t, database, id)
			if !ok {
				t.Fatalf("%s is gone after the migration", id)
			}
			if got != w {
				t.Errorf("%s after the migration = %+v; want %+v", id, got, w)
			}
		})
	}
}

// TestMigrate_EventHandlerModes_ConflictsHandlerMakesATask: the shipped
// conflicts trigger, which had no rule to make its task, makes one when it
// was disabled and the team had not edited it.
func TestMigrate_EventHandlerModes_ConflictsHandlerMakesATask(t *testing.T) {
	database := openEventHandlerModesTestDB(t)
	cases := []struct {
		name                  string
		enabled, userModified bool
		wantMode              string
	}{
		{"disabled", false, false, domain.EventHandlerModeWatch},
		{"disabled_and_user_modified", false, true, domain.EventHandlerModeOff},
		{"enabled", true, false, domain.EventHandlerModeAutomate},
	}
	for _, tc := range cases {
		team := "team-" + tc.name
		seedEventHandlerModesTeam(t, database, team)
		seedOldBlueprint(t, database, team, "bp-"+tc.name, "Resolve conflicts")
		insertOldHandler(t, database, oldHandler{
			id: "conflicts-" + tc.name, team: team, kind: "trigger", slug: "system-trigger-conflict-resolution",
			eventType: domain.EventGitHubPRConflicts, enabled: tc.enabled, userModified: tc.userModified,
			blueprint: "bp-" + tc.name, breaker: 2,
		})
	}

	migrateThrough(t, database, eventHandlerModes)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := readNewHandler(t, database, "conflicts-"+tc.name)
			if !ok {
				t.Fatalf("the conflicts handler is gone after the migration")
			}
			if got.mode != tc.wantMode {
				t.Errorf("mode = %q; want %q", got.mode, tc.wantMode)
			}
		})
	}
}

// TestMigrate_EventHandlerModes_Schema: the rebuilt table holds a mode in
// place of kind and enabled, every handler column is NOT NULL with its
// default but name (no default) and blueprint_id (nullable), and the table has
// the indexes its reads use.
func TestMigrate_EventHandlerModes_Schema(t *testing.T) {
	database := openEventHandlerModesTestDB(t)
	migrateThrough(t, database, eventHandlerModes)

	type column struct {
		notNull bool
		dflt    string // "" is no default
	}
	cols := map[string]column{}
	rows, err := database.Query(`SELECT name, "notnull", COALESCE(dflt_value, '') FROM pragma_table_info('event_handlers')`)
	if err != nil {
		t.Fatalf("read the columns: %v", err)
	}
	for rows.Next() {
		var name string
		var c column
		if err := rows.Scan(&name, &c.notNull, &c.dflt); err != nil {
			t.Fatalf("scan a column: %v", err)
		}
		cols[name] = c
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("columns: %v", err)
	}
	for _, gone := range []string{"kind", "enabled"} {
		if _, ok := cols[gone]; ok {
			t.Errorf("event_handlers still has %s", gone)
		}
	}
	for name, want := range map[string]column{
		"mode":                     {notNull: true},
		"name":                     {notNull: true},
		"default_priority":         {notNull: true, dflt: "0.5"},
		"sort_order":               {notNull: true, dflt: "0"},
		"breaker_threshold":        {notNull: true, dflt: "4"},
		"min_autonomy_suitability": {notNull: true, dflt: "0"},
		"blueprint_id":             {},
	} {
		got, ok := cols[name]
		if !ok {
			t.Errorf("event_handlers has no %s", name)
			continue
		}
		if got != want {
			t.Errorf("%s = %+v; want %+v", name, got, want)
		}
	}

	var indexes []string
	idx, err := database.Query(`SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'event_handlers' AND sql IS NOT NULL ORDER BY name`)
	if err != nil {
		t.Fatalf("read the indexes: %v", err)
	}
	for idx.Next() {
		var name string
		if err := idx.Scan(&name); err != nil {
			t.Fatalf("scan an index: %v", err)
		}
		indexes = append(indexes, name)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("indexes: %v", err)
	}
	wantIndexes := []string{
		"event_handlers_id_org_unique",
		"idx_event_handlers_blueprint",
		"idx_event_handlers_event_type_active",
		"idx_event_handlers_mode",
	}
	if !slices.Equal(indexes, wantIndexes) {
		t.Errorf("indexes = %v; want %v", indexes, wantIndexes)
	}
}

// oldHandler is one event_handlers row in the schema before the migration. A
// rule carries a name, a priority and a sort order, a trigger a blueprint, a
// breaker threshold and an autonomy floor, and a notify rule a name alone;
// insertOldHandler writes NULL for the columns a row's kind leaves out.
type oldHandler struct {
	id, team, kind, eventType string
	slug                      string // "" for a user row
	enabled                   bool
	predicate                 string // "" is NULL

	name      string
	priority  float64
	sortOrder int

	blueprint string
	breaker   int
	autonomy  float64

	userModified, deleted bool
	createdAt             string // "" is a fixed default
}

func insertOldHandler(t *testing.T, database *sql.DB, h oldHandler) {
	t.Helper()
	source, creator, slug := "user", any(runmode.LocalDefaultUserID), any(nil)
	if h.slug != "" {
		source, creator, slug = "system", nil, h.slug
	}
	var name, priority, sortOrder, blueprint, breaker, autonomy any
	switch h.kind {
	case "rule":
		name, priority, sortOrder = h.name, h.priority, h.sortOrder
	case "trigger":
		blueprint, breaker, autonomy = h.blueprint, h.breaker, h.autonomy
	case "notify":
		name = h.name
	default:
		t.Fatalf("handler %s: unknown kind %q", h.id, h.kind)
	}
	var deletedAt any
	if h.deleted {
		deletedAt = "2026-11-10 00:00:00"
	}
	createdAt := h.createdAt
	if createdAt == "" {
		createdAt = "2026-11-01 00:00:00"
	}
	if _, err := database.Exec(`
		INSERT INTO event_handlers
			(id, org_id, team_id, creator_user_id, kind, event_type, scope_predicate_json, enabled, source, system_slug,
			 name, default_priority, sort_order, blueprint_id, breaker_threshold, min_autonomy_suitability,
			 created_at, updated_at, user_modified, deleted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		h.id, runmode.LocalDefaultOrgID, h.team, creator, h.kind, h.eventType, nullable(h.predicate), h.enabled, source, slug,
		name, priority, sortOrder, blueprint, breaker, autonomy,
		createdAt, createdAt, h.userModified, deletedAt,
	); err != nil {
		t.Fatalf("seed %s %s: %v", h.kind, h.id, err)
	}
}

// newHandler is one event_handlers row after the migration, as the columns
// the migration writes.
type newHandler struct {
	mode         string
	slug         string // "" is NULL
	predicate    string // "" is NULL
	name         string
	priority     float64
	sortOrder    int
	blueprint    string // "" is NULL
	breaker      int
	autonomy     float64
	userModified bool
	deleted      bool
}

// readNewHandler reads one handler after the migration, and whether it is
// there.
func readNewHandler(t *testing.T, database *sql.DB, id string) (newHandler, bool) {
	t.Helper()
	var h newHandler
	err := database.QueryRow(`
		SELECT mode, COALESCE(system_slug, ''), COALESCE(scope_predicate_json, ''), name, default_priority,
		       sort_order, COALESCE(blueprint_id, ''), breaker_threshold, min_autonomy_suitability,
		       user_modified, deleted_at IS NOT NULL
		FROM event_handlers WHERE id = ?`, id,
	).Scan(&h.mode, &h.slug, &h.predicate, &h.name, &h.priority,
		&h.sortOrder, &h.blueprint, &h.breaker, &h.autonomy,
		&h.userModified, &h.deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return newHandler{}, false
	}
	if err != nil {
		t.Fatalf("read handler %s: %v", id, err)
	}
	return h, true
}

// assertNewHandler checks the handler id after the migration against want, a
// nil want being a row the migration deletes.
func assertNewHandler(t *testing.T, database *sql.DB, id string, want *newHandler) {
	t.Helper()
	got, ok := readNewHandler(t, database, id)
	switch {
	case want == nil && ok:
		t.Errorf("%s after the migration = %+v; want it deleted", id, got)
	case want != nil && !ok:
		t.Errorf("%s is gone after the migration; want %+v", id, *want)
	case want != nil && got != *want:
		t.Errorf("%s after the migration = %+v; want %+v", id, got, *want)
	}
}

// blueprintName is the name of the blueprint seeded for a pair case's team.
func blueprintName(caseName string) string {
	return "Blueprint " + caseName
}

// openEventHandlerModesTestDB opens a database at the migration before the
// one under test, with the local org, team and user and the events catalog.
func openEventHandlerModesTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database := openMigrationsTestDBAt(t, beforeEventHandlerModes)
	org, team, user := runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID
	execAll(t, database,
		`INSERT INTO orgs (id, slug, name) VALUES ('`+org+`', 'local', 'Local')`,
		`INSERT INTO teams (id, org_id, slug, name) VALUES ('`+team+`', '`+org+`', 'default', 'Default')`,
		`INSERT INTO users (id, display_name) VALUES ('`+user+`', 'Local')`,
	)
	// event_handlers.event_type references the catalog, which Migrate seeds
	// after the last migration and this database has not reached.
	if err := SeedEventTypes(database, "sqlite3"); err != nil {
		t.Fatalf("seed the events catalog: %v", err)
	}
	return database
}

func seedEventHandlerModesTeam(t *testing.T, database *sql.DB, team string) {
	t.Helper()
	if _, err := database.Exec(`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, ?, ?)`,
		team, runmode.LocalDefaultOrgID, team, team); err != nil {
		t.Fatalf("seed team %s: %v", team, err)
	}
}

// seedOldBlueprint inserts a blueprint a trigger on team can reference.
func seedOldBlueprint(t *testing.T, database *sql.DB, team, id, name string) {
	t.Helper()
	if _, err := database.Exec(`INSERT INTO blueprints (id, org_id, team_id, name, source, creator_user_id) VALUES (?, ?, ?, ?, 'user', ?)`,
		id, runmode.LocalDefaultOrgID, team, name, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("seed blueprint %s: %v", id, err)
	}
}
