package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// fakeCommentReader serves pages of a pull request's conversation comments
// keyed by the cursor that reads them ("" for the last page), and records the
// cursors it was asked for.
type fakeCommentReader struct {
	pages map[string]ghclient.PRCommentPage
	err   error
	asked []string
}

func (f *fakeCommentReader) PRCommentsBefore(_ context.Context, _ string, before string) (ghclient.PRCommentPage, error) {
	f.asked = append(f.asked, before)
	if f.err != nil {
		return ghclient.PRCommentPage{}, f.err
	}
	return f.pages[before], nil
}

func ghComment(id int64, login, createdAt string) ghclient.PRComment {
	return ghclient.PRComment{ID: id, Author: login, CreatedAt: createdAt}
}

// prevWithCommentMarker is a stored snapshot whose newest comment is id 100,
// made at 09:00, among count comments.
func prevWithCommentMarker(count int) domain.PRSnapshot {
	return domain.PRSnapshot{Number: 7, Repo: "octo/repo", CommentCount: count,
		CommentMarker: &domain.CommentMarker{ID: 100, CreatedAt: "2026-10-01T09:00:00Z"}}
}

func currWithCommentMarker(count int, id int64, createdAt string) domain.PRSnapshot {
	return domain.PRSnapshot{Number: 7, Repo: "octo/repo", CommentCount: count,
		CommentMarker: &domain.CommentMarker{ID: id, CreatedAt: createdAt}}
}

func commentIDs(comments []NewComment) []string {
	ids := []string{}
	for _, c := range comments {
		ids = append(ids, c.ID)
	}
	return ids
}

// TestDiffPRSnapshots_Commented covers the commented branch of the PR diff:
// comments TF posted are not a reason to emit, a mix is attributed to the
// newest comment a person wrote, a bot's comment counts, and every commenter
// is in the list commenter_in matches against, case-insensitively.
func TestDiffPRSnapshots_Commented(t *testing.T) {
	by := func(login string) func(id, at string) NewComment {
		return func(id, at string) NewComment {
			return NewComment{ID: id, Author: login, AuthorID: login, CreatedAt: at}
		}
	}
	alice, bob, bot := by("alice"), by("bob"), by("dependabot[bot]")
	tf := func(id, at string) NewComment {
		c := by(testUser)(id, at)
		c.ByTF = true
		return c
	}
	cases := []struct {
		name          string
		comments      []NewComment
		wantID        int64 // 0: no event
		wantCommenter string
		wantLogins    []string
		wantAt        string
	}{
		{name: "no new comments"},
		{name: "new comments by TF only",
			comments: []NewComment{tf("1", "2026-10-01T09:00:00Z"), tf("2", "2026-10-01T09:05:00Z")}},
		{name: "a person's comment and TF's after it",
			comments: []NewComment{alice("1", "2026-10-01T09:00:00Z"), tf("2", "2026-10-01T09:05:00Z")},
			wantID:   1, wantCommenter: "alice", wantLogins: []string{"alice"}, wantAt: "2026-10-01T09:00:00Z"},
		{name: "several comments by people",
			comments: []NewComment{
				alice("1", "2026-10-01T09:00:00Z"), bob("2", "2026-10-01T09:01:00Z"),
				tf("3", "2026-10-01T09:02:00Z"), alice("4", "2026-10-01T09:03:00Z"),
			},
			wantID: 4, wantCommenter: "alice", wantLogins: []string{"alice", "bob"}, wantAt: "2026-10-01T09:03:00Z"},
		{name: "a bot's comment",
			comments: []NewComment{bot("5", "2026-10-01T09:00:00Z")},
			wantID:   5, wantCommenter: "dependabot[bot]", wantLogins: []string{"dependabot[bot]"}, wantAt: "2026-10-01T09:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev := basePRSnapshot()
			curr := basePRSnapshot()
			curr.IsDraft, curr.Labels = true, []string{"needs-reply"}
			evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, nil, nil, tc.comments)
			evt := findEvent(evts, domain.EventGitHubPRCommented)
			if tc.wantID == 0 {
				if evt != nil {
					t.Fatalf("emitted commented: %s, want nothing", evt.MetadataJSON)
				}
				return
			}
			if got := findEvents(evts, domain.EventGitHubPRCommented); len(got) != 1 {
				t.Fatalf("commented events = %d, want one per poll", len(got))
			}
			assertEntityID(t, *evt)
			meta := decodeMetadata[events.GitHubPRCommentedMetadata](t, *evt)
			if meta.CommentID != tc.wantID || meta.Commenter != tc.wantCommenter || !slices.Equal(meta.CommenterLogins, tc.wantLogins) {
				t.Errorf("metadata = %+v, want comment %d by %s, logins %v", meta, tc.wantID, tc.wantCommenter, tc.wantLogins)
			}
			if meta.Author != testUser || meta.Repo != "owner/repo" || meta.PRNumber != 42 || !meta.IsDraft ||
				meta.HeadSHA != curr.HeadSHA || !slices.Equal(meta.Labels, []string{"needs-reply"}) {
				t.Errorf("pr fields = %+v, want the current snapshot's", meta)
			}
			if wantAt, _ := time.Parse(time.RFC3339, tc.wantAt); !evt.OccurredAt.Equal(wantAt) {
				t.Errorf("occurred_at = %v, want the newest person's comment at %v", evt.OccurredAt, wantAt)
			}
			for _, login := range tc.wantLogins {
				if !matchesPredicate(t, *evt, allowlist(t, "commenter_in", strings.ToUpper(login))) {
					t.Errorf("commenter_in naming %s did not match", login)
				}
			}
			if matchesPredicate(t, *evt, allowlist(t, "commenter_in", "carol")) {
				t.Error("commenter_in naming someone who did not comment matched")
			}
			if !matchesPredicate(t, *evt, allowlist(t, "author_in", testUser)) || matchesPredicate(t, *evt, allowlist(t, "author_in", "carol")) {
				t.Error("author_in did not match on the PR author alone")
			}
		})
	}
}

