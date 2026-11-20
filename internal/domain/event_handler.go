package domain

import (
	"encoding/json"
	"slices"
	"strings"
	"time"
)

// EventHandler is one team's answer to one event type: a predicate the event
// must match, and a mode saying what a matching event does.
//
//	mode="off"      — nothing.
//	mode="notify"   — tells the team's members through a notification and
//	                  makes no task.
//	mode="watch"    — makes a task.
//	mode="automate" — makes a task and runs the handler's blueprint on it.
//
// Every field is held in every mode, and a mode reads the ones it needs: a
// handler keeps its blueprint when it leaves automate, and its priority when
// it notifies. The one tie between a field and a mode is that automate needs a
// blueprint, which a CHECK on event_handlers enforces.
type EventHandler struct {
	ID                 string  `json:"id"`
	Mode               string  `json:"mode"`
	EventType          string  `json:"event_type"`
	ScopePredicateJSON *string `json:"scope_predicate_json"`
	Source             string  `json:"source"` // "system" | "user"
	// TeamID is the owning team — NOT NULL, the sole scoping signal. The
	// router routes a task a handler makes to this team's queue, and a
	// notification a handler raises to this team's members.
	TeamID string `json:"team_id"`
	// AppliesToUnowned is the explicit per-handler routing-scope flag. When
	// true, the handler's team is added to a matched task's visibility even
	// for entities the team doesn't own (external or ambiguous authors). When
	// false (the default), visibility rides ownership only: the team sees a
	// task off this handler only when the owning-team ladder resolves the team
	// as owner. It is a routing column read in ownerLadderRouting (via
	// explicitWatchTeams), NOT a matchPredicate predicate — "is the author a
	// member of this team" needs an author→team DB resolution the
	// metadata-only predicate can't do. A watcher never beats a MEMBER team to
	// ownership; but on a truly unowned entity (an external author, no member
	// owner at all) an opted-in watcher whose handler automates may fire and
	// consolidate ownership onto itself. A handler that makes a task without
	// automating surfaces the card NULL-owned and nothing fires.
	AppliesToUnowned bool `json:"applies_to_unowned"`
	// SystemSlug is the stable identifier for shipped handlers
	// ("system-handler-ci-check-failed"). Empty for user-authored team
	// handlers — the team event_handlers scanners don't surface it (it's a
	// seed/idempotency key, not request data); the per-team copy keys on
	// (org_id, team_id, system_slug). omitempty so it stays absent from
	// team-handler responses that never set it.
	SystemSlug string `json:"system_slug,omitempty"`
	// CreatorUserID is the user who authored a user-source handler, empty for
	// a shipped one. It is attribution only.
	CreatorUserID string    `json:"creator_user_id,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	// UserModified is the shipped-content sync's "never clobber a user edit"
	// signal (mirrors domain.Blueprint.UserModified): true once a team's copy
	// of a shipped handler has diverged from current shipped content via a
	// content-mutating write. A write of mode or sort_order alone never sets
	// it — what a handler does and where it sorts are the team's to own
	// regardless. Always false for user-authored (non-system_slug) rows.
	UserModified bool `json:"user_modified"`

	Name string `json:"name"`
	// DefaultPriority is the priority a task the handler makes starts at,
	// 0.0–1.0.
	DefaultPriority float64 `json:"default_priority"`
	SortOrder       int     `json:"sort_order"`
	// BlueprintID is the blueprint the handler runs in automate, empty when
	// it has none (JSON null).
	BlueprintID            string  `json:"-"`
	BreakerThreshold       int     `json:"breaker_threshold"`
	MinAutonomySuitability float64 `json:"min_autonomy_suitability"`
}

// eventHandlerJSON is EventHandler's wire shape: blueprint_id is null when the
// handler has none, which BlueprintID's empty string stands for in Go.
type eventHandlerJSON struct {
	eventHandlerAlias
	BlueprintID *string `json:"blueprint_id"`
}

type eventHandlerAlias EventHandler

// MarshalJSON writes an empty BlueprintID as null.
func (h EventHandler) MarshalJSON() ([]byte, error) {
	out := eventHandlerJSON{eventHandlerAlias: eventHandlerAlias(h)}
	if h.BlueprintID != "" {
		id := h.BlueprintID
		out.BlueprintID = &id
	}
	return json.Marshal(out)
}

// UnmarshalJSON reads a null or absent blueprint_id as an empty BlueprintID.
func (h *EventHandler) UnmarshalJSON(data []byte) error {
	var in eventHandlerJSON
	if err := json.Unmarshal(data, &in); err != nil {
		return err
	}
	*h = EventHandler(in.eventHandlerAlias)
	if in.BlueprintID != nil {
		h.BlueprintID = *in.BlueprintID
	}
	return nil
}

// EventHandler modes.
const (
	EventHandlerModeOff      = "off"
	EventHandlerModeNotify   = "notify"
	EventHandlerModeWatch    = "watch"
	EventHandlerModeAutomate = "automate"
)

// EventHandlerModes is every mode, in the order the product lists them.
var EventHandlerModes = []string{EventHandlerModeOff, EventHandlerModeNotify, EventHandlerModeWatch, EventHandlerModeAutomate}

// ValidEventHandlerMode reports whether mode is one of EventHandlerModes.
func ValidEventHandlerMode(mode string) bool {
	return slices.Contains(EventHandlerModes, mode)
}

// MakesTask reports whether a matching event makes a task: watch or automate.
func (h EventHandler) MakesTask() bool {
	return h.Mode == EventHandlerModeWatch || h.Mode == EventHandlerModeAutomate
}

// Automates reports whether a matching event runs the handler's blueprint.
func (h EventHandler) Automates() bool {
	return h.Mode == EventHandlerModeAutomate
}

// Notifies reports whether a matching event raises a notification.
func (h EventHandler) Notifies() bool {
	return h.Mode == EventHandlerModeNotify
}

// NotifyRuleSource reports whether a handler may notify on eventType:
// a GitHub, Jira or Linear event, whose tracked-set gate core owns and can
// answer strictly. An extension's source gate narrows and may fail open,
// which is acceptable for a task and a disclosure for a notification.
func NotifyRuleSource(eventType string) bool {
	return strings.HasPrefix(eventType, "github:") || strings.HasPrefix(eventType, "jira:") || strings.HasPrefix(eventType, "linear:")
}

// EventHandler sources. Mirrors prompts.source — system rows are
// admin-managed; user rows are user-authored.
const (
	EventHandlerSourceSystem = "system"
	EventHandlerSourceUser   = "user"
)

// TriggerTypeEvent is the trigger_type a conversation or blueprint run carries
// when an event handler's firing started it.
const TriggerTypeEvent = "event"
