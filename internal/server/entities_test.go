package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

type entityListPage struct {
	Items         []entityJSON `json:"items"`
	NextPageToken string       `json:"next_page_token"`
	TotalCount    int          `json:"total_count"`
}

func listEntitiesOK(t *testing.T, s *Server, body map[string]any) entityListPage {
	t.Helper()
	rec := doJSON(t, s, http.MethodPost, "/api/entities/list", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/entities/list %v = %d, want 200; body=%s", body, rec.Code, rec.Body.String())
	}
	var page entityListPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode entity list: %v", err)
	}
	return page
}

func recordTestEvent(t *testing.T, s *Server, entityID, eventType string) string {
	t.Helper()
	id, err := s.events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{EntityID: &entityID, EventType: eventType})
	if err != nil {
		t.Fatalf("record %s: %v", eventType, err)
	}
	return id
}

// A pull request is served with its repository registry id, its latest
// event, its fields from the snapshot, and whether the viewer wrote it; the
// single read answers the same object.
func TestEntityList_ServesTheEntityResource(t *testing.T) {
	s := newTestServer(t)
	host := dashboardTestHost(t, s, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID)
	if err := s.users.UpsertGitHubIdentity(context.Background(), runmode.LocalDefaultUserID, host, "octocat", "", "", "connect_oauth"); err != nil {
		t.Fatalf("bind identity: %v", err)
	}
	mine := seedPRSnapshot(t, s, "acme", "api", 7, domain.PRSnapshot{
		Number: 7, Repo: "acme/api", Author: "octocat", Title: "Mine", Additions: 3, Deletions: 1, IsDraft: true,
	})
	theirs := seedPRSnapshot(t, s, "acme", "api", 8, domain.PRSnapshot{Number: 8, Repo: "acme/api", Author: "someone", Title: "Theirs"})
	recordTestEvent(t, s, mine.ID, domain.EventGitHubPROpened)
	recordTestEvent(t, s, mine.ID, domain.EventGitHubPRCICheckPassed)

	page := listEntitiesOK(t, s, map[string]any{})
	got := map[string]entityJSON{}
	for _, it := range page.Items {
		got[it.ID] = it
	}
	e := got[mine.ID]
	if e.RepositoryID == nil || *e.RepositoryID != mine.RepositoryID {
		t.Errorf("repository_id = %v, want %s", e.RepositoryID, mine.RepositoryID)
	}
	if e.LatestEventType == nil || *e.LatestEventType != domain.EventGitHubPRCICheckPassed || e.LatestEventAt == nil {
		t.Errorf("latest event = (%v, %v), want ci_check_passed with a time", e.LatestEventType, e.LatestEventAt)
	}
	if e.PullRequest == nil || e.PullRequest.Number != 7 || e.PullRequest.Author != "octocat" ||
		e.PullRequest.Additions != 3 || e.PullRequest.Deletions != 1 || !e.PullRequest.Draft {
		t.Errorf("pull_request = %+v, want the snapshot's fields", e.PullRequest)
	}
	if !e.Mine {
		t.Error("mine = false for a pull request the viewer authored")
	}
	if got[theirs.ID].Mine {
		t.Error("mine = true for someone else's pull request")
	}
	if got[theirs.ID].LatestEventType != nil {
		t.Errorf("an entity with no event carries latest_event_type %v", *got[theirs.ID].LatestEventType)
	}

	rec := doJSON(t, s, http.MethodGet, "/api/entities/"+mine.ID, nil)
	var one entityJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &one); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("GET /api/entities/{id} = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if one.ID != mine.ID || !one.Mine || one.PullRequest == nil || one.PullRequest.Number != 7 {
		t.Errorf("single read = %+v, want the listed object", one)
	}
}

func TestEntityGet_UnknownOrMalformedIsNotFound(t *testing.T) {
	s := newTestServer(t)
	for _, id := range []string{"00000000-0000-4000-8000-0000000000ee", "not-a-uuid", "00000000-0000-4000-8000-0000000000EE"} {
		if rec := doJSON(t, s, http.MethodGet, "/api/entities/"+id, nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET /api/entities/%s = %d, want 404", id, rec.Code)
		}
	}
}

