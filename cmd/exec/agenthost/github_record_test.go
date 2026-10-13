package agenthost

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The TFAC-466 recording tests drive the LocalClient's host-routed standalone
// GitHub comment mutations against a fake GitHub backend with a *real* SQLite
// store bundle, and assert each successful action lands the right `artifacts`
// row. The LocalClient is the single seam the multi daemon dispatches through,
// so proving it here covers both the sandbox and local-mode paths. (Review
// line-comments are out of scope — they ride the review artifact.)

// startFakeGitHubComments stands a minimal GitHub REST backend for the comment
// verbs: add (POST → id + html_url), update (PATCH → 200), delete (DELETE →
// 204). The client tries the issue-comments endpoint first for update/delete,
// which this answers, so it never falls through to the review-comments path.
func startFakeGitHubComments(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/replies"):
			// A reply is a review comment, linked by its discussion anchor on
			// the org's own GitHub.
			_, _ = io.WriteString(w, `{"id":777,"node_id":"PRRC_kw777","html_url":"`+srv.URL+`/octo/repo/pull/1#discussion_r777"}`)
		case r.Method == http.MethodPost, r.Method == http.MethodPatch:
			_, _ = io.WriteString(w, `{"id":777,"node_id":"IC_kw777","html_url":"https://github.com/octo/repo/pull/1#issuecomment-777"}`)
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newGithubRecordingClient builds a LocalClient wired to a real SQLite store
// bundle (live Artifacts / Tx) and a fake resolver pointing at ghURL, seeds a
// conversation for the artifacts FK to anchor on, and returns the bundle +
// conversation identity + client. eventTriggered picks the write path withWrite routes through: admin
// pool (no user) when true, a synthetic-claims tx (manual conversation) when
// false.
func newGithubRecordingClient(t *testing.T, ghURL string, eventTriggered bool) (db.Stores, ConversationInfo, *LocalClient) {
	_, stores, info, client := newGithubRecordingClientConn(t, ghURL, eventTriggered)
	return stores, info, client
}

// newGithubRecordingClientConn is newGithubRecordingClient plus the raw *sql.DB,
// for touch tests that read conversation_memory_entities directly (the store interface
// has no role-returning read) or that drop a table to exercise the best-effort
// recording path.
func newGithubRecordingClientConn(t *testing.T, ghURL string, eventTriggered bool) (*sql.DB, db.Stores, ConversationInfo, *LocalClient) {
	t.Helper()
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	recordGitHubURLForTest(t, conn)

	const conversationID = "22222222-2222-2222-2222-222222222222"
	if _, err := conn.Exec(
		`INSERT INTO conversations (id, origin, status) VALUES (?, 'interactive', 'running')`, conversationID,
	); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	stores := sqlitestore.New(conn)
	info := ConversationInfo{
		OrgID:            runmode.LocalDefaultOrgID,
		TeamID:           runmode.LocalDefaultTeamID,
		ConversationID:   conversationID,
		IsEventTriggered: eventTriggered,
	}
	client := NewLocal(stores, info)
	client.SetGitHubResolver(fakeGitHubResolver{baseURL: ghURL, token: "org-pat"})
	return conn, stores, info, client
}

// TestLocalClient_GithubAddComment_RecordsArtifact pins a standalone comment
// landing one `comment` artifact with the full coordinates the add path knows —
// target owner/repo#N, the comment id and node id, its html_url, state posted,
// marked posted by TF, deduped on github:comment:<id> — across both write
// paths.
func TestLocalClient_GithubAddComment_RecordsArtifact(t *testing.T) {
	for _, eventTriggered := range []bool{true, false} {
		name := "manual"
		if eventTriggered {
			name = "event-triggered"
		}
		t.Run(name, func(t *testing.T) {
			gh := startFakeGitHubComments(t)
			stores, info, client := newGithubRecordingClient(t, gh.URL, eventTriggered)

			id, err := client.GithubAddComment(context.Background(), "octo", "repo", 1, "looks good to me")
			if err != nil {
				t.Fatalf("GithubAddComment: %v", err)
			}
			if id != 777 {
				t.Fatalf("comment id = %d, want 777", id)
			}
			arts := listConversationArtifacts(t, stores, info.ConversationID)
			if len(arts) != 1 {
				t.Fatalf("want 1 artifact, got %d: %+v", len(arts), arts)
			}
			a := arts[0]
			if a.Provider != domain.ArtifactProviderGitHub || a.Kind != domain.ArtifactKindComment ||
				a.Target != "octo/repo#1" || a.ExternalID != "777" ||
				a.URL != "https://github.com/octo/repo/pull/1#issuecomment-777" ||
				a.State != domain.ArtifactStateCommentPosted || a.DedupKey != "github:comment:777" ||
				a.NodeID != "IC_kw777" || !a.PostedByTF {
				t.Errorf("comment artifact mismatch: %+v", a)
			}
			if a.ConversationID != info.ConversationID || a.TeamID != runmode.LocalDefaultTeamID {
				t.Errorf("attribution mismatch: conversation=%q team=%q", a.ConversationID, a.TeamID)
			}
		})
	}
}

// TestLocalClient_GithubCommentLifecycle_DedupsAndRetires pins that add →
// comment-update → comment-delete collapse onto one row keyed on the comment id:
// update bumps it in place (still posted), delete retires it to the deleted
// state — and neither the update nor the delete (which carry only the id, not
// the PR number) blanks the target/url the add stamped.
func TestLocalClient_GithubCommentLifecycle_DedupsAndRetires(t *testing.T) {
	gh := startFakeGitHubComments(t)
	stores, info, client := newGithubRecordingClient(t, gh.URL, true)
	ctx := context.Background()

	id, err := client.GithubAddComment(ctx, "octo", "repo", 1, "first take")
	if err != nil {
		t.Fatalf("GithubAddComment: %v", err)
	}
	if err := client.GithubUpdateComment(ctx, "octo", "repo", id, "edited take"); err != nil {
		t.Fatalf("GithubUpdateComment: %v", err)
	}

	arts := listConversationArtifacts(t, stores, info.ConversationID)
	if len(arts) != 1 {
		t.Fatalf("update should upsert the same row, got %d artifacts: %+v", len(arts), arts)
	}
	if arts[0].State != domain.ArtifactStateCommentPosted || arts[0].Target != "octo/repo#1" ||
		arts[0].URL != "https://github.com/octo/repo/pull/1#issuecomment-777" {
		t.Errorf("after update the row drifted: %+v", arts[0])
	}

	if err := client.GithubDeleteComment(ctx, "octo", "repo", id); err != nil {
		t.Fatalf("GithubDeleteComment: %v", err)
	}
	arts = listConversationArtifacts(t, stores, info.ConversationID)
	if len(arts) != 1 {
		t.Fatalf("delete should retire the same row, got %d artifacts: %+v", len(arts), arts)
	}
	a := arts[0]
	if a.State != domain.ArtifactStateCommentDeleted || a.DedupKey != "github:comment:777" {
		t.Errorf("delete did not retire in place: %+v", a)
	}
	// The id-only update/delete paths must inherit, not blank, the add's
	// coordinates, and must not unsay that TF posted the comment.
	if a.Target != "octo/repo#1" || a.URL != "https://github.com/octo/repo/pull/1#issuecomment-777" || a.ExternalID != "777" ||
		a.NodeID != "IC_kw777" || !a.PostedByTF {
		t.Errorf("retire blanked the add's stable coordinates: %+v", a)
	}
}

// TestLocalClient_GithubCommentUpdateDelete_ReviewComment_RecordsItsRow pins
// comment-update / comment-delete on a review comment, which they reach by
// falling back to the pulls/comments endpoint when the id isn't a conversation
// comment. The edit and the delete land on one row keyed in the review
// comments' numbering, carrying the node id the edit's response returned, and
// the row does not claim TF posted the comment: these verbs edit a person's
// comment as readily as TF's own.
func TestLocalClient_GithubCommentUpdateDelete_ReviewComment_RecordsItsRow(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The issues-comments endpoint 404s (id is not a top-level comment);
		// the client then falls back to the pulls (review) endpoint, which
		// handles it.
		if strings.Contains(r.URL.Path, "/issues/comments/") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
			return
		}
		switch r.Method {
		case http.MethodPatch:
			_, _ = io.WriteString(w, `{"id":999,"node_id":"PRRC_kw999","html_url":"https://github.com/octo/repo/pull/1#discussion_r999"}`)
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	t.Cleanup(gh.Close)

	stores, info, client := newGithubRecordingClient(t, gh.URL, true)
	ctx := context.Background()

	if err := client.GithubUpdateComment(ctx, "octo", "repo", 999, "edit a review comment"); err != nil {
		t.Fatalf("update via the review fallback must still succeed: %v", err)
	}
	arts := listConversationArtifacts(t, stores, info.ConversationID)
	if len(arts) != 1 {
		t.Fatalf("want 1 artifact after the edit, got %d: %+v", len(arts), arts)
	}
	if a := arts[0]; a.DedupKey != domain.GitHubCommentDedupKey(domain.GitHubReviewComment, 999) ||
		a.State != domain.ArtifactStateCommentPosted || a.NodeID != "PRRC_kw999" || a.ExternalID != "999" || a.PostedByTF {
		t.Errorf("review comment edit artifact = %+v, want the review comment's row, posted, with its node id, not posted by TF", a)
	}

	if err := client.GithubDeleteComment(ctx, "octo", "repo", 999); err != nil {
		t.Fatalf("delete via the review fallback must still succeed: %v", err)
	}
	arts = listConversationArtifacts(t, stores, info.ConversationID)
	if len(arts) != 1 {
		t.Fatalf("the delete should retire the edit's row, got %d artifacts: %+v", len(arts), arts)
	}
	if a := arts[0]; a.State != domain.ArtifactStateCommentDeleted || a.NodeID != "PRRC_kw999" || a.PostedByTF {
		t.Errorf("review comment after delete = %+v, want it deleted with its node id kept", a)
	}
	posted, err := stores.Artifacts.PostedByTFDedupKeysSystem(ctx, info.OrgID, arts[0].Scope, []string{arts[0].DedupKey})
	if err != nil {
		t.Fatalf("PostedByTFDedupKeysSystem: %v", err)
	}
	if len(posted) != 0 {
		t.Errorf("posted-by-TF lookup = %v, want the person's comment the run edited left out", posted)
	}
}

// TestLocalClient_GithubReplyToComment_RecordsArtifact pins a thread reply's
// own `comment` artifact: keyed in the review comments' numbering, posted by
// TF, carrying the reply's node id, its discussion link, the PR-shaped target,
// and the comment it answers in its details.
func TestLocalClient_GithubReplyToComment_RecordsArtifact(t *testing.T) {
	gh := startFakeGitHubComments(t)
	stores, info, client := newGithubRecordingClient(t, gh.URL, false)

	id, err := client.GithubReplyToComment(context.Background(), "octo", "repo", 1, 555, "fixed in the next push")
	if err != nil {
		t.Fatalf("GithubReplyToComment: %v", err)
	}
	if id != 777 {
		t.Fatalf("reply id = %d, want 777", id)
	}
	arts := listConversationArtifacts(t, stores, info.ConversationID)
	if len(arts) != 1 {
		t.Fatalf("want 1 artifact, got %d: %+v", len(arts), arts)
	}
	a := arts[0]
	if a.Kind != domain.ArtifactKindComment || a.DedupKey != domain.GitHubCommentDedupKey(domain.GitHubReviewComment, 777) ||
		a.Target != "octo/repo#1" || a.ExternalID != "777" || a.NodeID != "PRRC_kw777" ||
		a.URL != gh.URL+"/octo/repo/pull/1#discussion_r777" || a.State != domain.ArtifactStateCommentPosted ||
		!a.PostedByTF || a.DetailsJSON != `{"in_reply_to":555}` {
		t.Errorf("reply artifact = %+v", a)
	}
}

// TestLocalClient_GithubComments_SameNumberInBothSpacesAreTwoRows pins that a
// conversation comment and a review comment GitHub happened to number alike are
// recorded as two comments: the two numberings are separate, so the key names
// which one a number is in.
func TestLocalClient_GithubComments_SameNumberInBothSpacesAreTwoRows(t *testing.T) {
	gh := startFakeGitHubComments(t)
	stores, info, client := newGithubRecordingClient(t, gh.URL, true)
	ctx := context.Background()

	commentID, err := client.GithubAddComment(ctx, "octo", "repo", 1, "a conversation comment")
	if err != nil {
		t.Fatalf("GithubAddComment: %v", err)
	}
	replyID, err := client.GithubReplyToComment(ctx, "octo", "repo", 1, 555, "a thread reply")
	if err != nil {
		t.Fatalf("GithubReplyToComment: %v", err)
	}
	if commentID != replyID {
		t.Fatalf("the fake numbered the two %d and %d; the case needs one number", commentID, replyID)
	}
	arts := listConversationArtifacts(t, stores, info.ConversationID)
	if len(arts) != 2 {
		t.Fatalf("want 2 artifacts for two comments numbered %d, got %d: %+v", commentID, len(arts), arts)
	}
	keys := map[string]bool{arts[0].DedupKey: true, arts[1].DedupKey: true}
	if !keys[domain.GitHubCommentDedupKey(domain.GitHubIssueComment, 777)] || !keys[domain.GitHubCommentDedupKey(domain.GitHubReviewComment, 777)] {
		t.Errorf("keys = %v, want one in each numbering", keys)
	}
}

// TestLocalClient_GithubUpdateComment_PersonsComment_NotPostedByTF pins that an
// edit of a conversation comment TF never recorded mints a row that records the
// edit and does not claim the comment, so the posted-by-TF lookup leaves it out.
func TestLocalClient_GithubUpdateComment_PersonsComment_NotPostedByTF(t *testing.T) {
	gh := startFakeGitHubComments(t)
	stores, info, client := newGithubRecordingClient(t, gh.URL, true)
	ctx := context.Background()

	if err := client.GithubUpdateComment(ctx, "octo", "repo", 777, "tidied a person's comment"); err != nil {
		t.Fatalf("GithubUpdateComment: %v", err)
	}
	arts := listConversationArtifacts(t, stores, info.ConversationID)
	if len(arts) != 1 || arts[0].PostedByTF || arts[0].DedupKey != "github:comment:777" {
		t.Fatalf("artifacts = %+v, want one row for comment 777 not posted by TF", arts)
	}
	posted, err := stores.Artifacts.PostedByTFDedupKeysSystem(ctx, info.OrgID, arts[0].Scope, []string{"github:comment:777"})
	if err != nil {
		t.Fatalf("PostedByTFDedupKeysSystem: %v", err)
	}
	if len(posted) != 0 {
		t.Errorf("posted-by-TF lookup = %v, want the edited comment left out", posted)
	}
}

// TestLocalClient_GithubAddComment_NoID_SkipsArtifact pins that when GitHub's
// response carries no comment id, the comment still posts (action succeeds) but
// no artifact is written — there's no stable key to dedup on.
func TestLocalClient_GithubAddComment_NoID_SkipsArtifact(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`) // POST returns a body with no id
	}))
	t.Cleanup(gh.Close)

	stores, info, client := newGithubRecordingClient(t, gh.URL, true)
	if _, err := client.GithubAddComment(context.Background(), "octo", "repo", 1, "no id back"); err != nil {
		t.Fatalf("GithubAddComment must succeed even when the id is missing: %v", err)
	}
	if arts := listConversationArtifacts(t, stores, info.ConversationID); len(arts) != 0 {
		t.Errorf("expected no artifact when comment id is empty, got %+v", arts)
	}
}

// TestLocalClient_GithubCommentRecordingFailure_DoesNotFailAction pins the
// best-effort contract: a store write that errors is swallowed (logged), and
// the comment — which already posted upstream — still returns success.
func TestLocalClient_GithubCommentRecordingFailure_DoesNotFailAction(t *testing.T) {
	gh := startFakeGitHubComments(t)
	_, _, client := newGithubRecordingClient(t, gh.URL, true)
	rec := &erroringArtifacts{}
	client.stores.Artifacts = rec
	// The runtime is derived from stores at construction, so a post-construction
	// swap of the recorder must rebuild it (the DB effects route through c.rt).
	client.rt = newDirectRuntime(client.stores, client.info)

	id, err := client.GithubAddComment(context.Background(), "octo", "repo", 1, "best effort")
	if err != nil {
		t.Fatalf("add-comment must not fail when recording fails: %v", err)
	}
	if id != 777 {
		t.Errorf("comment id = %d, want 777 — the action itself must still succeed", id)
	}
	if rec.callCount() == 0 {
		t.Fatal("expected the recorder to have been called (and to have failed)")
	}
}