// A pull request seen for the first time emits no commented event, whatever
// comments it carries.
func TestDiffPRSnapshots_CommentedNotOnFirstDiscovery(t *testing.T) {
	comments := []NewComment{{ID: "1", Author: "alice", AuthorID: "alice", CreatedAt: "2026-10-01T09:00:00Z"}}
	if evt := findEvent(DiffPRSnapshots(domain.PRSnapshot{}, basePRSnapshot(), testEntityID, testUser, nil, nil, comments), domain.EventGitHubPRCommented); evt != nil {
		t.Errorf("first discovery emitted commented: %s", evt.MetadataJSON)
	}
}

// A snapshot seeded from the REST listing has no marker and no count: the
// comments already on the pull request are its baseline, so nothing is read
// and curr's marker stands.
func TestGitHubNewComments_RESTSeedReadsNothing(t *testing.T) {
	tr, _ := newReviewTestTracker(t)
	reader := &fakeCommentReader{}
	prev := domain.PRSnapshot{Number: 7}
	curr := currWithCommentMarker(5, 300, "2026-10-01T10:00:00Z")

	if got, _ := tr.githubNewComments(context.Background(), reader, reviewTestHost, "PR_7", prev, &curr); got != nil {
		t.Errorf("comments = %+v, want none", got)
	}
	if len(reader.asked) != 0 {
		t.Errorf("read %d pages, want none", len(reader.asked))
	}
	if curr.CommentCount != 5 || curr.CommentMarker.ID != 300 {
		t.Errorf("curr = count %d marker %+v, want the batch's", curr.CommentCount, curr.CommentMarker)
	}
}

// A pull request whose newest comment did not change is not read, and neither
// is one whose newest comment is older than the marker: the marker's comment
// was deleted, and nothing was added. curr keeps what it has now.
func TestGitHubNewComments_UnchangedOrDeletedReadsNothing(t *testing.T) {
	for name, curr := range map[string]domain.PRSnapshot{
		"unchanged":      currWithCommentMarker(2, 100, "2026-10-01T09:00:00Z"),
		"newest deleted": currWithCommentMarker(1, 90, "2026-10-01T08:00:00Z"),
	} {
		t.Run(name, func(t *testing.T) {
			tr, _ := newReviewTestTracker(t)
			reader := &fakeCommentReader{}
			want := *curr.CommentMarker
			if got, _ := tr.githubNewComments(context.Background(), reader, reviewTestHost, "PR_7", prevWithCommentMarker(2), &curr); got != nil || len(reader.asked) != 0 {
				t.Errorf("comments = %+v after %d reads, want none and no read", got, len(reader.asked))
			}
			if *curr.CommentMarker != want {
				t.Errorf("marker = %+v, want curr's %+v", curr.CommentMarker, want)
			}
		})
	}
}

