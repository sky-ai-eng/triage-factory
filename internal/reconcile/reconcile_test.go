package reconcile

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// --- white-box unit tests for the pure mapping helpers ---

func TestPrState(t *testing.T) {
	cases := []struct {
		name    string
		snap    domain.PRSnapshot
		current string
		want    string
	}{
		{"merged flag", domain.PRSnapshot{Merged: true, State: "OPEN"}, domain.DeliverableStatePROpen, domain.DeliverableStatePRMerged},
		{"merged state", domain.PRSnapshot{State: "MERGED"}, domain.DeliverableStatePROpen, domain.DeliverableStatePRMerged},
		{"closed", domain.PRSnapshot{State: "CLOSED"}, domain.DeliverableStatePROpen, domain.DeliverableStatePRClosed},
		{"draft stays draft", domain.PRSnapshot{State: "OPEN", IsDraft: true}, domain.DeliverableStatePRDraft, domain.DeliverableStatePRDraft},
		{"draft marked ready", domain.PRSnapshot{State: "OPEN"}, domain.DeliverableStatePRDraft, domain.DeliverableStatePROpen},
		{"open", domain.PRSnapshot{State: "OPEN"}, domain.DeliverableStatePROpen, domain.DeliverableStatePROpen},
		// Draft is one-way: a resolved row converted back to draft on GitHub is
		// a human reworking it, not a new approval to wait on.
		{"open converted back to draft", domain.PRSnapshot{State: "OPEN", IsDraft: true}, domain.DeliverableStatePROpen, domain.DeliverableStatePROpen},
		// A draft merged or closed as a draft still ends where GitHub says.
		{"draft merged", domain.PRSnapshot{Merged: true, State: "MERGED", IsDraft: true}, domain.DeliverableStatePRDraft, domain.DeliverableStatePRMerged},
		{"draft closed", domain.PRSnapshot{State: "CLOSED", IsDraft: true}, domain.DeliverableStatePRDraft, domain.DeliverableStatePRClosed},
		// Merged takes precedence over closed even if both somehow read truthy.
		{"merged beats closed", domain.PRSnapshot{Merged: true, State: "CLOSED"}, domain.DeliverableStatePROpen, domain.DeliverableStatePRMerged},
	}
	for _, c := range cases {
		if got := prState(c.snap, c.current); got != c.want {
			t.Errorf("%s: prState = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestIsDraftResolved(t *testing.T) {
	pr := func(state string) domain.Deliverable {
		return domain.Deliverable{Kind: domain.DeliverableKindPullRequest, State: state}
	}
	cases := []struct {
		name string
		a    domain.Deliverable
		next string
		want bool
	}{
		{"draft marked ready", pr(domain.DeliverableStatePRDraft), domain.DeliverableStatePROpen, true},
		{"draft merged", pr(domain.DeliverableStatePRDraft), domain.DeliverableStatePRMerged, true},
		{"draft closed", pr(domain.DeliverableStatePRDraft), domain.DeliverableStatePRClosed, true},
		{"open merged", pr(domain.DeliverableStatePROpen), domain.DeliverableStatePRMerged, false},
		{"review submitted", domain.Deliverable{Kind: domain.DeliverableKindReview, State: domain.DeliverableStateReviewPending}, domain.DeliverableStateReviewSubmitted, false},
	}
	for _, c := range cases {
		if got := isDraftResolved(c.a, c.next); got != c.want {
			t.Errorf("%s: isDraftResolved = %v, want %v", c.name, got, c.want)
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
		{"submitted (approved)", "REV_sub", domain.DeliverableStateReviewSubmitted, true},
		{"dismissed", "REV_dis", domain.DeliverableStateReviewDismissed, true},
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
	if s, ok := reviewState(snap, "R"); !ok || s != domain.DeliverableStateReviewSubmitted {
		t.Errorf("CHANGES_REQUESTED → (%q,%v), want (submitted,true)", s, ok)
	}
}

// --- integration: Reconcile end-to-end against a stub GitHub + real SQLite ---

// stubGH is a canned GitHub endpoint for RefreshPRs (GraphQL nodes query). It
// records which PR node ids were queried so a test can assert terminal
// deliverables are never fetched.
type stubGH struct {
	prs     map[string]string // PR node id → JSON node body (RefreshPRs)
	reviews map[string]string // review node id → PullRequestReview JSON body (GetReview); absent → node null
	prCalls map[string]bool   // PR node ids actually requested
}

func newStubServer(t *testing.T, s *stubGH) *httptest.Server {
	t.Helper()
	s.prCalls = map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

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
	return srv
}

func newStubClient(t *testing.T, s *stubGH) *github.Client {
	t.Helper()
	return github.NewClient(newStubServer(t, s).URL, "test-token")
}

// cancelAfterFetchRT cancels a caller context the moment the GraphQL fetch (the
// only POST reconcile issues) has completed and its body is safely buffered in
// memory — so the fetch itself succeeds on the live ctx, but everything
// downstream observes a cancelled caller ctx. It's the deterministic way to
// prove reconcile's write-back detaches (context.WithoutCancel): a server-side
// cancel would race the client's own response read.
type cancelAfterFetchRT struct {
	t      *testing.T
	base   http.RoundTripper
	cancel context.CancelFunc
	once   sync.Once
}

func (rt *cancelAfterFetchRT) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := rt.base.RoundTrip(req)
	if err != nil || req.Method != http.MethodPost {
		return resp, err
	}
	// Fully buffer the fetch body BEFORE cancelling so the caller's read never
	// touches the about-to-be-cancelled connection. A read failure here must be
	// loud: proceeding with an empty body would surface as a confusing "state =
	// open" assertion downstream instead of the actual transport fault. RoundTrip
	// runs synchronously on the test goroutine (Reconcile is called inline), so
	// t.Fatalf is safe.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		rt.t.Fatalf("buffering fetch response body: %v", err)
	}
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	rt.once.Do(rt.cancel) // fetch done + buffered → safe to cancel the caller ctx
	return resp, nil
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

// reconcileTestDB returns a fresh in-memory database's connection and the
// stores over it, with seeders for a conversation and a deliverable. The
// connection serves fixtures that seed rows the stores do not write, and the
// read-backs no store method answers.
func reconcileTestDB(t *testing.T) (*sql.DB, db.Stores, func(entityID, conversationID string), func(a domain.Deliverable)) {
	t.Helper()
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = conn.Close() })
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := db.SetLocalGitHubURLForTest(conn, "https://github.com"); err != nil {
		t.Fatalf("set github url: %v", err)
	}
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	seedConversation := func(entityID, conversationID string) {
		sourceID := "octo/repo#" + entityID
		repoID := dbseed.GitHubEntityRepository(t, conn, runmode.LocalDefaultOrgID, "https://github.com", sourceID)
		if _, err := conn.Exec(`INSERT INTO entities (id, source, source_id, kind, scope, repository_id) VALUES (?, 'github', ?, 'pull_request', 'https://github.com', ?)`, entityID, sourceID, repoID); err != nil {
			t.Fatalf("seed entity: %v", err)
		}
		if _, err := conn.Exec(`INSERT INTO conversations (id, origin, status, completed_at) VALUES (?, 'interactive', 'open', CURRENT_TIMESTAMP)`, conversationID); err != nil {
			t.Fatalf("seed conversation: %v", err)
		}
	}
	seedArt := func(a domain.Deliverable) {
		a.OrgID = runmode.LocalDefaultOrgID
		a.TeamID = runmode.LocalDefaultTeamID
		if a.Scope == "" {
			a.Scope = reconcileTestHost
		}
		if _, err := stores.Deliverables.Upsert(ctx, runmode.LocalDefaultOrgID, a); err != nil {
			t.Fatalf("seed deliverable %s: %v", a.DedupKey, err)
		}
	}
	return conn, stores, seedConversation, seedArt
}

// The reconciler reaches GitHub under the name a registry row holds now,
// whatever the deliverable's target or its entity's key still spells: a pull
// request through its entity's row (the owner the client is resolved for). A
// retired row's name is another repository's, so its deliverables are not
// refreshed.
func TestReconcile_AddressesRepositoriesByTheirRegistryRows(t *testing.T) {
	conn, stores, seedConversation, seedArt := reconcileTestDB(t)
	ctx := context.Background()
	const conversationID = "66666666-6666-6666-6666-666666666666"
	seedConversation("ent-6", conversationID)
	orgID := runmode.LocalDefaultOrgID

	// seed records a pull request with its entity in the repository
	// registered as repositoryID.
	seed := func(entityID, repositoryID, repoPath string, number int) domain.Deliverable {
		t.Helper()
		nodeID, sourceID := fmt.Sprintf("PR_%d", number), domain.PullRequestTarget(repoPath, number)
		if _, err := conn.Exec(`INSERT INTO entities (id, source, source_id, kind, scope, repository_id, external_id) VALUES (?, 'github', ?, 'pull_request', ?, ?, ?)`,
			entityID, sourceID, reconcileTestHost, repositoryID, nodeID); err != nil {
			t.Fatalf("seed entity %s: %v", sourceID, err)
		}
		pr := domain.NewPullRequestDeliverable(repoPath, number, nodeID, "feat", "main", "u", "t", "b", false)
		pr.ConversationID, pr.EntityID = conversationID, entityID
		seedArt(pr)
		return pr
	}
	moved := dbseed.GitHubRepository(t, conn, orgID, reconcileTestHost, "octo", "old")
	retired := dbseed.GitHubRepository(t, conn, orgID, reconcileTestHost, "octo", "gone")
	movedPR := seed("ent-moved", moved, "octo/old", 7)
	retiredPR := seed("ent-retired", retired, "octo/gone", 8)

	// GitHub serves the retired row's name as another repository.
	dbseed.RenameRepositoryOnly(t, conn, moved, "acme", "new")
	if _, err := conn.Exec(`UPDATE repositories SET retired_at = CURRENT_TIMESTAMP WHERE id = ?`, retired); err != nil {
		t.Fatalf("retire repository: %v", err)
	}

	stub := &stubGH{
		prs: map[string]string{
			"PR_7": prNodeJSON("PR_7", 7, "MERGED", false, true),
			"PR_8": prNodeJSON("PR_8", 8, "MERGED", false, true),
		},
	}
	res := &fakeResolver{client: newStubClient(t, stub)}
	rc := NewReconciler(res, stores, TrustRecords, nil)
	if err := rc.ReconcileOrg(ctx, orgID); err != nil {
		t.Fatalf("ReconcileOrg: %v", err)
	}

	assertState(t, conn, orgID, movedPR.DedupKey, domain.DeliverableStatePRMerged)
	if !res.owners["acme"] {
		t.Errorf("client owners = %v, want acme: the moved pull request's row is owned there now", res.owners)
	}

	assertState(t, conn, orgID, retiredPR.DedupKey, domain.DeliverableStatePROpen)
	if stub.prCalls["PR_8"] {
		t.Errorf("a retired row's deliverables reached GitHub: prs %v", stub.prCalls)
	}
	if res.owners["octo"] {
		t.Errorf("client owners = %v: nothing on this pass is owned by octo any more", res.owners)
	}
}

// TestReconcile_Transitions is the headline end-to-end: a draft PR that merged
// reflects its new state, and a terminal deliverable already in the set is
// never re-queried. A review-draft deliverable is seeded too and must stay
// pending — reviews are staged TF-side and never reconciled.
func TestReconcile_Transitions(t *testing.T) {
	conn, stores, seedConversation, seedArt := reconcileTestDB(t)
	ctx := context.Background()
	const conversationID = "11111111-1111-1111-1111-111111111111"
	seedConversation("ent-1", conversationID)

	prArt := domain.NewPullRequestDeliverable("octo/repo", 1, "PR_1", "feat", "main", "https://github.com/octo/repo/pull/1", "t", "b", true)
	prArt.ConversationID = conversationID
	reviewArt := domain.NewReviewDeliverable("octo/repo", 2, "PR_2", "headsha2", conversationID)
	reviewArt.ConversationID = conversationID
	// A terminal PR already merged — it must NOT be in the non-terminal set and
	// must never be fetched.
	mergedArt := domain.NewPullRequestDeliverable("octo/repo", 9, "PR_9", "old", "main", "https://github.com/octo/repo/pull/9", "t", "b", false)
	mergedArt.State = domain.DeliverableStatePRMerged
	mergedArt.ConversationID = conversationID
	seedArt(prArt)
	seedArt(reviewArt)
	seedArt(mergedArt)

	stub := &stubGH{
		prs: map[string]string{
			"PR_1": prNodeJSON("PR_1", 1, "MERGED", false, true),
		},
	}
	res := &fakeResolver{client: newStubClient(t, stub)}
	rc := NewReconciler(res, stores, TrustRecords, nil)

	if err := rc.ReconcileOrg(ctx, runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("ReconcileOrg: %v", err)
	}

	// States transitioned.
	assertState(t, conn, runmode.LocalDefaultOrgID, prArt.DedupKey, domain.DeliverableStatePRMerged)
	// The review draft is staged TF-side and never reconciled — it stays pending.
	assertState(t, conn, runmode.LocalDefaultOrgID, reviewArt.DedupKey, domain.DeliverableStateReviewPending)

	// Terminal deliverable never re-queried.
	if stub.prCalls["PR_9"] {
		t.Error("terminal merged PR (PR_9) was re-queried; it should be excluded from the non-terminal set")
	}
}

// TestReconcile_PRClosed pins the close-without-merging terminal branch. A
// review-draft deliverable is seeded alongside and must stay pending — reviews are
// staged TF-side and never reconciled (TFAC-494).
func TestReconcile_PRClosed(t *testing.T) {
	conn, stores, seedConversation, seedArt := reconcileTestDB(t)
	ctx := context.Background()
	const conversationID = "22222222-2222-2222-2222-222222222222"
	seedConversation("ent-2", conversationID)

	prArt := domain.NewPullRequestDeliverable("octo/repo", 3, "PR_3", "x", "main", "u", "t", "b", false)
	prArt.ConversationID = conversationID
	reviewArt := domain.NewReviewDeliverable("octo/repo", 4, "PR_4", "headsha4", conversationID)
	reviewArt.ConversationID = conversationID
	seedArt(prArt)
	seedArt(reviewArt)

	stub := &stubGH{prs: map[string]string{
		"PR_3": prNodeJSON("PR_3", 3, "CLOSED", false, false),
	}}
	rc := NewReconciler(&fakeResolver{client: newStubClient(t, stub)}, stores, TrustRecords, nil)
	if err := rc.ReconcileOrg(ctx, runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("ReconcileOrg: %v", err)
	}
	assertState(t, conn, runmode.LocalDefaultOrgID, prArt.DedupKey, domain.DeliverableStatePRClosed)
	// Nobody resolved it through TF, and the row says so — the agent-facing note
	// reads it to report an out-of-band close rather than a human dismissal.
	if d, _ := domain.ParsePRDeliverableDetails(findDeliverable(t, conn, runmode.LocalDefaultOrgID, prArt.DedupKey).DetailsJSON); d.Resolution != domain.PRResolutionGitHub {
		t.Errorf("resolution = %q, want github", d.Resolution)
	}
	// The review draft is never reconciled — it stays pending.
	assertState(t, conn, runmode.LocalDefaultOrgID, reviewArt.DedupKey, domain.DeliverableStateReviewPending)
}

// TestReconcile_NoOpWhenUnchanged pins that an open PR still open and a
// pending review still private both stay put.
func TestReconcile_NoOpWhenUnchanged(t *testing.T) {
	conn, stores, seedConversation, seedArt := reconcileTestDB(t)
	const conversationID = "33333333-3333-3333-3333-333333333333"
	seedConversation("ent-3", conversationID)

	prArt := domain.NewPullRequestDeliverable("octo/repo", 5, "PR_5", "x", "main", "u", "t", "b", false) // open
	prArt.ConversationID = conversationID
	reviewArt := domain.NewReviewDeliverable("octo/repo", 6, "PR_6", "headsha6", conversationID) // pending draft
	reviewArt.ConversationID = conversationID
	seedArt(prArt)
	seedArt(reviewArt)

	stub := &stubGH{
		prs: map[string]string{
			"PR_5": prNodeJSON("PR_5", 5, "OPEN", false, false),
		},
	}
	updated, err := reconcileSet(t, stores, &fakeResolver{client: newStubClient(t, stub)}, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(updated) != 0 {
		t.Errorf("expected no transitions, got %d: %+v", len(updated), updated)
	}
	assertState(t, conn, runmode.LocalDefaultOrgID, prArt.DedupKey, domain.DeliverableStatePROpen)
	assertState(t, conn, runmode.LocalDefaultOrgID, reviewArt.DedupKey, domain.DeliverableStateReviewPending)
}

// TestReconcile_DraftMarkedReadyRunsTheResolvedHook pins resolution from any
// door: a draft PR that GitHub now reports as ready — marked so by a human on
// GitHub, or by an agent whose mission asked — flips to open, and the hook
// that closes its task runs once for the owning conversation, after the write
// has landed.
func TestReconcile_DraftMarkedReadyRunsTheResolvedHook(t *testing.T) {
	conn, stores, seedConversation, seedArt := reconcileTestDB(t)
	const conversationID = "77777777-7777-7777-7777-777777777777"
	seedConversation("ent-7", conversationID)

	draft := domain.NewPullRequestDeliverable("octo/repo", 7, "PR_7", "x", "main", "u", "t", "b", true)
	draft.ConversationID = conversationID
	seedArt(draft)

	stub := &stubGH{prs: map[string]string{"PR_7": prNodeJSON("PR_7", 7, "OPEN", false, false)}}
	rc := NewReconciler(&fakeResolver{client: newStubClient(t, stub)}, stores, TrustRecords, nil)
	var (
		mu       sync.Mutex
		resolved []string
	)
	rc.SetPullRequestResolvedHook(func(_ context.Context, orgID, convID string) {
		mu.Lock()
		defer mu.Unlock()
		// The write is visible to the hook: the closure's unresolved check
		// must read the flipped row, not race it.
		assertState(t, conn, orgID, draft.DedupKey, domain.DeliverableStatePROpen)
		resolved = append(resolved, convID)
	})

	if err := rc.ReconcileOrg(context.Background(), runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("ReconcileOrg: %v", err)
	}
	assertState(t, conn, runmode.LocalDefaultOrgID, draft.DedupKey, domain.DeliverableStatePROpen)
	mu.Lock()
	defer mu.Unlock()
	if len(resolved) != 1 || resolved[0] != conversationID {
		t.Errorf("resolved hook calls = %v, want exactly [%s]", resolved, conversationID)
	}
}

// TestReconcile_OpenNeverReturnsToDraft pins the one-way rule: a PR that
// already left draft — the approval click, or a ready observed above — is not
// re-drafted when a human converts it back on GitHub. The row stays open, no
// transition is reported, and the hook does not run.
func TestReconcile_OpenNeverReturnsToDraft(t *testing.T) {
	conn, stores, seedConversation, seedArt := reconcileTestDB(t)
	const conversationID = "88888888-8888-8888-8888-888888888888"
	seedConversation("ent-8", conversationID)

	open := domain.NewPullRequestDeliverable("octo/repo", 8, "PR_8", "x", "main", "u", "t", "b", false)
	open.ConversationID = conversationID
	seedArt(open)

	stub := &stubGH{prs: map[string]string{"PR_8": prNodeJSON("PR_8", 8, "OPEN", true, false)}}
	rc := NewReconciler(&fakeResolver{client: newStubClient(t, stub)}, stores, TrustRecords, nil)
	hookRan := false
	rc.SetPullRequestResolvedHook(func(context.Context, string, string) { hookRan = true })

	arts, err := stores.Deliverables.ListNonTerminalBySystem(context.Background(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("ListNonTerminalBySystem: %v", err)
	}
	updated, err := rc.Reconcile(context.Background(), runmode.LocalDefaultOrgID, arts)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(updated) != 0 {
		t.Errorf("expected no transition for an open PR converted back to draft, got %+v", updated)
	}
	assertState(t, conn, runmode.LocalDefaultOrgID, open.DedupKey, domain.DeliverableStatePROpen)
	if hookRan {
		t.Error("resolved hook ran for a PR that was already resolved")
	}
}

// TestReconcile_SingleConversationScope pins the Tier-2 shape: Reconcile over
// just one conversation's deliverables transitions exactly those (the
// conversation-scoped refresh path).
func TestReconcile_SingleConversationScope(t *testing.T) {
	conn, stores, seedConversation, seedArt := reconcileTestDB(t)
	ctx := context.Background()
	const convA = "55555555-5555-5555-5555-555555555555"
	const convB = "66666666-6666-6666-6666-666666666666"
	seedConversation("ent-a", convA)
	seedConversation("ent-b", convB)

	prA := domain.NewPullRequestDeliverable("octo/repo", 7, "PR_7", "x", "main", "u", "t", "b", false)
	prA.ConversationID = convA
	prB := domain.NewPullRequestDeliverable("octo/repo", 8, "PR_8", "y", "main", "u", "t", "b", false)
	prB.ConversationID = convB
	seedArt(prA)
	seedArt(prB)

	// Both PRs merged on GitHub, but we reconcile only conversation A's set.
	stub := &stubGH{prs: map[string]string{
		"PR_7": prNodeJSON("PR_7", 7, "MERGED", false, true),
		"PR_8": prNodeJSON("PR_8", 8, "MERGED", false, true),
	}}
	rc := NewReconciler(&fakeResolver{client: newStubClient(t, stub)}, stores, TrustRecords, nil)

	convAArts, _, err := stores.Deliverables.ListByConversation(ctx, runmode.LocalDefaultOrgID, convA, db.Unwindowed)
	if err != nil {
		t.Fatalf("ListByConversation: %v", err)
	}
	updated, err := rc.Reconcile(ctx, runmode.LocalDefaultOrgID, convAArts)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(updated) != 1 {
		t.Fatalf("expected 1 transition (conversation A only), got %d", len(updated))
	}
	assertState(t, conn, runmode.LocalDefaultOrgID, prA.DedupKey, domain.DeliverableStatePRMerged)
	// Conversation B untouched — only PR_7 was fetched.
	assertState(t, conn, runmode.LocalDefaultOrgID, prB.DedupKey, domain.DeliverableStatePROpen)
	if stub.prCalls["PR_8"] {
		t.Error("conversation B's PR_8 was queried during a conversation-A-scoped reconcile")
	}
}

// TestRunner_SuppressesCancelErrorOnShutdown pins that a cycle in flight when
// Stop() cancels the context does NOT log an error: the cancel propagates out
// through a DB call as context.Canceled, and the runner's ctx.Err() guard treats
// it as a graceful shutdown, not a failure.
func TestRunner_SuppressesCancelErrorOnShutdown(t *testing.T) {
	logs := &recLogger{}
	prev := reconcileLog
	reconcileLog = slog.New(logs)
	t.Cleanup(func() { reconcileLog = prev })

	_, stores, _, _ := reconcileTestDB(t)
	rc := NewReconciler(&fakeResolver{client: newStubClient(t, &stubGH{})}, stores, TrustRecords, nil)
	r := NewRunner(rc, runmode.LocalDefaultOrgID)

	// An already-cancelled ctx makes the cycle's first DB call (ListNonTerminalBySystem)
	// return context.Canceled — the in-flight-at-shutdown shape.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.run(ctx)

	for _, rec := range logs.records() {
		if rec.Level >= slog.LevelError {
			t.Errorf("graceful shutdown logged at %s: %q", rec.Level, rec.Message)
		}
	}
}

// recLogger is a minimal slog.Handler capturing records so a test can assert on
// log levels.
type recLogger struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *recLogger) Enabled(context.Context, slog.Level) bool { return true }
func (h *recLogger) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, r.Clone())
	return nil
}
func (h *recLogger) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recLogger) WithGroup(string) slog.Handler      { return h }
func (h *recLogger) records() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]slog.Record(nil), h.recs...)
}

