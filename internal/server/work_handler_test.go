package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitemtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/workkinds"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// fakeWorkKind is a db.WorkKindHandle over the work-item conformance
// fixture table in an in-memory SQLite: a real table behind a kind whose
// access policy and controls the test chooses, which is what the supersede
// route and the access switch's default arm need — no registered kind
// offers the one or declares the other.
type fakeWorkKind struct {
	name     string
	kind     workitem.Kind
	conn     *sql.DB
	access   db.WorkAccess
	controls db.WorkControls
	// describeErr makes Describe fail, so the list's dependence on it can be
	// pinned.
	describeErr error
}

func (f *fakeWorkKind) Name() string              { return f.name }
func (f *fakeWorkKind) Label() string             { return "Fixture work" }
func (f *fakeWorkKind) Kind() workitem.Kind       { return f.kind }
func (f *fakeWorkKind) Conn() workitem.DBTX       { return f.conn }
func (f *fakeWorkKind) Access() db.WorkAccess     { return f.access }
func (f *fakeWorkKind) Controls() db.WorkControls { return f.controls }
func (f *fakeWorkKind) Objective() db.WorkObjective {
	return db.WorkObjective{OldestReadyAge: 30 * 1e9}
}

func (f *fakeWorkKind) Describe(_ context.Context, _ string, ids []int64) (map[int64]db.WorkSubject, error) {
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	out := map[int64]db.WorkSubject{}
	for _, id := range ids {
		out[id] = db.WorkSubject{Label: fmt.Sprintf("fixture#%d", id), Detail: "a fixture row", Fields: map[string]string{"payload": "p"}}
	}
	return out, nil
}

func newFakeWorkKind(t *testing.T, name string) *fakeWorkKind {
	t.Helper()
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() { conn.Close() })
	for _, stmt := range workitemtest.FixtureDDL(workitem.SQLite) {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatalf("fixture DDL: %v", err)
		}
	}
	return &fakeWorkKind{
		name: name,
		kind: workitem.Kind{
			Table: workitemtest.FixtureTable, Dialect: workitem.SQLite,
			Unique: workitem.UniqueWhileUnsettled, Strategy: workitem.SingleTx,
			Columns: []string{"payload", "frozen_col"},
		},
		conn:     conn,
		access:   db.WorkAccessOrgAdmin,
		controls: db.WorkControls{Redrive: true, Supersede: true, Cancel: true},
	}
}

// admit inserts a ready fixture row for the org and returns its id.
func (f *fakeWorkKind) admit(t *testing.T, orgID, key string) int64 {
	t.Helper()
	id, _, err := workitem.Admit(context.Background(), f.conn, f.kind, orgID, key, map[string]any{"payload": "p", "frozen_col": 0})
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	return id
}

// park stages one parked fixture row. The status is written directly rather
// than driven through a claim, because a claim takes the oldest ready row in
// the org and the tests here keep ready rows beside parked ones; the fixture
// is a shape for the handler to act on, and the package's own suite is where
// the path into that shape is proven.
func (f *fakeWorkKind) park(t *testing.T, orgID, key string) int64 {
	t.Helper()
	id := f.admit(t, orgID, key)
	if _, err := f.conn.Exec("UPDATE "+workitemtest.FixtureTable+
		" SET status = 'parked', attempt = 1, done_at = strftime('%Y-%m-%d %H:%M:%f','now'), last_outcome = 'permanent', last_error = 'rejected' WHERE id = ?", id); err != nil {
		t.Fatalf("park: %v", err)
	}
	return id
}

func (f *fakeWorkKind) status(t *testing.T, id int64) string {
	t.Helper()
	var status string
	if err := f.conn.QueryRow("SELECT status FROM "+workitemtest.FixtureTable+" WHERE id = ?", id).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	return status
}

