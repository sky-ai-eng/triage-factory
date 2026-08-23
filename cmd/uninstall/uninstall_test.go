package uninstall

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

func TestGitHubAppKeychainKeys_EnumeratesConfiguredApps(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "triagefactory.db")
	conn, err := db.OpenAt(dbPath)
	if err != nil {
		t.Fatalf("OpenAt: %v", err)
	}
	// A minimal stand-in for org_github_apps — the enumeration only reads
	// app_id, so the FK columns the real schema carries are irrelevant here.
	if _, err := conn.Exec(`CREATE TABLE org_github_apps (app_id TEXT NOT NULL)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := conn.Exec(`INSERT INTO org_github_apps (app_id) VALUES ('123'), ('456')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	keys, err := gitHubAppKeychainKeys(dbPath)
	if err != nil {
		t.Fatalf("gitHubAppKeychainKeys: %v", err)
	}
	want := []string{
		"github_app_123_pem", "github_app_123_client_secret", "github_app_123_webhook_secret",
		"github_app_456_pem", "github_app_456_client_secret", "github_app_456_webhook_secret",
	}
	// SELECT without ORDER BY makes row order unspecified — compare as sets.
	slices.Sort(keys)
	slices.Sort(want)
	if !slices.Equal(keys, want) {
		t.Errorf("keys = %v, want (any order) %v", keys, want)
	}
}

func TestGitHubAppKeychainKeys_NoDBReturnsNil(t *testing.T) {
	keys, err := gitHubAppKeychainKeys(filepath.Join(t.TempDir(), "absent.db"))
	if err != nil {
		t.Fatalf("err = %v, want nil for an absent DB", err)
	}
	if keys != nil {
		t.Fatalf("keys = %v, want nil for an absent DB", keys)
	}
}

func TestGitHubAppKeychainKeys_EmptyTableReturnsNil(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "triagefactory.db")
	conn, err := db.OpenAt(dbPath)
	if err != nil {
		t.Fatalf("OpenAt: %v", err)
	}
	if _, err := conn.Exec(`CREATE TABLE org_github_apps (app_id TEXT NOT NULL)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	keys, err := gitHubAppKeychainKeys(dbPath)
	if err != nil {
		t.Fatalf("gitHubAppKeychainKeys: %v", err)
	}
	if keys != nil {
		t.Fatalf("keys = %v, want nil when no App is configured", keys)
	}
}
