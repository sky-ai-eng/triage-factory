package gh

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
)

// finalizeHost is a host whose FinalizeReviewDraft returns a canned outcome —
// the seam the response contract rests on. It embeds agenthost.Client (nil) so
// it satisfies the interface; prFinalizeReview reaches only LookupRun and
// FinalizeReviewDraft.
type finalizeHost struct {
	agenthost.Client
	result agenthost.ReviewFinalizeResult
	gotID  string
	gotEv  string
}

func (h *finalizeHost) LookupRun(context.Context) (agenthost.ConversationInfo, error) {
	return agenthost.ConversationInfo{ConversationID: "run-1", OrgID: "org-1"}, nil
}

func (h *finalizeHost) FinalizeReviewDraft(_ context.Context, reviewID, event, _ string) (agenthost.ReviewFinalizeResult, error) {
	h.gotID, h.gotEv = reviewID, event
	return h.result, nil
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what it
// wrote. printJSON writes straight to os.Stdout, which is the surface under
// test — the agent reads this JSON, so the assertion has to be on the bytes.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	os.Stdout = orig
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

// TestPrFinalizeReview_ReportsWhatHappened pins the response contract behind the
// per-team review posture: the prompt describes the SHAPE of the
// call, the tool response reports WHICH of the two things the host did. A verb
// that hardcoded "drafted_awaiting_approval" would be wrong for every team that
// switches posture, and the agent's wrap-up would be told a review is waiting on
// a human who has nothing to approve.
func TestPrFinalizeReview_ReportsWhatHappened(t *testing.T) {
	cases := []struct {
		name      string
		result    agenthost.ReviewFinalizeResult
		wantState string
		wantURL   string
	}{
		{
			name:      "staged for approval",
			result:    agenthost.ReviewFinalizeResult{},
			wantState: "drafted_awaiting_approval",
		},
		{
			name:      "submitted to github",
			result:    agenthost.ReviewFinalizeResult{Posted: true, URL: "https://github.com/octo/repo/pull/7#pullrequestreview-99"},
			wantState: "submitted",
			wantURL:   "https://github.com/octo/repo/pull/7#pullrequestreview-99",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := &finalizeHost{result: tc.result}
			out := captureStdout(t, func() {
				prFinalizeReview(context.Background(), host, []string{"rev-1", "--event", "comment", "--body", "looks fine"})
			})

			var got map[string]any
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("finalize-review printed non-JSON %q: %v", out, err)
			}
			if got["status"] != tc.wantState {
				t.Errorf("status = %v, want %q", got["status"], tc.wantState)
			}
			if got["review_id"] != "rev-1" || got["event"] != "COMMENT" {
				t.Errorf("review_id/event = %v/%v, want rev-1/COMMENT", got["review_id"], got["event"])
			}
			if tc.wantURL == "" {
				if _, ok := got["url"]; ok {
					t.Errorf("a staged review must carry no url (nothing is on GitHub yet); got %v", got["url"])
				}
			} else if got["url"] != tc.wantURL {
				t.Errorf("url = %v, want %q", got["url"], tc.wantURL)
			}
			// next_step tells the agent its review work is done under BOTH
			// outcomes — that's what stops the finalize loop.
			step, _ := got["next_step"].(string)
			if !strings.Contains(step, "Do not call finalize-review again") {
				t.Errorf("next_step = %q, want the do-not-retry wrap-up", step)
			}
			if host.gotEv != "COMMENT" {
				t.Errorf("host saw event %q, want the mapped COMMENT", host.gotEv)
			}
		})
	}
}
