package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

type activitySourceRow struct {
	Source     string     `json:"source"`
	Events     *int       `json:"events"`
	Tasks      int        `json:"tasks"`
	Runs       int        `json:"runs"`
	LastPollAt *time.Time `json:"last_poll_at"`
}

type activityBody struct {
	TeamID string `json:"team_id"`
	ByDay  []struct {
		Date   string `json:"date"`
		Events int    `json:"events"`
		Tasks  int    `json:"tasks"`
		Merged int    `json:"merged"`
		Failed int    `json:"failed"`
	} `json:"by_day"`
	ByDaySource []struct {
		Date   string `json:"date"`
		Source string `json:"source"`
		Events int    `json:"events"`
		Tasks  int    `json:"tasks"`
	} `json:"by_day_source"`
	BySource []activitySourceRow `json:"by_source"`
}

// TestTeamActivity_NodeShape walks the node end to end in local mode: the
// seeded flow lands in every cut, the by-source rows cover the external
// sources whatever the traffic, and last_poll_at is stitched in from
// poll_readiness for exactly the sources that have completed a cycle.
func TestTeamActivity_NodeShape(t *testing.T) {
	s := newTestServer(t)
	seedTaskFixture(t, s.db, taskFixture{name: "activity", status: "queued"})
	if err := s.allStores.PollReadiness.MarkPollComplete(
		context.Background(), runmode.LocalDefaultOrgID, "github", time.Time{}); err != nil {
		t.Fatalf("stamp poll completion: %v", err)
	}

	rec := doJSON(t, s, http.MethodGet, "/api/teams/default/activity?since=2020-01-01&until=2030-01-01", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body activityBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}

	dayEvents, dayTasks := 0, 0
	for _, d := range body.ByDay {
		dayEvents += d.Events
		dayTasks += d.Tasks
	}
	if dayEvents != 1 || dayTasks != 1 {
		t.Errorf("by_day sums = %d events / %d tasks, want 1 / 1", dayEvents, dayTasks)
	}
	dsEvents := 0
	for _, d := range body.ByDaySource {
		if d.Source != "github" {
			t.Errorf("by_day_source names %q, want only github", d.Source)
		}
		dsEvents += d.Events
	}
	if dsEvents != dayEvents {
		t.Errorf("by_day_source events = %d, disagree with by_day %d", dsEvents, dayEvents)
	}

	rows := map[string]activitySourceRow{}
	for _, r := range body.BySource {
		rows[r.Source] = r
	}
	gh, ok := rows["github"]
	if !ok || gh.Events == nil || *gh.Events != 1 || gh.Tasks != 1 || gh.Runs != 0 {
		t.Errorf("github row = %+v, want events 1 / tasks 1 / runs 0", gh)
	}
	if gh.LastPollAt == nil {
		t.Error("github last_poll_at is null after a completed cycle")
	}
	jira, ok := rows["jira"]
	if !ok || jira.LastPollAt != nil {
		t.Errorf("jira row = %+v, want present with null last_poll_at (never polled)", jira)
	}
	if _, ok := rows["slack"]; !ok {
		t.Error("by_source is missing the slack row")
	}
}

// TestTeamActivity_WindowIsValidated: the node speaks the usage node's
// window grammar, including its refusal of an inverted or empty window.
func TestTeamActivity_WindowIsValidated(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodGet, "/api/teams/default/activity?since=2026-01-02&until=2026-01-01", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("inverted window: status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, s, http.MethodGet, "/api/teams/default/activity?since=whenever", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed since: status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestTeamActivity_MergedAndFailedAreWindowedByTheAsk drives the day cut's two
// convergence counts onto the wire, and pins that an RFC3339 since windows
// ROWS rather than whole days: every fixture row shares one UTC day, and the
// half of them before the mid-day bound must not be counted. A store that
// bucketed first and clipped by day would answer 2 and 2.
func TestTeamActivity_MergedAndFailedAreWindowedByTheAsk(t *testing.T) {
	s := newTestServer(t)
	// A fixed past day, so the assertions don't depend on what time the
	// suite runs — the node's own grammar is UTC either way.
	const day = "2026-03-10"
	before, after := day+"T08:00:00Z", day+"T15:00:00Z"

	entityID := fixtureUUID("e_conv")
	execSQL(t, s.db, `INSERT INTO entities (id, source, source_id, kind, state) VALUES (?, 'github', 'owner/repo#77', 'pr', 'active')`, entityID)
	for i, at := range []string{before, after} {
		execSQL(t, s.db,
			`INSERT INTO events (id, entity_id, event_type, dedup_key, metadata_json, created_at) VALUES (?, ?, ?, '', '{}', ?)`,
			fixtureUUID(fmt.Sprintf("ev_merged_%d", i)), entityID, domain.EventGitHubPRMerged, at)
	}
	for i, at := range []string{before, after} {
		conversationID := seedSteerConversation(t, s.db, fmt.Sprintf("conv%d", i), domain.StatusFailed)
		execSQL(t, s.db, `UPDATE conversations SET completed_at = ? WHERE id = ?`, at, conversationID)
	}

	rec := doJSON(t, s, http.MethodGet,
		"/api/teams/default/activity?since="+day+"T12:00:00Z&until="+day+"T23:59:59Z", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body activityBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}

	if len(body.ByDay) != 1 || body.ByDay[0].Date != day {
		t.Fatalf("by_day = %+v, want the one day the fixtures share", body.ByDay)
	}
	got := body.ByDay[0]
	if got.Merged != 1 {
		t.Errorf("merged = %d, want 1 — the 08:00 merge is before the window opens", got.Merged)
	}
	if got.Failed != 1 {
		t.Errorf("failed = %d, want 1 — the 08:00 failure is before the window opens", got.Failed)
	}
	if got.Events != 1 {
		t.Errorf("events = %d, want 1 — a merge is an event, counted once in each", got.Events)
	}
}
