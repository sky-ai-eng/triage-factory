package tracker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// casLosingEntityStore wraps a real EntityStore but reports every snapshot
// CAS as lost — the straggler-ex-leader shape: another writer advanced
// poll_seq between this cycle's read and its write.
type casLosingEntityStore struct {
	db.EntityStore
	casCalls int32
}

func (s *casLosingEntityStore) UpdateSnapshotCASSystem(ctx context.Context, orgID, id, snapshotJSON string, expectedPollSeq int64) (bool, error) {
	atomic.AddInt32(&s.casCalls, 1)
	return false, nil
}

// TestRefreshGitHub_CASLossSuppressesTransitions pins the TFAC-579 contract
// end to end: the snapshot-diff is the SOLE re-emit prevention, so a cycle
// whose snapshot CAS loses must not publish the transitions it diffed —
// they would re-derive next cycle under fresh event ids (the event/trigger
// fence can't collapse them) and double-fire delegation. And nothing is
// lost by suppressing: the next cycle whose write DOES land re-diffs from
// the surviving snapshot and emits the transition then.
func TestRefreshGitHub_CASLossSuppressesTransitions(t *testing.T) {
	const basicResp = `{
		"number": 7, "node_id": "PR_node7", "title": "Stub PR",
		"state": "open", "merged": false,
		"html_url": "https://github.com/octo/repo/pull/7",
		"user": {"login": "bob"},
		"head": {"sha": "sha7", "ref": "feat"}, "base": {"ref": "main"}
	}`
	refreshResp := func(draft string) string {
		return `{"data":{"nodes":[
			{"id":"PR_node7","number":7,"title":"Stub PR","author":{"login":"bob"},
			 "state":"OPEN","merged":false,"isDraft":` + draft + `,
			 "url":"https://github.com/octo/repo/pull/7","repository":{"nameWithOwner":"octo/repo"},
			 "createdAt":"2026-06-01T00:00:00Z","updatedAt":"2026-06-10T00:00:00Z"}
		]}}`
	}

	// Cycle 1 (seed) sees a draft PR; every later cycle sees it ready — a
	// real draft→ready transition for cycles 2 and 3 to diff.
	var graphqlCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/graphql"):
			if atomic.AddInt32(&graphqlCalls, 1) == 1 {
				_, _ = w.Write([]byte(refreshResp("true")))
			} else {
				_, _ = w.Write([]byte(refreshResp("false")))
			}
		case strings.Contains(r.URL.Path, "/pulls/7"):
			_, _ = w.Write([]byte(basicResp))
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
	client := ghclient.NewClient(srv.URL, "tok")

	if _, _, err := stores.Entities.FindOrCreate(ctx, org, "github", "octo/repo#7", "pr", "", ""); err != nil {
		t.Fatalf("seed stub: %v", err)
	}

	// Cycle 1 against the REAL store: quiet-seeds the draft snapshot.
	pub := &recordingPublisher{}
	tr := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, org)
	if _, _, err := tr.RefreshGitHub(ctx, client, "", nil, nil); err != nil {
		t.Fatalf("RefreshGitHub cycle 1: %v", err)
	}
	if evts := pub.nonSystemEvents(); len(evts) != 0 {
		t.Fatalf("cycle 1 (seed) must emit no events, got %v", eventTypes(evts))
	}

	// Cycle 2 as the CAS-losing straggler: the diff computes draft→ready,
	// but the snapshot write reports lost — the transition must be
	// suppressed, not published off a snapshot that didn't win.
	losing := &casLosingEntityStore{EntityStore: stores.Entities}
	stragglerPub := &recordingPublisher{}
	straggler := New(database, stragglerPub, stores.Tasks, losing, stores.Repos, org)
	if _, _, err := straggler.RefreshGitHub(ctx, client, "", nil, nil); err != nil {
		t.Fatalf("RefreshGitHub cycle 2 (straggler): %v", err)
	}
	if atomic.LoadInt32(&losing.casCalls) == 0 {
		t.Fatal("test wiring: the straggler cycle never attempted a snapshot CAS")
	}
	if evts := stragglerPub.nonSystemEvents(); len(evts) != 0 {
		t.Errorf("a lost snapshot CAS must suppress the cycle's transitions, got %v", eventTypes(evts))
	}

	// Cycle 3 against the real store: the snapshot still says draft (the
	// straggler's write never landed), so the SAME transition re-diffs,
	// the CAS lands, and the event publishes — suppression lost nothing.
	winnerPub := &recordingPublisher{}
	winner := New(database, winnerPub, stores.Tasks, stores.Entities, stores.Repos, org)
	if _, _, err := winner.RefreshGitHub(ctx, client, "", nil, nil); err != nil {
		t.Fatalf("RefreshGitHub cycle 3: %v", err)
	}
	if evts := winnerPub.nonSystemEvents(); len(evts) != 1 || evts[0].EventType != domain.EventGitHubPRReadyForReview {
		t.Errorf("the winning cycle should emit the suppressed transition exactly once, got %v", eventTypes(evts))
	}
}