// TestReconcile_WriteBackSurvivesCallerCancel pins the durability fix: if the
// caller's ctx is cancelled (Tier-2 client disconnect / Tier-1 shutdown) once
// the apply phase begins, the state transition still lands. A terminal deliverable
// drops out of both tiers' working sets, so a write skipped here would never be
// re-attempted — the write-back must detach.
func TestReconcile_WriteBackSurvivesCallerCancel(t *testing.T) {
	conn, stores, seedConversation, seedArt := reconcileTestDB(t)
	const conversationID = "99999999-9999-9999-9999-999999999991"
	seedConversation("ent-9", conversationID)
	prArt := domain.NewPullRequestDeliverable("octo/repo", 12, "PR_12", "x", "main", "u", "t", "b", false)
	prArt.ConversationID = conversationID
	seedArt(prArt)

	stub := &stubGH{prs: map[string]string{"PR_12": prNodeJSON("PR_12", 12, "MERGED", false, true)}}
	// The fetch (GraphQL POST) now observes ctx (TFAC-475 made it cancellable),
	// so it must succeed BEFORE the cancel: cancelAfterFetchRT lets the fetch
	// complete on the live ctx, buffers its body, then cancels the caller ctx —
	// reconcile then enters the apply phase with ctx already cancelled, where the
	// write-back runs on context.WithoutCancel(ctx) and must still land. A
	// server-side cancel would race the client's own response read.
	srv := newStubServer(t, stub)
	ctx, cancel := context.WithCancel(context.Background())
	client := github.NewClientWithHTTPClient(srv.URL, "test-token",
		&http.Client{Transport: &cancelAfterFetchRT{t: t, base: http.DefaultTransport, cancel: cancel}})
	rc := NewReconciler(&fakeResolver{client: client}, stores, TrustRecords, nil)

	arts, err := stores.Deliverables.ListByConversationSystem(context.Background(), runmode.LocalDefaultOrgID, conversationID)
	if err != nil {
		t.Fatalf("ListByConversationSystem: %v", err)
	}
	if _, err := rc.Reconcile(ctx, runmode.LocalDefaultOrgID, arts); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("fetch RoundTripper never fired — the cancel-after-fetch guard didn't exercise the detached write-back")
	}

	assertState(t, conn, runmode.LocalDefaultOrgID, prArt.DedupKey, domain.DeliverableStatePRMerged)
}

