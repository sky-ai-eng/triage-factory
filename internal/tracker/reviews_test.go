package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

const reviewTestHost = "https://github.com"

// fakeReviewReader serves pages of a pull request's submitted reviews keyed by
// the cursor that reads them ("" for the last page), and records the cursors
// it was asked for.
type fakeReviewReader struct {
	pages map[string]ghclient.PRReviewPage
	err   error
	asked []string
}

func (f *fakeReviewReader) PRReviewsBefore(_ context.Context, _ string, before string) (ghclient.PRReviewPage, error) {
	f.asked = append(f.asked, before)
	if f.err != nil {
		return ghclient.PRReviewPage{}, f.err
	}
	return f.pages[before], nil
}

func newReviewTestTracker(t *testing.T) (*Tracker, db.Stores) {
	t.Helper()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	return New(database, &recordingPublisher{}, stores.Tasks, stores.Entities, stores.Repos, stores.Artifacts, stores.EventQueue, runmode.LocalDefaultOrgID), stores
}

func review(id int64, author, state, submittedAt string, comments ...ghclient.PRReviewComment) ghclient.PRReview {
	return ghclient.PRReview{ID: id, Author: author, State: state, SubmittedAt: submittedAt, CommentCount: len(comments), Comments: comments}
}

// prevWithMarker is a stored snapshot whose newest review is id 100, submitted
// at 09:00, among count submitted reviews.
func prevWithMarker(count int) domain.PRSnapshot {
	return domain.PRSnapshot{Number: 7, Repo: "octo/repo", ReviewCount: count,
		ReviewMarker: &domain.ReviewMarker{ID: 100, SubmittedAt: "2026-10-01T09:00:00Z"}}
}

func currWithMarker(count int, id int64, submittedAt string) domain.PRSnapshot {
	return domain.PRSnapshot{Number: 7, Repo: "octo/repo", ReviewCount: count,
		ReviewMarker: &domain.ReviewMarker{ID: id, SubmittedAt: submittedAt}}
}

func reviewIDs(reviews []NewReview) []int64 {
	var ids []int64
	for _, r := range reviews {
		ids = append(ids, r.ID)
	}
	return ids
}

// A snapshot stored without the marker reads nothing; curr's marker becomes
// the baseline.
func TestGitHubNewReviews_NoMarkerReadsNothing(t *testing.T) {
	tr, _ := newReviewTestTracker(t)
	reader := &fakeReviewReader{}
	prev := domain.PRSnapshot{Number: 7}
	curr := currWithMarker(3, 300, "2026-10-01T10:00:00Z")

	if got, _ := tr.githubNewReviews(context.Background(), reader, reviewTestHost, "PR_7", prev, &curr); got != nil {
		t.Errorf("reviews = %+v, want none", got)
	}
	if len(reader.asked) != 0 {
		t.Errorf("read %d pages, want none", len(reader.asked))
	}
	if curr.ReviewCount != 3 || curr.ReviewMarker.ID != 300 {
		t.Errorf("curr = count %d marker %+v, want the batch's", curr.ReviewCount, curr.ReviewMarker)
	}
}

// A pull request whose review count and newest review did not move is not
// read. A newest review older than the marker (the newest was deleted) keeps
// the marker, so the reviews between the two are never new again.
func TestGitHubNewReviews_UnchangedReadsNothing(t *testing.T) {
	tr, _ := newReviewTestTracker(t)
	reader := &fakeReviewReader{}
	prev := prevWithMarker(2)
	curr := currWithMarker(2, 90, "2026-10-01T08:00:00Z")

	if got, _ := tr.githubNewReviews(context.Background(), reader, reviewTestHost, "PR_7", prev, &curr); got != nil || len(reader.asked) != 0 {
		t.Errorf("reviews = %+v after %d reads, want none and no read", got, len(reader.asked))
	}
	if *curr.ReviewMarker != *prev.ReviewMarker {
		t.Errorf("marker = %+v, want prev's %+v kept", curr.ReviewMarker, prev.ReviewMarker)
	}
}

