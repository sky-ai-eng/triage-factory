package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

const (
	changeWindowNodeID = "PR_kwDOchange"
	changeWindowKey    = "octo/repo#42"
	changeWindowHost   = "https://github.com"
)

// changingPRServer is a fake GitHub serving one pull request, octo/repo#42,
// whose head, updatedAt, state and check runs a test moves between cycles. The
// open-PR listing answers 304 while what it lists is unchanged, as GitHub's
// does, so a cycle with nothing moved takes the quiet-repository path.
type changingPRServer struct {
	*httptest.Server
	t  *testing.T
	mu sync.Mutex
	// listed is whether the open-PR listing returns the pull request at all.
	listed    bool
	open      bool
	head      string
	createdAt string
	updatedAt string
	// checks are the head commit's check runs, as the GraphQL rollup's nodes.
	checks    []string
	refreshes int
	// refreshDelay holds every GraphQL refresh before it is answered, and
	// lastRefreshAt is when the latest one was answered.
	refreshDelay  time.Duration
	lastRefreshAt time.Time
}

func newChangingPRServer(t *testing.T) *changingPRServer {
	t.Helper()
	s := &changingPRServer{
		t:         t,
		listed:    true,
		open:      true,
		head:      "sha1",
		createdAt: "2026-10-01T09:00:00Z",
		updatedAt: "2026-10-30T12:00:00Z",
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

func (s *changingPRServer) serve(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/graphql") {
		s.mu.Lock()
		delay := s.refreshDelay
		s.mu.Unlock()
		time.Sleep(delay)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/graphql"):
		s.refreshes++
		s.lastRefreshAt = time.Now()
		_, _ = w.Write([]byte(s.graphQLBody()))
	case strings.HasSuffix(r.URL.Path, "/repos/octo/repo/pulls"):
		etag := fmt.Sprintf(`"%t-%t-%s-%s"`, s.listed, s.open, s.head, s.updatedAt)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		if !s.listed || !s.open {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte(`[` + s.restPR() + `]`))
	case strings.HasSuffix(r.URL.Path, "/repos/octo/repo/pulls/42"):
		_, _ = w.Write([]byte(s.restPR()))
	case isBranchRefRead(r):
		http.NotFound(w, r)
	default:
		s.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected", http.StatusNotFound)
	}
}

func (s *changingPRServer) restPR() string {
	state := "open"
	if !s.open {
		state = "closed"
	}
	return `{
		"number": 42, "node_id": "` + changeWindowNodeID + `", "title": "Add widget",
		"state": "` + state + `", "draft": false, "merged": false,
		"html_url": "https://github.com/octo/repo/pull/42",
		"created_at": "` + s.createdAt + `", "updated_at": "` + s.updatedAt + `",
		"user": {"login": "alice"},
		"head": {"sha": "` + s.head + `", "ref": "feature", "repo": {"full_name": "octo/repo"}},
		"base": {"ref": "main", "repo": {"full_name": "octo/repo"}},
		"requested_reviewers": [], "requested_teams": [], "labels": []
	}`
}

func (s *changingPRServer) graphQLBody() string {
	state := "OPEN"
	if !s.open {
		state = "CLOSED"
	}
	rollup := "null"
	if len(s.checks) > 0 {
		rollup = `{"contexts": {"pageInfo": {"hasNextPage": false}, "nodes": [` + strings.Join(s.checks, ",") + `]}}`
	}
	return `{"data": {"nodes": [{
		"id": "` + changeWindowNodeID + `", "number": 42, "title": "Add widget",
		"author": {"login": "alice"}, "state": "` + state + `", "merged": false, "isDraft": false,
		"url": "https://github.com/octo/repo/pull/42", "repository": {"nameWithOwner": "octo/repo"},
		"headRefName": "feature", "baseRefName": "main",
		"createdAt": "` + s.createdAt + `", "updatedAt": "` + s.updatedAt + `",
		"commits": {"nodes": [{"commit": {
			"oid": "` + s.head + `", "committedDate": "2026-01-01T00:00:00Z",
			"statusCheckRollup": ` + rollup + `
		}}]}
	}]}}`
}

// set moves the pull request on GitHub under the server's lock.
func (s *changingPRServer) set(move func(s *changingPRServer)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	move(s)
}

func (s *changingPRServer) refreshCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshes
}