// --- helpers ---

// reconcileSet runs Tier-1 ReconcileOrg and returns its transitions (by
// re-deriving them: ReconcileOrg discards the slice). Thin wrapper used where a
// test wants the count via the org-wide path.
func reconcileSet(t *testing.T, stores db.Stores, res clientResolver, orgID string) ([]domain.Deliverable, error) {
	t.Helper()
	rc := NewReconciler(res, stores, TrustRecords, nil)
	arts, err := stores.Deliverables.ListNonTerminalBySystem(context.Background(), orgID)
	if err != nil {
		return nil, err
	}
	return rc.Reconcile(context.Background(), orgID, arts)
}

// allDeliverables reads back every deliverable in orgID, in any state. No
// store method lists them all, so the ids come from the table and each row
// from the store's own point read.
func allDeliverables(t *testing.T, conn *sql.DB, orgID string) []domain.Deliverable {
	t.Helper()
	rows, err := conn.Query(`SELECT id FROM deliverables WHERE org_id = ? ORDER BY created_at DESC, id DESC`, orgID)
	if err != nil {
		t.Fatalf("list deliverable ids: %v", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan deliverable id: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list deliverable ids: %v", err)
	}
	_ = rows.Close()
	stores := sqlitestore.New(conn)
	out := make([]domain.Deliverable, 0, len(ids))
	for _, id := range ids {
		d, err := stores.Deliverables.Get(context.Background(), orgID, id)
		if err != nil || d == nil {
			t.Fatalf("read deliverable %s: %v, %v", id, d, err)
		}
		out = append(out, *d)
	}
	return out
}

// findDeliverable reads one deliverable back by dedup key, for asserting on fields
// beyond state.
func findDeliverable(t *testing.T, conn *sql.DB, orgID, dedupKey string) domain.Deliverable {
	t.Helper()
	for _, a := range allDeliverables(t, conn, orgID) {
		if a.DedupKey == dedupKey {
			return a
		}
	}
	t.Fatalf("deliverable %s not found", dedupKey)
	return domain.Deliverable{}
}

func assertState(t *testing.T, conn *sql.DB, orgID, dedupKey, want string) {
	t.Helper()
	for _, a := range allDeliverables(t, conn, orgID) {
		if a.DedupKey == dedupKey {
			if a.State != want {
				t.Errorf("deliverable %s: state = %q, want %q", dedupKey, a.State, want)
			}
			return
		}
	}
	t.Errorf("deliverable %s not found", dedupKey)
}

// --- PR-deliverable backstop ---

// backstopStub serves RefreshPRs' GraphQL nodes(ids:) from canned PR nodes
// keyed by node id, answering null for an id it does not hold, and records
// every request it receives: a node fetch as "nodes <ids>", anything else as
// its method and path, which it answers 404.
type backstopStub struct {
	mu    sync.Mutex
	nodes map[string]string
	calls []string
}

func newBackstopStub(t *testing.T, nodes map[string]string) (*backstopStub, *github.Client) {
	t.Helper()
	st := &backstopStub{nodes: nodes}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&req)
		}
		if !strings.Contains(req.Query, "nodes(ids:") {
			st.record(r.Method + " " + r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		ids, _ := req.Variables["ids"].([]any)
		names := make([]string, 0, len(ids))
		parts := make([]string, 0, len(ids))
		for _, idv := range ids {
			id, _ := idv.(string)
			names = append(names, id)
			if body, ok := st.nodes[id]; ok {
				parts = append(parts, body)
			} else {
				parts = append(parts, "null")
			}
		}
		st.record("nodes " + strings.Join(names, ","))
		fmt.Fprintf(w, `{"data":{"nodes":[%s]}}`, strings.Join(parts, ","))
	}))
	t.Cleanup(srv.Close)
	return st, github.NewClient(srv.URL, "test-token")
}

