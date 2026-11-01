package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
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
	BySource  []activitySourceRow `json:"by_source"`
	ByTracked []struct {
		Source      string     `json:"source"`
		UnitID      string     `json:"unit_id"`
		Events      int        `json:"events"`
		Tasks       int        `json:"tasks"`
		Runs        int        `json:"runs"`
		LastEventAt *time.Time `json:"last_event_at"`
	} `json:"by_tracked"`
}

// TestTeamActivity_NodeShape: the seeded flow lands in every cut, by_source
// covers every external source, last_poll_at appears only for sources that
// completed a cycle, and the tracked repository's row is keyed by registry id.
func TestTeamActivity_NodeShape(t *testing.T) {
	s := newTestServer(t)
	taskID := seedTaskFixture(t, s.db, taskFixture{name: "activity", status: "queued"})
	var repoID string
	if err := s.db.QueryRow(
		`SELECT e.repository_id FROM tasks t JOIN entities e ON e.id = t.entity_id WHERE t.id = ?`, taskID,
	).Scan(&repoID); err != nil {
		t.Fatalf("read fixture repository: %v", err)
	}
	execSQL(t, s.db, `INSERT INTO team_github_repos (team_id, repository_id, org_id) VALUES (?, ?, ?)`,
		runmode.LocalDefaultTeamID, repoID, runmode.LocalDefaultOrgID)
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

	if len(body.ByTracked) != 1 {
		t.Fatalf("by_tracked = %+v, want the one tracked repository", body.ByTracked)
	}
	unit := body.ByTracked[0]
	if unit.Source != "github" || unit.UnitID != repoID || unit.Events != 1 || unit.Tasks != 1 || unit.Runs != 0 {
		t.Errorf("by_tracked row = %+v, want github %s with events 1 / tasks 1 / runs 0", unit, repoID)
	}
	if unit.LastEventAt == nil {
		t.Error("by_tracked last_event_at is null for a repository that sent an event")
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

// TestTeamActivity_MergedAndFailedAreWindowedByTheAsk: the day cut's merged and
// failed counts are windowed by row, not by day: every fixture shares one UTC
// day, and the rows before the mid-day since bound are not counted.
func TestTeamActivity_MergedAndFailedAreWindowedByTheAsk(t *testing.T) {
	s := newTestServer(t)
	// A fixed past day, so the assertions don't depend on what time the
	// suite runs — the node's own grammar is UTC either way.
	const day = "2026-03-10"
	before, after := day+"T08:00:00Z", day+"T15:00:00Z"

	entityID := fixtureUUID("e_conv")
	repoID := dbseed.GitHubRepository(t, s.db, runmode.LocalDefaultOrgID, "https://github.com", "owner", "repo")
	execSQL(t, s.db, `INSERT INTO entities (id, source, source_id, kind, state, scope, repository_id) VALUES (?, 'github', 'owner/repo#77', 'pr', 'active', 'https://github.com', ?)`, entityID, repoID)
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
