package repoevent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// recordingHub captures Broadcast calls so tests can assert exactly which
// (org, user)-scoped envelopes a Publish produced.
type recordingHub struct {
	events []websocket.Event
}

func (r *recordingHub) Broadcast(evt websocket.Event) { r.events = append(r.events, evt) }

// TestPublish_LocalShape_SingleOrgScopedBroadcast pins the nil-recipients
// (local mode) path: exactly one event, org-scoped, no UserID — the
// pre-unification broadcast semantics, unchanged.
func TestPublish_LocalShape_SingleOrgScopedBroadcast(t *testing.T) {
	hub := &recordingHub{}
	n := NewNotifier(hub, nil)
	n.Publish(context.Background(), "org-1", Update{ID: "repo-1", Slug: "acme/api", ProfileText: Ptr("p")})

	if len(hub.events) != 1 {
		t.Fatalf("got %d events, want 1", len(hub.events))
	}
	evt := hub.events[0]
	if evt.Type != EventType || evt.OrgID != "org-1" || evt.UserID != "" {
		t.Errorf("event = {type:%q org:%q user:%q}; want {%q, org-1, \"\"}", evt.Type, evt.OrgID, evt.UserID, EventType)
	}
}

// TestPublish_FanOut_OneEventPerRecipient pins the multi-mode shape: the
// resolver is consulted with the org and the split (owner, repo) — read off
// the SLUG, since team tracking is the axis the audience is resolved on and
// the id would split into nothing — and one identically-bodied event goes out
// per returned user id. The hub's per-connection UserID filter (pkg/websocket
// hub_test) is what turns that into "a client whose teams don't track the repo
// receives nothing".
func TestPublish_FanOut_OneEventPerRecipient(t *testing.T) {
	hub := &recordingHub{}
	var gotOrg, gotOwner, gotRepo string
	n := NewNotifier(hub, func(_ context.Context, orgID, owner, repo string) ([]string, error) {
		gotOrg, gotOwner, gotRepo = orgID, owner, repo
		return []string{"user-a", "user-b"}, nil
	})
	upd := Update{ID: "repo-1", Slug: "acme/api", CloneStatus: Ptr("failed"), CloneError: Ptr("boom")}
	n.Publish(context.Background(), "org-1", upd)

	if gotOrg != "org-1" || gotOwner != "acme" || gotRepo != "api" {
		t.Errorf("resolver called with (%q, %q, %q); want (org-1, acme, api)", gotOrg, gotOwner, gotRepo)
	}
	if len(hub.events) != 2 {
		t.Fatalf("got %d events, want 2", len(hub.events))
	}
	for i, wantUser := range []string{"user-a", "user-b"} {
		evt := hub.events[i]
		if evt.Type != EventType || evt.OrgID != "org-1" || evt.UserID != wantUser {
			t.Errorf("event[%d] = {type:%q org:%q user:%q}; want {%q, org-1, %q}", i, evt.Type, evt.OrgID, evt.UserID, EventType, wantUser)
		}
		if data, ok := evt.Data.(Update); !ok || data != upd {
			t.Errorf("event[%d] data = %#v; want the published Update", i, evt.Data)
		}
	}
}

// TestPublish_FailsClosed pins that resolution problems DROP the event
// rather than widening it: a resolver error, an empty recipient set, and
// a malformed slug all produce zero broadcasts. Falling back to an
// org-wide broadcast here would re-open the cross-team disclosure the
// fan-out exists to prevent.
func TestPublish_FailsClosed(t *testing.T) {
	cases := []struct {
		name       string
		slug       string
		recipients RecipientsFunc
	}{
		{"resolver error", "acme/api", func(context.Context, string, string, string) ([]string, error) {
			return nil, errors.New("db down")
		}},
		{"no recipients", "acme/api", func(context.Context, string, string, string) ([]string, error) {
			return nil, nil
		}},
		{"malformed slug", "no-slash", func(context.Context, string, string, string) ([]string, error) {
			t.Error("resolver must not be called for a malformed slug")
			return []string{"user-a"}, nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := &recordingHub{}
			NewNotifier(hub, tc.recipients).Publish(context.Background(), "org-1", Update{ID: "repo-1", Slug: tc.slug})
			if len(hub.events) != 0 {
				t.Errorf("got %d events, want 0 (fail closed)", len(hub.events))
			}
		})
	}
}

