// Package conversationframe owns the websocket frames about one
// conversation: message, conversation_update, permission_request,
// permission_resolved, and deliverable_updated. Every one of them goes out
// through a Notifier, and the hub carries no other frame that names a
// conversation.
//
// Two properties are load-bearing:
//
//   - Each frame's payload is a typed struct, so only its fields can go over
//     the wire. A message frame carries the transcript row a run station
//     renders, tool inputs and outputs included, which is the reason the
//     scope below exists.
//   - Delivery scope mirrors the REST read. A conversation is readable by
//     its org, its team, or its creator, as its visibility says, and the hub
//     scopes connections on (org, user) only. So a Notifier built with an
//     AudienceFunc resolves the audience at emit time and sends one envelope
//     that carries it as a recipient set (websocket.Event.UserIDs). One
//     envelope rather than one per user, because message is the
//     highest-rate frame the hub carries and in multi mode every Broadcast
//     is a Postgres NOTIFY, or an outbox row for a large payload. The
//     audience is read fresh for every frame, so a membership change applies
//     from the next one; nothing is captured per connection.
package conversationframe

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

var frameLog = logging.Component("conversationframe")

const (
	typeMessage            = "message"
	typeConversationUpdate = "conversation_update"
	typePermissionRequest  = "permission_request"
	typePermissionResolved = "permission_resolved"
	typeDeliverableUpdated = "deliverable_updated"
)

// audienceTimeout bounds the audience read. The emitters run on a run's
// transcript and lifecycle paths and on request handlers, and a frame is a
// hint the client can recover from, so a slow read drops the frame rather
// than holding up the work it describes.
const audienceTimeout = 3 * time.Second

// StatusUpdate is the conversation_update payload: the status the
// conversation moved to, and for a failure its machine-readable kind. An
// unclassified failure omits the kind, which consumers read as a generic
// failure.
type StatusUpdate struct {
	Status      string `json:"status"`
	FailureKind string `json:"failure_kind,omitempty"`
}

// PermissionUpdate is the permission_request and permission_resolved
// payload. It names the prompt only; the client reads the prompt itself from
// the conversation's permissions route.
type PermissionUpdate struct {
	ToolCallID string `json:"tool_call_id"`
}

// DeliverableUpdate is the deliverable_updated payload: the deliverable
// that changed and the state it moved to.
type DeliverableUpdate struct {
	DeliverableID string `json:"deliverable_id"`
	State         string `json:"state"`
}

// AudienceFunc resolves who may receive a frame about conversationID in
// orgID: orgWide when every member of the org may read the conversation,
// otherwise the user ids that may. db.ConversationStore's
// ConversationFrameRecipientsSystem is the production implementation.
type AudienceFunc func(ctx context.Context, orgID, conversationID string) (orgWide bool, userIDs []string, err error)

// Broadcaster is the hub surface a Notifier needs; *websocket.Hub satisfies
// it, and is nil-receiver-safe, so a typed-nil hub no-ops.
type Broadcaster interface {
	Broadcast(websocket.Event)
}

// Notifier publishes the conversation frames with the delivery scope its
// construction chose: a nil audience is the local-mode shape (one
// org-scoped broadcast, since N=1 has no team boundary), a non-nil audience
// is the multi-mode shape (one broadcast addressed to the resolved
// recipients).
type Notifier struct {
	hub      Broadcaster
	audience AudienceFunc
}

// NewNotifier builds a Notifier over hub. Pass audience only in multi mode.
// A nil hub yields a Notifier whose methods no-op, matching the hub's own
// nil-safety so emit sites need no guards.
func NewNotifier(hub Broadcaster, audience AudienceFunc) *Notifier {
	return &Notifier{hub: hub, audience: audience}
}

// Message sends one transcript row.
func (n *Notifier) Message(ctx context.Context, orgID, conversationID string, msg domain.MessageDTO) {
	n.publish(ctx, typeMessage, orgID, conversationID, msg)
}

// ConversationUpdate sends a status change.
func (n *Notifier) ConversationUpdate(ctx context.Context, orgID, conversationID string, upd StatusUpdate) {
	n.publish(ctx, typeConversationUpdate, orgID, conversationID, upd)
}

// PermissionRequest announces a prompt waiting on a person.
func (n *Notifier) PermissionRequest(ctx context.Context, orgID, conversationID string, upd PermissionUpdate) {
	n.publish(ctx, typePermissionRequest, orgID, conversationID, upd)
}

// PermissionResolved announces that a prompt was answered or timed out.
func (n *Notifier) PermissionResolved(ctx context.Context, orgID, conversationID string, upd PermissionUpdate) {
	n.publish(ctx, typePermissionResolved, orgID, conversationID, upd)
}

// DeliverableUpdated announces a deliverable's state change.
func (n *Notifier) DeliverableUpdated(ctx context.Context, orgID, conversationID string, upd DeliverableUpdate) {
	n.publish(ctx, typeDeliverableUpdated, orgID, conversationID, upd)
}

// publish broadcasts one frame to its audience. In the multi-mode shape it
// fails closed: a resolution error drops the frame, because falling back to
// an org-wide broadcast would send the conversation to every team. A dropped
// message frame is repaired by the run station's tail-follow read, and any
// other frame by the client's next refetch.
func (n *Notifier) publish(ctx context.Context, eventType, orgID, conversationID string, data any) {
	if n == nil || n.hub == nil {
		return
	}
	evt := websocket.Event{Type: eventType, OrgID: orgID, ConversationID: conversationID, Data: data}
	if n.audience == nil {
		n.hub.Broadcast(evt)
		return
	}
	actx, cancel := context.WithTimeout(ctx, audienceTimeout)
	defer cancel()
	orgWide, userIDs, err := n.audience(actx, orgID, conversationID)
	if err != nil {
		frameLog.Error("resolve audience failed; dropping frame", "type", eventType, "org", orgID, "conversation", conversationID, "error", err)
		return
	}
	if orgWide {
		n.hub.Broadcast(evt)
		return
	}
	if len(userIDs) == 0 {
		// Nobody may read the conversation: no such row, an archived team,
		// or a creator who left the org. The hub refuses an empty set, so
		// the frame stops here.
		return
	}
	evt.UserIDs = userIDs
	n.hub.Broadcast(evt)
}