// New comments at the end of the list are read from one page, which reaches
// the marker; they come back oldest first, and the marker and count move to
// what the read saw, a comment made after the batch included.
func TestGitHubNewComments_NewCommentsAtTheEnd(t *testing.T) {
	tr, _ := newReviewTestTracker(t)
	reader := &fakeCommentReader{pages: map[string]ghclient.PRCommentPage{
		"": {TotalCount: 4, HasPrevious: true, Before: "CUR", Comments: []ghclient.PRComment{
			ghComment(100, "bob", "2026-10-01T09:00:00Z"),
			ghComment(101, "alice", "2026-10-01T10:00:00Z"),
			ghComment(102, "dependabot[bot]", "2026-10-01T10:05:00Z"),
		}},
	}}
	curr := currWithCommentMarker(3, 101, "2026-10-01T10:00:00Z")

	got, _ := tr.githubNewComments(context.Background(), reader, reviewTestHost, "PR_7", prevWithCommentMarker(2), &curr)
	if ids := commentIDs(got); !slices.Equal(ids, []string{"101", "102"}) {
		t.Fatalf("new comments = %v, want [101 102]", ids)
	}
	if got[0].Author != "alice" || got[0].AuthorID != "alice" || got[0].CreatedAt != "2026-10-01T10:00:00Z" {
		t.Errorf("comment 101 = %+v, want alice's at 10:00", got[0])
	}
	if !slices.Equal(reader.asked, []string{""}) {
		t.Errorf("cursors read = %q, want the last page alone", reader.asked)
	}
	if curr.CommentCount != 4 || *curr.CommentMarker != (domain.CommentMarker{ID: 102, CreatedAt: "2026-10-01T10:05:00Z"}) {
		t.Errorf("curr = count %d marker %+v, want 4 and comment 102", curr.CommentCount, curr.CommentMarker)
	}
}

// More new comments than a page holds: the read goes back page by page until
// a page reaches the marker, and no further.
func TestGitHubNewComments_PagesBackToTheMarker(t *testing.T) {
	tr, _ := newReviewTestTracker(t)
	reader := &fakeCommentReader{pages: map[string]ghclient.PRCommentPage{
		"": {TotalCount: 6, HasPrevious: true, Before: "CUR_2", Comments: []ghclient.PRComment{
			ghComment(103, "alice", "2026-10-01T10:03:00Z"),
			ghComment(104, "bob", "2026-10-01T10:04:00Z"),
		}},
		"CUR_2": {TotalCount: 6, HasPrevious: true, Before: "CUR_1", Comments: []ghclient.PRComment{
			ghComment(100, "bob", "2026-10-01T09:00:00Z"),
			ghComment(101, "alice", "2026-10-01T10:01:00Z"),
			ghComment(102, "carol", "2026-10-01T10:02:00Z"),
		}},
	}}
	curr := currWithCommentMarker(6, 104, "2026-10-01T10:04:00Z")

	got, _ := tr.githubNewComments(context.Background(), reader, reviewTestHost, "PR_7", prevWithCommentMarker(2), &curr)
	if ids := commentIDs(got); !slices.Equal(ids, []string{"101", "102", "103", "104"}) {
		t.Fatalf("new comments = %v, want [101 102 103 104]", ids)
	}
	if !slices.Equal(reader.asked, []string{"", "CUR_2"}) {
		t.Errorf("cursors read = %q, want the read to stop at the page holding the marker", reader.asked)
	}
}

