package db

import (
	"database/sql"
	"encoding/json"
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// seedLegacyJiraRule writes a rule the way the schema before the registry kept
// it: keyed by the project key.
func seedLegacyJiraRule(t *testing.T, database *sql.DB, teamID, key, pickup string) {
	t.Helper()
	mustExec(t, database, `
		INSERT INTO jira_project_status_rules (team_id, project_key, pickup_members, in_progress_members,
			in_progress_canonical, done_members, done_canonical)
		VALUES (?, ?, ?, '[{"id":"2","name":"Doing"}]', '{"id":"2","name":"Doing"}', '[{"id":"3","name":"Done"}]', '{"id":"3","name":"Done"}')`,
		teamID, key, pickup)
}

// seedJiraIssue writes a Jira entity read from site, whose snapshot carries
// projectID (none when empty), polled at polledAt (NULL when nil). The schema
// before the migration has no scope column; the migration takes a Jira row's
// scope from the site its url names.
func seedJiraIssue(t *testing.T, database *sql.DB, id, site, key, projectID string, polledAt any) {
	t.Helper()
	snap := map[string]string{"key": key}
	if projectID != "" {
		snap["project_id"] = projectID
	}
	raw, _ := json.Marshal(snap)
	mustExec(t, database, `
		INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json, last_polled_at)
		VALUES (?, 'jira', ?, 'issue', 't', ?, ?, ?)`, id, key, site+"/browse/"+key, string(raw), polledAt)
}

// registryRow is one jira_projects row as the migration left it.
type registryRow struct {
	id, site, externalID string
}

func registryByKey(t *testing.T, database *sql.DB) map[string]registryRow {
	t.Helper()
	rows, err := database.Query(`SELECT id, site, COALESCE(external_id, ''), project_key FROM jira_projects`)
	if err != nil {
		t.Fatalf("read jira_projects: %v", err)
	}
	defer rows.Close()
	out := map[string]registryRow{}
	for rows.Next() {
		var (
			r   registryRow
			key string
		)
		if err := rows.Scan(&r.id, &r.site, &r.externalID, &key); err != nil {
			t.Fatalf("scan jira_projects: %v", err)
		}
		if _, dup := out[key]; dup {
			t.Errorf("two registry rows for %s", key)
		}
		out[key] = r
	}
	return out
}

// TestMigrate_JiraProjectRegistry: every project a team's rules name gets one
// registry row on the org's Jira site, resolved to the project id its issues'
// snapshots carry; the rules and the team's display order reference the rows;
// and the rules keep everything they held.
func TestMigrate_JiraProjectRegistry(t *testing.T) {
	database := openMigrationsTestDBAt(t, TestDSNMemory, beforeProviderScopes)
	seedProviderScopesOrg(t, database)
	mustExec(t, database, `INSERT INTO teams (id, org_id, slug, name) VALUES ('team-2', ?, 'other', 'Other')`, runmode.LocalDefaultOrgID)
	const base = "Https://Acme.Atlassian.net/"
	setOrgBaseURL(t, database, runmode.LocalDefaultOrgID, "jira", base)
	site := domain.EntityScope("jira", domain.OrgSettings{JiraBaseURL: base})

	seedLegacyJiraRule(t, database, "team-1", "SKY", `[{"id":"1","name":"To Do"}]`)
	seedLegacyJiraRule(t, database, "team-1", "OPS", `[{"id":"1","name":"Open"}]`)
	seedLegacyJiraRule(t, database, "team-1", "OLD", `[{"id":"1","name":"To Do"}]`)
	seedLegacyJiraRule(t, database, "team-1", "NEW", `[{"id":"1","name":"To Do"}]`)
	seedLegacyJiraRule(t, database, "team-2", "SKY", `[{"id":"1","name":"Backlog"}]`)
	mustExec(t, database, `UPDATE team_settings SET jira_projects = '["OLD","SKY","OPS","NEW","GONE"]' WHERE team_id = 'team-1'`)
	mustExec(t, database, `INSERT OR IGNORE INTO team_settings (team_id, jira_projects) VALUES ('team-1', '["OLD","SKY","OPS","NEW","GONE"]')`)

	// SKY's issues carry its id; one on another site carries another project's
	// id under the same key, and one has a snapshot that is not JSON.
	seedJiraIssue(t, database, "e-sky-1", site, "SKY-1", "10000", "2026-09-01 00:00:00")
	seedJiraIssue(t, database, "e-sky-2", site, "SKY-2", "10000", "2026-09-02 00:00:00")
	seedJiraIssue(t, database, "e-sky-far", "https://other.example.com", "SKY-9", "99999", "2026-09-30 00:00:00")
	mustExec(t, database, `INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json)
		VALUES ('e-sky-bad', 'jira', 'SKY-3', 'issue', 't', ?, '{')`, site+"/browse/SKY-3")
	// OLD and NEW are one project: the team re-added it under its new key,
	// and the tracker renamed most of its issues; NEW's evidence is newer.
	seedJiraIssue(t, database, "e-old", site, "OLD-1", "30000", "2026-08-01 00:00:00")
	seedJiraIssue(t, database, "e-new", site, "NEW-1", "30000", "2026-09-01 00:00:00")
	// OPS has no issues, so nothing names its id.

	migrateProviderScopes(t, database)

	reg := registryByKey(t, database)
	want := map[string]string{"SKY": "10000", "OPS": "", "OLD": "", "NEW": "30000"}
	if len(reg) != len(want) {
		t.Fatalf("registry = %+v, want one row each for %v", reg, want)
	}
	for key, externalID := range want {
		r, ok := reg[key]
		if !ok {
			t.Errorf("no registry row for %s", key)
			continue
		}
		if r.site != site {
			t.Errorf("%s registered on %q, want %q (domain.EntityScope)", key, r.site, site)
		}
		if r.externalID != externalID {
			t.Errorf("%s external id = %q, want %q", key, r.externalID, externalID)
		}
	}

	// Each rule moved onto its project's row, org stamped, contents kept; the
	// two teams' SKY rules share the one row.
	type rule struct{ project, org, pickup string }
	rules := map[string]rule{}
	rows, err := database.Query(`SELECT r.team_id, p.project_key, r.jira_project_id, r.org_id, r.pickup_members
		FROM jira_project_status_rules r JOIN jira_projects p ON p.id = r.jira_project_id`)
	if err != nil {
		t.Fatalf("read rules: %v", err)
	}
	for rows.Next() {
		var team, key string
		var r rule
		if err := rows.Scan(&team, &key, &r.project, &r.org, &r.pickup); err != nil {
			t.Fatalf("scan rule: %v", err)
		}
		rules[team+"/"+key] = r
	}
	rows.Close()
	if len(rules) != 5 {
		t.Fatalf("rules = %+v, want the five seeded", rules)
	}
	for k, r := range rules {
		if r.org != runmode.LocalDefaultOrgID {
			t.Errorf("rule %s org = %q, want the team's", k, r.org)
		}
	}
	if rules["team-1/SKY"].project != reg["SKY"].id || rules["team-2/SKY"].project != reg["SKY"].id {
		t.Errorf("SKY rules reference %q and %q, want both on %q",
			rules["team-1/SKY"].project, rules["team-2/SKY"].project, reg["SKY"].id)
	}
	if rules["team-2/SKY"].pickup != `[{"id":"1","name":"Backlog"}]` {
		t.Errorf("team-2's SKY pickup = %s, want it carried", rules["team-2/SKY"].pickup)
	}

	var order []string
	if err := json.Unmarshal([]byte(queryString(t, database, `SELECT jira_projects FROM team_settings WHERE team_id = 'team-1'`)), &order); err != nil {
		t.Fatalf("decode order: %v", err)
	}
	if wantOrder := []string{reg["OLD"].id, reg["SKY"].id, reg["OPS"].id, reg["NEW"].id}; !slices.Equal(order, wantOrder) {
		t.Errorf("team-1 order = %v, want the registry ids of OLD, SKY, OPS, NEW in that order", order)
	}

	// The rules now reference the registry, and the schema holds them to it.
	if _, err := database.Exec(`INSERT INTO jira_project_status_rules (team_id, jira_project_id, org_id) VALUES ('team-1', 'no-such-row', ?)`, runmode.LocalDefaultOrgID); err == nil {
		t.Error("a rule naming no registry row was stored")
	}
	if _, err := database.Exec(`INSERT INTO jira_projects (id, org_id, site, project_key) VALUES ('dup', ?, ?, 'OPS')`, runmode.LocalDefaultOrgID, site); err == nil {
		t.Error("a second unresolved row for OPS was stored")
	}
	assertNoDanglingReferences(t, database)
}

// TestMigrate_JiraProjectRegistryEvidenceWithoutAPollTime: an issue row can
// carry no poll time. When two keys' evidence names one project id, the id
// still lands on exactly one registry row, so the identity index holds and the
// migration completes: a key with a poll time wins over one without, and the
// lower key wins when neither has one.
func TestMigrate_JiraProjectRegistryEvidenceWithoutAPollTime(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		oldPolled, newPolled any
		resolved, unresolved string
	}{
		{name: "one has a poll time", oldPolled: "2026-09-01 00:00:00", newPolled: nil, resolved: "OLD", unresolved: "NEW"},
		{name: "neither has one", oldPolled: nil, newPolled: nil, resolved: "NEW", unresolved: "OLD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := openMigrationsTestDBAt(t, TestDSNMemory, beforeProviderScopes)
			seedProviderScopesOrg(t, database)
			const site = "https://acme.atlassian.net"
			setOrgBaseURL(t, database, runmode.LocalDefaultOrgID, "jira", site)
			seedLegacyJiraRule(t, database, "team-1", "OLD", `[]`)
			seedLegacyJiraRule(t, database, "team-1", "NEW", `[]`)
			seedJiraIssue(t, database, "e-old", site, "OLD-1", "30000", tc.oldPolled)
			seedJiraIssue(t, database, "e-new", site, "NEW-1", "30000", tc.newPolled)

			migrateProviderScopes(t, database)

			reg := registryByKey(t, database)
			if reg[tc.resolved].externalID != "30000" || reg[tc.unresolved].externalID != "" {
				t.Errorf("registry = %+v, want 30000 on %s alone", reg, tc.resolved)
			}
			if n := queryInt(t, database, `SELECT count(*) FROM jira_project_status_rules`); n != 2 {
				t.Errorf("rules = %d, want both carried", n)
			}
		})
	}
}

