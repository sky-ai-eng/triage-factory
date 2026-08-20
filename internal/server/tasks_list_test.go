package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// listedTask is the slice of the task payload these tests assert on. The full
// shape is pinned by the handler's own taskJSON; here we only care about which
// rows came back, in what order.
type listedTask struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type taskListBody struct {
	Items         []listedTask `json:"items"`
	NextPageToken string       `json:"next_page_token"`
	TotalCount    int          `json:"total_count"`
}

// postTaskList calls POST /api/tasks/list, asserts a 200, and decodes the
// envelope.
func postTaskList(t *testing.T, s *Server, body map[string]any) taskListBody {
	t.Helper()
	rec := doJSON(t, s, http.MethodPost, "/api/tasks/list", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out taskListBody
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode list envelope: %v (body=%s)", err, rec.Body.String())
	}
	return out
}

func listedIDs(page taskListBody) []string {
	ids := make([]string, len(page.Items))
	for i, item := range page.Items {
		ids[i] = item.ID
	}
	return ids
}

// taskFixture describes one seeded row for the projection tests.
type taskFixture struct {
	name        string
	status      string
	claimedUser bool
	claimedBot  bool
	snoozeUntil *time.Time
	closedAt    *time.Time
	// inQueue is what the queue projection must answer for this row.
	inQueue bool
}

// seedTaskFixture inserts an entity + event + task for one fixture and returns
// the task id. It writes the tasks row directly because several of these
// states (a future snooze window, a closed_at in the past) have no store
// method that produces them, and the point of the fixture set is the states
// the queue filter has to exclude.
func seedTaskFixture(t *testing.T, database *sql.DB, f taskFixture) string {
	t.Helper()
	now := time.Now().UTC()
	entityID := uuid.New().String()
	eventID := uuid.New().String()
	taskID := uuid.New().String()
	sourceID := fmt.Sprintf("list-%s-%d", f.name, now.UnixNano())

	execSQL(t, database, `
		INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES (?, 'github', ?, 'pr', ?, ?, '{}', ?)`,
		entityID, sourceID, "List fixture "+f.name, "https://example/"+sourceID, now)
	execSQL(t, database, `
		INSERT INTO events (id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES (?, ?, ?, '', '{}', ?)`,
		eventID, entityID, domain.EventGitHubPRCICheckFailed, now)

	var userClaim, botClaim, snooze, closed any
	if f.claimedUser {
		userClaim = runmode.LocalDefaultUserID
	}
	if f.claimedBot {
		botClaim = runmode.LocalDefaultAgentID
	}
	if f.snoozeUntil != nil {
		snooze = *f.snoozeUntil
	}
	if f.closedAt != nil {
		closed = *f.closedAt
	}
	// team_id is set on every fixture: the claimed-⇒-owned CHECK rejects a
	// claim on a task with no owning team, so a NULL-team fixture couldn't
	// carry the claimed states this set exists to cover.
	execSQL(t, database, `
		INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id,
		                   status, priority_score, scoring_status, created_at,
		                   team_id, visibility,
		                   claimed_by_user_id, claimed_by_agent_id, snooze_until, closed_at)
		VALUES (?, ?, ?, '', ?, ?, 0.5, 'pending', ?, ?, 'team', ?, ?, ?, ?)`,
		taskID, entityID, domain.EventGitHubPRCICheckFailed, eventID, f.status, now,
		runmode.LocalDefaultTeamID, userClaim, botClaim, snooze, closed)
	return taskID
}

// queueProjectionBody is the body the triage deck and the board's Queued
// column send — the filter set that replaced GET /api/queue.
func queueProjectionBody() map[string]any {
	return map[string]any{
		"statuses":        []string{"queued"},
		"only_unclaimed":  true,
		"include_snoozed": false,
	}
}

