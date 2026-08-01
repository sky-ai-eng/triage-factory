package slack

// Coverage for the access-change-log half of the Slack connect surface: a
// workspace connect and disconnect are org-credential changes, so each must
// leave exactly one audit row, and no other path may invent one.
//
// This suite exists in ee/slack rather than alongside internal/server's
// credential_audit_test.go because the open-core boundary (scripts/lint.sh)
// forbids core importing ee — core's "every credential surface leaves a row"
// test structurally cannot reach these handlers, which is how this gap went
// unnoticed. Each ee surface that touches org access has to carry its own.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// auditRow is one access_change_log row with its detail_json flattened on.
type auditRow struct {
	Action      string
	Actor       string
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	WorkspaceID string `json:"workspace_id"`
	APIAppID    string `json:"api_app_id"`
}

// auditRows reads orgID's audit rows oldest-first. Admin pool: the assertion is
// about what was written, so it deliberately reads past RLS.
func (r *slackWorkspaceRig) auditRows(orgID string) []auditRow {
	r.t.Helper()
	rows, err := r.h.AdminDB.QueryContext(r.t.Context(), `
		SELECT action, COALESCE(actor_user_id::text, ''), COALESCE(detail_json, '')
		  FROM access_change_log
		 WHERE org_id = $1
		 ORDER BY created_at, id
	`, orgID)
	if err != nil {
		r.t.Fatalf("query access_change_log: %v", err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var row auditRow
		var detail string
		if err := rows.Scan(&row.Action, &row.Actor, &detail); err != nil {
			r.t.Fatalf("scan audit row: %v", err)
		}
		if detail != "" {
			if err := json.Unmarshal([]byte(detail), &row); err != nil {
				r.t.Fatalf("decode detail_json %q: %v", detail, err)
			}
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("audit rows: %v", err)
	}
	return out
}

// A successful connect records exactly one credential_set naming the workspace.
func TestHandleConnect_RecordsAccessChange(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-audit-connect")
	r.fake.stageConnectOK("xoxb-audit", "T0AUDIT1", "Acme Corp", "U0BOTA", "A0AUDIT01", "")

	rec := httptest.NewRecorder()
	r.hdl.handleConnect(rec, r.req(http.MethodPost, "/api/slack/workspaces", owner, orgID, "",
		map[string]string{"bot_token": "xoxb-audit", "app_token": "xapp-audit"}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("connect status = %d (body=%s)", rec.Code, rec.Body.String())
	}

	rows := r.auditRows(orgID)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %+v; want exactly 1 for one connect", rows)
	}
	got := rows[0]
	if got.Action != domain.AccessActionCredentialSet {
		t.Errorf("action = %q, want %q", got.Action, domain.AccessActionCredentialSet)
	}
	if got.Kind != domain.CredentialKindSlackWorkspace {
		t.Errorf("kind = %q, want %q — the three Slack secrets record as one workspace",
			got.Kind, domain.CredentialKindSlackWorkspace)
	}
	if got.Actor != owner {
		t.Errorf("actor = %q, want the connecting admin %q", got.Actor, owner)
	}
	if got.Name != "Acme Corp" {
		t.Errorf("name = %q, want the workspace name from auth.test", got.Name)
	}
	if got.WorkspaceID != "T0AUDIT1" || got.APIAppID != "A0AUDIT01" {
		t.Errorf("ids = (%q, %q); want (T0AUDIT1, A0AUDIT01) — the name alone doesn't identify the connection",
			got.WorkspaceID, got.APIAppID)
	}
}

// A re-connect is a rotation, not a no-op: the bot token has no keep-current
// path, so it is re-validated and re-stored every time and must record again.
func TestHandleConnect_ReconnectRecordsRotation(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-audit-rotate")
	r.fake.stageConnectOK("xoxb-first", "T0ROT1", "Acme", "U0BOTR", "A0ROT01", "")
	r.fake.stageConnectOK("xoxb-second", "T0ROT1", "Acme", "U0BOTR", "A0ROT01", "")

	for _, tok := range []string{"xoxb-first", "xoxb-second"} {
		rec := httptest.NewRecorder()
		r.hdl.handleConnect(rec, r.req(http.MethodPost, "/api/slack/workspaces", owner, orgID, "",
			map[string]string{"bot_token": tok, "app_token": "xapp-rot"}))
		if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
			t.Fatalf("connect %s status = %d (body=%s)", tok, rec.Code, rec.Body.String())
		}
	}

	rows := r.auditRows(orgID)
	if len(rows) != 2 {
		t.Fatalf("audit rows = %+v; want 2 (bind + rotation)", rows)
	}
	for i, row := range rows {
		if row.Action != domain.AccessActionCredentialSet {
			t.Errorf("row %d action = %q, want %q", i, row.Action, domain.AccessActionCredentialSet)
		}
	}
}

// Disconnect records a credential_removed that still names the workspace — the
// row it was read from is gone by the time an admin reads the log.
func TestHandleDelete_RecordsAccessChange(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-audit-delete")
	r.fake.stageConnectOK("xoxb-del", "T0DELA1", "Acme Corp", "U0BOTD", "A0DELA01", "")

	connect := httptest.NewRecorder()
	r.hdl.handleConnect(connect, r.req(http.MethodPost, "/api/slack/workspaces", owner, orgID, "",
		map[string]string{"bot_token": "xoxb-del", "app_token": "xapp-del"}))
	if connect.Code != http.StatusCreated {
		t.Fatalf("connect status = %d (body=%s)", connect.Code, connect.Body.String())
	}

	del := httptest.NewRecorder()
	r.hdl.handleDelete(del, r.reqDelete(owner, orgID, "T0DELA1", "A0DELA01"))
	if del.Code != http.StatusOK {
		t.Fatalf("delete status = %d (body=%s)", del.Code, del.Body.String())
	}

	rows := r.auditRows(orgID)
	if len(rows) != 2 {
		t.Fatalf("audit rows = %+v; want 2 (connect + disconnect)", rows)
	}
	got := rows[1]
	if got.Action != domain.AccessActionCredentialRemoved {
		t.Errorf("action = %q, want %q", got.Action, domain.AccessActionCredentialRemoved)
	}
	if got.Kind != domain.CredentialKindSlackWorkspace || got.Name != "Acme Corp" {
		t.Errorf("detail = (%q, %q); want the slack_workspace kind and the workspace name",
			got.Kind, got.Name)
	}
	if got.WorkspaceID != "T0DELA1" || got.APIAppID != "A0DELA01" {
		t.Errorf("ids = (%q, %q); want (T0DELA1, A0DELA01)", got.WorkspaceID, got.APIAppID)
	}
}

// Deleting a workspace the org never connected is a 404 and must leave no
// phantom revocation behind.
func TestHandleDelete_Missing_RecordsNothing(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-audit-nodel")

	rec := httptest.NewRecorder()
	r.hdl.handleDelete(rec, r.reqDelete(owner, orgID, "T0GHOST", "A0GHOST01"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete status = %d, want 404", rec.Code)
	}
	if rows := r.auditRows(orgID); len(rows) != 0 {
		t.Errorf("audit rows = %+v; want none for a delete that removed nothing", rows)
	}
}

// A connect that fails validation writes no secrets, so it must write no audit
// row either — the log may never claim a credential change that didn't happen.
func TestHandleConnect_Rejected_RecordsNothing(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-audit-reject")
	// No stageConnectOK call -> the fake answers {"ok":false,"error":"invalid_auth"}.

	rec := httptest.NewRecorder()
	r.hdl.handleConnect(rec, r.req(http.MethodPost, "/api/slack/workspaces", owner, orgID, "",
		map[string]string{"bot_token": "xoxb-bad", "app_token": "xapp-bad"}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("connect status = %d, want 400", rec.Code)
	}
	if rows := r.auditRows(orgID); len(rows) != 0 {
		t.Errorf("audit rows = %+v; want none for a rejected connect", rows)
	}
}