// A snapshot stored without the marker that counted comments counts the last
// ones as new, as many as the count rose by, and the read stops once it holds
// that many. One whose count did not rise is not read.
func TestGitHubNewComments_StoredWithoutMarker(t *testing.T) {
	tr, _ := newReviewTestTracker(t)
	reader := &fakeCommentReader{pages: map[string]ghclient.PRCommentPage{
		"": {TotalCount: 5, HasPrevious: true, Before: "CUR_2", Comments: []ghclient.PRComment{
			ghComment(205, "carol", "2026-10-01T10:05:00Z"),
		}},
		"CUR_2": {TotalCount: 5, HasPrevious: true, Before: "CUR_1", Comments: []ghclient.PRComment{
			ghComment(203, "alice", "2026-10-01T10:03:00Z"),
			ghComment(204, "bob", "2026-10-01T10:04:00Z"),
		}},
	}}
	prev := domain.PRSnapshot{Number: 7, CommentCount: 3}
	curr := currWithCommentMarker(5, 205, "2026-10-01T10:05:00Z")

	got, _ := tr.githubNewComments(context.Background(), reader, reviewTestHost, "PR_7", prev, &curr)
	if ids := commentIDs(got); !slices.Equal(ids, []string{"204", "205"}) {
		t.Fatalf("new comments = %v, want the last two, [204 205]", ids)
	}
	if !slices.Equal(reader.asked, []string{"", "CUR_2"}) {
		t.Errorf("cursors read = %q, want the read to stop once it holds two", reader.asked)
	}
	if curr.CommentCount != 5 || curr.CommentMarker == nil || curr.CommentMarker.ID != 205 {
		t.Errorf("curr = count %d marker %+v, want 5 and comment 205", curr.CommentCount, curr.CommentMarker)
	}

	reader.asked = nil
	unchanged := currWithCommentMarker(3, 203, "2026-10-01T10:03:00Z")
	if got, _ := tr.githubNewComments(context.Background(), reader, reviewTestHost, "PR_7", prev, &unchanged); got != nil || len(reader.asked) != 0 {
		t.Errorf("count did not rise: comments = %+v after %d reads, want none and no read", got, len(reader.asked))
	}
}

// A conversation comment TF recorded is TF's, matched on its artifact's key.
// A review comment with the same number is a different comment, and so is an
// artifact recorded on another GitHub host.
func TestGitHubNewComments_MarksTFComments(t *testing.T) {
	tr, stores := newReviewTestTracker(t)
	ctx := context.Background()
	org, team := runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID
	for _, seed := range []struct {
		scope string
		space domain.GitHubCommentSpace
		id    int64
	}{
		{reviewTestHost, domain.GitHubIssueComment, 101},
		{reviewTestHost, domain.GitHubReviewComment, 102},
		{"https://ghe.example.com", domain.GitHubIssueComment, 103},
	} {
		a := domain.Artifact{TeamID: team, Provider: domain.ArtifactProviderGitHub, Kind: domain.ArtifactKindComment,
			Scope: seed.scope, Target: "octo/repo#7", State: domain.ArtifactStateCommentPosted,
			DedupKey: domain.GitHubCommentDedupKey(seed.space, seed.id)}
		if _, err := stores.Artifacts.UpsertSystem(ctx, org, a); err != nil {
			t.Fatalf("seed comment artifact %d: %v", seed.id, err)
		}
	}
	reader := &fakeCommentReader{pages: map[string]ghclient.PRCommentPage{
		"": {TotalCount: 4, Comments: []ghclient.PRComment{
			ghComment(100, "bob", "2026-10-01T09:00:00Z"),
			ghComment(101, testUser, "2026-10-01T10:01:00Z"),
			ghComment(102, testUser, "2026-10-01T10:02:00Z"),
			ghComment(103, testUser, "2026-10-01T10:03:00Z"),
		}},
	}}
	curr := currWithCommentMarker(4, 103, "2026-10-01T10:03:00Z")

	got, _ := tr.githubNewComments(ctx, reader, reviewTestHost, "PR_7", prevWithCommentMarker(1), &curr)
	byTF := map[string]bool{}
	for _, c := range got {
		byTF[c.ID] = c.ByTF
	}
	want := map[string]bool{"101": true, "102": false, "103": false}
	for id, w := range want {
		if b, ok := byTF[id]; !ok || b != w {
			t.Errorf("comment %s: ByTF = %v (read: %v), want %v", id, b, ok, w)
		}
	}
}

// failingCommentArtifacts fails the comment-key lookup.
type failingCommentArtifacts struct{ db.ArtifactStore }

func (failingCommentArtifacts) PresentDedupKeysSystem(context.Context, string, string, []string) (map[string]bool, error) {
	return nil, errors.New("artifact store down")
}

