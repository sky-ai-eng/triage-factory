package db

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// providerScopesVersion is 202610050001_provider_scopes_and_linear.sql, and
// beforeProviderScopes the version before it. Every fixture here is written in
// the schema the migration starts from.
const (
	beforeProviderScopes  int64 = 202610030003
	providerScopesVersion int64 = 202610050001
)

// The migration computes every scope it writes in SQL from the org's stored
// base URLs, and every later read and write computes the same value in Go:
// domain.EntityScope for an entity, EffectiveGitHubHost for a repository and a
// team mapping, domain.ExternalObjectScope for a deliverable
// and an audit-ledger row. If the two disagreed, every pre-existing row would
// sit under a scope no read asks for: the poller would mint a second entity
// beside each one, the tracked set would read empty, and the next capture of
// an object would mint a second deliverable. So each case stages one row of each
// kind under one org configuration, migrates, and requires every stored value
// to equal what the Go side answers.
func TestMigrate_ProviderScopesMatchGo(t *testing.T) {
	cases := []struct {
		name       string
		githubBase *string // nil = no override recorded
		jiraBase   *string // nil = Jira not configured
	}{
		{name: "defaults"},
		{name: "empty settings", githubBase: ptr(""), jiraBase: ptr("")},
		{name: "enterprise hosts", githubBase: ptr("https://ghe.example.com"), jiraBase: ptr("https://acme.atlassian.net")},
		{name: "trailing slash", githubBase: ptr("https://ghe.example.com/"), jiraBase: ptr("https://jira.example.com/")},
		{name: "trailing slashes", githubBase: ptr("https://ghe.example.com//"), jiraBase: ptr("https://jira.example.com//")},
		{name: "context paths", githubBase: ptr("https://example.com/github/"), jiraBase: ptr("https://jira.example.com/jira/")},
		{name: "surrounding whitespace", githubBase: ptr("  https://ghe.example.com/ "), jiraBase: ptr("\thttps://jira.example.com\n")},
		// The scheme and authority compare case-insensitively and are stored
		// lowercased; a context path keeps its case.
		{name: "mixed case", githubBase: ptr("HTTPS://GHE.Example.com/GitHub/"), jiraBase: ptr("Https://Jira.Example.com/Jira")},
		{name: "only slashes", githubBase: ptr(" / "), jiraBase: ptr(" / ")},
		{name: "schemeless jira", jiraBase: ptr("jira.example.com")},
		{name: "jira with no authority", jiraBase: ptr("https:///jira")},
		{name: "jira with another scheme", jiraBase: ptr("ftp://jira.example.com")},
	}
	// "git" was a branch deliverable's provider when this migration ran.
	providers := []string{
		domain.ProviderGitHub, "git", domain.ProviderJira,
		domain.ProviderLinear, domain.ProviderSlack, domain.ProviderNetwork,
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			database := openMigrationsTestDBAt(t, TestDSNMemory, beforeProviderScopes)
			seedProviderScopesOrg(t, database)
			var githubBase, jiraBase string
			if tc.githubBase != nil {
				githubBase = *tc.githubBase
				setOrgBaseURL(t, database, runmode.LocalDefaultOrgID, "github", githubBase)
			}
			if tc.jiraBase != nil {
				jiraBase = *tc.jiraBase
				setOrgBaseURL(t, database, runmode.LocalDefaultOrgID, "jira", jiraBase)
			}
			site, siteOK := domain.JiraHost(jiraBase)

			mustExec(t, database, `INSERT INTO entities (id, source, source_id, kind, title, url) VALUES
				('e-github', 'github', 'octo/api#1', 'pr', 't', NULL),
				('e-jira-stub', 'jira', 'KEY-1', 'issue', 't', NULL),
				('e-jira-elsewhere', 'jira', 'KEY-3', 'issue', 't', 'https://Other.Example.com/Ctx/browse/KEY-3')`)
			if siteOK {
				// The url the tracker builds from the org's base URL.
				mustExec(t, database, `INSERT INTO entities (id, source, source_id, kind, title, url) VALUES ('e-jira', 'jira', 'KEY-2', 'issue', 't', ?)`,
					strings.TrimRight(strings.TrimSpace(jiraBase), "/")+"/browse/KEY-2")
			}
			mustExec(t, database, `INSERT INTO repositories (id, owner, repo, source, external_id) VALUES ('r1', 'octo', 'api', 'github', '7')`)
			mustExec(t, database, `INSERT INTO team_github_groups (team_id, github_org_login, github_team_slug) VALUES ('team-1', 'octo', 'core')`)
			mustExec(t, database, `INSERT INTO placement_overrides (key_kind, key_value, replicas) VALUES ('repo', 'octo/api', 2)`)
			for _, p := range providers {
				mustExec(t, database, `INSERT INTO artifacts (id, team_id, provider, kind, target, state, dedup_key)
					VALUES (?, 'team-1', ?, 'comment', 't', 'posted', ?)`, "a-"+p, p, p+":comment:k")
				mustExec(t, database, `INSERT INTO external_actions (id, provider, action, target, credential, dedup_key)
					VALUES (?, ?, 'comment_posted', 't', 'none', ?)`, "x-"+p, p, p+":k")
			}

			migrateProviderScopes(t, database)

			settings := domain.OrgSettings{GitHubBaseURL: migratedGitHubBase(githubBase), JiraBaseURL: jiraBase}
			githubHost := EffectiveGitHubHost(settings.GitHubBaseURL)
			want := map[string]string{
				`SELECT scope FROM entities WHERE id = 'e-github'`:             domain.EntityScope("github", settings),
				`SELECT scope FROM entities WHERE id = 'e-jira-stub'`:          domain.EntityScope("jira", settings),
				`SELECT scope FROM entities WHERE id = 'e-jira-elsewhere'`:     "https://other.example.com/Ctx",
				`SELECT host FROM repositories WHERE id = 'r1'`:                githubHost,
				`SELECT repository_id FROM entities WHERE id = 'e-github'`:     "r1",
				`SELECT host FROM team_github_groups WHERE team_id = 'team-1'`: githubHost,
				// The pin named octo/api on the org's GitHub host, which is r1.
				`SELECT repository_id FROM placement_overrides`: "r1",
				// A mapping keeps its handle and carries no GitHub ids until the
				// poll's team reconcile identifies it.
				`SELECT COALESCE(github_team_id, 'none') || ' ' || COALESCE(github_org_id, 'none') || ' ' || github_org_login || '/' || github_team_slug
				   FROM team_github_groups WHERE team_id = 'team-1'`: "none none octo/core",
			}
			if siteOK {
				want[`SELECT scope FROM entities WHERE id = 'e-jira'`] = site
			}
			for _, p := range providers {
				scope := domain.ExternalObjectScope(p, settings)
				if p == "git" {
					// A branch was pushed to a GitHub repository.
					scope = domain.EntityScope(domain.ProviderGitHub, settings)
				}
				want[`SELECT scope FROM artifacts WHERE id = 'a-`+p+`'`] = scope
				want[`SELECT scope FROM external_actions WHERE id = 'x-`+p+`'`] = scope
			}
			// A stored base URL is rewritten into its canonical spelling, and
			// one of only whitespace and slashes, which has none, is left as
			// it is.
			for kind, base := range map[string]*string{"github": tc.githubBase, "jira": tc.jiraBase} {
				if base == nil {
					continue
				}
				stored := ghbase.CanonicalBaseURL(*base)
				if stored == "" {
					stored = *base
				}
				want[`SELECT base_url FROM org_event_sources WHERE kind = '`+kind+`'`] = stored
			}
			for query, w := range want {
				if got := queryString(t, database, query); got != w {
					t.Errorf("%s = %q, want %q", query, got, w)
				}
			}
		})
	}
}

// Every column holding a GitHub host or Jira site is a key compared by exact
// string, and every later read and write computes the spelling through
// ghbase.CanonicalBaseURL. The migration computes it with SQLite string
// functions; if the two disagreed on a value, every row holding it would sit
// under a key no read asks for. Each case stores one spelling in every column
// that holds a host or site, and as the org's base URLs, migrates, and requires
// each column — and each scope derived from the base URLs — to hold what the Go
// canonicalizer answers, and the derivations that key real reads to land on it.
func TestMigrate_ProviderScopesStoreHostsCanonically(t *testing.T) {
	columns := []string{
		`SELECT base_url FROM org_event_sources WHERE kind = 'github'`,
		`SELECT base_url FROM org_event_sources WHERE kind = 'jira'`,
		`SELECT github_base_url FROM user_github_identities WHERE user_id = 'u1'`,
		`SELECT jira_base_url FROM user_jira_identities WHERE user_id = 'u1'`,
		`SELECT github_host FROM org_github_app_installations WHERE installation_id = '42'`,
		`SELECT host FROM reachable_repositories WHERE credential_class = 'pat'`,
		`SELECT host FROM reachable_repositories WHERE credential_class = 'byo_app'`,
		`SELECT host FROM reachable_scopes WHERE credential_class = 'pat'`,
		`SELECT scope FROM reachable_scopes WHERE credential_class = 'pat'`,
		`SELECT host FROM reachable_scopes WHERE credential_class = 'byo_app'`,
		`SELECT scope FROM entities WHERE id = 'e-jira'`,
		// Derived from the org's GitHub base URL.
		`SELECT host FROM repositories WHERE id = 'r1'`,
		`SELECT host FROM team_github_groups WHERE team_id = 'team-1'`,
		`SELECT scope FROM entities WHERE id = 'e-github'`,
		`SELECT scope FROM artifacts WHERE id = 'a1'`,
	}
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
			database := openMigrationsTestDBAt(t, TestDSNMemory, beforeProviderScopes)
			seedProviderScopesOrg(t, database)
			org := runmode.LocalDefaultOrgID
			setOrgBaseURL(t, database, org, "github", in)
			setOrgBaseURL(t, database, org, "jira", in)
			mustExec(t, database, `INSERT INTO entities (id, source, source_id, kind, url) VALUES
				('e-github', 'github', 'octo/api#1', 'pr', NULL), ('e-jira', 'jira', 'KEY-1', 'issue', ?)`, in+"/browse/KEY-1")
			mustExec(t, database, `INSERT INTO repositories (id, owner, repo, source) VALUES ('r1', 'octo', 'api', 'github')`)
			mustExec(t, database, `INSERT INTO team_github_groups (team_id, github_org_login, github_team_slug) VALUES ('team-1', 'octo', 'core')`)
			mustExec(t, database, `INSERT INTO user_github_identities (user_id, github_base_url, login, source) VALUES ('u1', ?, 'octocat', 'pat')`, in)
			mustExec(t, database, `INSERT INTO user_jira_identities (user_id, jira_base_url, account_id, source) VALUES ('u1', ?, 'acct', 'pat')`, in)
			mustExec(t, database, `INSERT INTO org_github_app_installations (installation_id, org_id, account_type, account_login, github_host)
				VALUES ('42', ?, 'Organization', 'octo', ?)`, org, in)
			mustExec(t, database, `INSERT INTO reachable_repositories (org_id, credential_class, installation_id, host, owner, repo) VALUES
				(?, 'pat', NULL, ?, 'octo', 'api'), (?, 'byo_app', '42', NULL, 'octo', 'app')`, org, in, org)
			mustExec(t, database, `INSERT INTO reachable_scopes (org_id, credential_class, scope) VALUES (?, 'pat', ?), (?, 'byo_app', '42')`, org, in, org)
			mustExec(t, database, `INSERT INTO artifacts (id, team_id, provider, kind, target, state, dedup_key)
				VALUES ('a1', 'team-1', 'github', 'pull_request', 'octo/api#1', 'open', 'k')`)
			mustExec(t, database, `INSERT INTO placement_overrides (key_kind, key_value) VALUES ('repo', 'octo/api')`)

			migrateProviderScopes(t, database)

			want := ghbase.CanonicalBaseURL(in)
			for _, query := range columns {
				if got := queryString(t, database, query); got != want {
					t.Errorf("%s = %q after migrate, ghbase.CanonicalBaseURL = %q", query, got, want)
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
			// The pin named octo/api on that host, so it lands on the row that
			// answers to the name there.
			if got := queryString(t, database, `SELECT repository_id FROM placement_overrides`); got != "r1" {
				t.Errorf("placement override references %q, want r1", got)
			}
		})
	}
}

