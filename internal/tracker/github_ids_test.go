package tracker

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

// requesting is a snapshot whose pending review requests are the handles
// given, with ids for the ones in ids.
func requesting(ids map[string]string, handles ...string) domain.PRSnapshot {
	snap := basePRSnapshot()
	snap.Author = "carol"
	snap.ReviewRequests = handles
	snap.ReviewRequestIDs = ids
	return snap
}

var everyoneKnown = fakeReviewerResolver{
	users: map[string]bool{"alice": true, "alice-renamed": true, "bob": true},
	teams: map[string]bool{"acme/backend": true, "acme-corp/server": true},
}

// Each event is listed as "+key handle#id" for an added request and "-key
// handle#id" for a removed one. A request read with GitHub's id is keyed by
// it; the handles decide only when one side has no id.
func TestDiff_ReviewRequestsByGitHubID(t *testing.T) {
	signs := map[string]string{domain.EventGitHubPRReviewRequested: "+", domain.EventGitHubPRReviewRequestRemoved: "-"}
	for _, c := range []struct {
		name       string
		prev, curr domain.PRSnapshot
		want       []string
	}{
		{"first poll after ids were recorded", requesting(nil, "alice", "acme/backend"),
			requesting(map[string]string{"alice": "5", "acme/backend": "11"}, "alice", "acme/backend"), nil},
		{"a user and a team renamed", requesting(map[string]string{"alice": "5", "acme/backend": "11"}, "alice", "acme/backend"),
			requesting(map[string]string{"alice-renamed": "5", "acme-corp/server": "11"}, "alice-renamed", "acme-corp/server"), nil},
		{"a login another account now holds", requesting(map[string]string{"alice": "5"}, "alice"),
			requesting(map[string]string{"alice": "9"}, "alice"), []string{"+user_id:9 alice#9", "-user_id:5 alice#5"}},
		{"requests read with and without ids", requesting(nil),
			requesting(map[string]string{"alice": "5", "acme/backend": "11"}, "alice", "acme/backend", "bob"),
			[]string{"+team_id:11 acme/backend#11", "+user:bob bob#", "+user_id:5 alice#5"}},
		{"a removal", requesting(map[string]string{"alice": "5"}, "alice"), requesting(nil), []string{"-user_id:5 alice#5"}},
	} {
		var got []string
		for _, e := range DiffPRSnapshots(c.prev, c.curr, testEntityID, testRepositoryID, domain.GitHubAccount{}, everyoneKnown, nil, nil) {
			if signs[e.EventType] == "" {
				got = append(got, e.EventType)
				continue
			}
			m := decodeMetadata[events.GitHubPRReviewRequestedMetadata](t, e)
			got = append(got, fmt.Sprintf("%s%s %s%s#%s%s", signs[e.EventType], e.DedupKey, m.RequestedLogin, m.RequestedTeam, m.RequestedUserID, m.RequestedTeamID))
		}
		slices.Sort(got)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: events = %v, want %v", c.name, got, c.want)
		}
	}
}

// The local user's own pull request is told by their GitHub id when both the
// snapshot and the binding carry one.
func TestDiff_OwnPullRequestByGitHubID(t *testing.T) {
	self := domain.GitHubAccount{Login: "aidan-renamed", ID: "583231"}
	resolver := NewLocalReviewerResolver(self, []string{"acme/backend"})
	for _, c := range []struct {
		name              string
		author, authorID  string
		wantReviewRequest bool
	}{
		{"own PR under the old login", "aidan", "583231", false},
		{"another account's PR under the user's login", "aidan-renamed", "999", true},
		{"own PR stored before ids were", "aidan-renamed", "", false},
	} {
		prev := requesting(nil)
		prev.Author, prev.AuthorID = c.author, c.authorID
		curr := requesting(map[string]string{"acme/backend": "11"}, "acme/backend")
		curr.Author, curr.AuthorID = c.author, c.authorID
		got := len(findEvents(DiffPRSnapshots(prev, curr, testEntityID, testRepositoryID, self, resolver, nil, nil), domain.EventGitHubPRReviewRequested)) == 1
		if got != c.wantReviewRequest {
			t.Errorf("%s: review_requested emitted = %v, want %v", c.name, got, c.wantReviewRequest)
		}
	}
}

