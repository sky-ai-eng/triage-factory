package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

const cursorTestHost = "https://github.com"

// listingServer answers every repo's open-PR listing with openPRBodyFor under
// ETag "etag-1", a listing that sends that ETag with a 304, and the Phase-2
// refresh with an inaccessible node. It records each listing's If-None-Match
// and the time each listing arrived, and answers a listing after delay.
type listingServer struct {
	*httptest.Server
	delay time.Duration

	mu         sync.Mutex
	conditions []string
	received   []time.Time
}

func newListingServer(t *testing.T, delay time.Duration) *listingServer {
	t.Helper()
	s := &listingServer{delay: delay}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/graphql"):
			_, _ = w.Write([]byte(`{"data":{"nodes":[null]}}`))
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			s.mu.Lock()
			s.received = append(s.received, time.Now())
			s.conditions = append(s.conditions, r.Header.Get("If-None-Match"))
			s.mu.Unlock()
			time.Sleep(s.delay)
			if r.Header.Get("If-None-Match") == `"etag-1"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"etag-1"`)
			_, _ = w.Write([]byte(openPRBodyFor(r.URL.Path)))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *listingServer) listingConditions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.conditions)
}

// faultyEntityStore is a real EntityStore with one-shot failures injected.
type faultyEntityStore struct {
	db.EntityStore
	// failCreate is the source id whose next FindOrCreateSystem fails.
	failCreate string
	// failReactivate makes the next ReactivateWithSnapshotCASSystem fail, or
	// report a lost CAS when reactivateLost is set.
	failReactivate bool
	reactivateLost bool
	// beforeCreate runs ahead of every FindOrCreateSystem.
	beforeCreate func()
}

func (s *faultyEntityStore) FindOrCreateSystem(ctx context.Context, orgID, source, scope, sourceID, externalID, kind, title, url string) (*domain.Entity, bool, error) {
	if s.beforeCreate != nil {
		s.beforeCreate()
	}
	if sourceID == s.failCreate {
		s.failCreate = ""
		return nil, false, errors.New("injected create failure")
	}
	return s.EntityStore.FindOrCreateSystem(ctx, orgID, source, scope, sourceID, externalID, kind, title, url)
}

func (s *faultyEntityStore) ReactivateWithSnapshotCASSystem(ctx context.Context, orgID, id, snapshotJSON string, expectedPollSeq int64) (bool, error) {
	if s.failReactivate {
		s.failReactivate = false
		if s.reactivateLost {
			return false, nil
		}
		return false, errors.New("injected reactivate failure")
	}
	return s.EntityStore.ReactivateWithSnapshotCASSystem(ctx, orgID, id, snapshotJSON, expectedPollSeq)
}

func pullsCursorOf(t *testing.T, stores db.Stores, repo string) (string, *time.Time) {
	t.Helper()
	etag, polledAt, err := stores.Repos.GetPullsPollStateByRefSystem(context.Background(), runmode.LocalDefaultOrgID, domain.RepoRefFromSlug(cursorTestHost, repo))
	if err != nil {
		t.Fatalf("GetPullsPollStateByRefSystem(%s): %v", repo, err)
	}
	return etag, polledAt
}

func refreshRepos(t *testing.T, tr *Tracker, client *ghclient.Client, repos ...string) {
	t.Helper()
	if _, _, err := tr.RefreshGitHub(context.Background(), cursorTestHost, client, domain.GitHubAccount{}, repos, nil); err != nil {
		t.Fatalf("RefreshGitHub: %v", err)
	}
}

