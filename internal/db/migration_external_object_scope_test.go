package db

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// externalObjectScopeVersion is the migration that gives artifacts and the
// audit ledger a scope and placement pins a host.
const externalObjectScopeVersion = 202610100004

// The migration computes every existing row's scope in SQL from the org's
// stored settings, and every later write and keyed read computes it through
// domain.ExternalObjectScope (a pin's host through EffectiveGitHubHost). If the
// two disagreed, every pre-existing artifact would sit under a scope no read
// asks for: the next capture of the same object would mint a second row beside
// it, a repository rename would skip it, and a pin would never be read. So
// each case stages one row per provider under one org configuration, migrates,
// and requires each stored scope to equal what the Go side answers.
func TestMigrate_ExternalObjectScopeBackfillMatchesGo(t *testing.T) {
	cases := []struct {
		name       string
		githubBase *string // nil = no override recorded
		jiraBase   *string // nil = Jira not configured
		workspace  *string // nil = no Linear workspace bound
	}{
		{name: "defaults"},
		{name: "empty settings", githubBase: ptr(""), jiraBase: ptr(""), workspace: ptr("")},
		{name: "enterprise hosts", githubBase: ptr("https://ghe.example.com"), jiraBase: ptr("https://acme.atlassian.net"), workspace: ptr("ws-1")},
		{name: "trailing slashes", githubBase: ptr("https://ghe.example.com//"), jiraBase: ptr("https://jira.example.com/")},
		{name: "context paths", githubBase: ptr("https://example.com/github/"), jiraBase: ptr("https://jira.example.com/jira/")},
		{name: "surrounding whitespace", githubBase: ptr("  https://ghe.example.com/ "), jiraBase: ptr("\thttps://jira.example.com\n")},
		// The scheme and authority compare case-insensitively and are stored
		// lowercased; a context path keeps its case.
		{name: "mixed case", githubBase: ptr("HTTPS://GHE.Example.com/GitHub/"), jiraBase: ptr("Https://Jira.Example.com/Jira")},
		{name: "schemeless jira", jiraBase: ptr("jira.example.com")},
		{name: "jira with no authority", jiraBase: ptr("https:///jira")},
		{name: "jira with another scheme", jiraBase: ptr("ftp://jira.example.com")},
	}
	providers := []string{
		domain.ArtifactProviderGitHub, domain.ArtifactProviderGit, domain.ArtifactProviderJira,
		domain.ArtifactProviderLinear, domain.ArtifactProviderSlack, domain.ArtifactProviderNetwork,
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			database := openMigrationsTestDBAt(t, TestDSNMemory, 202610100001)
			seedExternalObjectScopeOrg(t, database)
			settings := domain.OrgSettings{}
			for kind, base := range map[string]*string{"github": tc.githubBase, "jira": tc.jiraBase} {
				if base == nil {
					continue
				}
				if _, err := database.Exec(`
					INSERT INTO org_event_sources (org_id, kind, base_url) VALUES (?, ?, ?)
					ON CONFLICT (org_id, kind) DO UPDATE SET base_url = excluded.base_url
				`, runmode.LocalDefaultOrgID, kind, *base); err != nil {
					t.Fatalf("seed %s base url: %v", kind, err)
				}
			}
			if tc.githubBase != nil {
				settings.GitHubBaseURL = *tc.githubBase
			}
			if tc.jiraBase != nil {
				settings.JiraBaseURL = *tc.jiraBase
			}
			if tc.workspace != nil {
				settings.LinearWorkspaceID = *tc.workspace
				if _, err := database.Exec(`
					INSERT INTO org_settings (org_id, linear_workspace_id) VALUES (?, ?)
					ON CONFLICT (org_id) DO UPDATE SET linear_workspace_id = excluded.linear_workspace_id
				`, runmode.LocalDefaultOrgID, *tc.workspace); err != nil {
					t.Fatalf("seed linear workspace: %v", err)
				}
			}
			for _, p := range providers {
				if _, err := database.Exec(`
					INSERT INTO artifacts (id, team_id, provider, kind, target, state, dedup_key)
					VALUES (?, 'team-1', ?, 'comment', 't', 'posted', ?)
				`, "a-"+p, p, p+":comment:k"); err != nil {
					t.Fatalf("seed %s artifact: %v", p, err)
				}
				if _, err := database.Exec(`
					INSERT INTO external_actions (id, provider, action, target, credential, dedup_key)
					VALUES (?, ?, 'comment_posted', 't', 'none', ?)
				`, "x-"+p, p, p+":k"); err != nil {
					t.Fatalf("seed %s action: %v", p, err)
				}
			}
			if _, err := database.Exec(`
				INSERT INTO placement_overrides (key_kind, key_value, replicas) VALUES ('repo', 'octo/api', 2)
			`); err != nil {
				t.Fatalf("seed placement override: %v", err)
			}

			migrateExternalObjectScope(t, database)
			settings.GitHubBaseURL = migratedGitHubBase(settings.GitHubBaseURL)

			for _, p := range providers {
				want := domain.ExternalObjectScope(p, settings)
				var artifactScope, actionScope string
				if err := database.QueryRow(`SELECT scope FROM artifacts WHERE id = ?`, "a-"+p).Scan(&artifactScope); err != nil {
					t.Fatalf("read %s artifact scope: %v", p, err)
				}
				if artifactScope != want {
					t.Errorf("%s artifact scope = %q, domain.ExternalObjectScope = %q", p, artifactScope, want)
				}
				if err := database.QueryRow(`SELECT scope FROM external_actions WHERE id = ?`, "x-"+p).Scan(&actionScope); err != nil {
					t.Fatalf("read %s action scope: %v", p, err)
				}
				if actionScope != want {
					t.Errorf("%s action scope = %q, domain.ExternalObjectScope = %q", p, actionScope, want)
				}
			}
			var pinHost string
			if err := database.QueryRow(`SELECT host FROM placement_overrides WHERE key_value = 'octo/api'`).Scan(&pinHost); err != nil {
				t.Fatalf("read pin host: %v", err)
			}
			if want := EffectiveGitHubHost(settings.GitHubBaseURL); pinHost != want {
				t.Errorf("pin host = %q, EffectiveGitHubHost = %q", pinHost, want)
			}
		})
	}
}