// The filters narrow, closed_since keeps the open beside the recently closed,
// and the page answers the filtered total with a token for the rest.
func TestEntityList_FiltersAndPages(t *testing.T) {
	s := newTestServer(t)
	store := sqlitestore.New(s.db).Entities
	ctx := context.Background()
	pr := seedPRSnapshot(t, s, "acme", "api", 1, domain.PRSnapshot{Number: 1, Repo: "acme/api"})
	closed := seedPRSnapshot(t, s, "acme", "api", 2, domain.PRSnapshot{Number: 2, Repo: "acme/api"})
	if _, err := store.MarkClosed(ctx, runmode.LocalDefaultOrgID, closed.ID); err != nil {
		t.Fatalf("close: %v", err)
	}
	issue, _, err := store.FindOrCreate(ctx, runmode.LocalDefaultOrgID, "github", "https://github.com", "acme/api#3", "", domain.EntityKindIssue, "issue", "")
	if err != nil {
		t.Fatalf("seed issue: %v", err)
	}

	ids := func(page entityListPage) map[string]bool {
		out := map[string]bool{}
		for _, it := range page.Items {
			out[it.ID] = true
		}
		return out
	}
	if got := ids(listEntitiesOK(t, s, map[string]any{"kinds": []string{"pr"}})); !got[pr.ID] || !got[closed.ID] || got[issue.ID] {
		t.Errorf("kinds [pr] = %v, want both pull requests and not the issue", got)
	}
	if got := ids(listEntitiesOK(t, s, map[string]any{"states": []string{"active"}})); got[closed.ID] || !got[pr.ID] {
		t.Errorf("states [active] = %v, want the open pull request and not the closed one", got)
	}
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	if got := ids(listEntitiesOK(t, s, map[string]any{"closed_since": future})); got[closed.ID] || !got[pr.ID] || !got[issue.ID] {
		t.Errorf("closed_since in the future = %v, want every open entity and not the closed one", got)
	}
	if got := ids(listEntitiesOK(t, s, map[string]any{"ids": []string{issue.ID}})); len(got) != 1 || !got[issue.ID] {
		t.Errorf("ids [issue] = %v, want the issue alone", got)
	}

	first := listEntitiesOK(t, s, map[string]any{"page_size": 2})
	if len(first.Items) != 2 || first.TotalCount != 3 || first.NextPageToken == "" {
		t.Fatalf("first page = %d items of %d, token %q; want 2 of 3 and a token", len(first.Items), first.TotalCount, first.NextPageToken)
	}
	second := listEntitiesOK(t, s, map[string]any{"page_size": 2, "page_token": first.NextPageToken})
	if len(second.Items) != 1 || second.NextPageToken != "" {
		t.Errorf("second page = %d items, token %q; want the last one and no token", len(second.Items), second.NextPageToken)
	}
	count := listEntitiesOK(t, s, map[string]any{"page_size": 0})
	if len(count.Items) != 0 || count.TotalCount != 3 {
		t.Errorf("count-only = %d items of %d, want 0 of 3", len(count.Items), count.TotalCount)
	}
}