func (st *backstopStub) record(call string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.calls = append(st.calls, call)
}

func (st *backstopStub) requested() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return append([]string(nil), st.calls...)
}

// backstopPRNode is one open PR node as RefreshPRs' GraphQL fragment returns
// it, in repository repoPath under the node id PR_<number>.
func backstopPRNode(repoPath string, number int, head, title, body string, draft bool) string {
	b, _ := json.Marshal(map[string]any{
		"id": fmt.Sprintf("PR_%d", number), "number": number, "title": title, "body": body,
		"state": "OPEN", "isDraft": draft, "merged": false,
		"headRefName": head, "baseRefName": "main",
		"url":        fmt.Sprintf("https://github.com/%s/pull/%d", repoPath, number),
		"repository": map[string]any{"nameWithOwner": repoPath},
	})
	return string(b)
}

// recordPRCreated writes the pr_created ledger row the create verb writes for
// repoPath#number, opened by conversationID in toState, linked at url, under
// the PR's node id (empty for a row written without one).
func recordPRCreated(t *testing.T, stores db.Stores, scope, conversationID, repoPath string, number int, nodeID, toState, url string) {
	t.Helper()
	if err := stores.ExternalActions.RecordSystem(context.Background(), runmode.LocalDefaultOrgID, domain.ExternalAction{
		TeamID:         runmode.LocalDefaultTeamID,
		Provider:       domain.ProviderGitHub,
		Scope:          scope,
		Action:         domain.ActionPRCreated,
		Target:         domain.PullRequestTarget(repoPath, number),
		ExternalID:     strconv.Itoa(number),
		URL:            url,
		ToState:        toState,
		ConversationID: conversationID,
		Credential:     domain.CredentialGitHubApp,
		NodeID:         nodeID,
		PerformedBy:    domain.PerformedByAgent,
	}); err != nil {
		t.Fatalf("record pr_created for %s#%d: %v", repoPath, number, err)
	}
}

