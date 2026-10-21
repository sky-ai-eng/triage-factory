package db

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestMigrate_ProviderScopesGiveJiraEntitiesTheirProject: every Jira entity
// references the registry row of the project its issue sits in, on its own
// site, whether or not a team tracks that project, and the CHECK holds every
// Jira row with a site to one from then on.
func TestMigrate_ProviderScopesGiveJiraEntitiesTheirProject(t *testing.T) {
	database := openMigrationsTestDBAt(t, TestDSNMemory, beforeProviderScopes)
	seedProviderScopesOrg(t, database)
	const site = "https://acme.atlassian.net"
	setOrgBaseURL(t, database, runmode.LocalDefaultOrgID, "jira", site)
	seedLegacyJiraRule(t, database, "team-1", "SKY", `[]`)

	// A tracked project whose issue names its id.
	seedJiraIssue(t, database, "e-sky", site, "SKY-1", "10000", "2026-09-01 00:00:00")
	// An untracked project renamed from ENG to CORE: its issues name one id
	// under both keys, and the newest polled key is the one the row gets.
	seedJiraIssue(t, database, "e-eng", site, "ENG-1", "20000", "2026-08-01 00:00:00")
	seedJiraIssue(t, database, "e-core", site, "CORE-2", "20000", "2026-09-01 00:00:00")
	// Untracked issues no snapshot names a project for: two under one key
	// share one unresolved row, and one under a key a resolved row holds
	// takes that row.
	seedJiraIssue(t, database, "e-ops-1", site, "OPS-1", "", nil)
	mustExec(t, database, `INSERT INTO entities (id, source, source_id, kind, title, url)
		VALUES ('e-ops-2', 'jira', 'OPS-2', 'issue', 't', ?)`, site+"/browse/OPS-2")
	seedJiraIssue(t, database, "e-core-stub", site, "CORE-3", "", nil)

	migrateProviderScopes(t, database)

	reg := registryOnSite(t, database, site)
	for key, externalID := range map[string]string{"SKY": "10000", "CORE": "20000", "OPS": ""} {
		if r, ok := reg[key]; !ok || r.externalID != externalID {
			t.Errorf("registry row for %s = %+v (present %v), want external id %q", key, r, ok, externalID)
		}
	}
	if _, ok := reg["ENG"]; ok {
		t.Error("ENG got a row of its own; its issue names CORE's project id")
	}
	if n := queryInt(t, database, `SELECT count(*) FROM jira_project_status_rules r JOIN jira_projects p ON p.id = r.jira_project_id WHERE p.project_key <> 'SKY'`); n != 0 {
		t.Errorf("%d rules on projects no team tracked, want none: registering a project tracks nothing", n)
	}

	for entity, key := range map[string]string{
		"e-sky": "SKY", "e-eng": "CORE", "e-core": "CORE", "e-core-stub": "CORE",
		"e-ops-1": "OPS", "e-ops-2": "OPS",
	} {
		if got := jiraProjectOf(t, database, entity); got != reg[key].id {
			t.Errorf("%s references %q, want %s's row %q", entity, got, key, reg[key].id)
		}
	}
	assertNoDanglingReferences(t, database)

	refused := func(what, stmt string) {
		t.Helper()
		_, err := database.Exec(stmt)
		if err == nil || !strings.Contains(err.Error(), "entities_jira_project_required") {
			t.Errorf("%s = %v, want the entities_jira_project_required CHECK to refuse it", what, err)
		}
	}
	refused("a jira insert with no project",
		`INSERT INTO entities (id, source, scope, source_id, kind) VALUES ('e-bare', 'jira', '`+site+`', 'SKY-5', 'issue')`)
	refused("clearing a jira row's project",
		`UPDATE entities SET jira_project_id = NULL WHERE id = 'e-sky'`)
	mustExec(t, database, `INSERT INTO entities (id, source, scope, source_id, kind) VALUES ('e-slack', 'slack', 'slack.com', 'C1/1.0', 'thread')`)
	refused("turning a row into a jira row with no project",
		`UPDATE entities SET source = 'jira', scope = '`+site+`' WHERE id = 'e-slack'`)

	mustExec(t, database, `PRAGMA foreign_keys = on`)
	if _, err := database.Exec(`INSERT INTO entities (id, source, scope, source_id, kind, jira_project_id)
		VALUES ('e-ghost', 'jira', ?, 'SKY-6', 'issue', 'no-such-project')`, site); err == nil {
		t.Error("a jira insert naming a registry row that does not exist succeeded; the foreign key should refuse it")
	}
}

// TestMigrate_JiraEntityWithNoSiteKeepsNoProject: an issue whose url names no
// site, in an org with no Jira site configured, has no site a registry row
// could be keyed under. It keeps an empty scope and no project, which the
// CHECK allows for an empty scope alone, and the migration completes.
func TestMigrate_JiraEntityWithNoSiteKeepsNoProject(t *testing.T) {
	database := openMigrationsTestDBAt(t, TestDSNMemory, beforeProviderScopes)
	seedProviderScopesOrg(t, database)
	mustExec(t, database, `INSERT INTO entities (id, source, source_id, kind) VALUES ('e-stub', 'jira', 'SKY-1', 'issue')`)

	migrateProviderScopes(t, database)

	if got := queryString(t, database, `SELECT scope FROM entities WHERE id = 'e-stub'`); got != "" {
		t.Errorf("scope = %q, want none", got)
	}
	if got := jiraProjectOf(t, database, "e-stub"); got != "" {
		t.Errorf("e-stub references %q, want no project", got)
	}
	if n := queryInt(t, database, `SELECT count(*) FROM jira_projects`); n != 0 {
		t.Errorf("registry rows = %d, want none: there is no site to key one under", n)
	}
}

func jiraProjectOf(t *testing.T, database *sql.DB, entityID string) string {
	t.Helper()
	var id sql.NullString
	if err := database.QueryRow(`SELECT jira_project_id FROM entities WHERE id = ?`, entityID).Scan(&id); err != nil {
		t.Fatalf("read jira_project_id of %s: %v", entityID, err)
	}
	return id.String
}
