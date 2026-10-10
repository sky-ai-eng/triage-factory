package db

import (
	"database/sql"
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// hostCaseColumns names every column 202610100002 rewrites, as a query that
// reads the one seeded row's value back.
var hostCaseColumns = map[string]string{
	"org_event_sources.base_url (github)":      `SELECT base_url FROM org_event_sources WHERE kind = 'github'`,
	"org_event_sources.base_url (jira)":        `SELECT base_url FROM org_event_sources WHERE kind = 'jira'`,
	"repositories.host":                        `SELECT host FROM repositories WHERE id = 'r1'`,
	"team_github_groups.host":                  `SELECT host FROM team_github_groups WHERE team_id = 'team-1'`,
	"entities.scope (github)":                  `SELECT scope FROM entities WHERE id = 'e-gh'`,
	"entities.scope (jira)":                    `SELECT scope FROM entities WHERE id = 'e-jira'`,
	"user_github_identities.github_base_url":   `SELECT github_base_url FROM user_github_identities WHERE user_id = 'u1'`,
	"user_jira_identities.jira_base_url":       `SELECT jira_base_url FROM user_jira_identities WHERE user_id = 'u1'`,
	"org_github_app_installations.github_host": `SELECT github_host FROM org_github_app_installations WHERE installation_id = '42'`,
	"reachable_repositories.host (pat)":        `SELECT host FROM reachable_repositories WHERE credential_class = 'pat'`,
	"reachable_scopes.scope (pat)":             `SELECT scope FROM reachable_scopes WHERE credential_class = 'pat'`,
}

// The host-case migration computes the canonical form of a GitHub host or Jira
// site with SQLite string functions, and every later read and write computes it
// through ghbase.CanonicalBaseURL. If the two disagreed on a value, every row
// holding it would sit under a key no read asks for. Each case stores one
// spelling in every column the migration rewrites, migrates, and requires each
// column to hold what the Go canonicalizer answers, and what the derivations
// that key real reads (EffectiveGitHubHost, NormalizeJiraHost, the entity
// scope) answer for the migrated setting.
func TestMigrate_HostCaseMatchesGo(t *testing.T) {
	inputs := []string{
		"https://GHE.Acme.com",
		"https://GHE.Acme.com/",
		"HTTPS://GHE.ACME.COM//",
		"https://GHE.Acme.com:8443",
		"https://GHE.Acme.com/GitHub/Ctx",
		"https://GHE.Acme.com:8443/GitHub/Ctx/",
		" https://Jira.Example.com/Jira ",
		"http://[::1]:8080/Ctx",
		"https://Ünicode.Example.com/Ü",
		"https://ghe.acme.com",
		"https://ghe.acme.com/GitHub",
		"GHE.Acme.com/Path",
	}
	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			database := openMigrationsTestDBAt(t, TestDSNMemory, 202610100001)
			seedHostCaseOrg(t, database)
			seedHostCaseRows(t, database, in)

			migrateHostCase(t, database)

			want := ghbase.CanonicalBaseURL(in)
			for name, query := range hostCaseColumns {
				var got string
				if err := database.QueryRow(query).Scan(&got); err != nil {
					t.Fatalf("read %s: %v", name, err)
				}
				if got != want {
					t.Errorf("%s = %q after migrate, ghbase.CanonicalBaseURL = %q", name, got, want)
				}
			}

			// The keys every read derives from the migrated setting land on
			// the rewritten rows.
			if got := EffectiveGitHubHost(want); got != want {
				t.Errorf("EffectiveGitHubHost(migrated setting) = %q, want the stored host %q", got, want)
			}
			if got := NormalizeJiraHost(want); got != want {
				t.Errorf("NormalizeJiraHost(migrated setting) = %q, want the stored site %q", got, want)
			}
			if got := domain.EntityScope("github", domain.OrgSettings{GitHubBaseURL: in}); got != want {
				t.Errorf("domain.EntityScope(github, %q) = %q, want the migrated scope %q", in, got, want)
			}
			if site, ok := domain.JiraHost(in); ok && site != want {
				t.Errorf("domain.JiraHost(%q) = %q, want the migrated scope %q", in, site, want)
			}
		})
	}
}

