package agenthost

import (
	"context"
	"errors"
	"hash/fnv"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// reviewDiffServer is a fake GitHub serving the reads add-review-comment makes.
// The diff media-type request (Accept: diff) is answered the same whether it's
// GetPRDiff (live-head fallback) or GetCompareDiff (base...worktree-HEAD, the
// supplied-anchor path): a.go with new-side lines 1..5 commentable, for the
// range validation. The JSON request is the PR object (GetPRBasic) — base.ref
// for the compare and head.sha for the live-head fallback. headSHA is read live
// from *headSHA so a test can flip it between calls to simulate the live head
// moving.
func reviewDiffServer(t *testing.T, headSHA *string) *httptest.Server {
	t.Helper()
	const diff = "diff --git a/a.go b/a.go\n" +
		"--- a/a.go\n+++ b/a.go\n" +
		"@@ -1,3 +1,5 @@\n line1\n+added2\n+added3\n line4\n line5\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Accept"), "diff") {
			_, _ = io.WriteString(w, diff)
			return
		}
		if answerRepoRead(w, r) {
			return
		}
		if answerRepoRead(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"number":`+fakePRNumber(r.URL.Path)+`,"node_id":"`+fakePRNodeID(r.URL.Path)+`","base":{"ref":"main","repo":`+fakeRepoJSON(r.URL.Path)+`,"repo":`+fakeRepoJSON(r.URL.Path)+`},"head":{"sha":"`+*headSHA+`"}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// testPRNodeID is the GraphQL node id the fake GitHub servers in this package
// answer for pull request number: the id a start-review keys its draft on.
func testPRNodeID(number int) string {
	return "PR_kwDOtest" + strconv.Itoa(number)
}

// fakePRNumber is the pull request number a /repos/{owner}/{repo}/pulls/{number}
// request path names, "7" for a path that names none.
func fakePRNumber(path string) string {
	_, rest, ok := strings.Cut(path, "/pulls/")
	if !ok {
		return "7"
	}
	number, _, _ := strings.Cut(rest, "/")
	if _, err := strconv.Atoi(number); err != nil {
		return "7"
	}
	return number
}

// fakePRNodeID is testPRNodeID for the pull request a
// /repos/{owner}/{repo}/pulls/{number} request path names, so a fake server's
// PR object carries the node id start-review keyed the draft on. "" for a path
// that names no pull request.
func fakePRNodeID(path string) string {
	_, rest, ok := strings.Cut(path, "/pulls/")
	if !ok {
		return ""
	}
	number, _, _ := strings.Cut(rest, "/")
	n, err := strconv.Atoi(number)
	if err != nil {
		return ""
	}
	return testPRNodeID(n)
}

// answerPRRead answers r when it is a read of a pull request or of a
// repository, the identity reads the host makes for what it records
// (GET /repos/{owner}/{repo}/pulls/{number} and GET /repos/{owner}/{repo}):
// the PR object carries testPRNodeID and its base repository, and the
// repository testRepoID under the name the path spells. It reports whether it
// answered. A fake GitHub that must see nothing else hands every other request
// to its own failure.
func answerPRRead(w http.ResponseWriter, r *http.Request) bool {
	if answerRepoRead(w, r) {
		return true
	}
	if r.Method != http.MethodGet || strings.Contains(r.Header.Get("Accept"), "diff") {
		return false
	}
	_, number, ok := strings.Cut(r.URL.Path, "/pulls/")
	node := fakePRNodeID(r.URL.Path)
	if !ok || node == "" || strings.Contains(number, "/") {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"number":`+number+`,"node_id":"`+node+`","base":{"ref":"main","repo":`+fakeRepoJSON(r.URL.Path)+`},"head":{"sha":"pr-head"}}`)
	return true
}

// answerRepoRead answers r when it is a repository read
// (GET /repos/{owner}/{repo}), with testRepoID under the name the path
// spells, and reports whether it did.
func answerRepoRead(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v3")
	rest, ok := strings.CutPrefix(path, "/repos/")
	if !ok || strings.Count(rest, "/") != 1 {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, fakeRepoJSON(path))
	return true
}

// testRepoID is the GitHub id the fake GitHub servers in this package answer
// for the repository slug names, folded like GitHub folds names.
func testRepoID(slug string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(strings.ToLower(slug)))
	return strconv.FormatUint(uint64(h.Sum32()%1_000_000)+1, 10)
}

// fakeRepoJSON is the repository object a fake GitHub answers for the
// repository a /repos/{owner}/{repo}/... path names.
func fakeRepoJSON(path string) string {
	rest, _ := strings.CutPrefix(strings.TrimPrefix(path, "/api/v3"), "/repos/")
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 {
		return `null`
	}
	slug := parts[0] + "/" + parts[1]
	return `{"id":` + testRepoID(slug) + `,"full_name":"` + slug + `"}`
}

// TestLocalClient_GithubCreatePendingReview_RecordsLocalDraft pins that
// start-review lands one conversation-scoped `review` deliverable with ZERO GitHub writes:
// state=pending (no ready sentinel), empty ExternalID (no GitHub review until
// approval), the head SHA pinned into details, deduped on
// github:review:<the node id GitHub answers for the PR>:<conversation_id> —
// across both write paths. The returned handle is the deliverable id.
func TestLocalClient_GithubCreatePendingReview_RecordsLocalDraft(t *testing.T) {
	for _, eventTriggered := range []bool{true, false} {
		name := "manual"
		if eventTriggered {
			name = "event-triggered"
		}
		t.Run(name, func(t *testing.T) {
			// start-review reads the PR for its node id and makes no other GitHub
			// call; fail loudly if one happens.
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if answerPRRead(w, r) {
					return
				}
				t.Errorf("unexpected GitHub call during start-review: %s %s", r.Method, r.URL.Path)
				_, _ = io.WriteString(w, `{}`)
			}))
			t.Cleanup(srv.Close)
			stores, info, client := newGithubRecordingClient(t, srv.URL, eventTriggered)
			registerRepo(t, stores, "octo", "repo", testRepoID("octo/repo"))

			handle, err := client.GithubCreatePendingReview(context.Background(), "octo", "repo", 7, "headsha7", nil)
			if err != nil {
				t.Fatalf("GithubCreatePendingReview: %v", err)
			}
			if handle == "" {
				t.Fatal("expected a non-empty local review handle")
			}

			arts := listConversationDeliverables(t, stores, info.ConversationID)
			if len(arts) != 1 {
				t.Fatalf("want 1 deliverable, got %d: %+v", len(arts), arts)
			}
			a := arts[0]
			if a.ID != handle {
				t.Errorf("handle = %q, want the deliverable id %q", handle, a.ID)
			}
			wantDedup := domain.ReviewDedupKey(testPRNodeID(7), info.ConversationID)
			if a.Kind != domain.DeliverableKindReview || a.State != domain.DeliverableStateReviewPending ||
				a.Target != "octo/repo#7" || a.ExternalID != "" || a.DedupKey != wantDedup {
				t.Errorf("review deliverable mismatch: %+v (want dedup %q)", a, wantDedup)
			}
			d, derr := domain.ParseReviewDeliverableDetails(a.DetailsJSON)
			if derr != nil {
				t.Fatalf("ParseReviewDeliverableDetails: %v", derr)
			}
			if d.HeadSHA != "headsha7" || d.Number != 7 {
				t.Errorf("details mismatch: %+v", d)
			}
			if d.ReviewEvent != "" {
				t.Errorf("fresh review must have no ready sentinel: %+v", d)
			}
		})
	}
}

// TestLocalClient_GithubAddPendingReviewComment_StagesLocally pins the primary
// (TFAC-494) path: the CLI supplies the worktree HEAD, the host validates the
// range against THAT commit's diff (compare base...HEAD), anchors the staged
// comment to it, and stages into details.staged_comments — no GitHub write.
func TestLocalClient_GithubAddPendingReviewComment_StagesLocally(t *testing.T) {
	head := "live_head" // the LIVE head; the agent's checkout (anchor) is older
	srv := reviewDiffServer(t, &head)
	stores, info, client := newGithubRecordingClient(t, srv.URL, true)

	handle, err := client.GithubCreatePendingReview(context.Background(), "octo", "repo", 7, "headsha7", nil)
	if err != nil {
		t.Fatalf("GithubCreatePendingReview: %v", err)
	}

	const anchor = "worktree_head" // the commit the agent's checkout is on
	// An in-diff line (3) stages, anchored to the supplied worktree HEAD.
	cid, err := client.GithubAddPendingReviewComment(context.Background(), "octo", "repo", handle, "a.go", "nit: rename", 3, nil, anchor)
	if err != nil {
		t.Fatalf("GithubAddPendingReviewComment: %v", err)
	}
	if cid == "" {
		t.Fatal("expected a non-empty staged comment id")
	}

	arts := listConversationDeliverables(t, stores, info.ConversationID)
	d, _ := domain.ParseReviewDeliverableDetails(arts[0].DetailsJSON)
	if len(d.StagedComments) != 1 || d.StagedComments[0].ID != cid {
		t.Fatalf("comment not staged: %+v", d.StagedComments)
	}
	if d.StagedComments[0].Line == nil || *d.StagedComments[0].Line != 3 {
		t.Errorf("staged line = %+v, want 3", d.StagedComments[0].Line)
	}
	// Anchored to the supplied worktree HEAD — NOT the live head — so the comment
	// lands on the line the agent read.
	if d.StagedComments[0].CommitSHA != anchor {
		t.Errorf("staged CommitSHA = %q, want %q (the supplied worktree HEAD)", d.StagedComments[0].CommitSHA, anchor)
	}

	// An out-of-diff line (99) is rejected before staging.
	if _, err := client.GithubAddPendingReviewComment(context.Background(), "octo", "repo", handle, "a.go", "bad", 99, nil, anchor); err == nil {
		t.Error("expected an out-of-diff line to be rejected")
	}
	arts = listConversationDeliverables(t, stores, info.ConversationID)
	d, _ = domain.ParseReviewDeliverableDetails(arts[0].DetailsJSON)
	if len(d.StagedComments) != 1 {
		t.Errorf("a rejected comment must not be staged, got %d", len(d.StagedComments))
	}
}

// TestLocalClient_GithubAddPendingReviewComment_FallbackLiveHead pins the
// no-checkout fallback: with an empty anchor the host validates against and
// anchors to the LIVE PR head (the pre-TFAC-494 behavior).
func TestLocalClient_GithubAddPendingReviewComment_FallbackLiveHead(t *testing.T) {
	head := "commit_v1"
	srv := reviewDiffServer(t, &head)
	stores, info, client := newGithubRecordingClient(t, srv.URL, true)

	handle, err := client.GithubCreatePendingReview(context.Background(), "octo", "repo", 7, "headsha7", nil)
	if err != nil {
		t.Fatalf("GithubCreatePendingReview: %v", err)
	}

	cid, err := client.GithubAddPendingReviewComment(context.Background(), "octo", "repo", handle, "a.go", "nit", 3, nil, "")
	if err != nil {
		t.Fatalf("GithubAddPendingReviewComment: %v", err)
	}
	if cid == "" {
		t.Fatal("expected a non-empty staged comment id")
	}

	arts := listConversationDeliverables(t, stores, info.ConversationID)
	d, _ := domain.ParseReviewDeliverableDetails(arts[0].DetailsJSON)
	if len(d.StagedComments) != 1 || d.StagedComments[0].CommitSHA != "commit_v1" {
		t.Errorf("fallback must anchor to the live head commit_v1, got %+v", d.StagedComments)
	}
}

// TestLocalClient_FinalizeReviewDraft pins finalize-review's host behavior: no
// GitHub call, the agent's draft (body + event + the locally staged comments)
// snapshotted into the deliverable, the ready sentinel set, and the TFAC-358
// anti-double-submit guard.
func TestLocalClient_FinalizeReviewDraft(t *testing.T) {
	head := "commit_v1"
	srv := reviewDiffServer(t, &head)
	stores, info, client := newGithubRecordingClient(t, srv.URL, true)

	handle, err := client.GithubCreatePendingReview(context.Background(), "octo", "repo", 7, "headsha7", nil)
	if err != nil {
		t.Fatalf("GithubCreatePendingReview: %v", err)
	}
	if _, err := client.GithubAddPendingReviewComment(context.Background(), "octo", "repo", handle, "a.go", "nit: rename", 3, nil, "worktree_head"); err != nil {
		t.Fatalf("GithubAddPendingReviewComment: %v", err)
	}

	if _, err := client.FinalizeReviewDraft(context.Background(), handle, "COMMENT", "## Review body"); err != nil {
		t.Fatalf("FinalizeReviewDraft: %v", err)
	}

	arts := listConversationDeliverables(t, stores, info.ConversationID)
	if len(arts) != 1 {
		t.Fatalf("want 1 deliverable, got %d", len(arts))
	}
	if arts[0].State != domain.DeliverableStateReviewPending {
		t.Errorf("deliverable must stay pending until approval, got %q", arts[0].State)
	}
	d, _ := domain.ParseReviewDeliverableDetails(arts[0].DetailsJSON)
	if d.ReviewEvent != "COMMENT" || d.ReviewBody != "## Review body" {
		t.Errorf("ready sentinel / staged body not set: %+v", d)
	}
	if len(d.Proposed.Comments) != 1 || d.Proposed.Comments[0].Path != "a.go" {
		t.Errorf("proposed comments not snapshotted from the staged set: %+v", d.Proposed)
	}

	// Anti-double-submit (TFAC-358): a second finalize hard-errors.
	_, err = client.FinalizeReviewDraft(context.Background(), handle, "COMMENT", "## again")
	if !errors.Is(err, ErrReviewAlreadyFinalized) {
		t.Errorf("second finalize-review = %v, want ErrReviewAlreadyFinalized", err)
	}
}

// TestReviewDraft_RecordsTheReviewedPullRequestProduced pins that staging a
// review gives the conversation its pull request as produced. The draft is
// the run's output before any action is recorded for it, and the approval that
// posts it records no memory link of its own.
func TestReviewDraft_RecordsTheReviewedPullRequestProduced(t *testing.T) {
	head := "commit_v1"
	srv := reviewDiffServer(t, &head)
	conn, stores, info, client := newGithubRecordingClientConn(t, srv.URL, true)

	if _, err := client.GithubCreatePendingReview(context.Background(), "octo", "repo", 7, "headsha7", nil); err != nil {
		t.Fatalf("GithubCreatePendingReview: %v", err)
	}

	arts := listConversationDeliverables(t, stores, info.ConversationID)
	if len(arts) != 1 || arts[0].EntityID == "" {
		t.Fatalf("want one review draft linked to its pull request, got %+v", arts)
	}
	if role := touchRole(t, conn, info.ConversationID, arts[0].EntityID); role != domain.MemoryRoleProduced {
		t.Errorf("link role on the reviewed pull request = %q, want %q", role, domain.MemoryRoleProduced)
	}
}

// TestLocalClient_MultipleReviewDrafts_ResolveByHandle pins that when one conversation
// holds several review drafts (conversation-scoped dedup, one per PR — TFAC-494),
// add-review-comment and finalize-review act on the draft named by the handle, not
// just the first pending review. A naive "first pending review" lookup would
// stage/finalize against the wrong PR's draft.
func TestLocalClient_MultipleReviewDrafts_ResolveByHandle(t *testing.T) {
	head := "commit_v1"
	srv := reviewDiffServer(t, &head)
	stores, info, client := newGithubRecordingClient(t, srv.URL, true)

	// Two drafts on different PRs in the same conversation.
	h7, err := client.GithubCreatePendingReview(context.Background(), "octo", "repo", 7, "sha7", nil)
	if err != nil {
		t.Fatalf("create review 7: %v", err)
	}
	h8, err := client.GithubCreatePendingReview(context.Background(), "octo", "repo", 8, "sha8", nil)
	if err != nil {
		t.Fatalf("create review 8: %v", err)
	}
	if h7 == h8 {
		t.Fatalf("two drafts must get distinct handles, both = %q", h7)
	}

	// Stage a comment on the SECOND draft's handle; it must land on PR 8's draft.
	if _, err := client.GithubAddPendingReviewComment(context.Background(), "octo", "repo", h8, "a.go", "on 8", 3, nil, "worktree_head"); err != nil {
		t.Fatalf("add comment to review 8: %v", err)
	}
	// Finalize the SECOND draft by its handle.
	if _, err := client.FinalizeReviewDraft(context.Background(), h8, "COMMENT", "## review 8"); err != nil {
		t.Fatalf("finalize review 8: %v", err)
	}

	arts := listConversationDeliverables(t, stores, info.ConversationID)
	byID := map[string]domain.ReviewDeliverableDetails{}
	for _, a := range arts {
		d, _ := domain.ParseReviewDeliverableDetails(a.DetailsJSON)
		byID[a.ID] = d
	}
	// PR 8's draft got the comment + the ready sentinel.
	d8 := byID[h8]
	if len(d8.StagedComments) != 1 || d8.ReviewEvent != "COMMENT" || d8.ReviewBody != "## review 8" {
		t.Errorf("review 8 draft: %+v, want one staged comment + finalized COMMENT", d8)
	}
	// PR 7's draft is untouched — no comment, no sentinel.
	d7 := byID[h7]
	if len(d7.StagedComments) != 0 || d7.ReviewEvent != "" {
		t.Errorf("review 7 draft must be untouched, got %+v", d7)
	}

	// An unknown handle is rejected (not silently routed to the first draft).
	if _, err := client.GithubAddPendingReviewComment(context.Background(), "octo", "repo", "nope", "a.go", "x", 3, nil, "worktree_head"); err == nil {
		t.Error("an unknown handle must be rejected")
	}
}

// TestLocalClient_ResetReviewDraft pins start-review --fresh's host behavior: a
// finalized draft (staged comment + ready sentinel + proposed snapshot) is reset
// in place to an empty pending draft — same handle, same pinned head SHA — with
// zero GitHub calls beyond the comment staging that set it up.
func TestLocalClient_ResetReviewDraft(t *testing.T) {
	head := "commit_v1"
	srv := reviewDiffServer(t, &head)
	stores, info, client := newGithubRecordingClient(t, srv.URL, true)

	handle, err := client.GithubCreatePendingReview(context.Background(), "octo", "repo", 7, "headsha7", nil)
	if err != nil {
		t.Fatalf("GithubCreatePendingReview: %v", err)
	}
	if _, err := client.GithubAddPendingReviewComment(context.Background(), "octo", "repo", handle, "a.go", "nit", 3, nil, "worktree_head"); err != nil {
		t.Fatalf("GithubAddPendingReviewComment: %v", err)
	}
	if _, err := client.FinalizeReviewDraft(context.Background(), handle, "COMMENT", "## body"); err != nil {
		t.Fatalf("FinalizeReviewDraft: %v", err)
	}

	// Reset the draft for the same PR.
	got, gotSHA, err := client.ResetReviewDraft(context.Background(), "octo", "repo", 7)
	if err != nil {
		t.Fatalf("ResetReviewDraft: %v", err)
	}
	if got != handle {
		t.Errorf("reset handle = %q, want the same draft handle %q", got, handle)
	}
	// The preserved head SHA is returned so the CLI echoes the same commit_sha a
	// normal start-review prints.
	if gotSHA != "headsha7" {
		t.Errorf("reset commit_sha = %q, want the preserved headsha7", gotSHA)
	}

	arts := listConversationDeliverables(t, stores, info.ConversationID)
	if len(arts) != 1 {
		t.Fatalf("reset must keep the one draft row, got %d", len(arts))
	}
	if arts[0].State != domain.DeliverableStateReviewPending {
		t.Errorf("draft must stay pending after reset, got %q", arts[0].State)
	}
	d, _ := domain.ParseReviewDeliverableDetails(arts[0].DetailsJSON)
	if len(d.StagedComments) != 0 {
		t.Errorf("reset must clear staged comments, got %+v", d.StagedComments)
	}
	if d.ReviewEvent != "" || d.ReviewBody != "" || len(d.Proposed.Comments) != 0 {
		t.Errorf("reset must clear the ready sentinel + proposed snapshot, got %+v", d)
	}
	// The pinned head SHA + PR number survive so the restarted review still anchors.
	if d.HeadSHA != "headsha7" || d.Number != 7 {
		t.Errorf("reset must preserve Number/HeadSHA, got %+v", d)
	}
}

// TestLocalClient_ResetReviewDraft_NoDraft pins that --fresh with no draft for
// the PR returns an empty handle (not an error) so the CLI falls through to a
// normal start-review.
func TestLocalClient_ResetReviewDraft_NoDraft(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if answerPRRead(w, r) {
			return
		}
		t.Errorf("unexpected GitHub call during reset-with-no-draft: %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	_, _, client := newGithubRecordingClient(t, srv.URL, true)

	got, gotSHA, err := client.ResetReviewDraft(context.Background(), "octo", "repo", 7)
	if err != nil {
		t.Fatalf("ResetReviewDraft (no draft): %v", err)
	}
	if got != "" || gotSHA != "" {
		t.Errorf("reset = (%q, %q), want (\"\", \"\") when there's no draft to reset", got, gotSHA)
	}
}

// TestLocalClient_StagedReviewComment_UpdateDelete pins comment-update /
// comment-delete on the staged (non-numeric id) path: update rewrites one staged
// comment's body in place, delete removes it, an unknown id errors, and neither
// touches a sibling comment.
func TestLocalClient_StagedReviewComment_UpdateDelete(t *testing.T) {
	head := "commit_v1"
	srv := reviewDiffServer(t, &head)
	stores, info, client := newGithubRecordingClient(t, srv.URL, true)

	handle, err := client.GithubCreatePendingReview(context.Background(), "octo", "repo", 7, "headsha7", nil)
	if err != nil {
		t.Fatalf("GithubCreatePendingReview: %v", err)
	}
	cid1, err := client.GithubAddPendingReviewComment(context.Background(), "octo", "repo", handle, "a.go", "first", 3, nil, "worktree_head")
	if err != nil {
		t.Fatalf("add comment 1: %v", err)
	}
	cid2, err := client.GithubAddPendingReviewComment(context.Background(), "octo", "repo", handle, "a.go", "second", 4, nil, "worktree_head")
	if err != nil {
		t.Fatalf("add comment 2: %v", err)
	}

	// Update the first staged comment (body already badge-baked by the CLI).
	if err := client.UpdateStagedReviewComment(context.Background(), cid1, "first edited"); err != nil {
		t.Fatalf("UpdateStagedReviewComment: %v", err)
	}
	d, _ := domain.ParseReviewDeliverableDetails(listConversationDeliverables(t, stores, info.ConversationID)[0].DetailsJSON)
	if len(d.StagedComments) != 2 {
		t.Fatalf("update must not change the comment count, got %d", len(d.StagedComments))
	}
	if byID(d.StagedComments, cid1).Body != "first edited" {
		t.Errorf("comment 1 body = %q, want \"first edited\"", byID(d.StagedComments, cid1).Body)
	}
	if byID(d.StagedComments, cid2).Body != "second" {
		t.Errorf("sibling comment 2 must be untouched, got %q", byID(d.StagedComments, cid2).Body)
	}

	// Delete the second staged comment; only the first remains.
	if err := client.DeleteStagedReviewComment(context.Background(), cid2); err != nil {
		t.Fatalf("DeleteStagedReviewComment: %v", err)
	}
	d, _ = domain.ParseReviewDeliverableDetails(listConversationDeliverables(t, stores, info.ConversationID)[0].DetailsJSON)
	if len(d.StagedComments) != 1 || d.StagedComments[0].ID != cid1 {
		t.Fatalf("delete must leave only comment 1, got %+v", d.StagedComments)
	}

	// An unknown id is rejected on both verbs (most likely a numeric REST id sent
	// to the staged path).
	if err := client.UpdateStagedReviewComment(context.Background(), "nope", "x"); err == nil {
		t.Error("update with an unknown staged id must error")
	}
	if err := client.DeleteStagedReviewComment(context.Background(), "nope"); err == nil {
		t.Error("delete with an unknown staged id must error")
	}
}

// TestLocalClient_StagedReviewComment_RejectedAfterFinalize pins that once a
// review is finalized (ready sentinel set, Proposed snapshot frozen), an agent
// looping after finalize-review can't mutate the staged set — update and delete
// both reject, mirroring add-review-comment's guard, so the approve-time
// proposed-vs-live diff isn't corrupted. The comment survives untouched.
func TestLocalClient_StagedReviewComment_RejectedAfterFinalize(t *testing.T) {
	head := "commit_v1"
	srv := reviewDiffServer(t, &head)
	stores, info, client := newGithubRecordingClient(t, srv.URL, true)

	handle, err := client.GithubCreatePendingReview(context.Background(), "octo", "repo", 7, "headsha7", nil)
	if err != nil {
		t.Fatalf("GithubCreatePendingReview: %v", err)
	}
	cid, err := client.GithubAddPendingReviewComment(context.Background(), "octo", "repo", handle, "a.go", "first", 3, nil, "worktree_head")
	if err != nil {
		t.Fatalf("add comment: %v", err)
	}
	if _, err := client.FinalizeReviewDraft(context.Background(), handle, "COMMENT", "## body"); err != nil {
		t.Fatalf("FinalizeReviewDraft: %v", err)
	}

	if err := client.UpdateStagedReviewComment(context.Background(), cid, "sneaky post-finalize edit"); err == nil {
		t.Error("update on a finalized review must be rejected")
	}
	if err := client.DeleteStagedReviewComment(context.Background(), cid); err == nil {
		t.Error("delete on a finalized review must be rejected")
	}

	// The staged comment + frozen proposed snapshot are untouched.
	d, _ := domain.ParseReviewDeliverableDetails(listConversationDeliverables(t, stores, info.ConversationID)[0].DetailsJSON)
	if len(d.StagedComments) != 1 || byID(d.StagedComments, cid).Body != "first" {
		t.Errorf("a rejected mutation must leave the staged comment intact, got %+v", d.StagedComments)
	}
	if len(d.Proposed.Comments) != 1 || d.Proposed.Comments[0].Body != "first" {
		t.Errorf("the frozen proposed snapshot must be intact, got %+v", d.Proposed)
	}
}

// byID returns the staged comment with the given id, or a zero value.
func byID(comments []domain.ReviewDeliverableComment, id string) domain.ReviewDeliverableComment {
	for _, c := range comments {
		if c.ID == id {
			return c
		}
	}
	return domain.ReviewDeliverableComment{}
}

// TestLocalClient_GithubAddPendingReviewComment_DedupsExactMatch pins that a
// second add-review-comment call with the exact same (path, line, start_line,
// body) as an already-staged comment is a no-op: it returns the EXISTING
// comment id instead of appending a duplicate, so a confused/retrying agent
// can't stage the same finding twice. A same-line comment with a different
// body is a distinct finding and stages as normal (not collapsed).
func TestLocalClient_GithubAddPendingReviewComment_DedupsExactMatch(t *testing.T) {
	head := "live_head"
	srv := reviewDiffServer(t, &head)
	stores, info, client := newGithubRecordingClient(t, srv.URL, true)

	handle, err := client.GithubCreatePendingReview(context.Background(), "octo", "repo", 7, "headsha7", nil)
	if err != nil {
		t.Fatalf("GithubCreatePendingReview: %v", err)
	}

	const anchor = "worktree_head"
	cid1, err := client.GithubAddPendingReviewComment(context.Background(), "octo", "repo", handle, "a.go", "nit: rename", 3, nil, anchor)
	if err != nil {
		t.Fatalf("add comment 1: %v", err)
	}

	// Exact repeat: same path, line, start_line (nil), and body.
	cid2, err := client.GithubAddPendingReviewComment(context.Background(), "octo", "repo", handle, "a.go", "nit: rename", 3, nil, anchor)
	if err != nil {
		t.Fatalf("add comment 2 (duplicate): %v", err)
	}
	if cid2 != cid1 {
		t.Errorf("duplicate add returned id %q, want the existing id %q", cid2, cid1)
	}

	arts := listConversationDeliverables(t, stores, info.ConversationID)
	d, _ := domain.ParseReviewDeliverableDetails(arts[0].DetailsJSON)
	if len(d.StagedComments) != 1 {
		t.Fatalf("exact duplicate must not append a second staged comment, got %d: %+v", len(d.StagedComments), d.StagedComments)
	}
	if d.StagedComments[0].CommitSHA != anchor {
		t.Errorf("a repeat with the SAME anchor must not disturb CommitSHA, got %q, want %q", d.StagedComments[0].CommitSHA, anchor)
	}

	// Same line, different body: a distinct finding, stages normally.
	cid3, err := client.GithubAddPendingReviewComment(context.Background(), "octo", "repo", handle, "a.go", "different finding", 3, nil, anchor)
	if err != nil {
		t.Fatalf("add comment 3 (distinct): %v", err)
	}
	if cid3 == cid1 {
		t.Errorf("a different body on the same line must NOT dedup, got the same id %q", cid3)
	}
	arts = listConversationDeliverables(t, stores, info.ConversationID)
	d, _ = domain.ParseReviewDeliverableDetails(arts[0].DetailsJSON)
	if len(d.StagedComments) != 2 {
		t.Fatalf("a same-line different-body comment must stage separately, got %d: %+v", len(d.StagedComments), d.StagedComments)
	}

	// Multi-line range: a repeat with a non-nil start_line must dedup by VALUE
	// (pointer identity would never match across two separate CLI calls, since
	// each call mints its own *int) — pins that start_line is compared by
	// dereferenced value, not coincidentally via a nil/nil pointer match.
	cid4, err := client.GithubAddPendingReviewComment(context.Background(), "octo", "repo", handle, "a.go", "multi-line finding", 3, intPtr(2), anchor)
	if err != nil {
		t.Fatalf("add comment 4 (multi-line): %v", err)
	}
	cid5, err := client.GithubAddPendingReviewComment(context.Background(), "octo", "repo", handle, "a.go", "multi-line finding", 3, intPtr(2), anchor)
	if err != nil {
		t.Fatalf("add comment 5 (multi-line duplicate): %v", err)
	}
	if cid5 != cid4 {
		t.Errorf("a multi-line duplicate (start_line=2, line=3) returned id %q, want the existing id %q", cid5, cid4)
	}
	arts = listConversationDeliverables(t, stores, info.ConversationID)
	d, _ = domain.ParseReviewDeliverableDetails(arts[0].DetailsJSON)
	if len(d.StagedComments) != 3 {
		t.Fatalf("a multi-line exact duplicate must not append a second staged comment, got %d: %+v", len(d.StagedComments), d.StagedComments)
	}
	// A different start_line on the same line/body is a distinct range, not a dup.
	cid6, err := client.GithubAddPendingReviewComment(context.Background(), "octo", "repo", handle, "a.go", "multi-line finding", 3, intPtr(1), anchor)
	if err != nil {
		t.Fatalf("add comment 6 (different start_line): %v", err)
	}
	if cid6 == cid4 {
		t.Errorf("a different start_line must NOT dedup against the start_line=2 comment, got the same id %q", cid6)
	}
}

// intPtr returns a pointer to v, for constructing multi-line review-comment
// start_line arguments in tests.
func intPtr(v int) *int { return &v }

// TestLocalClient_GithubAddPendingReviewComment_DedupRefreshesStaleAnchor pins
// that a dedup hit is NOT a pure no-op when the anchor commit differs: the
// scenario is an agent whose checkout advances (a pull, a rebase) between two
// otherwise-identical add-review-comment calls for the same finding. Content
// matches, so it dedups — but blindly keeping the FIRST call's CommitSHA would
// leave the one surviving comment anchored to an older commit than the agent
// most recently validated against, which is exactly the stale-anchor risk
// finalize's forward-reconcile (TFAC-499) exists to avoid. The fix re-anchors
// the existing comment to the newer, already-validated commitSHA in place.
func TestLocalClient_GithubAddPendingReviewComment_DedupRefreshesStaleAnchor(t *testing.T) {
	head := "live_head"
	srv := reviewDiffServer(t, &head)
	stores, info, client := newGithubRecordingClient(t, srv.URL, true)

	handle, err := client.GithubCreatePendingReview(context.Background(), "octo", "repo", 7, "headsha7", nil)
	if err != nil {
		t.Fatalf("GithubCreatePendingReview: %v", err)
	}

	cid1, err := client.GithubAddPendingReviewComment(context.Background(), "octo", "repo", handle, "a.go", "nit: rename", 3, nil, "commit_v1")
	if err != nil {
		t.Fatalf("add comment (commit_v1): %v", err)
	}

	// The agent's checkout advances; it revalidates and re-adds the SAME finding
	// against the new head. Content is identical, so this dedups against cid1 —
	// but the anchor must move forward to commit_v2, not stay pinned to commit_v1.
	cid2, err := client.GithubAddPendingReviewComment(context.Background(), "octo", "repo", handle, "a.go", "nit: rename", 3, nil, "commit_v2")
	if err != nil {
		t.Fatalf("add comment (commit_v2, dedup): %v", err)
	}
	if cid2 != cid1 {
		t.Errorf("dedup across a changed anchor returned id %q, want the existing id %q", cid2, cid1)
	}

	arts := listConversationDeliverables(t, stores, info.ConversationID)
	d, _ := domain.ParseReviewDeliverableDetails(arts[0].DetailsJSON)
	if len(d.StagedComments) != 1 {
		t.Fatalf("dedup across a changed anchor must not append a second staged comment, got %d: %+v", len(d.StagedComments), d.StagedComments)
	}
	if d.StagedComments[0].CommitSHA != "commit_v2" {
		t.Errorf("dedup hit must refresh the anchor to the newer commit_v2, got %q", d.StagedComments[0].CommitSHA)
	}
}

// TestLocalClient_PerCommentHeadSHA_CapturedAcrossCheckoutAdvance pins that each
// staged comment records the worktree HEAD it was authored against — so an agent
// that pulls new commits between two adds yields two comments anchored to
// different commits (the signal the submit-time agreement check refuses on,
// instead of mis-anchoring).
func TestLocalClient_PerCommentHeadSHA_CapturedAcrossCheckoutAdvance(t *testing.T) {
	head := "live_head"
	srv := reviewDiffServer(t, &head)
	stores, info, client := newGithubRecordingClient(t, srv.URL, true)

	handle, err := client.GithubCreatePendingReview(context.Background(), "octo", "repo", 7, "commit_v1", nil)
	if err != nil {
		t.Fatalf("create review: %v", err)
	}
	// First comment authored against the initial checkout.
	if _, err := client.GithubAddPendingReviewComment(context.Background(), "octo", "repo", handle, "a.go", "first", 3, nil, "commit_v1"); err != nil {
		t.Fatalf("add comment 1: %v", err)
	}
	// The agent pulls: its checkout advances to a new commit.
	if _, err := client.GithubAddPendingReviewComment(context.Background(), "octo", "repo", handle, "a.go", "second", 4, nil, "commit_v2"); err != nil {
		t.Fatalf("add comment 2: %v", err)
	}

	arts := listConversationDeliverables(t, stores, info.ConversationID)
	d, _ := domain.ParseReviewDeliverableDetails(arts[0].DetailsJSON)
	if len(d.StagedComments) != 2 {
		t.Fatalf("want 2 staged comments, got %d", len(d.StagedComments))
	}
	if d.StagedComments[0].CommitSHA != "commit_v1" || d.StagedComments[1].CommitSHA != "commit_v2" {
		t.Errorf("per-comment anchors = %q,%q, want commit_v1,commit_v2",
			d.StagedComments[0].CommitSHA, d.StagedComments[1].CommitSHA)
	}
}

// TestReviewDraftTarget_ReturnsRecordedPR pins the read add-review-comment
// finds the reviewed PR's checkout through: the coordinates start-review
// recorded on the draft, over the in-process client and over the socket the
// jailed CLI dials. A handle that names no draft of this conversation is
// refused rather than answered.
func TestReviewDraftTarget_ReturnsRecordedPR(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !answerPRRead(w, r) {
			t.Errorf("unexpected GitHub call: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(gh.Close)
	stores, info, local := newGithubRecordingClient(t, gh.URL, true)
	registerRepo(t, stores, "octo", "repo", testRepoID("octo/repo"))
	handle, err := local.GithubCreatePendingReview(context.Background(), "octo", "repo", 7, "headsha7", nil)
	if err != nil {
		t.Fatalf("GithubCreatePendingReview: %v", err)
	}

	sockPath := tempSocket(t)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := NewServer(stores, info, nil)
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() {
		_ = listener.Close()
		_ = srv.Shutdown(context.Background())
	})
	ipc := Dial(sockPath)
	t.Cleanup(func() { _ = ipc.Close() })

	for name, c := range map[string]Client{"local": local, "ipc": ipc} {
		owner, repo, number, err := c.ReviewDraftTarget(context.Background(), handle)
		if err != nil || owner != "octo" || repo != "repo" || number != 7 {
			t.Errorf("%s: got (%s/%s#%d, %v), want octo/repo#7", name, owner, repo, number, err)
		}
		if _, _, _, err := c.ReviewDraftTarget(context.Background(), "not-a-review"); err == nil {
			t.Errorf("%s: an unknown handle should be refused", name)
		}
	}
}

// TestReviewDraftTarget_FollowsTheRegistryRow: the reviewed pull request is
// read by the registry row the draft's entity references, not parsed from the
// draft's target. A rename that rewrites no key is followed, in process and
// relayed; a retired row is reported and refused; an id naming none of the
// conversation's deliverables resolves nothing.
func TestReviewDraftTarget_FollowsTheRegistryRow(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !answerPRRead(w, r) {
			t.Errorf("unexpected GitHub call: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(gh.Close)
	conn, stores, info, local := newGithubRecordingClientConn(t, gh.URL, true)
	repoID := registerRepo(t, stores, "octo", "repo", testRepoID("octo/repo"))
	ctx := context.Background()
	handle, err := local.GithubCreatePendingReview(ctx, "octo", "repo", 7, "headsha7", nil)
	if err != nil {
		t.Fatalf("GithubCreatePendingReview: %v", err)
	}
	if arts := listConversationDeliverables(t, stores, info.ConversationID); len(arts) != 1 || arts[0].EntityID == "" {
		t.Fatalf("review draft = %+v, want one linked to its pull request's entity", arts)
	}
	dbseed.RenameRepositoryOnly(t, conn, repoID, "acme", "renamed")

	relayed := newRelayRuntime(directDispatchConn{srv: NewRelayServer(stores, info, nil)}, info, nil)
	pr, err := relayed.DeliverablePullRequest(ctx, handle)
	if err != nil || pr.RepositoryID != repoID || pr.Slug() != "acme/renamed" || pr.Number != 7 || pr.Retired {
		t.Errorf("relayed DeliverablePullRequest = (%+v, %v), want acme/renamed#7 on row %s", pr, err, repoID)
	}
	if _, err := relayed.DeliverablePullRequest(ctx, "not-an-deliverable"); err == nil {
		t.Error("an id naming none of the conversation's deliverables resolved a pull request")
	}
	if owner, repo, number, err := local.ReviewDraftTarget(ctx, handle); err != nil || owner != "acme" || repo != "renamed" || number != 7 {
		t.Errorf("ReviewDraftTarget = (%s/%s#%d, %v), want acme/renamed#7", owner, repo, number, err)
	}

	if _, err := conn.Exec(`UPDATE repositories SET retired_at = CURRENT_TIMESTAMP WHERE id = ?`, repoID); err != nil {
		t.Fatalf("retire repository: %v", err)
	}
	if pr, err := relayed.DeliverablePullRequest(ctx, handle); err != nil || !pr.Retired {
		t.Errorf("relayed DeliverablePullRequest after retire = (%+v, %v), want it reported retired", pr, err)
	}
	if _, _, _, err := local.ReviewDraftTarget(ctx, handle); err == nil {
		t.Error("ReviewDraftTarget answered for a draft whose repository's row is retired")
	}
}

// TestLocalClient_GithubAddPendingReviewComment_RefusesOtherRepo pins the
// host's own check that a staged comment's repo is the review's. The PR number
// always comes from the draft, so a different owner/repo would validate the
// comment against another repo's PR of the same number. The PR GitHub answers
// for the comment's owner/repo#number carries another node id than the one the
// draft is keyed on, so the comment is refused before any diff is read, and
// nothing is staged.
func TestLocalClient_GithubAddPendingReviewComment_RefusesOtherRepo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/repos/octo/repo/pulls/7") && answerPRRead(w, r) {
			return
		}
		if strings.Contains(r.Header.Get("Accept"), "diff") || !strings.HasSuffix(r.URL.Path, "/repos/octo/other/pulls/7") {
			t.Errorf("unexpected GitHub call for a mismatched repo: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"number":7,"node_id":"PR_kwDOother7","base":{"ref":"main","repo":`+fakeRepoJSON(r.URL.Path)+`},"head":{"sha":"h"}}`)
	}))
	t.Cleanup(srv.Close)
	stores, info, client := newGithubRecordingClient(t, srv.URL, true)
	registerRepo(t, stores, "octo", "repo", testRepoID("octo/repo"))

	handle, err := client.GithubCreatePendingReview(context.Background(), "octo", "repo", 7, "headsha7", nil)
	if err != nil {
		t.Fatalf("GithubCreatePendingReview: %v", err)
	}
	_, err = client.GithubAddPendingReviewComment(context.Background(), "octo", "other", handle, "a.go", "nit", 3, nil, "anchor")
	if err == nil || !strings.Contains(err.Error(), "is for octo/repo#7") {
		t.Fatalf("want a refusal naming the review's PR, got %v", err)
	}
	arts := listConversationDeliverables(t, stores, info.ConversationID)
	d, _ := domain.ParseReviewDeliverableDetails(arts[0].DetailsJSON)
	if len(d.StagedComments) != 0 {
		t.Errorf("a refused comment must not be staged, got %+v", d.StagedComments)
	}
}