// TestTaskList_QueueProjectionParity is the regression net for the alias
// divergence this route retires. GET /api/queue and GET /api/tasks?status=queued
// answered the same nominal filter with different hidden ones — the latter
// returned claimed and future-snoozed rows the queue view hides — and the fix
// is only real if the surviving route reproduces the *queue's* answer exactly.
//
// The oracle is the removed /api/queue store query, verbatim: the fixture set
// spans every state the two spellings disagreed on (claimed by a user, claimed
// by the bot, snoozed into the future, snoozed in the past, terminal), and the
// new route's queue projection must return the same task set the old SQL does.
// Sets, not sequences: the old ORDER BY had no id tiebreaker, so its order
// among rows tying on every key was never defined (which is the bug the new
// ordering fixes).
func TestTaskList_QueueProjectionParity(t *testing.T) {
	s := newTestServer(t)
	past := time.Now().UTC().Add(-2 * time.Hour)
	future := time.Now().UTC().Add(2 * time.Hour)

	fixtures := []taskFixture{
		{name: "pickable", status: "queued", inQueue: true},
		{name: "pickable-woke", status: "queued", snoozeUntil: &past, inQueue: true},
		{name: "user-claimed", status: "queued", claimedUser: true},
		{name: "bot-claimed", status: "queued", claimedBot: true},
		// A queued row still inside a snooze window: the defensive half of
		// the old filter, and the state ?status=queued surfaced and
		// /api/queue didn't.
		{name: "queued-snoozed-future", status: "queued", snoozeUntil: &future},
		{name: "snoozed-future", status: "snoozed", snoozeUntil: &future},
		{name: "snoozed-past", status: "snoozed", snoozeUntil: &past},
		{name: "in-progress", status: "in_progress", claimedUser: true},
		{name: "done", status: "done", closedAt: &past},
		{name: "dismissed", status: "dismissed", closedAt: &past},
	}
	var wantQueue []string
	for _, f := range fixtures {
		id := seedTaskFixture(t, s.db, f)
		if f.inQueue {
			wantQueue = append(wantQueue, id)
		}
	}

	got := listedIDs(postTaskList(t, s, queueProjectionBody()))
	slices.Sort(got)
	slices.Sort(wantQueue)
	if !slices.Equal(got, wantQueue) {
		t.Errorf("queue projection = %v, want %v", got, wantQueue)
	}

	// The oracle: the exact query GET /api/queue ran, as it stood before this
	// route replaced it. Any drift in the new filter's meaning shows up here
	// even if the hand-written expectation above drifted with it.
	rows, err := s.db.Query(`
		SELECT t.id
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id
		LEFT JOIN (
			SELECT org_id, event_type, MIN(sort_order) AS sort_order
			FROM event_handlers
			WHERE enabled = 1 AND kind = 'rule'
			GROUP BY org_id, event_type
		) tr ON t.event_type = tr.event_type AND t.org_id = tr.org_id
		WHERE t.status = 'queued'
			AND t.claimed_by_agent_id IS NULL
			AND t.claimed_by_user_id  IS NULL
			AND (t.snooze_until IS NULL OR t.snooze_until <= datetime('now'))
		ORDER BY COALESCE(tr.sort_order, 999) ASC, COALESCE(t.priority_score, 0.5) DESC
	`)
	if err != nil {
		t.Fatalf("oracle query: %v", err)
	}
	defer rows.Close()
	var oracle []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan oracle row: %v", err)
		}
		oracle = append(oracle, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("oracle rows: %v", err)
	}
	slices.Sort(oracle)
	if !slices.Equal(got, oracle) {
		t.Errorf("queue projection = %v, old /api/queue query = %v", got, oracle)
	}
}

// TestTaskList_ShowSnoozedProjection pins the board's "show snoozed" toggle:
// widening the status set and keeping the snooze window surfaces deferred
// rows, and they sort behind the pickable ones.
func TestTaskList_ShowSnoozedProjection(t *testing.T) {
	s := newTestServer(t)
	future := time.Now().UTC().Add(2 * time.Hour)
	live := seedTaskFixture(t, s.db, taskFixture{name: "snz-live", status: "queued"})
	deferred := seedTaskFixture(t, s.db, taskFixture{name: "snz-deferred", status: "snoozed", snoozeUntil: &future})

	page := postTaskList(t, s, map[string]any{
		"statuses":        []string{"queued", "snoozed"},
		"only_unclaimed":  true,
		"include_snoozed": true,
	})
	if want := []string{live, deferred}; !slices.Equal(listedIDs(page), want) {
		t.Errorf("show-snoozed projection = %v, want %v (snoozed rows sort behind live ones)", listedIDs(page), want)
	}
	if page.TotalCount != 2 {
		t.Errorf("total_count = %d, want 2", page.TotalCount)
	}
}