// completedCheck is a check run GitHub reports finished, as a rollup node.
func completedCheck(id int, name, conclusion string) string {
	return fmt.Sprintf(`{"__typename": "CheckRun", "databaseId": %d, "name": %q, "status": "COMPLETED",
		"conclusion": %q, "completedAt": "2026-10-30T12:05:00Z",
		"detailsUrl": "https://github.com/octo/repo/actions/runs/9/job/%d",
		"checkSuite": {"workflowRun": {"databaseId": 9}}}`, id, name, conclusion, id)
}

// changeWindowRig is a tracker polling a changingPRServer over real stores.
type changeWindowRig struct {
	t      *testing.T
	srv    *changingPRServer
	stores db.Stores
	pub    *recordingPublisher
	tr     *Tracker
	client *ghclient.Client
}

func newChangeWindowRig(t *testing.T) *changeWindowRig {
	t.Helper()
	srv := newChangingPRServer(t)
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	trackRepos(t, stores, org, []string{"octo/repo"})
	pub := &recordingPublisher{}
	return &changeWindowRig{
		t:      t,
		srv:    srv,
		stores: stores,
		pub:    pub,
		tr:     New(database, pub, stores.Tasks, stores.Entities, stores.Repos, stores.JiraProjects, stores.Deliverables, stores.ExternalActions, stores.EventQueue, stores.PRMergeability, org),
		client: ghclient.NewClient(srv.URL, "tok"),
	}
}

// cycle runs one poll cycle and returns the event types it emitted, and the
// window a change stamp it writes must fall in: the cycle's start through its
// end. from is truncated to the second because setStamp writes whole seconds.
func (r *changeWindowRig) cycle() (emitted []string, from, to time.Time) {
	r.t.Helper()
	before := len(r.pub.nonSystemEvents())
	from = time.Now().UTC().Truncate(time.Second)
	if _, _, err := r.tr.RefreshGitHub(context.Background(), changeWindowHost, r.client, domain.GitHubAccount{}, []string{"octo/repo"}, nil); err != nil {
		r.t.Fatalf("RefreshGitHub: %v", err)
	}
	to = time.Now().UTC()
	return eventTypes(r.pub.nonSystemEvents()[before:]), from, to
}

func (r *changeWindowRig) entity() domain.Entity {
	r.t.Helper()
	e, err := r.stores.Entities.GetBySource(context.Background(), runmode.LocalDefaultOrgID, "github", changeWindowHost, changeWindowKey)
	if err != nil || e == nil {
		r.t.Fatalf("GetBySource(%s): entity = %v, err = %v", changeWindowKey, e, err)
	}
	return *e
}

func (r *changeWindowRig) snapshot() domain.PRSnapshot {
	r.t.Helper()
	var snap domain.PRSnapshot
	if err := json.Unmarshal([]byte(r.entity().SnapshotJSON), &snap); err != nil {
		r.t.Fatalf("stored snapshot: %v", err)
	}
	return snap
}

// setStamp rewrites the stored snapshot's change stamp, as if TF had last seen
// the pull request change at another time.
func (r *changeWindowRig) setStamp(stamp string) {
	r.t.Helper()
	e := r.entity()
	snap := r.snapshot()
	snap.ChangeSeenAt = stamp
	raw, _ := json.Marshal(snap)
	if ok, err := r.stores.Entities.UpdateSnapshotCASSystem(context.Background(), runmode.LocalDefaultOrgID, e.ID, string(raw), e.PollSeq); err != nil || !ok {
		r.t.Fatalf("rewrite stored snapshot: ok=%v err=%v", ok, err)
	}
}

