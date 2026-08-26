package sqlite_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The clone protocol has three doors onto its default and they are reached at
// different moments — a read with no row at all, a row provisioned by a writer
// that names only the foreign key, and an explicit choice already stored. The
// first two have to agree, and the third has to be immune to both: changing a
// default is a statement about installs that do not exist yet, never a
// migration of one that does.
func TestOrgSettings_CloneProtocolDefault_SQLite(t *testing.T) {
	storedProtocol := func(t *testing.T, conn *sql.DB) string {
		t.Helper()
		var proto string
		if err := conn.QueryRow(
			`SELECT github_clone_protocol FROM org_settings WHERE org_id = ?`, runmode.LocalDefaultOrgID,
		).Scan(&proto); err != nil {
			t.Fatalf("read stored clone protocol: %v", err)
		}
		return proto
	}

	t.Run("no row resolves to the package default", func(t *testing.T) {
		conn := openSQLiteForTest(t)
		if _, err := conn.Exec(`DELETE FROM org_settings`); err != nil {
			t.Fatalf("drop seeded org_settings: %v", err)
		}
		got, err := sqlite.New(conn).Orgs.GetSettingsSystem(t.Context(), runmode.LocalDefaultOrgID)
		if err != nil {
			t.Fatalf("GetSettingsSystem: %v", err)
		}
		if got.GitHubCloneProtocol != "https" {
			t.Errorf("read with no row = %q; want %q", got.GitHubCloneProtocol, "https")
		}
	})

	t.Run("a row that names only the org takes the column default", func(t *testing.T) {
		conn := openSQLiteForTest(t)
		if _, err := conn.Exec(`DELETE FROM org_settings`); err != nil {
			t.Fatalf("drop seeded org_settings: %v", err)
		}
		// The shape SetGitHubCredentialClass provisions with: naming only the
		// foreign key is what leaves every other column to its DEFAULT, so this
		// is the row a fresh install is born with when a credential is bound
		// before any settings are saved.
		if _, err := conn.Exec(
			`INSERT INTO org_settings (org_id) VALUES (?)`, runmode.LocalDefaultOrgID,
		); err != nil {
			t.Fatalf("provision org settings: %v", err)
		}
		if got := storedProtocol(t, conn); got != "https" {
			t.Errorf("a freshly provisioned org stores %q; want %q", got, "https")
		}
	})

	t.Run("a stored choice survives an unrelated save", func(t *testing.T) {
		conn := openSQLiteForTest(t)
		orgs := sqlite.New(conn).Orgs

		set, err := orgs.GetSettingsSystem(t.Context(), runmode.LocalDefaultOrgID)
		if err != nil {
			t.Fatalf("GetSettingsSystem: %v", err)
		}
		set.GitHubCloneProtocol = "ssh"
		if _, err := orgs.UpdateSettings(t.Context(), runmode.LocalDefaultOrgID, set); err != nil {
			t.Fatalf("store the operator's choice: %v", err)
		}

		// Every real caller read-modify-writes, so the field arrives populated
		// and the default never gets a say. Nothing else rewrites the column —
		// there is no migration — which is what makes an existing install's
		// choice durable across an upgrade that moves the default.
		saved, err := orgs.GetSettingsSystem(t.Context(), runmode.LocalDefaultOrgID)
		if err != nil {
			t.Fatalf("GetSettingsSystem: %v", err)
		}
		saved.JiraPollInterval = 7 * time.Minute
		if _, err := orgs.UpdateSettings(t.Context(), runmode.LocalDefaultOrgID, saved); err != nil {
			t.Fatalf("unrelated save: %v", err)
		}
		if got := storedProtocol(t, conn); got != "ssh" {
			t.Errorf("after an unrelated save the column holds %q; want the operator's %q", got, "ssh")
		}
	})

	t.Run("the package default and the column default agree", func(t *testing.T) {
		// They are two independent literals in two languages, reached on
		// different paths, and a reader who finds one has no reason to look for
		// the other.
		if got := domain.DefaultOrgSettings().GitHubCloneProtocol; got != "https" {
			t.Errorf("DefaultOrgSettings = %q; want the column DEFAULT %q", got, "https")
		}
	})
}
