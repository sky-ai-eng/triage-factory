package tracker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// GitHub answers UNKNOWN for mergeable while it computes mergeability. The
// refresh stores the last known answer instead, so an UNKNOWN reading between
// two others is neither a conflict nor a resolution.

// refreshMergeableSequence polls one tracked pull request once per reading,
// GitHub answering each reading's mergeable in turn, and returns every
// transition the refreshes emitted and the mergeable each stored. The first
// reading quiet-seeds the snapshot-less stub.
func refreshMergeableSequence(t *testing.T, readings ...string) (emitted []domain.Event, stored []string) {
	t.Helper()
	var (
		mu        sync.Mutex
		mergeable string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/graphql"):
			mu.Lock()
			m := mergeable
			mu.Unlock()
			_, _ = w.Write([]byte(`{"data":{"nodes":[
				{"id":"PR_node7","number":7,"title":"Stub PR","body":"",
				 "author":{"login":"bob"},"state":"OPEN","merged":false,"mergeable":` + strconv.Quote(m) + `,
				 "url":"https://github.com/octo/repo/pull/7",
				 "repository":{"nameWithOwner":"octo/repo"},
				 "createdAt":"2026-06-01T00:00:00Z","updatedAt":"2026-06-10T00:00:00Z"}
			]}}`))
		case strings.Contains(r.URL.Path, "/pulls/7"):
			_, _ = w.Write([]byte(`{
				"number": 7, "node_id": "PR_node7", "title": "Stub PR",
				"state": "open", "merged": false,
				"html_url": "https://github.com/octo/repo/pull/7",
				"user": {"login": "bob"},
				"head": {"sha": "sha7", "ref": "feat"}, "base": {"ref": "main"}
			}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	entity, _, err := stores.Entities.FindOrCreate(ctx, org, "github", "https://github.com", "octo/repo#7", "", "pr", "", "")
	if err != nil {
		t.Fatalf("seed stub: %v", err)
	}
	pub := &recordingPublisher{}
	tr := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, stores.JiraProjects, stores.Deliverables, stores.ExternalActions, stores.EventQueue, org)
	client := ghclient.NewClient(srv.URL, "tok")

	for _, reading := range readings {
		mu.Lock()
		mergeable = reading
		mu.Unlock()
		if _, _, err := tr.RefreshGitHub(ctx, "https://github.com", client, domain.GitHubAccount{}, nil, nil); err != nil {
			t.Fatalf("refresh reading %s: %v", reading, err)
		}
		got, err := stores.Entities.Get(ctx, org, entity.ID)
		if err != nil || got == nil {
			t.Fatalf("read entity: %v", err)
		}
		var snap domain.PRSnapshot
		if err := json.Unmarshal([]byte(got.SnapshotJSON), &snap); err != nil {
			t.Fatalf("parse snapshot: %v", err)
		}
		stored = append(stored, snap.Mergeable)
	}
	return pub.nonSystemEvents(), stored
}

func TestRefreshGitHub_UnknownBetweenConflictsEmitsConflictsOnce(t *testing.T) {
	emitted, stored := refreshMergeableSequence(t, "MERGEABLE", "CONFLICTING", "UNKNOWN", "CONFLICTING")
	if got := eventTypes(emitted); len(got) != 1 || got[0] != domain.EventGitHubPRConflicts {
		t.Errorf("transitions = %v, want one %s", got, domain.EventGitHubPRConflicts)
	}
	if stored[2] != "CONFLICTING" {
		t.Errorf("mergeable stored after the UNKNOWN reading = %q, want CONFLICTING", stored[2])
	}
}

func TestRefreshGitHub_UnknownBeforeMergeableEmitsConflictsResolved(t *testing.T) {
	emitted, _ := refreshMergeableSequence(t, "MERGEABLE", "CONFLICTING", "UNKNOWN", "MERGEABLE")
	want := []string{domain.EventGitHubPRConflicts, domain.EventGitHubPRConflictsResolved}
	if got := eventTypes(emitted); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("transitions = %v, want %v", got, want)
	}
}
