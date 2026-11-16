package wsbackplane

import (
	"encoding/json"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// wireEvent mirrors websocket.Event but ALSO serializes OrgID, UserID and
// UserIDs. Event deliberately hides those from the browser-facing wire
// JSON (see Event's doc comment in pkg/websocket) — but the backplane
// needs them on the wire between pods so the receiving pod's
// DeliverRemote can reapply the same per-connection scope filter
// Broadcast would have applied locally. ReadersOfConversation is not a
// field here: the envelope key an event travels under carries it (see
// wsEnvelope).
//
// omitempty on UserIDs loses nothing: Broadcast drops an event with an
// empty non-nil set before it reaches Publish, so an absent user_ids is
// always an event that had no set.
type wireEvent struct {
	Type           string          `json:"type"`
	ConversationID string          `json:"conversation_id,omitempty"`
	OrgID          string          `json:"org_id,omitempty"`
	UserID         string          `json:"user_id,omitempty"`
	UserIDs        []string        `json:"user_ids,omitempty"`
	Data           json.RawMessage `json:"data"`
}

func newWireEvent(evt websocket.Event) (wireEvent, error) {
	data, err := json.Marshal(evt.Data)
	if err != nil {
		return wireEvent{}, fmt.Errorf("marshal event data: %w", err)
	}
	return wireEvent{
		Type:           evt.Type,
		ConversationID: evt.ConversationID,
		OrgID:          evt.OrgID,
		UserID:         evt.UserID,
		UserIDs:        evt.UserIDs,
		Data:           data,
	}, nil
}

func (w wireEvent) toEvent() websocket.Event {
	var data any
	// A malformed Data payload degrades to nil rather than dropping the
	// whole event — Type/OrgID/ConversationID still route and render
	// correctly, only the event-specific payload is empty.
	_ = json.Unmarshal(w.Data, &data)
	return websocket.Event{
		Type:           w.Type,
		ConversationID: w.ConversationID,
		OrgID:          w.OrgID,
		UserID:         w.UserID,
		UserIDs:        w.UserIDs,
		Data:           data,
	}
}

// wsEnvelope is the tf_ws NOTIFY payload shape (spec §5: "{origin_pod,
// event}"). Event carries the inline payload when it fits under the
// configured threshold; OutboxID carries a ws_outbox row reference
// otherwise.
//
// An event marked ReadersOfConversation travels under keys of its own,
// ReadersEvent and ReadersOutboxID, never Event or OutboxID. Pods on a
// build without the marker read only event and outbox_id, so they find
// neither and drop the envelope; under the shared keys they would deliver
// the event by its org alone, to every team in it. Exactly one of the four
// is set.
type wsEnvelope struct {
	OriginInstanceID string     `json:"origin_instance_id"`
	Event            *wireEvent `json:"event,omitempty"`
	OutboxID         int64      `json:"outbox_id,omitempty"`
	ReadersEvent     *wireEvent `json:"readers_event,omitempty"`
	ReadersOutboxID  int64      `json:"readers_outbox_id,omitempty"`
}

// inlineEnvelope is the envelope carrying we inline, under the readers key
// when its event is marked ReadersOfConversation.
func inlineEnvelope(originID string, we *wireEvent, forReaders bool) wsEnvelope {
	if forReaders {
		return wsEnvelope{OriginInstanceID: originID, ReadersEvent: we}
	}
	return wsEnvelope{OriginInstanceID: originID, Event: we}
}

// outboxEnvelope is the envelope referencing ws_outbox row outboxID, under
// the readers key when its event is marked ReadersOfConversation.
func outboxEnvelope(originID string, outboxID int64, forReaders bool) wsEnvelope {
	if forReaders {
		return wsEnvelope{OriginInstanceID: originID, ReadersOutboxID: outboxID}
	}
	return wsEnvelope{OriginInstanceID: originID, OutboxID: outboxID}
}

// kickEnvelope is the tf_ctl NOTIFY payload shape for the cross-pod
// session kick (TFAC-584). Kind is always "kick": tf_ctl is a shared,
// kind-discriminated channel (conversation-signal doorbells and brain trigger
// relays ride it too) and internal/app's unified dispatcher routes on
// that field — see ChannelCtl's doc comment. Sid/OrgID are optional
// narrowing filters, mirroring websocket.Hub.CloseUserConnections's own
// (userID, sid, orgID) signature exactly, so the receiving pod's dispatch
// is a direct pass-through.
type kickEnvelope struct {
	Kind             string `json:"kind"`
	OriginInstanceID string `json:"origin_instance_id"`
	UserID           string `json:"user_id"`
	Sid              string `json:"sid,omitempty"`
	OrgID            string `json:"org_id,omitempty"`
	Code             int    `json:"code"`
	Reason           string `json:"reason"`
}

// busEnvelope is the tf_bus NOTIFY payload shape for the brain-bound run
// sentinel relay (TFAC-592, spec §5.3). Event is always inline — run
// sentinels are small metadata (tool name, status), never worth an
// outbox row — but a sentinel that somehow exceeds the NOTIFY ceiling is
// dropped with a warning rather than erroring the publish, consistent
// with tf_bus's lossy-by-contract rule (an observability seam, never
// load-bearing state).
type busEnvelope struct {
	OriginInstanceID string       `json:"origin_instance_id"`
	Event            domain.Event `json:"event"`
}