// TestMigrate_ExternalObjectScopeKeysDedupWithinAScope pins the rebuilt keys:
// every row survives the migration, a dedup key is unique only within a scope,
// and a pin is keyed per host.
func TestMigrate_ExternalObjectScopeKeysDedupWithinAScope(t *testing.T) {
	database := openMigrationsTestDBAt(t, TestDSNMemory, 202610100001)
	seedExternalObjectScopeOrg(t, database)
	if _, err := database.Exec(`
		INSERT INTO artifacts (id, team_id, provider, kind, target, state, dedup_key)
		VALUES ('a1', 'team-1', 'github', 'pull_request', 'octo/api#1', 'open', 'github:pull_request:octo/api#1'),
		       ('a2', 'team-1', 'git', 'branch', 'octo/api', 'pushed', 'git:branch:octo/api:refs/heads/x')
	`); err != nil {
		t.Fatalf("seed artifacts: %v", err)
	}
	if _, err := database.Exec(`
		INSERT INTO external_actions (id, provider, action, target, credential, dedup_key)
		VALUES ('x1', 'github', 'branch_pushed', 'octo/api', 'github_app', 'branch:c:refs/heads/x:sha')
	`); err != nil {
		t.Fatalf("seed action: %v", err)
	}
	if _, err := database.Exec(`
		INSERT INTO placement_overrides (key_kind, key_value, pinned_instance_id) VALUES ('repo', 'octo/api', 'exec-1')
	`); err != nil {
		t.Fatalf("seed pin: %v", err)
	}

	migrateExternalObjectScope(t, database)

	for table, want := range map[string]int{"artifacts": 2, "external_actions": 1, "placement_overrides": 1} {
		var n int
		if err := database.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != want {
			t.Errorf("%s rows = %d after the migration, want %d", table, n, want)
		}
	}
	var pinned sql.NullString
	if err := database.QueryRow(`SELECT pinned_instance_id FROM placement_overrides WHERE key_value = 'octo/api'`).Scan(&pinned); err != nil {
		t.Fatalf("read pin: %v", err)
	}
	if pinned.String != "exec-1" {
		t.Errorf("pinned instance = %v after the rebuild, want exec-1", pinned)
	}

	for _, stmt := range []struct {
		name, sql string
		unique    bool
	}{
		{"an artifact under the same key in another scope", `
			INSERT INTO artifacts (id, team_id, provider, scope, kind, target, state, dedup_key)
			VALUES ('a3', 'team-1', 'github', 'https://ghe.example.com', 'pull_request', 'octo/api#1', 'open', 'github:pull_request:octo/api#1')`, false},
		{"an artifact under the same key in the same scope", `
			INSERT INTO artifacts (id, team_id, provider, scope, kind, target, state, dedup_key)
			VALUES ('a4', 'team-1', 'github', 'https://github.com', 'pull_request', 'octo/api#1', 'open', 'github:pull_request:octo/api#1')`, true},
		{"an action under the same key in another scope", `
			INSERT INTO external_actions (id, provider, scope, action, target, credential, dedup_key)
			VALUES ('x2', 'github', 'https://ghe.example.com', 'branch_pushed', 'octo/api', 'github_app', 'branch:c:refs/heads/x:sha')`, false},
		{"an action under the same key in the same scope", `
			INSERT INTO external_actions (id, provider, scope, action, target, credential, dedup_key)
			VALUES ('x3', 'github', 'https://github.com', 'branch_pushed', 'octo/api', 'github_app', 'branch:c:refs/heads/x:sha')`, true},
		{"a pin on the same repository on another host", `
			INSERT INTO placement_overrides (key_kind, host, key_value, replicas) VALUES ('repo', 'https://ghe.example.com', 'octo/api', 3)`, false},
		{"a second pin on the same repository and host", `
			INSERT INTO placement_overrides (key_kind, host, key_value, replicas) VALUES ('repo', 'https://github.com', 'octo/api', 3)`, true},
	} {
		_, err := database.Exec(stmt.sql)
		switch {
		case stmt.unique && (err == nil || !strings.Contains(err.Error(), "UNIQUE")):
			t.Errorf("%s: err = %v, want a UNIQUE violation", stmt.name, err)
		case !stmt.unique && err != nil:
			t.Errorf("%s was refused: %v", stmt.name, err)
		}
	}
}

// seedExternalObjectScopeOrg stages the local org and the team the artifact
// fixtures name; a migrated database carries neither until boot seeds them.
func seedExternalObjectScopeOrg(t *testing.T, database *sql.DB) {
	t.Helper()
	for _, stmt := range []string{
		`INSERT INTO orgs (id, slug, name) VALUES ('` + runmode.LocalDefaultOrgID + `', 'local', 'Local')`,
		`INSERT INTO teams (id, org_id, slug, name) VALUES ('team-1', '` + runmode.LocalDefaultOrgID + `', 'core', 'Core')`,
	} {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
}

func migrateExternalObjectScope(t *testing.T, database *sql.DB) {
	t.Helper()
	treeFS, dir, err := migrationsFor("sqlite3")
	if err != nil {
		t.Fatalf("migrationsFor: %v", err)
	}
	gooseMu.Lock()
	goose.SetBaseFS(treeFS)
	upErr := goose.SetDialect("sqlite3")
	if upErr == nil {
		upErr = goose.UpTo(database, dir, externalObjectScopeVersion)
	}
	gooseMu.Unlock()
	if upErr != nil {
		t.Fatalf("goose.UpTo external object scope: %v", upErr)
	}
}
