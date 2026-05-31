package domain

import "time"

// EventHandler unifies the former TaskRule + PromptTrigger domain types
// into one primitive. The kind discriminator selects per-kind behavior:
//
//	kind="rule"    — declarative; creates an unclaimed task (human triage).
//	                 default_priority, sort_order, name are required.
//	                 prompt_id, breaker_threshold, min_autonomy_suitability
//	                 must be nil.
//	kind="trigger" — auto-delegation; creates a task and (post-SKY-261)
//	                 stamps claimed_by_agent_id at creation. prompt_id,
//	                 breaker_threshold, min_autonomy_suitability are
//	                 required. default_priority, sort_order must be nil.
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
	// (SKY-380 dropped the visibility column; every handler is
	// team-owned). The router reads this to route tasks created off
	// matched rules to the correct team's queue.
	TeamID    string    `json:"team_id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Rule-only (nil for triggers).
	Name            string   `json:"name"`             // required for rules
	DefaultPriority *float64 `json:"default_priority"` // 0.0–1.0
	SortOrder       *int     `json:"sort_order"`

	// Trigger-only (zero/nil for rules).
	PromptID               string   `json:"prompt_id"`
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
// (event_handlers with kind='trigger'). Persisted only on the runs
// row (runs.trigger_type) post-SKY-259; the column was dropped from
// the pre-SKY-259 prompt_triggers table during unification.
const TriggerTypeEvent = "event"
