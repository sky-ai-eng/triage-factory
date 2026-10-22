package tracker

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

// Every GitHub event names its actors by GitHub's id beside their login: the
// PR author (AuthorID), a review's reviewer (ReviewerID), a comment's writer
// (CommenterID). A literal that sets the login and leaves the id out emits
// events no id predicate can match and that route by login alone, and
// compiles, so the package source is checked directly, as the repository id
// is (TestGitHubMetadataLiteralsSetRepositoryID).
func TestGitHubMetadataLiteralsSetActorIDs(t *testing.T) {
	pairs := map[string]string{"Author": "AuthorID", "Reviewer": "ReviewerID", "Commenter": "CommenterID"}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || !strings.HasPrefix(sel.Sel.Name, "GitHub") || !strings.HasSuffix(sel.Sel.Name, "Metadata") {
				return true
			}
			checked++
			keys := map[string]bool{}
			for _, elt := range lit.Elts {
				if kv, ok := elt.(*ast.KeyValueExpr); ok {
					if key, ok := kv.Key.(*ast.Ident); ok {
						keys[key.Name] = true
					}
				}
			}
			for handle, id := range pairs {
				if keys[handle] && !keys[id] {
					t.Errorf("%s: events.%s literal sets %s without %s", fset.Position(lit.Pos()), sel.Sel.Name, handle, id)
				}
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("found no GitHub metadata literals: the guard checked nothing")
	}
}

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

// The first poll after ids were recorded diffs a stored snapshot that has none
// against one that has them. The reviewers are the same, so nothing is added
// or removed — the handles decide when one side has no id.
func TestDiff_ReviewRequests_FirstPollWithIDsIsNoChange(t *testing.T) {
	prev := requesting(nil, "alice", "acme/backend")
	curr := requesting(map[string]string{"alice": "5", "acme/backend": "11"}, "alice", "acme/backend")
	if evts := DiffPRSnapshots(prev, curr, testEntityID, testRepositoryID, domain.GitHubAccount{}, everyoneKnown, nil, nil); len(evts) != 0 {
		t.Errorf("events = %v, want none", eventTypes(evts))
	}
}

// A user or team renamed between polls is the same reviewer: its id did not
// change.
func TestDiff_ReviewRequests_RenameIsNoChange(t *testing.T) {
	prev := requesting(map[string]string{"alice": "5", "acme/backend": "11"}, "alice", "acme/backend")
	curr := requesting(map[string]string{"alice-renamed": "5", "acme-corp/server": "11"}, "alice-renamed", "acme-corp/server")
	if evts := DiffPRSnapshots(prev, curr, testEntityID, testRepositoryID, domain.GitHubAccount{}, everyoneKnown, nil, nil); len(evts) != 0 {
		t.Errorf("events = %v, want none", eventTypes(evts))
	}
}

// A login GitHub handed to another account names a different reviewer: the
// first account's request is removed and the second's added.
func TestDiff_ReviewRequests_LoginOfAnotherAccountIsAnotherReviewer(t *testing.T) {
	prev := requesting(map[string]string{"alice": "5"}, "alice")
	curr := requesting(map[string]string{"alice": "9"}, "alice")
	evts := DiffPRSnapshots(prev, curr, testEntityID, testRepositoryID, domain.GitHubAccount{}, everyoneKnown, nil, nil)
	added := findEvents(evts, domain.EventGitHubPRReviewRequested)
	removed := findEvents(evts, domain.EventGitHubPRReviewRequestRemoved)
	if len(added) != 1 || added[0].DedupKey != "user_id:9" || len(removed) != 1 || removed[0].DedupKey != "user_id:5" {
		t.Errorf("events = %v, want user_id:9 added and user_id:5 removed", evts)
	}
}

// A request read with GitHub's id is keyed by it and carries it, beside the
// handle, in its metadata; one read without stays keyed by handle.
func TestDiff_ReviewRequests_KeyedByGitHubID(t *testing.T) {
	prev := requesting(nil)
	curr := requesting(map[string]string{"alice": "5", "acme/backend": "11"}, "alice", "acme/backend", "bob")
	evts := DiffPRSnapshots(prev, curr, testEntityID, testRepositoryID, domain.GitHubAccount{}, everyoneKnown, nil, nil)
	byKey := map[string]events.GitHubPRReviewRequestedMetadata{}
	for _, e := range findEvents(evts, domain.EventGitHubPRReviewRequested) {
		byKey[e.DedupKey] = decodeMetadata[events.GitHubPRReviewRequestedMetadata](t, e)
	}
	want := map[string]events.GitHubPRReviewRequestedMetadata{
		"user_id:5":  {RequestedLogin: "alice", RequestedUserID: "5"},
		"team_id:11": {RequestedTeam: "acme/backend", RequestedTeamID: "11"},
		"user:bob":   {RequestedLogin: "bob"},
	}
	if len(byKey) != len(want) {
		t.Fatalf("review_requested keys = %v, want %v", keysOf(byKey), keysOf(want))
	}
	for key, w := range want {
		got := byKey[key]
		if got.RequestedLogin != w.RequestedLogin || got.RequestedTeam != w.RequestedTeam ||
			got.RequestedUserID != w.RequestedUserID || got.RequestedTeamID != w.RequestedTeamID {
			t.Errorf("%s metadata = %+v, want the identity %+v", key, got, w)
		}
	}
}

// A removal names the reviewer by id and by handle, so the router can close a
// task keyed either way.
func TestDiff_ReviewRequestRemoved_CarriesTheHandleBesideTheID(t *testing.T) {
	prev := requesting(map[string]string{"alice": "5"}, "alice")
	curr := requesting(nil)
	removed := findEvents(DiffPRSnapshots(prev, curr, testEntityID, testRepositoryID, domain.GitHubAccount{}, everyoneKnown, nil, nil),
		domain.EventGitHubPRReviewRequestRemoved)
	if len(removed) != 1 || removed[0].DedupKey != "user_id:5" {
		t.Fatalf("removals = %v, want one keyed user_id:5", removed)
	}
	if m := decodeMetadata[events.GitHubPRReviewRequestRemovedMetadata](t, removed[0]); m.RequestedLogin != "alice" || m.RequestedUserID != "5" {
		t.Errorf("removal metadata = %+v, want alice and 5", m)
	}
}

// The local user's own pull request is told by their GitHub id when both the
// snapshot and the binding carry one: a rename keeps it theirs, and their old
// login on another account's pull request does not make it theirs.
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

// Diffed without the review marker, a reviewer is matched across snapshots by
// GitHub's id: a renamed reviewer's unchanged review is not a new one, and a
// review under a login another account now holds is.
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

// Every event carries the PR author's id, a review's reviewer's, and the ids
// of the commenters.
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

// A task's dedup key names the reviewer the quiet-entity reconcile checks the
// stored snapshot for: by id for a key by id, by handle for a key by handle, as
// a task created before ids were recorded carries.
func TestReviewerFromDedupKey_FindsTheRequestEitherWay(t *testing.T) {
	snap := requesting(map[string]string{"alice": "5", "acme/backend": "11"}, "alice-renamed", "acme/backend")
	snap.ReviewRequestIDs = map[string]string{"alice-renamed": "5", "acme/backend": "11"}
	for key, want := range map[string]bool{
		"user_id:5":         true,
		"team_id:11":        true,
		"team:acme/backend": true,
		"user:alice":        false, // keyed by a login the reviewer no longer has
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

// idUsers is a UsersStore whose reverse lookups answer from fixed maps.
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

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
