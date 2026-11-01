package server

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
)

// listedTask is the slice of the task payload these tests assert on: which rows
// came back, in what order.
type listedTask struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type taskListBody struct {
	Items         []listedTask `json:"items"`
	NextPageToken string       `json:"next_page_token"`
	TotalCount    int          `json:"total_count"`
}

// postTaskList calls POST /api/tasks/list and decodes the envelope, failing
// on a non-200, a null items list or a null total_count.
func postTaskList(t *testing.T, s *Server, body map[string]any) taskListBody {
	t.Helper()
	page := httpxtest.DecodeList[listedTask](t, doJSON(t, s, http.MethodPost, "/api/tasks/list", body))
	if page.TotalCount == nil {
		t.Fatalf("total_count is null")
	}
	return taskListBody{Items: page.Items, NextPageToken: page.NextPageToken, TotalCount: *page.TotalCount}
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
	// eventType is the station the row sits at. Empty defaults to
	// ci_check_failed — most fixtures are about a lane rather than a station,
	// and only the facet tests care which one.
	eventType string
	// priority is the row's priority_score. Nil takes the 0.5 every other
	// fixture carries — a set that ties on priority is what lets a test about
	// some other term read cleanly.
	priority *float64
	// inQueue is what the queue projection must answer for this row.
	inQueue bool
}