// TestMigrate_HostCaseSettlesCollisions stores rows that become one key once
// their host is lowercased. The migration must not fail on any of them, and each
// table settles the way its rule says: identities keep the most recently
// updated row (a tie keeps the canonical one), the deletable caches and
// mappings keep one row, and a referenced row whose key is already held keeps
// its old spelling, with the org's current spelling taking the key ahead of
// another non-canonical one.
func TestMigrate_HostCaseSettlesCollisions(t *testing.T) {
	const (
		current = "https://GHE.acme.com" // the org's setting
		other   = "https://Ghe.Acme.com" // another spelling of the same host
		canon   = "https://ghe.acme.com"
	)
	database := openMigrationsTestDBAt(t, TestDSNMemory, 202610100001)
	seedHostCaseOrg(t, database)
	org := runmode.LocalDefaultOrgID
	execHostCase(t, database, `INSERT INTO org_event_sources (org_id, kind, base_url) VALUES (?, 'github', ?)
		ON CONFLICT (org_id, kind) DO UPDATE SET base_url = excluded.base_url`, org, current+"/")
	execHostCase(t, database, `INSERT INTO users (id) VALUES ('u2')`)

	// Identities: u1's newer row is non-canonical, u2's two rows tie.
	execHostCase(t, database, `INSERT INTO user_github_identities (user_id, github_base_url, login, source, updated_at) VALUES
		('u1', ?, 'old-login', 'pat', '2026-01-01 00:00:00.000'),
		('u1', ?, 'new-login', 'pat', '2026-02-01 00:00:00.000'),
		('u2', ?, 'canonical-login', 'pat', '2026-01-01 00:00:00.000'),
		('u2', ?, 'other-login', 'pat', '2026-01-01 00:00:00.000')`, canon, current, canon, other)
	execHostCase(t, database, `INSERT INTO user_jira_identities (user_id, jira_base_url, account_id, source, updated_at) VALUES
		('u1', 'https://Jira.acme.com', 'old-account', 'pat', '2026-01-01 00:00:00.000'),
		('u1', 'https://jira.acme.com', 'new-account', 'pat', '2026-02-01 00:00:00.000')`)

	// Deletable duplicates.
	execHostCase(t, database, `INSERT INTO team_github_groups (team_id, host, github_org_login, github_team_slug) VALUES
		('team-1', ?, 'octo', 'core'), ('team-1', ?, 'octo', 'core')`, current, other)
	execHostCase(t, database, `INSERT INTO reachable_repositories (org_id, credential_class, host, owner, repo, observed_at) VALUES
		(?, 'pat', ?, 'octo', 'api', '2026-01-01 00:00:00.000'),
		(?, 'pat', ?, 'Octo', 'API', '2026-02-01 00:00:00.000')`, org, canon, org, current)
	execHostCase(t, database, `INSERT INTO reachable_scopes (org_id, credential_class, scope, refreshed_at) VALUES
		(?, 'pat', ?, '2026-01-01 00:00:00.000'), (?, 'pat', ?, '2026-02-01 00:00:00.000')`, org, other, org, current)

	// Referenced rows. octo/api: the other spelling is inserted first, so the
	// current spelling takes the key only because it is rewritten first.
	// octo/web: a canonical row already holds the key.
	execHostCase(t, database, `INSERT INTO repositories (id, owner, repo, source, host) VALUES
		('r-other', 'octo', 'api', 'github', ?),
		('r-current', 'octo', 'api', 'github', ?),
		('r-canon', 'octo', 'web', 'github', ?),
		('r-current-web', 'octo', 'web', 'github', ?)`, other, current, canon, current)
	execHostCase(t, database, `INSERT INTO entities (id, source, scope, source_id, kind, state) VALUES
		('e-other', 'github', ?, 'octo/api#1', 'pr', 'active'),
		('e-current', 'github', ?, 'octo/api#1', 'pr', 'active')`, other, current)

	migrateHostCase(t, database)

	readString := func(query string, args ...any) string {
		t.Helper()
		var s string
		if err := database.QueryRow(query, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return s
	}
	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := database.QueryRow(query, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return n
	}

	if got := readString(`SELECT base_url FROM org_event_sources WHERE kind = 'github'`); got != canon {
		t.Errorf("github base_url = %q, want %q", got, canon)
	}

	if n := count(`SELECT COUNT(*) FROM user_github_identities WHERE user_id = 'u1'`); n != 1 {
		t.Errorf("u1 github identities = %d, want 1", n)
	}
	if got := readString(`SELECT login FROM user_github_identities WHERE user_id = 'u1' AND github_base_url = ?`, canon); got != "new-login" {
		t.Errorf("u1 kept login %q, want the most recently updated row's new-login", got)
	}
	if got := readString(`SELECT login FROM user_github_identities WHERE user_id = 'u2' AND github_base_url = ?`, canon); got != "canonical-login" {
		t.Errorf("u2 kept login %q on a tie, want the canonical row's canonical-login", got)
	}
	if n := count(`SELECT COUNT(*) FROM user_github_identities WHERE user_id = 'u2'`); n != 1 {
		t.Errorf("u2 github identities = %d, want 1", n)
	}
	if got := readString(`SELECT account_id FROM user_jira_identities WHERE user_id = 'u1' AND jira_base_url = 'https://jira.acme.com'`); got != "new-account" {
		t.Errorf("u1 kept jira account %q, want new-account", got)
	}
	if n := count(`SELECT COUNT(*) FROM user_jira_identities`); n != 1 {
		t.Errorf("jira identities = %d, want 1", n)
	}

	if n := count(`SELECT COUNT(*) FROM team_github_groups WHERE host = ?`, canon); n != 1 {
		t.Errorf("team groups on %q = %d, want 1", canon, n)
	}
	if n := count(`SELECT COUNT(*) FROM team_github_groups`); n != 1 {
		t.Errorf("team groups = %d, want the duplicate removed", n)
	}
	if got := readString(`SELECT owner FROM reachable_repositories WHERE credential_class = 'pat' AND host = ?`, canon); got != "Octo" {
		t.Errorf("reachable row kept owner %q, want the most recently observed row's Octo", got)
	}
	if n := count(`SELECT COUNT(*) FROM reachable_repositories`); n != 1 {
		t.Errorf("reachable rows = %d, want 1", n)
	}
	if got := readString(`SELECT refreshed_at FROM reachable_scopes WHERE credential_class = 'pat' AND scope = ?`, canon); got != "2026-02-01 00:00:00.000" {
		t.Errorf("reachable scope kept refreshed_at %q, want the most recent", got)
	}
	if n := count(`SELECT COUNT(*) FROM reachable_scopes`); n != 1 {
		t.Errorf("reachable scopes = %d, want 1", n)
	}

	for id, want := range map[string]string{
		"r-current":     canon,
		"r-other":       other,
		"r-canon":       canon,
		"r-current-web": current,
	} {
		if got := readString(`SELECT host FROM repositories WHERE id = ?`, id); got != want {
			t.Errorf("repository %s host = %q, want %q", id, got, want)
		}
	}
	for id, want := range map[string]string{"e-current": canon, "e-other": other} {
		if got := readString(`SELECT scope FROM entities WHERE id = ?`, id); got != want {
			t.Errorf("entity %s scope = %q, want %q", id, got, want)
		}
	}

	fk, err := database.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer fk.Close()
	if fk.Next() {
		t.Error("foreign_key_check reports a dangling reference after the migration")
	}
}

// seedHostCaseOrg stages the local org, one team and one user.
func seedHostCaseOrg(t *testing.T, database *sql.DB) {
	t.Helper()
	seedGitHubHostOrg(t, database)
	execHostCase(t, database, `INSERT INTO users (id) VALUES ('u1')`)
}

// seedHostCaseRows stores host in every column hostCaseColumns reads.
func seedHostCaseRows(t *testing.T, database *sql.DB, host string) {
	t.Helper()
	org := runmode.LocalDefaultOrgID
	for _, kind := range []string{"github", "jira"} {
		execHostCase(t, database, `INSERT INTO org_event_sources (org_id, kind, base_url) VALUES (?, ?, ?)
			ON CONFLICT (org_id, kind) DO UPDATE SET base_url = excluded.base_url`, org, kind, host)
	}
	execHostCase(t, database, `INSERT INTO repositories (id, owner, repo, source, host) VALUES ('r1', 'octo', 'api', 'github', ?)`, host)
	execHostCase(t, database, `INSERT INTO team_github_groups (team_id, host, github_org_login, github_team_slug) VALUES ('team-1', ?, 'octo', 'core')`, host)
	execHostCase(t, database, `INSERT INTO entities (id, source, scope, source_id, kind) VALUES
		('e-gh', 'github', ?, 'octo/api#1', 'pr'), ('e-jira', 'jira', ?, 'KEY-1', 'issue')`, host, host)
	execHostCase(t, database, `INSERT INTO user_github_identities (user_id, github_base_url, login, source) VALUES ('u1', ?, 'octocat', 'pat')`, host)
	execHostCase(t, database, `INSERT INTO user_jira_identities (user_id, jira_base_url, account_id, source) VALUES ('u1', ?, 'acct', 'pat')`, host)
	execHostCase(t, database, `INSERT INTO org_github_app_installations (installation_id, org_id, account_type, account_login, github_host)
		VALUES ('42', ?, 'Organization', 'octo', ?)`, org, host)
	execHostCase(t, database, `INSERT INTO reachable_repositories (org_id, credential_class, host, owner, repo) VALUES (?, 'pat', ?, 'octo', 'api')`, org, host)
	execHostCase(t, database, `INSERT INTO reachable_scopes (org_id, credential_class, scope) VALUES (?, 'pat', ?)`, org, host)
}

func execHostCase(t *testing.T, database *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := database.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func migrateHostCase(t *testing.T, database *sql.DB) {
	t.Helper()
	treeFS, dir, err := migrationsFor("sqlite3")
	if err != nil {
		t.Fatalf("migrationsFor: %v", err)
	}
	gooseMu.Lock()
	goose.SetBaseFS(treeFS)
	upErr := goose.SetDialect("sqlite3")
	if upErr == nil {
		upErr = goose.UpTo(database, dir, 202610100002)
	}
	gooseMu.Unlock()
	if upErr != nil {
		t.Fatalf("goose.UpTo host case: %v", upErr)
	}
}
