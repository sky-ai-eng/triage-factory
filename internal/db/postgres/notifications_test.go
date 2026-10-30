package postgres_test

import (
	"database/sql"
	"errors"
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// seedNotificationFixture seeds the conformance world; claims selects whether
// the viewer's reads and marks run under their claims on the app pool.
func seedNotificationFixture(t *testing.T, claims bool) dbtest.NotificationFixture {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, viewer, team := pgtest.SeedOrgWithUser(t, h, "notify")
	archived := pgtest.SeedTeam(t, h, orgID, "archived")
	pgtest.MustExec(t, h.AdminDB, `INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'admin')`, viewer, archived)
	pgtest.MustExec(t, h.AdminDB, `UPDATE teams SET deleted_at = now() WHERE id = $1`, archived)
	other := pgtest.SeedUser(t, h, "other")
	pgtest.MustExec(t, h.AdminDB, `INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`, other, orgID)

	f := dbtest.NotificationFixture{OrgID: orgID, Viewer: viewer, Other: other, Team: team, ArchivedTeam: archived}
	if !claims {
		f.Stores = pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
		f.As = func(t *testing.T, _ string, fn func(db.NotificationStore)) { fn(f.Stores.Notifications) }
		return f
	}
	f.Stores = pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	f.As = func(t *testing.T, userID string, fn func(db.NotificationStore)) {
		t.Helper()
		if err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
			fn(pgstore.NewForTx(tx, pgtest.SecretKey).Notifications)
			return nil
		}); err != nil {
			t.Fatalf("WithUser: %v", err)
		}
	}
	return f
}

func TestNotificationStore_Postgres_Conformance(t *testing.T) {
	dbtest.RunNotificationStoreConformance(t, func(t *testing.T) dbtest.NotificationFixture {
		return seedNotificationFixture(t, false)
	})
}

// The admin-pool run bypasses RLS; this one runs the reads and marks writes
// under the viewer's claims.
func TestNotificationStore_Postgres_ClaimsConformance(t *testing.T) {
	dbtest.RunNotificationStoreConformance(t, func(t *testing.T) dbtest.NotificationFixture {
		return seedNotificationFixture(t, true)
	})
}

func TestNotificationStore_Postgres_RLS(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := t.Context()
	orgID, admin, _ := pgtest.SeedOrgWithUser(t, h, "rls")
	team := pgtest.SeedTeam(t, h, orgID, "t")
	member := pgtest.SeedUser(t, h, "member")
	pgtest.AddOrgMember(t, h, member, orgID, team, "member", "member")
	outsider := pgtest.SeedUser(t, h, "outsider")
	pgtest.MustExec(t, h.AdminDB, `INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`, outsider, orgID)

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	raise := func(key string, a domain.NotificationAudience) string {
		u, err := stores.Notifications.UpsertOpenSystem(ctx, db.NotificationOccurrence{
			OrgID: orgID, Origin: domain.NotificationOriginSystem, Lifecycle: domain.NotificationCondition,
			Kind: "rls", Key: key, Audience: a, Priority: domain.NotificationHigh, Title: key,
		})
		if err != nil {
			t.Fatal(err)
		}
		return u.Notification.ID
	}
	orgMembers := raise("om", domain.NotificationAudience{Scope: domain.NotificationScopeOrg, Role: domain.NotificationRoleMembers})
	orgAdmins := raise("oa", domain.NotificationAudience{Scope: domain.NotificationScopeOrg, Role: domain.NotificationRoleAdmins})
	teamMembers := raise("tm", domain.NotificationAudience{Scope: domain.NotificationScopeTeam, Role: domain.NotificationRoleMembers, TeamID: team})
	teamAdmins := raise("ta", domain.NotificationAudience{Scope: domain.NotificationScopeTeam, Role: domain.NotificationRoleAdmins, TeamID: team})
	memberOnly := raise("me", domain.NotificationAudience{Scope: domain.NotificationScopeUser, UserID: member})

	idsOf := func(items []domain.NotificationView) []string {
		out := make([]string, len(items))
		for i, v := range items {
			out[i] = v.ID
		}
		slices.Sort(out)
		return out
	}
	sorted := func(ids ...string) []string { slices.Sort(ids); return ids }

	for _, c := range []struct {
		name       string
		user       string
		read, sent []string
	}{
		// An org admin reads every team row but is sent none without the role.
		{"org admin", admin, sorted(orgMembers, orgAdmins, teamMembers, teamAdmins), sorted(orgMembers, orgAdmins)},
		{"team member", member, sorted(orgMembers, teamMembers, memberOnly), sorted(orgMembers, teamMembers, memberOnly)},
		{"org member", outsider, sorted(orgMembers), sorted(orgMembers)},
	} {
		if err := h.WithUser(t, c.user, orgID, func(tx *sql.Tx) error {
			s := pgstore.NewForTx(tx, pgtest.SecretKey).Notifications
			read, _, err := s.ListForOrg(ctx, orgID, c.user, db.NotificationOrgListFilter{}, db.ListOpts{Limit: 50})
			if err != nil {
				return err
			}
			sent, _, err := s.ListForViewer(ctx, orgID, c.user, db.NotificationListFilter{}, db.ListOpts{Limit: 50})
			if err != nil {
				return err
			}
			if got := idsOf(read); !slices.Equal(got, c.read) {
				t.Errorf("%s may read %v; want %v", c.name, got, c.read)
			}
			if got := idsOf(sent); !slices.Equal(got, c.sent) {
				t.Errorf("%s is sent %v; want %v", c.name, got, c.sent)
			}
			return nil
		}); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
	}

	yes := true
	if err := h.WithUser(t, member, orgID, func(tx *sql.Tx) error {
		_, err := pgstore.NewForTx(tx, pgtest.SecretKey).Notifications.SetMarks(ctx, orgID, member, teamAdmins, db.NotificationMarksPatch{Read: &yes})
		return err
	}); !errors.Is(err, db.ErrNoSuchNotification) {
		t.Errorf("marking an unreadable row: err = %v; want ErrNoSuchNotification", err)
	}
	pgtest.AssertRLSViolation(t, h.WithUser(t, member, orgID, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO notification_marks (notification_id, user_id, org_id) VALUES ($1, $2, $3)`, orgMembers, outsider, orgID)
		return err
	}))
	pgtest.AssertRLSViolation(t, h.WithUser(t, admin, orgID, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE notifications SET title = 'x' WHERE id = $1`, orgAdmins)
		return err
	}))
}
