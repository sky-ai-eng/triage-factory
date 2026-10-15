package tracker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// mentionResolver knows bob, erin, gina and hank, and the team acme/backend.
var mentionResolver = fakeReviewerResolver{
	users: map[string]bool{"bob": true, "erin": true, "gina": true, "hank": true},
	teams: map[string]bool{"acme/backend": true},
}

// wantMention is one mentioned event a test expects, keyed by its dedup_key.
type wantMention struct {
	login, team, source, commenter string
	commentID                      int64
	at                             string
}

// assertMentions checks that evts hold exactly the mentioned events want
// names, keyed by dedup_key, with their identity, source, commenter, comment
// id and time, entityID, and the PR fields of curr.
func assertMentions(t *testing.T, evts []domain.Event, entityID string, curr domain.PRSnapshot, want map[string]wantMention) {
	t.Helper()
	got := findEvents(evts, domain.EventGitHubPRMentioned)
	if len(got) != len(want) {
		t.Fatalf("mentioned events = %d (%v), want %d", len(got), mentionKeys(got), len(want))
	}
	for _, evt := range got {
		w, ok := want[evt.DedupKey]
		if !ok {
			t.Errorf("unexpected mentioned event keyed %q: %s", evt.DedupKey, evt.MetadataJSON)
			continue
		}
		if evt.EntityID == nil || *evt.EntityID != entityID {
			t.Errorf("%s: entity id = %v, want %s", evt.DedupKey, evt.EntityID, entityID)
		}
		meta := decodeMetadata[events.GitHubPRMentionedMetadata](t, evt)
		if meta.MentionedLogin != w.login || meta.MentionedTeam != w.team || meta.MentionSource != w.source ||
			meta.Commenter != w.commenter || meta.CommentID != w.commentID {
			t.Errorf("%s: metadata = %+v, want %+v", evt.DedupKey, meta, w)
		}
		if meta.Author != curr.Author || meta.Repo != curr.Repo || meta.PRNumber != curr.Number || meta.HeadSHA != curr.HeadSHA {
			t.Errorf("%s: pr fields = %+v, want the current snapshot's", evt.DedupKey, meta)
		}
		if wantAt, _ := time.Parse(time.RFC3339, w.at); !evt.OccurredAt.Equal(wantAt) {
			t.Errorf("%s: occurred_at = %v, want %v", evt.DedupKey, evt.OccurredAt, wantAt)
		}
	}
}

func mentionKeys(evts []domain.Event) []string {
	keys := []string{}
	for _, e := range evts {
		keys = append(keys, e.DedupKey)
	}
	return keys
}

func mentionComment(id, login, at string, mentions ...string) NewComment {
	return NewComment{ID: id, Author: login, AuthorID: login, CreatedAt: at, Mentions: mentions}
}

