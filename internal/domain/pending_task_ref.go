package domain

// PendingTaskRef is the minimal projection of a task used by snapshot
// surfaces that only need to know "this entity has an active task at
// this event_type, with this dedup_key" — no priority, no run linkage,
// no entity-side metadata. Read-path equivalent of factoryEntityJSON's
// pendingTaskRef on the wire.
//
// Populated by db.Tasks.ListActiveRefsForEntities.
type PendingTaskRef struct {
	ID        string
	EntityID  string
	EventType string
	DedupKey  string
}
