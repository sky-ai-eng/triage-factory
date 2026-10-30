package sqlite_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

func TestNotificationStore_SQLite_Conformance(t *testing.T) {
	dbtest.RunNotificationStoreConformance(t, func(t *testing.T) dbtest.NotificationFixture {
		conn := openSQLiteForTest(t)
		other, archived := uuid.New().String(), uuid.New().String()
		if _, err := conn.Exec(`INSERT INTO users (id, display_name) VALUES (?, 'other')`, other); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(`INSERT INTO teams (id, org_id, slug, name, deleted_at) VALUES (?, ?, 'archived', 'Archived', ?)`,
			archived, runmode.LocalDefaultOrgID, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		stores := sqlitestore.New(conn)
		return dbtest.NotificationFixture{
			Stores: stores, OrgID: runmode.LocalDefaultOrgID,
			Viewer: runmode.LocalDefaultUserID, Other: other,
			Team: runmode.LocalDefaultTeamID, ArchivedTeam: archived,
			As: func(t *testing.T, _ string, fn func(db.NotificationStore)) { fn(stores.Notifications) },
		}
	})
}