// A failed read, of the comments or of which are TF's, keeps prev's marker and
// count, so the next poll finds the same comments new again. A prev without
// the marker keeps none, so the next poll counts again.
func TestGitHubNewComments_FailedReadKeepsPrev(t *testing.T) {
	pages := map[string]ghclient.PRCommentPage{"": {TotalCount: 3, Comments: []ghclient.PRComment{
		ghComment(100, "bob", "2026-10-01T09:00:00Z"),
		ghComment(101, "alice", "2026-10-01T10:00:00Z"),
	}}}
	failures := map[string]func(tr *Tracker, r *fakeCommentReader){
		"comment read":   func(_ *Tracker, r *fakeCommentReader) { r.err = errors.New("graphql down") },
		"tf attribution": func(tr *Tracker, _ *fakeCommentReader) { tr.artifacts = failingCommentArtifacts{tr.artifacts} },
	}
	prevs := map[string]domain.PRSnapshot{
		"with marker":    prevWithCommentMarker(2),
		"without marker": {Number: 7, CommentCount: 2},
	}
	for failure, setup := range failures {
		for name, prev := range prevs {
			t.Run(failure+"/"+name, func(t *testing.T) {
				tr, _ := newReviewTestTracker(t)
				reader := &fakeCommentReader{pages: pages}
				setup(tr, reader)
				curr := currWithCommentMarker(3, 101, "2026-10-01T10:00:00Z")

				if got, err := tr.githubNewComments(context.Background(), reader, reviewTestHost, "PR_7", prev, &curr); got != nil || err != nil {
					t.Errorf("comments = %+v, err = %v; want none, and no error for a failure other than a rate limit", got, err)
				}
				if curr.CommentCount != 2 {
					t.Errorf("count = %d, want prev's 2", curr.CommentCount)
				}
				switch {
				case prev.CommentMarker == nil && curr.CommentMarker != nil:
					t.Errorf("marker = %+v, want none, as prev had", curr.CommentMarker)
				case prev.CommentMarker != nil && (curr.CommentMarker == nil || *curr.CommentMarker != *prev.CommentMarker):
					t.Errorf("marker = %+v, want prev's %+v", curr.CommentMarker, prev.CommentMarker)
				}
			})
		}
	}
}

// commentRefreshServer answers the batch refresh with a pull request whose
// comments member is comments, and the comment read with page. It counts the
// comment reads.
func commentRefreshServer(t *testing.T, comments, page string, reads *int) *httptest.Server {
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
				{"id":"PR_node7","number":7,"title":"Reviewed PR","author":{"__typename":"User","login":"aidan"},
				 "state":"OPEN","url":"https://github.com/octo/repo/pull/7","repository":{"nameWithOwner":"octo/repo"},
				 "commits":{"nodes":[{"commit":{"oid":"sha7","statusCheckRollup":null}}]},
				 "labels":{"nodes":[{"name":"needs-reply"}]},
				 "createdAt":"2026-10-01T00:00:00Z","updatedAt":"2026-10-01T12:00:00Z","comments":` + comments + `}
			]}}`))
		case strings.Contains(req.Query, "comments(last: 50"):
			*reads++
			_, _ = w.Write([]byte(`{"data":{"node":{"comments":` + page + `}}}`))
		default:
			t.Errorf("unexpected query: %s", req.Query)
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func commentNodeJSON(id int64, typename, login, createdAt string) string {
	return `{"fullDatabaseId":"` + strconv.FormatInt(id, 10) + `","createdAt":"` + createdAt +
		`","author":{"__typename":"` + typename + `","login":"` + login + `"}}`
}

