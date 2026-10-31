package tracker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// A refresh admits a mergeability probe for an open pull request whose
// answer can have changed since it was read: one with no answer, one whose
// head moved, and one whose answer is older than the sweep interval. A
// repository whose listing answered 304 gives no current head, so its pull
// requests admit nothing on the head rule.

// stored is the answer a case seeds before the cycle: the head it was read
// at and how long ago.
type storedAnswer struct {
	head string
	age  time.Duration
}

// refreshAdmissions runs one cycle over octo/repo#42 (quietPRListing, head
// deadbeef), its snapshot already current so the gate skips the refresh, and
// returns the reasons of the probes the cycle admitted.
func refreshAdmissions(t *testing.T, answer *storedAnswer, notModified bool) []string {
	t.Helper()
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/graphql"):
			t.Errorf("the cycle refreshed a pull request whose snapshot is current")
			_, _ = w.Write([]byte(`{"data":{"nodes":[null]}}`))
		case strings.HasSuffix(r.URL.Path, "/repos/octo/repo/pulls"):
			if r.Header.Get("If-None-Match") == `"etag-1"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"etag-2"`)
			_, _ = w.Write([]byte(quietPRListing))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	ent, _, err := stores.Entities.FindOrCreateSystem(ctx, org, "github", cursorTestHost, "octo/repo#42", "PR_quiet", "pr", "Add widget", "")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	trackRepos(t, stores, org, []string{"octo/repo"})
	raw, _ := json.Marshal(domain.PRSnapshot{
		NodeID: "PR_quiet", Number: 42, Title: "Add widget", Author: "alice", Repo: "octo/repo",
		State: "OPEN", HeadRef: "feature", BaseRef: "main", HeadSHA: "deadbeef",
		CreatedAt: "2026-05-20T10:00:00Z", UpdatedAt: "2026-05-29T12:00:00Z", CheckRuns: []domain.CheckRun{},
	})
	if ok, err := stores.Entities.UpdateSnapshotCASSystem(ctx, org, ent.ID, string(raw), ent.PollSeq); err != nil || !ok {
		t.Fatalf("seed snapshot: ok=%v err=%v", ok, err)
	}
	if notModified {
		if err := stores.Repos.SetPullsPollStateSystem(ctx, org, ent.RepositoryID, `"etag-1"`, ent.CreatedAt); err != nil {
			t.Fatalf("seed listing cursor: %v", err)
		}
	}
	if answer != nil {
		if _, err := database.Exec(`INSERT INTO pull_request_mergeability (entity_id, mergeable, mergeable_head_sha, checked_at) VALUES (?, 'MERGEABLE', ?, ?)`,
			ent.ID, answer.head, time.Now().UTC().Add(-answer.age)); err != nil {
			t.Fatalf("seed stored answer: %v", err)
		}
	}

	tr := New(database, &recordingPublisher{}, stores.Tasks, stores.Entities, stores.Repos, stores.JiraProjects, stores.Deliverables, stores.ExternalActions, stores.EventQueue, stores.PRMergeability, org)
	refreshRepos(t, tr, ghclient.NewClient(srv.URL, "tok"), "octo/repo")

	rows, err := database.Query(`SELECT reason FROM pr_mergeability_probes WHERE entity_id = ? ORDER BY id`, ent.ID)
	if err != nil {
		t.Fatalf("read probes: %v", err)
	}
	defer rows.Close()
	var reasons []string
	for rows.Next() {
		var reason string
		if err := rows.Scan(&reason); err != nil {
			t.Fatalf("scan probe: %v", err)
		}
		reasons = append(reasons, reason)
	}
	return reasons
}

func TestRefreshGitHub_AdmitsNewForAPullRequestWithNoAnswer(t *testing.T) {
	if got := refreshAdmissions(t, nil, false); !slices.Equal(got, []string{domain.MergeabilityProbeNew}) {
		t.Errorf("admitted %v, want [new]", got)
	}
}

func TestRefreshGitHub_AdmitsHeadMovedWhenTheListingNamesAnotherHead(t *testing.T) {
	if got := refreshAdmissions(t, &storedAnswer{head: "earlier", age: time.Minute}, false); !slices.Equal(got, []string{domain.MergeabilityProbeHeadMoved}) {
		t.Errorf("admitted %v, want [head_moved]", got)
	}
}

func TestRefreshGitHub_A304ListingAdmitsNoHeadMoved(t *testing.T) {
	if got := refreshAdmissions(t, &storedAnswer{head: "earlier", age: time.Minute}, true); len(got) != 0 {
		t.Errorf("admitted %v behind an unchanged listing, want nothing", got)
	}
}

func TestRefreshGitHub_AdmitsSweepForAnAnswerOlderThanTheInterval(t *testing.T) {
	if got := refreshAdmissions(t, &storedAnswer{head: "deadbeef", age: mergeabilitySweepInterval + time.Minute}, false); !slices.Equal(got, []string{domain.MergeabilityProbeSweep}) {
		t.Errorf("admitted %v, want [sweep]", got)
	}
	if got := refreshAdmissions(t, &storedAnswer{head: "deadbeef", age: time.Minute}, false); len(got) != 0 {
		t.Errorf("admitted %v for a fresh answer at the current head, want nothing", got)
	}
}
