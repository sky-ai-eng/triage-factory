package domain

import "time"

// PromptTrigger defines an automation rule that fires a prompt in response to
// an event — what runs automatically without user intervention.
type PromptTrigger struct {
	ID                     string    `json:"id"`
	PromptID               string    `json:"prompt_id"`
	TriggerType            string    `json:"trigger_type"`             // V1: only "event" is accepted
	EventType              string    `json:"event_type"`               // required for trigger_type="event"
	ScopePredicateJSON     *string   `json:"scope_predicate_json"`     // nullable; null = match-all
	BreakerThreshold       int       `json:"breaker_threshold"`        // consecutive-failure count that trips the per-(entity, prompt) breaker
	MinAutonomySuitability float64   `json:"min_autonomy_suitability"` // 0.0 = fire immediately; >0 = defer until AI scores above threshold
	Enabled                bool      `json:"enabled"`
	CreatedAt              time.Time `json:"created_at"`
	UpdatedAt              time.Time `json:"updated_at"`
}

// Valid trigger types. Only "event" is supported in V1; others are reserved.
const (
	TriggerTypeEvent = "event"
)
