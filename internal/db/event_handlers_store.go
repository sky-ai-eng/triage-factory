package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

//go:generate go run github.com/vektra/mockery/v2 --name=EventHandlerStore --output=./mocks --case=underscore --with-expecter

// EventHandlerStore is the unified successor to TaskRuleStore + TriggerStore
// (SKY-259). One table, one store, one router query. Rows are partitioned by
// the kind discriminator:
//
//	kind="rule"    — declarative; creates an unclaimed task. Carries
//	                 default_priority, sort_order, name.
//	kind="trigger" — auto-delegation; creates a task and fires a prompt.
//	                 Carries prompt_id, breaker_threshold,
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
type EventHandlerStore interface {
	// Seed materializes every row in ShippedEventHandlers as the given
	// team's own copy if it isn't already present, leaving existing rows
	// untouched so user customizations (renames, disables, predicate
	// edits, re-enables) survive across restarts. Idempotency keys on
	// (org_id, team_id, system_slug) via ON CONFLICT DO NOTHING (SKY-380):
	// the id is a random UUID per copy and the shipped slug lives in
	// system_slug, so a second team gets its own distinct rows (same slug,
	// different team_id) and a re-seed is a no-op. Shipped trigger rows
	// ship with Enabled=false per project convention (system triggers are
	// reference examples — users opt in).
	//
	// promptIDsBySlug resolves each shipped trigger's prompt slug
	// (ShippedEventHandler.PromptID) to the team's prompt-copy UUID — the
	// caller seeds the team's prompts first (phase 1) and threads the
	// resulting slug→id map here (phase 2) so the trigger→prompt
	// same-team FK ((prompt_id, team_id) → prompts(id, team_id)) resolves.
	// A trigger whose prompt slug is absent from the map is a seeding bug
	// and returns an error rather than writing a dangling row.
	//
	// System rows are team-scoped (team_id=teamID; no visibility column).
	// The router routes matched handlers to team-scoped tasks; carrying
	// team_id on the handler row itself lets every handler answer "which
	// team does this fire for?" In local mode the caller passes
	// runmode.LocalDefaultTeamID; in multi mode each new team calls Seed at
	// creation time to inherit the shipped defaults.
	Seed(ctx context.Context, orgID, teamID string, promptIDsBySlug map[string]string) error

	// List returns handlers in the order:
	//   rules first by sort_order ASC, then name ASC,
	//   then triggers by created_at DESC.
	// kind="" returns all. kind="rule" or kind="trigger" filters.
	// teamID="" returns all (solo/local, or unfiltered). A non-empty
	// teamID — the multi-team prompts page narrowed to one team — scopes
	// to that team's handlers. Every handler is team-owned (team_id NOT
	// NULL, no org-visible tier post-SKY-380), so this is a plain team
	// filter. The SQLite impl ignores it (local mode is single-team).
	List(ctx context.Context, orgID string, kind string, teamID string) ([]domain.EventHandler, error)

	// Get returns one handler by id, or (nil, nil) if not found.
	Get(ctx context.Context, orgID string, id string) (*domain.EventHandler, error)

	// GetEnabledForEvent returns enabled handlers (both kinds) matching
	// event_type, ordered rule-before-trigger so the router can process
	// them in one pass with the same observable shape as the pre-unification
	// two-phase loop.
	GetEnabledForEvent(ctx context.Context, orgID string, eventType string) ([]domain.EventHandler, error)

	// ListForPrompt returns all trigger handlers referencing the given
	// prompt (any source), ordered by created_at DESC. The prompts handler
	// uses it to surface "triggers using this prompt." Returns only
	// kind='trigger' rows.
	ListForPrompt(ctx context.Context, orgID string, promptID string) ([]domain.EventHandler, error)

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
	// prompt_id is also immutable. updated_at is refreshed.
	Update(ctx context.Context, orgID string, h domain.EventHandler) error

	// SetEnabled flips just the enabled bit. Used for the
	// "disable instead of delete" path on system rows — they're
	// shipped on every boot via Seed.
	SetEnabled(ctx context.Context, orgID string, id string, enabled bool) error

	// Delete hard-deletes a handler. Handlers gate this on
	// source='user' — system rows go through SetEnabled(false).
	Delete(ctx context.Context, orgID string, id string) error

	// Reorder updates sort_order for each rule based on its position
	// in the given ID list. Rule-only — trigger IDs in the list are
	// silently skipped (sort_order is rule-only by CHECK constraint).
	// Wrapped in a transaction.
	Reorder(ctx context.Context, orgID string, ids []string) error

	// Promote converts a kind='rule' row to kind='trigger' atomically:
	// validates the incoming trigger-only fields, clears rule-only
	// fields, writes both in one UPDATE. ID is preserved. Errors if
	// the row isn't found, isn't a rule, or the new trigger fields
	// don't satisfy the CHECK constraints.
	Promote(ctx context.Context, orgID string, id string, t domain.EventHandler) error

	// --- Admin-pool variants (`...System`) ---
	//
	// The router consumes these from its eventbus subscriber
	// goroutine, which has no JWT-claims context. Admin-pool routing
	// in Postgres; SQLite collapses to the non-System variants.
	// org_id stays in the WHERE clause as defense in depth.

	GetSystem(ctx context.Context, orgID string, id string) (*domain.EventHandler, error)
	GetEnabledForEventSystem(ctx context.Context, orgID string, eventType string) ([]domain.EventHandler, error)
}