// TestRefreshGitHub_FailedCreateKeepsCursor: a listed PR whose entity could
// not be created leaves its repo's cursor where it was, so the next cycle
// lists unconditionally, gets the PR again and stores it, though the open-PR
// listing has not changed in between.
func TestRefreshGitHub_FailedCreateKeepsCursor(t *testing.T) {
	srv := newListingServer(t, 0)
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	trackRepos(t, stores, org, []string{"octo/repo"})

	entities := &faultyEntityStore{EntityStore: stores.Entities, failCreate: "octo/repo#42"}
	tr := New(database, &recordingPublisher{}, stores.Tasks, entities, stores.Repos, stores.JiraProjects, stores.Artifacts, stores.EventQueue, org)
	client := ghclient.NewClient(srv.URL, "tok")

	refreshRepos(t, tr, client, "octo/repo")
	if ent, err := stores.Entities.GetBySource(context.Background(), org, "github", cursorTestHost, "octo/repo#42"); err != nil || ent != nil {
		t.Fatalf("after cycle 1: entity = %+v, err = %v; want none (its create failed)", ent, err)
	}
	if etag, polledAt := pullsCursorOf(t, stores, "octo/repo"); etag != "" || polledAt != nil {
		t.Fatalf("after cycle 1: cursor = (%q, %v); want it unwritten", etag, polledAt)
	}

	refreshRepos(t, tr, client, "octo/repo")
	if got := srv.listingConditions(); !slices.Equal(got, []string{"", ""}) {
		t.Errorf("listing If-None-Match per cycle = %q; want both unconditional", got)
	}
	ent, err := stores.Entities.GetBySource(context.Background(), org, "github", cursorTestHost, "octo/repo#42")
	if err != nil || ent == nil || ent.State != "active" {
		t.Fatalf("after cycle 2: entity = %+v, err = %v; want it active", ent, err)
	}
	if etag, _ := pullsCursorOf(t, stores, "octo/repo"); etag != `"etag-1"` {
		t.Errorf("after cycle 2: stored etag = %q; want %q", etag, `"etag-1"`)
	}
}

// TestRefreshGitHub_FailedReactivateKeepsCursor: a reopened PR whose closed
// entity was not reactivated, by an error or by a lost CAS, leaves its repo's
// cursor where it was, so the next cycle lists it again and reactivates it.
func TestRefreshGitHub_FailedReactivateKeepsCursor(t *testing.T) {
	for _, tc := range []struct {
		name string
		lost bool
	}{
		{"error", false},
		{"lost CAS", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			srv := newListingServer(t, 0)
			database := newMigratedSQLite(t)
			stores := sqlitestore.New(database)
			org := runmode.LocalDefaultOrgID
			trackRepos(t, stores, org, []string{"octo/repo"})

			ent, _, err := stores.Entities.FindOrCreateSystem(ctx, org, "github", cursorTestHost, "octo/repo#42", "", "pr", "Add widget", "https://github.com/octo/repo/pull/42")
			if err != nil {
				t.Fatalf("FindOrCreateSystem: %v", err)
			}
			closed, _ := json.Marshal(domain.PRSnapshot{Number: 42, Repo: "octo/repo", State: "CLOSED"})
			if ok, err := stores.Entities.CloseWithSnapshotCASSystem(ctx, org, ent.ID, string(closed), ent.PollSeq); err != nil || !ok {
				t.Fatalf("CloseWithSnapshotCASSystem: ok=%v err=%v", ok, err)
			}

			entities := &faultyEntityStore{EntityStore: stores.Entities, failReactivate: true, reactivateLost: tc.lost}
			tr := New(database, &recordingPublisher{}, stores.Tasks, entities, stores.Repos, stores.JiraProjects, stores.Artifacts, stores.EventQueue, org)
			client := ghclient.NewClient(srv.URL, "tok")

			refreshRepos(t, tr, client, "octo/repo")
			if etag, polledAt := pullsCursorOf(t, stores, "octo/repo"); etag != "" || polledAt != nil {
				t.Fatalf("after cycle 1: cursor = (%q, %v); want it unwritten", etag, polledAt)
			}

			refreshRepos(t, tr, client, "octo/repo")
			if got := srv.listingConditions(); !slices.Equal(got, []string{"", ""}) {
				t.Errorf("listing If-None-Match per cycle = %q; want both unconditional", got)
			}
			got, err := stores.Entities.Get(ctx, org, ent.ID)
			if err != nil || got == nil || got.State != "active" {
				t.Fatalf("after cycle 2: entity = %+v, err = %v; want it reactivated", got, err)
			}
		})
	}
}

