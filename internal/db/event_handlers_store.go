package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

//go:generate go run github.com/vektra/mockery/v2 --name=EventHandlerStore --output=./mocks --case=underscore --with-expecter

// EventHandlerStore is the unified successor to TaskRuleStore + TriggerStore.
// One table, one store, one router query. Rows are partitioned by
// the kind discriminator:
//
//	kind="rule"    — declarative; creates an unclaimed task. Carries
//	                 default_priority, sort_order, name.
//	kind="trigger" — auto-delegation; creates a task and fires a blueprint.
//	                 Carries blueprint_id, breaker_threshold,
//	                 min_autonomy_suitability.
//
// Per-kind CHECK constraints on the event_handlers table enforce that each
// row populates exactly its kind's required fields and leaves the other
// kind's fields NULL. Promotion (rule → trigger) is a single atomic UPDATE
// through the Promote method.
//
// Postgres / RLS: two pools (mirroring PromptStore + the former
// TaskRuleStore / TriggerStore).
//
//   - app pool — tf_app, RLS-active. Every CRUD method runs here.
//   - admin pool — supabase_admin, BYPASSRLS. Seed runs here so boot-time
//     system-row inserts (no JWT) satisfy the WITH CHECK that would
//     otherwise gate on creator_user_id = tf.current_user_id().
//
// SQLite: one connection; orgID is the local sentinel for local mode.
// EventHandlerListFilter is the event-handler list's filter set.
type EventHandlerListFilter struct {
	// Kind is "" (both), "rule", or "trigger".
	Kind string
	// TeamID scopes to one team's handlers; "" returns everything visible
	// (solo/local, or an unfiltered view). Every handler is team-owned
	// (team_id NOT NULL, no org-visible tier), so this is a plain team
	// filter. The SQLite impl ignores it — local mode is single-team.
	TeamID string
	// GatedEventTypes are event types this org is not entitled to; handlers
	// bound to one are hidden. Rows persist regardless of entitlement state —
	// this is visibility only.
	//
	// It filters in the query rather than after the read for the same reason
	// BlueprintListFilter's does: a post-window trim shortens the page and
	// leaves total_count counting rows the caller cannot see.
	GatedEventTypes []string
}

