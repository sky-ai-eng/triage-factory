package agenthost

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// startFakeGitHubPRCreate stands a minimal GitHub REST backend for the pulls
// create verb: POST /repos/{o}/{r}/pulls → {number, html_url, node_id}. The PR
// path records a `pull_request` artifact at the LocalClient seam on success, so
// this proves the recording for both the sandbox (daemon) and local-mode CLI in
// one place. The returned pointer receives the body of the last create request
// as GitHub saw it.
func startFakeGitHubPRCreate(t *testing.T) (*httptest.Server, *string) {
	t.Helper()
	var sentBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			var req struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			sentBody = req.Body
			_, _ = io.WriteString(w, `{"number":42,"html_url":"https://github.com/octo/repo/pull/42","node_id":"PR_kwDOABCD"}`)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &sentBody
}

// TestLocalClient_GithubCreatePR_RecordsArtifact pins that opening a draft PR
// lands one `pull_request` artifact carrying the draft state, the PR number,
// node_id, and the proposed snapshot (the agent's title/body), deduped on
// github:pull_request:owner/repo#<number> — across both write paths.
func TestLocalClient_GithubCreatePR_RecordsArtifact(t *testing.T) {
	for _, eventTriggered := range []bool{true, false} {
		name := "manual"
		if eventTriggered {
			name = "event-triggered"
		}
		t.Run(name, func(t *testing.T) {
			gh, sentBody := startFakeGitHubPRCreate(t)
			stores, info, client := newGithubRecordingClient(t, gh.URL, eventTriggered)

			number, htmlURL, nodeID, err := client.GithubCreatePR(
				context.Background(), "octo", "repo", "feature/x", "main",
				"Fix the thing", "Proposed body", true,
			)
			if err != nil {
				t.Fatalf("GithubCreatePR: %v", err)
			}
			if number != 42 || htmlURL != "https://github.com/octo/repo/pull/42" || nodeID != "PR_kwDOABCD" {
				t.Fatalf("create result mismatch: number=%d url=%q node=%q", number, htmlURL, nodeID)
			}

			arts := listConversationArtifacts(t, stores, info.ConversationID)
			if len(arts) != 1 {
				t.Fatalf("want 1 artifact, got %d: %+v", len(arts), arts)
			}
			a := arts[0]
			if a.Provider != domain.ArtifactProviderGitHub || a.Kind != domain.ArtifactKindPullRequest ||
				a.Target != "octo/repo#42" || a.ExternalID != "42" ||
				a.URL != "https://github.com/octo/repo/pull/42" ||
				a.State != domain.ArtifactStatePRDraft || a.DedupKey != "github:pull_request:octo/repo#42" {
				t.Errorf("PR artifact mismatch: %+v", a)
			}
			if a.ConversationID != info.ConversationID || a.TeamID != runmode.LocalDefaultTeamID {
				t.Errorf("attribution mismatch: run=%q team=%q", a.ConversationID, a.TeamID)
			}

			d, derr := domain.ParsePRArtifactDetails(a.DetailsJSON)
			if derr != nil {
				t.Fatalf("ParsePRArtifactDetails: %v", derr)
			}
			if d.NodeID != "PR_kwDOABCD" || d.HeadBranch != "feature/x" || d.Base != "main" {
				t.Errorf("details coords mismatch: %+v", d)
			}
			// The proposed snapshot is the body exactly as GitHub received it —
			// footer included — so the approval-time verdict diff compares like
			// with like.
			if d.Proposed.Title != "Fix the thing" || d.Proposed.Body != *sentBody {
				t.Errorf("proposed snapshot mismatch: %+v (sent %q)", d.Proposed, *sentBody)
			}
			if d.Snapshot != d.Proposed {
				t.Errorf("snapshot should start equal to proposed: %+v vs %+v", d.Snapshot, d.Proposed)
			}
		})
	}
}

