package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// seedStockConfig installs everything the carry-over routes gate on before
// they look at a single issue key: org Jira credentials, a team status rule,
// and the acting user's stored Jira identity (host-scoped, so it keys on the
// org's Jira site).
func seedStockConfig(t *testing.T, s *Server) {
	t.Helper()
	ctx := t.Context()
	if err := integrations.Save(ctx, s.secrets, runmode.LocalDefaultOrgID, auth.Credentials{
		JiraPAT: "tok",
	}); err != nil {
		t.Fatalf("save jira creds: %v", err)
	}
	project := dbseed.JiraProject(t, s.db, runmode.LocalDefaultOrgID, "https://jira.example.com", "SKY")
	if _, err := s.db.Exec(
		`INSERT INTO jira_project_status_rules
		   (team_id, jira_project_id, org_id, pickup_members, in_progress_members, in_progress_canonical, done_members, done_canonical)
		 VALUES (?, ?, ?, '["To Do"]', '["In Progress"]', 'In Progress', '["Done"]', 'Done')`,
		runmode.LocalDefaultTeamID, project, runmode.LocalDefaultOrgID,
	); err != nil {
		t.Fatalf("seed jira rule: %v", err)
	}
	if err := s.users.UpsertJiraIdentity(ctx, runmode.LocalDefaultUserID, "https://jira.example.com", "acc-1", "Tester", "pat"); err != nil {
		t.Fatalf("set jira identity: %v", err)
	}
	// The deck and the action routes resolve the user's Jira identity against
	// the org's Jira site (org_event_sources); without it the identity lookup
	// misses and the routes report Jira unconfigured.
	if _, err := s.db.Exec(
		`INSERT INTO org_event_sources (org_id, kind, base_url) VALUES (?, 'jira', 'https://jira.example.com')
		 ON CONFLICT(org_id, kind) DO UPDATE SET base_url = excluded.base_url`,
		runmode.LocalDefaultOrgID,
	); err != nil {
		t.Fatalf("seed org jira base url: %v", err)
	}
}

// seedStockTicket inserts an active Jira entity with a snapshot assigned to
// the acting user and sitting in the project's pickup status — the shape the
// deck's assigned bucket surfaces and the queue route accepts.
func seedStockTicket(t *testing.T, s *Server, issueKey string) {
	t.Helper()
	project := dbseed.JiraIssueProject(issueKey)
	snapshot := fmt.Sprintf(
		`{"key":%q,"project_id":%q,"project_key":%q,"summary":"Carry over me","status":"To Do","issue_type":"Task","priority":"Medium","assignee":"Tester","assignee_account_id":"acc-1","url":"https://jira.example.com/browse/%s"}`,
		issueKey, project.ID, project.Key, issueKey,
	)
	if _, err := s.db.Exec(
		`INSERT INTO entities (id, source, source_id, external_id, kind, title, url, state, snapshot_json, scope, jira_project_id)
		 VALUES (?, 'jira', ?, ?, 'issue', 'Carry over me', ?, 'active', ?, 'https://jira.example.com', ?)`,
		"e_stock_"+issueKey, issueKey, dbseed.JiraIssueID(issueKey), "https://jira.example.com/browse/"+issueKey, snapshot,
		dbseed.JiraIssueProjectID(t, s.db, runmode.LocalDefaultOrgID, "https://jira.example.com", issueKey),
	); err != nil {
		t.Fatalf("seed jira entity %s: %v", issueKey, err)
	}
}

// stockBatchBody is the response every carry-over action route answers with.
type stockBatchBody struct {
	Applied int               `json:"applied"`
	Failed  int               `json:"failed"`
	Results []stockItemResult `json:"results"`
}

