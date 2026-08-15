package domain

import "time"

// EventHandler unifies the former TaskRule + PromptTrigger domain types
// into one primitive. The kind discriminator selects per-kind behavior:
//
//	kind="rule"    — declarative; creates an unclaimed task (human triage).
//	                 default_priority, sort_order, name are required.
//	                 prompt_id, breaker_threshold, min_autonomy_suitability
//	                 must be nil.
//	kind="trigger" — auto-delegation; creates a task and
//	                 stamps claimed_by_agent_id at creation. blueprint_id,
//	                 breaker_threshold, min_autonomy_suitability are
//	                 required. default_priority, sort_order must be nil.
//	                 A trigger always fires a blueprint (length >= 1); a
//	                 single prompt is just a 1-step blueprint.
//
// The CHECK constraints on event_handlers enforce the shape pair at the
// SQL level; both backends rely on it.
type EventHandler struct {
	ID                 string  `json:"id"`
	Kind               string  `json:"kind"` // "rule" | "trigger"
	EventType          string  `json:"event_type"`
	ScopePredicateJSON *string `json:"scope_predicate_json"`
	Enabled            bool    `json:"enabled"`
	Source             string  `json:"source"` // "system" | "user"
	// TeamID is the owning team — NOT NULL, the sole scoping signal
	// (the visibility column was dropped; every handler is
	// team-owned). The router reads this to route tasks created off
	// matched rules to the correct team's queue.
	TeamID string `json:"team_id"`
	// AppliesToUnowned is the explicit per-rule routing-scope flag
	// (TFAC-517) that replaced the source-as-scope heuristic. When true,
	// the rule's team is added to a matched task's visibility even for
	// entities the team doesn't own (external/ambiguous authors) — the
	// deliberate "watch" opt-in. When false (the default), visibility
	// rides ownership only: the team sees a task off this rule only when
	// the owning-team ladder resolves the team as owner. It is a routing
	// column read in ownerLadderRouting (via explicitWatchTeams), NOT a
	// matchPredicate predicate — "is the author a member of this team"
	// needs an author→team DB resolution the metadata-only predicate
	// can't do. A watcher never beats a MEMBER team to ownership (the
	// TFAC-514 no-steal invariant); but on a truly unowned entity (an
	// external author, no member owner at all) an opted-in watcher whose
	// team also has a configured auto-delegation may fire and consolidate
	// ownership onto itself — the eyes-open behavior.
	//
	// It is a per-HANDLER flag, valid on both kinds: a rule grants
	// visibility reach; a trigger grants the same reach AND (being a
	// trigger) fires on the orphan, letting a team configure orphan
	// auto-delegation entirely from the prompts page with no companion
	// rule. The router reads it off both kinds via explicitWatchTeams. A
	// watcher whose opt-in is only a rule (no trigger) surfaces the card
	// NULL-owned and nothing fires.
	AppliesToUnowned bool `json:"applies_to_unowned"`
	// SystemSlug is the stable identifier for shipped handlers
	// ("system-rule-ci-check-failed", "system-trigger-ci-fix"). Empty for
	// user-authored team handlers — the team event_handlers scanners don't
	// surface it (it's a seed/idempotency key, not request data); the
	// per-team copy keys on (org_id, team_id, system_slug). omitempty so it
	// stays absent from team-handler responses that never set it.
	SystemSlug string    `json:"system_slug,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	// UserModified is the shipped-content sync's "never clobber a user edit"
	// signal (mirrors domain.Blueprint.UserModified): true once a team's copy
	// of a shipped handler has diverged from current shipped content via a
	// content-mutating write (Update on a non-enabled field, Promote,
	// RetargetBlueprint). SetEnabled and Reorder never set it — activation
	// state and presentation order are the user's to own regardless. Always
	// false for user-authored (non-system_slug) rows.
	UserModified bool `json:"user_modified"`

	// Rule-only (nil for triggers).
	Name            string   `json:"name"`             // required for rules
	DefaultPriority *float64 `json:"default_priority"` // 0.0–1.0
	SortOrder       *int     `json:"sort_order"`

	// Trigger-only (zero/nil for rules).
	BlueprintID            string   `json:"blueprint_id"`
	TriggerType            string   `json:"trigger_type"` // V1: only "event"
	BreakerThreshold       *int     `json:"breaker_threshold"`
	MinAutonomySuitability *float64 `json:"min_autonomy_suitability"`
}

// EventHandler kinds.
const (
	EventHandlerKindRule    = "rule"
	EventHandlerKindTrigger = "trigger"
)

// EventHandler sources. Mirrors prompts.source — system rows are
// admin-managed; user rows are user-authored.
const (
	EventHandlerSourceSystem = "system"
	EventHandlerSourceUser   = "user"
)

// TriggerTypeEvent is the V1 trigger_type value carried by triggers
// (event_handlers with kind='trigger'). Persisted only on the
// conversations row (conversations.trigger_type); the column was dropped from
// the prior prompt_triggers table during unification.
const TriggerTypeEvent = "event"