// TestLocalClient_GithubAddPendingReviewComment_AcceptsAnotherSpelling pins the
// other half of that check: an owner/repo GitHub resolves to the pull request
// the draft is keyed on — the repository's old name, or another case — is the
// review's repository, and the comment stages.
func TestLocalClient_GithubAddPendingReviewComment_AcceptsAnotherSpelling(t *testing.T) {
	head := "commit_v1"
	srv := reviewDiffServer(t, &head)
	stores, info, client := newGithubRecordingClient(t, srv.URL, true)
	registerRepo(t, stores, "octo", "repo", testRepoID("octo/repo"))

	handle, err := client.GithubCreatePendingReview(context.Background(), "octo", "repo", 7, "headsha7", nil)
	if err != nil {
		t.Fatalf("GithubCreatePendingReview: %v", err)
	}
	if _, err := client.GithubAddPendingReviewComment(context.Background(), "Octo", "old-repo", handle, "a.go", "nit", 3, nil, "worktree_head"); err != nil {
		t.Fatalf("GithubAddPendingReviewComment under another name of the repository: %v", err)
	}
	arts := listConversationDeliverables(t, stores, info.ConversationID)
	d, _ := domain.ParseReviewDeliverableDetails(arts[0].DetailsJSON)
	if len(d.StagedComments) != 1 {
		t.Errorf("staged comments = %+v, want the one comment", d.StagedComments)
	}
}