// New reviews at the end of the list are read from one page, which reaches an
// old review; they come back oldest first, and the marker and count move to
// what the read saw.
func TestGitHubNewReviews_NewReviewsAtTheEnd(t *testing.T) {
	tr, _ := newReviewTestTracker(t)
	reader := &fakeReviewReader{pages: map[string]ghclient.PRReviewPage{
		"": {TotalCount: 4, HasPrevious: true, Before: "CUR", Reviews: []ghclient.PRReview{
			review(100, "alice", "COMMENTED", "2026-10-01T09:00:00Z"),
			review(101, "alice", "COMMENTED", "2026-10-01T10:00:00Z", ghclient.PRReviewComment{ID: 501, Path: "a.go"}, ghclient.PRReviewComment{ID: 502, Path: "a.go"}),
			review(102, "bob", "APPROVED", "2026-10-01T10:05:00Z"),
		}},
	}}
	prev := prevWithMarker(2)
	curr := currWithMarker(3, 101, "2026-10-01T10:00:00Z")

	got, _ := tr.githubNewReviews(context.Background(), reader, reviewTestHost, "PR_7", prev, &curr)
	if ids := reviewIDs(got); !slices.Equal(ids, []int64{101, 102}) {
		t.Fatalf("new reviews = %v, want [101 102]", ids)
	}
	if !slices.Equal(got[0].Paths, []string{"a.go"}) || got[0].CommentCount != 2 {
		t.Errorf("review 101 = %+v, want 2 comments on a.go", got[0])
	}
	if !slices.Equal(reader.asked, []string{""}) {
		t.Errorf("cursors read = %q, want the last page alone", reader.asked)
	}
	// The read saw a review the batch had not (102), and four reviews in all.
	if curr.ReviewCount != 4 || *curr.ReviewMarker != (domain.ReviewMarker{ID: 102, SubmittedAt: "2026-10-01T10:05:00Z"}) {
		t.Errorf("curr = count %d marker %+v, want 4 and review 102", curr.ReviewCount, curr.ReviewMarker)
	}
}

// A review started as pending before the last poll and submitted since sits
// where it was started if GitHub lists reviews by creation. The count says one
// review is new, so the read goes back past the old reviews at the end until
// it finds it.
func TestGitHubNewReviews_LateSubmittedPendingReview(t *testing.T) {
	tr, _ := newReviewTestTracker(t)
	reader := &fakeReviewReader{pages: map[string]ghclient.PRReviewPage{
		"": {TotalCount: 3, HasPrevious: true, Before: "CUR_2", Reviews: []ghclient.PRReview{
			review(100, "bob", "COMMENTED", "2026-10-01T09:00:00Z"),
		}},
		"CUR_2": {TotalCount: 3, HasPrevious: true, Before: "CUR_1", Reviews: []ghclient.PRReview{
			review(95, "carol", "CHANGES_REQUESTED", "2026-10-01T11:00:00Z", ghclient.PRReviewComment{ID: 601, Path: "b.go"}),
		}},
		"CUR_1": {TotalCount: 3, Reviews: []ghclient.PRReview{
			review(90, "alice", "APPROVED", "2026-10-01T08:00:00Z"),
		}},
	}}
	prev := prevWithMarker(2)
	curr := currWithMarker(3, 95, "2026-10-01T11:00:00Z")

	got, _ := tr.githubNewReviews(context.Background(), reader, reviewTestHost, "PR_7", prev, &curr)
	if ids := reviewIDs(got); !slices.Equal(ids, []int64{95}) {
		t.Fatalf("new reviews = %v, want [95]", ids)
	}
	if !slices.Equal(reader.asked, []string{"", "CUR_2"}) {
		t.Errorf("cursors read = %q, want the read to stop once the count is made up", reader.asked)
	}
	if curr.ReviewMarker.ID != 95 {
		t.Errorf("marker = %+v, want review 95", curr.ReviewMarker)
	}
}

