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
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// seedStockConfig installs everything the carry-over routes gate on before
// they look at a single issue key: org Jira credentials, a team status rule,
// and the acting user's stored Jira identity (host-scoped, so it keys on the
// same host the org creds carry).
func seedStockConfig(t *testing.T, s *Server) {
	t.Helper()
	ctx := t.Context()
	if err := integrations.Save(ctx, s.secrets, runmode.LocalDefaultOrgID, auth.Credentials{
		JiraURL: "https://jira.example.com",
		JiraPAT: "tok",
	}); err != nil {
		t.Fatalf("save jira creds: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO jira_project_status_rules
		   (team_id, project_key, pickup_members, in_progress_members, in_progress_canonical, done_members, done_canonical)
		 VALUES (?, 'SKY', '["To Do"]', '["In Progress"]', 'In Progress', '["Done"]', 'Done')`,
		runmode.LocalDefaultTeamID,
	); err != nil {
		t.Fatalf("seed jira rule: %v", err)
	}
	if err := s.users.UpsertJiraIdentity(ctx, runmode.LocalDefaultUserID, "https://jira.example.com", "acc-1", "Tester", "pat"); err != nil {
		t.Fatalf("set jira identity: %v", err)
	}
	// The deck read resolves the user's Jira identity against the ORG's
	// configured host (org_settings), where the action routes read it off the
	// credential; both have to name the same host or the identity lookup
	// misses and the deck reports Jira unconfigured.
	if _, err := s.db.Exec(
		`UPDATE org_settings SET jira_base_url = 'https://jira.example.com' WHERE org_id = ?`,
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
	snapshot := fmt.Sprintf(
		`{"key":%q,"summary":"Carry over me","status":"To Do","issue_type":"Task","priority":"Medium","assignee":"Tester","assignee_account_id":"acc-1","url":"https://jira.example.com/browse/%s"}`,
		issueKey, issueKey,
	)
	if _, err := s.db.Exec(
		`INSERT INTO entities (id, source, source_id, kind, title, url, state, snapshot_json)
		 VALUES (?, 'jira', ?, 'issue', 'Carry over me', ?, 'active', ?)`,
		"e_stock_"+issueKey, issueKey, "https://jira.example.com/browse/"+issueKey, snapshot,
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
// batch policy: an empty list, an over-cap list, a blank key and a repeated
// key fail the whole call, because each would otherwise make the accounting
// invariant a lie for the batch that ran. Every action route enforces it
// identically — the validator is shared rather than restated per route.
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
			"empty":     {},
			"blank_key": {"SKY-1", "   "},
			"duplicate": {"SKY-1", "SKY-1"},
			"over_cap":  overCap,
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