// workReq builds a request the way withSession would seed it, with the path
// values the mux would have bound.
func workReq(method, path, orgID, caller, body string, pathValues map[string]string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	ctx := httpx.WithOrgID(r.Context(), orgID)
	if caller != "" {
		ctx = httpx.WithClaims(ctx, &verify.Claims{Subject: caller})
	}
	r = r.WithContext(ctx)
	r.SetPathValue("org_id", orgID)
	for k, v := range pathValues {
		r.SetPathValue(k, v)
	}
	return r
}

// localWorkRig wires the handler in local mode, where the org-admin gate
// short-circuits to allowed, so these cases exercise the handler's own
// behaviour with no authz store in the way.
func localWorkRig(t *testing.T, kinds ...db.WorkKindHandle) *workHandler {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeLocal)
	return &workHandler{az: nil, kinds: kinds}
}

func localEventQueueKind(t *testing.T) (db.WorkKindHandle, *sql.DB) {
	t.Helper()
	database, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { database.Close() })
	if err := db.BootstrapSchemaForTest(database); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return sqlitestore.New(database).EventQueue.(db.WorkKindHandle), database
}

// parkLocalEvent parks one event queue row against an entity and returns its
// queue id.
func parkLocalEvent(t *testing.T, database *sql.DB, title string) int64 {
	t.Helper()
	entityID := uuid.NewString()
	if _, err := database.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES (?, ?, 'github', ?, 'pr', ?, '', '{}', datetime('now'))
	`, entityID, runmode.LocalDefaultOrgID, "owner/repo#"+entityID[:6], title); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	store := sqlitestore.New(database).EventQueue
	if _, err := store.Enqueue(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EntityID: &entityID, EventType: domain.EventGitHubPRCICheckFailed,
	}, ""); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	batch, err := store.Claim(context.Background(), workitem.Owner{ID: "w", Epoch: 1}, 1)
	if err != nil || len(batch.Events) != 1 {
		t.Fatalf("Claim: %+v %v", batch, err)
	}
	if parked, err := store.Requeue(context.Background(), batch.Events[0].Receipt, workitem.OutcomePermanent, errors.New("route: db down")); err != nil || !parked {
		t.Fatalf("Requeue permanent: parked=%v err=%v", parked, err)
	}
	return batch.Events[0].Event.ID
}

func TestWorkCatalogue_LocalMode(t *testing.T) {
	h := localWorkRig(t, newFakeWorkKind(t, "fixture"))
	rec := httptest.NewRecorder()
	h.handleCatalogue(rec, workReq(http.MethodGet, "/api/orgs/x/work", runmode.LocalDefaultOrgID, "local-user", "", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Kinds []workKindJSON `json:"kinds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Kinds) != 1 || body.Kinds[0].Kind != "fixture" || body.Kinds[0].Label != "Fixture work" {
		t.Fatalf("kinds = %+v, want the one registered kind", body.Kinds)
	}
	if c := body.Kinds[0].Controls; !c.Redrive || !c.Supersede || !c.Cancel {
		t.Errorf("controls = %+v, want every control the fake offers", c)
	}
	if body.Kinds[0].Objective.OldestReadyAgeSeconds != 30 {
		t.Errorf("objective = %+v, want 30s", body.Kinds[0].Objective)
	}

	// The real registration: the event queue with its declared controls.
	eq, _ := localEventQueueKind(t)
	h = localWorkRig(t, eq)
	rec = httptest.NewRecorder()
	h.handleCatalogue(rec, workReq(http.MethodGet, "/api/orgs/x/work", runmode.LocalDefaultOrgID, "local-user", "", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Kinds) != 1 || body.Kinds[0].Kind != workkinds.EventQueueName || body.Kinds[0].Controls.Supersede || !body.Kinds[0].Controls.Redrive || !body.Kinds[0].Controls.Cancel {
		t.Errorf("event queue catalogue entry = %+v", body.Kinds)
	}
	if body.Kinds[0].Objective.OldestReadyAgeSeconds != 60 {
		t.Errorf("event queue objective = %+v, want 60s", body.Kinds[0].Objective)
	}
}