// TestMigrate_ProviderScopesSettleCollisions stores rows that become one key
// once their host is lowercased. The migration must not fail on any of them,
// and each table settles the way its rule says: identities keep the most
// recently updated row (a tie keeps the canonical one), the reachable-repo
// cache keeps one row per key, and of two orgs' live installations of one id
// that become one host, one keeps its old spelling, with its reach rows on
// the same spelling.
func TestMigrate_ProviderScopesSettleCollisions(t *testing.T) {
	const (
		current = "https://GHE.acme.com" // the org's setting
		other   = "https://Ghe.Acme.com" // another spelling of the same host
		canon   = "https://ghe.acme.com"
		orgB    = "org-b"
	)
	database := openMigrationsTestDBAt(t, TestDSNMemory, beforeProviderScopes)
	seedProviderScopesOrg(t, database)
	org := runmode.LocalDefaultOrgID
	setOrgBaseURL(t, database, org, "github", current+"/")
	mustExec(t, database, `INSERT INTO orgs (id, slug, name) VALUES (?, 'b', 'B')`, orgB)
	mustExec(t, database, `INSERT INTO users (id) VALUES ('u2')`)

	// Identities: u1's newer row is non-canonical, u2's two rows tie.
	mustExec(t, database, `INSERT INTO user_github_identities (user_id, github_base_url, login, source, updated_at) VALUES
		('u1', ?, 'old-login', 'pat', '2026-01-01 00:00:00.000'),
		('u1', ?, 'new-login', 'pat', '2026-02-01 00:00:00.000'),
		('u2', ?, 'canonical-login', 'pat', '2026-01-01 00:00:00.000'),
		('u2', ?, 'other-login', 'pat', '2026-01-01 00:00:00.000')`, canon, current, canon, other)
	mustExec(t, database, `INSERT INTO user_jira_identities (user_id, jira_base_url, account_id, source, updated_at) VALUES
		('u1', 'https://Jira.acme.com', 'old-account', 'pat', '2026-01-01 00:00:00.000'),
		('u1', 'https://jira.acme.com', 'new-account', 'pat', '2026-02-01 00:00:00.000')`)

	// The reachable-repo cache's PAT tier.
	mustExec(t, database, `INSERT INTO reachable_repositories (org_id, credential_class, host, owner, repo, observed_at) VALUES
		(?, 'pat', ?, 'octo', 'api', '2026-01-01 00:00:00.000'),
		(?, 'pat', ?, 'Octo', 'API', '2026-02-01 00:00:00.000')`, org, canon, org, current)
	mustExec(t, database, `INSERT INTO reachable_scopes (org_id, credential_class, scope, refreshed_at) VALUES
		(?, 'pat', ?, '2026-01-01 00:00:00.000'), (?, 'pat', ?, '2026-02-01 00:00:00.000')`, org, other, org, current)

	// Two orgs' live installations of one id, on two spellings of one host.
	mustExec(t, database, `INSERT INTO org_github_app_installations (installation_id, org_id, account_type, account_login, github_host) VALUES
		('42', ?, 'Organization', 'acme', ?), ('42', ?, 'Organization', 'acme', ?)`, org, current, orgB, other)
	mustExec(t, database, `INSERT INTO reachable_repositories (org_id, credential_class, installation_id, owner, repo) VALUES
		(?, 'byo_app', '42', 'acme', 'api'), (?, 'byo_app', '42', 'acme', 'web')`, org, orgB)

	migrateProviderScopes(t, database)

	if got := queryString(t, database, `SELECT base_url FROM org_event_sources WHERE kind = 'github'`); got != canon {
		t.Errorf("github base_url = %q, want %q", got, canon)
	}

	if n := queryInt(t, database, `SELECT COUNT(*) FROM user_github_identities WHERE user_id = 'u1'`); n != 1 {
		t.Errorf("u1 github identities = %d, want 1", n)
	}
	if got := queryString(t, database, `SELECT login FROM user_github_identities WHERE user_id = 'u1' AND github_base_url = ?`, canon); got != "new-login" {
		t.Errorf("u1 kept login %q, want the most recently updated row's new-login", got)
	}
	if got := queryString(t, database, `SELECT login FROM user_github_identities WHERE user_id = 'u2' AND github_base_url = ?`, canon); got != "canonical-login" {
		t.Errorf("u2 kept login %q on a tie, want the canonical row's canonical-login", got)
	}
	if n := queryInt(t, database, `SELECT COUNT(*) FROM user_github_identities WHERE user_id = 'u2'`); n != 1 {
		t.Errorf("u2 github identities = %d, want 1", n)
	}
	if got := queryString(t, database, `SELECT account_id FROM user_jira_identities WHERE user_id = 'u1' AND jira_base_url = 'https://jira.acme.com'`); got != "new-account" {
		t.Errorf("u1 kept jira account %q, want new-account", got)
	}
	if n := queryInt(t, database, `SELECT COUNT(*) FROM user_jira_identities`); n != 1 {
		t.Errorf("jira identities = %d, want 1", n)
	}

	if got := queryString(t, database, `SELECT owner FROM reachable_repositories WHERE credential_class = 'pat' AND host = ?`, canon); got != "Octo" {
		t.Errorf("reachable row kept owner %q, want the most recently observed row's Octo", got)
	}
	if n := queryInt(t, database, `SELECT COUNT(*) FROM reachable_repositories WHERE credential_class = 'pat'`); n != 1 {
		t.Errorf("PAT reachable rows = %d, want 1", n)
	}
	if got := queryString(t, database, `SELECT strftime('%Y-%m-%d', refreshed_at) FROM reachable_scopes WHERE credential_class = 'pat' AND scope = ?`, canon); got != "2026-02-01" {
		t.Errorf("reachable scope kept refreshed_at %q, want the most recent", got)
	}
	if n := queryInt(t, database, `SELECT COUNT(*) FROM reachable_scopes`); n != 1 {
		t.Errorf("reachable scopes = %d, want 1", n)
	}

	hosts := map[string]string{}
	for _, o := range []string{org, orgB} {
		host := queryString(t, database, `SELECT github_host FROM org_github_app_installations WHERE org_id = ?`, o)
		hosts[host] = o
		if reach := queryString(t, database, `SELECT host FROM reachable_repositories WHERE org_id = ? AND credential_class = 'byo_app'`, o); reach != host {
			t.Errorf("org %s App reach row host = %q, want its installation's %q", o, reach, host)
		}
	}
	if _, ok := hosts[canon]; !ok || len(hosts) != 2 {
		t.Errorf("installation hosts = %v, want one on %q and the other on its old spelling", hosts, canon)
	}

	assertNoDanglingReferences(t, database)
}

// TestMigrate_EntityScopeKeepsEveryReference rebuilds the entities table under
// rows that point at it, with foreign keys on. A rebuild that let the drop
// cascade would delete every task, event and memory row with the old table.
func TestMigrate_EntityScopeKeepsEveryReference(t *testing.T) {
	database := openMigrationsTestDBAt(t, TestDSNMemory, beforeProviderScopes)
	seedProviderScopesOrg(t, database)
	mustExec(t, database, `INSERT OR IGNORE INTO events_catalog (id, source, category, label, description) VALUES ('github:pr:opened', 'github', 'pr', 'Opened', '')`)
	mustExec(t, database, `
		INSERT INTO entities (id, source, source_id, kind, title, state, closed_at)
		VALUES ('e1', 'github', 'octo/api#1', 'pr', 't', 'active', NULL),
		       ('e2', 'github', 'octo/api#2', 'pr', 't', 'closed', CURRENT_TIMESTAMP)`)
	mustExec(t, database, `INSERT INTO events (id, entity_id, event_type, dedup_key) VALUES ('ev1', 'e1', 'github:pr:opened', '')`)
	mustExec(t, database, `
		INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status)
		VALUES ('t1', 'e1', 'github:pr:opened', 'ev1', 'queued')`)

	migrateProviderScopes(t, database)

	for table, want := range map[string]int{"entities": 2, "events": 1, "tasks": 1} {
		if n := queryInt(t, database, `SELECT COUNT(*) FROM `+table); n != want {
			t.Errorf("%s rows = %d after the rebuild, want %d", table, n, want)
		}
	}
	var state, extID sql.NullString
	if err := database.QueryRow(`SELECT state, external_id FROM entities WHERE id = 'e2'`).Scan(&state, &extID); err != nil {
		t.Fatalf("read e2: %v", err)
	}
	if state.String != "closed" || extID.Valid {
		t.Errorf("e2 = state %q external_id %v, want closed with no id learned", state.String, extID)
	}
	assertNoDanglingReferences(t, database)

	// The key is unique among active rows only: a closed row's key is free.
	// Both rows are in octo/api, the repository the backfill resolved for e1.
	repositoryID := queryString(t, database, `SELECT repository_id FROM entities WHERE id = 'e1'`)
	if _, err := database.Exec(`
		INSERT INTO entities (id, source, scope, source_id, kind, state, repository_id)
		VALUES ('e3', 'github', 'https://github.com', 'octo/api#2', 'pr', 'active', ?)
	`, repositoryID); err != nil {
		t.Errorf("a new active row under a closed row's key was refused: %v", err)
	}
	if _, err := database.Exec(`
		INSERT INTO entities (id, source, scope, source_id, kind, state, repository_id)
		VALUES ('e4', 'github', 'https://github.com', 'octo/api#1', 'pr', 'active', ?)
	`, repositoryID); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("a second active row under one key: err=%v, want a UNIQUE violation", err)
	}
}