// renameRepo tracks repository from on the test host under a provider id and
// renames it to the slug to through the store's rename rewrite: the ledger rows
// linked under the old name get a rewritten link, while their targets keep the
// old name.
func renameRepo(t *testing.T, stores db.Stores, from, to string) {
	t.Helper()
	ctx := context.Background()
	old := domain.RepoRefFromSlug(reconcileTestHost, from)
	if err := dbtest.TrackRepos(ctx, stores.Repos, stores.TeamGitHubRepos, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, reconcileTestHost,
		[]domain.TeamGitHubRepo{{Owner: old.Owner, Repo: old.Repo}}); err != nil {
		t.Fatalf("track %s: %v", from, err)
	}
	old.ExternalID = "4242"
	if filled, err := stores.Repos.FillMissingExternalIDsSystem(ctx, runmode.LocalDefaultOrgID, []domain.RepoRef{old}); err != nil || filled != 1 {
		t.Fatalf("give %s its provider id: filled=%d err=%v", from, filled, err)
	}
	renamed := domain.RepoRefFromSlug(reconcileTestHost, to)
	renamed.ExternalID = old.ExternalID
	out, err := stores.Repos.RenameSystem(ctx, runmode.LocalDefaultOrgID, renamed)
	if err != nil || !out.Renamed {
		t.Fatalf("rename %s -> %s: outcome %+v, err %v", from, to, out, err)
	}
}

// prDeliverableFor returns the pull_request deliverable whose target is
// repoPath#number on the test host, in any state, or nil.
func prDeliverableFor(t *testing.T, conn *sql.DB, repoPath string, number int) *domain.Deliverable {
	t.Helper()
	arts := allDeliverables(t, conn, runmode.LocalDefaultOrgID)
	want := domain.PullRequestTarget(repoPath, number)
	for i := range arts {
		if arts[i].Scope == reconcileTestHost && arts[i].Kind == domain.DeliverableKindPullRequest && arts[i].Target == want {
			return &arts[i]
		}
	}
	return nil
}

// runBackstop runs one backstop pass for the test org through client, and
// returns the resolver it asked for that client.
func runBackstop(t *testing.T, stores db.Stores, client *github.Client) *fakeResolver {
	t.Helper()
	res := &fakeResolver{client: client}
	rc := NewReconciler(res, stores, TrustRecords, nil)
	if err := rc.BackfillPRDeliverables(context.Background(), runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("backstop: %v", err)
	}
	return res
}

