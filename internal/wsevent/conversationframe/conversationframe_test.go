package conversationframe

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// recordingHub captures Broadcast calls so a test can assert exactly which
// envelopes a frame produced.
type recordingHub struct {
	events []websocket.Event
}

func (r *recordingHub) Broadcast(evt websocket.Event) { r.events = append(r.events, evt) }

// TestPublish_LocalShape_OneOrgScopedBroadcast pins the nil-audience shape
// local mode runs: one org-scoped envelope with no recipient set.
func TestPublish_LocalShape_OneOrgScopedBroadcast(t *testing.T) {
	hub := &recordingHub{}
	NewNotifier(hub, nil).ConversationUpdate(context.Background(), "org-1", "conv-1", StatusUpdate{Status: "running"})

	if len(hub.events) != 1 {
		t.Fatalf("got %d events, want 1", len(hub.events))
	}
	evt := hub.events[0]
	if evt.Type != "conversation_update" || evt.OrgID != "org-1" || evt.ConversationID != "conv-1" || evt.UserIDs != nil {
		t.Errorf("event = %+v; want an org-scoped conversation_update for conv-1 with no recipient set", evt)
	}
}

// TestPublish_MultiShape_OneBroadcastCarryingTheSet pins the multi-mode
// shape: the audience is resolved for the frame's org and conversation, and
// one envelope carries the resolved set, rather than one per recipient.
func TestPublish_MultiShape_OneBroadcastCarryingTheSet(t *testing.T) {
	hub := &recordingHub{}
	var gotOrg, gotConversation string
	n := NewNotifier(hub, func(_ context.Context, orgID, conversationID string) (bool, []string, error) {
		gotOrg, gotConversation = orgID, conversationID
		return false, []string{"user-a", "user-b"}, nil
	})
	n.PermissionRequest(context.Background(), "org-1", "conv-1", PermissionUpdate{ToolCallID: "tc-1"})

	if gotOrg != "org-1" || gotConversation != "conv-1" {
		t.Errorf("audience called with (%q, %q); want (org-1, conv-1)", gotOrg, gotConversation)
	}
	if len(hub.events) != 1 {
		t.Fatalf("got %d events, want 1", len(hub.events))
	}
	evt := hub.events[0]
	if evt.Type != "permission_request" || evt.OrgID != "org-1" || evt.ConversationID != "conv-1" || evt.UserID != "" {
		t.Errorf("event = %+v; want a permission_request for conv-1 in org-1 with no single user", evt)
	}
	if !slices.Equal(evt.UserIDs, []string{"user-a", "user-b"}) {
		t.Errorf("UserIDs = %v; want [user-a user-b]", evt.UserIDs)
	}
}

// TestPublish_OrgWideAnswer_OrgScopedBroadcast pins that an org-visible
// conversation goes out as an org-scoped envelope with no recipient set,
// because every member of the org may read it.
func TestPublish_OrgWideAnswer_OrgScopedBroadcast(t *testing.T) {
	hub := &recordingHub{}
	n := NewNotifier(hub, func(context.Context, string, string) (bool, []string, error) {
		return true, nil, nil
	})
	n.DeliverableUpdated(context.Background(), "org-1", "conv-1", DeliverableUpdate{DeliverableID: "d-1", State: "merged"})

	if len(hub.events) != 1 {
		t.Fatalf("got %d events, want 1", len(hub.events))
	}
	if evt := hub.events[0]; evt.OrgID != "org-1" || evt.UserIDs != nil {
		t.Errorf("event = %+v; want org-scoped with no recipient set", evt)
	}
}

// TestPublish_FailsClosed pins that a resolution error and an empty
// audience both send nothing: falling back to an org-wide broadcast would
// send the conversation to every team in the org.
func TestPublish_FailsClosed(t *testing.T) {
	cases := map[string]AudienceFunc{
		"resolution error": func(context.Context, string, string) (bool, []string, error) {
			return false, nil, errors.New("db down")
		},
		"empty audience": func(context.Context, string, string) (bool, []string, error) {
			return false, []string{}, nil
		},
	}
	for name, audience := range cases {
		t.Run(name, func(t *testing.T) {
			hub := &recordingHub{}
			NewNotifier(hub, audience).PermissionResolved(context.Background(), "org-1", "conv-1", PermissionUpdate{ToolCallID: "tc-1"})
			if len(hub.events) != 0 {
				t.Errorf("got %d events, want 0", len(hub.events))
			}
		})
	}
}