// TestMigrate_JiraProjectRegistrySiteMatchesGo: the site every registry row
// is keyed under is computed in SQL from the org's stored Jira base URL, and
// every later read computes it in Go with domain.EntityScope. If the two
// disagreed, every migrated rule would sit under a site no read asks for.
func TestMigrate_JiraProjectRegistrySiteMatchesGo(t *testing.T) {
	for _, base := range []string{
		"https://acme.atlassian.net",
		"https://jira.example.com/",
		"https://jira.example.com//",
		"https://jira.example.com/jira/",
		"\thttps://jira.example.com\n",
		"Https://Jira.Example.com/Jira",
	} {
		t.Run(base, func(t *testing.T) {
			database := openMigrationsTestDBAt(t, TestDSNMemory, beforeProviderScopes)
			seedProviderScopesOrg(t, database)
			setOrgBaseURL(t, database, runmode.LocalDefaultOrgID, "jira", base)
			seedLegacyJiraRule(t, database, "team-1", "SKY", `[]`)

			migrateProviderScopes(t, database)

			want := domain.EntityScope("jira", domain.OrgSettings{JiraBaseURL: base})
			if got := queryString(t, database, `SELECT site FROM jira_projects WHERE project_key = 'SKY'`); got != want {
				t.Errorf("site = %q, want %q", got, want)
			}
		})
	}
}

