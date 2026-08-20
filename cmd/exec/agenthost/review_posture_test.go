package agenthost

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// reviewPostureGitHub is reviewDiffServer plus the review-submit endpoint, so a
// posture test can tell "posted" from "staged" by what GitHub was actually
// asked to do. Every submit body is captured.
type reviewPostureGitHub struct {
	*httptest.Server
	mu      sync.Mutex
	submits []map[string]any
}

func (g *reviewPostureGitHub) submitted() []map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]map[string]any(nil), g.submits...)
}

func newReviewPostureGitHub(t *testing.T, headSHA *string) *reviewPostureGitHub {
	t.Helper()
	const diff = "diff --git a/a.go b/a.go\n" +
		"--- a/a.go\n+++ b/a.go\n" +
		"@@ -1,3 +1,5 @@\n line1\n+added2\n+added3\n line4\n line5\n"
	g := &reviewPostureGitHub{}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reviews") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			g.mu.Lock()
			g.submits = append(g.submits, body)
			g.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":555}`)
			return
		}
		if strings.Contains(r.Header.Get("Accept"), "diff") {
			_, _ = io.WriteString(w, diff)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"number":7,"base":{"ref":"main"},"head":{"sha":"`+*headSHA+`"}}`)
	}))
	t.Cleanup(g.Close)
	return g
}

// setTeamReviewPosture writes the team's posture through the real store, so the
// test exercises the same read the runtime performs at finalize time.
func setTeamReviewPosture(t *testing.T, stores db.Stores, teamID, posture string) {
	t.Helper()
	set, err := stores.Teams.GetSettingsSystem(context.Background(), teamID)
	if err != nil {
		t.Fatalf("GetSettingsSystem: %v", err)
	}
	set.ReviewPosture = posture
	if _, err := stores.Teams.UpdateSettings(context.Background(), teamID, set); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
}

// TestFinalizeReviewDraft_PostureDecidesPosting is the per-posture table: for
// each (posture, credential identity, review), does finalize hand the draft to
// the human approval queue or put it on GitHub?
//
// The outcome is asserted on three surfaces that must agree — the returned
// ReviewFinalizeResult (what the CLI reports), whether GitHub received a submit,
// and the artifact's state (a posted review must not also sit in the approval
// queue for a human to act on).
func TestFinalizeReviewDraft_PostureDecidesPosting(t *testing.T) {
	cases := []struct {
		name       string
		posture    string
		identity   ghclient.Identity
		event      string
		severity   string
		wantPosted bool
	}{
		{
			name:       "draft stages, byte-identically to the pre-posture behavior",
			posture:    domain.ReviewPostureDraft,
			identity:   ghclient.IdentityApp,
			event:      "COMMENT",
			wantPosted: false,
		},
		{
			name:       "auto posts",
			posture:    domain.ReviewPostureAuto,
			identity:   ghclient.IdentityPAT,
			event:      "COMMENT",
			wantPosted: true,
		},
		{
			name:       "identity posts under an App installation",
			posture:    domain.ReviewPostureIdentity,
			identity:   ghclient.IdentityApp,
			event:      "COMMENT",
			wantPosted: true,
		},
		{
			name:       "identity stages under a borrowed PAT",
			posture:    domain.ReviewPostureIdentity,
			identity:   ghclient.IdentityPAT,
			event:      "COMMENT",
			wantPosted: false,
		},
		{
			name:       "identity stages when the credential can't be classified",
			posture:    domain.ReviewPostureIdentity,
			identity:   ghclient.IdentityUnknown,
			event:      "COMMENT",
			wantPosted: false,
		},
		{
			name:       "auto_unless_blocking posts an ordinary comment review",
			posture:    domain.ReviewPostureAutoUnlessBlocking,
			identity:   ghclient.IdentityPAT,
			event:      "COMMENT",
			wantPosted: true,
		},
		{
			name:       "auto_unless_blocking stages a request-changes review",
			posture:    domain.ReviewPostureAutoUnlessBlocking,
			identity:   ghclient.IdentityApp,
			event:      "REQUEST_CHANGES",
			wantPosted: false,
		},
		{
			name:       "auto_unless_blocking stages a review carrying a blocker",
			posture:    domain.ReviewPostureAutoUnlessBlocking,
			identity:   ghclient.IdentityApp,
			event:      "COMMENT",
			severity:   domain.SeverityBlocker,
			wantPosted: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			head := "commit_v1"
			gh := newReviewPostureGitHub(t, &head)
			stores, info, client := newGithubRecordingClient(t, gh.URL, true)
			client.SetGitHubResolver(fakeGitHubResolver{baseURL: gh.URL, token: "tok", identity: tc.identity})
			setTeamReviewPosture(t, stores, info.TeamID, tc.posture)

			ctx := context.Background()
			handle, err := client.GithubCreatePendingReview(ctx, "octo", "repo", 7, "headsha7", nil)
			if err != nil {
				t.Fatalf("GithubCreatePendingReview: %v", err)
			}
			body := "nit: rename"
			if tc.severity != "" {
				body = domain.SeverityBadgeMarkdown(tc.severity) + body
			}
			if _, err := client.GithubAddPendingReviewComment(ctx, "octo", "repo", handle, "a.go", body, 3, nil, "worktree_head"); err != nil {
				t.Fatalf("GithubAddPendingReviewComment: %v", err)
			}

			res, err := client.FinalizeReviewDraft(ctx, handle, tc.event, "## Review body")
			if err != nil {
				t.Fatalf("FinalizeReviewDraft: %v", err)
			}

			if res.Posted != tc.wantPosted {
				t.Errorf("Posted = %v, want %v", res.Posted, tc.wantPosted)
			}
			submits := gh.submitted()
			if tc.wantPosted && len(submits) != 1 {
				t.Fatalf("GitHub saw %d review submits, want exactly 1", len(submits))
			}
			if !tc.wantPosted && len(submits) != 0 {
				t.Fatalf("a staged review must reach GitHub zero times, saw %d", len(submits))
			}

			arts := listConversationArtifacts(t, stores, info.ConversationID)
			if len(arts) != 1 {
				t.Fatalf("want 1 artifact, got %d", len(arts))
			}
			a := arts[0]
			d, _ := domain.ParseReviewArtifactDetails(a.DetailsJSON)
			// The ready sentinel is armed under BOTH outcomes — it's the
			// anti-double-submit guard, not an "awaiting approval" marker.
			if d.ReviewEvent != tc.event {
				t.Errorf("ready sentinel = %q, want %q", d.ReviewEvent, tc.event)
			}
			if len(d.Proposed.Comments) != 1 {
				t.Errorf("the agent's proposed draft must be snapshotted either way: %+v", d.Proposed)
			}
			if tc.wantPosted {
				// The posted payload matches what the human-approval path posts
				// (internal/server TestReviewArtifactApprove pins the same
				// shape): both publish through review.SubmitStaged, so the
				// commit pin, the comments, and the footer are one contract.
				got := submits[0]
				if got["event"] != tc.event {
					t.Errorf("submit event = %v, want %q", got["event"], tc.event)
				}
				if got["commit_id"] != "commit_v1" {
					t.Errorf("submit commit_id = %v, want the head the comments reconciled to", got["commit_id"])
				}
				if b, _ := got["body"].(string); !strings.HasPrefix(b, "## Review body") {
					t.Errorf("submit body = %q, want the staged body (plus the run footer)", b)
				}
				if c, _ := got["comments"].([]any); len(c) != 1 {
					t.Errorf("submit comments = %v, want the one staged comment", got["comments"])
				}
				if a.State != domain.ArtifactStateReviewSubmitted {
					t.Errorf("state = %q, want submitted (no pending row for a human to act on)", a.State)
				}
				if a.ExternalID != "555" {
					t.Errorf("ExternalID = %q, want the posted review's id 555", a.ExternalID)
				}
				if res.URL == "" || a.URL != res.URL {
					t.Errorf("URL = %q / result %q, want the posted review's deep link on both", a.URL, res.URL)
				}
				if !strings.Contains(a.URL, "#pullrequestreview-555") {
					t.Errorf("URL = %q, want the #pullrequestreview-555 anchor", a.URL)
				}
			} else {
				if a.State != domain.ArtifactStateReviewPending {
					t.Errorf("state = %q, want pending (awaiting human approval)", a.State)
				}
				if a.ExternalID != "" || a.URL != "" || res.URL != "" {
					t.Errorf("a staged review has nothing on GitHub to point at: id=%q url=%q result=%q", a.ExternalID, a.URL, res.URL)
				}
			}

			// No posture lets a review be submitted twice: the second finalize is
			// refused before it can reach GitHub.
			if _, err := client.FinalizeReviewDraft(ctx, handle, tc.event, "## again"); err == nil {
				t.Error("second finalize-review must be refused under every posture")
			}
			if got := len(gh.submitted()); got != len(submits) {
				t.Errorf("the refused second finalize submitted again: %d → %d", len(submits), got)
			}
		})
	}
}

// TestFinalizeReviewDraft_AutoPost_SubmitFailureStaysStaged pins the failure
// mode of the posting posture: when GitHub refuses the submit, the finalized
// draft is neither lost nor half-applied — it stays pending with its sentinel
// armed, which is exactly the state a human approves from, and the run reports
// the staged outcome rather than claiming a review that isn't there.
func TestFinalizeReviewDraft_AutoPost_SubmitFailureStaysStaged(t *testing.T) {
	head := "commit_v1"
	const diff = "diff --git a/a.go b/a.go\n" +
		"--- a/a.go\n+++ b/a.go\n" +
		"@@ -1,3 +1,5 @@\n line1\n+added2\n+added3\n line4\n line5\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reviews") {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"message":"upstream is down"}`)
			return
		}
		if strings.Contains(r.Header.Get("Accept"), "diff") {
			_, _ = io.WriteString(w, diff)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"number":7,"base":{"ref":"main"},"head":{"sha":"`+head+`"}}`)
	}))
	t.Cleanup(srv.Close)

	stores, info, client := newGithubRecordingClient(t, srv.URL, true)
	client.SetGitHubResolver(fakeGitHubResolver{baseURL: srv.URL, token: "tok", identity: ghclient.IdentityApp})
	setTeamReviewPosture(t, stores, info.TeamID, domain.ReviewPostureAuto)

	ctx := context.Background()
	handle, err := client.GithubCreatePendingReview(ctx, "octo", "repo", 7, "headsha7", nil)
	if err != nil {
		t.Fatalf("GithubCreatePendingReview: %v", err)
	}
	if _, err := client.GithubAddPendingReviewComment(ctx, "octo", "repo", handle, "a.go", "nit", 3, nil, "worktree_head"); err != nil {
		t.Fatalf("GithubAddPendingReviewComment: %v", err)
	}

	res, err := client.FinalizeReviewDraft(ctx, handle, "COMMENT", "## body")
	if err != nil {
		t.Fatalf("FinalizeReviewDraft must not fail the run on a submit failure: %v", err)
	}
	if res.Posted {
		t.Error("Posted = true after a failed submit — the response must report what actually happened")
	}

	arts := listConversationArtifacts(t, stores, info.ConversationID)
	if len(arts) != 1 {
		t.Fatalf("want 1 artifact, got %d", len(arts))
	}
	if arts[0].State != domain.ArtifactStateReviewPending {
		t.Errorf("state = %q, want pending so a human can still approve the draft", arts[0].State)
	}
	d, _ := domain.ParseReviewArtifactDetails(arts[0].DetailsJSON)
	if d.ReviewEvent != "COMMENT" || len(d.StagedComments) != 1 {
		t.Errorf("the staged draft must survive a failed submit intact: %+v", d)
	}
}

// TestPostFinalizedReview_LosesClaimToConcurrentApprove is the regression guard
// on the double-post race. A finalized draft is reachable by two independent
// publishers: this run's auto-post, and any human clicking Approve in the queue
// — both of which submit by calling SubmitReview. The window is real, because
// the ready sentinel is saved several round-trips (a settings read, a credential
// probe, a client build) before the submit, and the draft is approvable from the
// instant that write commits.
//
// The anti-double-submit sentinel does not cover this: it only refuses a second
// finalize from the same run. What settles it is the pending → submitted CAS,
// which admits exactly one publisher. Here the human wins it, and the auto-post
// must back off having made zero GitHub calls.
func TestPostFinalizedReview_LosesClaimToConcurrentApprove(t *testing.T) {
	head := "commit_v1"
	gh := newReviewPostureGitHub(t, &head)
	stores, info, client := newGithubRecordingClient(t, gh.URL, true)
	client.SetGitHubResolver(fakeGitHubResolver{baseURL: gh.URL, token: "tok", identity: ghclient.IdentityApp})
	setTeamReviewPosture(t, stores, info.TeamID, domain.ReviewPostureDraft)

	ctx := context.Background()
	handle, err := client.GithubCreatePendingReview(ctx, "octo", "repo", 7, "headsha7", nil)
	if err != nil {
		t.Fatalf("GithubCreatePendingReview: %v", err)
	}
	if _, err := client.GithubAddPendingReviewComment(ctx, "octo", "repo", handle, "a.go", "nit", 3, nil, "worktree_head"); err != nil {
		t.Fatalf("GithubAddPendingReviewComment: %v", err)
	}
	// Finalize under a staging posture: the sentinel is armed and the draft is
	// pending — exactly the state the auto-post path would find it in, and
	// exactly the state a human can approve from.
	if _, err := client.FinalizeReviewDraft(ctx, handle, "COMMENT", "## body"); err != nil {
		t.Fatalf("FinalizeReviewDraft: %v", err)
	}
	art := listConversationArtifacts(t, stores, info.ConversationID)[0]

	// The human approve lands first, taking the claim the handler takes.
	won, err := stores.Artifacts.TransitionReviewStateSystem(ctx, info.OrgID, art.ID,
		domain.ArtifactStateReviewPending, domain.ArtifactStateReviewSubmitted, "", "", "")
	if err != nil || !won {
		t.Fatalf("simulated approve claim = %v, %v; want it to win", won, err)
	}

	// Now the auto-post runs, having resolved its posture before the race.
	res := client.postFinalizedReview(ctx, &art, ReviewPostureResolution{
		Posture: domain.ReviewPostureAuto, Identity: ghclient.IdentityApp,
	})

	if res.Posted || res.URL != "" {
		t.Errorf("result = %+v, want the staged outcome — this run lost the claim and must not publish", res)
	}
	if n := len(gh.submitted()); n != 0 {
		t.Errorf("GitHub saw %d review submits from the losing publisher, want 0", n)
	}
	// The winner's claim is intact: the loser must not have released it, which
	// would put an already-approved review back in the queue.
	after := listConversationArtifacts(t, stores, info.ConversationID)[0]
	if after.State != domain.ArtifactStateReviewSubmitted {
		t.Errorf("state = %q, want the approver's claim left untouched (submitted)", after.State)
	}
}

// TestPostFinalizedReview_ClaimsBeforePosting is the other side of the race: the
// auto-post takes the claim first, so a human approve arriving afterwards loses
// it and never reaches GitHub.
func TestPostFinalizedReview_ClaimsBeforePosting(t *testing.T) {
	head := "commit_v1"
	gh := newReviewPostureGitHub(t, &head)
	stores, info, client := newGithubRecordingClient(t, gh.URL, true)
	client.SetGitHubResolver(fakeGitHubResolver{baseURL: gh.URL, token: "tok", identity: ghclient.IdentityApp})
	setTeamReviewPosture(t, stores, info.TeamID, domain.ReviewPostureAuto)

	ctx := context.Background()
	handle, err := client.GithubCreatePendingReview(ctx, "octo", "repo", 7, "headsha7", nil)
	if err != nil {
		t.Fatalf("GithubCreatePendingReview: %v", err)
	}
	if _, err := client.GithubAddPendingReviewComment(ctx, "octo", "repo", handle, "a.go", "nit", 3, nil, "worktree_head"); err != nil {
		t.Fatalf("GithubAddPendingReviewComment: %v", err)
	}
	if _, err := client.FinalizeReviewDraft(ctx, handle, "COMMENT", "## body"); err != nil {
		t.Fatalf("FinalizeReviewDraft: %v", err)
	}
	if n := len(gh.submitted()); n != 1 {
		t.Fatalf("GitHub saw %d submits, want 1", n)
	}

	// A human approve arriving now takes the same CAS the handler takes — and
	// loses, so the handler 409s instead of posting the review a second time.
	art := listConversationArtifacts(t, stores, info.ConversationID)[0]
	claimed, err := stores.Artifacts.TransitionReviewStateSystem(ctx, info.OrgID, art.ID,
		domain.ArtifactStateReviewPending, domain.ArtifactStateReviewSubmitted, "", "", "")
	if err != nil {
		t.Fatalf("late approve claim: %v", err)
	}
	if claimed {
		t.Error("a human approve after the auto-post won the claim — that is the double-submit")
	}
}

// TestPostFinalizedReview_PostsThePostClaimContent pins the claim as the
// linearization point for the draft's CONTENT, not just for who publishes.
// Every human draft mutation is pending-guarded, so one landing in the window
// between the sentinel write and the claim commits first and must be what
// reaches GitHub — posting the run's pre-claim snapshot would silently drop it.
func TestPostFinalizedReview_PostsThePostClaimContent(t *testing.T) {
	head := "commit_v1"
	gh := newReviewPostureGitHub(t, &head)
	stores, info, client := newGithubRecordingClient(t, gh.URL, true)
	client.SetGitHubResolver(fakeGitHubResolver{baseURL: gh.URL, token: "tok", identity: ghclient.IdentityApp})
	setTeamReviewPosture(t, stores, info.TeamID, domain.ReviewPostureDraft)

	ctx := context.Background()
	handle, err := client.GithubCreatePendingReview(ctx, "octo", "repo", 7, "headsha7", nil)
	if err != nil {
		t.Fatalf("GithubCreatePendingReview: %v", err)
	}
	if _, err := client.GithubAddPendingReviewComment(ctx, "octo", "repo", handle, "a.go", "nit", 3, nil, "worktree_head"); err != nil {
		t.Fatalf("GithubAddPendingReviewComment: %v", err)
	}
	if _, err := client.FinalizeReviewDraft(ctx, handle, "COMMENT", "## agent body"); err != nil {
		t.Fatalf("FinalizeReviewDraft: %v", err)
	}
	art := listConversationArtifacts(t, stores, info.ConversationID)[0]

	// A human edits the staged body while the draft is still pending — the
	// PATCH the overlay makes, guarded on pending exactly like this one.
	edited, err := domain.ParseReviewArtifactDetails(art.DetailsJSON)
	if err != nil {
		t.Fatalf("parse details: %v", err)
	}
	edited.ReviewBody = "## human-edited body"
	ok, err := stores.Artifacts.UpdateReviewDetailsIfPendingSystem(ctx, info.OrgID, art.ID,
		domain.MarshalReviewArtifactDetails(edited))
	if err != nil || !ok {
		t.Fatalf("simulated human edit = %v, %v; want it to land while pending", ok, err)
	}

	// The auto-post runs holding the pre-edit snapshot in `art`.
	res := client.postFinalizedReview(ctx, &art, ReviewPostureResolution{
		Posture: domain.ReviewPostureAuto, Identity: ghclient.IdentityApp,
	})
	if !res.Posted {
		t.Fatalf("result = %+v, want the review posted", res)
	}
	submits := gh.submitted()
	if len(submits) != 1 {
		t.Fatalf("GitHub saw %d submits, want 1", len(submits))
	}
	if body, _ := submits[0]["body"].(string); !strings.HasPrefix(body, "## human-edited body") {
		t.Errorf("submitted body = %q, want the human's edit — the claim is the linearization point", body)
	}
}

// TestPostFinalizedReview_ReEvaluatesPostureAgainstPostClaimContent pins the
// other half of that re-read: the posture decision is about the content, so a
// human flipping the draft to REQUEST_CHANGES in the claim window turns an
// auto_unless_blocking review into one that must be staged. Honoring the stale
// answer would post the very review the posture exists to hold back.
func TestPostFinalizedReview_ReEvaluatesPostureAgainstPostClaimContent(t *testing.T) {
	head := "commit_v1"
	gh := newReviewPostureGitHub(t, &head)
	stores, info, client := newGithubRecordingClient(t, gh.URL, true)
	client.SetGitHubResolver(fakeGitHubResolver{baseURL: gh.URL, token: "tok", identity: ghclient.IdentityApp})
	setTeamReviewPosture(t, stores, info.TeamID, domain.ReviewPostureDraft)

	ctx := context.Background()
	handle, err := client.GithubCreatePendingReview(ctx, "octo", "repo", 7, "headsha7", nil)
	if err != nil {
		t.Fatalf("GithubCreatePendingReview: %v", err)
	}
	if _, err := client.GithubAddPendingReviewComment(ctx, "octo", "repo", handle, "a.go", "nit", 3, nil, "worktree_head"); err != nil {
		t.Fatalf("GithubAddPendingReviewComment: %v", err)
	}
	if _, err := client.FinalizeReviewDraft(ctx, handle, "COMMENT", "## body"); err != nil {
		t.Fatalf("FinalizeReviewDraft: %v", err)
	}
	art := listConversationArtifacts(t, stores, info.ConversationID)[0]

	// The human escalates the verdict while it's still pending.
	escalated, err := domain.ParseReviewArtifactDetails(art.DetailsJSON)
	if err != nil {
		t.Fatalf("parse details: %v", err)
	}
	escalated.ReviewEvent = "REQUEST_CHANGES"
	ok, err := stores.Artifacts.UpdateReviewDetailsIfPendingSystem(ctx, info.OrgID, art.ID,
		domain.MarshalReviewArtifactDetails(escalated))
	if err != nil || !ok {
		t.Fatalf("simulated escalation = %v, %v; want it to land while pending", ok, err)
	}

	// The posture was resolved against the pre-escalation COMMENT verdict.
	res := client.postFinalizedReview(ctx, &art, ReviewPostureResolution{
		Posture: domain.ReviewPostureAutoUnlessBlocking, Identity: ghclient.IdentityApp,
	})
	if res.Posted {
		t.Error("posted a REQUEST_CHANGES review under auto_unless_blocking — the posture must be re-applied to the claimed content")
	}
	if n := len(gh.submitted()); n != 0 {
		t.Errorf("GitHub saw %d submits, want 0", n)
	}
	// The claim is released, so the escalated review is back in the queue.
	after := listConversationArtifacts(t, stores, info.ConversationID)[0]
	if after.State != domain.ArtifactStateReviewPending {
		t.Errorf("state = %q, want pending — the claim must be released when the run declines to post", after.State)
	}
}