// TestRefreshGitHub_CommentedEndToEnd runs polls against stored snapshots of
// each kind: one with the marker, where a person's comment and TF's between
// the same two polls emit one event attributed to the person, and a quiet
// poll after it reads and emits nothing; one stored without the marker,
// which counts the rise; and one seeded from the REST listing, whose
// comments are its baseline.
func TestRefreshGitHub_CommentedEndToEnd(t *testing.T) {
	const (
		bobAt   = "2026-10-01T09:00:00Z"
		aliceAt = "2026-10-01T10:00:00Z"
		tfAt    = "2026-10-01T10:05:00Z"
	)
	batch := `{"totalCount":3,"nodes":[{"fullDatabaseId":"4102","createdAt":"` + tfAt + `"}]}`
	page := `{"totalCount":3,"pageInfo":{"hasPreviousPage":false,"startCursor":"CUR"},"nodes":[` +
		commentNodeJSON(4100, "User", "bob", bobAt) + `,` +
		commentNodeJSON(4101, "User", "alice", aliceAt) + `,` +
		commentNodeJSON(4102, "User", "aidan", tfAt) + `]}`

	cases := []struct {
		name          string
		prev          domain.PRSnapshot
		wantCommenter string // "": no event
		wantID        int64
		wantReads     int
	}{
		{name: "stored with the marker",
			prev:          domain.PRSnapshot{CommentCount: 1, CommentMarker: &domain.CommentMarker{ID: 4100, CreatedAt: bobAt}},
			wantCommenter: "alice", wantID: 4101, wantReads: 1},
		{name: "stored without the marker",
			prev:          domain.PRSnapshot{CommentCount: 1},
			wantCommenter: "alice", wantID: 4101, wantReads: 1},
		{name: "seeded from the REST listing",
			prev:      domain.PRSnapshot{},
			wantReads: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reads := 0
			srv := commentRefreshServer(t, batch, page, &reads)

			ctx := context.Background()
			database := newMigratedSQLite(t)
			stores := sqlitestore.New(database)
			org := runmode.LocalDefaultOrgID
			posted := domain.Artifact{TeamID: runmode.LocalDefaultTeamID, Provider: domain.ArtifactProviderGitHub, Kind: domain.ArtifactKindComment,
				Scope: reviewTestHost, Target: "octo/repo#7", State: domain.ArtifactStateCommentPosted,
				DedupKey: domain.GitHubCommentDedupKey(domain.GitHubIssueComment, 4102)}
			if _, err := stores.Artifacts.UpsertSystem(ctx, org, posted); err != nil {
				t.Fatalf("seed TF's comment: %v", err)
			}
			entityID := seedReviewedPR(t, stores, tc.prev)

			pub := &recordingPublisher{}
			tr := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, stores.Artifacts, stores.EventQueue, org)
			client := ghclient.NewClient(srv.URL, "tok")
			if _, _, err := tr.RefreshGitHub(ctx, reviewTestHost, client, "", nil, nil); err != nil {
				t.Fatalf("RefreshGitHub: %v", err)
			}

			got := findEvents(pub.nonSystemEvents(), domain.EventGitHubPRCommented)
			switch {
			case tc.wantCommenter == "" && len(got) != 0:
				t.Fatalf("commented events = %d, want none", len(got))
			case tc.wantCommenter != "" && len(got) != 1:
				t.Fatalf("commented events = %d, want one", len(got))
			case tc.wantCommenter != "":
				meta := decodeMetadata[events.GitHubPRCommentedMetadata](t, got[0])
				if meta.Commenter != tc.wantCommenter || meta.CommentID != tc.wantID || !slices.Equal(meta.CommenterLogins, []string{tc.wantCommenter}) {
					t.Errorf("metadata = %+v, want comment %d by %s alone", meta, tc.wantID, tc.wantCommenter)
				}
				if meta.Author != "aidan" || meta.Repo != "octo/repo" || meta.PRNumber != 7 || meta.HeadSHA != "sha7" || !slices.Equal(meta.Labels, []string{"needs-reply"}) {
					t.Errorf("pr fields = %+v", meta)
				}
				if wantAt, _ := time.Parse(time.RFC3339, aliceAt); !got[0].OccurredAt.Equal(wantAt) {
					t.Errorf("occurred_at = %v, want alice's comment at %v", got[0].OccurredAt, wantAt)
				}
			}
			if reads != tc.wantReads {
				t.Errorf("comment reads = %d, want %d", reads, tc.wantReads)
			}
			snap := storedPRSnapshot(t, stores, entityID)
			if snap.CommentCount != 3 || snap.CommentMarker == nil || *snap.CommentMarker != (domain.CommentMarker{ID: 4102, CreatedAt: tfAt}) {
				t.Errorf("stored snapshot: count %d marker %+v, want 3 and comment 4102", snap.CommentCount, snap.CommentMarker)
			}

			// The same pull request on the next poll: nothing moved, so
			// nothing is read and nothing emits.
			before := len(pub.nonSystemEvents())
			if _, _, err := tr.RefreshGitHub(ctx, reviewTestHost, client, "", nil, nil); err != nil {
				t.Fatalf("second RefreshGitHub: %v", err)
			}
			if reads != tc.wantReads {
				t.Errorf("comment reads after a quiet poll = %d, want %d", reads, tc.wantReads)
			}
			if after := pub.nonSystemEvents(); len(after) != before {
				t.Errorf("quiet poll emitted %v", eventTypes(after[before:]))
			}
		})
	}
}