// TestTaskList_ClosedSinceIsExplicit pins that the seven-day window the old
// done column applied behind the caller's back is now the caller's to pass.
// No hidden default: without closed_since, an old closure still lists.
func TestTaskList_ClosedSinceIsExplicit(t *testing.T) {
	s := newTestServer(t)
	old := time.Now().UTC().AddDate(0, 0, -30)
	recent := time.Now().UTC().Add(-time.Hour)
	oldID := seedTaskFixture(t, s.db, taskFixture{name: "cs-old", status: "done", closedAt: &old})
	recentID := seedTaskFixture(t, s.db, taskFixture{name: "cs-recent", status: "done", closedAt: &recent})

	unwindowed := postTaskList(t, s, map[string]any{"statuses": []string{"done"}})
	if got := listedIDs(unwindowed); len(got) != 2 {
		t.Errorf("unwindowed done = %v, want both %s and %s (no hidden window)", got, oldID, recentID)
	}

	// Milliseconds on the wire: the browser sends this field as
	// Date.toISOString(), which always carries a fractional second. Go's
	// time.Parse accepts one after the seconds field even though
	// time.RFC3339's layout doesn't spell it, so the two agree — pinned here
	// because the layout constant reads as though they wouldn't.
	windowed := postTaskList(t, s, map[string]any{
		"statuses":     []string{"done"},
		"closed_since": time.Now().UTC().AddDate(0, 0, -7).Format("2006-01-02T15:04:05.000Z07:00"),
	})
	if want := []string{recentID}; !slices.Equal(listedIDs(windowed), want) {
		t.Errorf("7-day window = %v, want %v", listedIDs(windowed), want)
	}
	if windowed.TotalCount != 1 {
		t.Errorf("windowed total_count = %d, want 1 (the filtered total)", windowed.TotalCount)
	}

	// And a seconds-precision timestamp, the other spelling of the same
	// field, is accepted too.
	plain := postTaskList(t, s, map[string]any{
		"statuses":     []string{"done"},
		"closed_since": time.Now().UTC().AddDate(0, 0, -7).Format(time.RFC3339),
	})
	if want := []string{recentID}; !slices.Equal(listedIDs(plain), want) {
		t.Errorf("7-day window (seconds precision) = %v, want %v", listedIDs(plain), want)
	}
}

// TestTaskList_PageTokenRoundTrip walks a result set page by page: each page
// carries the filtered total, the token addresses the next window, and the
// last page carries no token at all.
func TestTaskList_PageTokenRoundTrip(t *testing.T) {
	s := newTestServer(t)
	for i := range 3 {
		seedTaskFixture(t, s.db, taskFixture{name: fmt.Sprintf("page-%d", i), status: "queued"})
	}

	body := queueProjectionBody()
	body["page_size"] = 2
	first := postTaskList(t, s, body)
	if len(first.Items) != 2 || first.TotalCount != 3 {
		t.Fatalf("first page = %d items / total %d, want 2 / 3", len(first.Items), first.TotalCount)
	}
	if first.NextPageToken == "" {
		t.Fatal("first page carries no next_page_token but two of three rows were returned")
	}

	body["page_token"] = first.NextPageToken
	second := postTaskList(t, s, body)
	if len(second.Items) != 1 || second.TotalCount != 3 {
		t.Fatalf("second page = %d items / total %d, want 1 / 3", len(second.Items), second.TotalCount)
	}
	if second.NextPageToken != "" {
		t.Errorf("last page carries next_page_token %q, want it absent", second.NextPageToken)
	}

	walked := append(listedIDs(first), listedIDs(second)...)
	body["page_size"] = 50
	delete(body, "page_token")
	whole := listedIDs(postTaskList(t, s, body))
	if !slices.Equal(walked, whole) {
		t.Errorf("paged walk = %v, want %v (pages must partition the total order)", walked, whole)
	}
}