// TestRefreshGitHub_CursorWrittenAfterPRsStored: while discovery's PRs are
// being stored, their repo's cursor is still the one the cycle started with.
func TestRefreshGitHub_CursorWrittenAfterPRsStored(t *testing.T) {
	srv := newListingServer(t, 0)
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	trackRepos(t, stores, org, []string{"octo/repo"})

	type cursor struct {
		etag     string
		polledAt *time.Time
	}
	var seen []cursor
	entities := &faultyEntityStore{EntityStore: stores.Entities}
	entities.beforeCreate = func() {
		etag, polledAt := pullsCursorOf(t, stores, "octo/repo")
		seen = append(seen, cursor{etag, polledAt})
	}
	tr := New(database, &recordingPublisher{}, stores.Tasks, entities, stores.Repos, stores.JiraProjects, stores.Artifacts, stores.EventQueue, org)

	refreshRepos(t, tr, ghclient.NewClient(srv.URL, "tok"), "octo/repo")
	if len(seen) == 0 {
		t.Fatal("FindOrCreateSystem was never called")
	}
	for i, c := range seen {
		if c.etag != "" || c.polledAt != nil {
			t.Errorf("store %d: cursor = (%q, %v) while PRs were being stored; want the pre-cycle empty cursor", i, c.etag, c.polledAt)
		}
	}
	if etag, polledAt := pullsCursorOf(t, stores, "octo/repo"); etag != `"etag-1"` || polledAt == nil {
		t.Errorf("after RefreshGitHub: cursor = (%q, %v); want %q with a polled-at", etag, polledAt, `"etag-1"`)
	}
}

// TestRefreshGitHub_CursorStampsListingStart: the stored polled-at is when
// the listing was sent, not when its response came back.
func TestRefreshGitHub_CursorStampsListingStart(t *testing.T) {
	srv := newListingServer(t, 50*time.Millisecond)
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	trackRepos(t, stores, org, []string{"octo/repo"})
	tr := New(database, &recordingPublisher{}, stores.Tasks, stores.Entities, stores.Repos, stores.JiraProjects, stores.Artifacts, stores.EventQueue, org)

	refreshRepos(t, tr, ghclient.NewClient(srv.URL, "tok"), "octo/repo")
	srv.mu.Lock()
	received := slices.Clone(srv.received)
	srv.mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("listing requests = %d; want 1", len(received))
	}
	_, polledAt := pullsCursorOf(t, stores, "octo/repo")
	if polledAt == nil {
		t.Fatal("pulls polled-at not stamped")
	}
	if polledAt.After(received[0]) {
		t.Errorf("stored polled-at %v is after the listing reached the server at %v; want the time it was sent", polledAt, received[0])
	}
}

// TestRefreshGitHub_ExpiredCycleContextKeepsFinishedCursor: a repo whose
// listing never answers lets the cycle context expire before the cursors
// are written, and a sibling whose listing finished still gets its cursor.
func TestRefreshGitHub_ExpiredCycleContextKeepsFinishedCursor(t *testing.T) {
	t.Setenv("TF_POLL_REPO_CONCURRENCY", "8")
	repos := []string{"octo/hangs", "octo/ok"}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/hangs/") {
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"etag-ok"`)
		_, _ = w.Write([]byte(openPRBodyFor(r.URL.Path)))
	}))
	t.Cleanup(srv.Close)

	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	trackRepos(t, stores, org, repos)
	tr := New(database, &recordingPublisher{}, stores.Tasks, stores.Entities, stores.Repos, stores.JiraProjects, stores.Artifacts, stores.EventQueue, org)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	// Phase 2 runs on the expired context and fails; only the cursors are
	// under test here.
	_, _, _ = tr.RefreshGitHub(ctx, cursorTestHost, ghclient.NewClient(srv.URL, "tok"), domain.GitHubAccount{}, repos, nil)

	if etag, _ := pullsCursorOf(t, stores, "octo/ok"); etag != `"etag-ok"` {
		t.Errorf("octo/ok stored etag = %q; want %q", etag, `"etag-ok"`)
	}
	if etag, polledAt := pullsCursorOf(t, stores, "octo/hangs"); etag != "" || polledAt != nil {
		t.Errorf("octo/hangs cursor = (%q, %v); want it unwritten", etag, polledAt)
	}
}