// ShippedEventHandler is the tabular shape of one shipped system handler.
// Kind selects which set of per-kind fields are populated.
//
// ID is the stable system_slug ("system-rule-ci-check-failed" /
// "system-trigger-ci-fix") — both dialects mint a random UUID for the row
// id and store this slug in event_handlers.system_slug, deduping re-seeds
// on (org_id, team_id, system_slug) (SKY-380). For triggers, PromptID is
// the prompt's system_slug ("system-ci-fix"), resolved to the team's
// prompt-copy UUID via the promptIDsBySlug map passed to Seed.
type ShippedEventHandler struct {
	ID        string
	Kind      string // "rule" | "trigger"
	EventType string
	Predicate string // "" → NULL (match-all)

	// Rule-only.
	Name            string
	DefaultPriority float64
	SortOrder       int

	// Trigger-only. PromptID is the referenced prompt's system_slug.
	PromptID               string
	BreakerThreshold       int
	MinAutonomySuitability float64
}

// ShippedEventHandlers is the v1 default set, combining the formerly
// separate ShippedTaskRules + ShippedPromptTriggers lists. Rules ship
// with Enabled=true (the post-Seed default in the Create path);
// triggers ship with Enabled=false per project convention.
//
// PromptID values on trigger entries reference shipped prompts seeded by
// PromptStore.SeedOrUpdate. Call order at boot is PromptStore.SeedOrUpdate
// → EventHandlerStore.Seed so the FK from event_handlers (prompt_id, org_id)
// → prompts (id, org_id) is satisfied at seed time in Postgres.
var ShippedEventHandlers = []ShippedEventHandler{
	// ----- rules ------------------------------------------------------------
	//
	// Rules that scope to "my PR" carry `author_in: []` as a placeholder.
	// The SQLite Seed implementation substitutes the local user's
	// users.github_username into the empty allowlist at insert time
	// (SKY-264). Postgres Seed leaves the empty allowlist alone — multi
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
		// suppresses the initial assigned event (SKY-173 — parents are
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
		PromptID:               "system-ci-fix",
		EventType:              domain.EventGitHubPRCICheckFailed,
		Predicate:              `{"author_in":[]}`,
		BreakerThreshold:       3,
		MinAutonomySuitability: 0.0,
	},
	{
		ID:                     "system-trigger-conflict-resolution",
		Kind:                   domain.EventHandlerKindTrigger,
		PromptID:               "system-conflict-resolution",
		EventType:              domain.EventGitHubPRConflicts,
		Predicate:              `{"author_in":[]}`,
		BreakerThreshold:       2,
		MinAutonomySuitability: 0.0,
	},
	{
		ID:                     "system-trigger-jira-implement",
		Kind:                   domain.EventHandlerKindTrigger,
		PromptID:               "system-jira-implement",
		EventType:              domain.EventJiraIssueAssigned,
		Predicate:              `{"assignee_in":[]}`,
		BreakerThreshold:       2,
		MinAutonomySuitability: 0.0,
	},
	{
		// Companion to system-trigger-jira-implement; see jira-became-atomic
		// rule note above for the decomposition path that lands here.
		ID:                     "system-trigger-jira-implement-atomic",
		Kind:                   domain.EventHandlerKindTrigger,
		PromptID:               "system-jira-implement",
		EventType:              domain.EventJiraIssueBecameAtomic,
		Predicate:              `{"assignee_in":[]}`,
		BreakerThreshold:       2,
		MinAutonomySuitability: 0.0,
	},
	{
		// review_requested only fires when the session user is added
		// to the PR's review-request list, so the event is already
		// user-scoped at emit time — no predicate needed.
		ID:                     "system-trigger-pr-review",
		Kind:                   domain.EventHandlerKindTrigger,
		PromptID:               "system-pr-review",
		EventType:              domain.EventGitHubPRReviewRequested,
		Predicate:              "",
		BreakerThreshold:       3,
		MinAutonomySuitability: 0.0,
	},
	{
		ID:                     "system-trigger-fix-review-feedback",
		Kind:                   domain.EventHandlerKindTrigger,
		PromptID:               "system-fix-review-feedback",
		EventType:              domain.EventGitHubPRReviewChangesRequested,
		Predicate:              `{"author_in":[]}`,
		BreakerThreshold:       3,
		MinAutonomySuitability: 0.0,
	},
}
