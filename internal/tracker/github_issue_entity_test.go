package tracker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestRefreshGitHub_MakesNoRequestForAnIssueEntity: an issue entity, which a
// run's write creates, is not polled yet, so the pull request poller neither
// refreshes it nor asks GitHub anything about it.
func TestRefreshGitHub_MakesNoRequestForAnIssueEntity(t *testing.T) {
	var mu sync.Mutex
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		mu.Unlock()
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	issue, _, err := stores.Entities.FindOrCreate(ctx, org, "github", "https://github.com", "octo/repo#9", "I_kw9", domain.EntityKindIssue, "", "")
	if err != nil {
		t.Fatalf("seed the issue entity: %v", err)
	}

	tr := New(database, &recordingPublisher{}, stores.Tasks, stores.Entities, stores.Repos, stores.JiraProjects, stores.Deliverables, stores.EventQueue, org)
	if _, _, err := tr.RefreshGitHub(ctx, "https://github.com", ghclient.NewClient(srv.URL, "tok"), domain.GitHubAccount{}, nil, nil); err != nil {
		t.Fatalf("RefreshGitHub: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 0 {
		t.Errorf("requests = %q, want none for an issue entity", requests)
	}
	got, err := stores.Entities.GetSystem(ctx, org, issue.ID)
	if err != nil || got == nil || got.SnapshotJSON != "" || got.State != "active" {
		t.Errorf("issue entity after the cycle = %+v, %v; want it untouched", got, err)
	}
}

// TestRefreshGitHub_AStubThatIsAnIssueBecomesOne: a pull request stub created
// from a key alone, before writes resolved their objects by id, may be an
// issue. Its pull request read answers 404, GitHub answers the number as an
// issue, and the stub takes kind issue and the issue's node id, so the next
// cycle asks GitHub nothing about it.
func TestRefreshGitHub_AStubThatIsAnIssueBecomesOne(t *testing.T) {
	var mu sync.Mutex
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/repos/octo/repo/issues/12"):
			_, _ = w.Write([]byte(`{"number":12,"node_id":"I_kw12","state":"open"}`))
		case strings.HasSuffix(r.URL.Path, "/repos/octo/repo"):
			_, _ = w.Write([]byte(`{"id":55,"full_name":"octo/repo"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	stub, _, err := stores.Entities.FindOrCreate(ctx, org, "github", "https://github.com", "octo/repo#12", "", "pr", "", "")
	if err != nil {
		t.Fatalf("seed the stub: %v", err)
	}

	tr := New(database, &recordingPublisher{}, stores.Tasks, stores.Entities, stores.Repos, stores.JiraProjects, stores.Deliverables, stores.EventQueue, org)
	client := ghclient.NewClient(srv.URL, "tok")
	if _, _, err := tr.RefreshGitHub(ctx, "https://github.com", client, domain.GitHubAccount{}, nil, nil); err != nil {
		t.Fatalf("RefreshGitHub: %v", err)
	}
	got, err := stores.Entities.GetSystem(ctx, org, stub.ID)
	if err != nil || got == nil || got.Kind != domain.EntityKindIssue || got.ExternalID != "I_kw12" || got.SnapshotJSON != "" {
		t.Fatalf("stub after the cycle = %+v, %v; want an issue carrying I_kw12, unseeded", got, err)
	}

	mu.Lock()
	requests = nil
	mu.Unlock()
	if _, _, err := tr.RefreshGitHub(ctx, "https://github.com", client, domain.GitHubAccount{}, nil, nil); err != nil {
		t.Fatalf("second RefreshGitHub: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 0 {
		t.Errorf("second cycle requests = %q, want none for the issue", requests)
	}
}
