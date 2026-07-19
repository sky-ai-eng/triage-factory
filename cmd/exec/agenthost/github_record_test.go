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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost:
			_, _ = io.WriteString(w, `{"id":777,"html_url":"https://github.com/octo/repo/pull/1#issuecomment-777"}`)
		case http.MethodPatch:
			_, _ = io.WriteString(w, `{"id":777,"html_url":"https://github.com/octo/repo/pull/1#issuecomment-777"}`)
		case http.MethodDelete:
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
// run for the artifacts FK to anchor on, and returns the bundle + run identity
// + client. eventTriggered picks the write path withWrite routes through: admin
// pool (no user) when true, a synthetic-claims tx (manual run) when false.
func newGithubRecordingClient(t *testing.T, ghURL string, eventTriggered bool) (db.Stores, RunInfo, *LocalClient) {
	_, stores, info, client := newGithubRecordingClientConn(t, ghURL, eventTriggered)
	return stores, info, client
}

// newGithubRecordingClientConn is newGithubRecordingClient plus the raw *sql.DB,
// for touch tests that read run_memory_entities directly (the store interface
// has no role-returning read) or that drop a table to exercise the best-effort
// recording path.
func newGithubRecordingClientConn(t *testing.T, ghURL string, eventTriggered bool) (*sql.DB, db.Stores, RunInfo, *LocalClient) {
	t.Helper()
	conn, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}

	const runID = "22222222-2222-2222-2222-222222222222"
	if _, err := conn.Exec(
		`INSERT INTO runs (id, origin, status) VALUES (?, 'interactive', 'running')`, runID,
	); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	stores := sqlitestore.New(conn)
	info := RunInfo{
		OrgID:            runmode.LocalDefaultOrgID,
		TeamID:           runmode.LocalDefaultTeamID,
		RunID:            runID,
		IsEventTriggered: eventTriggered,
	}
	client := NewLocal(stores, info)
	client.ghResolver = fakeGitHubResolver{baseURL: ghURL, token: "org-pat"}
	return conn, stores, info, client
}

// TestLocalClient_GithubAddComment_RecordsArtifact pins a standalone comment
// landing one `comment` artifact with the full coordinates the add path knows —
// target owner/repo#N, the comment id, its html_url, state posted, deduped on
// github:comment:<id> — across both write paths.
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
			arts := listRunArtifacts(t, stores, info.RunID)
			if len(arts) != 1 {
				t.Fatalf("want 1 artifact, got %d: %+v", len(arts), arts)
			}
			a := arts[0]
			if a.Provider != domain.ArtifactProviderGitHub || a.Kind != domain.ArtifactKindComment ||
				a.Target != "octo/repo#1" || a.ExternalID != "777" ||
				a.URL != "https://github.com/octo/repo/pull/1#issuecomment-777" ||
				a.State != domain.ArtifactStateCommentPosted || a.DedupKey != "github:comment:777" {
				t.Errorf("comment artifact mismatch: %+v", a)
			}
			if a.RunID != info.RunID || a.TeamID != runmode.LocalDefaultTeamID {
				t.Errorf("attribution mismatch: run=%q team=%q", a.RunID, a.TeamID)
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

	arts := listRunArtifacts(t, stores, info.RunID)
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
	arts = listRunArtifacts(t, stores, info.RunID)
	if len(arts) != 1 {
		t.Fatalf("delete should retire the same row, got %d artifacts: %+v", len(arts), arts)
	}
	a := arts[0]
	if a.State != domain.ArtifactStateCommentDeleted || a.DedupKey != "github:comment:777" {
		t.Errorf("delete did not retire in place: %+v", a)
	}
	// The id-only update/delete paths must inherit, not blank, the add's coordinates.
	if a.Target != "octo/repo#1" || a.URL != "https://github.com/octo/repo/pull/1#issuecomment-777" || a.ExternalID != "777" {
		t.Errorf("retire blanked the add's stable coordinates: %+v", a)
	}
}

// TestLocalClient_GithubCommentUpdateDelete_ReviewComment_NotRecorded pins the
// scope boundary: comment-update / comment-delete fall back to the
// pulls/comments (review line-comment) endpoint when the id isn't a top-level
// issue comment. Those ride the review artifact, so even though the action
// succeeds via the fallback, no comment artifact is written.
func TestLocalClient_GithubCommentUpdateDelete_ReviewComment_NotRecorded(t *testing.T) {
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
			_, _ = io.WriteString(w, `{"id":999}`)
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
	if err := client.GithubDeleteComment(ctx, "octo", "repo", 999); err != nil {
		t.Fatalf("delete via the review fallback must still succeed: %v", err)
	}
	if arts := listRunArtifacts(t, stores, info.RunID); len(arts) != 0 {
		t.Errorf("a review line-comment update/delete must record no comment artifact, got %+v", arts)
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
	if arts := listRunArtifacts(t, stores, info.RunID); len(arts) != 0 {
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
