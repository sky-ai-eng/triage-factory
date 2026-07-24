package agenthost

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// startFakeGitHubPRCreate stands a minimal GitHub REST backend for the pulls
// create verb: POST /repos/{o}/{r}/pulls → {number, html_url, node_id}. The PR
// path records a `pull_request` artifact at the LocalClient seam on success, so
// this proves the recording for both the sandbox (daemon) and local-mode CLI in
// one place.
func startFakeGitHubPRCreate(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			_, _ = io.WriteString(w, `{"number":42,"html_url":"https://github.com/octo/repo/pull/42","node_id":"PR_kwDOABCD"}`)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(srv.Close)
	return srv
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
			gh := startFakeGitHubPRCreate(t)
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

			arts := listRunArtifacts(t, stores, info.RunID)
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
			if a.ConversationID != info.RunID || a.TeamID != runmode.LocalDefaultTeamID {
				t.Errorf("attribution mismatch: run=%q team=%q", a.ConversationID, a.TeamID)
			}

			d, derr := domain.ParsePRArtifactDetails(a.DetailsJSON)
			if derr != nil {
				t.Fatalf("ParsePRArtifactDetails: %v", derr)
			}
			if d.NodeID != "PR_kwDOABCD" || d.HeadBranch != "feature/x" || d.Base != "main" {
				t.Errorf("details coords mismatch: %+v", d)
			}
			if d.Proposed.Title != "Fix the thing" || d.Proposed.Body != "Proposed body" {
				t.Errorf("proposed snapshot mismatch: %+v", d.Proposed)
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
	gh := startFakeGitHubPRCreate(t)
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
