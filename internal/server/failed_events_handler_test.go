package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// stubFailedEventsQueue records what the handler asked for and answers with
// canned data. It embeds the interface so the drain worker's methods — none
// of which this handler may call — panic loudly rather than silently
// returning a zero value if one is ever wired in here.
type stubFailedEventsQueue struct {
	db.EventQueueStore

	rows    []domain.FailedEvent
	listErr error

	gotOrgID string
	gotLimit int
	gotIDs   []int64

	requeued   int
	requeueErr error
}

func (s *stubFailedEventsQueue) ListFailedEvents(_ context.Context, orgID string, limit int) ([]domain.FailedEvent, error) {
	s.gotOrgID, s.gotLimit = orgID, limit
	return s.rows, s.listErr
}

func (s *stubFailedEventsQueue) RequeueFailedEvents(_ context.Context, orgID string, ids []int64) (int, error) {
	s.gotOrgID, s.gotIDs = orgID, ids
	return s.requeued, s.requeueErr
}

// localFailedEventsRig wires the handler in local mode, where the org-admin
// gate short-circuits to allowed (N=1 is admin of everything) — so these
// cases exercise the handler's own behavior with no authz store in the way.
// az is nil for the same reason: RequireOrgAdminRole returns before touching
// its receiver's fields in local mode.
func localFailedEventsRig(t *testing.T, q *stubFailedEventsQueue) *failedEventsHandler {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeLocal)
	return &failedEventsHandler{az: nil, queue: q}
}

// failedEventsReq builds a request carrying the session state withSession
// would normally seed: the active org and the caller's claims.
func failedEventsReq(method, target, orgID, caller, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	ctx := httpx.WithOrgID(r.Context(), orgID)
	ctx = httpx.WithClaims(ctx, &verify.Claims{Subject: caller})
	return r.WithContext(ctx)
}