func TestWorkDepth_LocalMode(t *testing.T) {
	f := newFakeWorkKind(t, "fixture")
	f.admit(t, runmode.LocalDefaultOrgID, "ready-1")
	f.park(t, runmode.LocalDefaultOrgID, "parked-1")
	h := localWorkRig(t, f)

	rec := httptest.NewRecorder()
	h.handleDepth(rec, workReq(http.MethodGet, "/api/orgs/x/work/fixture/depth", runmode.LocalDefaultOrgID, "local-user", "", map[string]string{"kind": "fixture"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var d workDepthJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if d.Ready != 1 || d.Parked != 1 || d.Leased != 0 || d.Deferred != 0 {
		t.Errorf("depth = %+v, want one ready and one parked", d)
	}

	rec = httptest.NewRecorder()
	h.handleDepth(rec, workReq(http.MethodGet, "/api/orgs/x/work/nope/depth", runmode.LocalDefaultOrgID, "local-user", "", map[string]string{"kind": "nope"}))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown kind = %d, want 404", rec.Code)
	}
}

func TestWorkItemsList_LocalMode(t *testing.T) {
	eq, database := localEventQueueKind(t)
	parkedID := parkLocalEvent(t, database, "Fix the flaky test")
	h := localWorkRig(t, eq)
	list := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.handleItemsList(rec, workReq(http.MethodPost, "/api/orgs/x/work/event_queue/items/list", runmode.LocalDefaultOrgID, "local-user", body, map[string]string{"kind": "event_queue"}))
		return rec
	}

	t.Run("parked_row_with_subject", func(t *testing.T) {
		page := decodeList[workItemJSON](t, list(`{"status":"parked"}`))
		if page.Total() != 1 || len(page.Items) != 1 {
			t.Fatalf("total=%d items=%d, want 1 and 1", page.Total(), len(page.Items))
		}
		got := page.Items[0]
		if got.ID != parkedID || got.Kind != "event_queue" || got.Status != "parked" || got.Attempt != 1 || got.MaxAttempts != 5 {
			t.Errorf("item = %+v", got)
		}
		// last_error is the whole reason an operator opens this panel; it
		// survives to the wire verbatim.
		if got.LastError != "route: db down" || got.LastOutcome != "permanent" {
			t.Errorf("outcome/error = %q/%q", got.LastOutcome, got.LastError)
		}
		if got.Subject == nil || got.Subject.Detail != "Fix the flaky test" || got.Subject.Fields["event_type"] != domain.EventGitHubPRCICheckFailed {
			t.Errorf("subject = %+v, want the entity's description", got.Subject)
		}
		if got.Lease != nil || got.Cancel != nil || got.DoneAt == "" || got.FirstEnqueuedAt == "" {
			t.Errorf("parked row carries lease=%v cancel=%v done_at=%q", got.Lease, got.Cancel, got.DoneAt)
		}
	})

	t.Run("every_status_accepted_unknown_rejected", func(t *testing.T) {
		for _, status := range workitem.ListStatuses {
			if rec := list(`{"status":"` + status + `"}`); rec.Code != http.StatusOK {
				t.Errorf("status %q = %d, want 200; body=%s", status, rec.Code, rec.Body.String())
			}
		}
		rec := list(`{"status":"processing"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("unknown status = %d, want 400", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"field":"status"`) || !strings.Contains(rec.Body.String(), httpx.ReasonInvalidField) {
			t.Errorf("body = %s, want INVALID_FIELD naming status", rec.Body.String())
		}
	})

	t.Run("count_only", func(t *testing.T) {
		page := decodeList[workItemJSON](t, list(`{"status":"parked","page_size":0}`))
		if page.Total() != 1 || len(page.Items) != 0 {
			t.Errorf("count-only: total=%d items=%d, want 1 and none", page.Total(), len(page.Items))
		}
	})

	t.Run("token_fingerprint_refuses_another_status", func(t *testing.T) {
		parkLocalEvent(t, database, "Another")
		page := decodeList[workItemJSON](t, list(`{"status":"parked","page_size":1}`))
		if page.NextPageToken == "" {
			t.Fatal("no next page token with two parked rows and a page of one")
		}
		rec := list(`{"status":"ready","page_size":1,"page_token":"` + page.NextPageToken + `"}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("page 2 of another status = %d, want 400", rec.Code)
		}
		if rec := list(`{"status":"parked","page_size":1,"page_token":"` + page.NextPageToken + `"}`); rec.Code != http.StatusOK {
			t.Errorf("page 2 of the same status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("unknown_field_400", func(t *testing.T) {
		if rec := list(`{"limit":25}`); rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("empty_list_is_an_array", func(t *testing.T) {
		if rec := list(`{"status":"cancelled"}`); !strings.Contains(rec.Body.String(), `"items":[]`) {
			t.Errorf("body = %s, want an empty items array", rec.Body.String())
		}
	})
}

func TestWorkItemGet_LocalMode(t *testing.T) {
	eq, database := localEventQueueKind(t)
	parkedID := parkLocalEvent(t, database, "Fix the flaky test")
	h := localWorkRig(t, eq)
	get := func(id string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.handleItemGet(rec, workReq(http.MethodGet, "/api/orgs/x/work/event_queue/items/"+id, runmode.LocalDefaultOrgID, "local-user", "", map[string]string{"kind": "event_queue", "id": id}))
		return rec
	}

	rec := get(fmt.Sprint(parkedID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got workItemJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	listRec := httptest.NewRecorder()
	h.handleItemsList(listRec, workReq(http.MethodPost, "/api/orgs/x/work/event_queue/items/list", runmode.LocalDefaultOrgID, "local-user", `{}`, map[string]string{"kind": "event_queue"}))
	page := decodeList[workItemJSON](t, listRec)
	if len(page.Items) != 1 {
		t.Fatalf("list = %+v", page.Items)
	}
	if a, b := mustJSON(t, got), mustJSON(t, page.Items[0]); a != b {
		t.Errorf("single read = %s, want the list row %s", a, b)
	}

	for _, tc := range []struct {
		name, id string
		want     int
	}{
		{"unknown_id_404", "999999", http.StatusNotFound},
		{"malformed_id_400", "not-a-number", http.StatusBadRequest},
		{"non_positive_id_400", "0", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rec := get(tc.id); rec.Code != tc.want {
				t.Errorf("status = %d, want %d; body=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestWorkRedrive_LocalMode(t *testing.T) {
	f := newFakeWorkKind(t, "fixture")
	h := localWorkRig(t, f)
	redrive := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.handleRedrive(rec, workReq(http.MethodPost, "/api/orgs/x/work/fixture/items/redrive", runmode.LocalDefaultOrgID, "local-user", body, map[string]string{"kind": "fixture"}))
		return rec
	}

	t.Run("counts_out_rows_that_are_not_parked", func(t *testing.T) {
		parked := f.park(t, runmode.LocalDefaultOrgID, "p1")
		ready := f.admit(t, runmode.LocalDefaultOrgID, "r1")
		rec := redrive(fmt.Sprintf(`{"ids":[%d,%d,999999]}`, parked, ready))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"redriven":1`) {
			t.Errorf("body = %s, want redriven 1", rec.Body.String())
		}
		if got := f.status(t, parked); got != workitem.StatusReady {
			t.Errorf("parked row after redrive = %q, want ready", got)
		}
	})

	t.Run("empty_ids_400", func(t *testing.T) {
		rec := redrive(`{"ids":[]}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), httpx.ReasonMissingField) {
			t.Errorf("status = %d body=%s, want 400 MISSING_FIELD", rec.Code, rec.Body.String())
		}
	})

	t.Run("oversized_selection_400", func(t *testing.T) {
		ids := make([]string, db.MaxRedriveIDs+1)
		for i := range ids {
			ids[i] = fmt.Sprint(i + 1)
		}
		rec := redrive(`{"ids":[` + strings.Join(ids, ",") + `]}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), httpx.ReasonOutOfRange) {
			t.Errorf("status = %d body=%s, want 400 OUT_OF_RANGE", rec.Code, rec.Body.String())
		}
	})

	t.Run("malformed_body_400", func(t *testing.T) {
		if rec := redrive(`{"ids":"all of them"}`); rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("kind_without_the_control_422", func(t *testing.T) {
		f.controls.Redrive = false
		defer func() { f.controls.Redrive = true }()
		parked := f.park(t, runmode.LocalDefaultOrgID, "p2")
		rec := redrive(fmt.Sprintf(`{"ids":[%d]}`, parked))
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), httpx.ReasonInvalidField) {
			t.Errorf("status = %d body=%s, want 422 INVALID_FIELD", rec.Code, rec.Body.String())
		}
		if got := f.status(t, parked); got != workitem.StatusParked {
			t.Errorf("row moved to %q through a refused control", got)
		}
	})
}