// A rate limit on either read is returned, for the cycle to stop reading, and
// the pull request keeps its stored marker and count.
func TestGitHubReads_RateLimitReturned(t *testing.T) {
	limit := &ghclient.ErrRateLimited{ResumeAt: time.Now().Add(time.Hour)}
	t.Run("comments", func(t *testing.T) {
		tr, _ := newReviewTestTracker(t)
		curr := currWithCommentMarker(3, 101, "2026-10-01T10:00:00Z")
		got, err := tr.githubNewComments(context.Background(), rateLimitedReader{err: limit}, reviewTestHost, "PR_7", prevWithCommentMarker(2), &curr)
		if got != nil || !errors.Is(err, limit) {
			t.Errorf("comments = %+v, err = %v; want none and the rate limit", got, err)
		}
		if curr.CommentCount != 2 || curr.CommentMarker.ID != 100 {
			t.Errorf("curr = count %d marker %+v, want prev's", curr.CommentCount, curr.CommentMarker)
		}
	})
	t.Run("reviews", func(t *testing.T) {
		tr, _ := newReviewTestTracker(t)
		curr := currWithMarker(3, 101, "2026-10-01T10:00:00Z")
		got, err := tr.githubNewReviews(context.Background(), rateLimitedReader{err: limit}, reviewTestHost, "PR_7", prevWithMarker(2), &curr)
		if got != nil || !errors.Is(err, limit) {
			t.Errorf("reviews = %+v, err = %v; want none and the rate limit", got, err)
		}
		if curr.ReviewCount != 2 || curr.ReviewMarker.ID != 100 {
			t.Errorf("curr = count %d marker %+v, want prev's", curr.ReviewCount, curr.ReviewMarker)
		}
	})
}