// A review TF submitted, matched on its review artifact's external id, and a
// thread reply TF posted, matched on its comment's artifact, are TF's. A
// person's reply, and a review with one of TF's comments among a person's,
// are not.
func TestGitHubNewReviews_MarksTFReviewsAndReplies(t *testing.T) {
	tr, stores := newReviewTestTracker(t)
	ctx := context.Background()
	org, team := runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID

	submitted := domain.NewReviewArtifact("octo/repo", 7, "sha", "conv-1")
	submitted.TeamID, submitted.Scope, submitted.State, submitted.ExternalID = team, reviewTestHost, domain.ArtifactStateReviewSubmitted, "101"
	if _, err := stores.Artifacts.UpsertSystem(ctx, org, submitted); err != nil {
		t.Fatalf("seed review artifact: %v", err)
	}
	for _, id := range []int64{701, 801} {
		reply := domain.Artifact{TeamID: team, Provider: domain.ArtifactProviderGitHub, Kind: domain.ArtifactKindComment,
			Scope: reviewTestHost, Target: "octo/repo#7", State: domain.ArtifactStateCommentPosted,
			DedupKey: domain.GitHubCommentDedupKey(domain.GitHubReviewComment, id)}
		if _, err := stores.Artifacts.UpsertSystem(ctx, org, reply); err != nil {
			t.Fatalf("seed comment artifact %d: %v", id, err)
		}
	}

	reader := &fakeReviewReader{pages: map[string]ghclient.PRReviewPage{
		"": {TotalCount: 6, Reviews: []ghclient.PRReview{
			review(100, "bob", "COMMENTED", "2026-10-01T09:00:00Z"),
			review(101, "aidan", "APPROVED", "2026-10-01T10:00:00Z"),
			review(102, "aidan", "COMMENTED", "2026-10-01T10:01:00Z", ghclient.PRReviewComment{ID: 701, Path: "a.go"}),
			review(103, "bob", "COMMENTED", "2026-10-01T10:02:00Z", ghclient.PRReviewComment{ID: 702, Path: "a.go"}),
			review(104, "aidan", "COMMENTED", "2026-10-01T10:03:00Z", ghclient.PRReviewComment{ID: 801, Path: "a.go"}, ghclient.PRReviewComment{ID: 802, Path: "b.go"}),
		}},
	}}
	prev := prevWithMarker(2)
	curr := currWithMarker(6, 104, "2026-10-01T10:03:00Z")

	got, _ := tr.githubNewReviews(ctx, reader, reviewTestHost, "PR_7", prev, &curr)
	byTF := map[int64]bool{}
	for _, r := range got {
		byTF[r.ID] = r.ByTF
	}
	want := map[int64]bool{101: true, 102: true, 103: false, 104: false}
	for id, w := range want {
		if b, ok := byTF[id]; !ok || b != w {
			t.Errorf("review %d: ByTF = %v (read: %v), want %v", id, b, ok, w)
		}
	}
}

// failingReviewArtifacts fails the review-id lookup.
type failingReviewArtifacts struct{ db.ArtifactStore }

func (failingReviewArtifacts) PresentExternalIDsSystem(context.Context, string, string, string, string, []string) (map[string]bool, error) {
	return nil, errors.New("artifact store down")
}