// TestBackstop_RecordsPRFromItsLedgerRow is the gap the backstop closes: the
// create verb opened a draft PR and wrote its pr_created row, but the deliverable
// write beside it failed. The backstop records the deliverable, owned by the
// conversation the ledger row names — not the one that pushed the PR's branch
// first.
func TestBackstop_RecordsPRFromItsLedgerRow(t *testing.T) {
	conn, stores, seedConversation, _ := reconcileTestDB(t)
	seedConversation("octo/repo#1", "conv-pusher")
	seedConversation("octo/repo#2", "conv-opener")

	recordPRCreated(t, stores, reconcileTestHost, "conv-opener", "octo/repo", 5, "PR_5",
		domain.DeliverableStatePRDraft, "https://github.com/octo/repo/pull/5")

	stub, client := newBackstopStub(t, map[string]string{"PR_5": backstopPRNode("octo/repo", 5, "feature-x", "My PR", "What it does", true)})
	runBackstop(t, stores, client)

	got := prDeliverableFor(t, conn, "octo/repo", 5)
	if got == nil {
		t.Fatal("backstop did not record the PR deliverable")
	}
	if got.ConversationID != "conv-opener" || got.TeamID != runmode.LocalDefaultTeamID {
		t.Errorf("PR deliverable owner = (%q, %q), want (conv-opener, %s): the ledger row names who opened it",
			got.ConversationID, got.TeamID, runmode.LocalDefaultTeamID)
	}
	if got.State != domain.DeliverableStatePRDraft || got.ExternalID != "5" || got.URL != "https://github.com/octo/repo/pull/5" || got.NodeID != "PR_5" {
		t.Errorf("PR deliverable = state %q external_id %q url %q node_id %q, want draft / 5 / the PR's link / PR_5",
			got.State, got.ExternalID, got.URL, got.NodeID)
	}
	d, _ := domain.ParsePRDeliverableDetails(got.DetailsJSON)
	if d.NodeID != "PR_5" || d.HeadBranch != "feature-x" || d.Base != "main" || d.Proposed.Title != "My PR" || d.Proposed.Body != "What it does" {
		t.Errorf("PR deliverable details = %+v, want node PR_5, feature-x → main, and the PR's title and body as proposed", d)
	}
	if calls := stub.requested(); len(calls) != 1 || calls[0] != "nodes PR_5" {
		t.Errorf("GitHub calls = %v, want one node fetch of PR_5", calls)
	}
}

// TestBackstop_IgnoresPRWithoutLedgerRow pins that a PR TF never opened gets no
// deliverable. A person opened a draft from a branch a run pushed: there is no
// pr_created row for it, so nothing is recorded, nothing is fetched, and the
// conversation that pushed the branch owes no approval for it.
func TestBackstop_IgnoresPRWithoutLedgerRow(t *testing.T) {
	conn, stores, seedConversation, _ := reconcileTestDB(t)
	seedConversation("octo/repo#5", "conv-1")

	stub, client := newBackstopStub(t, map[string]string{"PR_5": backstopPRNode("octo/repo", 5, "feature-x", "A person's draft", "", true)})
	runBackstop(t, stores, client)

	if got := prDeliverableFor(t, conn, "octo/repo", 5); got != nil {
		t.Fatalf("backstop recorded a PR TF never opened: %+v", got)
	}
	if calls := stub.requested(); len(calls) != 0 {
		t.Errorf("backstop called GitHub with no pr_created row to act on: %v", calls)
	}
	arts, err := stores.Deliverables.ListByConversationSystem(context.Background(), runmode.LocalDefaultOrgID, "conv-1")
	if err != nil {
		t.Fatalf("list conversation deliverables: %v", err)
	}
	if domain.HasUnresolvedDeliverables(arts) {
		t.Error("the pushing conversation shows an unresolved deliverable for a PR TF never opened")
	}
}

// TestBackstop_Idempotent pins that a PR with a deliverable is never fetched or
// rewritten — whatever state a prior writer advanced it to — and that a PR the
// backstop recorded is left alone on the next pass.
func TestBackstop_Idempotent(t *testing.T) {
	conn, stores, seedConversation, seedArt := reconcileTestDB(t)
	seedConversation("octo/repo#5", "conv-1")

	// The verb recorded PR 5, and it later merged.
	merged := domain.NewPullRequestDeliverable("octo/repo", 5, "PR_5", "feature-x", "main",
		"https://github.com/octo/repo/pull/5", "My PR", "body", false)
	merged.ConversationID = "conv-1"
	merged.State = domain.DeliverableStatePRMerged
	seedArt(merged)
	recordPRCreated(t, stores, reconcileTestHost, "conv-1", "octo/repo", 5, "PR_5",
		domain.DeliverableStatePROpen, "https://github.com/octo/repo/pull/5")
	// PR 7's deliverable write failed.
	recordPRCreated(t, stores, reconcileTestHost, "conv-1", "octo/repo", 7, "PR_7",
		domain.DeliverableStatePRDraft, "https://github.com/octo/repo/pull/7")

	stub, client := newBackstopStub(t, map[string]string{
		"PR_5": backstopPRNode("octo/repo", 5, "feature-x", "My PR", "body", false),
		"PR_7": backstopPRNode("octo/repo", 7, "feature-y", "Another", "body", true),
	})
	for i := 0; i < 2; i++ {
		runBackstop(t, stores, client)
	}

	if got := prDeliverableFor(t, conn, "octo/repo", 5); got == nil || got.State != domain.DeliverableStatePRMerged {
		t.Errorf("recorded PR 5 = %+v, want it left merged", got)
	}
	if got := prDeliverableFor(t, conn, "octo/repo", 7); got == nil || got.State != domain.DeliverableStatePRDraft {
		t.Errorf("PR 7 = %+v, want it recorded as a draft", got)
	}
	if calls := stub.requested(); len(calls) != 1 || calls[0] != "nodes PR_7" {
		t.Errorf("GitHub calls = %v, want one fetch of PR 7 across both passes", calls)
	}
}

// TestBackstop_BackfillsUnderTheRenamedRepositorysName pins that a PR whose
// deliverable write failed, in a repository renamed since, is backfilled under the
// repository's current name and owned by the conversation the row names. The
// rename rewrote the row's link but not its target, which still names the old
// repository; the node id finds the PR wherever it lives now.
func TestBackstop_BackfillsUnderTheRenamedRepositorysName(t *testing.T) {
	conn, stores, seedConversation, _ := reconcileTestDB(t)
	seedConversation("octo/repo#5", "conv-1")
	recordPRCreated(t, stores, reconcileTestHost, "conv-1", "octo/repo", 5, "PR_5",
		domain.DeliverableStatePRDraft, "https://github.com/octo/repo/pull/5")
	renameRepo(t, stores, "octo/repo", "octo/platform")

	_, client := newBackstopStub(t, map[string]string{"PR_5": backstopPRNode("octo/platform", 5, "feature-x", "My PR", "body", true)})
	runBackstop(t, stores, client)

	got := prDeliverableFor(t, conn, "octo/platform", 5)
	if got == nil {
		t.Fatal("backstop did not record the PR under the repository's current name")
	}
	if got.ConversationID != "conv-1" || got.TeamID != runmode.LocalDefaultTeamID || got.NodeID != "PR_5" ||
		got.URL != "https://github.com/octo/platform/pull/5" || got.State != domain.DeliverableStatePRDraft {
		t.Errorf("PR deliverable = %+v, want conv-1's draft octo/platform#5 under node PR_5", got)
	}
	if old := prDeliverableFor(t, conn, "octo/repo", 5); old != nil {
		t.Errorf("backstop also recorded the PR under the repository's old name: %+v", old)
	}
}