// Diffed without the review marker, a renamed reviewer's unchanged review is
// not a new one, and a review under a login another account now holds is.
func TestDiff_Reviews_ReviewerMatchedByGitHubID(t *testing.T) {
	prev := basePRSnapshot()
	prev.Reviews = []domain.ReviewState{
		{Author: "alice", AuthorID: "5", State: "APPROVED"},
		{Author: "bob", AuthorID: "6", State: "COMMENTED"},
	}
	curr := basePRSnapshot()
	curr.Reviews = []domain.ReviewState{
		{Author: "alice-renamed", AuthorID: "5", State: "APPROVED"},
		{Author: "bob", AuthorID: "9", State: "COMMENTED", DatabaseID: 77},
	}
	evts := DiffPRSnapshots(prev, curr, testEntityID, testRepositoryID, testSelf, nil, nil, nil)
	if len(evts) != 1 || evts[0].EventType != domain.EventGitHubPRReviewCommented {
		t.Fatalf("events = %v, want the other account's review_commented alone", eventTypes(evts))
	}
	if m := decodeMetadata[events.GitHubPRReviewCommentedMetadata](t, evts[0]); m.Reviewer != "bob" || m.ReviewerID != "9" {
		t.Errorf("reviewer = %s/%s, want bob/9", m.Reviewer, m.ReviewerID)
	}
}

func TestDiff_EventsCarryGitHubIDs(t *testing.T) {
	prev := markedPRSnapshot()
	prev.AuthorID = "583231"
	curr := markedPRSnapshot()
	curr.AuthorID = "583231"
	curr.Labels = []string{"needs-review"}
	reviews := []NewReview{{ID: 101, Author: "alice", AuthorID: "5", State: "APPROVED", SubmittedAt: "2026-10-01T10:00:00Z"}}
	comments := []NewComment{
		{ID: "201", Author: "bob", AuthorID: "6", CreatedAt: "2026-10-01T10:01:00Z"},
		{ID: "202", Author: "carol", AuthorID: "7", CreatedAt: "2026-10-01T10:02:00Z"},
		{ID: "203", Author: "bob", AuthorID: "6", CreatedAt: "2026-10-01T10:03:00Z"},
	}
	evts := DiffPRSnapshots(prev, curr, testEntityID, testRepositoryID, testSelf, nil, reviews, comments)
	type authorID struct {
		AuthorID string `json:"author_id"`
	}
	for _, evt := range evts {
		if m := decodeMetadata[authorID](t, evt); m.AuthorID != "583231" {
			t.Errorf("%s: author_id = %q, want 583231", evt.EventType, m.AuthorID)
		}
	}
	approved := findEvent(evts, domain.EventGitHubPRReviewApproved)
	if approved == nil || decodeMetadata[events.GitHubPRReviewApprovedMetadata](t, *approved).ReviewerID != "5" {
		t.Errorf("review_approved = %v, want reviewer_id 5", approved)
	}
	commented := findEvent(evts, domain.EventGitHubPRCommented)
	if commented == nil {
		t.Fatal("no commented event")
	}
	m := decodeMetadata[events.GitHubPRCommentedMetadata](t, *commented)
	if m.CommenterID != "6" || !reflect.DeepEqual(m.CommenterIDs, []string{"6", "7"}) || !reflect.DeepEqual(m.CommenterLogins, []string{"bob", "carol"}) {
		t.Errorf("commented = %s/%s ids %v logins %v, want bob/6, [6 7], [bob carol]", m.Commenter, m.CommenterID, m.CommenterIDs, m.CommenterLogins)
	}
}

