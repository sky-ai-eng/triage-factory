package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
// deck's assigned bucket surfaces and the queue route accepts. Its entity id
// is "e_stock_" + issueKey.
func seedStockTicket(t *testing.T, s *Server, issueKey string) {
	t.Helper()
	seedStockTicketIn(t, s, issueKey, "To Do", "Tester", "acc-1")
}

// seedStockTicketIn is seedStockTicket with the ticket in status and assigned
// to the user with display name assignee and stable id accountID, both empty
// for an unassigned ticket.
func seedStockTicketIn(t *testing.T, s *Server, issueKey, status, assignee, accountID string) {
	t.Helper()
	project := dbseed.JiraIssueProject(issueKey)
	snapshot := fmt.Sprintf(
		`{"key":%q,"project_id":%q,"project_key":%q,"summary":"Carry over me","status":%q,"issue_type":"Task","priority":"Medium","assignee":%q,"assignee_account_id":%q,"url":"https://jira.example.com/browse/%s"}`,
		issueKey, project.ID, project.Key, status, assignee, accountID, issueKey,
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

// postStock sends ids to a carry-over action route and decodes its answer.
func postStock(t *testing.T, s *Server, action string, ids ...string) (*httptest.ResponseRecorder, stockBatchBody) {
	t.Helper()
	rec := doJSON(t, s, http.MethodPost, "/api/jira/stock/"+action, map[string]any{"entity_ids": ids})
	var body stockBatchBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: decode body: %v; body=%s", action, err, rec.Body.String())
	}
	return rec, body
}

// TestStockQueue_PerItemAccounting pins the batch policy's core invariant:
// every submitted id comes back in the results, ok either way, so applied +
// failed = submitted and no id is silently dropped mid-loop. The mixed batch
// here is the case that used to under-report — one applicable ticket next to
// an id that names none, whose row names the field.
func TestStockQueue_PerItemAccounting(t *testing.T) {
	keyring.MockInit() // in-memory keychain — the sandbox has no dbus backend
	s := newTestServer(t)
	seedStockConfig(t, s)
	seedStockTicket(t, s, "SKY-1")

	submitted := []string{"e_stock_SKY-1", "e_stock_missing"}
	rec, body := postStock(t, s, "queue", submitted...)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
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
		seen[row.EntityID] = row
	}
	if row, ok := seen["e_stock_SKY-1"]; !ok || !row.OK || row.IssueKey != "SKY-1" {
		t.Errorf("e_stock_SKY-1 row = %+v, want ok under SKY-1", row)
	}
	// A failed row carries the envelope's item shape, not a prose string, and
	// an id that names no ticket names the field.
	row, ok := seen["e_stock_missing"]
	if !ok || row.OK {
		t.Fatalf("e_stock_missing row = %+v, want a failure", row)
	}
	if len(row.Errors) == 0 || row.Errors[0].Reason != "NOT_FOUND" || row.Errors[0].Field != "entity_ids" {
		t.Errorf("e_stock_missing errors = %+v, want NOT_FOUND on entity_ids", row.Errors)
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
// id and a repeated id fail the whole call, because each would otherwise
// make the accounting invariant a lie for the batch that ran — the padded id
// by answering under an id the caller never sent. Every action route enforces
// it identically: the validator is shared rather than restated per route.
func TestStockActions_RejectMalformedBatches(t *testing.T) {
	keyring.MockInit()
	s := newTestServer(t)
	seedStockConfig(t, s)

	overCap := make([]string, maxStockBatch+1)
	for i := range overCap {
		overCap[i] = fmt.Sprintf("e_%d", i)
	}

	for _, action := range []string{"queue", "claim", "done"} {
		for name, tc := range map[string]struct {
			field string
			ids   []string
		}{
			"empty":      {"entity_ids", []string{}},
			"blank_id":   {"entity_ids", []string{"e_1", "   "}},
			"padded_id":  {"entity_ids", []string{"e_1", " e_2 "}},
			"duplicate":  {"entity_ids", []string{"e_1", "e_1"}},
			"over_cap":   {"entity_ids", overCap},
			"issue_keys": {"issue_keys", []string{"SKY-1"}},
		} {
			t.Run(action+"/"+name, func(t *testing.T) {
				rec := doJSON(t, s, http.MethodPost, "/api/jira/stock/"+action, map[string]any{tc.field: tc.ids})
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
				}
				if !strings.Contains(rec.Body.String(), `"field":"`+tc.field+`"`) {
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
	if row["entity_id"] != "e_stock_SKY-1" || row["issue_key"] != "SKY-9" || row["url"] != "https://jira.example.com/browse/SKY-9" {
		t.Errorf("deck row = %v, want e_stock_SKY-1 under SKY-9", row)
	}

	rec, body := postStock(t, s, "queue", "e_stock_SKY-1")
	if rec.Code != http.StatusOK || body.Applied != 1 || len(body.Results) != 1 || body.Results[0].IssueKey != "SKY-9" {
		t.Fatalf("queue e_stock_SKY-1 = %d %s, want it applied under SKY-9", rec.Code, rec.Body.String())
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
	renamed, err := s.allStores.Entities.Get(ctx, org, entityID)
	if err != nil || renamed == nil {
		t.Fatalf("read the renamed issue: %v", err)
	}
	if ok, err := s.allStores.Entities.UpdateSnapshotCASSystem(ctx, org, entityID,
		`{"id":"10002","key":"CORE-2","project_id":"`+project.ID+`","project_key":"CORE","summary":"Pick me up","status":"To Do","assignee":""}`,
		renamed.PollSeq,
	); err != nil || !ok {
		t.Fatalf("refresh the snapshot: ok=%v err=%v", ok, err)
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
			rule = lookupJiraTaskTarget(ctx, tx, org, site, task).Rule
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

// TestStockDeck_BlankDisplayNameClaimsNoUnassignedTicket: a Jira identity can
// carry no display name (some Data Center installs). An unassigned ticket has
// none either, and must not read as the user's: it is neither on the deck's
// assigned half nor actionable as theirs.
func TestStockDeck_BlankDisplayNameClaimsNoUnassignedTicket(t *testing.T) {
	keyring.MockInit()
	s := newTestServer(t)
	seedStockConfig(t, s)
	if err := s.users.UpsertJiraIdentity(t.Context(), runmode.LocalDefaultUserID, "https://jira.example.com", "acc-1", "", "pat"); err != nil {
		t.Fatalf("blank the identity's display name: %v", err)
	}
	// Outside the pickup rule too, so it belongs on neither half.
	seedStockTicketIn(t, s, "SKY-3", "In Progress", "", "")
	fake := newJiraIssueFake(t, statusInProgressID, "")
	s.jiraResolver = &recordingJiraResolver{userClient: fake.client()}
	if err := s.allStores.PollReadiness.MarkPollComplete(t.Context(), runmode.LocalDefaultOrgID, "jira", time.Time{}); err != nil {
		t.Fatalf("mark jira poll complete: %v", err)
	}

	if assigned, _ := readStockDeckKeys(t, s)["assigned"].([]any); len(assigned) != 0 {
		t.Errorf("assigned = %v, want no ticket: nobody is assigned SKY-3", assigned)
	}
	for _, action := range []string{"queue", "claim", "done"} {
		if rec, body := postStock(t, s, action, "e_stock_SKY-3"); body.Applied != 0 || len(body.Results) != 1 || body.Results[0].OK {
			t.Errorf("%s on an unassigned ticket = %s, want it refused", action, rec.Body.String())
		}
	}
	var tasks int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE entity_id = 'e_stock_SKY-3'`).Scan(&tasks); err != nil || tasks != 0 {
		t.Errorf("tasks on the unassigned ticket = %d (%v), want none", tasks, err)
	}
	if writes := fake.Writes(); len(writes) != 0 {
		t.Errorf("Jira writes = %v, want none", writes)
	}
}

// TestStockClaimAndDone_AddressJiraByIssueID: the claim and done writes name
// the issue by its id, which a move leaves alone, never by its key.
func TestStockClaimAndDone_AddressJiraByIssueID(t *testing.T) {
	keyring.MockInit()
	s := newTestServer(t)
	seedStockConfig(t, s)
	seedStockTicket(t, s, "SKY-1")
	seedStockTicket(t, s, "SKY-2")
	fake := newJiraIssueFake(t, statusToDoID, "")
	s.jiraResolver = &recordingJiraResolver{userClient: fake.client()}

	for action, entityID := range map[string]string{"claim": "e_stock_SKY-1", "done": "e_stock_SKY-2"} {
		if rec, body := postStock(t, s, action, entityID); body.Applied != 1 {
			t.Fatalf("%s %s = %d %s, want it applied", action, entityID, rec.Code, rec.Body.String())
		}
	}
	fake.assertIssuesNamed(t, dbseed.JiraIssueID("SKY-1"), dbseed.JiraIssueID("SKY-2"))
}