// The backstop fetches a pull request under the owner of the entity its
// pr_created row is about, by the key that entity has now, not the owner the
// row's target recorded before the repository moved to another owner.
func TestBackstop_FetchesUnderTheOwnerTheEntityHasNow(t *testing.T) {
	conn, stores, seedConversation, _ := reconcileTestDB(t)
	seedConversation("octo/repo#5", "conv-1")
	ctx := context.Background()
	entity, _, err := stores.Entities.FindOrCreateSystem(ctx, runmode.LocalDefaultOrgID, "github", reconcileTestHost, "acme/repo#5", "PR_5", "pr", "Moved", "")
	if err != nil {
		t.Fatalf("seed the PR's entity: %v", err)
	}
	if err := stores.ExternalActions.RecordSystem(ctx, runmode.LocalDefaultOrgID, domain.ExternalAction{
		TeamID: runmode.LocalDefaultTeamID, Provider: domain.ProviderGitHub, Scope: reconcileTestHost,
		Action: domain.ActionPRCreated, Target: "octo/repo#5", EntityID: entity.ID, ExternalID: "5",
		ToState: domain.DeliverableStatePRDraft, ConversationID: "conv-1", Credential: domain.CredentialGitHubApp, NodeID: "PR_5",
		PerformedBy: domain.PerformedByAgent,
	}); err != nil {
		t.Fatalf("record pr_created: %v", err)
	}

	_, client := newBackstopStub(t, map[string]string{"PR_5": backstopPRNode("acme/repo", 5, "feature-x", "My PR", "body", true)})
	if resolver := runBackstop(t, stores, client); !resolver.owners["acme"] || resolver.owners["octo"] {
		t.Errorf("clients resolved for %v, want acme's alone", resolver.owners)
	}
	if got := prDeliverableFor(t, conn, "acme/repo", 5); got == nil || got.EntityID != entity.ID {
		t.Errorf("PR deliverable = %+v, want it recorded on the entity %s", got, entity.ID)
	}
}

// TestBackstop_RenamedRepositoryWithDeliverableIsLeftAlone pins that a PR whose
// deliverable was recorded under the repository's old name, and whose ledger row
// still names it, does not read as a missing deliverable after a rename: the
// deliverable is keyed on the PR's node id, nothing is recorded a second time,
// and GitHub is not called.
func TestBackstop_RenamedRepositoryWithDeliverableIsLeftAlone(t *testing.T) {
	conn, stores, seedConversation, seedArt := reconcileTestDB(t)
	seedConversation("octo/repo#5", "conv-1")
	recorded := domain.NewPullRequestDeliverable("octo/repo", 5, "PR_5", "feature-x", "main",
		"https://github.com/octo/repo/pull/5", "My PR", "body", true)
	recorded.ConversationID = "conv-1"
	seedArt(recorded)
	recordPRCreated(t, stores, reconcileTestHost, "conv-1", "octo/repo", 5, "PR_5",
		domain.DeliverableStatePRDraft, "https://github.com/octo/repo/pull/5")
	renameRepo(t, stores, "octo/repo", "octo/platform")

	stub, client := newBackstopStub(t, map[string]string{"PR_5": backstopPRNode("octo/platform", 5, "feature-x", "My PR", "body", true)})
	runBackstop(t, stores, client)

	if n := countPRDeliverables(t, conn); n != 1 {
		t.Errorf("PR deliverables = %d, want the one recorded before the rename", n)
	}
	if got := findDeliverable(t, conn, runmode.LocalDefaultOrgID, domain.PullRequestDedupKey("PR_5")); got.ConversationID != "conv-1" {
		t.Errorf("PR deliverable = %+v, want conv-1's row kept", got)
	}
	if calls := stub.requested(); len(calls) != 0 {
		t.Errorf("backstop called GitHub for a PR that has its deliverable: %v", calls)
	}
}

// TestBackstop_AndCreateWriterShareOneRowAcrossNames pins that the backstop,
// which records a PR under GitHub's current name, and the create verb, which
// records it under the registry's, land on one row: both key it on the PR's
// node id. Before a rename is applied the two spell the repository
// differently.
func TestBackstop_AndCreateWriterShareOneRowAcrossNames(t *testing.T) {
	conn, stores, seedConversation, seedArt := reconcileTestDB(t)
	seedConversation("octo/repo#5", "conv-1")
	recordPRCreated(t, stores, reconcileTestHost, "conv-1", "octo/repo", 5, "PR_5",
		domain.DeliverableStatePRDraft, "https://github.com/octo/repo/pull/5")

	_, client := newBackstopStub(t, map[string]string{"PR_5": backstopPRNode("octo/platform", 5, "feature-x", "My PR", "body", true)})
	runBackstop(t, stores, client)
	created := domain.NewPullRequestDeliverable("octo/repo", 5, "PR_5", "feature-x", "main",
		"https://github.com/octo/repo/pull/5", "My PR", "body", true)
	created.ConversationID = "conv-1"
	seedArt(created)

	if n := countPRDeliverables(t, conn); n != 1 {
		t.Errorf("PR deliverables = %d, want one row for the one pull request", n)
	}
}

// countPRDeliverables counts the org's pull_request deliverables on the test host.
func countPRDeliverables(t *testing.T, conn *sql.DB) int {
	t.Helper()
	n := 0
	for _, a := range allDeliverables(t, conn, runmode.LocalDefaultOrgID) {
		if a.Kind == domain.DeliverableKindPullRequest && a.Scope == reconcileTestHost {
			n++
		}
	}
	return n
}

// TestBackstop_IgnoresTheRowsLink pins that the row's link plays no part: a
// row whose link is not spelled the way the configured host is (GitHub reports
// the host with a port the org's setting leaves out, or under a name the org
// reaches it by another way) is backfilled like any other.
func TestBackstop_IgnoresTheRowsLink(t *testing.T) {
	conn, stores, seedConversation, _ := reconcileTestDB(t)
	seedConversation("octo/repo#5", "conv-1")
	recordPRCreated(t, stores, reconcileTestHost, "conv-1", "octo/repo", 5, "PR_5",
		domain.DeliverableStatePRDraft, "https://github.com:443/octo/repo/pull/5")

	_, client := newBackstopStub(t, map[string]string{"PR_5": backstopPRNode("octo/repo", 5, "feature-x", "My PR", "body", true)})
	runBackstop(t, stores, client)

	if got := prDeliverableFor(t, conn, "octo/repo", 5); got == nil || got.ConversationID != "conv-1" {
		t.Errorf("PR deliverable = %+v, want conv-1's PR recorded whatever its row's link says", got)
	}
}