// TestDiffPRSnapshots_Mentioned covers which mentions in new comments and
// reviews emit: one event per TF-known identity, attributed to its newest
// comment or review, and none for TF's own comments and reviews, a
// commenter's mention of themselves, or an identity TF does not know.
func TestDiffPRSnapshots_Mentioned(t *testing.T) {
	const (
		t1 = "2026-10-01T09:00:00Z"
		t2 = "2026-10-01T09:05:00Z"
		t3 = "2026-10-01T09:10:00Z"
	)
	tf := func(c NewComment) NewComment { c.ByTF = true; return c }
	cases := []struct {
		name     string
		comments []NewComment
		reviews  []NewReview
		want     map[string]wantMention
	}{
		{name: "nothing new"},
		{name: "a comment mentioning a user",
			comments: []NewComment{mentionComment("11", "carol", t1, "bob")},
			want:     map[string]wantMention{"user:bob": {login: "bob", source: "comment", commenter: "carol", commentID: 11, at: t1}}},
		{name: "a comment mentioning a team",
			comments: []NewComment{mentionComment("11", "carol", t1, "acme/backend")},
			want:     map[string]wantMention{"team:acme/backend": {team: "acme/backend", source: "comment", commenter: "carol", commentID: 11, at: t1}}},
		{name: "identities TF does not know",
			comments: []NewComment{mentionComment("11", "carol", t1, "stranger", "acme/unmapped")}},
		{name: "a comment TF posted",
			comments: []NewComment{tf(mentionComment("11", testUser, t1, "bob"))}},
		{name: "a bot's comment",
			comments: []NewComment{mentionComment("11", "dependabot[bot]", t1, "bob")},
			want:     map[string]wantMention{"user:bob": {login: "bob", source: "comment", commenter: "dependabot[bot]", commentID: 11, at: t1}}},
		{name: "a commenter mentioning themselves",
			comments: []NewComment{mentionComment("11", "Bob", t1, "bob"), mentionComment("12", "erin[bot]", t2, "erin")}},
		{name: "one identity mentioned several times",
			comments: []NewComment{mentionComment("11", "carol", t1, "bob"), mentionComment("12", "dave", t3, "bob")},
			reviews:  []NewReview{{ID: 21, Author: "frank", State: "COMMENTED", SubmittedAt: t2, Mentions: []string{"bob"}}},
			want:     map[string]wantMention{"user:bob": {login: "bob", source: "comment", commenter: "dave", commentID: 12, at: t3}}},
		{name: "a review's body",
			reviews: []NewReview{{ID: 21, Author: "frank", State: "APPROVED", SubmittedAt: t2, Mentions: []string{"bob"}}},
			want:    map[string]wantMention{"user:bob": {login: "bob", source: "review", commenter: "frank", commentID: 21, at: t2}}},
		{name: "a review's inline comment",
			reviews: []NewReview{{ID: 21, Author: "frank", State: "COMMENTED", SubmittedAt: t2,
				Comments: []NewReviewComment{{ID: 31}, {ID: 32, Mentions: []string{"acme/backend"}}}}},
			want: map[string]wantMention{"team:acme/backend": {team: "acme/backend", source: "review_comment", commenter: "frank", commentID: 32, at: t2}}},
		{name: "a review TF submitted",
			reviews: []NewReview{{ID: 21, Author: testUser, State: "COMMENTED", SubmittedAt: t2, ByTF: true,
				Mentions: []string{"bob"}, Comments: []NewReviewComment{{ID: 31, Mentions: []string{"erin"}}}}}},
		{name: "TF's reply inside a person's review",
			reviews: []NewReview{{ID: 21, Author: "frank", State: "COMMENTED", SubmittedAt: t2,
				Comments: []NewReviewComment{{ID: 31, ByTF: true, Mentions: []string{"bob"}}, {ID: 32, Mentions: []string{"erin"}}}}},
			want: map[string]wantMention{"user:erin": {login: "erin", source: "review_comment", commenter: "frank", commentID: 32, at: t2}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev := basePRSnapshot()
			prev.ReviewMarker = &domain.ReviewMarker{}
			curr := basePRSnapshot()
			curr.ReviewMarker = &domain.ReviewMarker{}
			evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, mentionResolver, tc.reviews, tc.comments)
			assertMentions(t, evts, testEntityID, curr, tc.want)
		})
	}
}

// TestDiffPRSnapshots_MentionedInBody covers the body: an identity added to
// its mentions emits once, an edit that adds none emits nothing, a snapshot
// without the mentions is a baseline, and a comment's mention of the same
// identity takes precedence over the body's.
func TestDiffPRSnapshots_MentionedInBody(t *testing.T) {
	const updatedAt = "2026-10-01T12:00:00Z"
	cases := []struct {
		name       string
		prev, curr []string
		comments   []NewComment
		want       map[string]wantMention
	}{
		{name: "a mention added", prev: []string{"stranger"}, curr: []string{"stranger", "bob", "acme/backend"},
			want: map[string]wantMention{
				"user:bob":          {login: "bob", source: "body", at: updatedAt},
				"team:acme/backend": {team: "acme/backend", source: "body", at: updatedAt},
			}},
		{name: "the first mention", prev: []string{}, curr: []string{"bob"},
			want: map[string]wantMention{"user:bob": {login: "bob", source: "body", at: updatedAt}}},
		{name: "an edit that adds no mention", prev: []string{"bob"}, curr: []string{"bob"}},
		{name: "a mention removed", prev: []string{"bob", "erin"}, curr: []string{"erin"}},
		{name: "an unknown identity added", prev: []string{}, curr: []string{"stranger"}},
		{name: "a snapshot stored without the mentions", prev: nil, curr: []string{"bob"}},
		{name: "a response without the body", prev: []string{}, curr: nil},
		{name: "a comment mentions the same identity", prev: []string{}, curr: []string{"bob"},
			comments: []NewComment{mentionComment("11", "carol", "2026-10-01T09:00:00Z", "bob")},
			want:     map[string]wantMention{"user:bob": {login: "bob", source: "comment", commenter: "carol", commentID: 11, at: "2026-10-01T09:00:00Z"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev := basePRSnapshot()
			prev.BodyMentions = tc.prev
			curr := basePRSnapshot()
			curr.BodyMentions, curr.UpdatedAt = tc.curr, updatedAt
			evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, mentionResolver, nil, tc.comments)
			assertMentions(t, evts, testEntityID, curr, tc.want)
		})
	}
}