// TestRefreshGitHub_RateLimitStopsTheReads runs a cycle over two pull requests
// with a new comment each, where GitHub rate-limits the first comment read: the
// other pull request's read is never sent, both still diff (their new label
// emits), the cycle reports no error, and both keep their stored markers. The
// next cycle reads both and reports both comments.
func TestRefreshGitHub_RateLimitStopsTheReads(t *testing.T) {
	var limited atomic.Bool
	limited.Store(true)
	var reads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string `json:"query"`
			Variables struct {
				IDs []string `json:"ids"`
				ID  string   `json:"id"`
			} `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		switch {
		case strings.Contains(req.Query, "nodes(ids:"):
			nodes := make([]string, 0, len(req.Variables.IDs))
			for _, id := range req.Variables.IDs {
				n, _ := strconv.Atoi(strings.TrimPrefix(id, "PR_node"))
				nodes = append(nodes, fmt.Sprintf(`{"id":%q,"number":%d,"title":"PR %d","author":{"__typename":"User","login":"aidan"},
					"state":"OPEN","url":"https://github.com/octo/repo/pull/%d","repository":{"nameWithOwner":"octo/repo"},
					"commits":{"nodes":[{"commit":{"oid":"sha%d","statusCheckRollup":null}}]},
					"labels":{"nodes":[{"name":"needs-reply"}]},
					"createdAt":"2026-10-01T00:00:00Z","updatedAt":"2026-10-01T12:00:00Z",
					"comments":{"totalCount":2,"nodes":[{"fullDatabaseId":"%d","createdAt":"2026-10-01T10:00:00Z"}]}}`,
					id, n, n, n, n, n*100+1))
			}
			_, _ = w.Write([]byte(`{"data":{"nodes":[` + strings.Join(nodes, ",") + `]}}`))
		case strings.Contains(req.Query, "comments(last: 50"):
			reads.Add(1)
			if limited.Load() {
				w.Header().Set("Retry-After", "600")
				http.Error(w, `{"message":"You have exceeded a secondary rate limit"}`, http.StatusTooManyRequests)
				return
			}
			n, _ := strconv.Atoi(strings.TrimPrefix(req.Variables.ID, "PR_node"))
			_, _ = w.Write([]byte(`{"data":{"node":{"comments":{"totalCount":2,"pageInfo":{"hasPreviousPage":false,"startCursor":"CUR"},"nodes":[` +
				commentNodeJSON(int64(n*100), "User", "bob", "2026-10-01T09:00:00Z") + `,` +
				commentNodeJSON(int64(n*100+1), "User", "alice", "2026-10-01T10:00:00Z") + `]}}}}`))
		default:
			t.Errorf("unexpected query: %s", req.Query)
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	entityIDs := map[int]string{}
	for _, n := range []int{7, 8} {
		e, _, err := stores.Entities.FindOrCreateSystem(ctx, org, "github", reviewTestHost, fmt.Sprintf("octo/repo#%d", n), "", "pr", fmt.Sprintf("PR %d", n), fmt.Sprintf("https://github.com/octo/repo/pull/%d", n))
		if err != nil {
			t.Fatalf("seed entity: %v", err)
		}
		prev := domain.PRSnapshot{NodeID: fmt.Sprintf("PR_node%d", n), Number: n, Repo: "octo/repo", State: "OPEN", Author: "aidan",
			HeadSHA: fmt.Sprintf("sha%d", n), Title: fmt.Sprintf("PR %d", n), UpdatedAt: "2026-10-01T08:00:00Z", CheckRuns: []domain.CheckRun{},
			CommentCount: 1, CommentMarker: &domain.CommentMarker{ID: int64(n * 100), CreatedAt: "2026-10-01T09:00:00Z"}}
		snapJSON, _ := json.Marshal(prev)
		if ok, err := stores.Entities.UpdateSnapshotCASSystem(ctx, org, e.ID, string(snapJSON), e.PollSeq); err != nil || !ok {
			t.Fatalf("seed snapshot: ok=%v err=%v", ok, err)
		}
		entityIDs[n] = e.ID
	}

	pub := &recordingPublisher{}
	tr := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, stores.Artifacts, stores.EventQueue, org)
	client := ghclient.NewClient(srv.URL, "tok")
	if _, _, err := tr.RefreshGitHub(ctx, reviewTestHost, client, "", nil, nil); err != nil {
		t.Fatalf("RefreshGitHub under a rate limit: %v", err)
	}
	if got := reads.Load(); got != 1 {
		t.Errorf("comment reads sent = %d, want 1: none after the rate limit", got)
	}
	evts := pub.nonSystemEvents()
	if got := findEvents(evts, domain.EventGitHubPRCommented); len(got) != 0 {
		t.Errorf("commented events = %d under a rate limit, want none", len(got))
	}
	if got := findEvents(evts, domain.EventGitHubPRLabelAdded); len(got) != 2 {
		t.Errorf("label_added events = %d, want one per pull request: the diff still runs", len(got))
	}
	for n, id := range entityIDs {
		if snap := storedPRSnapshot(t, stores, id); snap.CommentMarker == nil || snap.CommentMarker.ID != int64(n*100) || snap.CommentCount != 1 {
			t.Errorf("PR %d stored: count %d marker %+v, want its previous marker kept", n, snap.CommentCount, snap.CommentMarker)
		}
	}

	limited.Store(false)
	if _, _, err := tr.RefreshGitHub(ctx, reviewTestHost, client, "", nil, nil); err != nil {
		t.Fatalf("RefreshGitHub after the limit: %v", err)
	}
	got := findEvents(pub.nonSystemEvents(), domain.EventGitHubPRCommented)
	if len(got) != 2 {
		t.Fatalf("commented events after the limit = %d, want one per pull request", len(got))
	}
	for _, evt := range got {
		if meta := decodeMetadata[events.GitHubPRCommentedMetadata](t, evt); meta.Commenter != "alice" || meta.CommentID != int64(meta.PRNumber*100+1) {
			t.Errorf("metadata = %+v, want alice's comment on PR %d", meta, meta.PRNumber)
		}
	}
}
