package db

import (
	"context"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

//go:generate go run github.com/vektra/mockery/v2 --name=EventHandlerStore --output=./mocks --case=underscore --with-expecter

// ErrNoSuchEventHandler means an id-keyed write named no live handler: no row
// carries the id, or the row is soft-deleted. The id not parsing as a UUID is
// the same answer for the same reason — nothing can answer to it.
//
// Only the writes return it: Get keeps answering a miss with (nil, nil).
var ErrNoSuchEventHandler = errors.New("no live event handler with that id")

// ErrEventHandlerBlueprintChanged means an Update was refused because the
// handler's blueprint moved while the write was made: the row no longer holds
// the blueprint it held when Update read it, or the write would attach a
// blueprint that is deleted. A blueprint delete detaches its handlers in its
// own transaction, so a write composed from a read taken before it would
// otherwise attach the deleted blueprint again.
var ErrEventHandlerBlueprintChanged = errors.New("the event handler's blueprint changed during the write")

// EventHandlerStore reads and writes event_handlers: one row per handler,
// each a team's answer to one event type, carrying a mode (off, notify, watch
// or automate) and the same columns in every mode.
//
// Postgres / RLS: two pools (mirroring PromptStore).
//
//   - app pool — tf_app, RLS-active. Every CRUD method runs here.
//   - admin pool — supabase_admin, BYPASSRLS. Seed runs here so boot-time
//     system-row inserts (no JWT) satisfy the WITH CHECK that would
//     otherwise gate on creator_user_id = tf.current_user_id().
//
// SQLite: one connection; orgID is the local sentinel for local mode.
//
// # Every single-row write returns the row it persisted
//
// Create and Update hand back the stored row, read off RETURNING on the write
// statement itself rather than from a follow-up SELECT, projecting the point
// read's column list and scanner so the write shape cannot drift from the
// read shape. Update decides user_modified by comparing against stored
// content, so whether the flag flipped is not something the caller's struct
// knows; both restamp updated_at. A miss is ErrNoSuchEventHandler.
//
// Exempt, each said so at the method: Seed and Sync (shipped-content
// reconciliation over a whole team's set), Reorder and DetachBlueprint (set
// writes), and Delete (a delete).
// EventHandlerListFilter is the event-handler list's filter set.
type EventHandlerListFilter struct {
	// Modes narrows to handlers in one of these modes; empty is every mode.
	Modes []string
	// EventType narrows to handlers of one event type; "" is every type.
	EventType string
	// HasBlueprint narrows to handlers that reference a blueprint (true) or
	// reference none (false); nil is both.
	HasBlueprint *bool
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
	// untouched so a team's changes (renames, modes, predicate edits)
	// survive across restarts. Idempotency keys on
	// (org_id, team_id, system_slug) via ON CONFLICT DO NOTHING:
	// the id is a random UUID per copy and the shipped slug lives in
	// system_slug, so a second team gets its own distinct rows (same slug,
	// different team_id) and a re-seed is a no-op. Each row takes its shipped
	// mode and sort order.
	//
	// blueprintIDsBySlug resolves each shipped handler's blueprint slug
	// (ShippedEventHandler.BlueprintID) to the team's blueprint-copy UUID —
	// the caller seeds the team's prompts + blueprints first and threads the
	// resulting slug→id map here so the handler→blueprint same-team FK
	// ((blueprint_id, team_id) → blueprints(id, team_id)) resolves. A
	// handler whose blueprint slug is absent from the map is inserted with
	// no blueprint.
	//
	// System rows are team-scoped (team_id=teamID). In local mode the caller
	// passes runmode.LocalDefaultTeamID; in multi mode each new team calls
	// Seed at creation time to inherit the shipped defaults.
	//
	// Exempt from the returned-row rule: it reconciles a set, so there is no
	// single row a return value could name — and its INSERT is ON CONFLICT DO
	// NOTHING, which returns nothing at all for the rows that were already
	// there. What it wrote is read back through List.
	Seed(ctx context.Context, orgID, teamID string, blueprintIDsBySlug map[string]string) error

	// List returns one page of handlers plus the unpaged total, ordered by
	// sort_order ASC, name ASC, id ASC, so the order is total and the pages
	// partition it.
	List(ctx context.Context, orgID string, f EventHandlerListFilter, opts ListOpts) ([]domain.EventHandler, int, error)

	// Get returns one handler by id, or (nil, nil) if not found or soft-deleted.
	Get(ctx context.Context, orgID string, id string) (*domain.EventHandler, error)

	// GetActiveForEvent returns the live handlers matching event_type whose
	// mode is not off, ordered by sort_order, then id. A handler whose team is
	// archived is left out, because an archived team mints no task, fires
	// nothing and tells no one. Get and GetSystem by id still return it.
	GetActiveForEvent(ctx context.Context, orgID string, eventType string) ([]domain.EventHandler, error)

	// ListForBlueprint returns every live handler that references the given
	// blueprint, in any mode and from any source, ordered by created_at DESC.
	ListForBlueprint(ctx context.Context, orgID string, blueprintID string) ([]domain.EventHandler, error)

	// Create inserts a new user-source handler owned by teamID — the
	// acting team the handler resolved for the request. Caller supplies
	// ID, mode, event_type, and every other field ValidateEventHandler
	// checks. Source is forced to "user"; timestamps are stamped
	// server-side. The Postgres impl binds teamID directly (it satisfies
	// the team-membership RLS); the SQLite impl ignores it (local mode is
	// single-team and pins the sentinel).
	//
	// Returns the persisted row, which carries the server-stamped timestamps,
	// the forced source and the team_id and creator each dialect resolved its
	// own way — none of which h describes.
	Create(ctx context.Context, orgID, teamID string, h domain.EventHandler) (domain.EventHandler, error)

	// Update writes every mutable field of h: mode, name,
	// scope_predicate_json, default_priority, sort_order, blueprint_id,
	// breaker_threshold, min_autonomy_suitability and applies_to_unowned. ID,
	// event_type, source, team_id and created_at are immutable. updated_at is
	// refreshed.
	//
	// Stamps user_modified=true when a content field actually changed —
	// scope_predicate_json, name, default_priority, blueprint_id,
	// breaker_threshold or min_autonomy_suitability. Never stamps for mode or
	// sort_order alone: what a handler does and where it sorts are the team's
	// to own regardless of content drift, which is the same reason Reorder
	// never stamps either. Determined by comparing against the row's current
	// content, so a caller that resends unchanged content alongside a new mode
	// does not spuriously stamp.
	//
	// A mode-only update is how the product turns a handler on, off, or into
	// another mode, so that exclusion is load-bearing rather than an edge
	// case: changing a shipped handler's mode must not mark it user-modified
	// and take it out of content sync.
	//
	// Refuses with ErrEventHandlerBlueprintChanged when h names a blueprint
	// the row does not hold and that blueprint is deleted, or when the row's
	// blueprint changes between Update's read of it and its write. A blueprint
	// the row already holds is kept even when it is deleted, so a handler left
	// pointing at one can still be edited and moved off it.
	//
	// Returns the updated row, or ErrNoSuchEventHandler. The stamping decision
	// above is exactly why the row and not h is the answer: whether
	// user_modified flipped is a fact about stored content the caller never
	// held.
	Update(ctx context.Context, orgID string, h domain.EventHandler) (domain.EventHandler, error)

	// Delete soft-deletes a handler by stamping deleted_at. A shipped copy's
	// (org_id, team_id, system_slug) identity stays occupied so the
	// shipped-content sync never re-inserts it, and the runs and firings a
	// handler started keep referencing a row (conversations.trigger_id and
	// blueprint_runs.trigger_id have no ON DELETE action). Every read path
	// filters deleted_at IS NULL, so a deleted handler is gone to every
	// caller except the sync and the rows that reference it, and it never
	// fires again (the firing worker skips a queued firing whose handler it
	// cannot read).
	//
	// Exempt from the returned-row rule: it is a delete. It leaves a row, but
	// one every read path filters out — precisely the row a caller must not
	// be handed as a result.
	Delete(ctx context.Context, orgID string, id string) error

	// Reorder sets sort_order for each handler, of any mode, to its position
	// in the given ID list. Wrapped in a transaction. Never stamps
	// user_modified — sort_order is presentation order the team owns, not
	// shipped content.
	//
	// Exempt from the returned-row rule: it writes a set. The order it
	// establishes is a property of the whole list, and the caller already
	// holds that list — it is the argument.
	Reorder(ctx context.Context, orgID string, ids []string) error

	// DetachBlueprint clears blueprint_id on every live handler that
	// references blueprintID and moves each one in automate to watch, in one
	// statement, so a handler outlives the blueprint it ran and one that
	// automated keeps making tasks. It never stamps user_modified: the
	// blueprint went away, and the team did not edit the handler.
	//
	// Exempt from the returned-row rule: it writes a set, every handler of
	// one blueprint, and the caller reads ListForBlueprint first when it needs
	// to know which.
	DetachBlueprint(ctx context.Context, orgID string, blueprintID string) error

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
	//   - Insert if the team has no row for the shipped system_slug, in its
	//     shipped mode and sort order — matching Seed.
	//   - No-op if already equal to shipped content (no updated_at churn).
	//   - Otherwise update only the content fields (scope_predicate_json,
	//     name, default_priority, blueprint_id, breaker_threshold,
	//     min_autonomy_suitability) — never mode, never sort_order, never
	//     user_modified.
	//
	// A shipped blueprint slug that does not resolve in blueprintIDsBySlug
	// (the team's copy of that blueprint doesn't exist or is itself
	// soft-deleted) leaves blueprint_id as it is: NULL on an insert, and
	// unwritten on an update.
	//
	// blueprintIDsBySlug maps each shipped blueprint's system_slug to the
	// team's current (non-deleted) blueprint-copy id — the same shape Seed
	// takes, built fresh by the caller from post-blueprint-sync state.
	//
	// Runs on the admin pool; must run OUTSIDE any WithTx (mirrors Seed).
	//
	// Exempt from the returned-row rule: it reconciles a whole team's shipped
	// set, most of it to no-ops, so there is no single row a return value
	// could name.
	Sync(ctx context.Context, orgID, teamID string, shippedHandlers []ShippedEventHandler, blueprintIDsBySlug map[string]string) error

	// --- Admin-pool variants (`...System`) ---
	//
	// The router consumes these from its eventbus subscriber
	// goroutine, which has no JWT-claims context. Admin-pool routing
	// in Postgres; SQLite collapses to the non-System variants.
	// org_id stays in the WHERE clause as defense in depth. Both filter
	// deleted_at IS NULL like their app-pool counterparts — a soft-deleted
	// handler must resolve to "not found" for the router (no longer fires)
	// and for pending-firing resolution (treated as no longer automating),
	// not silently keep acting as if it were live.
	GetSystem(ctx context.Context, orgID string, id string) (*domain.EventHandler, error)
	GetActiveForEventSystem(ctx context.Context, orgID string, eventType string) ([]domain.EventHandler, error)
}

// ShippedEventHandler is the tabular shape of one shipped system handler.
//
// ID is the stable system_slug ("system-handler-ci-check-failed") — both
// dialects mint a random UUID for the row id and store this slug in
// event_handlers.system_slug, deduping re-seeds on (org_id, team_id,
// system_slug). BlueprintID is the blueprint's system_slug ("system-ci-fix"),
// or empty, resolved to the team's blueprint-copy UUID via the
// blueprintIDsBySlug map passed to Seed and Sync. Mode and SortOrder are the
// values a new copy starts with; the sync never writes either afterwards.
type ShippedEventHandler struct {
	ID        string
	EventType string
	Predicate string // "" → NULL (match-all)

	Name                   string
	DefaultPriority        float64
	SortOrder              int
	Mode                   string
	BlueprintID            string
	BreakerThreshold       int
	MinAutonomySuitability float64
}

// ShippedEventHandlers is the default set of event handlers every team gets:
// one per event, each in watch, so a team gets tasks and nothing automates
// until it chooses. A handler with a blueprint can be moved to automate.
//
// BlueprintID values reference shipped blueprints seeded by
// ShippedDefaultsStore.SeedShippedIntoTeam. Its phase order is prompts →
// blueprints(+steps) → EventHandlerStore.Seed so the FK from event_handlers
// (blueprint_id, org_id) → blueprints (id, org_id) is satisfied at seed time
// in Postgres.
//
// Handlers about "my PR" carry `author_id_in: []` and handlers about "my
// issue" carry `assignee_in: []`. An empty list matches everyone, and both
// dialects' Seed insert the predicate as written. Owner routing scopes these
// at event time instead: it resolves the PR author's (or the issue
// assignee's) team through the owning-team ladder. Users can fill either list
// to narrow a handler to particular people.
var ShippedEventHandlers = []ShippedEventHandler{
	{
		ID:               "system-handler-ci-check-failed",
		EventType:        domain.EventGitHubPRCICheckFailed,
		Predicate:        `{"author_id_in":[]}`,
		Name:             "CI check failed on my PR",
		DefaultPriority:  0.75,
		SortOrder:        0,
		Mode:             domain.EventHandlerModeWatch,
		BlueprintID:      "system-ci-fix",
		BreakerThreshold: 3,
	},
	{
		ID:               "system-handler-review-changes-requested",
		EventType:        domain.EventGitHubPRReviewChangesRequested,
		Predicate:        `{"author_id_in":[]}`,
		Name:             "Changes requested on my PR",
		DefaultPriority:  0.85,
		SortOrder:        1,
		Mode:             domain.EventHandlerModeWatch,
		BlueprintID:      "system-fix-review-feedback",
		BreakerThreshold: 3,
	},
	{
		ID:               "system-handler-review-commented",
		EventType:        domain.EventGitHubPRReviewCommented,
		Predicate:        `{"author_id_in":[]}`,
		Name:             "Reviewer commented on my PR",
		DefaultPriority:  0.65,
		SortOrder:        2,
		Mode:             domain.EventHandlerModeWatch,
		BreakerThreshold: 4,
	},
	{
		// review_requested only fires when the session user is added to the
		// PR's review-request list, so the event is already user-scoped at
		// emit time — no predicate needed.
		ID:               "system-handler-review-requested",
		EventType:        domain.EventGitHubPRReviewRequested,
		Predicate:        "",
		Name:             "Someone requested my review",
		DefaultPriority:  0.80,
		SortOrder:        3,
		Mode:             domain.EventHandlerModeWatch,
		BlueprintID:      "system-pr-review",
		BreakerThreshold: 3,
	},
	{
		ID:               "system-handler-jira-assigned",
		EventType:        domain.EventJiraIssueAssigned,
		Predicate:        `{"assignee_in":[]}`,
		Name:             "Jira issue assigned to me",
		DefaultPriority:  0.60,
		SortOrder:        4,
		Mode:             domain.EventHandlerModeWatch,
		BlueprintID:      "system-jira-implement",
		BreakerThreshold: 2,
	},
	{
		// Companion to jira-assigned. A ticket that had subtasks suppresses
		// the initial assigned event (parents are containers, not work), then
		// emits became_atomic when the last subtask closes.
		ID:               "system-handler-jira-became-atomic",
		EventType:        domain.EventJiraIssueBecameAtomic,
		Predicate:        `{"assignee_in":[]}`,
		Name:             "Jira issue decomposition resolved (now actionable)",
		DefaultPriority:  0.60,
		SortOrder:        5,
		Mode:             domain.EventHandlerModeWatch,
		BreakerThreshold: 4,
	},
	{
		ID:               "system-handler-linear-assigned",
		EventType:        domain.EventLinearIssueAssigned,
		Predicate:        `{"assignee_in":[]}`,
		Name:             "Linear issue assigned to me",
		DefaultPriority:  0.60,
		SortOrder:        6,
		Mode:             domain.EventHandlerModeWatch,
		BlueprintID:      "system-linear-implement",
		BreakerThreshold: 2,
	},
	{
		// Companion to linear-assigned, for the same reason as Jira's: an
		// issue with open sub-issues emits no assigned event, then emits
		// became_atomic when the last one closes.
		ID:               "system-handler-linear-became-atomic",
		EventType:        domain.EventLinearIssueBecameAtomic,
		Predicate:        `{"assignee_in":[]}`,
		Name:             "Linear issue decomposition resolved (now actionable)",
		DefaultPriority:  0.60,
		SortOrder:        7,
		Mode:             domain.EventHandlerModeWatch,
		BreakerThreshold: 4,
	},
	{
		// Match-all: a package none of whose alerts has a patched version
		// still delegates when automated, and the prompt tells the run to stop
		// with no pull request, so the task stays open for a person. A later
		// alert on a package with a run working on it folds into that run.
		ID:               "system-handler-vulnerability-alert-opened",
		EventType:        domain.EventGitHubVulnerabilityAlertOpened,
		Predicate:        "",
		Name:             "Vulnerable package in a tracked repository",
		DefaultPriority:  0.60,
		SortOrder:        8,
		Mode:             domain.EventHandlerModeWatch,
		BlueprintID:      "system-vulnerability-remediate",
		BreakerThreshold: 2,
	},
	{
		ID:               "system-handler-pr-conflicts",
		EventType:        domain.EventGitHubPRConflicts,
		Predicate:        `{"author_id_in":[]}`,
		Name:             "Merge conflicts on my PR",
		DefaultPriority:  0.70,
		SortOrder:        9,
		Mode:             domain.EventHandlerModeWatch,
		BlueprintID:      "system-conflict-resolution",
		BreakerThreshold: 2,
	},
}