// assertStampedIn fails unless the stored snapshot's change stamp is a time in
// [from, to].
func (r *changeWindowRig) assertStampedIn(from, to time.Time) {
	r.t.Helper()
	stamp := r.snapshot().ChangeSeenAt
	seen, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		r.t.Fatalf("change_seen_at = %q; want a time within the cycle", stamp)
	}
	if seen.Before(from) || seen.After(to) {
		r.t.Fatalf("change_seen_at = %s; want a time within the cycle, in [%s, %s]", stamp, from.Format(time.RFC3339), to.Format(time.RFC3339))
	}
}

func countType(types []string, want string) int {
	n := 0
	for _, typ := range types {
		if typ == want {
			n++
		}
	}
	return n
}

// TestRefreshGitHub_ChangeWindowReadsCheckRunsCreatedAfterRefresh is the race
// the window exists for. A push moves the head; the cycle that sees it
// refreshes before GitHub has created the check runs the push starts, and
// stores none. The next cycle's listing is a 304 (head and updatedAt unchanged,
// since a check run completing bumps neither), but GitHub now reports the check
// passed. Inside the window that cycle refreshes and emits ci_check_passed.
// Without a stamp, the gate skips the pull request as it did before the window
// existed, and the result waits for the staleness floor.
func TestRefreshGitHub_ChangeWindowReadsCheckRunsCreatedAfterRefresh(t *testing.T) {
	for _, tc := range []struct {
		name string
		// clearStamp strips the change stamp the push cycle wrote, which is
		// the gate as it was before the stamp existed.
		clearStamp bool
		wantPassed int
	}{
		{name: "stamped by the push"},
		{name: "no stamp", clearStamp: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newChangeWindowRig(t)
			r.srv.set(func(s *changingPRServer) { s.checks = []string{completedCheck(101, "build", "SUCCESS")} })
			r.cycle()

			// The push: a new head, and no check runs on it yet.
			r.srv.set(func(s *changingPRServer) {
				s.head, s.updatedAt, s.checks = "sha2", "2026-10-30T12:10:00Z", nil
			})
			if got, _, _ := r.cycle(); countType(got, domain.EventGitHubPRNewCommits) != 1 {
				t.Fatalf("push cycle emitted %v; want one new_commits", got)
			}
			if snap := r.snapshot(); snap.CheckRuns == nil || len(snap.CheckRuns) != 0 {
				t.Fatalf("push cycle stored check_runs %+v; want [] (polled, nothing here yet)", snap.CheckRuns)
			}
			if tc.clearStamp {
				r.setStamp("")
			}

			// CI finishes on the new head. The pull request itself is unchanged.
			r.srv.set(func(s *changingPRServer) { s.checks = []string{completedCheck(102, "build", "SUCCESS")} })
			refreshesBefore := r.srv.refreshCount()
			got, _, _ := r.cycle()
			refreshed := r.srv.refreshCount() > refreshesBefore

			if tc.clearStamp {
				if refreshed || len(got) != 0 {
					t.Fatalf("with no stamp: refreshed=%v, emitted %v; want the gate to skip it, as before the window", refreshed, got)
				}
				return
			}
			if !refreshed {
				t.Fatal("the cycle after the push skipped the pull request; want the window to refresh it")
			}
			if n := countType(got, domain.EventGitHubPRCICheckPassed); n != 1 || len(got) != 1 {
				t.Fatalf("the cycle after the push emitted %v; want exactly one ci_check_passed", got)
			}

			// Once read, nothing is emitted again: a further cycle inside the
			// window refreshes the unchanged pull request and diffs nothing.
			if again, _, _ := r.cycle(); len(again) != 0 {
				t.Fatalf("a later cycle in the window emitted %v; want nothing", again)
			}
		})
	}
}