// TestLocalClient_GithubCreatePR_RecordingFailure_DoesNotFailAction pins the
// best-effort contract: an artifacts write that errors is swallowed (logged) and
// the PR create — already applied on GitHub — still returns success.
func TestLocalClient_GithubCreatePR_RecordingFailure_DoesNotFailAction(t *testing.T) {
	gh, _ := startFakeGitHubPRCreate(t)
	_, _, client := newGithubRecordingClient(t, gh.URL, true)
	rec := &erroringArtifacts{}
	client.stores.Artifacts = rec
	// The runtime is derived from stores at construction, so a post-construction
	// swap of the recorder must rebuild it (the DB effects route through c.rt).
	client.rt = newDirectRuntime(client.stores, client.info)

	number, _, _, err := client.GithubCreatePR(
		context.Background(), "octo", "repo", "feature/x", "main", "t", "b", true,
	)
	if err != nil {
		t.Fatalf("GithubCreatePR must succeed even when recording fails: %v", err)
	}
	if number != 42 {
		t.Fatalf("number = %d, want 42", number)
	}
	if rec.callCount() == 0 {
		t.Errorf("expected the recording path to attempt a write")
	}
}

// TestLocalClient_GithubCreatePR_AppendsDisclosureFooter pins that the draft
// carries the disclosure footer from the moment it is opened — never a bare
// agent-authored body waiting on approval — with the run's deep link when the
// spawner stamped one and without it otherwise, and never a spend or elapsed
// figure, which would be partial at creation and stale ever after.
func TestLocalClient_GithubCreatePR_AppendsDisclosureFooter(t *testing.T) {
	for _, tc := range []struct {
		name   string
		runURL string
	}{
		{name: "with run link", runURL: "http://tf.test/runs/22222222-2222-2222-2222-222222222222"},
		{name: "without public URL", runURL: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh, sentBody := startFakeGitHubPRCreate(t)
			_, _, client := newGithubRecordingClient(t, gh.URL, false)
			client.info.RunURL = tc.runURL
			client.rt = newDirectRuntime(client.stores, client.info)

			if _, _, _, err := client.GithubCreatePR(
				context.Background(), "octo", "repo", "feature/x", "main", "T", "Proposed body", true,
			); err != nil {
				t.Fatalf("GithubCreatePR: %v", err)
			}
			got := *sentBody
			if !strings.HasPrefix(got, "Proposed body\n\n---\n*This PR was partially generated by AI using [Triage Factory]") {
				t.Errorf("body sent to GitHub = %q, want the draft followed by the disclosure footer", got)
			}
			link := "[View the run](" + tc.runURL + ")"
			if tc.runURL != "" && !strings.Contains(got, link) {
				t.Errorf("body sent to GitHub = %q, want the run link %q", got, link)
			}
			if tc.runURL == "" && strings.Contains(got, "View the run") {
				t.Errorf("body sent to GitHub = %q, want no run link when no public URL is configured", got)
			}
			for _, banned := range []string{"Time:", "Cost:", "Model:"} {
				if strings.Contains(got, banned) {
					t.Errorf("body sent to GitHub carries %q; the footer discloses, it does not meter: %q", banned, got)
				}
			}
		})
	}
}

// TestLocalClient_GithubCreatePR_NoConversationNoFooter pins the standalone
// case: a body opened with no conversation behind it is sent exactly as given,
// because there is no agent to disclose.
func TestLocalClient_GithubCreatePR_NoConversationNoFooter(t *testing.T) {
	gh, sentBody := startFakeGitHubPRCreate(t)
	_, _, client := newGithubRecordingClient(t, gh.URL, false)
	client.info.ConversationID = ""
	client.rt = newDirectRuntime(client.stores, client.info)

	if _, _, _, err := client.GithubCreatePR(
		context.Background(), "octo", "repo", "feature/x", "main", "T", "Plain body", true,
	); err != nil {
		t.Fatalf("GithubCreatePR: %v", err)
	}
	if *sentBody != "Plain body" {
		t.Errorf("body sent to GitHub = %q, want it untouched with no conversation", *sentBody)
	}
}