// TestBackstop_IgnoresRowWithoutNodeID pins that a pr_created row with no node
// id is never acted on: no GitHub call and no deliverable.
func TestBackstop_IgnoresRowWithoutNodeID(t *testing.T) {
	conn, stores, seedConversation, _ := reconcileTestDB(t)
	seedConversation("octo/repo#5", "conv-1")
	recordPRCreated(t, stores, reconcileTestHost, "conv-1", "octo/repo", 5, "",
		domain.DeliverableStatePRDraft, "https://github.com/octo/repo/pull/5")

	stub, client := newBackstopStub(t, map[string]string{"PR_5": backstopPRNode("octo/repo", 5, "feature-x", "My PR", "body", true)})
	runBackstop(t, stores, client)

	if got := prDeliverableFor(t, conn, "octo/repo", 5); got != nil {
		t.Errorf("backstop recorded a PR from a row with no node id: %+v", got)
	}
	if calls := stub.requested(); len(calls) != 0 {
		t.Errorf("backstop called GitHub for a row with no node id: %v", calls)
	}
}

// TestReconcileOrg_BackfilledPRTakesGitHubState pins the cycle's order: a PR
// the backstop records in the state the verb opened it in is reconciled in the
// same cycle. A draft merged on GitHub ends merged, stamped as resolved there,
// and the resolved hook runs for the conversation that opened it.
func TestReconcileOrg_BackfilledPRTakesGitHubState(t *testing.T) {
	conn, stores, seedConversation, _ := reconcileTestDB(t)
	const conversationID = "77777777-7777-7777-7777-777777777777"
	seedConversation("ent-7", conversationID)
	recordPRCreated(t, stores, reconcileTestHost, conversationID, "octo/repo", 7, "PR_7",
		domain.DeliverableStatePRDraft, "https://github.com/octo/repo/pull/7")

	stub := &stubGH{prs: map[string]string{"PR_7": prNodeJSON("PR_7", 7, "MERGED", true, true)}}
	rc := NewReconciler(&fakeResolver{client: newStubClient(t, stub)}, stores, TrustRecords, nil)
	var resolved []string
	rc.SetPullRequestResolvedHook(func(_ context.Context, _, id string) { resolved = append(resolved, id) })

	if err := rc.ReconcileOrg(context.Background(), runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("ReconcileOrg: %v", err)
	}

	got := prDeliverableFor(t, conn, "octo/repo", 7)
	if got == nil || got.State != domain.DeliverableStatePRMerged {
		t.Fatalf("PR 7 = %+v, want it recorded and reconciled to merged in one cycle", got)
	}
	if d, _ := domain.ParsePRDeliverableDetails(got.DetailsJSON); d.Resolution != domain.PRResolutionGitHub {
		t.Errorf("resolution = %q, want github", d.Resolution)
	}
	if len(resolved) != 1 || resolved[0] != conversationID {
		t.Errorf("resolved hook ran for %v, want [%s]", resolved, conversationID)
	}
}

// reconcileTestHost is the GitHub host of the test org — its default, the
// host the resolver's clients reach — and the scope seeded deliverables take
// unless a test names another.
const reconcileTestHost = "https://github.com"

// TestReconcile_SkipsDeliverablesOnAnotherHost pins that only deliverables on the
// org's current GitHub host are reconciled. The resolver's client reaches that
// host alone, so another host's pull request would be refreshed from a
// stranger.
func TestReconcile_SkipsDeliverablesOnAnotherHost(t *testing.T) {
	conn, stores, seedConversation, seedArt := reconcileTestDB(t)
	ctx := context.Background()
	const conversationID = "55555555-5555-5555-5555-555555555555"
	seedConversation("ent-5", conversationID)

	elsewherePR := domain.NewPullRequestDeliverable("octo/repo", 21, "PR_21", "x", "main", "https://ghe.example.com/octo/repo/pull/21", "t", "b", false)
	elsewherePR.ConversationID = conversationID
	elsewherePR.Scope = "https://ghe.example.com"
	ownPR := domain.NewPullRequestDeliverable("octo/repo", 22, "PR_22", "y", "main", "https://github.com/octo/repo/pull/22", "t", "b", false)
	ownPR.ConversationID = conversationID
	ownPR.Scope = reconcileTestHost
	seedArt(elsewherePR)
	seedArt(ownPR)

	stub := &stubGH{
		prs: map[string]string{
			"PR_21": prNodeJSON("PR_21", 21, "MERGED", false, true),
			"PR_22": prNodeJSON("PR_22", 22, "MERGED", false, true),
		},
	}
	rc := NewReconciler(&fakeResolver{client: newStubClient(t, stub)}, stores, TrustRecords, nil)
	if err := rc.ReconcileOrg(ctx, runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("ReconcileOrg: %v", err)
	}

	assertScopedState(t, conn, elsewherePR, domain.DeliverableStatePROpen)
	assertScopedState(t, conn, ownPR, domain.DeliverableStatePRMerged)
	if stub.prCalls["PR_21"] {
		t.Error("a pull request on another host was refreshed through the current host's client")
	}
}

// TestBackstop_RecordsOnTheCurrentHostOnly pins the backstop's scope: it
// works from pr_created rows on the org's current GitHub host, where its
// client looks, and records what it finds there. A row from another host is
// another host's PR, and is neither fetched nor recorded.
func TestBackstop_RecordsOnTheCurrentHostOnly(t *testing.T) {
	conn, stores, seedConversation, _ := reconcileTestDB(t)
	seedConversation("octo/repo#5", "conv-1")
	seedConversation("octo/repo#6", "conv-2")
	recordPRCreated(t, stores, reconcileTestHost, "conv-1", "octo/repo", 5, "PR_5",
		domain.DeliverableStatePRDraft, "https://github.com/octo/repo/pull/5")
	recordPRCreated(t, stores, "https://ghe.example.com", "conv-2", "octo/repo", 6, "PR_6",
		domain.DeliverableStatePRDraft, "https://ghe.example.com/octo/repo/pull/6")

	stub, client := newBackstopStub(t, map[string]string{
		"PR_5": backstopPRNode("octo/repo", 5, "feature-x", "Mine", "", true),
		"PR_6": backstopPRNode("octo/repo", 6, "feature-y", "Same repo name, other host", "", true),
	})
	runBackstop(t, stores, client)

	got := prDeliverableFor(t, conn, "octo/repo", 5)
	if got == nil || got.Scope != reconcileTestHost || got.ConversationID != "conv-1" {
		t.Fatalf("recorded PR = %+v, want conv-1's PR on %s", got, reconcileTestHost)
	}
	if got := prDeliverableFor(t, conn, "octo/repo", 6); got != nil {
		t.Errorf("a PR opened on another host was recorded on %s: %+v", reconcileTestHost, got)
	}
	if calls := stub.requested(); len(calls) != 1 || calls[0] != "nodes PR_5" {
		t.Errorf("GitHub calls = %v, want PR_5 alone fetched: another host's PR is not the current host's client's to fetch", calls)
	}
}

// assertScopedState reads want's row back by its scope and dedup key and
// requires state.
func assertScopedState(t *testing.T, conn *sql.DB, want domain.Deliverable, state string) {
	t.Helper()
	for _, a := range allDeliverables(t, conn, runmode.LocalDefaultOrgID) {
		if a.Scope == want.Scope && a.DedupKey == want.DedupKey {
			if a.State != state {
				t.Errorf("%s on %s state = %q, want %q", a.DedupKey, a.Scope, a.State, state)
			}
			return
		}
	}
	t.Fatalf("no deliverable %s on %s", want.DedupKey, want.Scope)
}