// A failed read, of the reviews or of which are TF's, keeps prev's marker and
// count, so the next poll finds the same reviews new again.
func TestGitHubNewReviews_FailedReadKeepsPrevMarker(t *testing.T) {
	pages := map[string]ghclient.PRReviewPage{"": {TotalCount: 3, Reviews: []ghclient.PRReview{
		review(101, "alice", "COMMENTED", "2026-10-01T10:00:00Z"),
	}}}
	for name, setup := range map[string]func(tr *Tracker, r *fakeReviewReader){
		"review read":    func(_ *Tracker, r *fakeReviewReader) { r.err = errors.New("graphql down") },
		"tf attribution": func(tr *Tracker, _ *fakeReviewReader) { tr.artifacts = failingReviewArtifacts{tr.artifacts} },
	} {
		t.Run(name, func(t *testing.T) {
			tr, _ := newReviewTestTracker(t)
			reader := &fakeReviewReader{pages: pages}
			setup(tr, reader)
			prev := prevWithMarker(2)
			curr := currWithMarker(3, 101, "2026-10-01T10:00:00Z")

			if got, err := tr.githubNewReviews(context.Background(), reader, reviewTestHost, "PR_7", prev, &curr); got != nil || err != nil {
				t.Errorf("reviews = %+v, err = %v; want none, and no error for a failure other than a rate limit", got, err)
			}
			if curr.ReviewCount != 2 || *curr.ReviewMarker != *prev.ReviewMarker {
				t.Errorf("curr = count %d marker %+v, want prev's 2 and %+v", curr.ReviewCount, curr.ReviewMarker, prev.ReviewMarker)
			}
		})
	}
}

