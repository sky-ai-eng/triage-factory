package reconcile

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// --- white-box unit tests for the pure mapping helpers ---

func TestPrState(t *testing.T) {
	cases := []struct {
		name string
		snap domain.PRSnapshot
		want string
	}{
		{"merged flag", domain.PRSnapshot{Merged: true, State: "OPEN"}, domain.ArtifactStatePRMerged},
		{"merged state", domain.PRSnapshot{State: "MERGED"}, domain.ArtifactStatePRMerged},
		{"closed", domain.PRSnapshot{State: "CLOSED"}, domain.ArtifactStatePRClosed},
		{"draft", domain.PRSnapshot{State: "OPEN", IsDraft: true}, domain.ArtifactStatePRDraft},
		{"open", domain.PRSnapshot{State: "OPEN"}, domain.ArtifactStatePROpen},
		// Merged takes precedence over closed even if both somehow read truthy.
		{"merged beats closed", domain.PRSnapshot{Merged: true, State: "CLOSED"}, domain.ArtifactStatePRMerged},
	}
	for _, c := range cases {
		if got := prState(c.snap); got != c.want {
			t.Errorf("%s: prState = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestReviewState(t *testing.T) {
	snap := domain.PRSnapshot{Reviews: []domain.ReviewState{
		{ID: "REV_sub", State: "APPROVED"},
		{ID: "REV_dis", State: "DISMISSED"},
		{ID: "REV_pend", State: "PENDING"},
	}}
	cases := []struct {
		name      string
		reviewID  string
		wantState string
		wantOK    bool
	}{
		{"submitted (approved)", "REV_sub", domain.ArtifactStateReviewSubmitted, true},
		{"dismissed", "REV_dis", domain.ArtifactStateReviewDismissed, true},
		{"still pending → no transition", "REV_pend", "", false},
		{"not in latestReviews → no transition", "REV_missing", "", false},
		{"empty id → no transition", "", "", false},
	}
	for _, c := range cases {
		gotState, gotOK := reviewState(snap, c.reviewID)
		if gotState != c.wantState || gotOK != c.wantOK {
			t.Errorf("%s: reviewState = (%q, %v), want (%q, %v)", c.name, gotState, gotOK, c.wantState, c.wantOK)
		}
	}
}

func TestReviewState_ChangesRequestedIsSubmitted(t *testing.T) {
	// A REQUEST_CHANGES / COMMENTED review is "submitted" (landed on the PR),
	// not dismissed — only DISMISSED maps to dismissed.
	snap := domain.PRSnapshot{Reviews: []domain.ReviewState{{ID: "R", State: "CHANGES_REQUESTED"}}}
	if s, ok := reviewState(snap, "R"); !ok || s != domain.ArtifactStateReviewSubmitted {
		t.Errorf("CHANGES_REQUESTED → (%q,%v), want (submitted,true)", s, ok)
	}
}

func TestBranchRefOf(t *testing.T) {
	a, _ := domain.NewBranchArtifact("octo/repo", "refs/heads/feature/x", "sha", true)
	ref, ok := branchRefOf(a)
	if !ok || ref.Owner != "octo" || ref.Repo != "repo" || ref.Branch != "feature/x" {
		t.Errorf("branchRefOf = (%+v, %v), want octo/repo/feature/x", ref, ok)
	}
	// Malformed coordinates are rejected, not guessed.
	if _, ok := branchRefOf(domain.Artifact{Target: "octo", ExternalID: "refs/heads/x"}); ok {
		t.Error("single-segment target should be rejected")
	}
	if _, ok := branchRefOf(domain.Artifact{Target: "octo/repo", ExternalID: "refs/tags/v1"}); ok {
		t.Error("non-branch ref should be rejected")
	}
}

func TestIsTerminalState(t *testing.T) {
	terminal := []struct{ kind, state string }{
		{domain.ArtifactKindPullRequest, domain.ArtifactStatePRMerged},
		{domain.ArtifactKindPullRequest, domain.ArtifactStatePRClosed},
		{domain.ArtifactKindReview, domain.ArtifactStateReviewSubmitted},
		{domain.ArtifactKindReview, domain.ArtifactStateReviewDismissed},
		{domain.ArtifactKindBranch, domain.ArtifactStateBranchDeleted},
	}
	for _, c := range terminal {
		if !isTerminalState(c.kind, c.state) {
			t.Errorf("isTerminalState(%s,%s) = false, want true", c.kind, c.state)
		}
	}
	nonTerminal := []struct{ kind, state string }{
		{domain.ArtifactKindPullRequest, domain.ArtifactStatePROpen},
		{domain.ArtifactKindPullRequest, domain.ArtifactStatePRDraft},
		{domain.ArtifactKindReview, domain.ArtifactStateReviewPending},
		{domain.ArtifactKindBranch, domain.ArtifactStateBranchPushed},
	}
	for _, c := range nonTerminal {
		if isTerminalState(c.kind, c.state) {
			t.Errorf("isTerminalState(%s,%s) = true, want false", c.kind, c.state)
		}
	}
}

func TestFormatTitleBodyDelta(t *testing.T) {
	// Unchanged → empty (the caller renders "shipped as you drafted them").
	if got := formatTitleBodyDelta("T", "B", "T", "B"); got != "" {
		t.Errorf("unchanged delta = %q, want empty", got)
	}
	// Title only.
	got := formatTitleBodyDelta("draft title", "B", "shipped title", "B")
	if !strings.Contains(got, "Title") || !strings.Contains(got, "shipped title") || !strings.Contains(got, "draft title") {
		t.Errorf("title delta = %q, want both shipped + drafted titles", got)
	}
	if strings.Contains(got, "Description") {
		t.Errorf("unchanged body should not appear: %q", got)
	}
	// Body only → the shipped body is quoted.
	got = formatTitleBodyDelta("T", "old body", "T", "new body line1\nline2")
	if !strings.Contains(got, "Description") || !strings.Contains(got, "> new body line1") || !strings.Contains(got, "> line2") {
		t.Errorf("body delta = %q, want the final body blockquoted", got)
	}
}

func TestDescribeArtifactOutcome_Dispositions(t *testing.T) {
	// The non-merged terminal kinds render disposition text without touching the
	// resolver, so a zero-value Reconciler is enough.
	rc := &Reconciler{}
	cases := []struct {
		art  domain.Artifact
		want string
	}{
		{domain.Artifact{Kind: domain.ArtifactKindPullRequest, Target: "octo/repo#1", State: domain.ArtifactStatePRClosed}, "closed without merging"},
		{domain.Artifact{Kind: domain.ArtifactKindReview, Target: "octo/repo#2", State: domain.ArtifactStateReviewSubmitted}, "submitted"},
		{domain.Artifact{Kind: domain.ArtifactKindReview, Target: "octo/repo#3", State: domain.ArtifactStateReviewDismissed}, "dismissed"},
		{domain.Artifact{Kind: domain.ArtifactKindBranch, Target: "octo/repo", ExternalID: "refs/heads/feat", State: domain.ArtifactStateBranchDeleted}, "deleted"},
		// Non-terminal → no block.
		{domain.Artifact{Kind: domain.ArtifactKindPullRequest, Target: "octo/repo#4", State: domain.ArtifactStatePROpen}, ""},
	}
	for _, c := range cases {
		got := rc.describeArtifactOutcome(context.Background(), "org", c.art)
		if c.want == "" {
			if got != "" {
				t.Errorf("describeArtifactOutcome(%s) = %q, want empty", c.art.State, got)
			}
			continue
		}
		if !strings.Contains(got, c.want) {
			t.Errorf("describeArtifactOutcome(%s) = %q, want to contain %q", c.art.State, got, c.want)
		}
	}
	// Branch block names the branch (not the ref) and the repo.
	br := domain.Artifact{Kind: domain.ArtifactKindBranch, Target: "octo/repo", ExternalID: "refs/heads/feat", State: domain.ArtifactStateBranchDeleted}
	if got := rc.describeArtifactOutcome(context.Background(), "org", br); !strings.Contains(got, "`feat`") || !strings.Contains(got, "`octo/repo`") {
		t.Errorf("branch block = %q, want to name `feat` in `octo/repo`", got)
	}
}

func TestVerdictMatches(t *testing.T) {
	// Draft event vocab (APPROVE) normalizes to GitHub state vocab (APPROVED).
	for _, c := range []struct {
		event, state string
		want         bool
	}{
		{"APPROVE", "APPROVED", true},
		{"REQUEST_CHANGES", "CHANGES_REQUESTED", true},
		{"COMMENT", "COMMENTED", true},
		{"APPROVE", "CHANGES_REQUESTED", false},
	} {
		if got := verdictMatches(c.event, c.state); got != c.want {
			t.Errorf("verdictMatches(%q,%q) = %v, want %v", c.event, c.state, got, c.want)
		}
	}
}

func TestFormatReviewDelta(t *testing.T) {
	line5, line7 := 5, 7

	// With a draft: verdict change, body edit, a per-comment edit, and an add.
	proposed := domain.ReviewArtifactProposed{
		Body:  "draft summary",
		Event: "COMMENT",
		Comments: []domain.ReviewArtifactComment{
			{ID: "PRRC_1", Path: "a.go", Line: &line5, Body: "draft comment"},
		},
	}
	final := github.SubmittedReview{
		State: "CHANGES_REQUESTED",
		Body:  "final summary",
		Comments: []github.PendingReviewComment{
			{ID: "PRRC_1", Path: "a.go", Line: &line5, Body: "final comment"},
			{ID: "PRRC_2", Path: "b.go", Line: &line7, Body: "new thing"},
		},
	}
	got := formatReviewDelta(proposed, final)
	for _, want := range []string{
		"Verdict shipped as changes requested",
		"Body was edited",
		"`a.go:5` — edited before submit",
		"final comment",
		"`b.go:7` — added before submit",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("review delta missing %q; got:\n%s", want, got)
		}
	}

	// A dropped comment.
	dropped := formatReviewDelta(
		domain.ReviewArtifactProposed{Event: "COMMENT", Comments: []domain.ReviewArtifactComment{{ID: "PRRC_9", Path: "c.go", Line: &line5, Body: "gone"}}},
		github.SubmittedReview{State: "COMMENTED"},
	)
	if !strings.Contains(dropped, "`c.go:5` — dropped before submit") {
		t.Errorf("dropped-comment delta = %q", dropped)
	}

	// No draft (Proposed empty) → record the final content instead of a diff.
	noDraft := formatReviewDelta(domain.ReviewArtifactProposed{}, github.SubmittedReview{
		State:    "APPROVED",
		Comments: []github.PendingReviewComment{{ID: "X", Path: "a.go", Line: &line5, Body: "looks good"}},
	})
	if !strings.Contains(noDraft, "Verdict: approved") || !strings.Contains(noDraft, "`a.go:5` — looks good") {
		t.Errorf("no-draft final-review record = %q", noDraft)
	}
}

// --- integration: Reconcile end-to-end against a stub GitHub + real SQLite ---

// stubGH is a canned GitHub endpoint for RefreshPRs (GraphQL nodes query),
// BranchesExist (GraphQL aliased repository query), and GetPRBasic (REST
// GET /pulls/{n}). It records which PR node ids and branch refs were queried so
// a test can assert terminal artifacts are never fetched.
type stubGH struct {
	prs      map[string]string // PR node id → JSON node body (RefreshPRs)
	branches map[string]bool   // "owner/repo/branch" → exists; absent key = unknown
	prFinal  map[int][2]string // PR number → {title, body} for GetPRBasic; absent → 404
	reviews  map[string]string // review node id → PullRequestReview JSON body (GetReview); absent → node null
	prCalls  map[string]bool   // PR node ids actually requested
	refCalls map[string]bool   // branch refs actually requested ("owner/repo/branch")
}

func newStubClient(t *testing.T, s *stubGH) *github.Client {
	t.Helper()
	s.prCalls = map[string]bool{}
	s.refCalls = map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// GetPRBasic is a REST GET to /repos/{o}/{r}/pulls/{n}; GraphQL is always
		// POST. Route on method.
		if r.Method == http.MethodGet {
			seg := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			number, _ := strconv.Atoi(seg[len(seg)-1])
			tb, ok := s.prFinal[number]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"Not Found"}`))
				return
			}
			body, _ := json.Marshal(map[string]any{
				"number": number, "node_id": fmt.Sprintf("PR_%d", number),
				"title": tb[0], "body": tb[1], "merged": true, "state": "closed",
			})
			_, _ = w.Write(body)
			return
		}

		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		switch {
		case strings.Contains(req.Query, "nodes(ids:"):
			ids, _ := req.Variables["ids"].([]any)
			parts := make([]string, 0, len(ids))
			for _, idv := range ids {
				id, _ := idv.(string)
				s.prCalls[id] = true
				if body, ok := s.prs[id]; ok {
					parts = append(parts, body)
				} else {
					parts = append(parts, "null")
				}
			}
			fmt.Fprintf(w, `{"data":{"nodes":[%s]}}`, strings.Join(parts, ","))

		case strings.Contains(req.Query, "repository("):
			var fields []string
			for i := 0; ; i++ {
				ov, ok := req.Variables[fmt.Sprintf("o%d", i)].(string)
				if !ok {
					break
				}
				nv, _ := req.Variables[fmt.Sprintf("n%d", i)].(string)
				qv, _ := req.Variables[fmt.Sprintf("q%d", i)].(string)
				key := ov + "/" + nv + "/" + strings.TrimPrefix(qv, "refs/heads/")
				s.refCalls[key] = true
				switch exists, known := s.branches[key]; {
				case !known:
					fields = append(fields, fmt.Sprintf(`"r%d":null`, i)) // unknown
				case exists:
					fields = append(fields, fmt.Sprintf(`"r%d":{"ref":{"id":"REF"}}`, i))
				default:
					fields = append(fields, fmt.Sprintf(`"r%d":{"ref":null}`, i)) // gone
				}
			}
			fmt.Fprintf(w, `{"data":{%s}}`, strings.Join(fields, ","))

		case strings.Contains(req.Query, "PullRequestReview"):
			// GetReview(node(id:)). Return the canned review body, or node:null.
			id, _ := req.Variables["id"].(string)
			if body, ok := s.reviews[id]; ok {
				fmt.Fprintf(w, `{"data":{"node":%s}}`, body)
			} else {
				_, _ = w.Write([]byte(`{"data":{"node":null}}`))
			}

		default:
			_, _ = w.Write([]byte(`{"data":{}}`))
		}
	}))
	t.Cleanup(srv.Close)
	return github.NewClient(srv.URL, "test-token")
}

// prNodeJSON builds one PR node body for the RefreshPRs response. reviews are
// {id,state} pairs surfaced under latestReviews.
func prNodeJSON(nodeID string, number int, state string, draft, merged bool, reviews ...[2]string) string {
	revNodes := make([]string, 0, len(reviews))
	for _, rv := range reviews {
		revNodes = append(revNodes, fmt.Sprintf(`{"id":%q,"author":{"login":"bot"},"state":%q,"submittedAt":"2026-01-01T00:00:00Z"}`, rv[0], rv[1]))
	}
	return fmt.Sprintf(`{"id":%q,"number":%d,"repository":{"nameWithOwner":"octo/repo"},"url":"https://github.com/octo/repo/pull/%d","state":%q,"isDraft":%t,"merged":%t,"latestReviews":{"nodes":[%s]}}`,
		nodeID, number, number, state, draft, merged, strings.Join(revNodes, ","))
}

type fakeResolver struct {
	client *github.Client
	owners map[string]bool // owners ClientFor was asked for
}

func (f *fakeResolver) ClientFor(_ context.Context, _, target string) (*github.Client, error) {
	if f.owners == nil {
		f.owners = map[string]bool{}
	}
	f.owners[target] = true
	return f.client, nil
}

func reconcileTestStores(t *testing.T) (db.Stores, func(entityID, runID, content string), func(a domain.Artifact)) {
	t.Helper()
	conn, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = conn.Close() })
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	seedRun := func(entityID, runID, content string) {
		if _, err := conn.Exec(`INSERT INTO entities (id, source, source_id, kind) VALUES (?, 'github', ?, 'pull_request')`, entityID, entityID); err != nil {
			t.Fatalf("seed entity: %v", err)
		}
		if _, err := conn.Exec(`INSERT INTO runs (id, origin, status) VALUES (?, 'interactive', 'completed')`, runID); err != nil {
			t.Fatalf("seed run: %v", err)
		}
		if err := stores.TaskMemory.UpsertAgentMemory(ctx, runmode.LocalDefaultOrgID, runID, entityID, "", content); err != nil {
			t.Fatalf("seed memory: %v", err)
		}
	}
	seedArt := func(a domain.Artifact) {
		a.OrgID = runmode.LocalDefaultOrgID
		a.TeamID = runmode.LocalDefaultTeamID
		if _, err := stores.Artifacts.Upsert(ctx, runmode.LocalDefaultOrgID, a); err != nil {
			t.Fatalf("seed artifact %s: %v", a.DedupKey, err)
		}
	}
	return stores, seedRun, seedArt
}

// TestReconcile_TransitionsAndFinalMemory is the headline end-to-end: a draft PR
// that merged, a pending review that was submitted, and a pushed branch that was
// deleted all reflect their new state; the run's post-run memory is ONE composed
// note covering all three (proving multi-artifact runs accumulate, not clobber),
// with the merged PR diffed against the agent's draft; and a terminal artifact
// already in the set is never re-queried.
func TestReconcile_TransitionsAndFinalMemory(t *testing.T) {
	stores, seedRun, seedArt := reconcileTestStores(t)
	ctx := context.Background()
	const runID = "11111111-1111-1111-1111-111111111111"
	seedRun("ent-1", runID, "agent narrative")

	prArt := domain.NewPullRequestArtifact("octo/repo", 1, "PR_1", "feat", "main", "https://github.com/octo/repo/pull/1", "t", "b", true)
	prArt.RunID = runID
	reviewArt := domain.NewReviewArtifact("octo/repo", 2, "PR_2", "REV_2")
	reviewArt.RunID = runID
	branchArt, _ := domain.NewBranchArtifact("octo/repo", "refs/heads/feature", "sha", true)
	branchArt.RunID = runID
	// A terminal PR already merged — it must NOT be in the non-terminal set and
	// must never be fetched.
	mergedArt := domain.NewPullRequestArtifact("octo/repo", 9, "PR_9", "old", "main", "https://github.com/octo/repo/pull/9", "t", "b", false)
	mergedArt.State = domain.ArtifactStatePRMerged
	mergedArt.RunID = runID
	seedArt(prArt)
	seedArt(reviewArt)
	seedArt(branchArt)
	seedArt(mergedArt)

	stub := &stubGH{
		prs: map[string]string{
			"PR_1": prNodeJSON("PR_1", 1, "MERGED", false, true),
			"PR_2": prNodeJSON("PR_2", 2, "OPEN", false, false, [2]string{"REV_2", "APPROVED"}),
		},
		branches: map[string]bool{"octo/repo/feature": false}, // gone
		// PR_1 shipped with an edited title vs the agent's draft ("t"/"b").
		prFinal: map[int][2]string{1: {"shipped title", "b"}},
	}
	res := &fakeResolver{client: newStubClient(t, stub)}
	rc := NewReconciler(res, stores.Artifacts, stores.TaskMemory, nil)

	if err := rc.ReconcileOrg(ctx, runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("ReconcileOrg: %v", err)
	}

	// States transitioned.
	assertState(t, stores, runmode.LocalDefaultOrgID, prArt.DedupKey, domain.ArtifactStatePRMerged)
	assertState(t, stores, runmode.LocalDefaultOrgID, reviewArt.DedupKey, domain.ArtifactStateReviewSubmitted)
	assertState(t, stores, runmode.LocalDefaultOrgID, branchArt.DedupKey, domain.ArtifactStateBranchDeleted)

	// Terminal artifact never re-queried.
	if stub.prCalls["PR_9"] {
		t.Error("terminal merged PR (PR_9) was re-queried; it should be excluded from the non-terminal set")
	}

	// ONE composed note covering all three artifacts, after the agent narrative
	// (which is untouched). Overwrite, not append — but composed over the run's
	// whole set, so nothing is clobbered.
	mem, err := stores.TaskMemory.GetRunMemory(ctx, runmode.LocalDefaultOrgID, runID)
	if err != nil || mem == nil {
		t.Fatalf("GetRunMemory: mem=%v err=%v", mem, err)
	}
	if !strings.HasPrefix(mem.Content, "agent narrative") {
		t.Errorf("agent narrative was trampled: %q", mem.Content)
	}
	for _, want := range []string{"was merged on GitHub", "was submitted on GitHub", "was deleted on GitHub"} {
		if !strings.Contains(mem.Content, want) {
			t.Errorf("composed outcome missing %q; got:\n%s", want, mem.Content)
		}
	}
	// The merged PR carries the title delta vs the agent's draft.
	if !strings.Contains(mem.Content, "shipped title") {
		t.Errorf("merged-PR title delta missing; got:\n%s", mem.Content)
	}
}

// TestReconcile_PRClosedAndReviewDismissed pins the other terminal branches: a
// PR closed-without-merging, and a review dismissed.
func TestReconcile_PRClosedAndReviewDismissed(t *testing.T) {
	stores, seedRun, seedArt := reconcileTestStores(t)
	ctx := context.Background()
	const runID = "22222222-2222-2222-2222-222222222222"
	seedRun("ent-2", runID, "narrative")

	prArt := domain.NewPullRequestArtifact("octo/repo", 3, "PR_3", "x", "main", "u", "t", "b", false)
	prArt.RunID = runID
	reviewArt := domain.NewReviewArtifact("octo/repo", 4, "PR_4", "REV_4")
	reviewArt.RunID = runID
	seedArt(prArt)
	seedArt(reviewArt)

	stub := &stubGH{prs: map[string]string{
		"PR_3": prNodeJSON("PR_3", 3, "CLOSED", false, false),
		"PR_4": prNodeJSON("PR_4", 4, "OPEN", false, false, [2]string{"REV_4", "DISMISSED"}),
	}}
	rc := NewReconciler(&fakeResolver{client: newStubClient(t, stub)}, stores.Artifacts, stores.TaskMemory, nil)
	if err := rc.ReconcileOrg(ctx, runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("ReconcileOrg: %v", err)
	}
	assertState(t, stores, runmode.LocalDefaultOrgID, prArt.DedupKey, domain.ArtifactStatePRClosed)
	assertState(t, stores, runmode.LocalDefaultOrgID, reviewArt.DedupKey, domain.ArtifactStateReviewDismissed)

	// One composed note covers both dispositions.
	mem, _ := stores.TaskMemory.GetRunMemory(ctx, runmode.LocalDefaultOrgID, runID)
	if mem == nil || !strings.Contains(mem.Content, "closed without merging") || !strings.Contains(mem.Content, "dismissed") {
		t.Errorf("composed note missing a disposition; got: %v", mem)
	}
}

// TestReconcile_ReviewContentDiff pins that an externally-submitted review's note
// carries the diff between the agent's drafted review (Proposed) and what landed
// — the content signal, not just the disposition. The review is fetched by its
// node id (GetReview), so a submitted review's body/comments are recovered.
func TestReconcile_ReviewContentDiff(t *testing.T) {
	stores, seedRun, seedArt := reconcileTestStores(t)
	ctx := context.Background()
	const runID = "88888888-8888-8888-8888-888888888888"
	seedRun("ent-8", runID, "narrative")

	// Review artifact with a finalized draft (Proposed) — a comment the human
	// then edits, and a body/verdict that shift before submit.
	reviewArt := domain.NewReviewArtifact("octo/repo", 11, "PR_11", "REV_11")
	d, _ := domain.ParseReviewArtifactDetails(reviewArt.DetailsJSON)
	line := 5
	d.Proposed = domain.ReviewArtifactProposed{
		Body:  "draft summary",
		Event: "COMMENT",
		Comments: []domain.ReviewArtifactComment{
			{ID: "PRRC_1", Path: "a.go", Line: &line, Body: "draft comment"},
		},
	}
	reviewArt.DetailsJSON = domain.MarshalReviewArtifactDetails(d)
	reviewArt.RunID = runID
	seedArt(reviewArt)

	stub := &stubGH{
		// The PR snapshot surfaces REV_11 as a landed (non-pending) review, so
		// the artifact transitions pending → submitted.
		prs: map[string]string{"PR_11": prNodeJSON("PR_11", 11, "OPEN", false, false, [2]string{"REV_11", "CHANGES_REQUESTED"})},
		// GetReview(REV_11) returns the final content: the comment edited, plus
		// a body/verdict shift.
		reviews: map[string]string{"REV_11": `{
			"state":"CHANGES_REQUESTED","body":"final summary",
			"comments":{"nodes":[{"id":"PRRC_1","path":"a.go","line":5,"startLine":null,"body":"final comment"}]}
		}`},
	}
	rc := NewReconciler(&fakeResolver{client: newStubClient(t, stub)}, stores.Artifacts, stores.TaskMemory, nil)
	if err := rc.ReconcileOrg(ctx, runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("ReconcileOrg: %v", err)
	}
	assertState(t, stores, runmode.LocalDefaultOrgID, reviewArt.DedupKey, domain.ArtifactStateReviewSubmitted)

	mem, err := stores.TaskMemory.GetRunMemory(ctx, runmode.LocalDefaultOrgID, runID)
	if err != nil || mem == nil {
		t.Fatalf("GetRunMemory: mem=%v err=%v", mem, err)
	}
	for _, want := range []string{
		"was submitted on GitHub",
		"Verdict shipped as changes requested",
		"Body was edited",
		"`a.go:5` — edited before submit",
		"final comment",
	} {
		if !strings.Contains(mem.Content, want) {
			t.Errorf("review note missing %q; got:\n%s", want, mem.Content)
		}
	}
}

// TestReconcile_NoOpWhenUnchanged pins that an open PR still open, a pending
// review still private, and a present branch all stay put — and a still-pending
// review writes no memory note (no terminal transition).
func TestReconcile_NoOpWhenUnchanged(t *testing.T) {
	stores, seedRun, seedArt := reconcileTestStores(t)
	ctx := context.Background()
	const runID = "33333333-3333-3333-3333-333333333333"
	seedRun("ent-3", runID, "narrative")

	prArt := domain.NewPullRequestArtifact("octo/repo", 5, "PR_5", "x", "main", "u", "t", "b", false) // open
	prArt.RunID = runID
	reviewArt := domain.NewReviewArtifact("octo/repo", 6, "PR_6", "REV_6") // pending
	reviewArt.RunID = runID
	branchArt, _ := domain.NewBranchArtifact("octo/repo", "refs/heads/keep", "sha", true)
	branchArt.RunID = runID
	seedArt(prArt)
	seedArt(reviewArt)
	seedArt(branchArt)

	stub := &stubGH{
		prs: map[string]string{
			"PR_5": prNodeJSON("PR_5", 5, "OPEN", false, false),
			// REV_6 still PENDING — surfaced or not, it must not transition.
			"PR_6": prNodeJSON("PR_6", 6, "OPEN", false, false, [2]string{"REV_6", "PENDING"}),
		},
		branches: map[string]bool{"octo/repo/keep": true}, // still there
	}
	updated, err := reconcileSet(t, stores, &fakeResolver{client: newStubClient(t, stub)}, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(updated) != 0 {
		t.Errorf("expected no transitions, got %d: %+v", len(updated), updated)
	}
	assertState(t, stores, runmode.LocalDefaultOrgID, prArt.DedupKey, domain.ArtifactStatePROpen)
	assertState(t, stores, runmode.LocalDefaultOrgID, reviewArt.DedupKey, domain.ArtifactStateReviewPending)
	assertState(t, stores, runmode.LocalDefaultOrgID, branchArt.DedupKey, domain.ArtifactStateBranchPushed)

	mem, _ := stores.TaskMemory.GetRunMemory(ctx, runmode.LocalDefaultOrgID, runID)
	if mem != nil && strings.Contains(mem.Content, "Post-run outcome") {
		t.Errorf("no terminal transition, but a post-run outcome note was written: %q", mem.Content)
	}
}

// TestReconcile_UnknownBranchNotDeleted pins the safety rule: a branch whose
// repository didn't resolve (inaccessible) is left as-is, never flipped to
// deleted on a non-answer.
func TestReconcile_UnknownBranchNotDeleted(t *testing.T) {
	stores, seedRun, seedArt := reconcileTestStores(t)
	const runID = "44444444-4444-4444-4444-444444444444"
	seedRun("ent-4", runID, "narrative")
	branchArt, _ := domain.NewBranchArtifact("octo/repo", "refs/heads/maybe", "sha", true)
	branchArt.RunID = runID
	seedArt(branchArt)

	stub := &stubGH{branches: map[string]bool{}} // empty → repository alias null → unknown
	updated, err := reconcileSet(t, stores, &fakeResolver{client: newStubClient(t, stub)}, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(updated) != 0 {
		t.Errorf("unknown branch should not transition, got %+v", updated)
	}
	assertState(t, stores, runmode.LocalDefaultOrgID, branchArt.DedupKey, domain.ArtifactStateBranchPushed)
}

// TestReconcile_SingleRunScope pins the Tier-2 shape: Reconcile over just one
// run's artifacts transitions exactly those (the run-scoped refresh path).
func TestReconcile_SingleRunScope(t *testing.T) {
	stores, seedRun, seedArt := reconcileTestStores(t)
	ctx := context.Background()
	const runA = "55555555-5555-5555-5555-555555555555"
	const runB = "66666666-6666-6666-6666-666666666666"
	seedRun("ent-a", runA, "a")
	seedRun("ent-b", runB, "b")

	prA := domain.NewPullRequestArtifact("octo/repo", 7, "PR_7", "x", "main", "u", "t", "b", false)
	prA.RunID = runA
	prB := domain.NewPullRequestArtifact("octo/repo", 8, "PR_8", "y", "main", "u", "t", "b", false)
	prB.RunID = runB
	seedArt(prA)
	seedArt(prB)

	// Both PRs merged on GitHub, but we reconcile only run A's set.
	stub := &stubGH{prs: map[string]string{
		"PR_7": prNodeJSON("PR_7", 7, "MERGED", false, true),
		"PR_8": prNodeJSON("PR_8", 8, "MERGED", false, true),
	}}
	rc := NewReconciler(&fakeResolver{client: newStubClient(t, stub)}, stores.Artifacts, stores.TaskMemory, nil)

	runAArts, err := stores.Artifacts.ListByRun(ctx, runmode.LocalDefaultOrgID, runA)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	updated, err := rc.Reconcile(ctx, runmode.LocalDefaultOrgID, runAArts)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(updated) != 1 {
		t.Fatalf("expected 1 transition (run A only), got %d", len(updated))
	}
	assertState(t, stores, runmode.LocalDefaultOrgID, prA.DedupKey, domain.ArtifactStatePRMerged)
	// Run B untouched — only PR_7 was fetched.
	assertState(t, stores, runmode.LocalDefaultOrgID, prB.DedupKey, domain.ArtifactStatePROpen)
	if stub.prCalls["PR_8"] {
		t.Error("run B's PR_8 was queried during a run-A-scoped reconcile")
	}
}

// TestReconcile_OutcomeSupersedesVerdict pins the overwrite semantics: a run
// approved through TF carries a human verdict in human_content; when its PR
// later merges on GitHub, the reconciler's post-run outcome OVERWRITES that
// verdict (the final state is the authoritative divergence account), while the
// agent's own narrative is untouched.
func TestReconcile_OutcomeSupersedesVerdict(t *testing.T) {
	stores, seedRun, seedArt := reconcileTestStores(t)
	ctx := context.Background()
	const runID = "77777777-7777-7777-7777-777777777777"
	seedRun("ent-7", runID, "agent narrative")
	// Approval-time verdict already recorded.
	if err := stores.TaskMemory.UpdateRunMemoryHumanContent(ctx, runmode.LocalDefaultOrgID, runID, "Human approved with a tweaked body."); err != nil {
		t.Fatalf("seed verdict: %v", err)
	}

	prArt := domain.NewPullRequestArtifact("octo/repo", 10, "PR_10", "x", "main", "u", "draft title", "draft body", false)
	prArt.RunID = runID
	seedArt(prArt)

	stub := &stubGH{
		prs:     map[string]string{"PR_10": prNodeJSON("PR_10", 10, "MERGED", false, true)},
		prFinal: map[int][2]string{10: {"shipped title", "shipped body"}},
	}
	rc := NewReconciler(&fakeResolver{client: newStubClient(t, stub)}, stores.Artifacts, stores.TaskMemory, nil)
	if err := rc.ReconcileOrg(ctx, runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("ReconcileOrg: %v", err)
	}

	mem, err := stores.TaskMemory.GetRunMemory(ctx, runmode.LocalDefaultOrgID, runID)
	if err != nil || mem == nil {
		t.Fatalf("GetRunMemory: mem=%v err=%v", mem, err)
	}
	if strings.Contains(mem.Content, "Human approved with a tweaked body.") {
		t.Errorf("the approval verdict should have been superseded by the post-run outcome; got:\n%s", mem.Content)
	}
	if !strings.HasPrefix(mem.Content, "agent narrative") {
		t.Errorf("agent narrative was trampled: %q", mem.Content)
	}
	// The outcome carries the real shipped-vs-drafted delta.
	for _, want := range []string{"was merged on GitHub", "shipped title", "shipped body"} {
		if !strings.Contains(mem.Content, want) {
			t.Errorf("outcome missing %q; got:\n%s", want, mem.Content)
		}
	}
}

// --- helpers ---

// reconcileSet runs Tier-1 ReconcileOrg and returns its transitions (by
// re-deriving them: ReconcileOrg discards the slice). Thin wrapper used where a
// test wants the count via the org-wide path.
func reconcileSet(t *testing.T, stores db.Stores, res clientResolver, orgID string) ([]domain.Artifact, error) {
	t.Helper()
	rc := NewReconciler(res, stores.Artifacts, stores.TaskMemory, nil)
	arts, err := stores.Artifacts.ListNonTerminalBySystem(context.Background(), orgID)
	if err != nil {
		return nil, err
	}
	return rc.Reconcile(context.Background(), orgID, arts)
}

func assertState(t *testing.T, stores db.Stores, orgID, dedupKey, want string) {
	t.Helper()
	// Read the row back via a fresh non-terminal/team listing is awkward; query
	// the team list and find by dedup key (ListByTeam returns all states).
	all, err := stores.Artifacts.ListByTeam(context.Background(), orgID, runmode.LocalDefaultTeamID, db.ArtifactListOpts{})
	if err != nil {
		t.Fatalf("ListByTeam: %v", err)
	}
	for _, a := range all {
		if a.DedupKey == dedupKey {
			if a.State != want {
				t.Errorf("artifact %s: state = %q, want %q", dedupKey, a.State, want)
			}
			return
		}
	}
	t.Errorf("artifact %s not found", dedupKey)
}