// TestMigrate_GitHubHostKeepsReferencesAndSettlesDuplicateIDs migrates a
// registry that two rows share one provider id in — nothing enforced the id's
// uniqueness before — under the rows that reference it. The id stays on the
// most recently updated row, every reference survives, and the new indexes
// separate hosts.
func TestMigrate_GitHubHostKeepsReferencesAndSettlesDuplicateIDs(t *testing.T) {
	database := openMigrationsTestDBAt(t, TestDSNMemory, beforeProviderScopes)
	seedProviderScopesOrg(t, database)
	mustExec(t, database, `
		INSERT INTO repositories (id, owner, repo, source, external_id, updated_at)
		VALUES ('r-old', 'octo', 'api', 'github', '7', '2026-01-01 00:00:00'),
		       ('r-new', 'octo', 'platform', 'github', '7', '2026-02-01 00:00:00'),
		       ('r-other', 'octo', 'web', 'github', '8', '2026-01-01 00:00:00')`)
	mustExec(t, database, `
		INSERT INTO team_github_repos (team_id, repository_id, org_id)
		VALUES ('team-1', 'r-old', ?), ('team-1', 'r-other', ?)`, runmode.LocalDefaultOrgID, runmode.LocalDefaultOrgID)

	migrateProviderScopes(t, database)

	ids := map[string]sql.NullString{}
	rows, err := database.Query(`SELECT id, external_id FROM repositories`)
	if err != nil {
		t.Fatalf("read repositories: %v", err)
	}
	for rows.Next() {
		var id string
		var ext sql.NullString
		if err := rows.Scan(&id, &ext); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids[id] = ext
	}
	_ = rows.Close()
	if ids["r-new"].String != "7" || ids["r-old"].Valid || ids["r-other"].String != "8" {
		t.Errorf("external ids after migrate = %v, want the shared id only on the most recently updated row", ids)
	}
	if n := queryInt(t, database, `SELECT COUNT(*) FROM team_github_repos`); n != 2 {
		t.Errorf("tracking rows = %d after migrate, want 2", n)
	}
	assertNoDanglingReferences(t, database)

	// One name per host, and one id per host.
	if _, err := database.Exec(`
		INSERT INTO repositories (id, owner, repo, source, host, external_id)
		VALUES ('r-ghe', 'OCTO', 'API', 'github', 'https://ghe.example.com', '7')
	`); err != nil {
		t.Errorf("the same name and id on another host was refused: %v", err)
	}
	if _, err := database.Exec(`
		INSERT INTO repositories (id, owner, repo, source, host)
		VALUES ('r-dup', 'Octo', 'Api', 'github', 'https://github.com')
	`); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("a second row under one name on one host: err=%v, want a UNIQUE violation", err)
	}
	if _, err := database.Exec(`
		INSERT INTO repositories (id, owner, repo, source, host, external_id)
		VALUES ('r-dup-id', 'octo', 'elsewhere', 'github', 'https://github.com', '8')
	`); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("a second row under one id on one host: err=%v, want a UNIQUE violation", err)
	}
}

// TestMigrate_ExternalObjectScopeKeysDedupWithinAScope pins the rebuilt keys:
// every row survives the migration, a dedup key is unique only within a scope,
// and a pin is keyed per registry row.
func TestMigrate_ExternalObjectScopeKeysDedupWithinAScope(t *testing.T) {
	database := openMigrationsTestDBAt(t, TestDSNMemory, beforeProviderScopes)
	seedProviderScopesOrg(t, database)
	mustExec(t, database, `
		INSERT INTO artifacts (id, team_id, provider, kind, target, state, dedup_key)
		VALUES ('a1', 'team-1', 'github', 'pull_request', 'octo/api#1', 'open', 'github:pull_request:octo/api#1'),
		       ('a2', 'team-1', 'git', 'branch', 'octo/api', 'pushed', 'git:branch:octo/api:refs/heads/x')`)
	mustExec(t, database, `
		INSERT INTO external_actions (id, provider, action, target, credential, dedup_key)
		VALUES ('x1', 'github', 'branch_pushed', 'octo/api', 'github_app', 'branch:c:refs/heads/x:sha')`)
	mustExec(t, database, `INSERT INTO repositories (id, owner, repo, source) VALUES ('r1', 'octo', 'api', 'github')`)
	mustExec(t, database, `INSERT INTO placement_overrides (key_kind, key_value, pinned_instance_id) VALUES ('repo', 'octo/api', 'exec-1')`)

	migrateProviderScopes(t, database)
	mustExec(t, database, `INSERT INTO repositories (id, host, owner, repo, source) VALUES ('r2', 'https://ghe.example.com', 'octo', 'api', 'github')`)

	for table, want := range map[string]int{"artifacts": 2, "external_actions": 1, "placement_overrides": 1} {
		if n := queryInt(t, database, `SELECT COUNT(*) FROM `+table); n != want {
			t.Errorf("%s rows = %d after the migration, want %d", table, n, want)
		}
	}
	if got := queryString(t, database, `SELECT pinned_instance_id FROM placement_overrides WHERE repository_id = 'r1'`); got != "exec-1" {
		t.Errorf("pinned instance = %q after the rebuild, want exec-1", got)
	}

	for _, stmt := range []struct {
		name, sql string
		unique    bool
	}{
		{"a deliverable under the same key in another scope", `
			INSERT INTO artifacts (id, team_id, provider, scope, kind, target, state, dedup_key)
			VALUES ('a3', 'team-1', 'github', 'https://ghe.example.com', 'pull_request', 'octo/api#1', 'open', 'github:pull_request:octo/api#1')`, false},
		{"a deliverable under the same key in the same scope", `
			INSERT INTO artifacts (id, team_id, provider, scope, kind, target, state, dedup_key)
			VALUES ('a4', 'team-1', 'github', 'https://github.com', 'pull_request', 'octo/api#1', 'open', 'github:pull_request:octo/api#1')`, true},
		{"an action under the same key in another scope", `
			INSERT INTO external_actions (id, provider, scope, action, target, credential, dedup_key)
			VALUES ('x2', 'github', 'https://ghe.example.com', 'branch_pushed', 'octo/api', 'github_app', 'branch:c:refs/heads/x:sha')`, false},
		{"an action under the same key in the same scope", `
			INSERT INTO external_actions (id, provider, scope, action, target, credential, dedup_key)
			VALUES ('x3', 'github', 'https://github.com', 'branch_pushed', 'octo/api', 'github_app', 'branch:c:refs/heads/x:sha')`, true},
		{"a pin on the repository under the same name on another host", `
			INSERT INTO placement_overrides (repository_id, replicas) VALUES ('r2', 3)`, false},
		{"a second pin on the same repository", `
			INSERT INTO placement_overrides (repository_id, replicas) VALUES ('r1', 3)`, true},
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

// Each repo pin lands on the live registry row its owner/repo names on the
// org's GitHub host, case-insensitively, and the most recently updated of two
// pins on one row is kept. A pin no row answers to, and a pin of another kind,
// are dropped.
func TestMigrate_ProviderScopesKeyPlacementOverridesOnRepositories(t *testing.T) {
	database := openMigrationsTestDBAt(t, TestDSNMemory, beforeProviderScopes)
	seedProviderScopesOrg(t, database)
	setOrgBaseURL(t, database, runmode.LocalDefaultOrgID, "github", "https://ghe.example.com")
	mustExec(t, database, `INSERT INTO repositories (id, owner, repo, source) VALUES
		('r-api', 'octo', 'api', 'github'), ('r-web', 'octo', 'Web', 'github')`)
	mustExec(t, database, `INSERT INTO placement_overrides (key_kind, key_value, pinned_instance_id, replicas, updated_at) VALUES
		('repo', 'octo/api', 'exec-old', 0, '2026-01-01 00:00:00'),
		('repo', 'Octo/API', 'exec-new', 0, '2026-02-01 00:00:00'),
		('repo', 'octo/web', NULL, 3, '2026-01-01 00:00:00'),
		('repo', 'octo/gone', 'exec-1', 0, '2026-01-01 00:00:00'),
		('project', 'octo/api', 'exec-2', 0, '2026-03-01 00:00:00')`)

	migrateProviderScopes(t, database)

	got := queryString(t, database, `SELECT group_concat(repository_id || '|' || COALESCE(pinned_instance_id, '') || '|' || replicas, ','
	                                  ORDER BY repository_id) FROM placement_overrides`)
	if want := "r-api|exec-new|0,r-web||3"; got != want {
		t.Errorf("overrides = %s, want %s", got, want)
	}
	assertNoDanglingReferences(t, database)
}

// TestMigrate_InstallationsKeyedByHost covers the installation keys. The
// fixture is written under the old ones: one install of a login per org across
// every GitHub, one installation per id in the org, App reach rows and markers
// that name no host, and an App marker whose installation no longer exists.
// Afterwards a login and an id are each unique per host, every row carries
// its installation's host, the orphan marker is gone, and the foreign key
// holds a reach row to the installation on its own host.
func TestMigrate_InstallationsKeyedByHost(t *testing.T) {
	const (
		org  = runmode.LocalDefaultOrgID
		ghes = "https://ghe.example.com"
		dot  = "https://github.com"
	)
	database := openMigrationsTestDBAt(t, TestDSNMemory, beforeProviderScopes)
	seedProviderScopesOrg(t, database)
	install := func(installationID, login, host string) error {
		_, err := database.Exec(`
			INSERT INTO org_github_app_installations (installation_id, org_id, account_type, account_login, github_host)
			VALUES (?, ?, 'Organization', ?, ?)
		`, installationID, org, login, host)
		return err
	}
	mustExec(t, database, `INSERT INTO org_github_app_installations (installation_id, org_id, account_type, account_login, github_host, repository_selection)
	         VALUES ('3', ?, 'Organization', 'acme', ?, 'selected')`, org, ghes)
	if err := install("2", "acme", dot); err == nil {
		t.Fatal("the same login on a second host was admitted before the migration; the fixture must start on the old key")
	}
	mustExec(t, database, `INSERT INTO reachable_repositories (org_id, credential_class, installation_id, owner, repo)
	         VALUES (?, 'byo_app', '3', 'acme', 'api')`, org)
	mustExec(t, database, `INSERT INTO reachable_repositories (org_id, credential_class, host, owner, repo)
	         VALUES (?, 'pat', ?, 'acme', 'personal')`, org, dot)
	mustExec(t, database, `INSERT INTO reachable_scopes (org_id, credential_class, scope) VALUES (?, 'byo_app', '3')`, org)
	mustExec(t, database, `INSERT INTO reachable_scopes (org_id, credential_class, scope) VALUES (?, 'byo_app', '9')`, org)
	mustExec(t, database, `INSERT INTO reachable_scopes (org_id, credential_class, scope) VALUES (?, 'pat', ?)`, org, dot)

	migrateProviderScopes(t, database)

	if got := queryString(t, database, `SELECT repository_selection FROM org_github_app_installations
	                                     WHERE org_id = ? AND github_host = ? AND installation_id = '3'`, org, ghes); got != "selected" {
		t.Errorf("repository_selection = %q after the copy; want selected", got)
	}

	hosts := func(t *testing.T, query string) map[string]string {
		t.Helper()
		rows, err := database.Query(query, org)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var key, host string
			if err := rows.Scan(&key, &host); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out[key] = host
		}
		return out
	}
	reach := hosts(t, `SELECT credential_class || ':' || repo, host FROM reachable_repositories WHERE org_id = ?`)
	if len(reach) != 2 || reach["byo_app:api"] != ghes || reach["pat:personal"] != dot {
		t.Errorf("reach rows = %v; want the App row on its installation's %s and the PAT row on %s", reach, ghes, dot)
	}
	scopes := hosts(t, `SELECT credential_class || ':' || scope, host FROM reachable_scopes WHERE org_id = ?`)
	if len(scopes) != 2 || scopes["byo_app:3"] != ghes || scopes["pat:"+dot] != dot {
		t.Errorf("scope markers = %v; want 3 on %s and the PAT marker on %s, and none for the missing installation 9", scopes, ghes, dot)
	}

	// A login is unique per host.
	if err := install("2", "acme", dot); err != nil {
		t.Errorf("the same login on a second host after the migration: %v", err)
	}
	if err := install("4", "acme", dot); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("the same login twice on one host after the migration: err=%v, want a UNIQUE violation", err)
	}

	// The same id on another host is another installation, with reach of its
	// own on that host.
	if err := install("3", "globex", dot); err != nil {
		t.Fatalf("the same id on another host: %v", err)
	}
	mustExec(t, database, `INSERT INTO reachable_repositories (org_id, credential_class, installation_id, host, owner, repo)
	         VALUES (?, 'byo_app', '3', ?, 'globex', 'web')`, org, dot)
	if _, err := database.Exec(`INSERT INTO reachable_repositories (org_id, credential_class, installation_id, host, owner, repo)
	                            VALUES (?, 'byo_app', '3', 'https://elsewhere.example.com', 'acme', 'ghost')`, org); err == nil {
		t.Error("an App reach row landed on a host where its installation does not exist; the foreign key must refuse it")
	}
	if _, err := database.Exec(`INSERT INTO org_github_app_installations (installation_id, org_id, account_type, account_login)
	                            VALUES ('5', ?, 'Organization', 'initech')`, org); err == nil {
		t.Error("an installation with no host landed; the column has no default to stand in for one")
	}

	// Deleting one host's installation takes its reach and nothing else.
	mustExec(t, database, `DELETE FROM org_github_app_installations WHERE org_id = ? AND github_host = ? AND installation_id = '3'`, org, ghes)
	reach = hosts(t, `SELECT credential_class || ':' || repo, host FROM reachable_repositories WHERE org_id = ?`)
	if len(reach) != 2 || reach["byo_app:web"] != dot || reach["pat:personal"] != dot {
		t.Errorf("reach rows after deleting the GHES installation = %v; want the github.com App row and the PAT row", reach)
	}
}

