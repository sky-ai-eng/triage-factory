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
// path records a `pull_request` deliverable at the LocalClient seam on success, so
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

// TestLocalClient_GithubCreatePR_RecordsDeliverable pins that opening a draft PR
// lands one `pull_request` deliverable carrying the draft state, the PR number,
// node_id, and the proposed snapshot (the agent's title/body), deduped on
// github:pull_request:owner/repo#<number> — across both write paths.
func TestLocalClient_GithubCreatePR_RecordsDeliverable(t *testing.T) {
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

			arts := listConversationDeliverables(t, stores, info.ConversationID)
			if len(arts) != 1 {
				t.Fatalf("want 1 deliverable, got %d: %+v", len(arts), arts)
			}
			a := arts[0]
			if a.Provider != domain.ProviderGitHub || a.Kind != domain.DeliverableKindPullRequest ||
				a.Target != "octo/repo#42" || a.ExternalID != "42" ||
				a.URL != "https://github.com/octo/repo/pull/42" ||
				a.State != domain.DeliverableStatePRDraft || a.DedupKey != domain.PullRequestDedupKey("PR_kwDOABCD") {
				t.Errorf("PR deliverable mismatch: %+v", a)
			}
			if a.ConversationID != info.ConversationID || a.TeamID != runmode.LocalDefaultTeamID {
				t.Errorf("attribution mismatch: run=%q team=%q", a.ConversationID, a.TeamID)
			}

			d, derr := domain.ParsePRDeliverableDetails(a.DetailsJSON)
			if derr != nil {
				t.Fatalf("ParsePRDeliverableDetails: %v", derr)
			}
			if d.NodeID != "PR_kwDOABCD" || d.HeadBranch != "feature/x" || d.Base != "main" {
				t.Errorf("details coords mismatch: %+v", d)
			}
			// The proposed snapshot is the body exactly as GitHub received it —
			// footer included — since it is the durable record of what the agent
			// drafted.
			if d.Proposed.Title != "Fix the thing" || d.Proposed.Body != *sentBody {
				t.Errorf("proposed snapshot mismatch: %+v (sent %q)", d.Proposed, *sentBody)
			}
			if d.Snapshot != d.Proposed {
				t.Errorf("snapshot should start equal to proposed: %+v vs %+v", d.Snapshot, d.Proposed)
			}

			// The node id is also on the deliverable's column and the pr_created
			// row: the reconciler's backstop matches the two by it.
			if a.NodeID != "PR_kwDOABCD" {
				t.Errorf("deliverable node_id = %q, want PR_kwDOABCD", a.NodeID)
			}
			acts := listExternalActions(t, stores)
			if len(acts) != 1 {
				t.Fatalf("want 1 ledger row, got %d: %+v", len(acts), acts)
			}
			if act := acts[0]; act.Action != domain.ActionPRCreated || act.Target != "octo/repo#42" || act.NodeID != "PR_kwDOABCD" {
				t.Errorf("ledger row = action %q target %q node_id %q, want pr_created on octo/repo#42 carrying PR_kwDOABCD",
					act.Action, act.Target, act.NodeID)
			}
		})
	}
}

// TestLocalClient_GithubCreatePR_FailedDeliverableRowCarriesNodeID pins that when
// the deliverable write fails, the pr_created row that lands alone still carries
// the PR's node id. That row is all the reconciler's backstop has to find the
// PR by, on both trigger types.
func TestLocalClient_GithubCreatePR_FailedDeliverableRowCarriesNodeID(t *testing.T) {
	for _, eventTriggered := range []bool{true, false} {
		name := "manual"
		if eventTriggered {
			name = "event-triggered"
		}
		t.Run(name, func(t *testing.T) {
			gh, _ := startFakeGitHubPRCreate(t)
			conn, stores, info, client := newGithubRecordingClientConn(t, gh.URL, eventTriggered)
			refuseDeliverableWrites(t, conn)

			if _, _, _, err := client.GithubCreatePR(
				context.Background(), "octo", "repo", "feature/x", "main", "t", "b", true,
			); err != nil {
				t.Fatalf("GithubCreatePR: %v", err)
			}

			if arts := listConversationDeliverables(t, stores, info.ConversationID); len(arts) != 0 {
				t.Fatalf("deliverables = %+v, want none: the write was refused", arts)
			}
			acts := listExternalActions(t, stores)
			if len(acts) != 1 || acts[0].Action != domain.ActionPRCreated || acts[0].NodeID != "PR_kwDOABCD" {
				t.Errorf("ledger rows = %+v, want the one pr_created row carrying PR_kwDOABCD", acts)
			}
		})
	}
}

// TestLocalClient_GithubCreatePR_RecordingFailure_DoesNotFailAction pins the
// best-effort contract: a deliverables write that errors is swallowed (logged) and
// the PR create — already applied on GitHub — still returns success.
func TestLocalClient_GithubCreatePR_RecordingFailure_DoesNotFailAction(t *testing.T) {
	gh, _ := startFakeGitHubPRCreate(t)
	_, _, client := newGithubRecordingClient(t, gh.URL, true)
	rec := &erroringDeliverables{}
	client.stores.Deliverables = rec
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
