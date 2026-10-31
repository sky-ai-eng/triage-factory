package dbtest

import (
	"errors"
	"slices"
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
		one, err := s.ResolveIDSystem(ctx, f.OrgID, again.Notification.ID)
		if err != nil || one == nil || one.ID != again.Notification.ID || one.ResolvedAt == nil ||
			one.ResolvedReason != domain.NotificationResolvedCleared {
			t.Fatalf("ResolveIDSystem = %+v, %v; want the org admins row cleared", one, err)
		}
		AssertWriteReturnedStoredRow(t, "ResolveIDSystem", *one, stored(t, f, one.ID))
		if v := get(t, f, sibling.ID); v == nil || v.ResolvedAt != nil {
			t.Errorf("sibling audience after ResolveIDSystem = %+v; want it open", v)
		}
		for _, id := range []string{again.Notification.ID, "not-a-uuid", "00000000-0000-4000-8000-000000000000"} {
			if got, err := s.ResolveIDSystem(ctx, f.OrgID, id); err != nil || got != nil {
				t.Errorf("ResolveIDSystem(%q) = %+v, %v; want nil for a row that is not open", id, got, err)
			}
		}
		expires := time.Now().Add(time.Hour)
		n := raise(t, f, db.NotificationOccurrence{Key: "n", Lifecycle: domain.NotificationNotice, Dismissible: true, ExpiresAt: &expires}).Notification
		if got, err := s.ResolveIDSystem(ctx, f.OrgID, n.ID); err != nil || got != nil {
			t.Errorf("ResolveIDSystem on a notice = %+v, %v; want nil: a notice expires", got, err)
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
		raise(t, f, db.NotificationOccurrence{Kind: "test.event", Key: "e", Origin: domain.NotificationOriginEvent})
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
			items, total, err := s.ListForOrg(ctx, f.OrgID, f.Viewer, db.NotificationOrgListFilter{}, db.ListOpts{Limit: 2})
			if err != nil || total != 4 || len(items) != 2 {
				t.Errorf("ListForOrg page = %d items, total %d, %v; want 2 of 4", len(items), total, err)
			}
			items, total, err = s.ListForOrg(ctx, f.OrgID, f.Viewer, db.NotificationOrgListFilter{TeamIDIn: []string{f.ArchivedTeam}}, db.ListOpts{Limit: 50})
			if err != nil || total != 1 || items[0].ID != archived.ID {
				t.Errorf("ListForOrg team_id_in = %v, %v; want the archived team's row", ids(items), err)
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