// One live installation per account is held on the account id after the
// migration, and on the login only for a row with no id. Where the old login
// key let one account hold two live rows under two logins, the migration keeps
// the later install and removes the earlier one with its reach and refresh
// marker, as an uninstall would.
func TestMigrate_InstallationsKeyedByAccountID(t *testing.T) {
	const (
		org = runmode.LocalDefaultOrgID
		dot = "https://github.com"
	)
	database := openMigrationsTestDBAt(t, TestDSNMemory, beforeProviderScopes)
	seedProviderScopesOrg(t, database)
	mustExec(t, database, `INSERT INTO org_github_app_installations
	             (installation_id, org_id, account_type, account_id, account_login, github_host, installed_at)
	         VALUES ('10', ?, 'Organization', '1234', 'acme', ?, '2026-01-01 00:00:00'),
	                ('11', ?, 'Organization', '1234', 'acme-corp', ?, '2026-02-01 00:00:00')`, org, dot, org, dot)
	mustExec(t, database, `INSERT INTO reachable_repositories (org_id, credential_class, installation_id, owner, repo)
	         VALUES (?, 'byo_app', '10', 'acme', 'api'), (?, 'byo_app', '11', 'acme-corp', 'api')`, org, org)
	mustExec(t, database, `INSERT INTO reachable_scopes (org_id, credential_class, scope)
	         VALUES (?, 'byo_app', '10'), (?, 'byo_app', '11')`, org, org)

	migrateProviderScopes(t, database)

	for _, q := range []string{
		`SELECT group_concat(installation_id) FROM org_github_app_installations WHERE removed_at IS NULL`,
		`SELECT group_concat(installation_id) FROM reachable_repositories`,
		`SELECT group_concat(scope) FROM reachable_scopes`,
	} {
		if got := queryString(t, database, q); got != "11" {
			t.Errorf("%s = %q; want the later install's alone", q, got)
		}
	}

	for _, c := range []struct {
		installationID, accountID, login string
		refused                          bool
	}{
		{"12", "1234", "acme-renamed", true}, // a second live installation for one account
		{"13", "5678", "acme-corp", false},   // another account under a login a live row carries
		{"14", "", "initech", false},
		{"15", "", "initech", true}, // a second id-less installation under one login
	} {
		_, err := database.Exec(`
			INSERT INTO org_github_app_installations (installation_id, org_id, account_type, account_id, account_login, github_host)
			VALUES (?, ?, 'Organization', NULLIF(?, ''), ?, ?)`, c.installationID, org, c.accountID, c.login, dot)
		if (err != nil) != c.refused || (err != nil && !strings.Contains(err.Error(), "UNIQUE")) {
			t.Errorf("install %s (account %q, login %s): err=%v, want refused=%v", c.installationID, c.accountID, c.login, err, c.refused)
		}
	}
}

// Nothing stored holds the owner account's id for a repository, and an
// installation whose login matches the owner is not taken for one, so every
// existing repositories and reachable_repositories row comes out with no
// owner_external_id: the "not learned yet" state.
func TestMigrate_ProviderScopesLeaveRepositoryOwnerIDsToBeLearned(t *testing.T) {
	const (
		org = runmode.LocalDefaultOrgID
		dot = "https://github.com"
	)
	database := openMigrationsTestDBAt(t, TestDSNMemory, beforeProviderScopes)
	seedProviderScopesOrg(t, database)
	setOrgBaseURL(t, database, org, "github", dot)
	mustExec(t, database, `INSERT INTO repositories (id, owner, repo, source, external_id)
	         VALUES ('r-api', 'acme', 'api', 'github', '10')`)
	mustExec(t, database, `INSERT INTO org_github_app_installations
	             (installation_id, org_id, account_type, account_id, account_login, github_host)
	         VALUES ('1', ?, 'Organization', '500', 'acme', ?)`, org, dot)
	mustExec(t, database, `INSERT INTO reachable_repositories (org_id, credential_class, installation_id, owner, repo, external_id)
	         VALUES (?, 'byo_app', '1', 'acme', 'api', '10')`, org)

	migrateProviderScopes(t, database)

	for _, table := range []string{"repositories", "reachable_repositories"} {
		if got := queryString(t, database, `SELECT COUNT(*) || ' rows, ' || COUNT(owner_external_id) || ' with an owner id' FROM `+table); got != "1 rows, 0 with an owner id" {
			t.Errorf("%s: %s; want the fixture's 1 row with none", table, got)
		}
	}
}

// The migration takes 'completed' out of conversations.status. A verdict parks
// the row `open`, and completed_at is what makes an `open` row concluded, so
// the stamp has to survive the rewrite — and a row the old terminal write left
// without one has to gain one, or it lands unsettled: live to the router and
// claimable on undelivered input.
//
// Staged by migrating UP TO the previous version and writing rows the way the
// old build did, for the same reason as TestMigrate_RetiresDeadConversationStatuses.
func TestMigrate_ConversationVerdictParksOpen(t *testing.T) {
	database := openMigrationsTestDBAt(t, TestDSNMemory, beforeProviderScopes)

	seedCompletedConversation(t, database, "stamped", "2026-01-02 03:04:05", nil, "user_cancelled")
	seedCompletedConversation(t, database, "unstamped-ended", nil, "2026-01-03 00:00:00", nil)
	seedCompletedConversation(t, database, "unstamped", nil, nil, nil)

	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	read := func(id string) (status, outcome string, completedAt, parkReason sql.NullString) {
		t.Helper()
		if err := database.QueryRow(
			`SELECT status, outcome, completed_at, park_reason FROM conversations WHERE id = ?`, id,
		).Scan(&status, &outcome, &completedAt, &parkReason); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return status, outcome, completedAt, parkReason
	}

	status, outcome, completedAt, parkReason := read("stamped")
	if status != "open" || outcome != "finish" {
		t.Errorf("stamped row = (%q, %q), want (open, finish) — the verdict stays on the row", status, outcome)
	}
	if completedAt.String != "2026-01-02T03:04:05Z" {
		t.Errorf("completed_at = %v, want the terminal write's stamp kept", completedAt)
	}
	if parkReason.Valid {
		t.Errorf("park_reason = %q, want cleared — a verdict records no reason", parkReason.String)
	}

	if _, _, completedAt, _ := read("unstamped-ended"); completedAt.String != "2026-01-03T00:00:00Z" {
		t.Errorf("unstamped ended row completed_at = %v, want its ended_at", completedAt)
	}
	if _, _, completedAt, _ := read("unstamped"); completedAt.String != "2026-01-01T00:00:00Z" {
		t.Errorf("unstamped row completed_at = %v, want its started_at", completedAt)
	}

	if n := queryInt(t, database,
		`SELECT COUNT(*) FROM conversations WHERE status = 'completed' OR (status = 'open' AND completed_at IS NULL AND outcome IS NOT NULL)`,
	); n != 0 {
		t.Errorf("%d conversations still read completed, or carry a verdict without a conclusion stamp", n)
	}
}