// TestRefreshGitHub_ChangeSeenAtLifecycle pins where the tracker writes the
// change stamp: a refresh that sees the head or updatedAt move stamps its own
// time; one that sees no change carries the stored stamp and writes no
// snapshot; a newly opened pull request and a reopened one are stamped; a
// baseline pull request and a quiet seed are not.
func TestRefreshGitHub_ChangeSeenAtLifecycle(t *testing.T) {
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	t.Run("a refresh that sees the head move stamps the refresh's time", func(t *testing.T) {
		r := newChangeWindowRig(t)
		r.cycle()
		r.srv.set(func(s *changingPRServer) { s.head, s.updatedAt = "sha2", "2026-10-30T12:10:00Z" })
		_, from, to := r.cycle()
		r.assertStampedIn(from, to)
	})

	t.Run("a refresh that sees only updatedAt move stamps the refresh's time", func(t *testing.T) {
		r := newChangeWindowRig(t)
		r.cycle()
		r.setStamp(time.Now().UTC().Add(-time.Hour).Format(time.RFC3339))
		// A label, or a comment a CI bot acts on: updatedAt moves, the head
		// does not.
		r.srv.set(func(s *changingPRServer) { s.updatedAt = "2026-10-30T12:20:00Z" })
		_, from, to := r.cycle()
		r.assertStampedIn(from, to)
	})

	t.Run("a refresh that sees no change carries the stamp and writes no snapshot", func(t *testing.T) {
		r := newChangeWindowRig(t)
		r.cycle()
		stamp := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
		r.setStamp(stamp)
		before := r.entity()
		refreshesBefore := r.srv.refreshCount()
		r.cycle()
		if r.srv.refreshCount() == refreshesBefore {
			t.Fatal("the window did not refresh the pull request; the test needs a window-forced refresh")
		}
		after := r.entity()
		if after.PollSeq != before.PollSeq || after.SnapshotJSON != before.SnapshotJSON {
			t.Fatalf("a refresh that saw no change wrote the snapshot: poll_seq %d → %d, snapshot %s → %s",
				before.PollSeq, after.PollSeq, before.SnapshotJSON, after.SnapshotJSON)
		}
		if got := r.snapshot().ChangeSeenAt; got != stamp {
			t.Fatalf("change_seen_at = %q; want the stored %q carried", got, stamp)
		}
	})

	t.Run("a pull request opened since the last listing is stamped", func(t *testing.T) {
		r := newChangeWindowRig(t)
		// The repository's first listing has no pull request; the next one
		// returns one opened since.
		r.srv.set(func(s *changingPRServer) { s.listed = false })
		r.cycle()
		r.srv.set(func(s *changingPRServer) {
			s.listed, s.createdAt, s.updatedAt = true, time.Now().UTC().Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339)
		})
		_, from, to := r.cycle()
		r.assertStampedIn(from, to)
	})

	t.Run("a pull request a repository's first listing returns is not stamped", func(t *testing.T) {
		r := newChangeWindowRig(t)
		r.cycle()
		if got := r.snapshot().ChangeSeenAt; got != "" {
			t.Fatalf("change_seen_at = %q; want none on a pull request open before TF listed its repository", got)
		}
	})

	t.Run("a reopened pull request is stamped", func(t *testing.T) {
		r := newChangeWindowRig(t)
		ent, _, err := r.stores.Entities.FindOrCreateSystem(ctx, org, "github", changeWindowHost, changeWindowKey, "", "pr", "Add widget", "https://github.com/octo/repo/pull/42")
		if err != nil {
			t.Fatalf("FindOrCreateSystem: %v", err)
		}
		closed, _ := json.Marshal(domain.PRSnapshot{NodeID: changeWindowNodeID, Number: 42, Repo: "octo/repo", State: "CLOSED"})
		if ok, err := r.stores.Entities.CloseWithSnapshotCASSystem(ctx, org, ent.ID, string(closed), ent.PollSeq); err != nil || !ok {
			t.Fatalf("CloseWithSnapshotCASSystem: ok=%v err=%v", ok, err)
		}
		_, from, to := r.cycle()
		if got := r.entity(); got.State != "active" {
			t.Fatalf("entity state = %q; want reactivated", got.State)
		}
		r.assertStampedIn(from, to)
	})

	t.Run("a quiet seed is not stamped", func(t *testing.T) {
		r := newChangeWindowRig(t)
		// Not in the listing, so only the snapshot-less stub path reaches it.
		r.srv.set(func(s *changingPRServer) { s.listed = false })
		if _, _, err := r.stores.Entities.FindOrCreate(ctx, org, "github", changeWindowHost, changeWindowKey, "", "pr", "", ""); err != nil {
			t.Fatalf("seed stub: %v", err)
		}
		r.cycle()
		snap := r.snapshot()
		if snap.NodeID != changeWindowNodeID {
			t.Fatalf("stub was not seeded: %+v", snap)
		}
		if snap.ChangeSeenAt != "" {
			t.Fatalf("change_seen_at = %q; want none on a quiet seed", snap.ChangeSeenAt)
		}
	})
}

