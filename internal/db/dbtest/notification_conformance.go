package dbtest

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// NotificationFixture is one subtest's world. System writes go through
// Stores; viewer reads and marks go through As, which a Postgres backend may
// run under the viewer's claims on the app pool.
type NotificationFixture struct {
	Stores db.Stores
	OrgID  string
	// Viewer is an org admin and an admin of Team and ArchivedTeam.
	Viewer string
	// Other is another member of the org.
	Other        string
	Team         string
	ArchivedTeam string
	As           func(t *testing.T, userID string, fn func(s db.NotificationStore))
}

type NotificationFactory func(t *testing.T) NotificationFixture

func RunNotificationStoreConformance(t *testing.T, mk NotificationFactory) {
	ctx := t.Context()
	yes, no := true, false

	raise := func(t *testing.T, f NotificationFixture, o db.NotificationOccurrence) db.NotificationUpsert {
		t.Helper()
		if o.OrgID == "" {
			o.OrgID = f.OrgID
		}
		if o.Origin == "" {
			o.Origin = domain.NotificationOriginSystem
		}
		if o.Lifecycle == "" {
			o.Lifecycle = domain.NotificationCondition
		}
		if o.Kind == "" {
			o.Kind = "test.kind"
		}
		if o.Audience.Scope == "" {
			o.Audience = domain.NotificationAudience{Scope: domain.NotificationScopeOrg, Role: domain.NotificationRoleAdmins}
		}
		if o.Priority == "" {
			o.Priority = domain.NotificationHigh
		}
		if o.Title == "" {
			o.Title = "title"
		}
		u, err := f.Stores.Notifications.UpsertOpenSystem(ctx, o)
		if err != nil {
			t.Fatalf("UpsertOpenSystem: %v", err)
		}
		return u
	}
	get := func(t *testing.T, f NotificationFixture, id string) *domain.NotificationView {
		t.Helper()
		var v *domain.NotificationView
		f.As(t, f.Viewer, func(s db.NotificationStore) {
			var err error
			if v, err = s.GetForViewer(ctx, f.OrgID, f.Viewer, id); err != nil {
				t.Fatalf("GetForViewer: %v", err)
			}
		})
		return v
	}
	inbox := func(t *testing.T, f NotificationFixture, filter db.NotificationListFilter) ([]domain.NotificationView, int) {
		t.Helper()
		var items []domain.NotificationView
		var total int
		f.As(t, f.Viewer, func(s db.NotificationStore) {
			var err error
			if items, total, err = s.ListForViewer(ctx, f.OrgID, f.Viewer, filter, db.ListOpts{Limit: 50}); err != nil {
				t.Fatalf("ListForViewer: %v", err)
			}
		})
		return items, total
	}
	ids := func(items []domain.NotificationView) []string {
		out := make([]string, len(items))
		for i, v := range items {
			out[i] = v.ID
		}
		return out
	}
	stored := func(t *testing.T, f NotificationFixture, id string) func() (*domain.Notification, error) {
		return func() (*domain.Notification, error) {
			if v := get(t, f, id); v != nil {
				return &v.Notification, nil
			}
			return nil, nil
		}
	}
	// entity creates a pull request entity for an event row to be about.
	entity := func(t *testing.T, f NotificationFixture, key string) string {
		t.Helper()
		e, _, err := f.Stores.Entities.FindOrCreateSystem(ctx, f.OrgID, domain.ProviderGitHub, TestGitHubHost, key, "", "pr", "t", "")
		if err != nil {
			t.Fatalf("create entity %s: %v", key, err)
		}
		return e.ID
	}
	setMarks := func(t *testing.T, f NotificationFixture, id string, p db.NotificationMarksPatch) (domain.NotificationMarks, error) {
		t.Helper()
		var m domain.NotificationMarks
		var err error
		f.As(t, f.Viewer, func(s db.NotificationStore) { m, err = s.SetMarks(ctx, f.OrgID, f.Viewer, id, p) })
		return m, err
	}

	t.Run("upsert_coalesces_and_escalates", func(t *testing.T) {
		f := mk(t)
		first := raise(t, f, db.NotificationOccurrence{Key: "k", Dismissible: true, Priority: domain.NotificationLow,
			Actions: []byte(`[{"id":"a"}]`), Params: []byte(`{"x":1}`)})
		if !first.Opened || first.Escalated || first.Notification.SeenCount != 1 {
			t.Fatalf("first raise = %+v; want opened", first)
		}
		AssertWriteReturnedStoredRow(t, "UpsertOpenSystem insert", first.Notification, stored(t, f, first.Notification.ID))

		bump := raise(t, f, db.NotificationOccurrence{Key: "k", Dismissible: true, Priority: domain.NotificationLow, Title: "new"})
		if bump.Opened || bump.Escalated || bump.Notification.ID != first.Notification.ID ||
			bump.Notification.SeenCount != 2 || bump.Notification.Title != "new" ||
			!bump.Notification.EscalatedAt.Equal(first.Notification.EscalatedAt) {
			t.Fatalf("bump = %+v; want the same row, seen twice, retitled, not escalated", bump)
		}
		AssertWriteReturnedStoredRow(t, "UpsertOpenSystem bump", bump.Notification, stored(t, f, first.Notification.ID))

		if _, err := setMarks(t, f, first.Notification.ID, db.NotificationMarksPatch{Read: &yes, Dismissed: &yes}); err != nil {
			t.Fatalf("SetMarks: %v", err)
		}
		if v := get(t, f, first.Notification.ID); !v.Read || !v.Dismissed {
			t.Fatalf("after marking: read=%v dismissed=%v", v.Read, v.Dismissed)
		}
		up := raise(t, f, db.NotificationOccurrence{Key: "k", Dismissible: true, Priority: domain.NotificationUrgent})
		if !up.Escalated || up.Notification.Priority != domain.NotificationUrgent {
			t.Fatalf("priority increase = %+v; want escalated", up)
		}
		if v := get(t, f, first.Notification.ID); v.Read || v.Dismissed {
			t.Errorf("after escalation: read=%v dismissed=%v; want both cleared", v.Read, v.Dismissed)
		}
		if down := raise(t, f, db.NotificationOccurrence{Key: "k", Dismissible: true, Priority: domain.NotificationLow}); down.Escalated ||
			!down.Notification.EscalatedAt.Equal(up.Notification.EscalatedAt) {
			t.Errorf("priority decrease = %+v; want no escalation", down)
		}

		if _, err := f.Stores.Notifications.UpsertOpenSystem(ctx, db.NotificationOccurrence{
			OrgID: f.OrgID, Origin: domain.NotificationOriginSystem, Lifecycle: domain.NotificationCondition,
			Kind: "test.kind", Priority: domain.NotificationHigh, Title: "t",
			Audience: domain.NotificationAudience{Scope: domain.NotificationScopeTeam, Role: domain.NotificationRoleAdmins},
		}); !errors.Is(err, db.ErrInvalidNotification) {
			t.Errorf("team audience with no team: err = %v; want ErrInvalidNotification", err)
		}
	})

	t.Run("resolves", func(t *testing.T) {
		f := mk(t)
		orgAdmins := domain.NotificationAudience{Scope: domain.NotificationScopeOrg, Role: domain.NotificationRoleAdmins}
		teamAdmins := domain.NotificationAudience{Scope: domain.NotificationScopeTeam, Role: domain.NotificationRoleAdmins, TeamID: f.Team}
		a1 := raise(t, f, db.NotificationOccurrence{Key: "a", Audience: orgAdmins}).Notification
		a2 := raise(t, f, db.NotificationOccurrence{Key: "a", Audience: teamAdmins}).Notification
		b := raise(t, f, db.NotificationOccurrence{Key: "b", Source: "linear"}).Notification
		c := raise(t, f, db.NotificationOccurrence{Key: "c"}).Notification
		s := f.Stores.Notifications

		got, err := s.ResolveAudiencesNotInSystem(ctx, f.OrgID, domain.NotificationOriginSystem, "test.kind", "a", []domain.NotificationAudience{orgAdmins})
		if err != nil || len(got) != 1 || got[0].ID != a2.ID || got[0].ResolvedReason != domain.NotificationResolvedAudienceRemoved {
			t.Fatalf("ResolveAudiencesNotInSystem = %+v, %v; want the team row", got, err)
		}
		if got, err := s.ResolveKeysNotInSystem(ctx, f.OrgID, domain.NotificationOriginSystem, "test.kind", []string{"a", "b"}); err != nil ||
			len(got) != 1 || got[0].ID != c.ID || got[0].ResolvedReason != domain.NotificationResolvedCleared {
			t.Fatalf("ResolveKeysNotInSystem = %+v, %v; want c", got, err)
		}
		if got, err := s.ResolveSourceSystem(ctx, f.OrgID, "linear"); err != nil ||
			len(got) != 1 || got[0].ID != b.ID || got[0].ResolvedReason != domain.NotificationResolvedSourceOff {
			t.Fatalf("ResolveSourceSystem = %+v, %v; want b", got, err)
		}
		if got, err := s.ResolveSystem(ctx, f.OrgID, domain.NotificationOriginSystem, "test.kind", "a"); err != nil ||
			len(got) != 1 || got[0].ID != a1.ID || got[0].ResolvedAt == nil {
			t.Fatalf("ResolveSystem = %+v, %v; want a1", got, err)
		}
		again := raise(t, f, db.NotificationOccurrence{Key: "a", Audience: orgAdmins})
		if !again.Opened || again.Notification.ID == a1.ID {
			t.Errorf("raise after resolve = %+v; want a new occurrence", again)
		}
		if _, total := inbox(t, f, db.NotificationListFilter{Status: domain.NotificationResolved}); total != 4 {
			t.Errorf("resolved count = %d; want 4", total)
		}

		// One row by id: its sibling audience of the same key stays open.
		sibling := raise(t, f, db.NotificationOccurrence{Key: "a", Audience: teamAdmins}).Notification
		one, err := s.ResolveIDSystem(ctx, f.OrgID, again.Notification.ID, domain.NotificationResolvedAudienceRemoved)
		if err != nil || one == nil || one.ID != again.Notification.ID || one.ResolvedAt == nil ||
			one.ResolvedReason != domain.NotificationResolvedAudienceRemoved {
			t.Fatalf("ResolveIDSystem = %+v, %v; want the org admins row resolved for the reason given", one, err)
		}
		AssertWriteReturnedStoredRow(t, "ResolveIDSystem", *one, stored(t, f, one.ID))
		if v := get(t, f, sibling.ID); v == nil || v.ResolvedAt != nil {
			t.Errorf("sibling audience after ResolveIDSystem = %+v; want it open", v)
		}
		for _, id := range []string{again.Notification.ID, "not-a-uuid", "00000000-0000-4000-8000-000000000000"} {
			if got, err := s.ResolveIDSystem(ctx, f.OrgID, id, domain.NotificationResolvedCleared); err != nil || got != nil {
				t.Errorf("ResolveIDSystem(%q) = %+v, %v; want nil for a row that is not open", id, got, err)
			}
		}
		expires := time.Now().Add(time.Hour)
		n := raise(t, f, db.NotificationOccurrence{Key: "n", Lifecycle: domain.NotificationNotice, Dismissible: true, ExpiresAt: &expires}).Notification
		if got, err := s.ResolveIDSystem(ctx, f.OrgID, n.ID, domain.NotificationResolvedCleared); err != nil || got != nil {
			t.Errorf("ResolveIDSystem on a notice = %+v, %v; want nil: a notice expires", got, err)
		}
	})

	t.Run("resolution", func(t *testing.T) {
		f := mk(t)
		s := f.Stores.Notifications
		open := raise(t, f, db.NotificationOccurrence{Key: "open"}).Notification
		done := raise(t, f, db.NotificationOccurrence{Key: "done"}).Notification
		if got, err := s.ResolveIDSystem(ctx, f.OrgID, done.ID, domain.NotificationResolvedCleared); err != nil || got == nil || got.Resolution != "" {
			t.Fatalf("ResolveIDSystem = %+v, %v; want the row resolved with no resolution yet", got, err)
		}

		got, err := s.SetResolutionSystem(ctx, f.OrgID, done.ID, "GitHub was connected again.")
		if err != nil || got == nil || got.Resolution != "GitHub was connected again." {
			t.Fatalf("SetResolutionSystem = %+v, %v; want the row with its resolution", got, err)
		}
		AssertWriteReturnedStoredRow(t, "SetResolutionSystem", *got, stored(t, f, done.ID))
		if v := get(t, f, done.ID); v == nil || v.Resolution != "GitHub was connected again." {
			t.Errorf("viewer read = %+v; want the resolution", v)
		}

		// An open row has no resolution to describe, and a row that is not
		// there answers nothing.
		for _, id := range []string{open.ID, "not-a-uuid", "00000000-0000-4000-8000-000000000000"} {
			if got, err := s.SetResolutionSystem(ctx, f.OrgID, id, "x"); err != nil || got != nil {
				t.Errorf("SetResolutionSystem(%q) = %+v, %v; want nil for a row that is not resolved", id, got, err)
			}
		}
		if v := get(t, f, open.ID); v == nil || v.Resolution != "" {
			t.Errorf("open row = %+v; want no resolution", v)
		}
	})

	t.Run("open_condition_orgs", func(t *testing.T) {
		f := mk(t)
		s := f.Stores.Notifications
		openIn := func(t *testing.T, origin domain.NotificationOrigin, kind string) []string {
			t.Helper()
			got, err := s.OpenConditionOrgsSystem(ctx, origin, kind)
			if err != nil {
				t.Fatalf("OpenConditionOrgsSystem(%s): %v", kind, err)
			}
			return got
		}
		system := domain.NotificationOriginSystem
		if got := openIn(t, system, "test.open"); len(got) != 0 {
			t.Errorf("before any raise = %v; want none", got)
		}
		teamAdmins := domain.NotificationAudience{Scope: domain.NotificationScopeTeam, Role: domain.NotificationRoleAdmins, TeamID: f.Team}
		raise(t, f, db.NotificationOccurrence{Kind: "test.open", Key: "a"})
		raise(t, f, db.NotificationOccurrence{Kind: "test.open", Key: "a", Audience: teamAdmins})
		raise(t, f, db.NotificationOccurrence{Kind: "test.open", Key: "b"})
		if got := openIn(t, system, "test.open"); !slices.Equal(got, []string{f.OrgID}) {
			t.Errorf("with three open rows = %v; want the org once", got)
		}

		expires := time.Now().Add(time.Hour)
		raise(t, f, db.NotificationOccurrence{Kind: "test.notice", Key: "n", Lifecycle: domain.NotificationNotice,
			Dismissible: true, Priority: domain.NotificationLow, ExpiresAt: &expires})
		if got := openIn(t, system, "test.notice"); len(got) != 0 {
			t.Errorf("a notice = %v; want none: it is not a condition", got)
		}
		raise(t, f, db.NotificationOccurrence{Kind: "test.event", Key: "e", Origin: domain.NotificationOriginEvent, EntityID: entity(t, f, "octo/notify#1")})
		if got := openIn(t, system, "test.event"); len(got) != 0 {
			t.Errorf("another origin's condition = %v; want none", got)
		}

		if _, err := s.ResolveKeysNotInSystem(ctx, f.OrgID, system, "test.open", nil); err != nil {
			t.Fatal(err)
		}
		if got := openIn(t, system, "test.open"); len(got) != 0 {
			t.Errorf("once every row resolves = %v; want none", got)
		}
	})

	// An event row names its entity, and only an event row does. The close
	// phase reads an entity's open event conditions by kind, and a recurring
	// event notice resurfaces: unread and undismissed again.
	t.Run("event_rows", func(t *testing.T) {
		f := mk(t)
		s := f.Stores.Notifications
		pr, other := entity(t, f, "octo/notify#1"), entity(t, f, "octo/notify#2")
		event := domain.NotificationOriginEvent
		user := domain.NotificationAudience{Scope: domain.NotificationScopeUser, UserID: f.Viewer}

		for _, o := range []db.NotificationOccurrence{
			{OrgID: f.OrgID, Origin: event, Lifecycle: domain.NotificationCondition, Kind: "k", Key: "x", Audience: user, Priority: domain.NotificationHigh, Title: "t"},
			{OrgID: f.OrgID, Origin: domain.NotificationOriginSystem, Lifecycle: domain.NotificationCondition, Kind: "k", Key: "x", Audience: user, Priority: domain.NotificationHigh, Title: "t", EntityID: pr},
		} {
			if _, err := s.UpsertOpenSystem(ctx, o); !errors.Is(err, db.ErrInvalidNotification) {
				t.Errorf("UpsertOpenSystem(origin %s, entity %q) = %v; want ErrInvalidNotification", o.Origin, o.EntityID, err)
			}
		}

		failed := raise(t, f, db.NotificationOccurrence{Origin: event, Kind: "ev:failed", Key: pr + "/", EntityID: pr, Audience: user, Dismissible: true}).Notification
		if failed.EntityID != pr || failed.Origin != event {
			t.Fatalf("event row = %+v; want origin event about %s", failed, pr)
		}
		AssertWriteReturnedStoredRow(t, "UpsertOpenSystem event row", failed, stored(t, f, failed.ID))
		raise(t, f, db.NotificationOccurrence{Origin: event, Kind: "ev:failed", Key: pr + "/", EntityID: pr, Audience: domain.NotificationAudience{
			Scope: domain.NotificationScopeTeam, Role: domain.NotificationRoleMembers, TeamID: f.Team}, Dismissible: true})
		raise(t, f, db.NotificationOccurrence{Origin: event, Kind: "ev:requested", Key: pr + "/user:1", EntityID: pr, Audience: user, Dismissible: true})
		raise(t, f, db.NotificationOccurrence{Origin: event, Kind: "ev:failed", Key: other + "/", EntityID: other, Audience: user, Dismissible: true})
		expires := time.Now().Add(time.Hour)
		notice := raise(t, f, db.NotificationOccurrence{Origin: event, Lifecycle: domain.NotificationNotice, Kind: "ev:commented", Key: pr + "/",
			EntityID: pr, Audience: user, Dismissible: true, ExpiresAt: &expires}).Notification

		got, err := s.OpenEventKeysSystem(ctx, f.OrgID, pr, []string{"ev:failed", "ev:requested", "ev:commented", "ev:none"})
		want := []db.NotificationKindKey{{Kind: "ev:failed", Key: pr + "/"}, {Kind: "ev:requested", Key: pr + "/user:1"}}
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("OpenEventKeysSystem = %+v, %v; want %+v: the entity's open conditions of those kinds, once each, no notice", got, err, want)
		}
		if got, err := s.OpenEventKeysSystem(ctx, f.OrgID, pr, nil); err != nil || len(got) != 0 {
			t.Errorf("OpenEventKeysSystem with no kinds = %+v, %v; want none", got, err)
		}
		if resolved, err := s.ResolveSystem(ctx, f.OrgID, event, "ev:failed", pr+"/"); err != nil || len(resolved) != 2 {
			t.Fatalf("ResolveSystem(ev:failed) = %+v, %v; want both audiences' rows", resolved, err)
		}
		if got, err := s.OpenEventKeysSystem(ctx, f.OrgID, pr, []string{"ev:failed", "ev:requested"}); err != nil ||
			!slices.Equal(got, []db.NotificationKindKey{{Kind: "ev:requested", Key: pr + "/user:1"}}) {
			t.Errorf("OpenEventKeysSystem after the resolve = %+v, %v; want only ev:requested", got, err)
		}

		if _, err := setMarks(t, f, notice.ID, db.NotificationMarksPatch{Read: &yes, Dismissed: &yes}); err != nil {
			t.Fatalf("mark the notice: %v", err)
		}
		quiet := raise(t, f, db.NotificationOccurrence{Origin: event, Lifecycle: domain.NotificationNotice, Kind: "ev:commented", Key: pr + "/",
			EntityID: pr, Audience: user, Dismissible: true, ExpiresAt: &expires})
		if quiet.Escalated {
			t.Errorf("a bump without Resurface escalated: %+v", quiet)
		}
		if v := get(t, f, notice.ID); v == nil || !v.Read || !v.Dismissed {
			t.Errorf("after a quiet bump = %+v; want it still read and dismissed", v)
		}
		loud := raise(t, f, db.NotificationOccurrence{Origin: event, Lifecycle: domain.NotificationNotice, Kind: "ev:commented", Key: pr + "/",
			EntityID: pr, Audience: user, Dismissible: true, ExpiresAt: &expires, Resurface: true})
		if loud.Opened || !loud.Escalated || loud.Notification.ID != notice.ID || loud.Notification.SeenCount != 3 {
			t.Errorf("a resurfacing bump = %+v; want the same row escalated", loud)
		}
		if v := get(t, f, notice.ID); v == nil || v.Read || v.Dismissed {
			t.Errorf("after a resurfacing bump = %+v; want it unread and undismissed", v)
		}
	})

	t.Run("event_rows_on_closed_entities", func(t *testing.T) {
		f := mk(t)
		s := f.Stores.Notifications
		closing, open := entity(t, f, "octo/closed#1"), entity(t, f, "octo/closed#2")
		event := domain.NotificationOriginEvent
		user := domain.NotificationAudience{Scope: domain.NotificationScopeUser, UserID: f.Viewer}
		team := domain.NotificationAudience{Scope: domain.NotificationScopeTeam, Role: domain.NotificationRoleMembers, TeamID: f.Team}
		expires := time.Now().Add(time.Hour)

		raise(t, f, db.NotificationOccurrence{Origin: event, Kind: "ev:failed", Key: closing + "/", EntityID: closing, Audience: user, Dismissible: true})
		raise(t, f, db.NotificationOccurrence{Origin: event, Kind: "ev:failed", Key: closing + "/", EntityID: closing, Audience: team, Dismissible: true})
		notice := raise(t, f, db.NotificationOccurrence{Origin: event, Lifecycle: domain.NotificationNotice, Kind: "ev:commented", Key: closing + "/",
			EntityID: closing, Audience: user, Dismissible: true, ExpiresAt: &expires}).Notification
		raise(t, f, db.NotificationOccurrence{Origin: event, Kind: "ev:failed", Key: open + "/", EntityID: open, Audience: user, Dismissible: true})

		if got, err := s.ResolveClosedEntityEventsSystem(ctx, f.OrgID); err != nil || len(got) != 0 {
			t.Fatalf("ResolveClosedEntityEventsSystem with every entity active = %+v, %v; want nothing", got, err)
		}
		if orgs, err := s.ClosedEntityEventOrgsSystem(ctx); err != nil || slices.Contains(orgs, f.OrgID) {
			t.Fatalf("ClosedEntityEventOrgsSystem with every entity active = %v, %v; want no %s", orgs, err, f.OrgID)
		}
		if _, err := f.Stores.Entities.CloseSystem(ctx, f.OrgID, closing); err != nil {
			t.Fatalf("close entity: %v", err)
		}
		if orgs, err := s.ClosedEntityEventOrgsSystem(ctx); err != nil || !slices.Contains(orgs, f.OrgID) {
			t.Fatalf("ClosedEntityEventOrgsSystem after the close = %v, %v; want %s", orgs, err, f.OrgID)
		}
		got, err := s.ResolveClosedEntityEventsSystem(ctx, f.OrgID)
		if err != nil || len(got) != 2 {
			t.Fatalf("ResolveClosedEntityEventsSystem = %+v, %v; want the closed entity's condition, both audiences", got, err)
		}
		for _, n := range got {
			if n.EntityID != closing || n.ResolvedAt == nil || n.ResolvedReason != domain.NotificationResolvedCleared {
				t.Errorf("resolved %+v; want a cleared row about %s", n, closing)
			}
		}
		if kept, err := s.OpenEventKeysSystem(ctx, f.OrgID, open, []string{"ev:failed"}); err != nil || len(kept) != 1 {
			t.Errorf("the active entity's condition = %+v, %v; want it still open", kept, err)
		}
		if v := get(t, f, notice.ID); v == nil || v.ResolvedAt != nil {
			t.Errorf("the closed entity's notice = %+v; want it open until it expires", v)
		}
		if again, err := s.ResolveClosedEntityEventsSystem(ctx, f.OrgID); err != nil || len(again) != 0 {
			t.Errorf("a second pass = %+v, %v; want nothing", again, err)
		}
		if orgs, err := s.ClosedEntityEventOrgsSystem(ctx); err != nil || slices.Contains(orgs, f.OrgID) {
			t.Errorf("ClosedEntityEventOrgsSystem after the resolve = %v, %v; want no %s", orgs, err, f.OrgID)
		}
	})

	t.Run("notices_expire_and_gc", func(t *testing.T) {
		f := mk(t)
		past, future := time.Now().Add(-time.Minute), time.Now().Add(time.Hour)
		notice := func(at time.Time) db.NotificationUpsert {
			return raise(t, f, db.NotificationOccurrence{Key: "n", Lifecycle: domain.NotificationNotice,
				Dismissible: true, Priority: domain.NotificationLow, ExpiresAt: &at})
		}
		expired := notice(past)
		if get(t, f, expired.Notification.ID) != nil {
			t.Errorf("expired notice is readable")
		}
		fresh := notice(future)
		if !fresh.Opened || fresh.Notification.ID == expired.Notification.ID {
			t.Errorf("re-post after expiry = %+v; want a new notice", fresh)
		}
		resolved := raise(t, f, db.NotificationOccurrence{Key: "r"}).Notification
		if _, err := f.Stores.Notifications.ResolveSystem(ctx, f.OrgID, domain.NotificationOriginSystem, "test.kind", "r"); err != nil {
			t.Fatal(err)
		}
		open := raise(t, f, db.NotificationOccurrence{Key: "o"}).Notification

		if n, err := f.Stores.Notifications.DeleteRetiredSystem(ctx, time.Now()); err != nil || n != 0 {
			t.Fatalf("DeleteRetiredSystem(now) = %d, %v; want 0", n, err)
		}
		if n, err := f.Stores.Notifications.DeleteRetiredSystem(ctx, time.Now().Add(db.NotificationResolvedRetention+time.Hour)); err != nil || n != 2 {
			t.Fatalf("DeleteRetiredSystem(+31d) = %d, %v; want 2", n, err)
		}
		if get(t, f, resolved.ID) != nil || get(t, f, fresh.Notification.ID) != nil || get(t, f, open.ID) == nil {
			t.Errorf("GC kept the wrong rows")
		}
	})

	t.Run("lists", func(t *testing.T) {
		f := mk(t)
		urgent := raise(t, f, db.NotificationOccurrence{Key: "u", Priority: domain.NotificationUrgent}).Notification
		team := raise(t, f, db.NotificationOccurrence{Key: "t", Audience: domain.NotificationAudience{
			Scope: domain.NotificationScopeTeam, Role: domain.NotificationRoleMembers, TeamID: f.Team}}).Notification
		mine := raise(t, f, db.NotificationOccurrence{Key: "me", Priority: domain.NotificationLow, Audience: domain.NotificationAudience{
			Scope: domain.NotificationScopeUser, UserID: f.Viewer}}).Notification
		raise(t, f, db.NotificationOccurrence{Key: "other", Audience: domain.NotificationAudience{
			Scope: domain.NotificationScopeUser, UserID: f.Other}})
		archived := raise(t, f, db.NotificationOccurrence{Key: "a", Audience: domain.NotificationAudience{
			Scope: domain.NotificationScopeTeam, Role: domain.NotificationRoleAdmins, TeamID: f.ArchivedTeam}}).Notification

		items, total := inbox(t, f, db.NotificationListFilter{})
		if want := []string{urgent.ID, team.ID, mine.ID}; total != 3 || !slices.Equal(ids(items), want) {
			t.Fatalf("inbox = %v (total %d); want %v in order", ids(items), total, want)
		}
		for _, v := range items {
			if !v.Sent {
				t.Errorf("inbox row %s reads as not sent", v.ID)
			}
		}
		// The archived team's row is one the viewer may read and is not sent.
		if v := get(t, f, archived.ID); v == nil || v.Sent {
			t.Errorf("archived team's row = %+v; want it readable and not sent", v)
		}
		if v := get(t, f, team.ID); v == nil || !v.Sent {
			t.Errorf("team row = %+v; want it sent", v)
		}
		if _, err := setMarks(t, f, urgent.ID, db.NotificationMarksPatch{Read: &yes}); err != nil {
			t.Fatal(err)
		}
		for name, c := range map[string]struct {
			f    db.NotificationListFilter
			want []string
		}{
			"unread":   {db.NotificationListFilter{Unread: &yes}, []string{team.ID, mine.ID}},
			"read":     {db.NotificationListFilter{Unread: &no}, []string{urgent.ID}},
			"priority": {db.NotificationListFilter{PriorityIn: []domain.NotificationPriority{domain.NotificationUrgent, domain.NotificationLow}}, []string{urgent.ID, mine.ID}},
			"scope":    {db.NotificationListFilter{ScopeIn: []domain.NotificationScope{domain.NotificationScopeTeam}}, []string{team.ID}},
			"open":     {db.NotificationListFilter{Status: domain.NotificationOpen, Dismissed: &no}, []string{urgent.ID, team.ID, mine.ID}},
		} {
			if items, total := inbox(t, f, c.f); total != len(c.want) || !slices.Equal(ids(items), c.want) {
				t.Errorf("%s: %v (total %d); want %v", name, ids(items), total, c.want)
			}
		}

		f.As(t, f.Viewer, func(s db.NotificationStore) {
			items, total, err := s.ListForOrg(ctx, f.OrgID, f.Viewer, db.NotificationListFilter{}, db.ListOpts{Limit: 2})
			if err != nil || total != 4 || len(items) != 2 {
				t.Errorf("ListForOrg page = %d items, total %d, %v; want 2 of 4", len(items), total, err)
			}
			items, total, err = s.ListForOrg(ctx, f.OrgID, f.Viewer, db.NotificationListFilter{TeamIDIn: []string{f.ArchivedTeam}}, db.ListOpts{Limit: 50})
			if err != nil || total != 1 || items[0].ID != archived.ID {
				t.Errorf("ListForOrg team_id_in = %v, %v; want the archived team's row", ids(items), err)
			} else if items[0].Sent {
				t.Errorf("ListForOrg: the archived team's row reads as sent")
			}
			items, _, err = s.ListForOrg(ctx, f.OrgID, f.Viewer, db.NotificationListFilter{}, db.ListOpts{Limit: 50})
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range items {
				if v.Sent != (v.ID != archived.ID) {
					t.Errorf("ListForOrg row %s sent = %v", v.ID, v.Sent)
				}
			}
			counted, countTotal, err := s.ListForViewer(ctx, f.OrgID, f.Viewer, db.NotificationListFilter{}, db.ListOpts{CountOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			AssertCountOnlyList(t, "ListForViewer", len(counted), countTotal, 3)
			if _, _, err := s.ListForViewer(ctx, f.OrgID, f.Viewer, db.NotificationListFilter{Status: "bogus"}, db.ListOpts{Limit: 50}); !errors.Is(err, db.ErrInvalidNotificationFilter) {
				t.Errorf("bad status: err = %v", err)
			}
		})
	})

	t.Run("sorts_by_resolved_at", func(t *testing.T) {
		f := mk(t)
		s := f.Stores.Notifications
		// Resolved high, then urgent, then low: the default order (urgent,
		// high, low), resolved_at desc and resolved_at asc all differ.
		high := raise(t, f, db.NotificationOccurrence{Key: "h", Priority: domain.NotificationHigh}).Notification
		urgent := raise(t, f, db.NotificationOccurrence{Key: "u", Priority: domain.NotificationUrgent}).Notification
		low := raise(t, f, db.NotificationOccurrence{Key: "l", Priority: domain.NotificationLow}).Notification
		for _, key := range []string{"h", "u", "l"} {
			if _, err := s.ResolveSystem(ctx, f.OrgID, domain.NotificationOriginSystem, "test.kind", key); err != nil {
				t.Fatalf("ResolveSystem(%s): %v", key, err)
			}
		}
		// An open row, which the default order puts first.
		open := raise(t, f, db.NotificationOccurrence{Key: "o", Priority: domain.NotificationUrgent}).Notification

		byResolution := func(dir string, status domain.NotificationStatus) db.NotificationListFilter {
			return db.NotificationListFilter{Status: status, SortKey: db.NotificationSortResolvedAt, SortDir: dir}
		}
		for name, c := range map[string]struct {
			f    db.NotificationListFilter
			want []string
		}{
			"default":       {db.NotificationListFilter{Status: domain.NotificationResolved}, []string{urgent.ID, high.ID, low.ID}},
			"desc":          {byResolution(db.NotificationSortDirDesc, domain.NotificationResolved), []string{low.ID, urgent.ID, high.ID}},
			"asc":           {byResolution(db.NotificationSortDirAsc, domain.NotificationResolved), []string{high.ID, urgent.ID, low.ID}},
			"desc, no dir":  {byResolution("", domain.NotificationResolved), []string{low.ID, urgent.ID, high.ID}},
			"desc and open": {byResolution(db.NotificationSortDirDesc, ""), []string{low.ID, urgent.ID, high.ID, open.ID}},
			"asc and open":  {byResolution(db.NotificationSortDirAsc, ""), []string{high.ID, urgent.ID, low.ID, open.ID}},
		} {
			if items, total := inbox(t, f, c.f); total != len(c.want) || !slices.Equal(ids(items), c.want) {
				t.Errorf("%s: %v (total %d); want %v", name, ids(items), total, c.want)
			}
		}

		f.As(t, f.Viewer, func(s db.NotificationStore) {
			// One row is the latest resolution, whatever its priority.
			items, total, err := s.ListForViewer(ctx, f.OrgID, f.Viewer, byResolution(db.NotificationSortDirDesc, domain.NotificationResolved), db.ListOpts{Limit: 1})
			if err != nil || total != 3 || !slices.Equal(ids(items), []string{low.ID}) {
				t.Errorf("latest resolution = %v (total %d), %v; want %s", ids(items), total, err, low.ID)
			}
			items, _, err = s.ListForOrg(ctx, f.OrgID, f.Viewer, byResolution(db.NotificationSortDirAsc, ""), db.ListOpts{Limit: 50})
			if want := []string{high.ID, urgent.ID, low.ID, open.ID}; err != nil || !slices.Equal(ids(items), want) {
				t.Errorf("ListForOrg asc = %v, %v; want %v", ids(items), err, want)
			}
			for name, bad := range map[string]db.NotificationListFilter{
				"unknown key":       {SortKey: "seen_count"},
				"unknown direction": {SortKey: db.NotificationSortResolvedAt, SortDir: "up"},
				"direction alone":   {SortDir: db.NotificationSortDirAsc},
			} {
				if _, _, err := s.ListForViewer(ctx, f.OrgID, f.Viewer, bad, db.ListOpts{Limit: 50}); !errors.Is(err, db.ErrInvalidNotificationFilter) {
					t.Errorf("%s: err = %v; want ErrInvalidNotificationFilter", name, err)
				}
			}
		})
	})

	// narrowWorld raises five rows the viewer is sent, one per audience shape,
	// in this order, so first_seen_at and escalated_at rise from a to e.
	type narrowWorld struct {
		a, b, c, d, e domain.Notification
	}
	raiseNarrowWorld := func(t *testing.T, f NotificationFixture) narrowWorld {
		t.Helper()
		var w narrowWorld
		w.a = raise(t, f, db.NotificationOccurrence{Key: "a", Priority: domain.NotificationUrgent,
			Title: "Disk 100% full", Body: "volume a"}).Notification
		w.b = raise(t, f, db.NotificationOccurrence{Key: "b", Priority: domain.NotificationHigh,
			Title: "beta sync", Body: "Lagging 50_percent", Audience: domain.NotificationAudience{
				Scope: domain.NotificationScopeTeam, Role: domain.NotificationRoleMembers, TeamID: f.Team}}).Notification
		w.c = raise(t, f, db.NotificationOccurrence{Key: "c", Priority: domain.NotificationLow,
			Title: "Alpha", Body: "for the team's admins", Audience: domain.NotificationAudience{
				Scope: domain.NotificationScopeTeam, Role: domain.NotificationRoleAdmins, TeamID: f.Team}}).Notification
		w.d = raise(t, f, db.NotificationOccurrence{Key: "d", Priority: domain.NotificationHigh,
			Title: "gamma", Body: "100 percent", Audience: domain.NotificationAudience{
				Scope: domain.NotificationScopeUser, UserID: f.Viewer}}).Notification
		w.e = raise(t, f, db.NotificationOccurrence{Key: "e", Priority: domain.NotificationLow,
			Title: "Delta", Audience: domain.NotificationAudience{
				Scope: domain.NotificationScopeOrg, Role: domain.NotificationRoleMembers}}).Notification
		rows := []domain.Notification{w.a, w.b, w.c, w.d, w.e}
		for i := 1; i < len(rows); i++ {
			if !rows[i].FirstSeenAt.After(rows[i-1].FirstSeenAt) || !rows[i].EscalatedAt.After(rows[i-1].EscalatedAt) {
				t.Fatalf("rows raised in order share a timestamp: %+v then %+v", rows[i-1], rows[i])
			}
		}
		return w
	}

	t.Run("narrowing", func(t *testing.T) {
		f := mk(t)
		w := raiseNarrowWorld(t, f)
		for name, c := range map[string]struct {
			f    db.NotificationListFilter
			want []string
		}{
			"title":        {db.NotificationListFilter{Search: "ALPHA"}, []string{w.c.ID}},
			"body":         {db.NotificationListFilter{Search: "volume"}, []string{w.a.ID}},
			"percent":      {db.NotificationListFilter{Search: "100%"}, []string{w.a.ID}},
			"underscore":   {db.NotificationListFilter{Search: "_percent"}, []string{w.b.ID}},
			"no match":     {db.NotificationListFilter{Search: "nothing like it"}, []string{}},
			"admins":       {db.NotificationListFilter{RoleIn: []domain.NotificationRole{domain.NotificationRoleAdmins}}, []string{w.a.ID, w.c.ID}},
			"members":      {db.NotificationListFilter{RoleIn: []domain.NotificationRole{domain.NotificationRoleMembers}}, []string{w.b.ID, w.e.ID}},
			"org admins":   {db.NotificationListFilter{ScopeIn: []domain.NotificationScope{domain.NotificationScopeOrg}, RoleIn: []domain.NotificationRole{domain.NotificationRoleAdmins}}, []string{w.a.ID}},
			"team":         {db.NotificationListFilter{TeamIDIn: []string{f.Team}}, []string{w.b.ID, w.c.ID}},
			"user":         {db.NotificationListFilter{ScopeIn: []domain.NotificationScope{domain.NotificationScopeUser}}, []string{w.d.ID}},
			"search, team": {db.NotificationListFilter{Search: "a", TeamIDIn: []string{f.Team}}, []string{w.b.ID, w.c.ID}},
		} {
			items, total := inbox(t, f, c.f)
			if total != len(c.want) || !slices.Equal(ids(items), c.want) {
				t.Errorf("%s: %v (total %d); want %v", name, ids(items), total, c.want)
			}
		}
		f.As(t, f.Viewer, func(s db.NotificationStore) {
			for name, bad := range map[string]db.NotificationListFilter{
				"role":     {RoleIn: []domain.NotificationRole{"owners"}},
				"sort key": {SortKey: "seen_count", SortDir: db.NotificationSortDirAsc},
				"sort dir": {SortKey: db.NotificationSortTitle, SortDir: "up"},
				"lone dir": {SortDir: db.NotificationSortDirAsc},
			} {
				if _, _, err := s.ListForViewer(ctx, f.OrgID, f.Viewer, bad, db.ListOpts{Limit: 50}); !errors.Is(err, db.ErrInvalidNotificationFilter) {
					t.Errorf("ListForViewer(bad %s) err = %v", name, err)
				}
				if _, err := s.FacetsForViewer(ctx, f.OrgID, f.Viewer, bad); !errors.Is(err, db.ErrInvalidNotificationFilter) {
					t.Errorf("FacetsForViewer(bad %s) err = %v", name, err)
				}
			}
			// The All-teams list takes the same filters.
			items, total, err := s.ListForOrg(ctx, f.OrgID, f.Viewer, db.NotificationListFilter{Search: "100%"}, db.ListOpts{Limit: 50})
			if err != nil || total != 1 || !slices.Equal(ids(items), []string{w.a.ID}) {
				t.Errorf("ListForOrg search = %v (total %d), %v; want the literal match", ids(items), total, err)
			}
		})
	})

	t.Run("order", func(t *testing.T) {
		f := mk(t)
		w := raiseNarrowWorld(t, f)
		// Seen again, b is the latest last_seen_at; its first_seen_at and
		// escalated_at stay.
		w.b = raise(t, f, db.NotificationOccurrence{Key: "b", Priority: domain.NotificationHigh,
			Title: "beta sync", Body: "Lagging 50_percent", Audience: domain.NotificationAudience{
				Scope: domain.NotificationScopeTeam, Role: domain.NotificationRoleMembers, TeamID: f.Team}}).Notification
		a, b, c, d, e := w.a.ID, w.b.ID, w.c.ID, w.d.ID, w.e.ID
		check := func(t *testing.T, name, key, dir string, want []string) {
			t.Helper()
			if items, _ := inbox(t, f, db.NotificationListFilter{SortKey: key, SortDir: dir}); !slices.Equal(ids(items), want) {
				t.Errorf("%s: %v; want %v", name, ids(items), want)
			}
		}
		const asc, desc = db.NotificationSortDirAsc, db.NotificationSortDirDesc
		// Default: priority, urgent first, then escalated_at newest first.
		check(t, "default", "", "", []string{a, d, b, e, c})
		// Each key's desc is its asc reversed, n.id included.
		check(t, "priority asc", db.NotificationSortPriority, asc, []string{a, d, b, e, c})
		check(t, "priority desc", db.NotificationSortPriority, desc, []string{c, e, b, d, a})
		// Case-folded: "Delta" sorts between "beta sync" and "Disk 100% full".
		check(t, "title asc", db.NotificationSortTitle, asc, []string{c, b, e, a, d})
		check(t, "title desc", db.NotificationSortTitle, desc, []string{d, a, e, b, c})
		check(t, "first_seen_at asc", db.NotificationSortFirstSeenAt, asc, []string{a, b, c, d, e})
		check(t, "first_seen_at desc", db.NotificationSortFirstSeenAt, desc, []string{e, d, c, b, a})
		check(t, "last_seen_at asc", db.NotificationSortLastSeenAt, asc, []string{a, c, d, e, b})
		check(t, "last_seen_at desc", db.NotificationSortLastSeenAt, desc, []string{b, e, d, c, a})
		// No direction is the key's own: urgent first, A to Z, the latest
		// time first.
		check(t, "priority, no dir", db.NotificationSortPriority, "", []string{a, d, b, e, c})
		check(t, "title, no dir", db.NotificationSortTitle, "", []string{c, b, e, a, d})
		check(t, "first_seen_at, no dir", db.NotificationSortFirstSeenAt, "", []string{e, d, c, b, a})
		check(t, "last_seen_at, no dir", db.NotificationSortLastSeenAt, "", []string{b, e, d, c, a})

		for _, key := range []string{"c", "a"} {
			if _, err := f.Stores.Notifications.ResolveSystem(ctx, f.OrgID, domain.NotificationOriginSystem, "test.kind", key); err != nil {
				t.Fatalf("ResolveSystem %s: %v", key, err)
			}
		}
		// The open rows have no resolved_at and trail every resolved row in
		// both directions, ordered among themselves by id.
		open := []string{b, d, e}
		slices.Sort(open)
		check(t, "resolved_at asc", db.NotificationSortResolvedAt, asc, append([]string{c, a}, open...))
		slices.Reverse(open)
		check(t, "resolved_at desc", db.NotificationSortResolvedAt, desc, append([]string{a, c}, open...))
		f.As(t, f.Viewer, func(s db.NotificationStore) {
			// Paging walks the sorted order whole.
			var walked []string
			for offset := 0; offset < 5; offset += 2 {
				page, total, err := s.ListForOrg(ctx, f.OrgID, f.Viewer, db.NotificationListFilter{SortKey: db.NotificationSortTitle, SortDir: asc},
					db.ListOpts{Limit: 2, Offset: offset})
				if err != nil || total != 5 {
					t.Fatalf("ListForOrg page at %d: total %d, %v", offset, total, err)
				}
				walked = append(walked, ids(page)...)
			}
			if !slices.Equal(walked, []string{c, b, e, a, d}) {
				t.Errorf("paged title order = %v", walked)
			}
		})
	})

	t.Run("facets", func(t *testing.T) {
		f := mk(t)
		raiseNarrowWorld(t, f)
		// Read, but not sent: on the All-teams cuts only.
		raise(t, f, db.NotificationOccurrence{Key: "archived", Audience: domain.NotificationAudience{
			Scope: domain.NotificationScopeTeam, Role: domain.NotificationRoleAdmins, TeamID: f.ArchivedTeam}})
		team, archived := "team:"+f.Team, "team:"+f.ArchivedTeam
		facets := func(t *testing.T, org bool, filter db.NotificationListFilter) db.NotificationFacets {
			t.Helper()
			var out db.NotificationFacets
			f.As(t, f.Viewer, func(s db.NotificationStore) {
				var err error
				if org {
					out, err = s.FacetsForOrg(ctx, f.OrgID, f.Viewer, filter)
				} else {
					out, err = s.FacetsForViewer(ctx, f.OrgID, f.Viewer, filter)
				}
				if err != nil {
					t.Fatalf("facets(%+v): %v", filter, err)
				}
			})
			return out
		}
		check := func(t *testing.T, name string, got, want []db.Facet) {
			t.Helper()
			if !slices.Equal(got, want) {
				t.Errorf("%s = %+v; want %+v", name, got, want)
			}
		}
		fc := func(value string, count int) db.Facet { return db.Facet{Value: value, Count: count} }
		allPriorities := []db.Facet{fc("urgent", 1), fc("high", 2), fc("low", 2)}
		allAudiences := []db.Facet{fc("org:admins", 1), fc("org:members", 1), fc(team, 2), fc("user", 1)}

		got := facets(t, false, db.NotificationListFilter{})
		check(t, "inbox by_priority", got.ByPriority, allPriorities)
		check(t, "inbox by_audience", got.ByAudience, allAudiences)

		// Each cut ignores its own filters and honours the others.
		got = facets(t, false, db.NotificationListFilter{PriorityIn: []domain.NotificationPriority{domain.NotificationHigh}})
		check(t, "priority-filtered by_priority", got.ByPriority, allPriorities)
		check(t, "priority-filtered by_audience", got.ByAudience, []db.Facet{fc(team, 1), fc("user", 1)})
		got = facets(t, false, db.NotificationListFilter{TeamIDIn: []string{f.Team}, RoleIn: []domain.NotificationRole{domain.NotificationRoleAdmins}})
		check(t, "audience-filtered by_priority", got.ByPriority, []db.Facet{fc("low", 1)})
		check(t, "audience-filtered by_audience", got.ByAudience, allAudiences)
		got = facets(t, false, db.NotificationListFilter{Search: "100%"})
		check(t, "searched by_priority", got.ByPriority, []db.Facet{fc("urgent", 1)})
		check(t, "searched by_audience", got.ByAudience, []db.Facet{fc("org:admins", 1)})
		// A value's count is the total its filters list.
		if _, total := inbox(t, f, db.NotificationListFilter{TeamIDIn: []string{f.Team}}); total != 2 {
			t.Errorf("team_id_in lists %d rows; its facet counts 2", total)
		}

		got = facets(t, false, db.NotificationListFilter{Status: domain.NotificationResolved})
		if got.ByPriority == nil || got.ByAudience == nil || len(got.ByPriority)+len(got.ByAudience) != 0 {
			t.Errorf("nothing resolved: facets = %#v; want empty cuts, not nil", got)
		}

		got = facets(t, true, db.NotificationListFilter{})
		check(t, "org by_priority", got.ByPriority, []db.Facet{fc("urgent", 1), fc("high", 3), fc("low", 2)})
		wantOrg := append([]db.Facet{}, allAudiences...)
		wantOrg = append(wantOrg, fc(archived, 1))
		slices.SortFunc(wantOrg, func(x, y db.Facet) int { return strings.Compare(x.Value, y.Value) })
		check(t, "org by_audience", got.ByAudience, wantOrg)
	})

	t.Run("marks", func(t *testing.T) {
		f := mk(t)
		row := raise(t, f, db.NotificationOccurrence{Key: "d", Dismissible: true}).Notification
		fixed := raise(t, f, db.NotificationOccurrence{Key: "f"}).Notification
		other := raise(t, f, db.NotificationOccurrence{Key: "o", Audience: domain.NotificationAudience{
			Scope: domain.NotificationScopeUser, UserID: f.Other}}).Notification

		readMarks := func() (*domain.NotificationMarks, error) {
			if v := get(t, f, row.ID); v != nil {
				return v.Marks, nil
			}
			return nil, nil
		}
		m, err := setMarks(t, f, row.ID, db.NotificationMarksPatch{Dismissed: &yes})
		if err != nil || m.DismissedAt == nil || m.ReadAt != nil {
			t.Fatalf("SetMarks dismiss = %+v, %v", m, err)
		}
		AssertWriteReturnedStoredRow(t, "SetMarks insert", m, readMarks)
		m, err = setMarks(t, f, row.ID, db.NotificationMarksPatch{Read: &yes, Dismissed: &no})
		if err != nil || m.ReadAt == nil || m.DismissedAt != nil {
			t.Fatalf("SetMarks read+undismiss = %+v, %v", m, err)
		}
		AssertWriteReturnedStoredRow(t, "SetMarks update", m, readMarks)

		if _, err := setMarks(t, f, fixed.ID, db.NotificationMarksPatch{Dismissed: &yes}); !errors.Is(err, db.ErrNotificationNotDismissible) {
			t.Errorf("dismiss non-dismissible: err = %v", err)
		}
		if _, err := setMarks(t, f, other.ID, db.NotificationMarksPatch{Read: &yes}); !errors.Is(err, db.ErrNoSuchNotification) {
			t.Errorf("mark another user's row: err = %v", err)
		}
		if _, err := setMarks(t, f, "not-a-uuid", db.NotificationMarksPatch{Read: &yes}); !errors.Is(err, db.ErrNoSuchNotification) {
			t.Errorf("mark a malformed id: err = %v", err)
		}
		if get(t, f, "not-a-uuid") != nil {
			t.Errorf("GetForViewer(malformed id) found a row")
		}
		f.As(t, f.Viewer, func(s db.NotificationStore) {
			got, err := s.MarkRead(ctx, f.OrgID, f.Viewer, []string{fixed.ID, other.ID, "not-a-uuid"})
			if err != nil || len(got) != 1 || got[0].NotificationID != fixed.ID || got[0].ReadAt == nil {
				t.Errorf("MarkRead = %+v, %v; want only the readable row", got, err)
			}
		})
	})

	t.Run("dismiss", func(t *testing.T) {
		f := mk(t)
		a := raise(t, f, db.NotificationOccurrence{Key: "a", Dismissible: true}).Notification
		b := raise(t, f, db.NotificationOccurrence{Key: "b", Dismissible: true}).Notification
		c := raise(t, f, db.NotificationOccurrence{Key: "c", Dismissible: true}).Notification
		fixed := raise(t, f, db.NotificationOccurrence{Key: "f"}).Notification
		other := raise(t, f, db.NotificationOccurrence{Key: "o", Dismissible: true, Audience: domain.NotificationAudience{
			Scope: domain.NotificationScopeUser, UserID: f.Other}}).Notification
		if _, err := setMarks(t, f, b.ID, db.NotificationMarksPatch{Read: &yes}); err != nil {
			t.Fatal(err)
		}
		dismiss := func(ids ...string) ([]domain.NotificationMarks, error) {
			var got []domain.NotificationMarks
			var err error
			f.As(t, f.Viewer, func(s db.NotificationStore) { got, err = s.Dismiss(ctx, f.OrgID, f.Viewer, ids) })
			return got, err
		}

		got, err := dismiss(a.ID, b.ID, other.ID, "not-a-uuid", a.ID)
		if err != nil {
			t.Fatalf("Dismiss: %v", err)
		}
		byID := map[string]domain.NotificationMarks{}
		for _, m := range got {
			byID[m.NotificationID] = m
		}
		if len(got) != 2 || len(byID) != 2 || byID[a.ID].DismissedAt == nil || byID[b.ID].DismissedAt == nil {
			t.Fatalf("Dismiss = %+v; want a and b dismissed, and the unreadable ids skipped", got)
		}
		for _, id := range []string{a.ID, b.ID} {
			AssertWriteReturnedStoredRow(t, "Dismiss "+id, byID[id], func() (*domain.NotificationMarks, error) {
				return get(t, f, id).Marks, nil
			})
		}
		if v := get(t, f, b.ID); !v.Dismissed || !v.Read {
			t.Errorf("b after Dismiss: read=%v dismissed=%v; want its read mark kept", v.Read, v.Dismissed)
		}

		// A non-dismissible row refuses the batch, naming it, and the
		// dismissible row beside it is left as it was.
		_, err = dismiss(c.ID, fixed.ID, other.ID)
		var refused *db.NotificationsNotDismissibleError
		if !errors.As(err, &refused) || !errors.Is(err, db.ErrNotificationNotDismissible) || !slices.Equal(refused.IDs, []string{fixed.ID}) {
			t.Fatalf("Dismiss with a non-dismissible row: err = %v; want a refusal naming %s", err, fixed.ID)
		}
		for _, id := range []string{c.ID, fixed.ID} {
			if v := get(t, f, id); v.Dismissed || v.Marks != nil {
				t.Errorf("%s after a refused Dismiss: marks %+v; want none written", id, v.Marks)
			}
		}

		if got, err := dismiss(other.ID, "not-a-uuid"); err != nil || len(got) != 0 {
			t.Errorf("Dismiss of nothing readable = %+v, %v; want nothing written", got, err)
		}
	})

	t.Run("recipients", func(t *testing.T) {
		f := mk(t)
		recipients := func(a domain.NotificationAudience) []string {
			t.Helper()
			got, err := f.Stores.Notifications.RecipientsSystem(ctx, f.OrgID, a)
			if err != nil {
				t.Fatalf("RecipientsSystem(%+v): %v", a, err)
			}
			return got
		}
		for _, c := range []struct {
			name string
			a    domain.NotificationAudience
			want []string
		}{
			{"org admins", domain.NotificationAudience{Scope: domain.NotificationScopeOrg, Role: domain.NotificationRoleAdmins}, []string{f.Viewer}},
			{"team admins", domain.NotificationAudience{Scope: domain.NotificationScopeTeam, Role: domain.NotificationRoleAdmins, TeamID: f.Team}, []string{f.Viewer}},
			{"team members", domain.NotificationAudience{Scope: domain.NotificationScopeTeam, Role: domain.NotificationRoleMembers, TeamID: f.Team}, []string{f.Viewer}},
			{"archived team", domain.NotificationAudience{Scope: domain.NotificationScopeTeam, Role: domain.NotificationRoleAdmins, TeamID: f.ArchivedTeam}, []string{}},
			{"user", domain.NotificationAudience{Scope: domain.NotificationScopeUser, UserID: f.Other}, []string{f.Other}},
		} {
			if got := recipients(c.a); !slices.Equal(got, c.want) {
				t.Errorf("%s: RecipientsSystem = %#v, want %#v", c.name, got, c.want)
			}
		}
		// N=1 has no other member to list; the viewer is in every org's.
		members := recipients(domain.NotificationAudience{Scope: domain.NotificationScopeOrg, Role: domain.NotificationRoleMembers})
		if !slices.Contains(members, f.Viewer) || !slices.IsSorted(members) {
			t.Errorf("org members: RecipientsSystem = %v, want a sorted list holding %s", members, f.Viewer)
		}
		if _, err := f.Stores.Notifications.RecipientsSystem(ctx, f.OrgID, domain.NotificationAudience{Scope: domain.NotificationScopeTeam}); !errors.Is(err, db.ErrInvalidNotification) {
			t.Errorf("RecipientsSystem(malformed audience) = %v, want ErrInvalidNotification", err)
		}
	})

	t.Run("system_tx", func(t *testing.T) {
		f := mk(t)
		var id string
		boom := errors.New("boom")
		err := f.Stores.Tx.SystemWithTx(ctx, f.OrgID, func(tx db.SystemTxStores) error {
			o := db.NotificationOccurrence{
				OrgID: f.OrgID, Origin: domain.NotificationOriginSystem, Lifecycle: domain.NotificationCondition,
				Kind: "test.kind", Key: "tx", Priority: domain.NotificationHigh, Title: "t",
				Audience: domain.NotificationAudience{Scope: domain.NotificationScopeOrg, Role: domain.NotificationRoleMembers},
			}
			if _, err := tx.Notifications.UpsertOpenSystem(ctx, o); err != nil {
				return err
			}
			u, err := tx.Notifications.UpsertOpenSystem(ctx, o)
			if err != nil {
				return err
			}
			if u.Opened || u.Escalated {
				t.Errorf("second raise in one transaction = %+v; want a plain bump", u)
			}
			id = u.Notification.ID
			return boom
		})
		if !errors.Is(err, boom) || get(t, f, id) != nil {
			t.Errorf("SystemWithTx = %v; the upsert should roll back", err)
		}
	})
}
