// Package conversationevent owns the "conversations_updated" websocket event —
// the invalidation ping for "something about the conversations resource
// changed in a way its read projections would show".
//
// It is the cheap tier, and deliberately so: payload-free and org-scoped, so
// it is safe under the hub's org-only connection filter BY CONSTRUCTION rather
// than by a hand-mirrored read policy. It names nothing about what changed —
// the client refetches through POST /api/agent/conversations/list, which is
// what carries the scoping.
//
// It is not a second spelling of `conversation_update`, the payload-carrying
// event a run surface merges to follow ONE conversation's status. The two are
// the same pair `task_updated` and `tasks_updated` already are: one says what a
// watcher of a row should merge, the other says a set went stale. The shell's
// live rail counts a set, so it is the one that keeps the counter honest when
// a change moves no status at all.
//
// The emitters are the writes that move a conversation between the rail's sets
// without touching conversations.status: a human resolving an artifact
// (approve / dismiss, PR or review), and the reconciler flipping one terminal
// because the object moved on GitHub. Both change whether a conversation is
// waiting on a person, and neither writes the conversation row.
package conversationevent

import (
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// EventType is the websocket event type for a conversations-resource change.
const EventType = "conversations_updated"

// Broadcaster is the minimal hub surface Publish needs; *websocket.Hub
// satisfies it (and is nil-receiver-safe, so a typed-nil hub no-ops).
type Broadcaster interface {
	Broadcast(websocket.Event)
}

// Publish fires the ping for orgID. A nil hub is a no-op — every emitter is a
// best-effort announcement beside a write that has already committed, so a
// deployment without a hub (or a test without one) must not be a special case
// at the call site.
//
// Data is an empty map rather than nil so the frame carries `"data": {}` and a
// client reading `event.data` never meets a null.
func Publish(hub Broadcaster, orgID string) {
	if hub == nil {
		return
	}
	hub.Broadcast(websocket.Event{
		Type:  EventType,
		OrgID: orgID,
		Data:  map[string]any{},
	})
}