// The quiet-entity reconcile checks the stored snapshot for the reviewer a
// task's dedup key names: by id, or by handle for a task created before ids
// were recorded.
func TestReviewerFromDedupKey_FindsTheRequestEitherWay(t *testing.T) {
	snap := requesting(map[string]string{"alice-renamed": "5", "acme/backend": "11"}, "alice-renamed", "acme/backend")
	for key, want := range map[string]bool{
		"user_id:5":         true,
		"team_id:11":        true,
		"team:acme/backend": true,
		"user:alice":        false, // a login the reviewer no longer has
		"user_id:9":         false,
		"team_id:5":         false, // a user's id is not a team's
	} {
		r, ok := reviewerFromDedupKey(key)
		if !ok {
			t.Fatalf("reviewerFromDedupKey(%q) not ok", key)
		}
		if got := snap.HasRequestedReviewer(r); got != want {
			t.Errorf("%s still requested = %v, want %v", key, got, want)
		}
		if got := reviewerDedupKey(r); got != key {
			t.Errorf("reviewerDedupKey(reviewerFromDedupKey(%q)) = %q", key, got)
		}
	}
	if _, ok := reviewerFromDedupKey("build"); ok {
		t.Error("an unprefixed key parsed as a reviewer")
	}
}

type idUsers struct {
	db.UsersStore
	byID, byLogin map[string][]string
}

func (u idUsers) UserIDsForGitHubUserIDSystem(_ context.Context, _, id, _ string) ([]string, error) {
	return u.byID[id], nil
}

func (u idUsers) UserIDsForGitHubLoginSystem(_ context.Context, _, login string) ([]string, error) {
	return u.byLogin[login], nil
}

// The multi-mode resolver looks a user up by GitHub's id when the request
// carries one, and by login only when it does not.
func TestStoreReviewerResolver_KnownUserByGitHubID(t *testing.T) {
	users := idUsers{byID: map[string][]string{"5": {"u-alice"}}, byLogin: map[string][]string{"alice": {"u-other"}}}
	r := NewStoreReviewerResolver(context.Background(), "org", "https://github.com", users, nil)
	if !r.KnownUser("5", "alice-renamed") {
		t.Error("KnownUser(5) = false; want the bound account")
	}
	if r.KnownUser("9", "alice") {
		t.Error("KnownUser(9, alice) = true; an id answers alone, the login is not consulted")
	}
	if !r.KnownUser("", "alice") {
		t.Error("KnownUser(no id, alice) = false; want the login lookup")
	}
}

// merged and closed name who merged or closed the pull request, login and
// GitHub's id, from the snapshot, on the transition and on first discovery.
func TestDiff_MergedAndClosedNameTheActor(t *testing.T) {
	merged := basePRSnapshot()
	merged.State, merged.Merged = "MERGED", true
	merged.MergedBy, merged.MergedByID = "github-merge-queue[bot]", "9919"
	closed := basePRSnapshot()
	closed.State = "CLOSED"
	closed.ClosedBy, closed.ClosedByID = "stale[bot]", "2740"

	for _, prev := range []domain.PRSnapshot{basePRSnapshot(), {}} {
		evts := DiffPRSnapshots(prev, merged, testEntityID, testRepositoryID, testSelf, nil, nil, nil)
		if e := findEvent(evts, domain.EventGitHubPRMerged); e == nil {
			t.Errorf("prev #%d: no merged event in %v", prev.Number, eventTypes(evts))
		} else if m := decodeMetadata[events.GitHubPRMergedMetadata](t, *e); m.MergedBy != "github-merge-queue[bot]" || m.MergedByID != "9919" {
			t.Errorf("prev #%d: merged by %s/%s, want github-merge-queue[bot]/9919", prev.Number, m.MergedBy, m.MergedByID)
		}

		evts = DiffPRSnapshots(prev, closed, testEntityID, testRepositoryID, testSelf, nil, nil, nil)
		if e := findEvent(evts, domain.EventGitHubPRClosed); e == nil {
			t.Errorf("prev #%d: no closed event in %v", prev.Number, eventTypes(evts))
		} else if m := decodeMetadata[events.GitHubPRClosedMetadata](t, *e); m.ClosedBy != "stale[bot]" || m.ClosedByID != "2740" {
			t.Errorf("prev #%d: closed by %s/%s, want stale[bot]/2740", prev.Number, m.ClosedBy, m.ClosedByID)
		}
	}
}