// TestRefreshGitHub_ChangeSeenAtIsTheRefreshTime: the stamp is when the refresh
// that saw the change was answered, not when the cycle started. Discovery and
// the refresh batches can take minutes at scale; a stamp from the cycle's start
// would then be expired before the next cycle, which would skip the check runs
// created just after that refresh. The refresh here answers more than a second
// after the cycle starts, so a cycle-start stamp fails the test. A newly opened
// and a reopened pull request are stamped by discovery first, and must be
// stamped again by the refresh that first reads their check runs.
func TestRefreshGitHub_ChangeSeenAtIsTheRefreshTime(t *testing.T) {
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	for _, tc := range []struct {
		name string
		// setup runs before the delayed cycle; move changes the pull request
		// on GitHub just before it.
		setup func(r *changeWindowRig)
		move  func(s *changingPRServer)
	}{
		{
			name:  "a head push",
			setup: func(r *changeWindowRig) { r.cycle() },
			move:  func(s *changingPRServer) { s.head, s.updatedAt = "sha2", "2026-10-30T12:10:00Z" },
		},
		{
			name: "a pull request opened since the last listing",
			setup: func(r *changeWindowRig) {
				r.srv.set(func(s *changingPRServer) { s.listed = false })
				r.cycle()
			},
			move: func(s *changingPRServer) {
				now := time.Now().UTC().Format(time.RFC3339)
				s.listed, s.createdAt, s.updatedAt = true, now, now
			},
		},
		{
			name: "a reopened pull request",
			setup: func(r *changeWindowRig) {
				ent, _, err := r.stores.Entities.FindOrCreateSystem(ctx, org, "github", changeWindowHost, changeWindowKey, "", "pr", "Add widget", "https://github.com/octo/repo/pull/42")
				if err != nil {
					t.Fatalf("FindOrCreateSystem: %v", err)
				}
				closed, _ := json.Marshal(domain.PRSnapshot{NodeID: changeWindowNodeID, Number: 42, Repo: "octo/repo", State: "CLOSED"})
				if ok, err := r.stores.Entities.CloseWithSnapshotCASSystem(ctx, org, ent.ID, string(closed), ent.PollSeq); err != nil || !ok {
					t.Fatalf("CloseWithSnapshotCASSystem: ok=%v err=%v", ok, err)
				}
			},
			move: func(*changingPRServer) {},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newChangeWindowRig(t)
			tc.setup(r)
			r.srv.set(func(s *changingPRServer) {
				tc.move(s)
				s.refreshDelay = 1100 * time.Millisecond
			})
			r.cycle()

			stamp := r.snapshot().ChangeSeenAt
			seen, err := time.Parse(time.RFC3339, stamp)
			if err != nil {
				t.Fatalf("change_seen_at = %q; want a time", stamp)
			}
			r.srv.mu.Lock()
			answered := r.srv.lastRefreshAt
			r.srv.mu.Unlock()
			if seen.Before(answered) {
				t.Fatalf("change_seen_at = %s, before the refresh that saw the change was answered at %s",
					stamp, answered.UTC().Format(time.RFC3339Nano))
			}
		})
	}
}
