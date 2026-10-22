package db

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// An active task on a Jira status or priority change, opened under the name
// the dedup_key used to carry, is re-keyed to the id its entity's snapshot
// shows beside that name, so the next event into the same status bumps it
// rather than opening a second task. A task whose issue has moved on, whose
// snapshot carries no id or is unreadable, or that is closed keeps its key.
func TestMigrate_ProviderScopesKeyJiraChangeTasksOnIDs(t *testing.T) {
	database := openMigrationsTestDBAt(t, TestDSNMemory, beforeProviderScopes)
	if err := SeedEventTypes(database, "sqlite3"); err != nil {
		t.Fatalf("seed event types: %v", err)
	}
	seedProviderScopesOrg(t, database)
	const site = "https://acme.atlassian.net"
	setOrgBaseURL(t, database, runmode.LocalDefaultOrgID, "jira", site)
	issue := func(id, key, snapshot string) {
		t.Helper()
		mustExec(t, database, `
			INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json)
			VALUES (?, 'jira', ?, 'issue', 't', ?, ?)`, id, key, site+"/browse/"+key, snapshot)
		mustExec(t, database, `INSERT INTO events (id, entity_id, event_type, dedup_key) VALUES (?, ?, 'jira:issue:status_changed', '')`,
			"ev-"+id, id)
	}
	issue("e-review", "SKY-1",
		`{"id":"10001","key":"SKY-1","project_id":"100","status":"In Review","status_id":"10003","priority":"High","priority_id":"2"}`)
	issue("e-moved-on", "SKY-2", `{"id":"10002","key":"SKY-2","project_id":"100","status":"Done","status_id":"10004"}`)
	issue("e-no-ids", "SKY-3", `{"id":"10005","key":"SKY-3","project_id":"100","status":"In Review","priority":"High"}`)
	issue("e-malformed", "SKY-4", `{not json`)

	const statusChanged, priorityChanged = "jira:issue:status_changed", "jira:issue:priority_changed"
	cases := []struct{ id, entityID, eventType, dedupKey, status, want string }{
		{"t-status", "e-review", statusChanged, "In Review", "queued", "10003"},
		{"t-priority", "e-review", priorityChanged, "High", "in_progress", "2"},
		{"t-earlier-status", "e-review", statusChanged, "To Do", "snoozed", "To Do"},
		{"t-closed", "e-review", statusChanged, "In Review", "done", "In Review"},
		{"t-moved-on", "e-moved-on", statusChanged, "In Review", "queued", "In Review"},
		{"t-no-ids", "e-no-ids", statusChanged, "In Review", "queued", "In Review"},
		{"t-no-priority-id", "e-no-ids", priorityChanged, "High", "queued", "High"},
		{"t-malformed", "e-malformed", statusChanged, "In Review", "queued", "In Review"},
		{"t-other-type", "e-review", "jira:issue:label_added", "In Review", "queued", "In Review"},
	}
	for _, c := range cases {
		mustExec(t, database, `INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id, status)
			VALUES (?, ?, ?, ?, ?, ?)`, c.id, c.entityID, c.eventType, c.dedupKey, "ev-"+c.entityID, c.status)
	}

	migrateProviderScopes(t, database)

	for _, c := range cases {
		if got := queryString(t, database, `SELECT dedup_key FROM tasks WHERE id = ?`, c.id); got != c.want {
			t.Errorf("%s dedup_key = %q, want %q", c.id, got, c.want)
		}
	}
}