// TestMigrate_JiraProjectRegistryWithoutAConfiguredSite: an org whose Jira
// base URL is not a usable site keeps its rules on the site its issues were
// last polled from; an org with neither has no site to key them under, and
// its rules are not carried.
func TestMigrate_JiraProjectRegistryWithoutAConfiguredSite(t *testing.T) {
	t.Run("issues name the site", func(t *testing.T) {
		database := openMigrationsTestDBAt(t, TestDSNMemory, beforeProviderScopes)
		seedProviderScopesOrg(t, database)
		setOrgBaseURL(t, database, runmode.LocalDefaultOrgID, "jira", "")
		seedLegacyJiraRule(t, database, "team-1", "SKY", `[]`)
		seedJiraIssue(t, database, "e-old", "https://old.example.com", "SKY-1", "10000", "2026-08-01 00:00:00")
		seedJiraIssue(t, database, "e-new", "https://last.example.com", "SKY-2", "10000", "2026-09-01 00:00:00")

		migrateProviderScopes(t, database)

		if got := queryString(t, database, `SELECT site FROM jira_projects WHERE project_key = 'SKY'`); got != "https://last.example.com" {
			t.Errorf("site = %q, want the one SKY was last polled from", got)
		}
		if got := queryString(t, database, `SELECT COALESCE(external_id, '') FROM jira_projects WHERE project_key = 'SKY'`); got != "10000" {
			t.Errorf("external id = %q, want 10000", got)
		}
		if n := queryInt(t, database, `SELECT count(*) FROM jira_project_status_rules`); n != 1 {
			t.Errorf("rules = %d, want the one carried", n)
		}
	})
	t.Run("nothing names a site", func(t *testing.T) {
		database := openMigrationsTestDBAt(t, TestDSNMemory, beforeProviderScopes)
		seedProviderScopesOrg(t, database)
		seedLegacyJiraRule(t, database, "team-1", "SKY", `[]`)

		migrateProviderScopes(t, database)

		if n := queryInt(t, database, `SELECT count(*) FROM jira_projects`); n != 0 {
			t.Errorf("registry rows = %d, want none", n)
		}
		if n := queryInt(t, database, `SELECT count(*) FROM jira_project_status_rules`); n != 0 {
			t.Errorf("rules = %d, want none carried", n)
		}
	})
}
