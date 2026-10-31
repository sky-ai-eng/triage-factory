package tracker

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// commentedMeta is the commented metadata of either source, read through each
// source's own field names.
type commentedMeta struct {
	CommentID string
	Commenter string
	AuthorID  string
	AuthorIDs []string
}

// commentedSource diffs a Jira or Linear snapshot pair into its commented
// event and reads the metadata back.
type commentedSource struct {
	name      string
	eventType string
	diff      func(comments []NewComment) []domain.Event
	meta      func(t *testing.T, evt domain.Event) commentedMeta
}

func commentedSources() []commentedSource {
	return []commentedSource{
		{
			name:      "jira",
			eventType: domain.EventJiraIssueCommented,
			diff: func(comments []NewComment) []domain.Event {
				snap := domain.JiraSnapshot{ID: "10001", Key: "SKY-1", Status: "To Do", UpdatedAt: "2026-10-01T10:00:00.000+0000"}
				return DiffJiraSnapshots(snap, snap, testEntityID, JiraEventProjects{}, testDoneStatuses, comments)
			},
			meta: func(t *testing.T, evt domain.Event) commentedMeta {
				var m struct {
					CommentID string   `json:"comment_id"`
					Commenter string   `json:"commenter"`
					AuthorID  string   `json:"commenter_account_id"`
					AuthorIDs []string `json:"commenter_account_ids"`
				}
				if err := json.Unmarshal([]byte(evt.MetadataJSON), &m); err != nil {
					t.Fatal(err)
				}
				return commentedMeta(m)
			},
		},
		{
			name:      "linear",
			eventType: domain.EventLinearIssueCommented,
			diff: func(comments []NewComment) []domain.Event {
				return DiffLinearSnapshots(linSnap(), linSnap(), testEntityID, linDoneSet, comments, nil)
			},
			meta: func(t *testing.T, evt domain.Event) commentedMeta {
				var m struct {
					CommentID string   `json:"comment_id"`
					Commenter string   `json:"commenter"`
					AuthorID  string   `json:"commenter_user_id"`
					AuthorIDs []string `json:"commenter_user_ids"`
				}
				if err := json.Unmarshal([]byte(evt.MetadataJSON), &m); err != nil {
					t.Fatal(err)
				}
				return commentedMeta(m)
			},
		},
	}
}

// TestDiffCommented covers the commented branch of both diffs: comments TF
// posted are not a reason to emit, a mix is attributed to the newest comment
// a person wrote, and every person who commented is in the list commenter_in
// matches against.
func TestDiffCommented(t *testing.T) {
	ada := func(id, at string) NewComment {
		return NewComment{ID: id, Author: "Ada", AuthorID: "id-ada", CreatedAt: at}
	}
	bob := func(id, at string) NewComment {
		return NewComment{ID: id, Author: "Bob", AuthorID: "id-bob", CreatedAt: at}
	}
	tf := func(id, at string) NewComment {
		return NewComment{ID: id, Author: "Ada", AuthorID: "id-ada", CreatedAt: at, ByTF: true}
	}
	cases := []struct {
		name     string
		comments []NewComment
		want     *commentedMeta // nil: no event
		wantAt   string
	}{
		{name: "no new comments"},
		{name: "new comments by TF only",
			comments: []NewComment{tf("1", "2026-10-01T09:00:00.000Z"), tf("2", "2026-10-01T09:05:00.000Z")}},
		{name: "a person's comment and TF's after it",
			comments: []NewComment{ada("1", "2026-10-01T09:00:00.000Z"), tf("2", "2026-10-01T09:05:00.000Z")},
			want:     &commentedMeta{CommentID: "1", Commenter: "Ada", AuthorID: "id-ada", AuthorIDs: []string{"id-ada"}},
			wantAt:   "2026-10-01T09:00:00.000Z"},
		{name: "several comments by people",
			comments: []NewComment{
				ada("1", "2026-10-01T09:00:00.000Z"), bob("2", "2026-10-01T09:01:00.000Z"),
				tf("3", "2026-10-01T09:02:00.000Z"), ada("4", "2026-10-01T09:03:00.000Z"),
			},
			want:   &commentedMeta{CommentID: "4", Commenter: "Ada", AuthorID: "id-ada", AuthorIDs: []string{"id-ada", "id-bob"}},
			wantAt: "2026-10-01T09:03:00.000Z"},
	}
	for _, src := range commentedSources() {
		for _, tc := range cases {
			t.Run(src.name+"/"+tc.name, func(t *testing.T) {
				evts := src.diff(tc.comments)
				evt := findEvent(evts, src.eventType)
				if tc.want == nil {
					if evt != nil {
						t.Fatalf("emitted %s: %s, want nothing", src.eventType, evt.MetadataJSON)
					}
					return
				}
				if evt == nil {
					t.Fatalf("no %s event in %v", src.eventType, eventTypes(evts))
				}
				if got := src.meta(t, *evt); !commentedMetaEqual(got, *tc.want) {
					t.Errorf("metadata = %+v, want %+v", got, *tc.want)
				}
				if wantAt, _ := time.Parse(time.RFC3339Nano, tc.wantAt); !evt.OccurredAt.Equal(wantAt) {
					t.Errorf("occurred_at = %v, want the newest person's comment at %v", evt.OccurredAt, wantAt)
				}
				for _, id := range tc.want.AuthorIDs {
					if !matchesPredicate(t, *evt, allowlist(t, "commenter_in", id)) {
						t.Errorf("commenter_in naming %s did not match", id)
					}
				}
				if matchesPredicate(t, *evt, allowlist(t, "commenter_in", "id-carol")) {
					t.Error("commenter_in naming someone who did not comment matched")
				}
			})
		}
	}
}

