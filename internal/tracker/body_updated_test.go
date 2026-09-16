package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

func TestBodyUpdatedDiff(t *testing.T) {
	const updated = "2026-09-15T12:00:00Z"
	for _, tc := range []struct {
		name, prev, curr string
		want             bool
	}{
		{"upgrade baseline", "", "first", false},
		{"missing field", "first", "", false},
		{"unchanged", "first", "first", false},
		{"edit", "first", "second", true},
		{"clear", "first", domain.JSONBodyHash([]byte(`null`)), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ghPrev, ghCurr := basePRSnapshot(), basePRSnapshot()
			ghPrev.BodyHash, ghCurr.BodyHash, ghCurr.UpdatedAt = tc.prev, tc.curr, updated
			jiraPrev := domain.JiraSnapshot{Key: "PROJ-1", BodyHash: tc.prev}
			jiraCurr := domain.JiraSnapshot{Key: "PROJ-1", BodyHash: tc.curr, UpdatedAt: updated}
			for _, evt := range []*domain.Event{
				findEvent(DiffPRSnapshots(ghPrev, ghCurr, testEntityID, testUser, nil), domain.EventGitHubPRBodyUpdated),
				findEvent(DiffJiraSnapshots(jiraPrev, jiraCurr, testEntityID, nil), domain.EventJiraIssueBodyUpdated),
			} {
				if (evt != nil) != tc.want {
					t.Fatalf("body update = %v, want present=%v", evt, tc.want)
				}
				if evt == nil {
					continue
				}
				var meta map[string]any
				if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
					t.Fatal(err)
				}
				if meta["previous_body_hash"] != tc.prev || meta["body_hash"] != tc.curr || evt.DedupKey != "" || evt.OccurredAt.Format(time.RFC3339) != updated {
					t.Fatalf("incorrect body revision metadata/time: %+v", evt)
				}
			}
		})
	}
	if evt := findEvent(DiffPRSnapshots(domain.PRSnapshot{}, domain.PRSnapshot{Number: 1, BodyHash: "first"}, testEntityID, testUser, nil), domain.EventGitHubPRBodyUpdated); evt != nil {
		t.Fatal("first discovery emitted a body update")
	}
	if evt := findEvent(DiffJiraSnapshots(domain.JiraSnapshot{}, domain.JiraSnapshot{Key: "PROJ-1", BodyHash: "first"}, testEntityID, nil), domain.EventJiraIssueBodyUpdated); evt != nil {
		t.Fatal("first discovery emitted a body update")
	}
}

func TestRefreshJira_BatchDescriptionLifecycle(t *testing.T) {
	var mu sync.Mutex
	var description string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/search") {
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		var request struct {
			JQL    string   `json:"jql"`
			Fields []string `json:"fields"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode search: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if !strings.Contains(request.JQL, "key IN") {
			// This tracked issue no longer matches either discovery query.
			_, _ = w.Write([]byte(`{"issues":[],"total":0}`))
			return
		}
		if !slices.Contains(request.Fields, "description") {
			t.Error("batch refresh must request description")
		}
		mu.Lock()
		field := description
		mu.Unlock()
		if field != "" {
			field = `,"description":` + field
		}
		_, _ = fmt.Fprintf(w, `{"issues":[{"key":"PROJ-1","fields":{"summary":"Tracked elsewhere","status":{"name":"Open"},"updated":"2026-09-15T12:00:00.000+0000"%s}}],"total":1}`, field)
	}))
	t.Cleanup(srv.Close)
	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	entity, _, err := stores.Entities.FindOrCreate(ctx, org, "jira", "PROJ-1", "issue", "", "")
	if err != nil {
		t.Fatal(err)
	}
	pub := &recordingPublisher{}
	tr := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, stores.EventQueue, org)
	client := jiraclient.NewClient(jiraclient.DataCenterPAT(srv.URL, "pat"))
	long := strings.Repeat("é", 2100)
	quote := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	for _, tc := range []struct {
		name, raw, preview string
		wantTotal          int
	}{
		{"quiet stub seed", quote(long + "before"), truncateDescription(long, descriptionStoreMaxRunes), 0},
		{"tail edit", quote(long + "after"), truncateDescription(long, descriptionStoreMaxRunes), 1},
		{"same again", quote(long + "after"), truncateDescription(long, descriptionStoreMaxRunes), 1},
		{"omitted is not cleared", "", truncateDescription(long, descriptionStoreMaxRunes), 1},
		{"edit after omission", `"new body"`, "new body", 2},
		{"explicit clear", `null`, "", 3},
		{"empty equals cleared", `""`, "", 3},
		{"ADF body", `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"rich text"}]}],"version":1}`, "rich text", 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			description = tc.raw
			mu.Unlock()
			if _, err := tr.RefreshJira(ctx, client, srv.URL, JiraRules{{Key: "PROJ", DoneMembers: jiraRefs("Done")}}); err != nil {
				t.Fatal(err)
			}
			got, err := stores.Entities.Get(ctx, org, entity.ID)
			if err != nil || got == nil {
				t.Fatalf("read entity: %v", err)
			}
			if got.Description != tc.preview {
				t.Fatalf("description = %q, want %q", got.Description, tc.preview)
			}
			queued, err := stores.EventQueue.ListForEntity(ctx, org, entity.ID)
			if err != nil || len(queued) != tc.wantTotal {
				t.Fatalf("durable events=%d want=%d err=%v", len(queued), tc.wantTotal, err)
			}
			for _, evt := range pub.nonSystemEvents() {
				if evt.EventType != domain.EventJiraIssueBodyUpdated {
					t.Fatalf("unexpected transition: %s", evt.EventType)
				}
			}
			if strings.Contains(got.SnapshotJSON, "rich text") || strings.Contains(got.SnapshotJSON, "éé") {
				t.Fatal("snapshot contains bulk description")
			}
		})
	}
}