// Among the deliverable rows written before the migration, a GitHub comment row
// with no target was left by an edit or a delete of a comment TF had not
// posted, and is deleted, so a comment row existing means TF posted the
// comment. The rows the posting verbs wrote stay, and so does every targetless
// row of another provider or kind.
func TestMigrate_DeliverableCommentIdentityKeepsOnlyCommentsTFPosted(t *testing.T) {
	database := openMigrationsTestDBAt(t, TestDSNMemory, beforeProviderScopes)
	seedProviderScopesOrg(t, database)

	rows := []struct {
		id, provider, kind, target, state string
		kept                              bool
	}{
		{"gh-posted", "github", "comment", "octo/api#3", "posted", true},
		{"gh-posted-then-deleted", "github", "comment", "octo/api#3", "deleted", true},
		{"gh-edited", "github", "comment", "", "posted", false},
		{"gh-deleted", "github", "comment", "", "deleted", false},
		{"jira-posted", "jira", "comment", "ENG-1", "posted", true},
		{"linear-posted", "linear", "comment", "ENG-2", "posted", true},
		{"pr", "github", "pull_request", "octo/api#3", "open", true},
		{"jira-targetless", "jira", "comment", "", "posted", true},
		{"gh-targetless-branch", "github", "branch", "", "pushed", true},
	}
	for _, r := range rows {
		mustExec(t, database, `
			INSERT INTO artifacts (id, team_id, provider, kind, target, state, dedup_key)
			VALUES (?, 'team-1', ?, ?, ?, ?, ?)
		`, r.id, r.provider, r.kind, r.target, r.state, "key-"+r.id)
	}

	migrateProviderScopes(t, database)

	for _, r := range rows {
		var nodeID *string
		err := database.QueryRow(`SELECT node_id FROM artifacts WHERE id = ?`, r.id).Scan(&nodeID)
		switch {
		case !r.kept && err == nil:
			t.Errorf("%s survived the migration, want it deleted", r.id)
		case r.kept && err != nil:
			t.Errorf("read %s: %v, want it kept", r.id, err)
		case r.kept && nodeID != nil:
			t.Errorf("%s node_id = %q, want NULL", r.id, *nodeID)
		}
	}
}