// seedTaskFixture inserts an entity, event and task for one fixture and returns
// the task id, writing the row directly because states such as a future snooze
// or a past closed_at have no store method that produces them.
func seedTaskFixture(t *testing.T, database *sql.DB, f taskFixture) string {
	t.Helper()
	now := time.Now().UTC()
	entityID := uuid.New().String()
	eventID := uuid.New().String()
	taskID := uuid.New().String()
	sourceID := fmt.Sprintf("octo/list#%s-%d", f.name, now.UnixNano())
	eventType := f.eventType
	if eventType == "" {
		eventType = domain.EventGitHubPRCICheckFailed
	}

	repoID := dbseed.GitHubEntityRepository(t, database, runmode.LocalDefaultOrgID, "https://github.com", sourceID)
	execSQL(t, database, `
		INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json, created_at, scope, repository_id)
		VALUES (?, 'github', ?, 'pr', ?, ?, '{}', ?, 'https://github.com', ?)`,
		entityID, sourceID, "List fixture "+f.name, "https://example/"+sourceID, now, repoID)
	execSQL(t, database, `
		INSERT INTO events (id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES (?, ?, ?, '', '{}', ?)`,
		eventID, entityID, eventType, now)

	priority := 0.5
	if f.priority != nil {
		priority = *f.priority
	}
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
		VALUES (?, ?, ?, '', ?, ?, ?, 'pending', ?, ?, 'team', ?, ?, ?, ?)`,
		taskID, entityID, eventType, eventID, f.status, priority, now,
		runmode.LocalDefaultTeamID, userClaim, botClaim, snooze, closed)
	return taskID
}

// queueProjectionBody is the body the triage deck and the board's Queued
// column send.
func queueProjectionBody() map[string]any {
	return map[string]any{
		"statuses":        []string{"queued"},
		"only_unclaimed":  true,
		"include_snoozed": false,
	}
}

// TestTaskList_QueueProjectionParity: the queue projection returns exactly the
// pickable rows across every state that could leak in (held, snoozed into the
// future or past, terminal), checked against the queue's definition in SQL too.
func TestTaskList_QueueProjectionParity(t *testing.T) {
	s := newTestServer(t)
	past := time.Now().UTC().Add(-2 * time.Hour)
	future := time.Now().UTC().Add(2 * time.Hour)

	fixtures := []taskFixture{
		{name: "pickable", status: "queued", inQueue: true},
		{name: "pickable-woke", status: "queued", snoozeUntil: &past, inQueue: true},
		// Held rows. Neither is queued: a claim lands its task in progress,
		// so the queue holds nobody's work and these are here to show the
		// projection has nothing to exclude.
		{name: "user-claimed", status: "in_progress", claimedUser: true},
		{name: "bot-claimed", status: "in_progress", claimedBot: true},
		// A queued row still inside a snooze window.
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

	// The oracle: the queue's definition as SQL, so a drift in the filter shows
	// up even if the hand-written expectation drifted with it. It is compared
	// as a set, since its ORDER BY has no id tiebreaker.
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
		t.Errorf("queue projection = %v, oracle query = %v", got, oracle)
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

// TestTaskList_ClosedSinceIsExplicit: the done lane has no hidden window, so an
// old closure lists until the caller passes closed_since, in either precision.
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

// TestTaskList_PageTokenSurvivesTheLaneMoving: the token is a keyset position,
// so a row leaving the lane above it does not shift page 2, which the board
// fetches on scroll with no way to re-ask for a skipped row.
func TestTaskList_PageTokenSurvivesTheLaneMoving(t *testing.T) {
	s := newTestServer(t)
	for i := range 7 {
		seedTaskFixture(t, s.db, taskFixture{name: fmt.Sprintf("moving-%d", i), status: "queued"})
	}

	body := queueProjectionBody()
	body["page_size"] = 50
	whole := listedIDs(postTaskList(t, s, body))
	if len(whole) != 7 {
		t.Fatalf("unpaged read returned %d rows, want 7", len(whole))
	}

	body["page_size"] = 3
	first := postTaskList(t, s, body)
	if !slices.Equal(listedIDs(first), whole[0:3]) {
		t.Fatalf("first page = %v, want %v", listedIDs(first), whole[0:3])
	}

	// A run finishes and its card leaves the lane, from above the point page 1
	// stopped at. An offset page 2 would now start at the fourth row of what
	// is left — skipping one, permanently, since the board fetches on scroll
	// and has no way to re-ask.
	//
	// The mirror case, a row arriving above the cut, is pinned in the store
	// conformance rather than here: these rows tie on every term of the
	// default order but their ids, so where a new one lands is its uuid's
	// business, and naming a position takes a sort that orders on something
	// the test controls.
	execSQL(t, s.db, `UPDATE tasks SET status = 'done', closed_at = ? WHERE id = ?`, time.Now().UTC(), whole[1])

	body["page_token"] = first.NextPageToken
	second := postTaskList(t, s, body)
	if got := listedIDs(second); !slices.Equal(got, whole[3:6]) {
		t.Errorf("second page = %v, want %v (the token names a position, not a count of rows above it)", got, whole[3:6])
	}
	if second.TotalCount != 6 {
		t.Errorf("total_count = %d, want 6 — the total is the filtered set as it stands, not a number the page derives its position from", second.TotalCount)
	}
}

// TestTaskList_OffsetTokenIsRefused: an offset-form token, which the stores
// would still honor for other routes, is a 400 here, so nothing can make this
// route page by offset; its own keyset tokens still walk it.
func TestTaskList_OffsetTokenIsRefused(t *testing.T) {
	s := newTestServer(t)
	for i := range 5 {
		seedTaskFixture(t, s.db, taskFixture{name: fmt.Sprintf("offtok-%d", i), status: "queued"})
	}

	// The same opaque envelope, fingerprinted for this very query, carrying an
	// offset.
	fingerprint := httpx.FilterFingerprint(taskListFilterKey{
		Statuses:      []string{"queued"},
		OnlyUnclaimed: true,
	})
	raw, err := json.Marshal(map[string]any{"o": 2, "f": fingerprint})
	if err != nil {
		t.Fatalf("marshal legacy token: %v", err)
	}
	body := queueProjectionBody()
	body["page_size"] = 2
	body["page_token"] = base64.RawURLEncoding.EncodeToString(raw)

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/list", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	httpxtest.AssertFirstError(t, rec, "INVALID_PARAM", "page_token")

	// And the route's own tokens still walk it, so the refusal is about the
	// form and not about tokens.
	delete(body, "page_token")
	first := postTaskList(t, s, body)
	if first.NextPageToken == "" {
		t.Fatal("no next_page_token minted for a partial page")
	}
	body["page_token"] = first.NextPageToken
	if got := len(postTaskList(t, s, body).Items); got != 2 {
		t.Errorf("second page = %d items, want 2", got)
	}
}

// TestTaskList_UnusablePageTokenIsA400: a keyset whose terms do not fit its
// order reaches the store, the only place that knows the tuple, and comes back
// as a bad page_token rather than a server fault.
func TestTaskList_UnusablePageTokenIsA400(t *testing.T) {
	s := newTestServer(t)
	seedTaskFixture(t, s.db, taskFixture{name: "badtok", status: "queued"})

	fingerprint := httpx.FilterFingerprint(taskListFilterKey{
		Statuses:      []string{"queued"},
		OnlyUnclaimed: true,
	})
	raw, err := json.Marshal(map[string]any{"k": []string{"one-term-is-not-the-tuple"}, "f": fingerprint})
	if err != nil {
		t.Fatalf("marshal token: %v", err)
	}
	body := queueProjectionBody()
	body["page_token"] = base64.RawURLEncoding.EncodeToString(raw)

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/list", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	httpxtest.AssertFirstError(t, rec, "INVALID_PARAM", "page_token")
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
			// 'in_review' is outside the vocabulary, so asking for it names
			// the field rather than answering an empty page.
			name:       "retired status",
			body:       map[string]any{"statuses": []string{"in_review"}},
			wantReason: "INVALID_FIELD",
			wantField:  "statuses",
		},
		{
			// 'claimed' is a claim, not a lane: the vocabulary is the five
			// lifecycle statuses.
			name:       "retired claim projection",
			body:       map[string]any{"statuses": []string{"claimed"}},
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
			name:       "malformed created_since",
			body:       map[string]any{"created_since": "last tuesday"},
			wantReason: "INVALID_FIELD",
			wantField:  "created_since",
		},
		{
			name:       "unknown source",
			body:       map[string]any{"sources": []string{"github", "bogus"}},
			wantReason: "INVALID_FIELD",
			wantField:  "sources",
		},
		{
			name:       "malformed created_before",
			body:       map[string]any{"created_before": "next tuesday"},
			wantReason: "INVALID_FIELD",
			wantField:  "created_before",
		},
		{
			name:       "unknown event type",
			body:       map[string]any{"event_types": []string{domain.EventGitHubPRCICheckFailed, "github:pr:invented"}},
			wantReason: "INVALID_FIELD",
			wantField:  "event_types",
		},
		{
			name:       "search past the rune cap",
			body:       map[string]any{"search": strings.Repeat("x", 201)},
			wantReason: "INVALID_FIELD",
			wantField:  "search",
		},
		{
			name:       "unknown sort_key",
			body:       map[string]any{"sort_key": "priority"},
			wantReason: "INVALID_FIELD",
			wantField:  "sort_key",
		},
		{
			// "default" is the client's word for the absent case, not a
			// value the wire accepts — absent is how you ask for it.
			name:       "sort_key spelled default",
			body:       map[string]any{"sort_key": "default"},
			wantReason: "INVALID_FIELD",
			wantField:  "sort_key",
		},
		{
			name:       "unknown sort_dir",
			body:       map[string]any{"sort_key": "title", "sort_dir": "sideways"},
			wantReason: "INVALID_FIELD",
			wantField:  "sort_dir",
		},
		{
			// The default order has no direction, so a lone sort_dir asks
			// for something the answer can't show. Refused, not ignored.
			name:       "sort_dir without sort_key",
			body:       map[string]any{"sort_dir": "asc"},
			wantReason: "INVALID_FIELD",
			wantField:  "sort_dir",
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
			httpxtest.AssertFirstError(t, rec, tc.wantReason, tc.wantField)
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

// TestTaskList_CreatedSinceAndSources: sources and created_since narrow the
// list, and with page_size 0 answer "how many arrived from this source in this
// window" with no rows and no token.
func TestTaskList_CreatedSinceAndSources(t *testing.T) {
	s := newTestServer(t)
	a := seedTaskFixture(t, s.db, taskFixture{name: "flow-a", status: "queued"})
	b := seedTaskFixture(t, s.db, taskFixture{name: "flow-b", status: "queued"})

	// The fixtures are github entities, so the github narrow keeps both and
	// the jira narrow keeps neither — an empty page, not an error.
	page := postTaskList(t, s, map[string]any{"sources": []string{"github"}})
	if got := listedIDs(page); !slices.Contains(got, a) || !slices.Contains(got, b) {
		t.Errorf("sources=[github] returned %v, want both %s and %s", got, a, b)
	}
	page = postTaskList(t, s, map[string]any{"sources": []string{"jira"}})
	if len(page.Items) != 0 || page.TotalCount != 0 {
		t.Errorf("sources=[jira] = %+v, want an empty page", page)
	}

	// created_since applies to every status, bounded by created_at.
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	page = postTaskList(t, s, map[string]any{"created_since": past})
	if page.TotalCount < 2 {
		t.Errorf("created_since in the past counted %d, want >= 2", page.TotalCount)
	}
	page = postTaskList(t, s, map[string]any{"created_since": future})
	if page.TotalCount != 0 {
		t.Errorf("created_since in the future counted %d, want 0", page.TotalCount)
	}

	// The figure request: filters + page_size 0. No rows come back, the
	// total is the filtered count, and no token is minted.
	page = postTaskList(t, s, map[string]any{"sources": []string{"github"}, "page_size": 0})
	if len(page.Items) != 0 || page.TotalCount < 2 || page.NextPageToken != "" {
		t.Errorf("count-only page = %+v, want no items, total >= 2, no token", page)
	}
}

// TestTaskList_SearchNarrowsTheCountItReports: search narrows the rows and the
// total_count alike, so the lane's `N of M` answers the query; the needle is
// trimmed, and a blank one is no filter.
func TestTaskList_SearchNarrowsTheCountItReports(t *testing.T) {
	s := newTestServer(t)
	hit := seedTaskFixture(t, s.db, taskFixture{name: "searchable-widget", status: "queued"})
	seedTaskFixture(t, s.db, taskFixture{name: "other-thing", status: "queued"})

	unfiltered := postTaskList(t, s, map[string]any{"page_size": 0})
	if unfiltered.TotalCount != 2 {
		t.Fatalf("unfiltered total = %d, want 2", unfiltered.TotalCount)
	}

	page := postTaskList(t, s, map[string]any{"search": "searchable-widget"})
	if got := listedIDs(page); len(got) != 1 || got[0] != hit {
		t.Errorf("search returned %v, want just %s", got, hit)
	}
	if page.TotalCount != 1 {
		t.Errorf("total under search = %d, want 1 — the count must run the filtered query", page.TotalCount)
	}

	// Surrounding whitespace is not part of the needle: the same query
	// spelled loosely answers the same.
	if padded := postTaskList(t, s, map[string]any{"search": "  searchable-widget  "}); padded.TotalCount != 1 {
		t.Errorf("padded needle counted %d, want 1", padded.TotalCount)
	}
	// An empty-after-trim search is absent, not a match-nothing filter.
	if blank := postTaskList(t, s, map[string]any{"search": "   ", "page_size": 0}); blank.TotalCount != 2 {
		t.Errorf("whitespace-only search counted %d, want the unfiltered 2", blank.TotalCount)
	}
	if none := postTaskList(t, s, map[string]any{"search": "nothing matches this"}); len(none.Items) != 0 || none.TotalCount != 0 {
		t.Errorf("unmatched needle = %+v, want an empty page", none)
	}
}

// TestTaskList_EventTypesAndCreatedBefore: event_types narrows to the station
// set, and created_before bounds the created window alone or with created_since.
func TestTaskList_EventTypesAndCreatedBefore(t *testing.T) {
	s := newTestServer(t)
	a := seedTaskFixture(t, s.db, taskFixture{name: "et-a", status: "queued"})
	b := seedTaskFixture(t, s.db, taskFixture{name: "et-b", status: "queued"})

	// Every fixture sits at ci_check_failed, so that station is the whole
	// set and any other station is none of it.
	page := postTaskList(t, s, map[string]any{"event_types": []string{domain.EventGitHubPRCICheckFailed}})
	if got := listedIDs(page); !slices.Contains(got, a) || !slices.Contains(got, b) {
		t.Errorf("event_types on the seeded station returned %v, want both %s and %s", got, a, b)
	}
	page = postTaskList(t, s, map[string]any{"event_types": []string{domain.EventGitHubPRReviewRequested}})
	if len(page.Items) != 0 || page.TotalCount != 0 || page.NextPageToken != "" {
		t.Errorf("event_types on an unused station = %+v, want an empty page with no token", page)
	}

	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	if page = postTaskList(t, s, map[string]any{"created_before": future, "page_size": 0}); page.TotalCount != 2 {
		t.Errorf("created_before in the future counted %d, want 2", page.TotalCount)
	}
	if page = postTaskList(t, s, map[string]any{"created_before": past, "page_size": 0}); page.TotalCount != 0 {
		t.Errorf("created_before in the past counted %d, want 0", page.TotalCount)
	}
	// Both ends together are a window.
	if page = postTaskList(t, s, map[string]any{"created_since": past, "created_before": future, "page_size": 0}); page.TotalCount != 2 {
		t.Errorf("bracketing window counted %d, want 2", page.TotalCount)
	}
}

// TestTaskList_SortReordersTheLane: a sort key reorders a lane's rows (desc by
// default) but never the lane structure, so a snoozed row stays last.
func TestTaskList_SortReordersTheLane(t *testing.T) {
	s := newTestServer(t)
	// seedTaskFixture builds the entity title out of the name, so these are
	// the sort keys.
	for _, name := range []string{"ccc", "aaa", "bbb"} {
		seedTaskFixture(t, s.db, taskFixture{name: "sort-" + name, status: "queued"})
	}

	read := func(dir string) []string {
		t.Helper()
		body := map[string]any{"statuses": []string{"queued"}, "sort_key": "title"}
		if dir != "" {
			body["sort_dir"] = dir
		}
		return listedIDs(postTaskList(t, s, body))
	}

	asc := read("asc")
	if len(asc) != 3 {
		t.Fatalf("title asc returned %d rows, want 3", len(asc))
	}
	desc := read("desc")
	for i, id := range asc {
		if got := desc[len(desc)-1-i]; got != id {
			t.Errorf("title desc[%d] = %s, want %s (desc must be asc reversed)", len(desc)-1-i, got, id)
		}
	}
	// An absent direction alongside a key is desc — the board's own default.
	if implied := read(""); !slices.Equal(implied, desc) {
		t.Errorf("sort_key with no sort_dir = %v, want the desc order %v", implied, desc)
	}

	// A snoozed row is lane structure: it stays behind the live ones however
	// its title sorts. "aaa" would lead an ascending title sort otherwise.
	snoozed := time.Now().UTC().Add(time.Hour)
	sleeper := seedTaskFixture(t, s.db, taskFixture{
		name: "sort-aaa-asleep", status: "snoozed", snoozeUntil: &snoozed,
	})
	withSleeper := listedIDs(postTaskList(t, s, map[string]any{
		"statuses":        []string{"queued", "snoozed"},
		"include_snoozed": true,
		"sort_key":        "title",
		"sort_dir":        "asc",
	}))
	if len(withSleeper) != 4 {
		t.Fatalf("lane with the snoozed row returned %d rows, want 4", len(withSleeper))
	}
	if got := withSleeper[len(withSleeper)-1]; got != sleeper {
		t.Errorf("snoozed row sorted to %v, want last — the lane partition outranks the key", withSleeper)
	}
}

// TestTaskList_TokenIsBoundToItsSearchAndSort: a token names a position in one
// query, so it is refused under other filters, another search or another sort,
// and accepted under another spelling of the same query.
func TestTaskList_TokenIsBoundToItsSearchAndSort(t *testing.T) {
	s := newTestServer(t)
	for i := range 3 {
		seedTaskFixture(t, s.db, taskFixture{name: fmt.Sprintf("bound-search-%d", i), status: "queued"})
	}

	mint := func(body map[string]any) string {
		t.Helper()
		body["page_size"] = 1
		token := postTaskList(t, s, body).NextPageToken
		if token == "" {
			t.Fatal("no token minted for a partial page")
		}
		return token
	}
	refused := func(body map[string]any, token string) {
		t.Helper()
		body["page_size"] = 1
		body["page_token"] = token
		rec := doJSON(t, s, http.MethodPost, "/api/tasks/list", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		httpxtest.AssertFirstError(t, rec, "INVALID_PARAM", "page_token")
	}

	searchToken := mint(map[string]any{"search": "bound-search"})
	refused(map[string]any{"search": "bound-search-1"}, searchToken)
	refused(map[string]any{}, searchToken)

	// Case is not part of the query — the needle is folded before it reaches
	// the predicate — so the same search spelled loudly keeps the token.
	if page := postTaskList(t, s, map[string]any{
		"search": "BOUND-SEARCH", "page_size": 1, "page_token": searchToken,
	}); len(page.Items) != 1 {
		t.Errorf("second page under a differently-cased needle = %d items, want 1", len(page.Items))
	}

	// A duplicated status list is the same query; a different filter set is not.
	statusToken := mint(map[string]any{"statuses": []string{"queued"}, "only_unclaimed": true})
	refused(map[string]any{"statuses": []string{"queued"}}, statusToken)
	if page := postTaskList(t, s, map[string]any{
		"statuses": []string{"queued", "queued"}, "only_unclaimed": true, "page_size": 1, "page_token": statusToken,
	}); len(page.Items) != 1 {
		t.Errorf("second page under an equivalent filter spelling = %d items, want 1", len(page.Items))
	}

	sortToken := mint(map[string]any{"sort_key": "title", "sort_dir": "asc"})
	refused(map[string]any{"sort_key": "title", "sort_dir": "desc"}, sortToken)
	refused(map[string]any{"sort_key": "created", "sort_dir": "asc"}, sortToken)

	// The implied direction is the same query as the explicit one, so its
	// token still works.
	impliedToken := mint(map[string]any{"sort_key": "title"})
	page := postTaskList(t, s, map[string]any{
		"sort_key": "title", "sort_dir": "desc", "page_size": 1, "page_token": impliedToken,
	})
	if len(page.Items) != 1 {
		t.Errorf("second page under the explicit spelling of the same sort = %d items, want 1", len(page.Items))
	}
}

// seedListConversation hangs one conversation off a seeded task in the given
// stored status ("" = SQL NULL, the mid-flight state) and returns its id.
// origin='interactive' is how a conversation with no blueprint behind it is
// spelled — the origin CHECK demands the blueprint parents only for 'blueprint'.
func seedListConversation(t *testing.T, database *sql.DB, taskID, storedStatus string) string {
	t.Helper()
	convID := uuid.New().String()
	resolved, concluded := dbtest.SeedStatus(storedStatus)
	var status any
	if resolved != "" {
		status = resolved
	}
	var completedAt any
	if concluded {
		completedAt = time.Now().UTC()
	}
	execSQL(t, database, `
		INSERT INTO conversations (id, task_id, status, trigger_type, origin,
		                          team_id, visibility, creator_user_id, completed_at)
		VALUES (?, ?, ?, 'manual', 'interactive', ?, 'team', ?, ?)`,
		convID, taskID, status, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID, completedAt)
	return convID
}

// seedDraftPR leaves an unresolved deliverable on the conversation — the shape
// that makes a concluded run somebody's move.
func seedDraftPR(t *testing.T, database *sql.DB, conversationID string) {
	t.Helper()
	id := uuid.New().String()
	execSQL(t, database, `
		INSERT INTO deliverables (id, conversation_id, team_id, provider, kind, target, state, dedup_key)
		VALUES (?, ?, ?, 'github', 'pull_request', 'o/r#7', 'draft', ?)`,
		id, conversationID, runmode.LocalDefaultTeamID, id)
}

// TestTaskList_AttentionOrderSurvivesPaging: the needs-you tier orders the lane
// on the server, so both needs-you rows, the lowest priority in the lane, fill
// page 1, and the last page carries the total and no token.
func TestTaskList_AttentionOrderSurvivesPaging(t *testing.T) {
	s := newTestServer(t)
	priority := func(v float64) *float64 { return &v }
	seed := func(name string, p float64, needsYou bool) string {
		t.Helper()
		id := seedTaskFixture(t, s.db, taskFixture{
			name: name, status: "in_progress", claimedUser: true, priority: priority(p),
		})
		conv := seedListConversation(t, s.db, id, dbtest.SeedConcluded)
		if needsYou {
			seedDraftPR(t, s.db, conv)
		}
		return id
	}
	// Descending priority within each tier, so both pages are a fixed order.
	needsHigher := seed("paged-needs-hi", 0.2, true)
	needsLower := seed("paged-needs-lo", 0.1, true)
	quietHigher := seed("paged-quiet-hi", 0.9, false)
	quietLower := seed("paged-quiet-lo", 0.8, false)

	body := map[string]any{"statuses": []string{"in_progress"}, "page_size": 2}
	first := postTaskList(t, s, body)
	if want := []string{needsHigher, needsLower}; !slices.Equal(listedIDs(first), want) {
		t.Errorf("page 1 = %v, want %v — the needs-you rows, despite being the lowest priority in the lane",
			listedIDs(first), want)
	}
	if first.TotalCount != 4 {
		t.Errorf("total_count = %d, want 4 — the tier orders the lane, it doesn't narrow it", first.TotalCount)
	}
	if first.NextPageToken == "" {
		t.Fatal("no token minted for a partial page")
	}

	body["page_token"] = first.NextPageToken
	second := postTaskList(t, s, body)
	if want := []string{quietHigher, quietLower}; !slices.Equal(listedIDs(second), want) {
		t.Errorf("page 2 = %v, want %v — the concluded rows, in priority order behind the tier",
			listedIDs(second), want)
	}
	if second.TotalCount != 4 || second.NextPageToken != "" {
		t.Errorf("last page total_count = %d, token %q; want 4 and no token", second.TotalCount, second.NextPageToken)
	}
}