// TestTaskList_EmptyResult pins the envelope for a filter that matches
// nothing: an empty items list (not null), no token, zero total.
func TestTaskList_EmptyResult(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPost, "/api/tasks/list", map[string]any{"statuses": []string{"in_review"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(raw["items"]) != "[]" {
		t.Errorf("items = %s, want [] (never null — clients iterate it unguarded)", raw["items"])
	}
	if _, present := raw["next_page_token"]; present {
		t.Errorf("next_page_token present on an empty result: %s", raw["next_page_token"])
	}
	if string(raw["total_count"]) != "0" {
		t.Errorf("total_count = %s, want 0", raw["total_count"])
	}
}

// TestTaskList_StrictBody is the negative space: every malformed filter is
// rejected with the field named, never resolved to a default. A corrupt filter
// that silently widens a result set is the failure this contract forbids.
func TestTaskList_StrictBody(t *testing.T) {
	cases := []struct {
		name       string
		body       map[string]any
		wantReason string
		wantField  string
	}{
		{
			name:       "unknown status",
			body:       map[string]any{"statuses": []string{"queued", "bogus"}},
			wantReason: "INVALID_FIELD",
			wantField:  "statuses",
		},
		{
			name:       "malformed team id",
			body:       map[string]any{"team_ids": []string{"junk"}},
			wantReason: "INVALID_FIELD",
			wantField:  "team_ids",
		},
		{
			name:       "malformed closed_since",
			body:       map[string]any{"closed_since": "last tuesday"},
			wantReason: "INVALID_FIELD",
			wantField:  "closed_since",
		},
		{
			name:       "page_size over the max",
			body:       map[string]any{"page_size": 500},
			wantReason: "OUT_OF_RANGE",
			wantField:  "page_size",
		},
		{
			name:       "negative page_size",
			body:       map[string]any{"page_size": -1},
			wantReason: "OUT_OF_RANGE",
			wantField:  "page_size",
		},
		{
			name:       "garbage page_token",
			body:       map[string]any{"page_token": "not-a-token"},
			wantReason: "INVALID_PARAM",
			wantField:  "page_token",
		},
		{
			name:       "unknown body field",
			body:       map[string]any{"bogus": 1},
			wantReason: "UNKNOWN_FIELD",
			wantField:  "bogus",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			rec := doJSON(t, s, http.MethodPost, "/api/tasks/list", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			assertFirstError(t, rec, tc.wantReason, tc.wantField)
		})
	}

	// Every failing field is reported, not just the first one hit.
	t.Run("accumulates every fault", func(t *testing.T) {
		s := newTestServer(t)
		rec := doJSON(t, s, http.MethodPost, "/api/tasks/list", map[string]any{
			"statuses":  []string{"bogus"},
			"team_ids":  []string{"junk"},
			"page_size": 500,
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		var body struct {
			Errors []struct {
				Field string `json:"field"`
			} `json:"errors"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		var fields []string
		for _, e := range body.Errors {
			fields = append(fields, e.Field)
		}
		slices.Sort(fields)
		if want := []string{"page_size", "statuses", "team_ids"}; !slices.Equal(fields, want) {
			t.Errorf("reported fields = %v, want %v", fields, want)
		}
	})
}

// TestTaskList_TokenIsBoundToItsFilters pins the token/filter binding: page 2
// of one query cannot be requested with page 1's token of another. Without it
// the offset would silently address a different result set and the caller
// would page through rows it never asked for.
func TestTaskList_TokenIsBoundToItsFilters(t *testing.T) {
	s := newTestServer(t)
	for i := range 3 {
		seedTaskFixture(t, s.db, taskFixture{name: fmt.Sprintf("bound-%d", i), status: "queued"})
	}

	body := queueProjectionBody()
	body["page_size"] = 2
	token := postTaskList(t, s, body).NextPageToken
	if token == "" {
		t.Fatal("no token minted for a partial page")
	}

	// Same route, different filters, that token: refused.
	other := map[string]any{
		"statuses":   []string{"queued"},
		"page_size":  2,
		"page_token": token,
	}
	rec := doJSON(t, s, http.MethodPost, "/api/tasks/list", other)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertFirstError(t, rec, "INVALID_PARAM", "page_token")

	// The same filters spelled differently — a reordered, duplicated status
	// list — are the same query, so the token still works.
	sameQuery := map[string]any{
		"statuses":        []string{"queued", "queued"},
		"only_unclaimed":  true,
		"include_snoozed": false,
		"page_size":       2,
		"page_token":      token,
	}
	if page := postTaskList(t, s, sameQuery); len(page.Items) != 1 {
		t.Errorf("second page under an equivalent filter spelling = %d items, want 1", len(page.Items))
	}
}