type EventHandlerStore interface {
	// Seed materializes every row in ShippedEventHandlers as the given
	// team's own copy if it isn't already present, leaving existing rows
	// untouched so user customizations (renames, disables, predicate
	// edits, re-enables) survive across restarts. Idempotency keys on
	// (org_id, team_id, system_slug) via ON CONFLICT DO NOTHING:
	// the id is a random UUID per copy and the shipped slug lives in
	// system_slug, so a second team gets its own distinct rows (same slug,
	// different team_id) and a re-seed is a no-op. Shipped trigger rows
	// ship with Enabled=false per project convention (system triggers are
	// reference examples — users opt in).
	//
	// blueprintIDsBySlug resolves each shipped trigger's blueprint slug
	// (ShippedEventHandler.BlueprintID) to the team's blueprint-copy UUID —
	// the caller seeds the team's prompts + blueprints first and threads the
	// resulting slug→id map here so the trigger→blueprint same-team FK
	// ((blueprint_id, team_id) → blueprints(id, team_id)) resolves. A trigger
	// whose blueprint slug is absent from the map is a seeding bug and returns
	// an error rather than writing a dangling row.
	//
	// System rows are team-scoped (team_id=teamID; no visibility column).
	// The router routes matched handlers to team-scoped tasks; carrying
	// team_id on the handler row itself lets every handler answer "which
	// team does this fire for?" In local mode the caller passes
	// runmode.LocalDefaultTeamID; in multi mode each new team calls Seed at
	// creation time to inherit the shipped defaults.
	Seed(ctx context.Context, orgID, teamID string, blueprintIDsBySlug map[string]string) error

	// List returns one page of handlers plus the unpaged total, in the order:
	//   rules first by sort_order ASC, then name ASC,
	//   then triggers by created_at DESC,
	// with an id tiebreaker so the order is total and the pages partition it.
	List(ctx context.Context, orgID string, f EventHandlerListFilter, opts ListOpts) ([]domain.EventHandler, int, error)

	// Get returns one handler by id, or (nil, nil) if not found or soft-deleted.
	Get(ctx context.Context, orgID string, id string) (*domain.EventHandler, error)

	// GetBySystemSlug resolves a team's copy of a shipped handler by its
	// stable system_slug. Returns (nil, nil) when the team has no copy or its
	// copy is soft-deleted — the sync's "does this slot need an insert"
	// probe.
	GetBySystemSlug(ctx context.Context, orgID, teamID, systemSlug string) (*domain.EventHandler, error)

	// GetEnabledForEvent returns enabled, non-deleted handlers (both kinds)
	// matching event_type, ordered rule-before-trigger so the router can
	// process them in one pass with the same observable shape as the
	// pre-unification two-phase loop.
	GetEnabledForEvent(ctx context.Context, orgID string, eventType string) ([]domain.EventHandler, error)

	// ListForBlueprint returns all non-deleted trigger handlers referencing
	// the given blueprint (any source), ordered by created_at DESC. Used to
	// surface "triggers firing this blueprint." Returns only kind='trigger'
	// rows.
	ListForBlueprint(ctx context.Context, orgID string, blueprintID string) ([]domain.EventHandler, error)

	// Create inserts a new user-source handler owned by teamID — the
	// acting team the handler resolved for the request. Caller supplies
	// ID, kind, event_type, and the per-kind fields appropriate to the
	// kind. Source is forced to "user"; timestamps are stamped
	// server-side. The Postgres impl binds teamID directly (it satisfies
	// the team-membership RLS); the SQLite impl ignores it (local mode is
	// single-team and pins the sentinel).
	Create(ctx context.Context, orgID, teamID string, h domain.EventHandler) error

	// Update changes a handler's mutable fields. ID, kind, source,
	// event_type, and created_at are immutable. For triggers,
	// blueprint_id is also immutable (see RetargetBlueprint). updated_at is
	// refreshed.
	//
	// Stamps user_modified=true when a content field actually changed —
	// scope_predicate_json, name, default_priority (rules), or
	// breaker_threshold / min_autonomy_suitability (triggers). Never stamps
	// for enabled or sort_order alone: activation state and presentation
	// order are the user's to own regardless of content drift, matching the
	// dedicated SetEnabled / Reorder methods below. Determined by comparing
	// against the row's current content, so a caller that resends unchanged
	// content alongside a new enabled value does not spuriously stamp.
	Update(ctx context.Context, orgID string, h domain.EventHandler) error

	// SetEnabled flips just the enabled bit — the user-facing enable/disable
	// toggle, and (per the automations-as-toggles product direction) the
	// primary way most users interact with a shipped handler. Never stamps
	// user_modified: activation state is not content.
	SetEnabled(ctx context.Context, orgID string, id string, enabled bool) error

	// Delete removes a handler. A row with a system_slug (a shipped copy)
	// soft-deletes — stamps deleted_at, leaving the
	// (org_id, team_id, system_slug) identity occupied so the shipped-content
	// sync never re-inserts (never resurrect) and never mistakes the empty
	// slot for "add this new shipped default." A user-created row (no
	// system_slug) hard-deletes, matching the pre-sync behavior. Every read
	// path filters deleted_at IS NULL, so a soft-deleted row is
	// indistinguishable from a hard-deleted one to every caller except the
	// sync itself.
	Delete(ctx context.Context, orgID string, id string) error

	// Reorder updates sort_order for each rule based on its position
	// in the given ID list. Rule-only — trigger IDs in the list are
	// silently skipped (sort_order is rule-only by CHECK constraint).
	// Wrapped in a transaction. Never stamps user_modified — sort_order is
	// presentation order the user owns, not shipped content.
	Reorder(ctx context.Context, orgID string, ids []string) error

	// Promote converts a kind='rule' row to kind='trigger' atomically:
	// validates the incoming trigger-only fields, clears rule-only
	// fields, writes both in one UPDATE. ID is preserved. Errors if
	// the row isn't found, isn't a rule, or the new trigger fields
	// don't satisfy the CHECK constraints. Always stamps user_modified=true —
	// a kind change is never something the sync should undo.
	Promote(ctx context.Context, orgID string, id string, t domain.EventHandler) error

	// RetargetBlueprint re-points a trigger at a different blueprint in a single
	// UPDATE, preserving the handler row — and its id, which runs /
	// blueprint_runs / pending_firings FK-reference — so run history and the
	// event-trigger fence survive the move (a delete+recreate would orphan that
	// history and trip the FK). Trigger-only: the WHERE pins kind='trigger', so a
	// rule id is a no-op. The caller validates that the new blueprint exists, is
	// same-team, and is not already triggered; the one-trigger-per-blueprint
	// partial-unique index is the hard backstop. Always stamps
	// user_modified=true — a user-initiated retarget must never be silently
	// reverted by a later sync pass (which re-resolves an unmodified trigger's
	// blueprint binding from the shipped slug on every boot).
	RetargetBlueprint(ctx context.Context, orgID string, id string, newBlueprintID string) error

	// Sync brings teamID's unmodified copies of shippedHandlers up to current
	// shipped content, one handler at a time. Production callers always pass
	// db.ShippedEventHandlers — the parameter (mirroring
	// SyncShippedIntoTeam's shippedPrompts/shippedBlueprints) exists so tests
	// can sync against fixture content instead of the real compile-time list.
	// See db.ShippedDefaultsStore.SyncShippedIntoTeam for the normative
	// rules; this is its handler-sync phase, called after the blueprint sync
	// phase so blueprintIDsBySlug reflects post-sync state. Per shipped
	// handler:
	//
	//   - Skip if the team's row is soft-deleted or user_modified.
	//   - Skip a trigger whose shipped blueprint slug isn't present in
	//     blueprintIDsBySlug (the team's copy of that blueprint doesn't exist
	//     or is itself soft-deleted) — never wire a trigger to a blueprint
	//     that isn't there.
	//   - Insert if the team has no row for the shipped system_slug, honoring
	//     the shipped convention (rules enabled=true, triggers enabled=false)
	//     and initial sort_order — matching Seed.
	//   - No-op if already equal to shipped content (no updated_at churn).
	//   - Otherwise update only the content fields (scope_predicate_json,
	//     name, default_priority for rules; blueprint_id, breaker_threshold,
	//     min_autonomy_suitability for triggers) — never enabled, never
	//     sort_order, never user_modified.
	//
	// blueprintIDsBySlug maps each shipped blueprint's system_slug to the
	// team's current (non-deleted) blueprint-copy id — the same shape Seed
	// takes, built fresh by the caller from post-blueprint-sync state.
	//
	// Runs on the admin pool; must run OUTSIDE any WithTx (mirrors Seed).
	Sync(ctx context.Context, orgID, teamID string, shippedHandlers []ShippedEventHandler, blueprintIDsBySlug map[string]string) error

	// --- Admin-pool variants (`...System`) ---
	//
	// The router consumes these from its eventbus subscriber
	// goroutine, which has no JWT-claims context. Admin-pool routing
	// in Postgres; SQLite collapses to the non-System variants.
	// org_id stays in the WHERE clause as defense in depth. Both filter
	// deleted_at IS NULL like their app-pool counterparts — a soft-deleted
	// trigger must resolve to "not found" for the router (no longer fires)
	// and for pending-firing resolution (treated as trigger-disabled), not
	// silently keep acting as if it were live.
	GetSystem(ctx context.Context, orgID string, id string) (*domain.EventHandler, error)
	GetEnabledForEventSystem(ctx context.Context, orgID string, eventType string) ([]domain.EventHandler, error)
}