func TestEntityAndEventLists_RefuseBadBodies(t *testing.T) {
	s := newTestServer(t)
	for _, c := range []struct {
		name, path string
		body       map[string]any
		field      string
	}{
		{"unknown field", "/api/entities/list", map[string]any{"limit": 5}, ""},
		{"unknown source", "/api/entities/list", map[string]any{"sources": []string{"gitlab"}}, "sources"},
		{"system source", "/api/entities/list", map[string]any{"sources": []string{"system"}}, "sources"},
		{"unknown kind", "/api/entities/list", map[string]any{"kinds": []string{"epic"}}, "kinds"},
		{"unknown state", "/api/entities/list", map[string]any{"states": []string{"merged"}}, "states"},
		{"malformed closed_since", "/api/entities/list", map[string]any{"closed_since": "yesterday"}, "closed_since"},
		{"malformed team", "/api/entities/list", map[string]any{"team_ids": []string{"junk"}}, "team_ids"},
		{"urn team", "/api/entities/list", map[string]any{"team_ids": []string{"urn:uuid:6ba7b810-9dad-11d1-80b4-00c04fd430c8"}}, "team_ids"},
		{"malformed id", "/api/entities/list", map[string]any{"ids": []string{"junk"}}, "ids[0]"},
		{"page too large", "/api/entities/list", map[string]any{"page_size": 201}, "page_size"},
		{"unknown event field", "/api/events/list", map[string]any{"event_types": []string{"x"}}, ""},
		{"malformed entity id", "/api/events/list", map[string]any{"entity_ids": []string{"junk"}}, "entity_ids[0]"},
		{"malformed since", "/api/events/list", map[string]any{"since": "2026-13-01"}, "since"},
		{"foreign page token", "/api/events/list", map[string]any{"page_token": "garbage"}, "page_token"},
		{"zero per_entity", "/api/events/list", map[string]any{"per_entity": 0}, "per_entity"},
		{"per_entity too large", "/api/events/list", map[string]any{"per_entity": 201}, "per_entity"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := doJSON(t, s, http.MethodPost, c.path, c.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if c.field == "" {
				return
			}
			var body struct {
				Errors []struct {
					Field string `json:"field"`
				} `json:"errors"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if len(body.Errors) == 0 || body.Errors[0].Field != c.field {
				t.Errorf("errors = %s, want one naming %s", rec.Body.String(), c.field)
			}
		})
	}
}

// The event list pages newest recorded first and resumes by keyset, so an
// event recorded between two pages neither repeats nor pushes one out.
func TestEventList_PagesByKeyset(t *testing.T) {
	s := newTestServer(t)
	pr := seedPRSnapshot(t, s, "acme", "api", 1, domain.PRSnapshot{Number: 1, Repo: "acme/api"})
	other := seedPRSnapshot(t, s, "acme", "api", 2, domain.PRSnapshot{Number: 2, Repo: "acme/api"})
	opened := recordTestEvent(t, s, pr.ID, domain.EventGitHubPROpened)
	commits := recordTestEvent(t, s, pr.ID, domain.EventGitHubPRNewCommits)
	passed := recordTestEvent(t, s, pr.ID, domain.EventGitHubPRCICheckPassed)
	recordTestEvent(t, s, other.ID, domain.EventGitHubPROpened)

	type page struct {
		Items []struct {
			ID         string     `json:"id"`
			EntityID   string     `json:"entity_id"`
			EventType  string     `json:"event_type"`
			OccurredAt *time.Time `json:"occurred_at"`
		} `json:"items"`
		NextPageToken string `json:"next_page_token"`
		TotalCount    int    `json:"total_count"`
	}
	read := func(body map[string]any) page {
		t.Helper()
		rec := doJSON(t, s, http.MethodPost, "/api/events/list", body)
		var p page
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("POST /api/events/list = %d %s, want 200", rec.Code, rec.Body.String())
		}
		return p
	}

	first := read(map[string]any{"entity_ids": []string{pr.ID}, "page_size": 2})
	if first.TotalCount != 3 || len(first.Items) != 2 || first.Items[0].ID != passed || first.Items[1].ID != commits || first.NextPageToken == "" {
		t.Fatalf("first page = %+v, want [passed commits] of 3 with a token", first)
	}
	if first.Items[0].EntityID != pr.ID || first.Items[0].OccurredAt != nil {
		t.Errorf("event = %+v, want the pull request's, with no source time", first.Items[0])
	}
	recordTestEvent(t, s, pr.ID, domain.EventGitHubPRReviewApproved)
	second := read(map[string]any{"entity_ids": []string{pr.ID}, "page_size": 2, "page_token": first.NextPageToken})
	if len(second.Items) != 1 || second.Items[0].ID != opened || second.NextPageToken != "" {
		t.Errorf("second page = %+v, want [opened] alone and no token", second)
	}

	since := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	if p := read(map[string]any{"since": since}); p.TotalCount != 0 {
		t.Errorf("since an hour from now = %d events, want none", p.TotalCount)
	}

	capped := read(map[string]any{"entity_ids": []string{pr.ID, other.ID}, "per_entity": 1})
	if capped.TotalCount != 2 || len(capped.Items) != 2 {
		t.Errorf("per_entity 1 over two entities = %+v, want one event of each", capped)
	}
}

// An event is read by its id; one the viewer cannot read, or a malformed id,
// is 404.
func TestEventGet_ServesTheEventResource(t *testing.T) {
	s := newTestServer(t)
	pr := seedPRSnapshot(t, s, "acme", "api", 1, domain.PRSnapshot{Number: 1, Repo: "acme/api"})
	id := recordTestEvent(t, s, pr.ID, domain.EventGitHubPROpened)

	rec := doJSON(t, s, http.MethodGet, "/api/events/"+id, nil)
	var got struct {
		ID        string `json:"id"`
		EntityID  string `json:"entity_id"`
		EventType string `json:"event_type"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("GET /api/events/%s = %d %s, want 200", id, rec.Code, rec.Body.String())
	}
	if got.ID != id || got.EntityID != pr.ID || got.EventType != domain.EventGitHubPROpened {
		t.Errorf("event = %+v, want %s on %s", got, id, pr.ID)
	}
	for _, path := range []string{"/api/events/" + fixtureUUID("e_missing"), "/api/events/junk"} {
		if rec := doJSON(t, s, http.MethodGet, path, nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
}

// The task and conversation lists narrow to the work on the entities named.
func TestTaskAndConversationLists_NarrowByEntity(t *testing.T) {
	s := newTestServer(t)
	ours := seedSteerConversation(t, s.db, "ent-ours", "running")
	theirs := seedSteerConversation(t, s.db, "ent-theirs", "running")
	ourEntity := fixtureUUID("e_ent-ours")
	ourTask := fixtureUUID("t_ent-ours")

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/list", map[string]any{"entity_ids": []string{ourEntity}})
	var tasks struct {
		Items      []taskJSON `json:"items"`
		TotalCount int        `json:"total_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &tasks); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("POST /api/tasks/list = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if tasks.TotalCount != 1 || len(tasks.Items) != 1 || tasks.Items[0].ID != ourTask {
		t.Errorf("tasks on %s = %+v, want its one task", ourEntity, tasks.Items)
	}

	rec = doJSON(t, s, http.MethodPost, "/api/agent/conversations/list", map[string]any{"entity_ids": []string{ourEntity}})
	var convs struct {
		Runs       map[string][]map[string]any `json:"runs"`
		TotalCount int                         `json:"total_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &convs); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("POST /api/agent/conversations/list = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if convs.TotalCount != 1 || len(convs.Runs[ourTask]) != 1 || convs.Runs[ourTask][0]["id"] != ours {
		t.Errorf("conversations on %s = %+v, want %s alone", ourEntity, convs.Runs, ours)
	}
	for _, rows := range convs.Runs {
		for _, row := range rows {
			if row["id"] == theirs {
				t.Errorf("the conversation on another entity was listed")
			}
		}
	}

	for _, path := range []string{"/api/tasks/list", "/api/agent/conversations/list"} {
		if rec := doJSON(t, s, http.MethodPost, path, map[string]any{"entity_ids": []string{"junk"}}); rec.Code != http.StatusBadRequest {
			t.Errorf("%s with a malformed entity id = %d, want 400", path, rec.Code)
		}
	}
}

// The viewer's node is the team node's cuts over the viewer's teams, with the
// event-type cut beside the others.
func TestMeActivity_ServesTheNodeForTheViewersTeams(t *testing.T) {
	s := newTestServer(t)
	pr := seedPRSnapshot(t, s, "acme", "api", 1, domain.PRSnapshot{Number: 1, Repo: "acme/api"})
	recordTestEvent(t, s, pr.ID, domain.EventGitHubPROpened)
	recordTestEvent(t, s, pr.ID, domain.EventGitHubPROpened)

	var me struct {
		TeamIDs     []string                    `json:"team_ids"`
		ByEventType []teamActivityEventTypeJSON `json:"by_event_type"`
		BySource    []teamActivitySourceJSON    `json:"by_source"`
	}
	rec := doJSON(t, s, http.MethodGet, "/api/me/activity?since=2020-01-01&until=2030-01-01", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("GET /api/me/activity = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if len(me.TeamIDs) != 1 || me.TeamIDs[0] != runmode.LocalDefaultTeamID {
		t.Errorf("team_ids = %v, want the local team", me.TeamIDs)
	}
	if len(me.ByEventType) != 1 || me.ByEventType[0].EventType != domain.EventGitHubPROpened ||
		me.ByEventType[0].Events != 2 || me.ByEventType[0].Entities != 1 {
		t.Errorf("by_event_type = %+v, want opened with 2 events from 1 entity", me.ByEventType)
	}
	if len(me.BySource) == 0 {
		t.Error("by_source missing from the viewer's node")
	}

	var team struct {
		ByEventType []teamActivityEventTypeJSON `json:"by_event_type"`
	}
	rec = doJSON(t, s, http.MethodGet, "/api/teams/"+runmode.LocalDefaultTeamID+"/activity?since=2020-01-01&until=2030-01-01", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &team); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("GET team activity = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if len(team.ByEventType) != 1 || team.ByEventType[0] != me.ByEventType[0] {
		t.Errorf("team by_event_type = %+v, want the viewer's node's own (one team)", team.ByEventType)
	}
	if rec := doJSON(t, s, http.MethodGet, "/api/me/activity?since=junk", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("a malformed window = %d, want 400", rec.Code)
	}
}