// A PR seen for the first time records its body's mentions and emits nothing
// for them.
func TestDiffPRSnapshots_MentionedNotOnFirstDiscovery(t *testing.T) {
	curr := basePRSnapshot()
	curr.BodyMentions = []string{"bob", "acme/backend"}
	if evts := DiffPRSnapshots(domain.PRSnapshot{}, curr, testEntityID, testUser, mentionResolver, nil, nil); len(evts) != 0 {
		t.Errorf("first discovery emitted %v, want nothing", eventTypes(evts))
	}
}

// TestDiffPRSnapshots_MentionedPredicate pins that a rule can select a
// mention by the identity mentioned, its source and its commenter.
func TestDiffPRSnapshots_MentionedPredicate(t *testing.T) {
	prev, curr := basePRSnapshot(), basePRSnapshot()
	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, mentionResolver, nil,
		[]NewComment{mentionComment("11", "carol", "2026-10-01T09:00:00Z", "bob")})
	evt := findEvent(evts, domain.EventGitHubPRMentioned)
	if evt == nil {
		t.Fatal("no mentioned event")
	}
	for pred, want := range map[string]bool{
		`{"mentioned_login":"BOB"}`:                    true,
		`{"mentioned_login":"erin"}`:                   false,
		`{"mentioned_team":"acme/backend"}`:            false,
		`{"mention_source":"comment"}`:                 true,
		`{"mention_source":"body"}`:                    false,
		allowlist(t, "commenter_in", "Carol"):          true,
		`{"mentioned_login":"bob","author":"someone"}`: false,
	} {
		if got := matchesPredicate(t, *evt, pred); got != want {
			t.Errorf("%s matched = %v, want %v", pred, got, want)
		}
	}
}

// TestLocalReviewerResolver_FoldsCase pins that the local resolver knows the
// user and their teams however a mention spells them.
func TestLocalReviewerResolver_FoldsCase(t *testing.T) {
	r := NewLocalReviewerResolver("AidanAllchin", []string{"Sky-AI-Eng/Backend"})
	if !r.KnownUser("aidanallchin") || !r.KnownUser("AidanAllchin") || r.KnownUser("someone") {
		t.Error("KnownUser does not fold case")
	}
	if !r.KnownTeam("sky-ai-eng", "backend") || r.KnownTeam("sky-ai-eng", "frontend") {
		t.Error("KnownTeam does not fold case")
	}
}

// A reply TF posted that joined a person's review is TF's; the review and the
// person's own comment in it are not.
func TestGitHubNewReviews_MarksTFRepliesInAPersonsReview(t *testing.T) {
	tr, stores := newReviewTestTracker(t)
	ctx := context.Background()
	reply := domain.Artifact{TeamID: runmode.LocalDefaultTeamID, Provider: domain.ArtifactProviderGitHub, Kind: domain.ArtifactKindComment,
		Scope: reviewTestHost, Target: "octo/repo#7", State: domain.ArtifactStateCommentPosted,
		DedupKey: domain.GitHubCommentDedupKey(domain.GitHubReviewComment, 802)}
	if _, err := stores.Artifacts.UpsertSystem(ctx, runmode.LocalDefaultOrgID, reply); err != nil {
		t.Fatalf("seed comment artifact: %v", err)
	}
	reader := &fakeReviewReader{pages: map[string]ghclient.PRReviewPage{
		"": {TotalCount: 2, Reviews: []ghclient.PRReview{
			review(100, "bob", "COMMENTED", "2026-10-01T09:00:00Z"),
			review(101, "bob", "COMMENTED", "2026-10-01T10:00:00Z",
				ghclient.PRReviewComment{ID: 801, Path: "a.go", Mentions: []string{"erin"}},
				ghclient.PRReviewComment{ID: 802, Path: "a.go", Mentions: []string{"carol"}}),
		}},
	}}
	prev := prevWithMarker(1)
	curr := currWithMarker(2, 101, "2026-10-01T10:00:00Z")

	got, _ := tr.githubNewReviews(ctx, reader, reviewTestHost, "PR_7", prev, &curr)
	if len(got) != 1 || got[0].ByTF || len(got[0].Comments) != 2 {
		t.Fatalf("reviews = %+v, want bob's review, not TF's, with both comments", got)
	}
	if c := got[0].Comments; c[0].ByTF || !c[1].ByTF || c[0].Mentions[0] != "erin" || c[1].Mentions[0] != "carol" {
		t.Errorf("comments = %+v, want 802 alone TF's, each with its mentions", c)
	}
}