// ShippedEventHandler is the tabular shape of one shipped system handler.
// Kind selects which set of per-kind fields are populated.
//
// ID is the stable system_slug ("system-rule-ci-check-failed" /
// "system-trigger-ci-fix") — both dialects mint a random UUID for the row
// id and store this slug in event_handlers.system_slug, deduping re-seeds
// on (org_id, team_id, system_slug). For triggers, BlueprintID is
// the blueprint's system_slug ("system-ci-fix"), resolved to the team's
// blueprint-copy UUID via the blueprintIDsBySlug map passed to Seed.
type ShippedEventHandler struct {
	ID        string
	Kind      string // "rule" | "trigger"
	EventType string
	Predicate string // "" → NULL (match-all)

	// Rule-only.
	Name            string
	DefaultPriority float64
	SortOrder       int

	// Trigger-only. BlueprintID is the referenced blueprint's system_slug
	// (which reuses the wrapped prompt's slug).
	BlueprintID            string
	BreakerThreshold       int
	MinAutonomySuitability float64
}

// ShippedEventHandlers is the v1 default set, combining the formerly
// separate ShippedTaskRules + ShippedPromptTriggers lists. Rules ship
// with Enabled=true (the post-Seed default in the Create path);
// triggers ship with Enabled=false per project convention.
//
// BlueprintID values on trigger entries reference shipped blueprints seeded
// by ShippedDefaultsStore.SeedShippedIntoTeam. Its phase order is prompts →
// blueprints(+steps) → EventHandlerStore.Seed so the FK from event_handlers
// (blueprint_id, org_id) → blueprints (id, org_id) is satisfied at seed time
// in Postgres.
var ShippedEventHandlers = []ShippedEventHandler{
	// ----- rules ------------------------------------------------------------
	//
	// Rules that scope to "my PR" carry `author_in: []` as a placeholder.
	// The SQLite Seed implementation substitutes the local user's
	// users.github_username into the empty allowlist at insert time.
	// Postgres Seed leaves the empty allowlist alone — multi
	// mode doesn't have a single "self" to substitute; team visibility
	// is the scoping mechanism there. Either way, users can edit the
	// allowlist via the Settings UI to add or remove handles.
	{
		ID:              "system-rule-ci-check-failed",
		Kind:            domain.EventHandlerKindRule,
		EventType:       domain.EventGitHubPRCICheckFailed,
		Predicate:       `{"author_in":[]}`,
		Name:            "CI check failed on my PR",
		DefaultPriority: 0.75,
		SortOrder:       0,
	},
	{
		ID:              "system-rule-review-changes-requested",
		Kind:            domain.EventHandlerKindRule,
		EventType:       domain.EventGitHubPRReviewChangesRequested,
		Predicate:       `{"author_in":[]}`,
		Name:            "Changes requested on my PR",
		DefaultPriority: 0.85,
		SortOrder:       1,
	},
	{
		ID:              "system-rule-review-commented",
		Kind:            domain.EventHandlerKindRule,
		EventType:       domain.EventGitHubPRReviewCommented,
		Predicate:       `{"author_in":[]}`,
		Name:            "Reviewer commented on my PR",
		DefaultPriority: 0.65,
		SortOrder:       2,
	},
	{
		ID:              "system-rule-review-requested",
		Kind:            domain.EventHandlerKindRule,
		EventType:       domain.EventGitHubPRReviewRequested,
		Predicate:       "",
		Name:            "Someone requested my review",
		DefaultPriority: 0.80,
		SortOrder:       3,
	},
	{
		ID:              "system-rule-jira-assigned",
		Kind:            domain.EventHandlerKindRule,
		EventType:       domain.EventJiraIssueAssigned,
		Predicate:       `{"assignee_in":[]}`,
		Name:            "Jira issue assigned to me",
		DefaultPriority: 0.60,
		SortOrder:       4,
	},
	{
		// Companion to jira-assigned. A ticket that had subtasks
		// suppresses the initial assigned event (parents are
		// containers, not work), then emits became_atomic when the
		// last subtask closes. Same task-creation behavior as the
		// assignment rule.
		ID:              "system-rule-jira-became-atomic",
		Kind:            domain.EventHandlerKindRule,
		EventType:       domain.EventJiraIssueBecameAtomic,
		Predicate:       `{"assignee_in":[]}`,
		Name:            "Jira issue decomposition resolved (now actionable)",
		DefaultPriority: 0.60,
		SortOrder:       5,
	},

	// ----- triggers ---------------------------------------------------------
	{
		ID:                     "system-trigger-ci-fix",
		Kind:                   domain.EventHandlerKindTrigger,
		BlueprintID:            "system-ci-fix",
		EventType:              domain.EventGitHubPRCICheckFailed,
		Predicate:              `{"author_in":[]}`,
		BreakerThreshold:       3,
		MinAutonomySuitability: 0.0,
	},
	{
		ID:                     "system-trigger-conflict-resolution",
		Kind:                   domain.EventHandlerKindTrigger,
		BlueprintID:            "system-conflict-resolution",
		EventType:              domain.EventGitHubPRConflicts,
		Predicate:              `{"author_in":[]}`,
		BreakerThreshold:       2,
		MinAutonomySuitability: 0.0,
	},
	{
		ID:                     "system-trigger-jira-implement",
		Kind:                   domain.EventHandlerKindTrigger,
		BlueprintID:            "system-jira-implement",
		EventType:              domain.EventJiraIssueAssigned,
		Predicate:              `{"assignee_in":[]}`,
		BreakerThreshold:       2,
		MinAutonomySuitability: 0.0,
	},
	// No became_atomic trigger ships: a blueprint is fired by exactly one event
	// (the partial-unique event_handlers_one_trigger_per_blueprint index), and
	// system-jira-implement is already claimed by system-trigger-jira-implement
	// on jira_issue_assigned. A decomposition-resolved ticket still surfaces as
	// a task via system-rule-jira-became-atomic; auto-delegating it is a
	// user-authored trigger on its own blueprint (or a blueprint copy).
	{
		// review_requested only fires when the session user is added
		// to the PR's review-request list, so the event is already
		// user-scoped at emit time — no predicate needed.
		ID:                     "system-trigger-pr-review",
		Kind:                   domain.EventHandlerKindTrigger,
		BlueprintID:            "system-pr-review",
		EventType:              domain.EventGitHubPRReviewRequested,
		Predicate:              "",
		BreakerThreshold:       3,
		MinAutonomySuitability: 0.0,
	},
	{
		ID:                     "system-trigger-fix-review-feedback",
		Kind:                   domain.EventHandlerKindTrigger,
		BlueprintID:            "system-fix-review-feedback",
		EventType:              domain.EventGitHubPRReviewChangesRequested,
		Predicate:              `{"author_in":[]}`,
		BreakerThreshold:       3,
		MinAutonomySuitability: 0.0,
	},
}
