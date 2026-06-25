package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// pendingReviewGQL returns a GetPendingReview GraphQL response carrying one
// inline comment whose body has a severity badge baked in.
func pendingReviewGQL(reviewID, commentID, badgedBody string) string {
	b, _ := json.Marshal(badgedBody)
	return `{"data":{"repository":{"pullRequest":{"reviews":{"nodes":[{"id":"` + reviewID +
		`","viewerDidAuthor":true,"comments":{"nodes":[{"id":"` + commentID +
		`","path":"a.go","line":3,"startLine":null,"body":` + string(b) + `}]}}]}}}}}`
}

// TestLocalClient_GithubCreatePendingReview_RecordsArtifact pins that start-review
// lands one `review` artifact: state=pending (no ready sentinel yet), the review
// node id as ExternalID, the PR node id + number in details, deduped on
// github:review:owner/repo#<number> — across both write paths.
func TestLocalClient_GithubCreatePendingReview_RecordsArtifact(t *testing.T) {
	for _, eventTriggered := range []bool{true, false} {
		name := "manual"
		if eventTriggered {
			name = "event-triggered"
		}
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/graphql"):
					// Collision check: no existing pending review on the PR.
					_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequest":{"reviews":{"nodes":[]}}}}}`)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reviews"):
					_, _ = io.WriteString(w, `{"id":555,"node_id":"PRR_new"}`)
				case r.Method == http.MethodGet:
					// GetPRBasic for the PR node_id recorded into details.
					_, _ = io.WriteString(w, `{"number":7,"node_id":"PR_node7"}`)
				default:
					_, _ = io.WriteString(w, `{}`)
				}
			}))
			t.Cleanup(srv.Close)
			stores, info, client := newGithubRecordingClient(t, srv.URL, eventTriggered)

			reviewID, err := client.GithubCreatePendingReview(context.Background(), "octo", "repo", 7, "sha", nil)
			if err != nil {
				t.Fatalf("GithubCreatePendingReview: %v", err)
			}
			if reviewID != "PRR_new" {
				t.Fatalf("reviewID = %q, want PRR_new", reviewID)
			}

			arts := listRunArtifacts(t, stores, info.RunID)
			if len(arts) != 1 {
				t.Fatalf("want 1 artifact, got %d: %+v", len(arts), arts)
			}
			a := arts[0]
			if a.Kind != domain.ArtifactKindReview || a.State != domain.ArtifactStateReviewPending ||
				a.Target != "octo/repo#7" || a.ExternalID != "PRR_new" || a.DedupKey != "github:review:octo/repo#7" {
				t.Errorf("review artifact mismatch: %+v", a)
			}
			d, derr := domain.ParseReviewArtifactDetails(a.DetailsJSON)
			if derr != nil {
				t.Fatalf("ParseReviewArtifactDetails: %v", derr)
			}
			if d.NodeID != "PR_node7" || d.Number != 7 {
				t.Errorf("details coords mismatch: %+v", d)
			}
			// Fresh review: no ready sentinel until submit-review.
			if d.ReviewEvent != "" {
				t.Errorf("fresh review must have no ready sentinel: %+v", d)
			}
		})
	}
}

// TestLocalClient_FinalizeReviewDraft pins submit-review's host behavior: no
// GitHub submit, the agent's draft (body + event + live comments) snapshotted into
// the artifact, the ready sentinel set, and the TFAC-358 anti-double-submit guard.
func TestLocalClient_FinalizeReviewDraft(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/graphql") {
			_, _ = io.WriteString(w, pendingReviewGQL("PRR_1", "PRRC_1", domain.SeverityBadgeMarkdown(domain.SeverityMajor)+"nit: rename"))
			return
		}
		// Any REST hit here would mean an unexpected GitHub submit — fail loudly.
		t.Errorf("unexpected GitHub REST call during finalize: %s %s", r.Method, r.URL.Path)
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(srv.Close)
	stores, info, client := newGithubRecordingClient(t, srv.URL, true)

	// Seed the run's pending review artifact (as start-review would have).
	seed := domain.NewReviewArtifact("octo/repo", 7, "PR_node7", "PRR_1")
	if _, err := client.UpsertArtifact(context.Background(), seed); err != nil {
		t.Fatalf("seed review artifact: %v", err)
	}

	if err := client.FinalizeReviewDraft(context.Background(), "PRR_1", "COMMENT", "## Review body"); err != nil {
		t.Fatalf("FinalizeReviewDraft: %v", err)
	}

	arts := listRunArtifacts(t, stores, info.RunID)
	if len(arts) != 1 {
		t.Fatalf("want 1 artifact, got %d", len(arts))
	}
	if arts[0].State != domain.ArtifactStateReviewPending {
		t.Errorf("artifact must stay pending until approval, got %q", arts[0].State)
	}
	d, _ := domain.ParseReviewArtifactDetails(arts[0].DetailsJSON)
	if d.ReviewEvent != "COMMENT" || d.ReviewBody != "## Review body" {
		t.Errorf("ready sentinel / staged body not set: %+v", d)
	}
	if len(d.Proposed.Comments) != 1 || d.Proposed.Comments[0].ID != "PRRC_1" {
		t.Errorf("proposed comments not snapshotted from the live pending review: %+v", d.Proposed)
	}

	// Anti-double-submit (TFAC-358): a second finalize hard-errors.
	err := client.FinalizeReviewDraft(context.Background(), "PRR_1", "COMMENT", "## again")
	if !errors.Is(err, ErrReviewAlreadyFinalized) {
		t.Errorf("second submit-review = %v, want ErrReviewAlreadyFinalized", err)
	}
}