func TestWorkCancel_LocalMode(t *testing.T) {
	f := newFakeWorkKind(t, "fixture")
	h := localWorkRig(t, f)
	cancel := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.handleCancel(rec, workReq(http.MethodPost, "/api/orgs/x/work/fixture/items/cancel", runmode.LocalDefaultOrgID, "local-user", body, map[string]string{"kind": "fixture"}))
		return rec
	}

	t.Run("records_a_request_and_counts_out_settled_ids", func(t *testing.T) {
		ready := f.admit(t, runmode.LocalDefaultOrgID, "c1")
		parked := f.park(t, runmode.LocalDefaultOrgID, "c2")
		rec := cancel(fmt.Sprintf(`{"ids":[%d,%d,999999],"reason":"  not needed  "}`, ready, parked))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"requested":1`) {
			t.Errorf("body = %s, want requested 1", rec.Body.String())
		}
		// A request, not a status change: the row is still ready until a
		// claim settles it, with the trimmed reason on record.
		if got := f.status(t, ready); got != workitem.StatusReady {
			t.Errorf("ready row after a cancel request = %q, want still ready", got)
		}
		it, err := workitem.Get(context.Background(), f.conn, f.kind, runmode.LocalDefaultOrgID, ready)
		if err != nil || it == nil || it.CancelRequestedAt == nil || it.CancelReason != "not needed" || it.CancelRequestedBy != "local-user" {
			t.Errorf("row = %+v err=%v, want the request recorded by the caller with the trimmed reason", it, err)
		}
		// A second request against the same row is counted out.
		if rec := cancel(fmt.Sprintf(`{"ids":[%d],"reason":"again"}`, ready)); !strings.Contains(rec.Body.String(), `"requested":0`) {
			t.Errorf("second request body = %s, want requested 0", rec.Body.String())
		}
	})

	t.Run("reason_required_and_bounded", func(t *testing.T) {
		id := f.admit(t, runmode.LocalDefaultOrgID, "c3")
		rec := cancel(fmt.Sprintf(`{"ids":[%d],"reason":"   "}`, id))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), httpx.ReasonMissingField) {
			t.Errorf("blank reason: status = %d body=%s, want 400 MISSING_FIELD", rec.Code, rec.Body.String())
		}
		rec = cancel(fmt.Sprintf(`{"ids":[%d],"reason":"%s"}`, id, strings.Repeat("x", maxCancelReasonLen+1)))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), httpx.ReasonInvalidField) {
			t.Errorf("long reason: status = %d body=%s, want 400 INVALID_FIELD", rec.Code, rec.Body.String())
		}
		// Every failing field is reported at once.
		rec = cancel(`{"ids":[],"reason":""}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"field":"ids"`) || !strings.Contains(rec.Body.String(), `"field":"reason"`) {
			t.Errorf("both faults: status = %d body=%s", rec.Code, rec.Body.String())
		}
		if got := f.status(t, id); got != workitem.StatusReady {
			t.Errorf("row = %q after refused requests", got)
		}
	})

	t.Run("kind_without_the_control_422", func(t *testing.T) {
		f.controls.Cancel = false
		defer func() { f.controls.Cancel = true }()
		id := f.admit(t, runmode.LocalDefaultOrgID, "c4")
		if rec := cancel(fmt.Sprintf(`{"ids":[%d],"reason":"stop"}`, id)); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
		}
	})
}

func TestWorkSupersede_LocalMode(t *testing.T) {
	f := newFakeWorkKind(t, "fixture")
	h := localWorkRig(t, f)
	supersede := func(kind, id, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.handleSupersede(rec, workReq(http.MethodPost, "/api/orgs/x/work/"+kind+"/items/"+id+"/supersede", runmode.LocalDefaultOrgID, "local-user", body, map[string]string{"kind": kind, "id": id}))
		return rec
	}

	parked := f.park(t, runmode.LocalDefaultOrgID, "s1")
	replacement := f.admit(t, runmode.LocalDefaultOrgID, "s2")

	rec := supersede("fixture", fmt.Sprint(parked), fmt.Sprintf(`{"superseded_by":%d}`, replacement))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"superseded":1`) {
		t.Fatalf("status = %d body=%s, want 200 superseded 1", rec.Code, rec.Body.String())
	}
	if got := f.status(t, parked); got != workitem.StatusCancelled {
		t.Errorf("superseded row = %q, want cancelled", got)
	}
	// A row that is no longer parked is counted out, not an error.
	if rec := supersede("fixture", fmt.Sprint(parked), fmt.Sprintf(`{"superseded_by":%d}`, replacement)); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"superseded":0`) {
		t.Errorf("second supersede: status = %d body=%s, want 200 superseded 0", rec.Code, rec.Body.String())
	}
	if rec := supersede("fixture", fmt.Sprint(replacement), fmt.Sprintf(`{"superseded_by":%d}`, replacement)); rec.Code != http.StatusBadRequest {
		t.Errorf("self-supersede = %d, want 400", rec.Code)
	}
	if rec := supersede("fixture", fmt.Sprint(replacement), `{"superseded_by":0}`); rec.Code != http.StatusBadRequest {
		t.Errorf("no replacement = %d, want 400", rec.Code)
	}
	if rec := supersede("fixture", "abc", `{"superseded_by":1}`); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), httpx.ReasonInvalidID) {
		t.Errorf("malformed id = %d body=%s, want 400 INVALID_ID", rec.Code, rec.Body.String())
	}

	// The event queue offers no supersede: 422 for every real kind.
	eq, database := localEventQueueKind(t)
	eqParked := parkLocalEvent(t, database, "Parked")
	h = localWorkRig(t, eq)
	if rec := supersede("event_queue", fmt.Sprint(eqParked), `{"superseded_by":1}`); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("event queue supersede = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

// TestWorkHandler_UnknownAccessPolicyIsRefused pins the access switch's
// default arm: a kind declaring a policy the handler does not know answers
// 500 on every route, never 200, and the row is untouched.
func TestWorkHandler_UnknownAccessPolicyIsRefused(t *testing.T) {
	f := newFakeWorkKind(t, "fixture")
	f.access = db.WorkAccess(99)
	parked := f.park(t, runmode.LocalDefaultOrgID, "u1")
	h := localWorkRig(t, f)
	pv := map[string]string{"kind": "fixture", "id": fmt.Sprint(parked)}

	routes := []struct {
		name string
		call func(w http.ResponseWriter, r *http.Request)
		req  *http.Request
	}{
		{"catalogue", h.handleCatalogue, workReq(http.MethodGet, "/", runmode.LocalDefaultOrgID, "u", "", pv)},
		{"depth", h.handleDepth, workReq(http.MethodGet, "/", runmode.LocalDefaultOrgID, "u", "", pv)},
		{"list", h.handleItemsList, workReq(http.MethodPost, "/", runmode.LocalDefaultOrgID, "u", `{}`, pv)},
		{"get", h.handleItemGet, workReq(http.MethodGet, "/", runmode.LocalDefaultOrgID, "u", "", pv)},
		{"redrive", h.handleRedrive, workReq(http.MethodPost, "/", runmode.LocalDefaultOrgID, "u", fmt.Sprintf(`{"ids":[%d]}`, parked), pv)},
		{"cancel", h.handleCancel, workReq(http.MethodPost, "/", runmode.LocalDefaultOrgID, "u", fmt.Sprintf(`{"ids":[%d],"reason":"x"}`, parked), pv)},
		{"supersede", h.handleSupersede, workReq(http.MethodPost, "/", runmode.LocalDefaultOrgID, "u", `{"superseded_by":1}`, pv)},
	}
	for _, rt := range routes {
		rec := httptest.NewRecorder()
		rt.call(rec, rt.req)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s under an unknown policy = %d, want 500; body=%s", rt.name, rec.Code, rec.Body.String())
		}
	}
	if got := f.status(t, parked); got != workitem.StatusParked {
		t.Errorf("row = %q, want untouched", got)
	}
}

// TestWorkHandler_DescribeFailureIs500 pins that a page without subjects is
// not served: an operator acting on a row needs to know what it is.
func TestWorkHandler_DescribeFailureIs500(t *testing.T) {
	f := newFakeWorkKind(t, "fixture")
	f.park(t, runmode.LocalDefaultOrgID, "d1")
	f.describeErr = errors.New("join broke")
	h := localWorkRig(t, f)
	rec := httptest.NewRecorder()
	h.handleItemsList(rec, workReq(http.MethodPost, "/", runmode.LocalDefaultOrgID, "u", `{"status":"parked"}`, map[string]string{"kind": "fixture"}))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

// TestWorkHandler_Gate_Postgres pins the multi-mode gate against real
// memberships: no session is 401, a plain member 403 on every route and
// changes nothing, a token sealed to another org 404, and an org admin gets
// the row and can move it. There is no RLS backstop on this surface — every
// kind runs on the admin pool — so this predicate is the entire enforcement.
func TestWorkHandler_Gate_Postgres(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	h := pgtest.Shared(t)
	h.Reset(t)

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	s := New(h.AdminDB, stores)

	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, h, "work-founder")
	member := pgtest.SeedUser(t, h, "work-member")
	pgtest.AddOrgMember(t, h, member, orgID, teamID, "member", "member")

	entityID := uuid.New().String()
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES ($1, $2, 'github', $3, 'pr', 'Parked PR', '', '{}'::jsonb, now())
	`, entityID, orgID, "owner/repo#"+entityID[:8])
	ctx := context.Background()
	if _, err := stores.EventQueue.Enqueue(ctx, orgID, domain.Event{
		EntityID: &entityID, EventType: domain.EventGitHubPRCICheckFailed,
	}, ""); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	batch, err := stores.EventQueue.Claim(ctx, workitem.Owner{ID: "gate-test-executor", Epoch: 1}, 1)
	if err != nil || len(batch.Events) != 1 {
		t.Fatalf("Claim: got=%+v err=%v", batch, err)
	}
	claimed := batch.Events[0].Event
	if parked, err := stores.EventQueue.Requeue(ctx, batch.Events[0].Receipt, workitem.OutcomePermanent, errors.New("route: db down")); err != nil || !parked {
		t.Fatalf("Requeue permanent: parked=%v err=%v", parked, err)
	}
	handle := stores.EventQueue.(db.WorkKindHandle)

	wh := &workHandler{az: s.az, kinds: stores.WorkKinds}
	pv := map[string]string{"kind": "event_queue", "id": fmt.Sprint(claimed.ID)}
	redriveBody := fmt.Sprintf(`{"ids":[%d]}`, claimed.ID)
	cancelBody := fmt.Sprintf(`{"ids":[%d],"reason":"stop"}`, claimed.ID)
	routes := []struct {
		name   string
		call   func(w http.ResponseWriter, r *http.Request)
		method string
		body   string
	}{
		{"catalogue", wh.handleCatalogue, http.MethodGet, ""},
		{"depth", wh.handleDepth, http.MethodGet, ""},
		{"list", wh.handleItemsList, http.MethodPost, `{"status":"parked"}`},
		{"get", wh.handleItemGet, http.MethodGet, ""},
		{"redrive", wh.handleRedrive, http.MethodPost, redriveBody},
		{"cancel", wh.handleCancel, http.MethodPost, cancelBody},
		{"supersede", wh.handleSupersede, http.MethodPost, `{"superseded_by":1}`},
	}
	stillParked := func(t *testing.T) {
		t.Helper()
		it, err := workitem.Get(ctx, handle.Conn(), handle.Kind(), orgID, claimed.ID)
		if err != nil || it == nil || it.Status != workitem.StatusParked || it.CancelRequestedAt != nil {
			t.Errorf("row after a refused call = %+v err=%v, want still parked with no request", it, err)
		}
	}

	t.Run("no_session_401", func(t *testing.T) {
		for _, rt := range routes {
			rec := httptest.NewRecorder()
			rt.call(rec, workReq(rt.method, "/", orgID, "", rt.body, pv))
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s without a session = %d, want 401", rt.name, rec.Code)
			}
		}
		stillParked(t)
	})

	t.Run("member_403_and_changes_nothing", func(t *testing.T) {
		for _, rt := range routes {
			rec := httptest.NewRecorder()
			rt.call(rec, workReq(rt.method, "/", orgID, member, rt.body, pv))
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s as a plain member = %d, want 403; body=%s", rt.name, rec.Code, rec.Body.String())
			}
		}
		stillParked(t)
	})

	t.Run("token_sealed_to_another_org_404", func(t *testing.T) {
		for _, rt := range routes {
			r := workReq(rt.method, "/", orgID, owner, rt.body, pv)
			r = r.WithContext(httpx.WithTokenAuth(r.Context(), &httpx.TokenAuth{TokenID: "tok", OrgID: uuid.NewString()}))
			rec := httptest.NewRecorder()
			rt.call(rec, r)
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s with a foreign token = %d, want 404; body=%s", rt.name, rec.Code, rec.Body.String())
			}
		}
		stillParked(t)
	})

	t.Run("malformed_org_404", func(t *testing.T) {
		rec := httptest.NewRecorder()
		wh.handleCatalogue(rec, workReq(http.MethodGet, "/", "not-a-uuid", owner, "", pv))
		if rec.Code != http.StatusNotFound {
			t.Errorf("malformed org = %d, want 404", rec.Code)
		}
	})

	t.Run("admin_reads_and_redrives", func(t *testing.T) {
		rec := httptest.NewRecorder()
		wh.handleItemsList(rec, workReq(http.MethodPost, "/", orgID, owner, `{"status":"parked"}`, pv))
		page := decodeList[workItemJSON](t, rec)
		if page.Total() != 1 || len(page.Items) != 1 || page.Items[0].ID != claimed.ID {
			t.Fatalf("admin list = %+v (total %d), want the one parked row", page.Items, page.Total())
		}
		if page.Items[0].Subject == nil || page.Items[0].Subject.Detail != "Parked PR" {
			t.Errorf("subject = %+v, want the joined entity's title", page.Items[0].Subject)
		}

		rec = httptest.NewRecorder()
		wh.handleDepth(rec, workReq(http.MethodGet, "/", orgID, owner, "", pv))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"parked":1`) {
			t.Errorf("depth = %d %s, want parked 1", rec.Code, rec.Body.String())
		}

		rec = httptest.NewRecorder()
		wh.handleRedrive(rec, workReq(http.MethodPost, "/", orgID, owner, redriveBody, pv))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"redriven":1`) {
			t.Fatalf("admin redrive = %d %s, want redriven 1", rec.Code, rec.Body.String())
		}
		it, err := workitem.Get(ctx, handle.Conn(), handle.Kind(), orgID, claimed.ID)
		if err != nil || it == nil || it.Status != workitem.StatusReady {
			t.Errorf("row after the admin redrive = %+v err=%v, want ready", it, err)
		}
	})
}