func commentedMetaEqual(a, b commentedMeta) bool {
	return a.CommentID == b.CommentID && a.Commenter == b.Commenter && a.AuthorID == b.AuthorID && slices.Equal(a.AuthorIDs, b.AuthorIDs)
}

// TestDiffLinearCommented_IntegrationComment: a comment an integration posted
// names the integration and carries no user id, so no commenter_in allowlist
// can match it.
func TestDiffLinearCommented_IntegrationComment(t *testing.T) {
	evts := DiffLinearSnapshots(linSnap(), linSnap(), testEntityID, linDoneSet, []NewComment{
		linearComment(linear.Comment{ID: "c-1", CreatedAt: "2026-10-01T09:00:00.000Z", BotActor: &linear.BotActor{Name: "GitHub"}}),
	}, nil)
	evt := findEvent(evts, domain.EventLinearIssueCommented)
	if evt == nil {
		t.Fatalf("no commented event in %v", eventTypes(evts))
	}
	got := commentedSources()[1].meta(t, *evt)
	if want := (commentedMeta{CommentID: "c-1", Commenter: "GitHub", AuthorIDs: []string{}}); !commentedMetaEqual(got, want) {
		t.Errorf("metadata = %+v, want %+v", got, want)
	}
	if matchesPredicate(t, *evt, allowlist(t, "commenter_in", "u-viewer")) {
		t.Error("commenter_in matched an integration's comment")
	}
}