// TestStockQueue_PerItemAccounting pins the batch policy's core invariant:
// every submitted key comes back in the results, ok either way, so applied +
// failed = submitted and no id is silently dropped mid-loop. The mixed batch
// here is the case that used to under-report — one applicable ticket next to
// one that doesn't exist.
func TestStockQueue_PerItemAccounting(t *testing.T) {
	keyring.MockInit() // in-memory keychain — the sandbox has no dbus backend
	s := newTestServer(t)
	seedStockConfig(t, s)
	seedStockTicket(t, s, "SKY-1")

	submitted := []string{"SKY-1", "SKY-404"}
	rec := doJSON(t, s, http.MethodPost, "/api/jira/stock/queue",
		map[string]any{"issue_keys": submitted})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var body stockBatchBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Results) != len(submitted) {
		t.Fatalf("results = %d rows, want one per submitted key (%d): %s", len(body.Results), len(submitted), rec.Body.String())
	}
	if body.Applied+body.Failed != len(submitted) {
		t.Errorf("applied(%d) + failed(%d) != submitted(%d)", body.Applied, body.Failed, len(submitted))
	}
	if body.Applied != 1 || body.Failed != 1 {
		t.Errorf("applied=%d failed=%d, want 1 and 1; body=%s", body.Applied, body.Failed, rec.Body.String())
	}
	seen := map[string]stockItemResult{}
	for _, row := range body.Results {
		seen[row.IssueKey] = row
	}
	if row, ok := seen["SKY-1"]; !ok || !row.OK {
		t.Errorf("SKY-1 row = %+v, want ok", row)
	}
	// A failed row carries the envelope's item shape, not a prose string.
	row, ok := seen["SKY-404"]
	if !ok || row.OK {
		t.Fatalf("SKY-404 row = %+v, want a failure", row)
	}
	if len(row.Errors) == 0 || row.Errors[0].Reason == "" {
		t.Errorf("SKY-404 errors = %+v, want a structured item with a reason", row.Errors)
	}

	// The applied key really did mint a task — the accounting isn't reporting
	// work it didn't do.
	var tasks int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM tasks WHERE entity_id = 'e_stock_SKY-1'`,
	).Scan(&tasks); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if tasks != 1 {
		t.Errorf("tasks for the queued ticket = %d, want 1", tasks)
	}
}

// TestStockActions_RejectMalformedBatches pins the request-level half of the
// batch policy: an empty list, an over-cap list, a blank or whitespace-padded
// key and a repeated key fail the whole call, because each would otherwise
// make the accounting invariant a lie for the batch that ran — the padded key
// by answering under a key the caller never sent. Every action route enforces
// it identically: the validator is shared rather than restated per route.
func TestStockActions_RejectMalformedBatches(t *testing.T) {
	keyring.MockInit()
	s := newTestServer(t)
	seedStockConfig(t, s)

	overCap := make([]string, maxStockBatch+1)
	for i := range overCap {
		overCap[i] = fmt.Sprintf("SKY-%d", i)
	}

	for _, action := range []string{"queue", "claim", "done"} {
		for name, keys := range map[string][]string{
			"empty":      {},
			"blank_key":  {"SKY-1", "   "},
			"padded_key": {"SKY-1", " SKY-2 "},
			"duplicate":  {"SKY-1", "SKY-1"},
			"over_cap":   overCap,
		} {
			t.Run(action+"/"+name, func(t *testing.T) {
				rec := doJSON(t, s, http.MethodPost, "/api/jira/stock/"+action,
					map[string]any{"issue_keys": keys})
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
				}
				if !strings.Contains(rec.Body.String(), "issue_keys") {
					t.Errorf("body=%s; want the failing field named", rec.Body.String())
				}
			})
		}
	}
}

// TestStockDeck_OneSchemaInEveryState pins the GET contract: status
// distinguishes "still polling" from "here is the deck", and it never changes
// which keys are present. A client that has to branch on status before it
// knows whether `assigned` exists ends up writing two parsers and picking the
// wrong one.
func TestStockDeck_OneSchemaInEveryState(t *testing.T) {
	keyring.MockInit()
	s := newTestServer(t)
	seedStockConfig(t, s)

	// Polling: no poll cycle has completed for this org yet.
	polling := readStockDeckKeys(t, s)
	if polling["status"] != "polling" {
		t.Fatalf("status = %v, want polling; keys=%v", polling["status"], polling)
	}

	if err := s.allStores.PollReadiness.MarkPollComplete(t.Context(), runmode.LocalDefaultOrgID, "jira", time.Time{}); err != nil {
		t.Fatalf("mark jira poll complete: %v", err)
	}
	ready := readStockDeckKeys(t, s)
	if ready["status"] != "ready" {
		t.Fatalf("status = %v, want ready; keys=%v", ready["status"], ready)
	}

	for _, key := range []string{"status", "assigned", "available"} {
		if _, ok := polling[key]; !ok {
			t.Errorf("polling response is missing %q: %v", key, polling)
		}
		if _, ok := ready[key]; !ok {
			t.Errorf("ready response is missing %q: %v", key, ready)
		}
	}
	if len(polling) != len(ready) {
		t.Errorf("polling has %d keys and ready has %d; the shape must not depend on the status", len(polling), len(ready))
	}
	// Empty rather than absent while polling: a client can render the (empty)
	// deck without a null check.
	for _, key := range []string{"assigned", "available"} {
		rows, ok := polling[key].([]any)
		if !ok {
			t.Errorf("polling %q = %T, want an array", key, polling[key])
			continue
		}
		if len(rows) != 0 {
			t.Errorf("polling %q has %d rows, want none", key, len(rows))
		}
	}
}

// readStockDeckKeys fetches the deck and returns it as a raw key map, so the
// assertions above can talk about key PRESENCE rather than about a Go struct
// that would happily materialize a missing field as its zero value.
func readStockDeckKeys(t *testing.T, s *Server) map[string]any {
	t.Helper()
	rec := doJSON(t, s, http.MethodGet, "/api/jira/stock", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/jira/stock = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode deck: %v", err)
	}
	return out
}

// TestStockDeck_ListsAndActsOnTheEntitysCurrentKey: an entity renamed when TF
// learned its issue moved — before the next poll refreshed its snapshot —
// lists under the key it answers to now, and an action on that key finds it.
func TestStockDeck_ListsAndActsOnTheEntitysCurrentKey(t *testing.T) {
	keyring.MockInit()
	s := newTestServer(t)
	seedStockConfig(t, s)
	seedStockTicket(t, s, "SKY-1")
	// Renamed in place; the snapshot still names the key the issue left.
	if _, err := s.db.Exec(
		`UPDATE entities SET source_id = 'SKY-9', url = 'https://jira.example.com/browse/SKY-9', external_id = '10001' WHERE id = 'e_stock_SKY-1'`,
	); err != nil {
		t.Fatalf("rename entity: %v", err)
	}
	if err := s.allStores.PollReadiness.MarkPollComplete(t.Context(), runmode.LocalDefaultOrgID, "jira", time.Time{}); err != nil {
		t.Fatalf("mark jira poll complete: %v", err)
	}

	deck := readStockDeckKeys(t, s)
	assigned, _ := deck["assigned"].([]any)
	if len(assigned) != 1 {
		t.Fatalf("assigned = %v, want the renamed ticket", deck["assigned"])
	}
	row, _ := assigned[0].(map[string]any)
	if row["issue_key"] != "SKY-9" || row["url"] != "https://jira.example.com/browse/SKY-9" {
		t.Errorf("deck row = %v, want it under SKY-9", row)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/jira/stock/queue", map[string]any{"issue_keys": []string{"SKY-9"}})
	var body stockBatchBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if rec.Code != http.StatusOK || body.Applied != 1 || len(body.Results) != 1 || body.Results[0].IssueKey != "SKY-9" {
		t.Fatalf("queue SKY-9 = %d %s, want it applied", rec.Code, rec.Body.String())
	}
	var meta string
	if err := s.db.QueryRow(
		`SELECT e.metadata_json FROM tasks t JOIN events e ON e.id = t.primary_event_id WHERE t.entity_id = 'e_stock_SKY-1'`,
	).Scan(&meta); err != nil {
		t.Fatalf("read the queued task's event: %v", err)
	}
	if !strings.Contains(meta, `"issue_key":"SKY-9"`) || !strings.Contains(meta, `"issue_id":"10001"`) {
		t.Errorf("event metadata = %s, want the current key and the issue id", meta)
	}
}

// TestJiraReaders_FollowAProjectKeyRename: a project whose key is renamed keeps
// its registry row, which the issue's entity and the team's rule both
// reference. Once the issue answers under the new key, the deck still
// classifies it under its project's rule (an unassigned ticket in a pickup
// status is still available to claim) and the claim/requeue path still finds
// that rule: before the poll has re-keyed the registry row, when no key the
// issue carries matches the rule's, and after it.
func TestJiraReaders_FollowAProjectKeyRename(t *testing.T) {
	keyring.MockInit()
	s := newTestServer(t)
	seedStockConfig(t, s)
	ctx := t.Context()
	const (
		org      = runmode.LocalDefaultOrgID
		site     = "https://jira.example.com"
		entityID = "e_stock_renamed"
	)
	project := dbseed.JiraIssueProject("SKY-2")
	row := dbseed.JiraIssueProjectID(t, s.db, org, site, "SKY-2")
	if _, err := s.db.Exec(
		`INSERT INTO entities (id, source, source_id, external_id, kind, title, url, state, snapshot_json, scope, jira_project_id)
		 VALUES (?, 'jira', 'SKY-2', '10002', 'issue', 'Pick me up', ?, 'active', ?, ?, ?)`,
		entityID, site+"/browse/SKY-2",
		`{"id":"10002","key":"SKY-2","project_id":"`+project.ID+`","project_key":"SKY","summary":"Pick me up","status":"To Do","assignee":""}`,
		site, row,
	); err != nil {
		t.Fatalf("seed jira entity: %v", err)
	}
	if err := s.allStores.PollReadiness.MarkPollComplete(ctx, org, "jira", time.Time{}); err != nil {
		t.Fatalf("mark jira poll complete: %v", err)
	}

	// The issue renamed by its id and its snapshot refreshed from a read
	// that names the new key, as the tracker applies a rename.
	if _, err := s.allStores.Entities.RenameSystem(ctx, org, "jira", site, "10002", "CORE-2", site+"/browse/CORE-2"); err != nil {
		t.Fatalf("rename the issue: %v", err)
	}
	if _, err := s.allStores.Entities.PatchSnapshot(ctx, org, entityID,
		`{"id":"10002","key":"CORE-2","project_id":"`+project.ID+`","project_key":"CORE","summary":"Pick me up","status":"To Do","assignee":""}`,
	); err != nil {
		t.Fatalf("refresh the snapshot: %v", err)
	}

	team := runmode.LocalDefaultTeamID
	task := &domain.Task{EntityID: entityID, EntitySource: "jira", EntitySourceID: "CORE-2", TeamID: &team}
	check := func(stage, ruleKey string) {
		t.Helper()
		deck := readStockDeckKeys(t, s)
		available, _ := deck["available"].([]any)
		if len(available) != 1 {
			t.Fatalf("%s: available = %v, want the renamed ticket still under its project's pickup rule", stage, deck["available"])
		}
		if got, _ := available[0].(map[string]any); got["issue_key"] != "CORE-2" || got["project"] != "CORE" {
			t.Errorf("%s: deck row = %v, want CORE-2 in CORE", stage, got)
		}
		var rule *domain.JiraProjectStatusRules
		if err := s.tx.WithReadTx(ctx, org, runmode.LocalDefaultUserID, func(tx db.TxStores) error {
			rule = lookupJiraRuleForTask(ctx, tx, org, site, task)
			return nil
		}); err != nil {
			t.Fatalf("%s: read the rule: %v", stage, err)
		}
		if rule == nil || rule.JiraProjectID != row || rule.ProjectKey != ruleKey || rule.InProgressCanonical.Name != "In Progress" {
			t.Errorf("%s: claim rule = %+v, want the project's rule under %s", stage, rule, ruleKey)
		}
	}
	check("before the registry row is re-keyed", "SKY")

	if _, err := s.allStores.JiraProjects.RefreshSystem(ctx, org, row, project.ID, "CORE", "Core"); err != nil {
		t.Fatalf("rename the project: %v", err)
	}
	check("after the registry row is re-keyed", "CORE")
}