// mentionRefreshServer answers the batch refresh with octo/repo#7 carrying
// body, comments and reviews as its members, and the comment and review
// reads with their pages.
func mentionRefreshServer(t *testing.T, body, comments, reviews, commentPage, reviewPage string) *httptest.Server {
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
				{"id":"PR_node7","number":7,"title":"Reviewed PR","author":{"__typename":"User","login":"aidan"},"body":` + body + `,
				 "state":"OPEN","url":"https://github.com/octo/repo/pull/7","repository":{"nameWithOwner":"octo/repo"},
				 "commits":{"nodes":[{"commit":{"oid":"sha7","statusCheckRollup":null}}]},
				 "createdAt":"2026-10-01T00:00:00Z","updatedAt":"2026-10-01T12:00:00Z",
				 "comments":` + comments + `,"reviews":` + reviews + `}
			]}}`))
		case strings.Contains(req.Query, "comments(last: 50"):
			_, _ = w.Write([]byte(`{"data":{"node":{"comments":` + commentPage + `}}}`))
		case strings.Contains(req.Query, "reviews(last: 50"):
			_, _ = w.Write([]byte(`{"data":{"node":{"reviews":` + reviewPage + `}}}`))
		default:
			t.Errorf("unexpected query: %s", req.Query)
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// jsonString quotes s as a JSON string.
func jsonString(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

// TestRefreshGitHub_MentionedEndToEnd runs a poll over a PR whose new comments,
// reviews and body mention people, with the bodies as GitHub returns them. A
// comment, a review's body and a bot's comment emit, and so does the body's
// added mention, with no commenter. hank is mentioned only where nothing
// counts: an older comment, a quoted line, a code block, inline code, TF's
// comment, TF's reply inside a person's review, and an HTML comment in the
// body. The snapshot stores the body's mentions, and the next poll emits
// nothing.
func TestRefreshGitHub_MentionedEndToEnd(t *testing.T) {
	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org, team := runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID
	for _, key := range []string{
		domain.GitHubCommentDedupKey(domain.GitHubIssueComment, 4102),
		domain.GitHubCommentDedupKey(domain.GitHubReviewComment, 6002),
	} {
		posted := domain.Artifact{TeamID: team, Provider: domain.ArtifactProviderGitHub, Kind: domain.ArtifactKindComment,
			Scope: reviewTestHost, Target: "octo/repo#7", State: domain.ArtifactStateCommentPosted, DedupKey: key}
		if _, err := stores.Artifacts.UpsertSystem(ctx, org, posted); err != nil {
			t.Fatalf("seed TF's comment %s: %v", key, err)
		}
	}
	entityID := seedReviewedPR(t, stores, domain.PRSnapshot{
		CommentCount: 1, CommentMarker: &domain.CommentMarker{ID: 4100, CreatedAt: "2026-10-01T09:00:00Z"},
		ReviewCount: 1, ReviewMarker: &domain.ReviewMarker{ID: 5100, SubmittedAt: "2026-10-01T09:00:00Z"},
		BodyMentions: []string{"stranger"},
	})

	comment := func(id, typename, login, at, body string) string {
		return `{"fullDatabaseId":"` + id + `","createdAt":"` + at + `","author":{"__typename":"` + typename + `","login":"` + login + `"},"body":` + jsonString(body) + `}`
	}
	commentPage := `{"totalCount":4,"pageInfo":{"hasPreviousPage":false,"startCursor":"C1"},"nodes":[` +
		comment("4100", "User", "bob", "2026-10-01T09:00:00Z", "@hank from before") + `,` +
		comment("4101", "User", "carol", "2026-10-01T10:00:00Z", "@Bob can you look?\n\n> @hank wrote this\n\n```\n@hank\n```\nand `@hank`") + `,` +
		comment("4102", "User", "aidan", "2026-10-01T10:05:00Z", "@hank done") + `,` +
		comment("4103", "Bot", "dependabot", "2026-10-01T10:30:00Z", "@acme/backend rebased") + `]}`
	reviewNode := func(id, login, at, body, comments string) string {
		return `{"fullDatabaseId":"` + id + `","state":"COMMENTED","submittedAt":"` + at + `","author":{"__typename":"User","login":"` + login + `"},` +
			`"body":` + jsonString(body) + `,"comments":{"totalCount":` + comments + `}}`
	}
	reviewPage := `{"totalCount":3,"pageInfo":{"hasPreviousPage":false,"startCursor":"R1"},"nodes":[` +
		reviewNode("5100", "frank", "2026-10-01T09:00:00Z", "", `0,"nodes":[]`) + `,` +
		reviewNode("5101", "dave", "2026-10-01T10:10:00Z", "LGTM, cc @erin", `0,"nodes":[]`) + `,` +
		reviewNode("5102", "frank", "2026-10-01T10:20:00Z", "", `2,"nodes":[`+
			`{"fullDatabaseId":"6001","path":"a.go","body":"nit"},`+
			`{"fullDatabaseId":"6002","path":"a.go","body":"cc @hank"}]`) + `]}`
	srv := mentionRefreshServer(t,
		jsonString("Fixes it. <!-- @hank --> cc @stranger and @Gina"),
		`{"totalCount":4,"nodes":[{"fullDatabaseId":"4103","createdAt":"2026-10-01T10:30:00Z"}]}`,
		`{"totalCount":3,"nodes":[{"fullDatabaseId":"5102","submittedAt":"2026-10-01T10:20:00Z"}]}`,
		commentPage, reviewPage)

	pub := &recordingPublisher{}
	tr := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, stores.Artifacts, stores.EventQueue, org)
	client := ghclient.NewClient(srv.URL, "tok")
	if _, _, err := tr.RefreshGitHub(ctx, reviewTestHost, client, "", nil, mentionResolver); err != nil {
		t.Fatalf("RefreshGitHub: %v", err)
	}

	curr := domain.PRSnapshot{Author: "aidan", Repo: "octo/repo", Number: 7, HeadSHA: "sha7"}
	assertMentions(t, pub.nonSystemEvents(), entityID, curr, map[string]wantMention{
		"user:bob":          {login: "bob", source: "comment", commenter: "carol", commentID: 4101, at: "2026-10-01T10:00:00Z"},
		"team:acme/backend": {team: "acme/backend", source: "comment", commenter: "dependabot[bot]", commentID: 4103, at: "2026-10-01T10:30:00Z"},
		"user:erin":         {login: "erin", source: "review", commenter: "dave", commentID: 5101, at: "2026-10-01T10:10:00Z"},
		"user:gina":         {login: "gina", source: "body", at: "2026-10-01T12:00:00Z"},
	})
	if snap := storedPRSnapshot(t, stores, entityID); strings.Join(snap.BodyMentions, ",") != "stranger,gina" {
		t.Errorf("stored body_mentions = %q, want [stranger gina]", snap.BodyMentions)
	}

	before := len(pub.nonSystemEvents())
	if _, _, err := tr.RefreshGitHub(ctx, reviewTestHost, client, "", nil, mentionResolver); err != nil {
		t.Fatalf("second RefreshGitHub: %v", err)
	}
	if after := pub.nonSystemEvents(); len(after) != before {
		t.Errorf("quiet poll emitted %v", eventTypes(after[before:]))
	}
}

// A batch response without the body keeps the stored body mentions, as it
// keeps the body's fingerprint, so the next response with the body diffs
// against them rather than taking its mentions as a new baseline.
func TestRefreshGitHub_KeepsBodyMentionsWithoutTheBody(t *testing.T) {
	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	entityID := seedReviewedPR(t, stores, domain.PRSnapshot{BodyHash: "h", BodyMentions: []string{"bob"}})
	reads := 0
	srv := commentRefreshServer(t, `{"totalCount":0,"nodes":[]}`, `{}`, &reads)

	tr := New(database, &recordingPublisher{}, stores.Tasks, stores.Entities, stores.Repos, stores.Artifacts, stores.EventQueue, runmode.LocalDefaultOrgID)
	if _, _, err := tr.RefreshGitHub(ctx, reviewTestHost, ghclient.NewClient(srv.URL, "tok"), "", nil, mentionResolver); err != nil {
		t.Fatalf("RefreshGitHub: %v", err)
	}
	if snap := storedPRSnapshot(t, stores, entityID); len(snap.BodyMentions) != 1 || snap.BodyMentions[0] != "bob" || snap.BodyHash != "h" {
		t.Errorf("stored body = (%q, %q), want the previous fingerprint and mentions kept", snap.BodyHash, snap.BodyMentions)
	}
}
