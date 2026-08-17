package domain

import "time"

// FactoryActiveRun is one in-flight agent run as displayed in the
// Factory overlay: the run row joined with its task + a couple of
// pre-extracted entity fields the renderer needs for keyed lookups.
//
// The embedded Conversation + Task carry the full state; EntityAuthor
// and EntityEventTyp are pre-copied so the renderer doesn't have to
// re-derive them per row.
type FactoryActiveRun struct {
	Run            Conversation
	Task           Task
	EntityAuthor   string // PR author login (github) or assignee (jira); "" if unknown
	EntityEventTyp string // task.event_type; pre-copied for keyed lookup
}

// FactoryEntityRow is one entity as the Factory overlay sees it:
// the entity itself plus the most recent event's type + occurred-at
// timestamp. The two fields drive the "which station is this card
// currently on" rendering decision.
type FactoryEntityRow struct {
	Entity          Entity
	LatestEventType string
	LatestEventAt   *time.Time
}

// FactoryRecentEvent is a single entry in an entity's recent event
// history. Ordered chronologically ascending by caller
// (db.FactoryReadStore.RecentEventsByEntity).
//
// Two timestamps because we need both for the factory animation:
//   - CreatedAt is the "event time" used for chain ordering:
//     occurred_at when the upstream system reported it, falling back
//     to detection time. So two events from one poll order by their
//     upstream times, not their insert order.
//   - DetectedAt is purely the row's insert time (events.created_at).
type FactoryRecentEvent struct {
	EventType  string
	CreatedAt  time.Time
	DetectedAt time.Time
}