// reviewRefreshServer answers the batch refresh with a pull request whose
// reviews are batch (the latestReviews and reviews members) and the review
// read with page. It counts the review reads.
func reviewRefreshServer(t *testing.T, batch, page string, reads *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		switch {
		case !strings.HasSuffix(r.URL.Path, "/graphql"):
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		case strings.Contains(req.Query, "nodes(ids:"):
			_, _ = w.Write([]byte(`{"data":{"nodes":[
				{"id":"PR_node7","number":7,"title":"Reviewed PR","author":{"login":"aidan"},
				 "state":"OPEN","url":"https://github.com/octo/repo/pull/7","repository":{"nameWithOwner":"octo/repo"},
				 "commits":{"nodes":[{"commit":{"oid":"sha7","statusCheckRollup":null}}]},
				 "createdAt":"2026-10-01T00:00:00Z","updatedAt":"2026-10-01T12:00:00Z",` + batch + `}
			]}}`))
		default:
			*reads++
			_, _ = w.Write([]byte(`{"data":{"node":{"reviews":` + page + `}}}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// seedReviewedPR stores the open pull request octo/repo#7 with prev as its
// snapshot.
func seedReviewedPR(t *testing.T, stores db.Stores, prev domain.PRSnapshot) string {
	t.Helper()
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	e, _, err := stores.Entities.FindOrCreateSystem(ctx, org, "github", reviewTestHost, "octo/repo#7", "", "pr", "Reviewed PR", "https://github.com/octo/repo/pull/7")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	prev.NodeID, prev.Number, prev.Repo, prev.State, prev.Author, prev.HeadSHA = "PR_node7", 7, "octo/repo", "OPEN", "aidan", "sha7"
	prev.Title, prev.UpdatedAt, prev.CheckRuns = "Reviewed PR", "2026-10-01T08:00:00Z", []domain.CheckRun{}
	snapJSON, _ := json.Marshal(prev)
	if ok, err := stores.Entities.UpdateSnapshotCASSystem(ctx, org, e.ID, string(snapJSON), e.PollSeq); err != nil || !ok {
		t.Fatalf("seed snapshot: ok=%v err=%v", ok, err)
	}
	return e.ID
}

func storedPRSnapshot(t *testing.T, stores db.Stores, entityID string) domain.PRSnapshot {
	t.Helper()
	e, err := stores.Entities.GetSystem(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || e == nil {
		t.Fatalf("read entity: %v", err)
	}
	var snap domain.PRSnapshot
	if err := json.Unmarshal([]byte(e.SnapshotJSON), &snap); err != nil {
		t.Fatalf("parse snapshot: %v", err)
	}
	return snap
}

func reviewEvents(evts []domain.Event) []domain.Event {
	var out []domain.Event
	for _, e := range evts {
		if strings.HasPrefix(e.EventType, "github:pr:review_") {
			out = append(out, e)
		}
	}
	return out
}

// TestRefreshGitHub_EmitsOneEventPerReview runs a poll end to end against a
// stored snapshot with the marker: a reviewer's second COMMENTED review and a
// thread reply each emit, carrying their id, comment count and files; a
// thread reply TF posted emits nothing; the marker moves to the newest review,
// and the next poll reads and emits nothing.
func TestRefreshGitHub_EmitsOneEventPerReview(t *testing.T) {
	const batch = `
		"latestReviews":{"nodes":[
			{"id":"PRR_alice","fullDatabaseId":"102","author":{"__typename":"User","login":"alice"},"state":"COMMENTED","submittedAt":"2026-10-01T10:05:00Z","comments":{"totalCount":1}},
			{"id":"PRR_aidan","fullDatabaseId":"103","author":{"__typename":"User","login":"aidan"},"state":"COMMENTED","submittedAt":"2026-10-01T10:10:00Z","comments":{"totalCount":1}}
		]},
		"reviews":{"totalCount":4,"nodes":[{"fullDatabaseId":"103","submittedAt":"2026-10-01T10:10:00Z"}]}`
	const page = `{"totalCount":4,"pageInfo":{"hasPreviousPage":false,"startCursor":"CUR"},"nodes":[
		{"fullDatabaseId":"100","state":"COMMENTED","submittedAt":"2026-10-01T09:00:00Z","author":{"__typename":"User","login":"alice"},"comments":{"totalCount":0,"nodes":[]}},
		{"fullDatabaseId":"101","state":"COMMENTED","submittedAt":"2026-10-01T10:00:00Z","author":{"__typename":"User","login":"alice"},"comments":{"totalCount":2,"nodes":[{"fullDatabaseId":"4001","path":"a.go"},{"fullDatabaseId":"4002","path":"b.go"}]}},
		{"fullDatabaseId":"102","state":"COMMENTED","submittedAt":"2026-10-01T10:05:00Z","author":{"__typename":"User","login":"alice"},"comments":{"totalCount":1,"nodes":[{"fullDatabaseId":"4003","path":"a.go"}]}},
		{"fullDatabaseId":"103","state":"COMMENTED","submittedAt":"2026-10-01T10:10:00Z","author":{"__typename":"User","login":"aidan"},"comments":{"totalCount":1,"nodes":[{"fullDatabaseId":"4004","path":"a.go"}]}}
	]}`
	reads := 0
	srv := reviewRefreshServer(t, batch, page, &reads)

	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	reply := domain.Artifact{TeamID: runmode.LocalDefaultTeamID, Provider: domain.ArtifactProviderGitHub, Kind: domain.ArtifactKindComment,
		Scope: reviewTestHost, Target: "octo/repo#7", State: domain.ArtifactStateCommentPosted,
		DedupKey: domain.GitHubCommentDedupKey(domain.GitHubReviewComment, 4004)}
	if _, err := stores.Artifacts.UpsertSystem(ctx, org, reply); err != nil {
		t.Fatalf("seed TF's reply: %v", err)
	}
	entityID := seedReviewedPR(t, stores, domain.PRSnapshot{
		ReviewCount:  1,
		ReviewMarker: &domain.ReviewMarker{ID: 100, SubmittedAt: "2026-10-01T09:00:00Z"},
		Reviews:      []domain.ReviewState{{ID: "PRR_alice", DatabaseID: 100, Author: "alice", State: "COMMENTED", SubmittedAt: "2026-10-01T09:00:00Z"}},
	})

	pub := &recordingPublisher{}
	tr := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, stores.Artifacts, stores.EventQueue, org)
	client := ghclient.NewClient(srv.URL, "tok")
	if _, _, err := tr.RefreshGitHub(ctx, reviewTestHost, client, "", nil, nil); err != nil {
		t.Fatalf("RefreshGitHub: %v", err)
	}

	got := reviewEvents(pub.nonSystemEvents())
	if len(got) != 2 {
		t.Fatalf("review events = %v, want alice's two reviews", eventTypes(got))
	}
	first := decodeMetadata[events.GitHubPRReviewCommentedMetadata](t, got[0])
	second := decodeMetadata[events.GitHubPRReviewCommentedMetadata](t, got[1])
	if first.ReviewID != 101 || first.CommentCount != 2 || !slices.Equal(first.Paths, []string{"a.go", "b.go"}) {
		t.Errorf("first review = %+v, want 101 with 2 comments on a.go and b.go", first)
	}
	if second.ReviewID != 102 || second.CommentCount != 1 || !slices.Equal(second.Paths, []string{"a.go"}) {
		t.Errorf("second review (a thread reply) = %+v, want 102 with 1 comment on a.go", second)
	}
	snap := storedPRSnapshot(t, stores, entityID)
	if snap.ReviewCount != 4 || snap.ReviewMarker == nil || snap.ReviewMarker.ID != 103 {
		t.Errorf("stored snapshot: count %d marker %+v, want 4 and review 103", snap.ReviewCount, snap.ReviewMarker)
	}

	// The same pull request on the next poll: nothing moved, so nothing is
	// read and nothing emits.
	before := len(pub.nonSystemEvents())
	if _, _, err := tr.RefreshGitHub(ctx, reviewTestHost, client, "", nil, nil); err != nil {
		t.Fatalf("second RefreshGitHub: %v", err)
	}
	if reads != 1 {
		t.Errorf("review reads = %d, want 1 (none on the quiet poll)", reads)
	}
	if after := pub.nonSystemEvents(); len(after) != before {
		t.Errorf("quiet poll emitted %v", eventTypes(after[before:]))
	}
}

// TestRefreshGitHub_SnapshotWithoutMarker pins the first poll of a pull
// request whose stored snapshot predates the marker: it emits what comparing
// reviewer states always did, reads no reviews, and stores the marker the
// next poll reads from.
func TestRefreshGitHub_SnapshotWithoutMarker(t *testing.T) {
	const batch = `
		"latestReviews":{"nodes":[
			{"id":"PRR_alice","fullDatabaseId":"102","author":{"__typename":"User","login":"alice"},"state":"APPROVED","submittedAt":"2026-10-01T10:05:00Z","comments":{"totalCount":0}}
		]},
		"reviews":{"totalCount":3,"nodes":[{"fullDatabaseId":"102","submittedAt":"2026-10-01T10:05:00Z"}]}`
	reads := 0
	srv := reviewRefreshServer(t, batch, `{}`, &reads)

	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	entityID := seedReviewedPR(t, stores, domain.PRSnapshot{
		ReviewCount: 1,
		Reviews:     []domain.ReviewState{{ID: "PRR_alice0", Author: "alice", State: "COMMENTED", SubmittedAt: "2026-10-01T09:00:00Z"}},
	})

	pub := &recordingPublisher{}
	tr := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, stores.Artifacts, stores.EventQueue, org)
	if _, _, err := tr.RefreshGitHub(ctx, reviewTestHost, ghclient.NewClient(srv.URL, "tok"), "", nil, nil); err != nil {
		t.Fatalf("RefreshGitHub: %v", err)
	}

	got := reviewEvents(pub.nonSystemEvents())
	if len(got) != 1 || got[0].EventType != domain.EventGitHubPRReviewApproved {
		t.Fatalf("review events = %v, want [review_approved]", eventTypes(got))
	}
	if meta := decodeMetadata[events.GitHubPRReviewApprovedMetadata](t, got[0]); meta.Reviewer != "alice" || meta.ReviewID != 102 {
		t.Errorf("metadata = %+v, want alice's review 102", meta)
	}
	if reads != 0 {
		t.Errorf("review reads = %d, want none", reads)
	}
	if snap := storedPRSnapshot(t, stores, entityID); snap.ReviewMarker == nil || snap.ReviewMarker.ID != 102 || snap.ReviewCount != 3 {
		t.Errorf("stored snapshot: count %d marker %+v, want 3 and review 102", snap.ReviewCount, snap.ReviewMarker)
	}
}
