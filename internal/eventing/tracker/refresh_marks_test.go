package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

const marksHost = "https://github.com"

// marksPR is one pull request a marksServer serves.
type marksPR struct {
	repo      string
	number    int
	head      string
	updatedAt string
	open      bool
	checks    []string
}

func (p *marksPR) nodeID() string {
	return fmt.Sprintf("PR_%s_%d", strings.ReplaceAll(p.repo, "/", "_"), p.number)
}

// marksServer is a fake GitHub serving pull requests in several repositories.
// It counts each repository's open-PR listings and base-branch reads, and
// records every node id a refresh asked for, so a test can tell which
// repositories and pull requests a pass touched.
type marksServer struct {
	*httptest.Server
	t   *testing.T
	mu  sync.Mutex
	prs []*marksPR
	// listings is open-PR listings sent, by owner/repo.
	listings map[string]int
	// refs is base-branch tip reads sent, by owner/repo@branch.
	refs map[string]int
	// refreshed is every node id a GraphQL refresh asked for, in order.
	refreshed []string
}

func newMarksServer(t *testing.T, prs ...*marksPR) *marksServer {
	t.Helper()
	s := &marksServer{t: t, prs: prs, listings: map[string]int{}, refs: map[string]int{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

func (s *marksServer) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api/v3")
	switch {
	case path == "/graphql" || strings.HasSuffix(path, "/graphql"):
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Variables struct {
				IDs []string `json:"ids"`
			} `json:"variables"`
		}
		_ = json.Unmarshal(body, &req)
		nodes := make([]string, 0, len(req.Variables.IDs))
		for _, id := range req.Variables.IDs {
			s.refreshed = append(s.refreshed, id)
			nodes = append(nodes, s.node(id))
		}
		_, _ = w.Write([]byte(`{"data":{"nodes":[` + strings.Join(nodes, ",") + `]}}`))
	case strings.HasPrefix(path, "/repos/") && strings.HasSuffix(path, "/pulls"):
		repo := strings.TrimSuffix(strings.TrimPrefix(path, "/repos/"), "/pulls")
		s.listings[repo]++
		var listed []string
		etag := repo
		for _, p := range s.prs {
			if p.repo == repo && p.open {
				listed = append(listed, s.restPR(p))
				etag += "|" + p.head + "@" + p.updatedAt
			}
		}
		etag = fmt.Sprintf("%q", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		_, _ = w.Write([]byte(`[` + strings.Join(listed, ",") + `]`))
	case strings.Contains(path, "/git/ref/heads/"):
		repo, branch, _ := strings.Cut(strings.TrimPrefix(path, "/repos/"), "/git/ref/heads/")
		s.refs[repo+"@"+branch]++
		etag := `"tip-1"`
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		_, _ = w.Write([]byte(`{"ref":"refs/heads/` + branch + `","object":{"sha":"tip1","type":"commit"}}`))
	default:
		s.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected", http.StatusNotFound)
	}
}

func (s *marksServer) find(nodeID string) *marksPR {
	for _, p := range s.prs {
		if p.nodeID() == nodeID {
			return p
		}
	}
	return nil
}

func (s *marksServer) restPR(p *marksPR) string {
	return fmt.Sprintf(`{
		"number": %d, "node_id": %q, "title": "PR %d", "state": "open", "draft": false, "merged": false,
		"html_url": "https://github.com/%s/pull/%d",
		"created_at": "2026-10-01T09:00:00Z", "updated_at": %q,
		"user": {"login": "alice"},
		"head": {"sha": %q, "ref": "feature-%d", "repo": {"full_name": %q}},
		"base": {"ref": "main", "repo": {"full_name": %q}},
		"requested_reviewers": [], "requested_teams": [], "labels": []
	}`, p.number, p.nodeID(), p.number, p.repo, p.number, p.updatedAt, p.head, p.number, p.repo, p.repo)
}

func (s *marksServer) node(id string) string {
	p := s.find(id)
	if p == nil {
		return "null"
	}
	state := "OPEN"
	if !p.open {
		state = "MERGED"
	}
	rollup := "null"
	if len(p.checks) > 0 {
		rollup = `{"contexts": {"pageInfo": {"hasNextPage": false}, "nodes": [` + strings.Join(p.checks, ",") + `]}}`
	}
	return fmt.Sprintf(`{
		"id": %q, "number": %d, "title": "PR %d",
		"author": {"login": "alice"}, "state": %q, "merged": %t, "isDraft": false,
		"url": "https://github.com/%s/pull/%d", "repository": {"nameWithOwner": %q},
		"headRefName": "feature-%d", "baseRefName": "main",
		"createdAt": "2026-10-01T09:00:00Z", "updatedAt": %q,
		"commits": {"nodes": [{"commit": {"oid": %q, "committedDate": "2026-01-01T00:00:00Z", "statusCheckRollup": %s}}]}
	}`, p.nodeID(), p.number, p.number, state, !p.open, p.repo, p.number, p.repo, p.number, p.updatedAt, p.head, rollup)
}

// set changes what the server serves, under its lock.
func (s *marksServer) set(change func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	change()
}

// take returns what the server counted since the last take, and resets it.
func (s *marksServer) take() (listings, refs map[string]int, refreshed []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	listings, refs, refreshed = s.listings, s.refs, s.refreshed
	s.listings, s.refs, s.refreshed = map[string]int{}, map[string]int{}, nil
	return listings, refs, refreshed
}

// marksRig is the tracker polling a marksServer over real stores, with every
// served repository tracked.
type marksRig struct {
	t        *testing.T
	srv      *marksServer
	database *sql.DB
	stores   db.Stores
	pub      *recordingPublisher
	client   *ghclient.Client
	repoID   map[string]string
	now      time.Time
	repos    []string
}

func newMarksRig(t *testing.T, repos []string, prs ...*marksPR) *marksRig {
	t.Helper()
	srv := newMarksServer(t, prs...)
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	trackRepos(t, stores, runmode.LocalDefaultOrgID, repos)
	r := &marksRig{t: t, srv: srv, database: database, stores: stores, pub: &recordingPublisher{}, client: ghclient.NewClient(srv.URL, "tok"), repoID: map[string]string{}, repos: repos, now: time.Now()}
	rows, _, err := stores.Repos.ListTrackedSystem(context.Background(), runmode.LocalDefaultOrgID, marksHost, db.Unwindowed)
	if err != nil {
		t.Fatalf("list tracked: %v", err)
	}
	for _, row := range rows {
		r.repoID[row.Slug()] = row.ID
	}
	return r
}

// tracker is a fresh tracker over the rig's stores, its change window on the
// rig's clock and honoring marks.
func (r *marksRig) tracker(marks ...domain.GitHubRefreshMark) *Tracker {
	s := r.stores
	return New(r.database, r.pub, s.Tasks, s.Entities, s.Repos, s.JiraProjects, s.Deliverables, s.ExternalActions, s.EventQueue, s.PRMergeability, runmode.LocalDefaultOrgID).
		WithChangeClock(func() time.Time { return r.now }).
		WithRefreshMarks(marks)
}

// scheduled runs a scheduled pass over every tracked repository and returns
// the event types it emitted.
func (r *marksRig) scheduled(marks ...domain.GitHubRefreshMark) []string {
	r.t.Helper()
	before := len(r.pub.events)
	if _, _, err := r.tracker(marks...).RefreshGitHub(context.Background(), marksHost, r.client, domain.GitHubAccount{}, r.repos, ReachAll(), nil); err != nil {
		r.t.Fatalf("RefreshGitHub: %v", err)
	}
	return r.since(before)
}

// narrowed runs a narrowed pass over repos and returns the event types it
// emitted, poll sentinels included.
func (r *marksRig) narrowed(repos []string, marks ...domain.GitHubRefreshMark) []string {
	r.t.Helper()
	before := len(r.pub.events)
	if _, err := r.tracker(marks...).RefreshMarkedGitHub(context.Background(), marksHost, r.client, domain.GitHubAccount{}, repos, nil); err != nil {
		r.t.Fatalf("RefreshMarkedGitHub: %v", err)
	}
	return r.since(before)
}

func (r *marksRig) since(before int) []string {
	r.pub.mu.Lock()
	defer r.pub.mu.Unlock()
	var out []string
	for _, e := range r.pub.events[before:] {
		out = append(out, e.EventType)
	}
	return out
}

// mark is a mark on repo for target, as the store would return it.
func (r *marksRig) mark(repo string, target domain.GitHubRefreshTarget) domain.GitHubRefreshMark {
	return domain.GitHubRefreshMark{ID: repo + "/" + target.Value, RepositoryID: r.repoID[repo], Target: target, Revision: 1}
}

// settle advances the rig's clock past the change window and stamps every
// entity as just polled, so the gate's timers keep every pull request's
// stored snapshot.
func (r *marksRig) settle() {
	r.t.Helper()
	r.now = r.now.Add(changeRefreshWindow + time.Minute)
	entities, err := r.stores.Entities.ListActiveSystem(context.Background(), runmode.LocalDefaultOrgID, "github")
	if err != nil {
		r.t.Fatalf("list entities: %v", err)
	}
	for _, e := range entities {
		if err := r.stores.Entities.MarkPolledSystem(context.Background(), runmode.LocalDefaultOrgID, e.ID); err != nil {
			r.t.Fatalf("stamp polled: %v", err)
		}
	}
}

func passedCheck(id int) string { return completedCheck(id, "build", "SUCCESS") }

// A scheduled pass refreshes a pull request a mark names even when the gate
// would keep its stored snapshot, and only that one: a CI re-run on an
// unchanged head is seen at once instead of at the floor.
func TestRefreshGitHub_MarkedPullRequestIsRefreshedWhateverTheGate(t *testing.T) {
	a1 := &marksPR{repo: "octo/a", number: 1, head: "aaa1", updatedAt: "2026-10-30T12:00:00Z", open: true, checks: []string{passedCheck(1)}}
	a2 := &marksPR{repo: "octo/a", number: 2, head: "aaa2", updatedAt: "2026-10-30T12:00:00Z", open: true, checks: []string{passedCheck(2)}}
	r := newMarksRig(t, []string{"octo/a"}, a1, a2)
	r.scheduled()
	r.settle()
	r.srv.take()

	// Both re-run CI on their unchanged heads; neither listing entry moves.
	r.srv.set(func() {
		a1.checks = []string{completedCheck(11, "build", "FAILURE")}
		a2.checks = []string{completedCheck(12, "build", "FAILURE")}
	})
	got := r.scheduled(r.mark("octo/a", domain.PullRequestRefreshTarget(1)))
	_, _, refreshed := r.srv.take()
	if !slices.Equal(refreshed, []string{a1.nodeID()}) {
		t.Fatalf("refreshed %v; want only the marked pull request %s", refreshed, a1.nodeID())
	}
	if countType(got, domain.EventGitHubPRCICheckFailed) != 1 {
		t.Errorf("emitted %v; want one ci_check_failed, for the marked pull request", got)
	}

	// A head SHA mark names the pull request whose stored head it is.
	got = r.scheduled(r.mark("octo/a", domain.HeadSHARefreshTarget("aaa2")))
	if _, _, refreshed := r.srv.take(); !slices.Equal(refreshed, []string{a2.nodeID()}) {
		t.Fatalf("refreshed %v; want %s, whose stored head the SHA mark names", refreshed, a2.nodeID())
	}
	if countType(got, domain.EventGitHubPRCICheckFailed) != 1 {
		t.Errorf("emitted %v; want one ci_check_failed", got)
	}
}

// A narrowed pass lists only the repositories it is handed, refreshes the
// pull requests its marks name and the ones its listings show changed, leaves
// the timer rules to the scheduled pass, and reports no completed poll.
func TestRefreshMarkedGitHub_RefreshesOnlyWhatTheMarksAndListingsName(t *testing.T) {
	a1 := &marksPR{repo: "octo/a", number: 1, head: "aaa1", updatedAt: "2026-10-30T12:00:00Z", open: true, checks: []string{passedCheck(1)}}
	a2 := &marksPR{repo: "octo/a", number: 2, head: "aaa2", updatedAt: "2026-10-30T12:00:00Z", open: true, checks: []string{passedCheck(2)}}
	a3 := &marksPR{repo: "octo/a", number: 3, head: "aaa3", updatedAt: "2026-10-30T12:00:00Z", open: true, checks: []string{passedCheck(3)}}
	b1 := &marksPR{repo: "octo/b", number: 1, head: "bbb1", updatedAt: "2026-10-30T12:00:00Z", open: true, checks: []string{passedCheck(4)}}
	r := newMarksRig(t, []string{"octo/a", "octo/b"}, a1, a2, a3, b1)
	r.scheduled()
	r.settle()
	r.srv.take()

	r.srv.set(func() {
		// a1: CI re-run, named by a mark. a2: pushed, which its listing
		// shows. a3: nothing. b1: pushed, in a repository no mark names.
		a1.checks = []string{completedCheck(11, "build", "FAILURE")}
		a2.head, a2.updatedAt = "aaa2-new", "2026-10-30T13:00:00Z"
		b1.head, b1.updatedAt = "bbb1-new", "2026-10-30T13:00:00Z"
	})
	// a3's last refresh is past the floor, which a scheduled pass would
	// refresh it for.
	r.agePolled(t, a3, staleRefreshFloor+time.Minute)

	got := r.narrowed([]string{"octo/a"}, r.mark("octo/a", domain.PullRequestRefreshTarget(1)))
	listings, _, refreshed := r.srv.take()
	if listings["octo/b"] != 0 || listings["octo/a"] != 1 {
		t.Errorf("listings = %v; want one of octo/a and none of octo/b", listings)
	}
	slices.Sort(refreshed)
	if want := []string{a1.nodeID(), a2.nodeID()}; !slices.Equal(refreshed, want) {
		t.Errorf("refreshed %v; want %v: the marked one and the one whose listing changed, not the one only the floor names nor octo/b's", refreshed, want)
	}
	if countType(got, domain.EventGitHubPRCICheckFailed) != 1 || countType(got, domain.EventGitHubPRNewCommits) != 1 {
		t.Errorf("emitted %v; want a ci_check_failed for #1 and a new_commits for #2", got)
	}
	if countType(got, domain.EventSystemPollCompleted) != 0 {
		t.Errorf("emitted %v; a narrowed pass reports no completed poll", got)
	}

	// The scheduled pass that follows finds octo/b's push, and nothing it
	// emits repeats the narrowed pass's events.
	got = r.scheduled()
	if countType(got, domain.EventGitHubPRNewCommits) != 1 || countType(got, domain.EventGitHubPRCICheckFailed) != 0 {
		t.Errorf("the next scheduled pass emitted %v; want only octo/b's new_commits", got)
	}
}

// agePolled moves pr's entity's last refresh d into the past.
func (r *marksRig) agePolled(t *testing.T, pr *marksPR, d time.Duration) {
	t.Helper()
	e, err := r.stores.Entities.GetBySource(context.Background(), runmode.LocalDefaultOrgID, "github", marksHost, fmt.Sprintf("%s#%d", pr.repo, pr.number))
	if err != nil || e == nil {
		t.Fatalf("entity for %s#%d: %v, %v", pr.repo, pr.number, e, err)
	}
	if _, err := r.database.Exec(`UPDATE entities SET last_polled_at = ? WHERE id = ?`, time.Now().Add(-d), e.ID); err != nil {
		t.Fatalf("age last_polled_at: %v", err)
	}
}

// A pull request that leaves the open listing of a repository the narrowed
// pass lists is refreshed there: it merged.
func TestRefreshMarkedGitHub_RefreshesAPullRequestThatLeftTheListing(t *testing.T) {
	a1 := &marksPR{repo: "octo/a", number: 1, head: "aaa1", updatedAt: "2026-10-30T12:00:00Z", open: true, checks: []string{passedCheck(1)}}
	a2 := &marksPR{repo: "octo/a", number: 2, head: "aaa2", updatedAt: "2026-10-30T12:00:00Z", open: true, checks: []string{passedCheck(2)}}
	r := newMarksRig(t, []string{"octo/a"}, a1, a2)
	r.scheduled()
	r.settle()
	r.srv.take()

	r.srv.set(func() { a2.open = false })
	got := r.narrowed([]string{"octo/a"}, r.mark("octo/a", domain.BranchRefreshTarget("main")))
	if _, _, refreshed := r.srv.take(); !slices.Equal(refreshed, []string{a2.nodeID()}) {
		t.Errorf("refreshed %v; want %s, which left the listing", refreshed, a2.nodeID())
	}
	if countType(got, domain.EventGitHubPRMerged) != 1 {
		t.Errorf("emitted %v; want one merged", got)
	}
}

// A narrowed pass checks a base branch a mark names even when its listing
// answered 304 and the branch was checked moments ago, and checks no other.
func TestRefreshMarkedGitHub_ChecksTheMarkedBaseBranch(t *testing.T) {
	a1 := &marksPR{repo: "octo/a", number: 1, head: "aaa1", updatedAt: "2026-10-30T12:00:00Z", open: true, checks: []string{passedCheck(1)}}
	r := newMarksRig(t, []string{"octo/a"}, a1)
	r.scheduled()
	r.settle()
	if _, refs, _ := r.srv.take(); refs["octo/a@main"] != 1 {
		t.Fatalf("the first pass read refs %v; want main read once", refs)
	}

	r.narrowed([]string{"octo/a"})
	if listings, refs, _ := r.srv.take(); listings["octo/a"] != 1 || len(refs) != 0 {
		t.Fatalf("an unmarked narrowed pass over a quiet repository: listings %v, refs %v; want one 304 listing and no ref read", listings, refs)
	}
	r.narrowed([]string{"octo/a"}, r.mark("octo/a", domain.BranchRefreshTarget("main")))
	if _, refs, _ := r.srv.take(); refs["octo/a@main"] != 1 {
		t.Errorf("the marked narrowed pass read refs %v; want main read once", refs)
	}
}

// The missed-hint counter counts a scheduled refresh a timer forced that
// emitted an event, labeled by the rule, and not one a mark named.
func TestRefreshGitHub_CountsTimerRefreshesThatFoundAChange(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	prev := stats.Load()
	stats.Store(newInstruments(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))
	t.Cleanup(func() { stats.Store(prev) })

	a1 := &marksPR{repo: "octo/a", number: 1, head: "aaa1", updatedAt: "2026-10-30T12:00:00Z", open: true, checks: []string{passedCheck(1)}}
	a2 := &marksPR{repo: "octo/a", number: 2, head: "aaa2", updatedAt: "2026-10-30T12:00:00Z", open: true, checks: []string{passedCheck(2)}}
	r := newMarksRig(t, []string{"octo/a"}, a1, a2)
	r.scheduled()
	r.settle()
	r.srv.set(func() {
		a1.checks = []string{completedCheck(11, "build", "FAILURE")}
		a2.checks = []string{completedCheck(12, "build", "FAILURE")}
	})
	r.agePolled(t, a1, staleRefreshFloor+time.Minute)
	r.agePolled(t, a2, staleRefreshFloor+time.Minute)
	r.scheduled(r.mark("octo/a", domain.PullRequestRefreshTarget(2)))

	got := missedHintsByRule(t, reader)
	if len(got) != 1 || got[string(refreshFloor)] != 1 {
		t.Errorf("missed hints = %v; want one under rule=floor, for the unmarked pull request", got)
	}
}

// A narrowed pass whose review and comment reads a rate limit cut short
// reports the limit, after committing what it did read, so the poller keeps
// the marks for a pass after the reset. A scheduled pass reports none (see
// TestRefreshGitHub_RateLimitStopsTheReads).
func TestRefreshMarkedGitHub_ReportsARateLimitThatCutTheReadsShort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/repos/octo/a/pulls") {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "nodes(ids:"):
			_, _ = w.Write([]byte(`{"data":{"nodes":[{"id":"PR_node7","number":7,"title":"PR 7","author":{"__typename":"User","login":"aidan"},
				"state":"OPEN","url":"https://github.com/octo/a/pull/7","repository":{"nameWithOwner":"octo/a"},
				"commits":{"nodes":[{"commit":{"oid":"sha7","statusCheckRollup":null}}]},
				"createdAt":"2026-10-01T00:00:00Z","updatedAt":"2026-10-01T12:00:00Z",
				"comments":{"totalCount":2,"nodes":[{"fullDatabaseId":"701","createdAt":"2026-10-01T10:00:00Z"}]}}]}}`))
		case strings.Contains(req.Query, "comments(last: 50"):
			w.Header().Set("Retry-After", "600")
			http.Error(w, `{"message":"You have exceeded a secondary rate limit"}`, http.StatusTooManyRequests)
		default:
			t.Errorf("unexpected request: %s %s %.60q", r.Method, r.URL.Path, req.Query)
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	trackRepos(t, stores, org, []string{"octo/a"})
	e, _, err := stores.Entities.FindOrCreateSystem(ctx, org, "github", marksHost, "octo/a#7", "", "pr", "PR 7", "https://github.com/octo/a/pull/7")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	prev := domain.PRSnapshot{NodeID: "PR_node7", Number: 7, Repo: "octo/a", State: "OPEN", Author: "aidan", HeadSHA: "sha7", Title: "PR 7",
		UpdatedAt: "2026-10-01T08:00:00Z", CheckRuns: []domain.CheckRun{},
		CommentCount: 1, CommentMarker: &domain.CommentMarker{ID: 700, CreatedAt: "2026-10-01T09:00:00Z"}}
	snapJSON, _ := json.Marshal(prev)
	if ok, err := stores.Entities.UpdateSnapshotCASSystem(ctx, org, e.ID, string(snapJSON), e.PollSeq); err != nil || !ok {
		t.Fatalf("seed snapshot: ok=%v err=%v", ok, err)
	}

	tr := New(database, &recordingPublisher{}, stores.Tasks, stores.Entities, stores.Repos, stores.JiraProjects, stores.Deliverables, stores.ExternalActions, stores.EventQueue, stores.PRMergeability, org).
		WithRefreshMarks([]domain.GitHubRefreshMark{{ID: "m", RepositoryID: e.RepositoryID, Target: domain.PullRequestRefreshTarget(7), Revision: 1}})
	_, err = tr.RefreshMarkedGitHub(ctx, marksHost, ghclient.NewClient(srv.URL, "tok"), domain.GitHubAccount{}, []string{"octo/a"}, nil)
	var rl *ghclient.ErrRateLimited
	if !errors.As(err, &rl) {
		t.Fatalf("RefreshMarkedGitHub = %v; want the rate limit that cut the comment read short", err)
	}
}