// The migration runs outside goose's transaction, because its rebuilds need
// foreign keys off, and opens one of its own. A statement failing part way
// must commit nothing — the connection closing at the failed boot rolls the
// transaction back — so the next boot can run the whole file again. The
// failure is forced on a statement in the middle, after the Linear tables, the
// conversation rewrite and the settings rewrite have run inside it.
func TestMigrate_ProviderScopesFailureCommitsNothing(t *testing.T) {
	database := openMigrationsTestDBAt(t, TestDSNMemory, beforeProviderScopes)
	seedProviderScopesOrg(t, database)
	setOrgBaseURL(t, database, runmode.LocalDefaultOrgID, "github", "HTTPS://GHE.Example.com/")
	seedCompletedConversation(t, database, "c1", "2026-01-02 03:04:05", nil, nil)
	mustExec(t, database, `CREATE TABLE entities_new (id TEXT)`)

	treeFS, dir, err := migrationsFor("sqlite3")
	if err != nil {
		t.Fatalf("migrationsFor: %v", err)
	}
	gooseMu.Lock()
	goose.SetBaseFS(treeFS)
	upErr := goose.SetDialect("sqlite3")
	if upErr == nil {
		upErr = goose.UpTo(database, dir, providerScopesVersion)
	}
	gooseMu.Unlock()
	if upErr == nil || !strings.Contains(upErr.Error(), "entities_new") {
		t.Fatalf("goose.UpTo = %v, want the forced failure on entities_new", upErr)
	}

	// What closing the connection does.
	if _, err := database.Exec(`ROLLBACK`); err != nil {
		t.Fatalf("ROLLBACK: %v — the failure left no transaction open, so the statements before it committed", err)
	}
	if got := queryString(t, database, `SELECT status FROM conversations WHERE id = 'c1'`); got != "completed" {
		t.Errorf("conversation status = %q after the failed migration, want completed", got)
	}
	if got := queryString(t, database, `SELECT base_url FROM org_event_sources WHERE kind = 'github'`); got != "HTTPS://GHE.Example.com/" {
		t.Errorf("github base_url = %q after the failed migration, want it unchanged", got)
	}
	if n := queryInt(t, database, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'user_linear_identities'`); n != 0 {
		t.Error("user_linear_identities exists after the failed migration")
	}
	if n := queryInt(t, database, `SELECT COUNT(*) FROM goose_db_version WHERE version_id = ?`, providerScopesVersion); n != 0 {
		t.Error("the failed migration was recorded as applied")
	}

	mustExec(t, database, `DROP TABLE entities_new`)
	migrateProviderScopes(t, database)
	if got := queryString(t, database, `SELECT status FROM conversations WHERE id = 'c1'`); got != "open" {
		t.Errorf("conversation status = %q after the retry, want open", got)
	}
	if n := queryInt(t, database, `PRAGMA foreign_keys`); n != 1 {
		t.Error("foreign keys are off after the migration")
	}
}

// seedProviderScopesOrg stages the local org, one team and one user; a migrated
// database carries none of them until boot seeds them.
// The migration gives every GitHub entity the registry row of its repository,
// minting the rows that do not exist yet, and then requires one on every GitHub
// row. Its failure modes are quiet: two rows minted for one repository spelled
// two ways, a row minted when one already answers to the name, or a row that
// tracking would then read as tracked. So this stages each case in the schema
// the migration starts from, migrates, and reads every row back.
// TestMigrate_ProviderScopesMatchGo pins that the row resolves on the host the
// entity is scoped under, for every spelling of the org's GitHub URL.
func TestMigrate_ProviderScopesGiveGitHubEntitiesTheirRepository(t *testing.T) {
	database := openMigrationsTestDBAt(t, TestDSNMemory, beforeProviderScopes)
	seedProviderScopesOrg(t, database)
	for _, stmt := range []string{
		// A repository a team tracks, already in the registry.
		`INSERT INTO repositories (id, owner, repo, source) VALUES ('repo-tracked', 'octo', 'tracked', 'github')`,
		`INSERT INTO team_github_repos (team_id, repository_id, org_id) VALUES ('team-1', 'repo-tracked', '` + runmode.LocalDefaultOrgID + `')`,

		// A pull request in it, its key spelled in another casing: it takes
		// the existing row rather than minting a second.
		`INSERT INTO entities (id, source, source_id, kind, created_at)
		 VALUES ('e-tracked', 'github', 'Octo/Tracked#1', 'pr', '2026-01-01 00:00:00')`,

		// Two pull requests in a repository with no row, spelled two ways:
		// one row is minted, carrying the newer key's casing.
		`INSERT INTO entities (id, source, source_id, kind, created_at)
		 VALUES ('e-new-old', 'github', 'acme/widgets#2', 'pr', '2026-01-01 00:00:00')`,
		`INSERT INTO entities (id, source, source_id, kind, created_at, state, closed_at)
		 VALUES ('e-new-new', 'github', 'Acme/Widgets#3', 'pr', '2026-02-01 00:00:00', 'closed', '2026-02-02 00:00:00')`,

		// Other sources reference no repository.
		`INSERT INTO entities (id, source, source_id, kind) VALUES ('e-jira', 'jira', 'OCTO-1', 'issue')`,
	} {
		mustExec(t, database, stmt)
	}

	migrateProviderScopes(t, database)

	repositoryOf := func(entityID string) string {
		t.Helper()
		var id sql.NullString
		if err := database.QueryRow(`SELECT repository_id FROM entities WHERE id = ?`, entityID).Scan(&id); err != nil {
			t.Fatalf("read repository_id of %s: %v", entityID, err)
		}
		return id.String
	}

	if got := repositoryOf("e-tracked"); got != "repo-tracked" {
		t.Errorf("e-tracked references %q, want the existing row repo-tracked", got)
	}

	var widgetsID, owner, repo, host string
	if err := database.QueryRow(`
		SELECT id, owner, repo, host FROM repositories
		 WHERE LOWER(owner) = 'acme' AND LOWER(repo) = 'widgets'
	`).Scan(&widgetsID, &owner, &repo, &host); err != nil {
		t.Fatalf("registry row for acme/widgets: %v", err)
	}
	if owner != "Acme" || repo != "Widgets" {
		t.Errorf("minted row is %s/%s, want the newer key's casing Acme/Widgets", owner, repo)
	}
	if want := queryString(t, database, `SELECT scope FROM entities WHERE id = 'e-new-old'`); host != want {
		t.Errorf("minted row is on %q, want the host its entities are scoped under, %q", host, want)
	}
	for _, e := range []string{"e-new-old", "e-new-new"} {
		if got := repositoryOf(e); got != widgetsID {
			t.Errorf("%s references %q, want the one minted row %s", e, got, widgetsID)
		}
	}
	if got := repositoryOf("e-jira"); got != "" {
		t.Errorf("e-jira references %q, want no repository", got)
	}
	if n := queryString(t, database, `SELECT COUNT(*) FROM repositories`); n != "2" {
		t.Errorf("registry holds %s rows, want 2: the tracked row and the one minted", n)
	}
	if n := queryString(t, database, `SELECT COUNT(*) FROM team_github_repos WHERE repository_id = ?`, widgetsID); n != "0" {
		t.Errorf("the minted row is tracked by %s teams, want none", n)
	}

	refused := func(what, stmt string) {
		t.Helper()
		_, err := database.Exec(stmt)
		if err == nil || !strings.Contains(err.Error(), "entities_repository_required") {
			t.Errorf("%s = %v, want the entities_repository_required CHECK to refuse it", what, err)
		}
	}
	refused("a github insert with no repository",
		`INSERT INTO entities (id, source, scope, source_id, kind) VALUES ('e-bare', 'github', 'https://github.com', 'octo/tracked#5', 'pr')`)
	refused("clearing a github row's repository",
		`UPDATE entities SET repository_id = NULL WHERE id = 'e-tracked'`)
	refused("turning a row into a github row with no repository",
		`UPDATE entities SET source = 'github' WHERE id = 'e-jira'`)

	mustExec(t, database, `INSERT INTO entities (id, source, scope, source_id, kind, repository_id)
		VALUES ('e-ok', 'github', 'https://github.com', 'octo/tracked#6', 'pr', 'repo-tracked')`)
	mustExec(t, database, `INSERT INTO jira_projects (id, org_id, site, external_id, project_key)
		VALUES ('jp-octo', ?, 'https://jira.example.com', '10000', 'OCTO')`, runmode.LocalDefaultOrgID)
	mustExec(t, database, `INSERT INTO entities (id, source, scope, source_id, external_id, kind, jira_project_id)
		VALUES ('e-jira-2', 'jira', 'https://jira.example.com', 'OCTO-2', '10002', 'issue', 'jp-octo')`)
	if _, err := database.Exec(`INSERT INTO entities (id, source, scope, source_id, kind, repository_id)
		VALUES ('e-ghost', 'github', 'https://github.com', 'ghost/repo#1', 'pr', 'no-such-repository')`); err == nil {
		t.Error("a github insert naming a repository row that does not exist succeeded; the foreign key should refuse it")
	}
}

// Every GitHub row with a snapshot takes its pull request's node id as its
// external id. A stub, a snapshot with no node id and a malformed snapshot
// stay NULL. Two rows for one pull request, created under two names of its
// repository, would break the identity index: the older keeps the id and the
// newer stays NULL. A Jira row takes its issue id, never a node id.
func TestMigrate_ProviderScopesRecordGitHubNodeIDs(t *testing.T) {
	database := openMigrationsTestDBAt(t, TestDSNMemory, beforeProviderScopes)
	seedProviderScopesOrg(t, database)
	for _, stmt := range []string{
		`INSERT INTO entities (id, source, source_id, kind, snapshot_json, created_at)
		 VALUES ('e-polled', 'github', 'octo/repo#1', 'pr', '{"node_id":"PR_1","number":1,"repo":"octo/repo"}', '2026-01-01 00:00:00')`,
		`INSERT INTO entities (id, source, source_id, kind, created_at)
		 VALUES ('e-stub', 'github', 'octo/repo#2', 'pr', '2026-01-01 00:00:00')`,
		`INSERT INTO entities (id, source, source_id, kind, snapshot_json, created_at)
		 VALUES ('e-no-node', 'github', 'octo/repo#3', 'pr', '{"number":3}', '2026-01-01 00:00:00')`,
		`INSERT INTO entities (id, source, source_id, kind, snapshot_json, created_at)
		 VALUES ('e-malformed', 'github', 'octo/repo#4', 'pr', '{not json', '2026-01-01 00:00:00')`,

		// One pull request under its repository's old and new names.
		`INSERT INTO entities (id, source, source_id, kind, snapshot_json, created_at)
		 VALUES ('e-old-name', 'github', 'octo/old#7', 'pr', '{"node_id":"PR_7","number":7,"repo":"octo/old"}', '2026-01-01 00:00:00')`,
		`INSERT INTO entities (id, source, source_id, kind, snapshot_json, created_at)
		 VALUES ('e-new-name', 'github', 'octo/new#7', 'pr', '{"node_id":"PR_7","number":7,"repo":"octo/new"}', '2026-02-01 00:00:00')`,

		`INSERT INTO entities (id, source, source_id, kind, snapshot_json)
		 VALUES ('e-jira', 'jira', 'OCTO-1', 'issue', '{"node_id":"PR_1","id":"10001"}')`,
	} {
		mustExec(t, database, stmt)
	}

	migrateProviderScopes(t, database)

	for entityID, want := range map[string]string{
		"e-polled":    "PR_1",
		"e-stub":      "",
		"e-no-node":   "",
		"e-malformed": "",
		"e-old-name":  "PR_7",
		"e-new-name":  "",
		"e-jira":      "10001",
	} {
		var got sql.NullString
		if err := database.QueryRow(`SELECT external_id FROM entities WHERE id = ?`, entityID).Scan(&got); err != nil {
			t.Fatalf("read external_id of %s: %v", entityID, err)
		}
		if got.String != want {
			t.Errorf("%s external_id = %q, want %q", entityID, got.String, want)
		}
	}

	// The identity index holds afterwards: a second row for PR_7 is refused.
	if _, err := database.Exec(`UPDATE entities SET external_id = 'PR_7' WHERE id = 'e-new-name'`); err == nil {
		t.Error("a second row carrying PR_7 was accepted; the identity index should refuse it")
	}
}

// Every Jira row whose snapshot names its issue's id takes it as its external
// id. A snapshot written before the poller recorded one, a stub and a
// malformed snapshot stay NULL. Two rows naming one id on one site would break
// the identity index, so the older keeps it; one id on two sites is two
// issues. From then on a Jira or Linear row is not written without its id and
// does not lose the one it has, while a row the migration left without one is
// still written to, and learns its id.
func TestMigrate_ProviderScopesRecordJiraIssueIDs(t *testing.T) {
	database := openMigrationsTestDBAt(t, TestDSNMemory, beforeProviderScopes)
	seedProviderScopesOrg(t, database)
	const site, otherSite = "https://acme.atlassian.net", "https://other.atlassian.net"
	setOrgBaseURL(t, database, runmode.LocalDefaultOrgID, "jira", site)
	issue := func(id, onSite, key, snapshot, createdAt string) {
		t.Helper()
		mustExec(t, database, `
			INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json, created_at)
			VALUES (?, 'jira', ?, 'issue', 't', ?, NULLIF(?, ''), ?)`,
			id, key, onSite+"/browse/"+key, snapshot, createdAt)
	}
	issue("e-polled", site, "SKY-1", `{"id":"10001","key":"SKY-1","project_id":"100"}`, "2026-01-01 00:00:00")
	issue("e-no-id", site, "SKY-2", `{"key":"SKY-2","status":"To Do"}`, "2026-01-01 00:00:00")
	issue("e-stub", site, "SKY-3", "", "2026-01-01 00:00:00")
	issue("e-malformed", site, "SKY-4", `{not json`, "2026-01-01 00:00:00")
	// One issue under the key it had before a move and the key it has now.
	issue("e-before-move", site, "SKY-7", `{"id":"10007","key":"SKY-7"}`, "2026-01-01 00:00:00")
	issue("e-after-move", site, "OPS-1", `{"id":"10007","key":"OPS-1"}`, "2026-02-01 00:00:00")
	issue("e-other-site", otherSite, "OTH-1", `{"id":"10001","key":"OTH-1"}`, "2026-01-01 00:00:00")

	migrateProviderScopes(t, database)

	for entityID, want := range map[string]string{
		"e-polled":      "10001",
		"e-no-id":       "",
		"e-stub":        "",
		"e-malformed":   "",
		"e-before-move": "10007",
		"e-after-move":  "",
		"e-other-site":  "10001",
	} {
		var got sql.NullString
		if err := database.QueryRow(`SELECT external_id FROM entities WHERE id = ?`, entityID).Scan(&got); err != nil {
			t.Fatalf("read external_id of %s: %v", entityID, err)
		}
		if got.String != want {
			t.Errorf("%s external_id = %q, want %q", entityID, got.String, want)
		}
	}

	refused := func(what, stmt string) {
		t.Helper()
		_, err := database.Exec(stmt)
		if err == nil || !strings.Contains(err.Error(), "entities_external_id_required") {
			t.Errorf("%s = %v, want the entities_external_id_required trigger to refuse it", what, err)
		}
	}
	project := queryString(t, database, `SELECT jira_project_id FROM entities WHERE id = 'e-polled'`)
	refused("a jira insert with no issue id",
		`INSERT INTO entities (id, source, scope, source_id, kind, jira_project_id)
		 VALUES ('e-bare', 'jira', '`+site+`', 'SKY-9', 'issue', '`+project+`')`)
	refused("a jira insert with an empty issue id",
		`INSERT INTO entities (id, source, scope, source_id, external_id, kind, jira_project_id)
		 VALUES ('e-empty', 'jira', '`+site+`', 'SKY-9', '', 'issue', '`+project+`')`)
	refused("a linear insert with no issue id",
		`INSERT INTO entities (id, source, scope, source_id, kind, linear_team_id)
		 VALUES ('e-linear', 'linear', 'workspace-1', 'ENG-1', 'issue', 'team-1')`)
	refused("clearing a jira row's issue id", `UPDATE entities SET external_id = NULL WHERE id = 'e-polled'`)
	refused("emptying a jira row's issue id", `UPDATE entities SET external_id = '' WHERE id = 'e-polled'`)

	mustExec(t, database, `INSERT INTO entities (id, source, scope, source_id, external_id, kind, jira_project_id)
		VALUES ('e-new', 'jira', ?, 'SKY-9', '10009', 'issue', ?)`, site, project)
	mustExec(t, database, `INSERT INTO entities (id, source, scope, source_id, external_id, kind, linear_team_id)
		VALUES ('e-linear', 'linear', 'workspace-1', 'ENG-1', 'uuid-1', 'issue', 'team-1')`)
	mustExec(t, database, `INSERT INTO entities (id, source, scope, source_id, kind) VALUES ('e-slack', 'slack', 'slack.com', 'C1/1.0', 'thread')`)

	// A row left without an id is written to like any other, and learns it.
	mustExec(t, database, `UPDATE entities SET external_id = COALESCE(external_id, NULL), poll_seq = poll_seq + 1 WHERE id = 'e-stub'`)
	mustExec(t, database, `UPDATE entities SET state = 'closed', closed_at = CURRENT_TIMESTAMP WHERE id = 'e-stub'`)
	mustExec(t, database, `UPDATE entities SET external_id = '10002' WHERE id = 'e-no-id'`)
}

// Pull request, review and branch deliverables are re-keyed on ids: a pull
// request on the node id its row's details carry, a review on its pull
// request's node id, read off the pull request's deliverable or its entity, and a
// branch on its repository's registry row, names compared case-insensitively.
// A row with nothing to resolve keeps its key, and one pull request recorded
// under two names of its repository merges onto the older row.
func TestMigrate_ProviderScopesKeyDeliverablesOnIDs(t *testing.T) {
	database := openMigrationsTestDBAt(t, TestDSNMemory, beforeProviderScopes)
	seedProviderScopesOrg(t, database)
	mustExec(t, database, `INSERT INTO repositories (id, owner, repo, source) VALUES ('repo-api', 'octo', 'api', 'github')`)
	// A polled pull request, whose entity takes its node id from its snapshot.
	mustExec(t, database, `INSERT INTO entities (id, source, source_id, kind, snapshot_json)
		VALUES ('e-pr-9', 'github', 'octo/api#9', 'pr', '{"node_id":"PR_9","number":9}')`)
	deliverable := func(id, provider, kind, target, externalID, key, details, createdAt string) {
		t.Helper()
		mustExec(t, database, `
			INSERT INTO artifacts (id, team_id, provider, kind, target, external_id, state, dedup_key, details_json, created_at)
			VALUES (?, 'team-1', ?, ?, ?, NULLIF(?, ''), 'open', ?, NULLIF(?, ''), ?)`,
			id, provider, kind, target, externalID, key, details, createdAt)
	}
	const day1, day2 = "2026-01-01 00:00:00", "2026-02-01 00:00:00"
	deliverable("pr-3", "github", "pull_request", "octo/api#3", "3", "github:pull_request:octo/api#3", `{"node_id":"PR_3"}`, day1)
	// One pull request recorded under its repository's old name and its new.
	deliverable("pr-7-old", "github", "pull_request", "octo/old-api#7", "7", "github:pull_request:octo/old-api#7", `{"node_id":"PR_7"}`, day1)
	deliverable("pr-7-new", "github", "pull_request", "octo/api#7", "7", "github:pull_request:octo/api#7", `{"node_id":"PR_7"}`, day2)
	deliverable("pr-5", "github", "pull_request", "octo/api#5", "5", "github:pull_request:octo/api#5", `{"head_branch":"x"}`, day1)
	deliverable("rv-3", "github", "review", "Octo/API#3", "", "github:review:Octo/API#3:conv-a", `{"number":3}`, day1)
	deliverable("rv-9", "github", "review", "octo/api#9", "", "github:review:octo/api#9:conv-b", `{"number":9}`, day1)
	deliverable("rv-11", "github", "review", "octo/api#11", "", "github:review:octo/api#11:conv-c", `{"number":11}`, day1)
	deliverable("br-api", "git", "branch", "Octo/API", "refs/heads/octo/api-fix", "git:branch:Octo/API:refs/heads/octo/api-fix", `{"sha":"a"}`, day1)
	deliverable("br-gone", "git", "branch", "octo/gone", "refs/heads/fix", "git:branch:octo/gone:refs/heads/fix", `{"sha":"b"}`, day1)

	migrateProviderScopes(t, database)

	for id, want := range map[string]string{
		"pr-3":     "github:pull_request:PR_3",
		"pr-7-old": "github:pull_request:PR_7",
		"pr-5":     "github:pull_request:octo/api#5",
		"rv-3":     "github:review:PR_3:conv-a",
		"rv-9":     "github:review:PR_9:conv-b",
		"rv-11":    "github:review:octo/api#11:conv-c",
		"br-api":   "git:branch:repo-api:refs/heads/octo/api-fix",
		"br-gone":  "git:branch:octo/gone:refs/heads/fix",
	} {
		if got := queryString(t, database, `SELECT dedup_key FROM artifacts WHERE id = ?`, id); got != want {
			t.Errorf("%s dedup_key = %q, want %q", id, got, want)
		}
	}
	if n := queryInt(t, database, `SELECT COUNT(*) FROM artifacts WHERE id = 'pr-7-new'`); n != 0 {
		t.Error("pr-7-new survived; one pull request recorded under two names merges onto the older row")
	}
	for id, want := range map[string]string{"pr-3": "PR_3", "pr-7-old": "PR_7"} {
		if got := queryString(t, database, `SELECT COALESCE(node_id, '') FROM artifacts WHERE id = ?`, id); got != want {
			t.Errorf("%s node_id = %q, want %q", id, got, want)
		}
	}
	// The keys are the ones the Go side builds.
	for id, want := range map[string]string{
		"pr-3":   domain.PullRequestDedupKey("PR_3"),
		"rv-3":   domain.ReviewDedupKey("PR_3", "conv-a"),
		"br-api": "git:branch:repo-api:refs/heads/octo/api-fix",
	} {
		if got := queryString(t, database, `SELECT dedup_key FROM artifacts WHERE id = ?`, id); got != want {
			t.Errorf("%s dedup_key = %q, want the Go key %q", id, got, want)
		}
	}
}

// Every deliverable is linked to the entity it is about: the one carrying the
// provider id its key names, else the one its target names, and its target
// takes that entity's key. A branch has none. A Jira deliverable keyed on an
// issue key, or a Jira comment keyed on its comment id alone, is re-keyed on
// the issue's site and id, and one issue recorded under both shapes keeps the
// older row.
func TestMigrate_ProviderScopesLinkDeliverablesToEntities(t *testing.T) {
	database := openMigrationsTestDBAt(t, TestDSNMemory, beforeProviderScopes)
	seedProviderScopesOrg(t, database)
	const site = "https://acme.atlassian.net"
	setOrgBaseURL(t, database, runmode.LocalDefaultOrgID, "jira", site)
	mustExec(t, database, `INSERT INTO entities (id, source, source_id, kind, snapshot_json)
		VALUES ('e-pr-9', 'github', 'octo/api#9', 'pr', '{"node_id":"PR_9","number":9}')`)
	jiraIssue := func(id, key, snapshot string) {
		t.Helper()
		mustExec(t, database, `
			INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json)
			VALUES (?, 'jira', ?, 'issue', 't', ?, NULLIF(?, ''))`,
			id, key, site+"/browse/"+key, snapshot)
	}
	jiraIssue("e-sky-1", "SKY-1", `{"id":"10001","key":"SKY-1","project_id":"100"}`)
	jiraIssue("e-sky-2", "SKY-2", `{"key":"SKY-2"}`)
	jiraIssue("e-ops-1", "OPS-1", `{"id":"10007","key":"OPS-1","project_id":"200"}`)
	deliverable := func(id, provider, kind, target, key, details, createdAt string) {
		t.Helper()
		mustExec(t, database, `
			INSERT INTO artifacts (id, team_id, provider, kind, target, state, dedup_key, details_json, created_at)
			VALUES (?, 'team-1', ?, ?, ?, 'open', ?, NULLIF(?, ''), ?)`,
			id, provider, kind, target, key, details, createdAt)
	}
	const day1, day2 = "2026-01-01 00:00:00", "2026-02-01 00:00:00"
	resource := domain.JiraIssueResource(site, "10001")
	deliverable("pr-9", "github", "pull_request", "octo/api#9", "github:pull_request:octo/api#9", `{"node_id":"PR_9"}`, day1)
	deliverable("gh-comment", "github", "comment", "Octo/API#9", "github:comment:555", "", day1)
	deliverable("branch", "git", "branch", "octo/gone", "git:branch:octo/gone:refs/heads/fix", "", day1)
	deliverable("issue-legacy", "jira", "issue", "SKY-1", "jira:issue:SKY-1", "", day1)
	deliverable("issue-keyed", "jira", "issue", "SKY-1", domain.ObjectKey("jira", "issue", resource, ""), "", day2)
	deliverable("comment-legacy", "jira", "comment", "SKY-1", "jira:comment:20311", "", day1)
	deliverable("comment-no-id", "jira", "comment", "SKY-2", "jira:comment:20312", "", day1)
	// Recorded under the key the issue had before it moved, keyed on its id.
	deliverable("issue-moved", "jira", "issue", "SKY-7",
		domain.ObjectKey("jira", "issue", domain.JiraIssueResource(site, "10007"), ""), "", day1)

	migrateProviderScopes(t, database)

	for id, want := range map[string]struct{ entity, target, key string }{
		"pr-9":           {"e-pr-9", "octo/api#9", domain.PullRequestDedupKey("PR_9")},
		"gh-comment":     {"e-pr-9", "octo/api#9", "github:comment:555"},
		"branch":         {"", "octo/gone", "git:branch:octo/gone:refs/heads/fix"},
		"issue-legacy":   {"e-sky-1", "SKY-1", domain.ObjectKey("jira", "issue", resource, "")},
		"comment-legacy": {"e-sky-1", "SKY-1", domain.ObjectKey("jira", "comment", resource, "20311")},
		"comment-no-id":  {"e-sky-2", "SKY-2", "jira:comment:20312"},
		"issue-moved":    {"e-ops-1", "OPS-1", domain.ObjectKey("jira", "issue", domain.JiraIssueResource(site, "10007"), "")},
	} {
		var entity, target, key string
		if err := database.QueryRow(`SELECT COALESCE(entity_id, ''), target, dedup_key FROM artifacts WHERE id = ?`, id).
			Scan(&entity, &target, &key); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if entity != want.entity || target != want.target || key != want.key {
			t.Errorf("%s = (entity %q, target %q, key %q), want (%q, %q, %q)", id, entity, target, key, want.entity, want.target, want.key)
		}
	}
	if n := queryInt(t, database, `SELECT COUNT(*) FROM artifacts WHERE id = 'issue-keyed'`); n != 0 {
		t.Error("issue-keyed survived; one issue recorded under its key and its id keeps the older row")
	}
	assertNoDanglingReferences(t, database)
}

// Each existing audit-ledger row is linked to the entity its target names,
// case-insensitively, or else to the one whose url its link continues (the
// link a rename of a moved issue left it with); a GitHub row about no entity is
// linked to the registry row its owner/repo names; and a row nothing answers to
// stays unlinked. A url match needs a boundary after the entity's url, so
// SKY-12's link is not SKY-1's.
func TestMigrate_ProviderScopesLinkAuditLedgerRows(t *testing.T) {
	database := openMigrationsTestDBAt(t, TestDSNMemory, beforeProviderScopes)
	seedProviderScopesOrg(t, database)
	const site = "https://acme.atlassian.net"
	setOrgBaseURL(t, database, runmode.LocalDefaultOrgID, "jira", site)
	mustExec(t, database, `INSERT INTO entities (id, source, source_id, kind, url, snapshot_json) VALUES
		('e-pr-9', 'github', 'octo/api#9', 'pr', 'https://github.com/octo/api/pull/9', '{"node_id":"PR_9","number":9}'),
		('e-sky-1', 'jira', 'SKY-1', 'issue', ?, '{"id":"10001","key":"SKY-1","project_id":"100"}')`, site+"/browse/SKY-1")
	mustExec(t, database, `INSERT INTO repositories (id, owner, repo, source) VALUES ('r-web', 'octo', 'web', 'github')`)
	// want is entity_id|repository_id.
	cases := []struct{ id, provider, target, url, currentURL, want string }{
		{"x-pr", "github", "Octo/API#9", "https://github.com/octo/api/pull/9#issuecomment-1", "", "e-pr-9|"},
		{"x-key", "jira", "SKY-1", site + "/browse/SKY-1", "", "e-sky-1|"},
		{"x-moved", "jira", "OLD-1", site + "/browse/OLD-1", site + "/browse/SKY-1?focusedCommentId=5", "e-sky-1|"},
		{"x-prefix", "jira", "SKY-12", site + "/browse/SKY-12", "", "|"},
		{"x-branch", "github", "octo/web", "https://github.com/octo/web/tree/fix", "", "|r-web"},
		{"x-pr-unrecorded", "github", "Octo/Web#3", "https://github.com/octo/web/pull/3", "", "|r-web"},
		{"x-gone", "github", "octo/gone", "", "", "|"},
		{"x-egress", "network", "evil.example:443", "", "", "|"},
	}
	for _, c := range cases {
		mustExec(t, database, `
			INSERT INTO external_actions (id, provider, action, target, url, current_url, credential, dedup_key)
			VALUES (?, ?, 'comment_posted', ?, NULLIF(?, ''), NULLIF(?, ''), 'github_app', ?)`,
			c.id, c.provider, c.target, c.url, c.currentURL, "k-"+c.id)
	}

	migrateProviderScopes(t, database)

	for _, c := range cases {
		if got := queryString(t, database, `SELECT COALESCE(entity_id, '') || '|' || COALESCE(repository_id, '')
		                                     FROM external_actions WHERE id = ?`, c.id); got != c.want {
			t.Errorf("%s references %q, want %q", c.id, got, c.want)
		}
	}
	assertNoDanglingReferences(t, database)
}

// A GitHub predicate's login-valued identity filters become the id-valued
// ones the matcher reads: each login replaced by the id of the identity
// binding that holds it on the org's GitHub, a login no binding with an id
// holds kept as written (it matches nothing, as a filter must not widen), a
// singular login folded into its list, and a team's org/slug kept, since the
// copied mappings have no ids yet. Every rewritten predicate decodes under the
// strict schema; a predicate with nothing to rewrite, and a Jira one, are left
// as they were.
func TestMigrate_GitHubPredicatesNameIDs(t *testing.T) {
	database := openMigrationsTestDBAt(t, TestDSNMemory, beforeProviderScopes)
	seedProviderScopesOrg(t, database)
	setOrgBaseURL(t, database, runmode.LocalDefaultOrgID, "github", "https://github.com")
	mustExec(t, database, `INSERT INTO users (id) VALUES ('u2'), ('u3')`)
	for _, identity := range [][]any{
		{"u1", "https://github.com", "Aidan", "583231"},
		{"u2", "https://github.com", "hubot", nil},
		{"u3", "https://ghe.corp.example", "alice", "77"},
	} {
		mustExec(t, database, `
			INSERT INTO user_github_identities (user_id, github_base_url, login, github_user_id, source, verified_at)
			VALUES (?, ?, ?, ?, 'pat', CURRENT_TIMESTAMP)`, identity...)
	}

	cases := []struct {
		id, eventType, before, after string
	}{
		{"list", domain.EventGitHubPRCICheckFailed, `{"author_in":["aidan","hubot","alice"],"repo_in":["r1"]}`,
			`{"repo_in":["r1"],"author_id_in":["583231","hubot","alice"]}`},
		{"empty-list", domain.EventGitHubPRCICheckFailed, `{"author_in":[]}`, `{"author_id_in":[]}`},
		{"singular", domain.EventGitHubPRReviewApproved, `{"reviewer":"AIDAN"}`, `{"reviewer_id_in":["583231"]}`},
		{"singular-narrows", domain.EventGitHubPRReviewApproved, `{"reviewer_in":["aidan","bob"],"reviewer":"aidan"}`,
			`{"reviewer_id_in":["583231"]}`},
		{"singular-outside-its-list", domain.EventGitHubPRNewCommits, `{"author_in":["bob"],"author":"aidan"}`,
			`{"author_id_in":["0"]}`},
		{"requested", domain.EventGitHubPRReviewRequested, `{"requested_login":"Aidan","requested_team":"acme/backend"}`,
			`{"requested_user_id_in":["583231"],"requested_team_id_in":["acme/backend"]}`},
		{"commented", domain.EventGitHubPRCommented, `{"commenter_in":["aidan"]}`, `{"commenter_id_in":["583231"]}`},
		{"mentioned", domain.EventGitHubPRMentioned, `{"commenter":"aidan","mentioned_login":"bob","author":null}`,
			`{"mentioned_login":"bob","commenter_id_in":["583231"]}`},
		{"untouched", domain.EventGitHubPROpened, `{"repo_in":["r1"]}`, `{"repo_in":["r1"]}`},
		{"jira", domain.EventJiraIssueCommented, `{"commenter_in":["557058:ada"]}`, `{"commenter_in":["557058:ada"]}`},
		{"match-all", domain.EventGitHubPRCICheckFailed, "", ""},
	}
	for _, c := range cases {
		mustExec(t, database, `INSERT OR IGNORE INTO events_catalog (id, source, category, label, description)
			VALUES (?, 'test', 'test', ?, '')`, c.eventType, c.eventType)
		mustExec(t, database, `
			INSERT INTO event_handlers (id, team_id, creator_user_id, kind, event_type, scope_predicate_json,
			                            source, name, default_priority, sort_order)
			VALUES (?, 'team-1', 'u1', 'rule', ?, NULLIF(?, ''), 'user', ?, 0.5, 0)`,
			c.id, c.eventType, c.before, c.id)
	}

	migrateProviderScopes(t, database)

	for _, c := range cases {
		got := queryString(t, database, `SELECT COALESCE(scope_predicate_json, '') FROM event_handlers WHERE id = ?`, c.id)
		if got != c.after {
			t.Errorf("%s = %s, want %s", c.id, got, c.after)
		} else if got != "" {
			if _, err := events.ValidatePredicateJSON(c.eventType, got); err != nil {
				t.Errorf("%s: the rewritten predicate does not decode: %v", c.id, err)
			}
		}
	}
}

// A database seeded by an earlier build carries a catalog row for
// system:task:delegation_blocked_by_subtasks, which db.SeedEventTypes only
// upserts and so never removes. The migration deletes the handlers on it,
// soft-deleted ones included, and then the row; a handler on another type
// stays.
func TestMigrate_ProviderScopesDropDelegationBlockedEventType(t *testing.T) {
	const blocked = "system:task:delegation_blocked_by_subtasks"
	database := openMigrationsTestDBAt(t, TestDSNMemory, beforeProviderScopes)
	seedProviderScopesOrg(t, database)
	for _, eventType := range []string{blocked, domain.EventGitHubPROpened} {
		mustExec(t, database, `INSERT OR IGNORE INTO events_catalog (id, source, category, label, description)
			VALUES (?, 'test', 'test', ?, '')`, eventType, eventType)
	}
	for _, h := range [][]any{
		{"blocked", blocked, nil},
		{"blocked-deleted", blocked, "2026-01-01 00:00:00"},
		{"opened", domain.EventGitHubPROpened, nil},
	} {
		mustExec(t, database, `
			INSERT INTO event_handlers (id, team_id, creator_user_id, kind, event_type, source, name,
			                            default_priority, sort_order, deleted_at)
			VALUES (?, 'team-1', 'u1', 'rule', ?, 'user', ?, 0.5, 0, ?)`, h[0], h[1], h[0], h[2])
	}

	migrateProviderScopes(t, database)

	if n := queryInt(t, database, `SELECT COUNT(*) FROM events_catalog WHERE id = ?`, blocked); n != 0 {
		t.Errorf("%d catalog rows for %s, want 0", n, blocked)
	}
	if n := queryInt(t, database, `SELECT COUNT(*) FROM event_handlers WHERE event_type = ?`, blocked); n != 0 {
		t.Errorf("%d handlers on %s, want 0", n, blocked)
	}
	if got := queryString(t, database, `SELECT id FROM event_handlers WHERE event_type = ?`, domain.EventGitHubPROpened); got != "opened" {
		t.Errorf("handler on %s = %q, want opened kept", domain.EventGitHubPROpened, got)
	}
	assertNoDanglingReferences(t, database)
}

func seedProviderScopesOrg(t *testing.T, database *sql.DB) {
	t.Helper()
	mustExec(t, database, `INSERT INTO orgs (id, slug, name) VALUES (?, 'local', 'Local')`, runmode.LocalDefaultOrgID)
	mustExec(t, database, `INSERT INTO teams (id, org_id, slug, name) VALUES ('team-1', ?, 'core', 'Core')`, runmode.LocalDefaultOrgID)
	mustExec(t, database, `INSERT INTO users (id) VALUES ('u1')`)
}

func setOrgBaseURL(t *testing.T, database *sql.DB, orgID, kind, base string) {
	t.Helper()
	mustExec(t, database, `INSERT INTO org_event_sources (org_id, kind, base_url) VALUES (?, ?, ?)
		ON CONFLICT (org_id, kind) DO UPDATE SET base_url = excluded.base_url`, orgID, kind, base)
}

// seedCompletedConversation writes a conversation the way a terminal write of
// the old build left it.
func seedCompletedConversation(t *testing.T, database *sql.DB, id string, completedAt, endedAt, parkReason any) {
	t.Helper()
	mustExec(t, database, `
		INSERT INTO conversations (id, type, origin, trigger_type, creator_user_id, visibility, team_id,
		                           status, outcome, completed_at, ended_at, ended_reason, park_reason, started_at)
		VALUES (?, 'delegation', 'interactive', 'event', NULL, 'team',
		        '00000000-0000-0000-0000-000000000010', 'completed', 'finish',
		        ?, ?, CASE WHEN ? IS NULL THEN NULL ELSE 'requeued' END, ?, '2026-01-01 00:00:00')
	`, id, completedAt, endedAt, endedAt, parkReason)
}

func migrateProviderScopes(t *testing.T, database *sql.DB) {
	t.Helper()
	treeFS, dir, err := migrationsFor("sqlite3")
	if err != nil {
		t.Fatalf("migrationsFor: %v", err)
	}
	gooseMu.Lock()
	goose.SetBaseFS(treeFS)
	upErr := goose.SetDialect("sqlite3")
	if upErr == nil {
		upErr = goose.UpTo(database, dir, providerScopesVersion)
	}
	gooseMu.Unlock()
	if upErr != nil {
		t.Fatalf("goose.UpTo provider scopes: %v", upErr)
	}
}

// migratedGitHubBase is the GitHub URL an org's migrated rows are keyed
// under: its own, or, with none set, github.com — the GitHub such an org was
// on, and the URL boot gives it when it uses GitHub
// (integrations.AdoptSavedGitHubHost). Go resolves no host for an empty
// setting, so the migration and the adoption are what agree on this one.
func migratedGitHubBase(base string) string {
	if ghbase.CanonicalBaseURL(base) == "" {
		return ghbase.GitHubCom
	}
	return base
}

func assertNoDanglingReferences(t *testing.T, database *sql.DB) {
	t.Helper()
	rows, err := database.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Error("foreign_key_check reports a dangling reference after the migration")
	}
}

func mustExec(t *testing.T, database *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := database.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func queryString(t *testing.T, database *sql.DB, query string, args ...any) string {
	t.Helper()
	var s string
	if err := database.QueryRow(query, args...).Scan(&s); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return s
}

func queryInt(t *testing.T, database *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := database.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func ptr(s string) *string { return &s }