// TestJiraCommentsSince covers which of an issue's comments are new: the ones
// after the stored marker, compared by time and then by id, or, for a snapshot
// stored without one, the last ones by the amount the count rose.
func TestJiraCommentsSince(t *testing.T) {
	known := []NewComment{
		{ID: "10100", CreatedAt: "2026-10-01T09:00:00.000+0000"},
		{ID: "10101", CreatedAt: "2026-10-01T09:05:00.000+0000"},
		{ID: "10102", CreatedAt: "2026-10-01T09:05:00.000+0000"},
		{ID: "10103", CreatedAt: "2026-10-01T09:10:00.000+0000"},
	}
	ids := func(cs []NewComment) []string {
		out := []string{}
		for _, c := range cs {
			out = append(out, c.ID)
		}
		return out
	}
	cases := []struct {
		name  string
		prev  domain.JiraSnapshot
		total int
		want  []string
	}{
		{name: "after the marker",
			prev:  domain.JiraSnapshot{CommentCount: 1, LastCommentID: "10100", LastCommentAt: "2026-10-01T09:00:00.000+0000"},
			total: 4, want: []string{"10101", "10102", "10103"}},
		{name: "a comment at the marker's time is after it by id",
			prev:  domain.JiraSnapshot{CommentCount: 2, LastCommentID: "10101", LastCommentAt: "2026-10-01T09:05:00.000+0000"},
			total: 4, want: []string{"10102", "10103"}},
		{name: "the marker comment was deleted",
			prev:  domain.JiraSnapshot{CommentCount: 2, LastCommentID: "10099", LastCommentAt: "2026-10-01T09:01:00.000+0000"},
			total: 4, want: []string{"10101", "10102", "10103"}},
		{name: "nothing after the marker",
			prev:  domain.JiraSnapshot{CommentCount: 4, LastCommentID: "10103", LastCommentAt: "2026-10-01T09:10:00.000+0000"},
			total: 4, want: []string{}},
		{name: "no marker: the last ones by the count's rise",
			prev:  domain.JiraSnapshot{CommentCount: 2},
			total: 4, want: []string{"10102", "10103"}},
		{name: "no marker and the count fell",
			prev:  domain.JiraSnapshot{CommentCount: 5},
			total: 4, want: []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ids(jiraCommentsSince(tc.prev, tc.total, known)); !slices.Equal(got, tc.want) {
				t.Errorf("new comments = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- Jira refresh ----------------------------------------------------------

const jiraCommentSite = "https://jira.example.com"

// jiraCommentIssue is one Jira issue as a test server serves it: its
// comments, oldest first, and how many of them the search returns with the
// issue (0 for all). Adding or removing a comment advances the issue's updated
// time, as Jira does. failComments makes the comment-list endpoint answer 403.
type jiraCommentIssue struct {
	mu           sync.Mutex
	comments     []string // rendered comment objects
	changes      int      // how many times a comment was added or removed
	updated      string
	inline       int
	failComments bool
	listReads    []string // "startAt/maxResults" of each comment-list read
}

// change advances the issue's clock by a minute and returns the new time.
func (j *jiraCommentIssue) change() string {
	j.updated = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC).Add(time.Duration(j.changes) * time.Minute).Format("2006-01-02T15:04:05.000-0700")
	j.changes++
	return j.updated
}

func (j *jiraCommentIssue) add(id, accountID, name string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	at := j.change()
	j.comments = append(j.comments, fmt.Sprintf(`{"id":%q,"author":{"displayName":%q,"accountId":%q},"created":%q}`, id, name, accountID, at))
}

func (j *jiraCommentIssue) remove(id string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.comments = slices.DeleteFunc(j.comments, func(c string) bool { return strings.HasPrefix(c, fmt.Sprintf(`{"id":%q,`, id)) })
	j.change()
}

func (j *jiraCommentIssue) reads() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Clone(j.listReads)
}

func (j *jiraCommentIssue) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		j.mu.Lock()
		defer j.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
			page := j.comments
			if j.inline > 0 && len(page) > j.inline {
				page = page[:j.inline]
			}
			fmt.Fprintf(w, `{"issues":[{"id":"10001","key":"SKY-1","fields":{
				"summary":"Commented subject","status":{"id":"1","name":"To Do"},
				"assignee":{"displayName":"Alice","accountId":"acc-alice"},
				"comment":{"total":%d,"comments":[%s]},
				"updated":%q}}]}`, len(j.comments), strings.Join(page, ","), cmp.Or(j.updated, "2026-10-01T08:00:00.000+0000"))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issue/10001/comment"):
			start, _ := strconv.Atoi(r.URL.Query().Get("startAt"))
			limit, _ := strconv.Atoi(r.URL.Query().Get("maxResults"))
			j.listReads = append(j.listReads, fmt.Sprintf("%d/%d", start, limit))
			if j.failComments {
				http.Error(w, `{"errorMessages":["forbidden"]}`, http.StatusForbidden)
				return
			}
			end := min(len(j.comments), start+limit)
			fmt.Fprintf(w, `{"startAt":%d,"maxResults":%d,"total":%d,"comments":[%s]}`,
				start, limit, len(j.comments), strings.Join(j.comments[min(start, end):end], ","))
		default:
			t.Errorf("unexpected jira request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

type jiraCommentFixture struct {
	issue  *jiraCommentIssue
	srv    *httptest.Server
	tr     *Tracker
	pub    *recordingPublisher
	stores db.Stores
}

// newJiraCommentFixture tracks SKY-1 and runs the cycle that seeds it, so the
// next cycle diffs against a snapshot holding whatever comments were added
// before this call.
func newJiraCommentFixture(t *testing.T, issue *jiraCommentIssue) *jiraCommentFixture {
	t.Helper()
	srv := issue.serve(t)
	tr, pub, stores := jiraRefreshFixture(t, srv)
	fx := &jiraCommentFixture{issue: issue, srv: srv, tr: tr, pub: pub, stores: stores}
	if evts := fx.cycle(t); len(evts) != 0 {
		t.Fatalf("seed cycle emitted %v, want a quiet seed", eventTypes(evts))
	}
	return fx
}

// cycle runs one refresh and returns the events it emitted.
func (fx *jiraCommentFixture) cycle(t *testing.T) []domain.Event {
	t.Helper()
	fx.pub = &recordingPublisher{}
	fx.tr.pub = fx.pub
	client := jiraclient.NewClient(jiraclient.DataCenterPAT(fx.srv.URL, "pat"))
	if _, err := fx.tr.RefreshJira(context.Background(), jiraCommentSite, client, fx.srv.URL, nil); err != nil {
		t.Fatalf("RefreshJira: %v", err)
	}
	return fx.pub.nonSystemEvents()
}

// postedByTF records commentID as TF's, under the key the agent's
// `jira ticket comment` verb records it with.
func (fx *jiraCommentFixture) postedByTF(t *testing.T, commentID string) {
	t.Helper()
	site, _ := domain.JiraHost(jiraCommentSite)
	key := domain.ObjectKey(domain.ProviderJira, domain.DeliverableKindComment, domain.JiraIssueResource(site, "10001"), commentID)
	recordTFComment(t, fx.stores, domain.ProviderJira, site, "SKY-1", key)
}

func (fx *jiraCommentFixture) snapshot(t *testing.T) domain.JiraSnapshot {
	t.Helper()
	e, err := fx.stores.Entities.GetBySource(context.Background(), runmode.LocalDefaultOrgID, "jira", jiraCommentSite, "SKY-1")
	if err != nil || e == nil {
		t.Fatalf("GetBySource: %v %v", e, err)
	}
	var snap domain.JiraSnapshot
	if err := json.Unmarshal([]byte(e.SnapshotJSON), &snap); err != nil {
		t.Fatal(err)
	}
	return snap
}

func onlyCommented(t *testing.T, evts []domain.Event, eventType string) domain.Event {
	t.Helper()
	if len(evts) != 1 || evts[0].EventType != eventType {
		t.Fatalf("events = %v, want one %s", eventTypes(evts), eventType)
	}
	return evts[0]
}

// TestRefreshJira_CommentedNamesTheCommenter: a person's comment produces one
// commented event naming them, commenter_in matches them and nobody else, and
// the next poll does not report it again.
func TestRefreshJira_CommentedNamesTheCommenter(t *testing.T) {
	fx := newJiraCommentFixture(t, &jiraCommentIssue{})
	fx.issue.add("20001", "557058:ada", "Ada Lovelace")

	evt := onlyCommented(t, fx.cycle(t), domain.EventJiraIssueCommented)
	got := commentedSources()[0].meta(t, evt)
	want := commentedMeta{CommentID: "20001", Commenter: "Ada Lovelace", AuthorID: "557058:ada", AuthorIDs: []string{"557058:ada"}}
	if !commentedMetaEqual(got, want) {
		t.Errorf("metadata = %+v, want %+v", got, want)
	}
	if !matchesPredicate(t, evt, allowlist(t, "commenter_in", "557058:ada")) {
		t.Error("commenter_in naming the commenter did not match")
	}
	if matchesPredicate(t, evt, allowlist(t, "commenter_in", "557058:bob")) {
		t.Error("commenter_in naming someone else matched")
	}
	if snap := fx.snapshot(t); snap.LastCommentID != "20001" || snap.CommentCount != 1 {
		t.Errorf("snapshot marker = (%q, %d), want (20001, 1)", snap.LastCommentID, snap.CommentCount)
	}
	if evts := fx.cycle(t); len(evts) != 0 {
		t.Errorf("the next poll emitted %v, want nothing", eventTypes(evts))
	}
}

// TestRefreshJira_TFCommentsEmitNothing: a poll that finds only TF's comment
// emits nothing, and one that finds a person's comment followed by TF's emits
// one event, attributed to the person.
func TestRefreshJira_TFCommentsEmitNothing(t *testing.T) {
	fx := newJiraCommentFixture(t, &jiraCommentIssue{})

	fx.issue.add("20001", "557058:alice", "Alice")
	fx.postedByTF(t, "20001")
	if evts := fx.cycle(t); len(evts) != 0 {
		t.Fatalf("TF's own comment emitted %v, want nothing", eventTypes(evts))
	}

	fx.issue.add("20002", "557058:ada", "Ada Lovelace")
	fx.issue.add("20003", "557058:alice", "Alice")
	fx.postedByTF(t, "20003")
	evt := onlyCommented(t, fx.cycle(t), domain.EventJiraIssueCommented)
	got := commentedSources()[0].meta(t, evt)
	if want := (commentedMeta{CommentID: "20002", Commenter: "Ada Lovelace", AuthorID: "557058:ada", AuthorIDs: []string{"557058:ada"}}); !commentedMetaEqual(got, want) {
		t.Errorf("metadata = %+v, want %+v", got, want)
	}
}

// TestRefreshJira_SnapshotWithoutMarkerCountsNewComments: a snapshot stored
// before the marker existed has only its count, so the new comments are the
// last ones by the amount it rose. The comment that was already there is not
// reported.
func TestRefreshJira_SnapshotWithoutMarkerCountsNewComments(t *testing.T) {
	issue := &jiraCommentIssue{}
	issue.add("20001", "557058:carol", "Carol")
	fx := newJiraCommentFixture(t, issue)

	e, err := fx.stores.Entities.GetBySource(context.Background(), runmode.LocalDefaultOrgID, "jira", jiraCommentSite, "SKY-1")
	if err != nil || e == nil {
		t.Fatalf("GetBySource: %v %v", e, err)
	}
	legacy := fx.snapshot(t)
	legacy.LastCommentID, legacy.LastCommentAt = "", ""
	raw, _ := json.Marshal(legacy)
	if ok, err := fx.stores.Entities.UpdateSnapshotCASSystem(context.Background(), runmode.LocalDefaultOrgID, e.ID, string(raw), e.PollSeq); err != nil || !ok {
		t.Fatalf("store the markerless snapshot: ok=%v err=%v", ok, err)
	}

	fx.issue.add("20002", "557058:ada", "Ada Lovelace")
	fx.issue.add("20003", "557058:bob", "Bob")
	evt := onlyCommented(t, fx.cycle(t), domain.EventJiraIssueCommented)
	got := commentedSources()[0].meta(t, evt)
	if want := (commentedMeta{CommentID: "20003", Commenter: "Bob", AuthorID: "557058:bob", AuthorIDs: []string{"557058:ada", "557058:bob"}}); !commentedMetaEqual(got, want) {
		t.Errorf("metadata = %+v, want %+v", got, want)
	}
	if snap := fx.snapshot(t); snap.LastCommentID != "20003" {
		t.Errorf("snapshot marker = %q, want 20003 recorded from this poll", snap.LastCommentID)
	}
}

// TestRefreshJira_ReadsCommentsPastTheInlinePage: when the issue has more
// comments than the search returns with it, the rest are read from the
// comment list, and only when the count moved.
func TestRefreshJira_ReadsCommentsPastTheInlinePage(t *testing.T) {
	issue := &jiraCommentIssue{inline: 2}
	issue.add("20001", "557058:carol", "Carol")
	issue.add("20002", "557058:carol", "Carol")
	fx := newJiraCommentFixture(t, issue)
	if snap := fx.snapshot(t); snap.LastCommentID != "20002" {
		t.Fatalf("seed marker = %q, want 20002 (the page held every comment)", snap.LastCommentID)
	}

	fx.issue.add("20003", "557058:ada", "Ada Lovelace")
	fx.issue.add("20004", "557058:alice", "Alice")
	fx.postedByTF(t, "20004")
	fx.issue.add("20005", "557058:bob", "Bob")
	evt := onlyCommented(t, fx.cycle(t), domain.EventJiraIssueCommented)
	got := commentedSources()[0].meta(t, evt)
	if want := (commentedMeta{CommentID: "20005", Commenter: "Bob", AuthorID: "557058:bob", AuthorIDs: []string{"557058:ada", "557058:bob"}}); !commentedMetaEqual(got, want) {
		t.Errorf("metadata = %+v, want %+v", got, want)
	}
	if reads := fx.issue.reads(); !slices.Equal(reads, []string{"2/3"}) {
		t.Errorf("comment-list reads = %v, want the three past the page", reads)
	}
	if snap := fx.snapshot(t); snap.LastCommentID != "20005" || snap.CommentCount != 5 {
		t.Errorf("snapshot marker = (%q, %d), want (20005, 5)", snap.LastCommentID, snap.CommentCount)
	}

	if evts := fx.cycle(t); len(evts) != 0 {
		t.Errorf("an unchanged issue emitted %v", eventTypes(evts))
	}
	if reads := fx.issue.reads(); len(reads) != 1 {
		t.Errorf("comment-list reads after an unchanged poll = %v, want no new read", reads)
	}
	if snap := fx.snapshot(t); snap.LastCommentID != "20005" {
		t.Errorf("snapshot marker after an unchanged poll = %q, want 20005 carried over", snap.LastCommentID)
	}
}

// TestRefreshJira_CommentAddedAndAnotherDeletedPastThePage: on an issue with
// more comments than the search page holds, a comment added and another
// deleted between two polls leave the count where it was. The issue's updated
// time still moves, so the comments past the page are read and the new one is
// reported.
func TestRefreshJira_CommentAddedAndAnotherDeletedPastThePage(t *testing.T) {
	issue := &jiraCommentIssue{inline: 1}
	issue.add("20001", "557058:carol", "Carol")
	issue.add("20002", "557058:carol", "Carol")
	fx := newJiraCommentFixture(t, issue)
	fx.issue.add("20003", "557058:carol", "Carol")
	onlyCommented(t, fx.cycle(t), domain.EventJiraIssueCommented)
	if snap := fx.snapshot(t); snap.LastCommentID != "20003" || snap.CommentCount != 3 {
		t.Fatalf("snapshot marker = (%q, %d), want (20003, 3)", snap.LastCommentID, snap.CommentCount)
	}

	fx.issue.add("20004", "557058:ada", "Ada Lovelace")
	fx.issue.remove("20002")
	evt := onlyCommented(t, fx.cycle(t), domain.EventJiraIssueCommented)
	got := commentedSources()[0].meta(t, evt)
	if want := (commentedMeta{CommentID: "20004", Commenter: "Ada Lovelace", AuthorID: "557058:ada", AuthorIDs: []string{"557058:ada"}}); !commentedMetaEqual(got, want) {
		t.Errorf("metadata = %+v, want %+v", got, want)
	}
	if snap := fx.snapshot(t); snap.LastCommentID != "20004" || snap.CommentCount != 3 {
		t.Errorf("snapshot marker = (%q, %d), want (20004, 3)", snap.LastCommentID, snap.CommentCount)
	}
}

// TestRefreshJira_CommentTailStopsAtTheMarker: the comments past the page are
// read from the end backwards, a page at a time, and the read stops at the
// first page that reaches the marker.
func TestRefreshJira_CommentTailStopsAtTheMarker(t *testing.T) {
	issue := &jiraCommentIssue{inline: 2}
	for i := range 2 * jiraCommentPageSize {
		issue.add(strconv.Itoa(30000+i), "557058:carol", "Carol")
	}
	fx := newJiraCommentFixture(t, issue)
	// The seed's page did not hold every comment, so it recorded no marker;
	// the read that settles one comes with the first change of count.
	fx.issue.add("31000", "557058:ada", "Ada Lovelace")
	onlyCommented(t, fx.cycle(t), domain.EventJiraIssueCommented)
	if snap := fx.snapshot(t); snap.LastCommentID != "31000" {
		t.Fatalf("snapshot marker = %q, want 31000", snap.LastCommentID)
	}

	before := len(fx.issue.reads())
	fx.issue.add("31001", "557058:bob", "Bob")
	evt := onlyCommented(t, fx.cycle(t), domain.EventJiraIssueCommented)
	if got := commentedSources()[0].meta(t, evt); !slices.Equal(got.AuthorIDs, []string{"557058:bob"}) {
		t.Errorf("commenters = %v, want Bob alone", got.AuthorIDs)
	}
	total := 2*jiraCommentPageSize + 2
	want := fmt.Sprintf("%d/%d", total-jiraCommentPageSize, jiraCommentPageSize)
	if reads := fx.issue.reads()[before:]; !slices.Equal(reads, []string{want}) {
		t.Errorf("comment-list reads = %v, want the last page alone (%s)", reads, want)
	}
}

// TestRefreshJira_FailedCommentReadIsRetried: when the comments past the page
// cannot be read, nothing is reported and the marker stays, so the next poll
// that can read them reports them.
func TestRefreshJira_FailedCommentReadIsRetried(t *testing.T) {
	issue := &jiraCommentIssue{inline: 1}
	issue.add("20001", "557058:carol", "Carol")
	fx := newJiraCommentFixture(t, issue)

	fx.issue.add("20002", "557058:ada", "Ada Lovelace")
	fx.issue.failComments = true
	if evts := fx.cycle(t); len(evts) != 0 {
		t.Fatalf("a failed comment read emitted %v", eventTypes(evts))
	}
	if snap := fx.snapshot(t); snap.CommentCount != 1 || snap.LastCommentID != "20001" {
		t.Errorf("snapshot after a failed read = (%q, %d), want prev's (20001, 1)", snap.LastCommentID, snap.CommentCount)
	}

	fx.issue.failComments = false
	evt := onlyCommented(t, fx.cycle(t), domain.EventJiraIssueCommented)
	if got := commentedSources()[0].meta(t, evt); got.CommentID != "20002" {
		t.Errorf("comment_id = %q, want 20002", got.CommentID)
	}
}

// --- Linear refresh --------------------------------------------------------

// linComment is a comment the fake client serves, created n minutes after a
// fixed time. An empty userID makes it an integration's.
func linComment(id string, n int, userID, name string) linear.Comment {
	c := linear.Comment{ID: id, CreatedAt: time.Date(2026, 10, 1, 9, n, 0, 0, time.UTC).Format("2006-01-02T15:04:05.000Z")}
	if userID == "" {
		c.BotActor = &linear.BotActor{Name: name}
	} else {
		c.User = &linear.User{ID: userID, Name: name}
	}
	return c
}

// addLinComments appends comments to issue n in the fake, and makes the
// newest the issue's last comment.
func (fx *linearFixture) addComments(n int, cs ...linear.Comment) {
	is := linIssue(n)
	if prev, ok := fx.client.byID[is.ID]; ok {
		is = prev
	}
	fx.client.comments[is.ID] = append(fx.client.comments[is.ID], cs...)
	last := cs[len(cs)-1]
	is.LastComment = &last
	fx.client.put(is)
}

// postedByTF records commentID on issue n as TF's, under the key the agent's
// `linear issue comment` verb records it with.
func (fx *linearFixture) postedByTF(t *testing.T, n int, commentID string) {
	t.Helper()
	key := domain.ObjectKey(domain.ProviderLinear, domain.DeliverableKindComment, linIssue(n).ID, commentID)
	recordTFComment(t, fx.stores, domain.ProviderLinear, fx.workspace, linIssue(n).Identifier, key)
}

// TestRefreshLinear_Commented covers who a Linear commented event names, and
// that TF's own comments are not a reason to emit one.
func TestRefreshLinear_Commented(t *testing.T) {
	t.Run("a person's comment", func(t *testing.T) {
		fx := newLinearFixture(t)
		fx.seed(t, linIssue(1))
		fx.addComments(1, linComment("c-1", 1, "u-ada", "Ada"))

		evts, err := fx.cycle(t, linRules())
		if err != nil {
			t.Fatal(err)
		}
		evt := onlyCommented(t, evts, domain.EventLinearIssueCommented)
		got := commentedSources()[1].meta(t, evt)
		if want := (commentedMeta{CommentID: "c-1", Commenter: "Ada", AuthorID: "u-ada", AuthorIDs: []string{"u-ada"}}); !commentedMetaEqual(got, want) {
			t.Errorf("metadata = %+v, want %+v", got, want)
		}
		if !matchesPredicate(t, evt, allowlist(t, "commenter_in", "u-ada")) {
			t.Error("commenter_in naming the commenter did not match")
		}
		if matchesPredicate(t, evt, allowlist(t, "commenter_in", "u-bob")) {
			t.Error("commenter_in naming someone else matched")
		}
		if evts, _ := fx.cycle(t, linRules()); len(evts) != 0 {
			t.Errorf("the next poll emitted %v", eventTypes(evts))
		}
	})

	t.Run("TF's comment alone", func(t *testing.T) {
		fx := newLinearFixture(t)
		fx.seed(t, linIssue(1))
		fx.addComments(1, linComment("c-1", 1, "u-viewer", "Viewer"))
		fx.postedByTF(t, 1, "c-1")
		if evts, err := fx.cycle(t, linRules()); err != nil || len(evts) != 0 {
			t.Errorf("TF's own comment: events=%v err=%v, want nothing", eventTypes(evts), err)
		}
	})

	t.Run("a person's comment and TF's after it", func(t *testing.T) {
		fx := newLinearFixture(t)
		fx.seed(t, linIssue(1))
		fx.addComments(1, linComment("c-1", 1, "u-ada", "Ada"), linComment("c-2", 2, "u-viewer", "Viewer"))
		fx.postedByTF(t, 1, "c-2")
		evts, err := fx.cycle(t, linRules())
		if err != nil {
			t.Fatal(err)
		}
		got := commentedSources()[1].meta(t, onlyCommented(t, evts, domain.EventLinearIssueCommented))
		if want := (commentedMeta{CommentID: "c-1", Commenter: "Ada", AuthorID: "u-ada", AuthorIDs: []string{"u-ada"}}); !commentedMetaEqual(got, want) {
			t.Errorf("metadata = %+v, want %+v", got, want)
		}
	})

	t.Run("several people since the last poll", func(t *testing.T) {
		fx := newLinearFixture(t)
		is := linIssue(1)
		old := linComment("c-0", 0, "u-carol", "Carol")
		is.LastComment = &old
		fx.client.comments[is.ID] = []linear.Comment{old}
		fx.seed(t, is)
		fx.addComments(1, linComment("c-1", 1, "u-ada", "Ada"), linComment("c-2", 2, "", "GitHub"), linComment("c-3", 3, "u-bob", "Bob"))

		evts, err := fx.cycle(t, linRules())
		if err != nil {
			t.Fatal(err)
		}
		got := commentedSources()[1].meta(t, onlyCommented(t, evts, domain.EventLinearIssueCommented))
		if want := (commentedMeta{CommentID: "c-3", Commenter: "Bob", AuthorID: "u-bob", AuthorIDs: []string{"u-ada", "u-bob"}}); !commentedMetaEqual(got, want) {
			t.Errorf("metadata = %+v, want %+v (Carol's comment predates the poll)", got, want)
		}
	})

	t.Run("comment times in different offsets", func(t *testing.T) {
		// 10:00+01:00 is 09:00Z: Bob's comment is the older one, though its
		// text sorts after Ada's.
		fx := newLinearFixture(t)
		fx.seed(t, linIssue(1))
		is := fx.client.byID[linIssue(1).ID]
		bob := linear.Comment{ID: "c-1", CreatedAt: "2026-10-01T10:00:00.000+01:00", User: &linear.User{ID: "u-bob", Name: "Bob"}}
		ada := linear.Comment{ID: "c-2", CreatedAt: "2026-10-01T09:30:00.000Z", User: &linear.User{ID: "u-ada", Name: "Ada"}}
		fx.client.comments[is.ID] = []linear.Comment{ada, bob}
		is.LastComment = &ada
		fx.client.put(is)

		evts, err := fx.cycle(t, linRules())
		if err != nil {
			t.Fatal(err)
		}
		got := commentedSources()[1].meta(t, onlyCommented(t, evts, domain.EventLinearIssueCommented))
		if want := (commentedMeta{CommentID: "c-2", Commenter: "Ada", AuthorID: "u-ada", AuthorIDs: []string{"u-bob", "u-ada"}}); !commentedMetaEqual(got, want) {
			t.Errorf("metadata = %+v, want %+v (ordered by the moment, not the text)", got, want)
		}
	})

	t.Run("the newest comment deleted", func(t *testing.T) {
		fx := newLinearFixture(t)
		is := linIssue(1)
		newest := linComment("c-2", 2, "u-ada", "Ada")
		is.LastComment = &newest
		fx.seed(t, is)
		older := linComment("c-1", 1, "u-ada", "Ada")
		is.LastComment = &older
		fx.client.put(is)
		if evts, err := fx.cycle(t, linRules()); err != nil || len(evts) != 0 {
			t.Errorf("deleting the newest comment: events=%v err=%v, want nothing", eventTypes(evts), err)
		}
		if slices.ContainsFunc(fx.client.callLog(), func(c string) bool { return strings.HasPrefix(c, "comments:") }) {
			t.Error("a deleted newest comment led to a comment read")
		}
	})
}

// TestRefreshLinear_FailedCommentReadIsRetried: a failed comment read keeps
// the stored newest comment, so the next poll reports the comments; a rate
// limit on it ends the cycle.
func TestRefreshLinear_FailedCommentReadIsRetried(t *testing.T) {
	fx := newLinearFixture(t)
	fx.seed(t, linIssue(1))
	fx.addComments(1, linComment("c-1", 1, "u-ada", "Ada"))

	fx.client.fail = func(call string, _ int) error {
		if strings.HasPrefix(call, "comments:") {
			return errors.New("linear: connection reset")
		}
		return nil
	}
	if evts, err := fx.cycle(t, linRules()); err != nil || len(evts) != 0 {
		t.Fatalf("failed comment read: events=%v err=%v, want nothing and no error", eventTypes(evts), err)
	}
	if snap := fx.snapshot(t, "ENG-1"); snap.LastCommentID != "" {
		t.Errorf("stored newest comment after a failed read = %q, want prev's (none)", snap.LastCommentID)
	}

	fx.client.fail = func(call string, _ int) error {
		if strings.HasPrefix(call, "comments:") {
			return rateLimited(time.Now().Add(time.Minute))
		}
		return nil
	}
	if _, err := fx.cycle(t, linRules()); !errors.Is(err, linear.ErrRateLimited) {
		t.Fatalf("rate-limited comment read: err = %v, want ErrRateLimited", err)
	}
	if fx.pollCompleted() {
		t.Error("a rate-limited cycle reported a completed poll")
	}

	fx.client.fail = nil
	evts, err := fx.cycle(t, linRules())
	if err != nil {
		t.Fatal(err)
	}
	onlyCommented(t, evts, domain.EventLinearIssueCommented)
}

// TestMarkTFComments_ReadsOnlyItsScope: a comment deliverable recorded in another
// workspace does not make a comment TF's.
func TestMarkTFComments_ReadsOnlyItsScope(t *testing.T) {
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	tr := New(database, &recordingPublisher{}, stores.Tasks, stores.Entities, stores.Repos, stores.JiraProjects, stores.Deliverables, stores.ExternalActions, stores.EventQueue, stores.PRMergeability, runmode.LocalDefaultOrgID)
	fx := &linearFixture{tr: tr, stores: stores, workspace: "ws-other"}
	fx.postedByTF(t, 1, "c-1")

	comments := []NewComment{{ID: "c-1"}}
	if err := tr.markTFComments(context.Background(), linWorkspace, comments, issueCommentKey(domain.ProviderLinear, linIssue(1).ID)); err != nil {
		t.Fatal(err)
	}
	if comments[0].ByTF {
		t.Error("a comment recorded in another workspace was taken as TF's")
	}
}

// TestIssueToState_Comments decodes an issue's inline comments, authors by the
// stable-id rule assignee_in follows, and records the newest as the marker
// only when the page holds every comment.
func TestIssueToState_Comments(t *testing.T) {
	decode := func(t *testing.T, total int) jiraIssueState {
		t.Helper()
		raw := fmt.Sprintf(`{"id":"10001","key":"SKY-1","fields":{"comment":{"total":%d,"comments":[
			{"id":"20002","author":{"displayName":"Ada","key":"JIRAUSER10200","name":"ada"},"created":"2026-10-01T09:05:00.000+0000"},
			{"id":"20001","author":{"displayName":"Bob","accountId":"557058:bob"},"created":"2026-10-01T09:00:00.000+0000"}
		]}}}`, total)
		var issue jiraclient.Issue
		if err := json.Unmarshal([]byte(raw), &issue); err != nil {
			t.Fatal(err)
		}
		return issueToState(issue, jiraCommentSite, nil)
	}

	state := decode(t, 2)
	want := []NewComment{
		{ID: "20001", Author: "Bob", AuthorID: "557058:bob", CreatedAt: "2026-10-01T09:00:00.000+0000"},
		{ID: "20002", Author: "Ada", AuthorID: "JIRAUSER10200", CreatedAt: "2026-10-01T09:05:00.000+0000"},
	}
	if !reflect.DeepEqual(state.Comments, want) {
		t.Errorf("comments = %+v, want oldest first %+v", state.Comments, want)
	}
	if s := state.Snap; s.CommentCount != 2 || s.LastCommentID != "20002" || s.LastCommentAt != "2026-10-01T09:05:00.000+0000" {
		t.Errorf("snapshot = count %d marker (%q, %q), want 2 and the newest", s.CommentCount, s.LastCommentID, s.LastCommentAt)
	}

	if s := decode(t, 7).Snap; s.CommentCount != 7 || s.LastCommentID != "" || s.LastCommentAt != "" {
		t.Errorf("truncated page: snapshot = count %d marker (%q, %q), want 7 and no marker", s.CommentCount, s.LastCommentID, s.LastCommentAt)
	}
}