func TestFailedEventsList_LocalMode(t *testing.T) {
	enqueued := time.Date(2026, 8, 7, 9, 30, 0, 0, time.UTC)
	q := &stubFailedEventsQueue{rows: []domain.FailedEvent{{
		ID:             42,
		EventType:      domain.EventGitHubPRCICheckFailed,
		EntityID:       "entity-1",
		EntitySource:   "github",
		EntitySourceID: "owner/repo#18",
		EntityTitle:    "Fix the flaky test",
		Attempts:       5,
		LastError:      "route: upsert task: db down (after 5 attempts)",
		EnqueuedAt:     enqueued,
	}}}
	h := localFailedEventsRig(t, q)

	rec := httptest.NewRecorder()
	h.handleFailedEventsList(rec, failedEventsReq(http.MethodGet, "/api/events/failed",
		runmode.LocalDefaultOrgID, "local-user", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Events []failedEventJSON `json:"events"`
		Count  int               `json:"count"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Count != 1 || len(resp.Events) != 1 {
		t.Fatalf("count=%d events=%d, want 1 and 1", resp.Count, len(resp.Events))
	}
	got := resp.Events[0]
	if got.ID != 42 || got.Attempts != 5 {
		t.Errorf("id/attempts = %d/%d, want 42/5", got.ID, got.Attempts)
	}
	// last_error is the whole reason an operator opens this panel — it must
	// survive to the wire verbatim, not be summarized away.
	if got.LastError != "route: upsert task: db down (after 5 attempts)" {
		t.Errorf("last_error = %q, want the store's reason verbatim", got.LastError)
	}
	if got.EntitySourceID != "owner/repo#18" || got.EntityTitle != "Fix the flaky test" {
		t.Errorf("entity fields = {%q %q}, want the display pair", got.EntitySourceID, got.EntityTitle)
	}
	if got.EnqueuedAt != "2026-08-07T09:30:00Z" {
		t.Errorf("enqueued_at = %q, want RFC3339 UTC", got.EnqueuedAt)
	}
	// The store, not the handler, resolves an unspecified limit — the handler
	// must forward the "unspecified" spelling rather than pick a number.
	if q.gotLimit != 0 {
		t.Errorf("limit forwarded = %d, want 0 (unspecified) when ?limit= is absent", q.gotLimit)
	}
	if q.gotOrgID != runmode.LocalDefaultOrgID {
		t.Errorf("org forwarded = %q, want the request's org", q.gotOrgID)
	}
}

func TestFailedEventsList_LimitAndErrors(t *testing.T) {
	t.Run("limit_forwarded", func(t *testing.T) {
		q := &stubFailedEventsQueue{}
		h := localFailedEventsRig(t, q)
		rec := httptest.NewRecorder()
		h.handleFailedEventsList(rec, failedEventsReq(http.MethodGet, "/api/events/failed?limit=25",
			runmode.LocalDefaultOrgID, "local-user", ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if q.gotLimit != 25 {
			t.Errorf("limit forwarded = %d, want 25", q.gotLimit)
		}
	})

	t.Run("non_numeric_limit_400", func(t *testing.T) {
		q := &stubFailedEventsQueue{}
		h := localFailedEventsRig(t, q)
		rec := httptest.NewRecorder()
		h.handleFailedEventsList(rec, failedEventsReq(http.MethodGet, "/api/events/failed?limit=lots",
			runmode.LocalDefaultOrgID, "local-user", ""))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})

	// An oversized limit is clamped by the store, not rejected — a diagnostics
	// read shouldn't fail over a display preference.
	t.Run("oversized_limit_forwarded_for_clamping", func(t *testing.T) {
		q := &stubFailedEventsQueue{}
		h := localFailedEventsRig(t, q)
		rec := httptest.NewRecorder()
		h.handleFailedEventsList(rec, failedEventsReq(http.MethodGet, "/api/events/failed?limit=100000",
			runmode.LocalDefaultOrgID, "local-user", ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if q.gotLimit != 100000 {
			t.Errorf("limit forwarded = %d, want the raw value (the store clamps)", q.gotLimit)
		}
	})

	t.Run("store_failure_500", func(t *testing.T) {
		q := &stubFailedEventsQueue{listErr: errors.New("db down")}
		h := localFailedEventsRig(t, q)
		rec := httptest.NewRecorder()
		h.handleFailedEventsList(rec, failedEventsReq(http.MethodGet, "/api/events/failed",
			runmode.LocalDefaultOrgID, "local-user", ""))
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rec.Code)
		}
	})

	// An empty parked population is the healthy state and must serialize as an
	// empty array, not null — the panel renders the list without a guard.
	t.Run("empty_list_is_an_array", func(t *testing.T) {
		q := &stubFailedEventsQueue{rows: []domain.FailedEvent{}}
		h := localFailedEventsRig(t, q)
		rec := httptest.NewRecorder()
		h.handleFailedEventsList(rec, failedEventsReq(http.MethodGet, "/api/events/failed",
			runmode.LocalDefaultOrgID, "local-user", ""))
		if !strings.Contains(rec.Body.String(), `"events":[]`) {
			t.Errorf("body = %s, want an empty events array", rec.Body.String())
		}
	})
}

func TestFailedEventsRequeue_LocalMode(t *testing.T) {
	t.Run("forwards_ids_and_reports_the_moved_count", func(t *testing.T) {
		// Three ids, two moved — the store counts out the id that is no longer
		// parked, and the handler reports what actually happened rather than
		// what was asked for.
		q := &stubFailedEventsQueue{requeued: 2}
		h := localFailedEventsRig(t, q)
		rec := httptest.NewRecorder()
		h.handleFailedEventsRequeue(rec, failedEventsReq(http.MethodPost, "/api/events/failed/requeue",
			runmode.LocalDefaultOrgID, "local-user", `{"ids":[1,2,3]}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Requeued int `json:"requeued"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Requeued != 2 {
			t.Errorf("requeued = %d, want 2", resp.Requeued)
		}
		if len(q.gotIDs) != 3 || q.gotIDs[0] != 1 || q.gotIDs[2] != 3 {
			t.Errorf("ids forwarded = %v, want [1 2 3]", q.gotIDs)
		}
	})

	t.Run("empty_ids_400", func(t *testing.T) {
		q := &stubFailedEventsQueue{}
		h := localFailedEventsRig(t, q)
		rec := httptest.NewRecorder()
		h.handleFailedEventsRequeue(rec, failedEventsReq(http.MethodPost, "/api/events/failed/requeue",
			runmode.LocalDefaultOrgID, "local-user", `{"ids":[]}`))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
		if q.gotIDs != nil {
			t.Errorf("store called with %v; an empty selection must not reach it", q.gotIDs)
		}
	})

	t.Run("oversized_selection_400", func(t *testing.T) {
		ids := make([]string, db.MaxFailedEventsLimit+1)
		for i := range ids {
			ids[i] = fmt.Sprint(i + 1)
		}
		q := &stubFailedEventsQueue{}
		h := localFailedEventsRig(t, q)
		rec := httptest.NewRecorder()
		h.handleFailedEventsRequeue(rec, failedEventsReq(http.MethodPost, "/api/events/failed/requeue",
			runmode.LocalDefaultOrgID, "local-user", `{"ids":[`+strings.Join(ids, ",")+`]}`))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("malformed_body_400", func(t *testing.T) {
		q := &stubFailedEventsQueue{}
		h := localFailedEventsRig(t, q)
		rec := httptest.NewRecorder()
		h.handleFailedEventsRequeue(rec, failedEventsReq(http.MethodPost, "/api/events/failed/requeue",
			runmode.LocalDefaultOrgID, "local-user", `{"ids":"all of them"}`))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("store_failure_500", func(t *testing.T) {
		q := &stubFailedEventsQueue{requeueErr: errors.New("db down")}
		h := localFailedEventsRig(t, q)
		rec := httptest.NewRecorder()
		h.handleFailedEventsRequeue(rec, failedEventsReq(http.MethodPost, "/api/events/failed/requeue",
			runmode.LocalDefaultOrgID, "local-user", `{"ids":[1]}`))
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rec.Code)
		}
	})
}

// TestFailedEventsHandler_AdminGate_Postgres pins the multi-mode gate against
// a real org: a plain member gets 403 on both endpoints and changes nothing,
// an org admin gets the parked row and can move it. There is no RLS backstop
// on this surface — EventQueueStore is admin-pool wired — so this predicate is
// the entire enforcement and is worth testing against real memberships rather
// than a stubbed checker.
func TestFailedEventsHandler_AdminGate_Postgres(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	h := pgtest.Shared(t)
	h.Reset(t)

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	s := New(h.AdminDB, stores, 3000)

	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, h, "failed-events-founder")
	member := pgtest.SeedUser(t, h, "failed-events-member")
	pgtest.AddOrgMember(t, h, member, orgID, teamID, "member", "member")

	// Park a row the honest way: enqueue, claim, MarkFailed.
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
	claimed, err := stores.EventQueue.ClaimNext(ctx, "gate-test-executor", 1)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNext: got=%v err=%v", claimed, err)
	}
	if err := stores.EventQueue.MarkFailed(ctx, orgID, claimed.ID, "route: db down (after 5 attempts)"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	fe := &failedEventsHandler{az: s.az, queue: stores.EventQueue}
	requeueBody := fmt.Sprintf(`{"ids":[%d]}`, claimed.ID)

	t.Run("member_list_403", func(t *testing.T) {
		rec := httptest.NewRecorder()
		fe.handleFailedEventsList(rec, failedEventsReq(http.MethodGet, "/api/events/failed", orgID, member, ""))
		if rec.Code != http.StatusForbidden {
			t.Errorf("list as plain member = %d, want 403; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("member_requeue_403_and_changes_nothing", func(t *testing.T) {
		rec := httptest.NewRecorder()
		fe.handleFailedEventsRequeue(rec, failedEventsReq(http.MethodPost, "/api/events/failed/requeue", orgID, member, requeueBody))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("requeue as plain member = %d, want 403; body=%s", rec.Code, rec.Body.String())
		}
		rows, err := stores.EventQueue.ListFailedEvents(ctx, orgID, 0)
		if err != nil {
			t.Fatalf("ListFailedEvents: %v", err)
		}
		if len(rows) != 1 || rows[0].ID != claimed.ID {
			t.Errorf("parked rows after the refused requeue = %+v, want the row still parked", rows)
		}
	})

	t.Run("admin_list_200", func(t *testing.T) {
		rec := httptest.NewRecorder()
		fe.handleFailedEventsList(rec, failedEventsReq(http.MethodGet, "/api/events/failed", orgID, owner, ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("list as org admin = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Events []failedEventJSON `json:"events"`
			Count  int               `json:"count"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Count != 1 || len(resp.Events) != 1 || resp.Events[0].ID != claimed.ID {
			t.Fatalf("admin list = %+v, want the one parked row", resp)
		}
		if resp.Events[0].EntityTitle != "Parked PR" {
			t.Errorf("entity_title = %q, want the joined entity's title", resp.Events[0].EntityTitle)
		}
		if resp.Events[0].LastError != "route: db down (after 5 attempts)" {
			t.Errorf("last_error = %q, want the park reason", resp.Events[0].LastError)
		}
	})

	t.Run("admin_requeue_200", func(t *testing.T) {
		rec := httptest.NewRecorder()
		fe.handleFailedEventsRequeue(rec, failedEventsReq(http.MethodPost, "/api/events/failed/requeue", orgID, owner, requeueBody))
		if rec.Code != http.StatusOK {
			t.Fatalf("requeue as org admin = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Requeued int `json:"requeued"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Requeued != 1 {
			t.Errorf("requeued = %d, want 1", resp.Requeued)
		}
		rows, _ := stores.EventQueue.ListFailedEvents(ctx, orgID, 0)
		if len(rows) != 0 {
			t.Errorf("parked rows after the requeue = %+v, want none", rows)
		}
	})
}