// TestPublish_EmptyRecipientID_DroppedNotWidened pins the empty-id guard
// in the fan-out loop: the hub reads empty evt.UserID as "not
// user-specific", so a stray "" in the recipient slice would turn that
// envelope into an org-wide broadcast — the exact disclosure the fan-out
// prevents. It must be dropped while the legitimate recipients still
// receive theirs.
func TestPublish_EmptyRecipientID_DroppedNotWidened(t *testing.T) {
	hub := &recordingHub{}
	n := NewNotifier(hub, func(context.Context, string, string, string) ([]string, error) {
		return []string{"", "user-a"}, nil
	})
	n.Publish(context.Background(), "org-1", Update{ID: "repo-1", Slug: "acme/api", ProfileText: Ptr("p")})

	if len(hub.events) != 1 {
		t.Fatalf("got %d events, want 1 (empty-id envelope dropped)", len(hub.events))
	}
	if evt := hub.events[0]; evt.UserID != "user-a" {
		t.Errorf("event UserID = %q; want user-a, and never an empty (org-wide) envelope", evt.UserID)
	}
}

// TestPublish_NilSafety mirrors the hub's own nil tolerance: a nil
// Notifier and a Notifier over a nil hub both no-op, so emit sites need
// no guards (the profiler is constructed hub-less in several tests).
func TestPublish_NilSafety(t *testing.T) {
	var nilNotifier *Notifier
	nilNotifier.Publish(context.Background(), "org-1", Update{ID: "repo-1", Slug: "acme/api"})
	NewNotifier(nil, nil).Publish(context.Background(), "org-1", Update{ID: "repo-1", Slug: "acme/api"})
	var nilHub *websocket.Hub
	NewNotifier(nilHub, nil).Publish(context.Background(), "org-1", Update{ID: "repo-1", Slug: "acme/api"})
}

// TestUpdate_WireShape_SparseAndAllowlisted pins the wire JSON: the two
// identity fields are always present (id to merge on, slug to render), unset
// diff fields are absent (sparse diff, so the frontend's field-wise merge
// can't be clobbered by explicit zero values), set fields serialize under
// their REST names, and the payload is exactly the allowlist — no extra
// columns can ride along.
func TestUpdate_WireShape_SparseAndAllowlisted(t *testing.T) {
	sparse, err := json.Marshal(Update{ID: "repo-1", Slug: "acme/api", CloneStatus: Ptr("ok")})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(sparse) != `{"id":"repo-1","slug":"acme/api","clone_status":"ok"}` {
		t.Errorf("sparse wire JSON = %s; unset fields must be omitted", sparse)
	}

	full, err := json.Marshal(Update{
		ID:          "repo-1",
		Slug:        "acme/api",
		HasReadme:   Ptr(true),
		HasClaudeMd: Ptr(false),
		HasAgentsMd: Ptr(false),
		ProfileText: Ptr("p"), CloneStatus: Ptr("failed"),
		CloneError: Ptr("e"), CloneErrorKind: Ptr("ssh"),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var keys map[string]any
	if err := json.Unmarshal(full, &keys); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := []string{"id", "slug", "has_readme", "has_claude_md", "has_agents_md", "profile_text", "clone_status", "clone_error", "clone_error_kind"}
	if len(keys) != len(want) {
		t.Errorf("full payload has %d keys %v; want exactly the allowlist %v", len(keys), keys, want)
	}
	for _, k := range want {
		if _, ok := keys[k]; !ok {
			t.Errorf("allowlist field %q missing from wire JSON", k)
		}
	}
	// A false bool must serialize (pointer set), not vanish: absent and
	// false are different facts to the merging client.
	if v, ok := keys["has_claude_md"].(bool); !ok || v {
		t.Errorf("has_claude_md = %v; want explicit false on the wire", keys["has_claude_md"])
	}
}
