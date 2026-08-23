package knowledgeevent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/kbstore"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

type recorder struct{ events []websocket.Event }

func (r *recorder) Broadcast(e websocket.Event) { r.events = append(r.events, e) }

func (r *recorder) userIDs() []string {
	out := make([]string, len(r.events))
	for i, e := range r.events {
		out[i] = e.UserID
	}
	return out
}

// TestPublish_SharedIsOrgScoped: a change under shared/ is readable by any
// member of the org, so the hub's own org filter IS the audience — fanning it
// per user would be N envelopes for the same reach.
func TestPublish_SharedIsOrgScoped(t *testing.T) {
	rec := &recorder{}
	called := false
	n := NewNotifier(rec, func(context.Context, string, string) ([]string, error) {
		called = true
		return []string{"u1", "u2"}, nil
	})

	n.Publish(context.Background(), "org-1", "team-a", kbstore.RootShared)

	if len(rec.events) != 1 {
		t.Fatalf("published %d events, want 1", len(rec.events))
	}
	if rec.events[0].OrgID != "org-1" || rec.events[0].UserID != "" {
		t.Errorf("shared event = org %q user %q; want the org-scoped envelope", rec.events[0].OrgID, rec.events[0].UserID)
	}
	if called {
		t.Errorf("resolved the team's members for an org-readable change")
	}
}

// TestPublish_PrivateFansOutPerMember: private/ is readable only through its
// own team's gate, so the audience is resolved at emit time and delivered on
// the hub's UserID axis.
func TestPublish_PrivateFansOutPerMember(t *testing.T) {
	rec := &recorder{}
	n := NewNotifier(rec, func(_ context.Context, orgID, teamID string) ([]string, error) {
		if orgID != "org-1" || teamID != "team-a" {
			t.Errorf("resolved (%s, %s); want (org-1, team-a)", orgID, teamID)
		}
		return []string{"u1", "u2"}, nil
	})

	n.Publish(context.Background(), "org-1", "team-a", kbstore.RootPrivate)

	if got := rec.userIDs(); len(got) != 2 || got[0] != "u1" || got[1] != "u2" {
		t.Fatalf("fan-out = %v; want one envelope per member", got)
	}
	var payload Update
	raw, _ := json.Marshal(rec.events[0].Data)
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.TeamID != "team-a" || payload.Root != kbstore.RootPrivate {
		t.Errorf("payload = %+v; want the team and the root that changed", payload)
	}
}

// TestPublish_ResolutionFailureDropsTheEvent is the fail-closed rule: a missed
// ping costs a manual refresh, while falling back to an org-wide broadcast
// would announce a private-root change to the whole org.
func TestPublish_ResolutionFailureDropsTheEvent(t *testing.T) {
	rec := &recorder{}
	n := NewNotifier(rec, func(context.Context, string, string) ([]string, error) {
		return nil, errors.New("database is away")
	})

	n.Publish(context.Background(), "org-1", "team-a", kbstore.RootPrivate)

	if len(rec.events) != 0 {
		t.Fatalf("published %d events after a failed resolve; want none", len(rec.events))
	}
}

// TestPublish_EmptyRecipientIsDropped: an empty user id would silently widen
// the envelope to the whole org, since the hub reads it as "not user-specific".
func TestPublish_EmptyRecipientIsDropped(t *testing.T) {
	rec := &recorder{}
	n := NewNotifier(rec, func(context.Context, string, string) ([]string, error) {
		return []string{"", "u2"}, nil
	})

	n.Publish(context.Background(), "org-1", "team-a", kbstore.RootPrivate)

	if got := rec.userIDs(); len(got) != 1 || got[0] != "u2" {
		t.Fatalf("fan-out = %v; want the empty id dropped", got)
	}
}

// TestPublish_LocalShapeBroadcasts: with no members func — N=1, one user, no
// team boundary — every change is one org-scoped envelope.
func TestPublish_LocalShapeBroadcasts(t *testing.T) {
	rec := &recorder{}
	n := NewNotifier(rec, nil)

	n.Publish(context.Background(), "org-1", "team-a", kbstore.RootPrivate, kbstore.RootShared)

	if len(rec.events) != 2 {
		t.Fatalf("published %d events, want one per root", len(rec.events))
	}
	for _, e := range rec.events {
		if e.UserID != "" {
			t.Errorf("local mode fanned out per user (%q); it broadcasts org-wide", e.UserID)
		}
	}
}

// TestPublish_MoveAnnouncesBothRootsOnce: a publish empties one listing and
// fills the other, and each root reaches its own audience.
func TestPublish_MoveAnnouncesBothRootsOnce(t *testing.T) {
	rec := &recorder{}
	n := NewNotifier(rec, func(context.Context, string, string) ([]string, error) {
		return []string{"u1"}, nil
	})

	n.Publish(context.Background(), "org-1", "team-a", kbstore.RootPrivate, kbstore.RootShared, kbstore.RootPrivate)

	if len(rec.events) != 2 {
		t.Fatalf("published %d events; want one per distinct root", len(rec.events))
	}
	if rec.events[0].UserID != "u1" {
		t.Errorf("the private ping went to %q; want the resolved member", rec.events[0].UserID)
	}
	if rec.events[1].UserID != "" {
		t.Errorf("the shared ping fanned out per user; it is org-scoped")
	}
}

// TestPublish_IgnoresWhatItCannotAddress: an unknown root and an empty team
// both name no audience, and a ping with no audience is not sent.
func TestPublish_IgnoresWhatItCannotAddress(t *testing.T) {
	rec := &recorder{}
	n := NewNotifier(rec, nil)

	n.Publish(context.Background(), "org-1", "team-a", kbstore.Root("public"))
	n.Publish(context.Background(), "org-1", "", kbstore.RootShared)
	n.Publish(context.Background(), "org-1", "team-a")

	if len(rec.events) != 0 {
		t.Fatalf("published %d events; want none", len(rec.events))
	}
}

// TestPublish_NilHubIsSafe mirrors the hub's own nil-safety so emit sites do
// not have to guard.
func TestPublish_NilHubIsSafe(t *testing.T) {
	var n *Notifier
	n.Publish(context.Background(), "org-1", "team-a", kbstore.RootShared)
	NewNotifier(nil, nil).Publish(context.Background(), "org-1", "team-a", kbstore.RootShared)
}
