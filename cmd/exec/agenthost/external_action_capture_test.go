package agenthost

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/ghwrite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The TFAC-483 capture tests prove the bot funnels append the right
// external_actions audit row alongside the artifact upsert — the finer-grained
// action discriminator the artifact's state can't carry, the org credential, the
// run + actor attribution, and the branch hook+proxy dedup. The LocalClient is
// the one seam the multi daemon dispatches through, so this covers both modes.

// newCaptureStores builds a real SQLite store bundle (Artifacts + ExternalActions
// + Tx live) with a seeded run, no Jira credential needed (the branch path and
// the injected-error paths don't call out). eventTriggered picks the write path:
// admin pool (no user) when true, a synthetic-claims tx (manual run, with a user)
// when false.
func newCaptureStores(t *testing.T, eventTriggered bool) (db.Stores, RunInfo) {
	_, stores, info := newCaptureStoresConn(t, eventTriggered)
	return stores, info
}

// newCaptureStoresConn is newCaptureStores plus the raw *sql.DB, for touch tests
// that read conversation_memory_entities directly (the store interface has no
// role-returning read).
func newCaptureStoresConn(t *testing.T, eventTriggered bool) (*sql.DB, db.Stores, RunInfo) {
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
	const runID = "11111111-1111-1111-1111-111111111111"
	if _, err := conn.Exec(`INSERT INTO conversations (id, origin, status) VALUES (?, 'interactive', 'running')`, runID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	userID := ""
	if !eventTriggered {
		userID = runmode.LocalDefaultUserID
	}
	return conn, sqlitestore.New(conn), RunInfo{
		OrgID:            runmode.LocalDefaultOrgID,
		TeamID:           runmode.LocalDefaultTeamID,
		RunID:            runID,
		UserID:           userID,
		IsEventTriggered: eventTriggered,
	}
}

func listExternalActions(t *testing.T, stores db.Stores) []domain.ExternalAction {
	t.Helper()
	rows, err := stores.ExternalActions.ListByOrgSystem(context.Background(), runmode.LocalDefaultOrgID, domain.ExternalActionListOpts{})
	if err != nil {
		t.Fatalf("ListByOrgSystem: %v", err)
	}
	return rows
}

// TestCapture_JiraActions_RecordExternalActions pins that a Jira create and a
// transition each append one external_actions row with the right action (finer
// than the artifact's created/updated state), the org Jira credential, the run +
// actor attribution, and the transition's to_state — across both write paths.
func TestCapture_JiraActions_RecordExternalActions(t *testing.T) {
	for _, eventTriggered := range []bool{true, false} {
		name := "manual"
		if eventTriggered {
			name = "event-triggered"
		}
		t.Run(name, func(t *testing.T) {
			t.Run("create", func(t *testing.T) {
				jira := startFakeJira(t)
				stores, info := newJiraRecordingStoresForCapture(t, jira.URL, eventTriggered)
				client := NewLocal(stores, info)

				if _, err := client.JiraCreateIssue(context.Background(), "SKY", "Task", "do a thing", "", "", ""); err != nil {
					t.Fatalf("JiraCreateIssue: %v", err)
				}
				acts := listExternalActions(t, stores)
				if len(acts) != 1 {
					t.Fatalf("want 1 action, got %d: %+v", len(acts), acts)
				}
				a := acts[0]
				if a.Action != domain.ActionIssueCreated || a.Provider != domain.ArtifactProviderJira ||
					a.Credential != domain.CredentialJiraOrg || a.Target != "SKY-1" || a.ConversationID != info.RunID ||
					a.TeamID != runmode.LocalDefaultTeamID {
					t.Errorf("create action mismatch: %+v", a)
				}
				assertActor(t, a, eventTriggered)
			})

			t.Run("transition carries from/to", func(t *testing.T) {
				jira := startFakeJira(t)
				stores, info := newJiraRecordingStoresForCapture(t, jira.URL, eventTriggered)
				client := NewLocal(stores, info)

				if err := client.JiraTransitionTo(context.Background(), "SKY-9", "Done"); err != nil {
					t.Fatalf("JiraTransitionTo: %v", err)
				}
				acts := listExternalActions(t, stores)
				if len(acts) != 1 {
					t.Fatalf("want 1 action, got %d: %+v", len(acts), acts)
				}
				a := acts[0]
				if a.Action != domain.ActionIssueTransitioned || a.ToState != "Done" || a.Credential != domain.CredentialJiraOrg {
					t.Errorf("transition action mismatch: %+v", a)
				}
			})
		})
	}
}

// TestCapture_BranchPush_RecordsActionAndDedupsTwin pins the branch capture: a
// push records ActionBranchPushed under the github credential with the
// deterministic run:ref:sha dedup key, and a second observation of the SAME push
// (the git hook+proxy twin) collapses to one action row.
func TestCapture_BranchPush_RecordsActionAndDedupsTwin(t *testing.T) {
	for _, eventTriggered := range []bool{true, false} {
		name := "manual"
		if eventTriggered {
			name = "event-triggered"
		}
		t.Run(name, func(t *testing.T) {
			stores, info := newCaptureStores(t, eventTriggered)
			client := NewLocal(stores, info)
			ctx := context.Background()

			branch, ok := domain.NewBranchArtifact("octo/repo", "refs/heads/feat", "abc123", true)
			if !ok {
				t.Fatal("NewBranchArtifact returned ok=false")
			}
			// The hook fires, then the proxy backstop observes the same push.
			if _, err := client.UpsertArtifact(ctx, branch); err != nil {
				t.Fatalf("hook UpsertArtifact: %v", err)
			}
			if _, err := client.UpsertArtifact(ctx, branch); err != nil {
				t.Fatalf("proxy UpsertArtifact (twin): %v", err)
			}

			acts := listExternalActions(t, stores)
			if len(acts) != 1 {
				t.Fatalf("branch twin did not collapse: want 1 action, got %d: %+v", len(acts), acts)
			}
			a := acts[0]
			if a.Action != domain.ActionBranchPushed || a.Provider != domain.ArtifactProviderGitHub ||
				a.Credential != domain.CredentialGitHubApp || a.Target != "octo/repo" ||
				a.ExternalID != "refs/heads/feat" {
				t.Errorf("branch action mismatch: %+v", a)
			}
			if want := domain.BranchPushDedupKey(info.RunID, "refs/heads/feat", "abc123"); a.DedupKey != want {
				t.Errorf("branch dedup_key = %q, want %q", a.DedupKey, want)
			}
			// A genuinely new push (new sha) is recorded distinctly.
			next, _ := domain.NewBranchArtifact("octo/repo", "refs/heads/feat", "def456", false)
			if _, err := client.UpsertArtifact(ctx, next); err != nil {
				t.Fatalf("new-push UpsertArtifact: %v", err)
			}
			if got := len(listExternalActions(t, stores)); got != 2 {
				t.Errorf("a new push (new sha) should append: want 2 actions, got %d", got)
			}
		})
	}
}

// TestCapture_BranchPush_AnchorsURLToOrgHost pins that the host-side sink
// re-anchors a branch artifact's web link to the org's own GitHub host. The URL
// is synthesized against github.com where the host can't be known (the sandbox
// pre-push hook, the domain constructor), so without this an org on GHES/GHEC
// would get a dead github.com "open branch" link. Both the artifact URL and the
// branch-push audit action's deep link (built from the same artifact) are
// corrected.
func TestCapture_BranchPush_AnchorsURLToOrgHost(t *testing.T) {
	stores, info := newCaptureStores(t, false)
	client := NewLocal(stores, info)
	client.SetGitHubResolver(fakeGitHubResolver{baseURL: "https://ghe.example.com", token: "org-pat"})

	branch, ok := domain.NewBranchArtifact("octo/repo", "refs/heads/feat/x", "abc123", true)
	if !ok {
		t.Fatal("NewBranchArtifact returned ok=false")
	}
	// The constructor can't know the org host, so it defaults to github.com...
	if branch.URL != "https://github.com/octo/repo/tree/feat/x" {
		t.Fatalf("precondition: constructor URL = %q, want the github.com default", branch.URL)
	}

	stored, err := client.UpsertArtifact(context.Background(), branch)
	if err != nil {
		t.Fatalf("UpsertArtifact: %v", err)
	}
	// ...and the sink rewrites it to the org's GHES host, segments preserved.
	if stored.URL != "https://ghe.example.com/octo/repo/tree/feat/x" {
		t.Errorf("stored branch URL = %q, want the org GHES host", stored.URL)
	}

	acts := listExternalActions(t, stores)
	if len(acts) != 1 {
		t.Fatalf("want 1 branch action, got %d: %+v", len(acts), acts)
	}
	if acts[0].URL != "https://ghe.example.com/octo/repo/tree/feat/x" {
		t.Errorf("branch action URL = %q, want the org GHES host", acts[0].URL)
	}
}

// TestCapture_GithubReply_RecordsArtifactlessAction pins that a review-thread
// reply — which rides the review and produces NO comment artifact — is still
// captured as a standalone comment_posted external action (the Actions lens is
// the one surface that records org-credential writes with no artifact). The
// in_reply_to context lands in detail_json. TFAC-483.
func TestCapture_GithubReply_RecordsArtifactlessAction(t *testing.T) {
	for _, eventTriggered := range []bool{true, false} {
		name := "manual"
		if eventTriggered {
			name = "event-triggered"
		}
		t.Run(name, func(t *testing.T) {
			gh := startFakeGitHubComments(t)
			stores, info, client := newGithubRecordingClient(t, gh.URL, eventTriggered)

			replyID, err := client.GithubReplyToComment(context.Background(), "octo", "repo", 1, 555, "thanks for the catch")
			if err != nil {
				t.Fatalf("GithubReplyToComment: %v", err)
			}
			if replyID != 777 {
				t.Fatalf("reply id = %d, want 777", replyID)
			}
			// No artifact — the reply rides the review thread, like every line-comment.
			if arts := listRunArtifacts(t, stores, info.RunID); len(arts) != 0 {
				t.Errorf("a review-thread reply should produce no artifact, got %+v", arts)
			}
			// But exactly one external action (the audit log records artifact-less writes).
			acts := listExternalActions(t, stores)
			if len(acts) != 1 {
				t.Fatalf("want 1 action, got %d: %+v", len(acts), acts)
			}
			a := acts[0]
			if a.Action != domain.ActionCommentPosted || a.Provider != domain.ArtifactProviderGitHub ||
				a.Credential != domain.CredentialGitHubApp || a.Target != "octo/repo#1" ||
				a.ExternalID != "777" || a.ConversationID != info.RunID {
				t.Errorf("reply action mismatch: %+v", a)
			}
			// The audit row deep-links to the reply on the PR.
			if a.URL != "https://github.com/octo/repo/pull/1#discussion_r777" {
				t.Errorf("reply url = %q, want the PR discussion deep link", a.URL)
			}
			if !strings.Contains(a.DetailJSON, `"in_reply_to":555`) {
				t.Errorf("detail_json %q did not carry in_reply_to", a.DetailJSON)
			}
		})
	}
}

// TestCapture_GithubReviewCommentEditDelete_RecordsArtifactlessAction pins
// TFAC-485: editing/deleting a PUBLISHED review line-comment via the
// pulls/comments fallback (isIssueComment == false) rides the review, so it
// writes no comment artifact — but it is still an immediate org-credential write
// the audit log of record must capture, as an artifact-less review_comment_edited
// / review_comment_deleted action. (Pending-draft churn edits human-side via the
// server staging path and is unrecorded, so reaching this branch is
// published-by-construction.)
func TestCapture_GithubReviewCommentEditDelete_RecordsArtifactlessAction(t *testing.T) {
	// A backend that 404s the issue-comments endpoint so the client falls back to
	// the pulls (review line-comment) endpoint, which succeeds.
	startReviewCommentBackend := func(t *testing.T) *httptest.Server {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
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
		t.Cleanup(srv.Close)
		return srv
	}

	for _, eventTriggered := range []bool{true, false} {
		name := "manual"
		if eventTriggered {
			name = "event-triggered"
		}
		t.Run(name, func(t *testing.T) {
			t.Run("edit records review_comment_edited", func(t *testing.T) {
				gh := startReviewCommentBackend(t)
				stores, info, client := newGithubRecordingClient(t, gh.URL, eventTriggered)
				if !eventTriggered {
					info.UserID = runmode.LocalDefaultUserID
					client = NewLocal(stores, info)
					client.SetGitHubResolver(fakeGitHubResolver{baseURL: gh.URL, token: "org-pat"})
				}

				if err := client.GithubUpdateComment(context.Background(), "octo", "repo", 999, "edit a review comment"); err != nil {
					t.Fatalf("GithubUpdateComment: %v", err)
				}
				// No comment artifact — the review line-comment rides the review.
				if arts := listRunArtifacts(t, stores, info.RunID); len(arts) != 0 {
					t.Errorf("a review line-comment edit must record no comment artifact, got %+v", arts)
				}
				acts := listExternalActions(t, stores)
				if len(acts) != 1 {
					t.Fatalf("want 1 action, got %d: %+v", len(acts), acts)
				}
				a := acts[0]
				if a.Action != domain.ActionReviewCommentEdited || a.Provider != domain.ArtifactProviderGitHub ||
					a.Credential != domain.CredentialGitHubApp || a.Target != "octo/repo" ||
					a.ExternalID != "999" || a.URL != "" || a.ConversationID != info.RunID {
					t.Errorf("review-comment edit action mismatch: %+v", a)
				}
				assertActor(t, a, eventTriggered)
			})

			t.Run("delete records review_comment_deleted", func(t *testing.T) {
				gh := startReviewCommentBackend(t)
				stores, info, client := newGithubRecordingClient(t, gh.URL, eventTriggered)
				if !eventTriggered {
					info.UserID = runmode.LocalDefaultUserID
					client = NewLocal(stores, info)
					client.SetGitHubResolver(fakeGitHubResolver{baseURL: gh.URL, token: "org-pat"})
				}

				if err := client.GithubDeleteComment(context.Background(), "octo", "repo", 999); err != nil {
					t.Fatalf("GithubDeleteComment: %v", err)
				}
				if arts := listRunArtifacts(t, stores, info.RunID); len(arts) != 0 {
					t.Errorf("a review line-comment delete must record no comment artifact, got %+v", arts)
				}
				acts := listExternalActions(t, stores)
				if len(acts) != 1 {
					t.Fatalf("want 1 action, got %d: %+v", len(acts), acts)
				}
				a := acts[0]
				if a.Action != domain.ActionReviewCommentDeleted || a.Provider != domain.ArtifactProviderGitHub ||
					a.Credential != domain.CredentialGitHubApp || a.Target != "octo/repo" ||
					a.ExternalID != "999" || a.URL != "" || a.ConversationID != info.RunID {
					t.Errorf("review-comment delete action mismatch: %+v", a)
				}
				assertActor(t, a, eventTriggered)
			})
		})
	}
}

// TestCapture_GithubStandaloneCommentEditDelete_StillRecordsCommentAction pins
// that the standalone (top-level issue) comment path is unchanged by TFAC-485: an
// edit still records ActionCommentEdited and a delete ActionCommentDeleted (via
// the artifact+action funnel), NOT the review_comment_* actions.
func TestCapture_GithubStandaloneCommentEditDelete_StillRecordsCommentAction(t *testing.T) {
	t.Run("edit records comment_edited", func(t *testing.T) {
		gh := startFakeGitHubComments(t)
		stores, _, client := newGithubRecordingClient(t, gh.URL, true)

		if err := client.GithubUpdateComment(context.Background(), "octo", "repo", 777, "edited take"); err != nil {
			t.Fatalf("GithubUpdateComment: %v", err)
		}
		acts := listExternalActions(t, stores)
		if len(acts) != 1 {
			t.Fatalf("want 1 action, got %d: %+v", len(acts), acts)
		}
		if a := acts[0]; a.Action != domain.ActionCommentEdited || a.Provider != domain.ArtifactProviderGitHub {
			t.Errorf("standalone edit action mismatch: %+v", a)
		}
	})

	t.Run("delete records comment_deleted", func(t *testing.T) {
		gh := startFakeGitHubComments(t)
		stores, _, client := newGithubRecordingClient(t, gh.URL, true)

		if err := client.GithubDeleteComment(context.Background(), "octo", "repo", 777); err != nil {
			t.Fatalf("GithubDeleteComment: %v", err)
		}
		acts := listExternalActions(t, stores)
		if len(acts) != 1 {
			t.Fatalf("want 1 action, got %d: %+v", len(acts), acts)
		}
		if a := acts[0]; a.Action != domain.ActionCommentDeleted || a.Provider != domain.ArtifactProviderGitHub {
			t.Errorf("standalone delete action mismatch: %+v", a)
		}
	})
}

// TestCapture_EventPath_RecordFailure_DoesNotFailAction pins the event-path
// best-effort contract: a forced RecordSystem failure is swallowed (the action
// row is absent) while the artifact still lands and the call returns success —
// the external write already took effect, so recording must never fail it.
func TestCapture_EventPath_RecordFailure_DoesNotFailAction(t *testing.T) {
	jira := startFakeJira(t)
	stores, info := newJiraRecordingStoresForCapture(t, jira.URL, true) // event-triggered → admin pool
	stores.ExternalActions = erroringExternalActions{}
	client := NewLocal(stores, info)

	if err := client.JiraTransitionTo(context.Background(), "SKY-9", "Done"); err != nil {
		t.Fatalf("transition must not fail when action recording fails: %v", err)
	}
	// The artifact (the separate admin write) still committed.
	if arts := listRunArtifacts(t, stores, info.RunID); len(arts) != 1 {
		t.Errorf("artifact should persist despite the action-record failure, got %d", len(arts))
	}
}

// assertActor checks the actor model: an event-triggered (autonomous) run has no
// actor (NULL); a manual run carries the kicking-off user.
func assertActor(t *testing.T, a domain.ExternalAction, eventTriggered bool) {
	t.Helper()
	if eventTriggered {
		if a.ActorUserID != "" {
			t.Errorf("event-triggered action should have no actor, got %q", a.ActorUserID)
		}
	} else if a.ActorUserID != runmode.LocalDefaultUserID {
		t.Errorf("manual action actor = %q, want the kicking-off user", a.ActorUserID)
	}
}

// newJiraRecordingStoresForCapture is newJiraRecordingStores but also sets a
// non-empty UserID on the manual path so the actor assertion has something to
// check (the shared helper leaves it empty).
func newJiraRecordingStoresForCapture(t *testing.T, jiraURL string, eventTriggered bool) (db.Stores, RunInfo) {
	t.Helper()
	stores, info := newJiraRecordingStores(t, jiraURL, eventTriggered)
	if !eventTriggered {
		info.UserID = runmode.LocalDefaultUserID
	}
	return stores, info
}

// erroringExternalActions fails every Record so the best-effort path is
// exercised. The List methods are unused here (nil embedded interface).
type erroringExternalActions struct{ db.ExternalActionStore }

func (erroringExternalActions) Record(context.Context, string, domain.ExternalAction) error {
	return errors.New("boom")
}

func (erroringExternalActions) RecordSystem(context.Context, string, domain.ExternalAction) error {
	return errors.New("boom")
}

// TestCapture_EgressDenied_RecordsOneRowPerConversationAndHost pins the audit
// row behind a refused sandbox connection — the signal that used to exist only
// as a log line. The retry loop collapses to one row per (conversation, host),
// because "the agent tried to reach api.github.com and was refused" is the
// finding; the hundredth identical row adds nothing and buries the rest of the
// log.
func TestCapture_EgressDenied_RecordsOneRowPerConversationAndHost(t *testing.T) {
	stores, info := newCaptureStores(t, true)
	client := NewLocal(stores, info)
	ctx := context.Background()

	const reason = `host "api.github.com" is not on the sandbox egress allowlist`
	for i := 0; i < 6; i++ {
		client.RecordEgressDenied(ctx, "api.github.com:443", reason)
	}
	// A different host from the same conversation is its own signal.
	client.RecordEgressDenied(ctx, "pypi.org:443", "port 8080 is not tunneled")

	acts := listExternalActions(t, stores)
	if len(acts) != 2 {
		t.Fatalf("want 2 rows (one per host, repeats collapsed), got %d: %+v", len(acts), acts)
	}
	var gh domain.ExternalAction
	for _, a := range acts {
		if a.Target == "api.github.com:443" {
			gh = a
		}
	}
	if gh.Action != domain.ActionEgressDenied || gh.Provider != domain.ArtifactProviderNetwork ||
		gh.Credential != domain.CredentialNone {
		t.Errorf("egress row mismatch: %+v", gh)
	}
	if gh.ConversationID != info.RunID {
		t.Errorf("conversation = %q, want the run's own %q", gh.ConversationID, info.RunID)
	}
	if want := domain.EgressDenialDedupKey(info.RunID, "api.github.com:443"); gh.DedupKey != want {
		t.Errorf("dedup_key = %q, want %q", gh.DedupKey, want)
	}
	if !strings.Contains(gh.DetailJSON, "allowlist") || !strings.Contains(gh.DetailJSON, "api.github.com:443") {
		t.Errorf("detail_json = %q, want the target and the refusal reason", gh.DetailJSON)
	}
}

// TestCapture_GHChannelWrite_RecordsEveryAttempt pins the gh-channel write
// audit: each mutating REST call is its own row (no dedup — a retried edit and a
// refused one are distinct events), and a non-2xx is recorded exactly like a
// success. The refused write is the whole point: the artifact path drops it, so
// without this row an off-scope write leaves no trace at all.
func TestCapture_GHChannelWrite_RecordsEveryAttempt(t *testing.T) {
	stores, info := newCaptureStores(t, true)
	client := NewLocal(stores, info)
	ctx := context.Background()

	edit := ghwrite.Observation{Method: "PATCH", Path: "/repos/octo/repo/pulls/7", Status: 200}
	client.RecordGHChannelWrite(ctx, edit)
	client.RecordGHChannelWrite(ctx, edit)
	client.RecordGHChannelWrite(ctx, ghwrite.Observation{
		Method: "PATCH", Path: "/repos/other/repo/pulls/1", Status: 404,
	})

	acts := listExternalActions(t, stores)
	if len(acts) != 3 {
		t.Fatalf("want 3 rows (every attempt is its own event), got %d: %+v", len(acts), acts)
	}
	var refused domain.ExternalAction
	for _, a := range acts {
		if a.Provider != domain.ArtifactProviderGitHub || a.Credential != domain.CredentialGitHubApp {
			t.Errorf("gh channel row mismatch: %+v", a)
		}
		if a.ConversationID != info.RunID {
			t.Errorf("conversation = %q, want %q", a.ConversationID, info.RunID)
		}
		if strings.Contains(a.DetailJSON, "404") {
			refused = a
		} else if a.Action != domain.ActionPREdited || a.Target != "octo/repo#7" {
			t.Errorf("accepted PR edit = %+v, want pr_edited on octo/repo#7", a)
		}
	}
	// The refusal keeps the opaque row: nothing was edited, so nothing may claim
	// the verb — and the target stays repo-level, since a 404 is also how an
	// off-scope repo is masked and #1 there may not exist at all.
	if refused.Action != domain.ActionGHChannelWrite || refused.Target != "other/repo" {
		t.Errorf("refused write = %+v, want the fallback action on the repo the path names", refused)
	}
	if !strings.Contains(refused.DetailJSON, `"method":"PATCH"`) ||
		!strings.Contains(refused.DetailJSON, "/repos/other/repo/pulls/1") ||
		!strings.Contains(refused.DetailJSON, `"attempted":"pr_edited"`) {
		t.Errorf("detail_json = %q, want method + path + status + the attempted act", refused.DetailJSON)
	}
}

// TestGHChannelWriteAction_ReviewSubmit pins the raw review post: the same
// review_submitted verb the governed path records, on the PR the request path
// named, keyed on the review id the response returned — plus the raw method and
// path, so a reader can tell this review did not come through the verbs.
func TestGHChannelWriteAction_ReviewSubmit(t *testing.T) {
	const reviewURL = "https://github.com/acme/widgets/pull/841#pullrequestreview-999"
	act := GHChannelWriteAction(ghwrite.Observation{
		Method:     "POST",
		Path:       "/repos/acme/widgets/pulls/841/reviews",
		Status:     200,
		ExternalID: "999",
		URL:        reviewURL,
	}, domain.CredentialGitHubApp)
	if act.Action != domain.ActionReviewSubmitted || act.Target != "acme/widgets#841" ||
		act.ExternalID != "999" || act.URL != reviewURL {
		t.Errorf("review submit row = %+v, want review_submitted on acme/widgets#841", act)
	}
	if !strings.Contains(act.DetailJSON, `"path":"/repos/acme/widgets/pulls/841/reviews"`) {
		t.Errorf("detail_json = %q, want the raw path", act.DetailJSON)
	}
	// A refused submit is an attempt, exactly like a refused merge.
	refused := GHChannelWriteAction(ghwrite.Observation{
		Method: "POST", Path: "/repos/acme/widgets/pulls/841/reviews", Status: 422,
	}, domain.CredentialGitHubApp)
	if refused.Action != domain.ActionGHChannelWrite ||
		!strings.Contains(refused.DetailJSON, `"attempted":"review_submitted"`) {
		t.Errorf("refused submit = %+v, want the attempt row naming the act", refused)
	}
}

// TestGHChannelWriteAction_UnclassifiedKeepsTheFallback pins what survives of
// the old opaque row: a shape the table doesn't model records that a write
// happened and refuses to name it, and a path naming no repo doesn't invent one.
func TestGHChannelWriteAction_UnclassifiedKeepsTheFallback(t *testing.T) {
	act := GHChannelWriteAction(ghwrite.Observation{Method: "POST", Path: "/user/repos", Status: 201}, domain.CredentialGitHubApp)
	if act.Action != domain.ActionGHChannelWrite || act.Target != "/user/repos" {
		t.Errorf("org-level write = %+v, want the fallback keyed on the path", act)
	}
	if strings.Contains(act.DetailJSON, "attempted") {
		t.Errorf("detail_json = %q, want no attempted act on an unclassified shape", act.DetailJSON)
	}
	// A classified shape's 2xx is the other half of the same decision.
	del := GHChannelWriteAction(ghwrite.Observation{
		Method: "DELETE", Path: "/repos/octo/repo/issues/comments/5", Status: 204,
	}, domain.CredentialGitHubApp)
	if del.Action != domain.ActionCommentDeleted || del.Target != "octo/repo" || del.ExternalID != "5" {
		t.Errorf("comment delete = %+v, want comment_deleted on octo/repo naming comment 5", del)
	}
}

// TestCapture_ExecVerbsRideTheAPIProxyNotTheGHChannel pins why the row above
// can never be written twice for one act. A verb write and a raw write are
// audited by different observers — the verb self-reports, the injector observes
// — so double-counting is only possible if a verb's traffic could transit the
// injector. It cannot: the exec client is built against the credential proxy's
// address and holds no other, and the gh channel's listener exists only for the
// jail's own `gh`. Structural, and pinned here so it stays that way.
func TestCapture_ExecVerbsRideTheAPIProxyNotTheGHChannel(t *testing.T) {
	var apiProxyHits, ghChannelHits atomic.Int32
	apiProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		apiProxyHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":777}`)
	}))
	defer apiProxy.Close()
	ghChannel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ghChannelHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":778}`)
	}))
	defer ghChannel.Close()

	client, _, err := proxyRepoClient(
		&ProxyCredentials{GitHubAPIURL: apiProxy.URL, GitHubAPIToken: "run-placeholder"}, "octo", "repo")
	if err != nil {
		t.Fatalf("proxyRepoClient: %v", err)
	}
	if _, err := client.ReplyToComment(context.Background(), "octo", "repo", 1, 555, "thanks"); err != nil {
		t.Fatalf("ReplyToComment: %v", err)
	}
	if got := apiProxyHits.Load(); got != 1 {
		t.Errorf("credential proxy saw %d requests, want the verb's 1", got)
	}
	if got := ghChannelHits.Load(); got != 0 {
		t.Errorf("the gh channel saw %d verb requests, want 0 — a verb write must never reach the injector", got)
	}
}

// TestCapture_GHChannelReply_IsIndistinguishableFromTheVerbRow is the incident's
// acceptance: the raw `gh api .../comments/{id}/replies` an agent reached for
// must leave the same audit row as `exec gh pr comment-reply`, down to the
// external id, the discussion deep link, and the in_reply_to detail — and it
// must mint the same entity touch, which a repo-level target could not.
func TestCapture_GHChannelReply_IsIndistinguishableFromTheVerbRow(t *testing.T) {
	ctx := context.Background()

	// Each half runs against its own store, so the row and the touch below are
	// unambiguously that half's work rather than a leftover of the other's.
	gh := startFakeGitHubComments(t)
	_, verbStores, _, verbClient := newGithubRecordingClientConn(t, gh.URL, true)
	if _, err := verbClient.GithubReplyToComment(ctx, "octo", "repo", 1, 555, "thanks for the catch"); err != nil {
		t.Fatalf("GithubReplyToComment: %v", err)
	}
	verbRows := listExternalActions(t, verbStores)
	if len(verbRows) != 1 {
		t.Fatalf("want 1 verb row, got %d: %+v", len(verbRows), verbRows)
	}

	// The same reply, made by hand through the gh channel: the injector saw the
	// method and path, and read the created reply's id + link off the response.
	conn, stores, info, client := newGithubRecordingClientConn(t, gh.URL, true)
	client.RecordGHChannelWrite(ctx, ghwrite.Observation{
		Method:     "POST",
		Path:       "/repos/octo/repo/pulls/1/comments/555/replies",
		Status:     201,
		ExternalID: "777",
		URL:        "https://github.com/octo/repo/pull/1#discussion_r777",
	})
	rows := listExternalActions(t, stores)
	if len(rows) != 1 {
		t.Fatalf("want exactly 1 row for one request, got %d: %+v", len(rows), rows)
	}

	raw, verb := rows[0], verbRows[0]
	if raw.Action != verb.Action || raw.Provider != verb.Provider || raw.Credential != verb.Credential ||
		raw.Target != verb.Target || raw.ExternalID != verb.ExternalID || raw.URL != verb.URL ||
		raw.DetailJSON != verb.DetailJSON || raw.ConversationID != verb.ConversationID {
		t.Errorf("raw gh reply row\n  %+v\ndiffers from the verb's\n  %+v", raw, verb)
	}
	if raw.Target != "octo/repo#1" || raw.ExternalID != "777" {
		t.Errorf("raw row = %+v, want the PR-shaped target and the reply id", raw)
	}

	// The PR-shaped target is what re-enables the touch; a repo-level one is
	// skipped by the touch rule, which is why this write used to leave none.
	ent, err := stores.Entities.GetBySource(ctx, runmode.LocalDefaultOrgID, "github", "octo/repo#1")
	if err != nil || ent == nil {
		t.Fatalf("GetBySource(github, octo/repo#1): ent=%v err=%v", ent, err)
	}
	var touches int
	if err := conn.QueryRow(
		`SELECT COUNT(*) FROM conversation_memory_entities WHERE conversation_id = ? AND entity_id = ? AND role = 'touched'`,
		info.RunID, ent.ID,
	).Scan(&touches); err != nil {
		t.Fatalf("count touches: %v", err)
	}
	if touches == 0 {
		t.Error("the raw gh reply minted no entity touch")
	}
}

// TestGHChannelWriteAction_GraphQLSemanticRow: a mutation the table knows, that
// the endpoint accepted, earns the same verb its REST twin would. `gh pr
// comment` and a hand-written `gh api …/comments` are one act, and the log has
// to be filterable for it without the reader knowing which pipe was used.
func TestGHChannelWriteAction_GraphQLSemanticRow(t *testing.T) {
	act := GHChannelWriteAction(ghwrite.Observation{
		Method: "POST",
		Path:   "/graphql",
		Status: 200,
		URL:    "https://github.com/octo/repo/pull/1#issuecomment-7",
		GraphQL: &ghwrite.GraphQLFacts{
			Operation: "CommentCreate",
			Fields:    []string{"addComment"},
			Subject:   "PR_kwRow",
		},
	}, domain.CredentialGitHubApp)
	if act.Action != domain.ActionCommentPosted {
		t.Errorf("action = %q, want comment_posted", act.Action)
	}
	// The response located an object the request named only by node id, so the
	// row addresses it the way every other surface does.
	if act.Target != "octo/repo#1" {
		t.Errorf("target = %q, want octo/repo#1 from the response url", act.Target)
	}
	if act.URL != "https://github.com/octo/repo/pull/1#issuecomment-7" {
		t.Errorf("url = %q, want the created comment's link", act.URL)
	}
	// A comment's row must read like the verb's: no provenance, because the
	// question "which channel posted this" is not asked of comments.
	if strings.Contains(act.DetailJSON, "mutation") {
		t.Errorf("detail_json = %q, want no provenance on a comment row", act.DetailJSON)
	}
}

// TestGHChannelWriteAction_GraphQLCarriesProvenanceWhereItIsAsked: a PR create
// and a review submit each have a governed verb path, and whether one came
// through it or through a raw call is the fact the policy work reads this log
// to settle. On this transport that provenance is the mutation's name.
func TestGHChannelWriteAction_GraphQLCarriesProvenanceWhereItIsAsked(t *testing.T) {
	act := GHChannelWriteAction(ghwrite.Observation{
		Method: "POST", Path: "/graphql", Status: 200,
		URL: "https://github.com/octo/repo/pull/9",
		GraphQL: &ghwrite.GraphQLFacts{
			Operation: "PullRequestCreate",
			Fields:    []string{"createPullRequest"},
			Subject:   "R_kwRepo",
		},
	}, domain.CredentialGitHubApp)
	if act.Action != domain.ActionPRCreated || act.Target != "octo/repo#9" || act.ExternalID != "9" {
		t.Errorf("row = %+v, want pr_created on octo/repo#9 keyed by number", act)
	}
	if !strings.Contains(act.DetailJSON, `"mutation":"createPullRequest"`) {
		t.Errorf("detail_json = %q, want the mutation recorded as provenance", act.DetailJSON)
	}
}

// TestGHChannelWriteAction_GraphQLRefusalIsAnAttempt: the endpoint answers a
// refused mutation with 200 and an errors array, so without reading it a merge
// that never happened would be logged as one that did.
func TestGHChannelWriteAction_GraphQLRefusalIsAnAttempt(t *testing.T) {
	act := GHChannelWriteAction(ghwrite.Observation{
		Method: "POST", Path: "/graphql", Status: 200, Errored: true,
		GraphQL: &ghwrite.GraphQLFacts{Fields: []string{"mergePullRequest"}, Subject: "PR_kwRefused"},
	}, domain.CredentialGitHubApp)
	if act.Action != domain.ActionGraphQLWrite {
		t.Errorf("action = %q, want the graphql fallback — a refused merge is not pr_merged", act.Action)
	}
	if !strings.Contains(act.DetailJSON, `"attempted":"pr_merged"`) {
		t.Errorf("detail_json = %q, want the attempted act named", act.DetailJSON)
	}
	if !strings.Contains(act.DetailJSON, `"errored":true`) {
		t.Errorf("detail_json = %q, want the errors array recorded", act.DetailJSON)
	}
	if act.Target != "PR_kwRefused" {
		t.Errorf("target = %q, want the node id the request named", act.Target)
	}
}

// TestGHChannelWriteAction_GraphQLFallback covers what the row says when no act
// could be named: enough to go looking on GitHub, and an honest account of why
// the name is missing.
func TestGHChannelWriteAction_GraphQLFallback(t *testing.T) {
	// The fixture deliberately names a mutation from outside the product's write
	// surface. Coverage sweeps keep widening the table, and one that adopted this
	// fixture's mutation would leave the test passing while asserting nothing —
	// the failure mode is silent, so the guard is choosing a name no sweep will
	// ever reach for.
	t.Run("unknown mutation", func(t *testing.T) {
		act := GHChannelWriteAction(ghwrite.Observation{
			Method: "POST", Path: "/graphql", Status: 200,
			GraphQL: &ghwrite.GraphQLFacts{
				Operation: "Sponsor",
				Fields:    []string{"createSponsorship"},
				Subject:   "U_kwSponsor",
			},
		}, domain.CredentialGitHubApp)
		if act.Action != domain.ActionGraphQLWrite {
			t.Errorf("action = %q, want graphql_write", act.Action)
		}
		if !strings.Contains(act.DetailJSON, `"mutations":["createSponsorship"]`) {
			t.Errorf("detail_json = %q, want the mutation name recorded verbatim", act.DetailJSON)
		}
		if strings.Contains(act.DetailJSON, "attempted") {
			t.Errorf("detail_json = %q, want no attempted act for a shape the table cannot name", act.DetailJSON)
		}
	})

	t.Run("over cap", func(t *testing.T) {
		act := GHChannelWriteAction(ghwrite.Observation{
			Method: "POST", Path: "/graphql", Status: 200,
			GraphQL: &ghwrite.GraphQLFacts{Unreadable: ghwrite.GraphQLOverCap},
		}, domain.CredentialGitHubApp)
		if act.Action != domain.ActionGraphQLWrite {
			t.Errorf("action = %q, want graphql_write", act.Action)
		}
		if !strings.Contains(act.DetailJSON, `"unreadable":"over_cap"`) {
			t.Errorf("detail_json = %q, want the reason recorded", act.DetailJSON)
		}
		// Nothing was readable, so the endpoint path is all the target can be —
		// never an invented repo.
		if act.Target != "/graphql" {
			t.Errorf("target = %q, want the endpoint path", act.Target)
		}
	})

	t.Run("several acts in one request", func(t *testing.T) {
		act := GHChannelWriteAction(ghwrite.Observation{
			Method: "POST", Path: "/graphql", Status: 200,
			GraphQL: &ghwrite.GraphQLFacts{
				Fields:  []string{"closePullRequest", "mergePullRequest"},
				Subject: "PR_kwBoth",
			},
		}, domain.CredentialGitHubApp)
		if act.Action != domain.ActionGraphQLWrite {
			t.Errorf("action = %q, want graphql_write — no one row can carry two acts", act.Action)
		}
		if !strings.Contains(act.DetailJSON, `"mutations":["closePullRequest","mergePullRequest"]`) {
			t.Errorf("detail_json = %q, want both names recorded", act.DetailJSON)
		}
	})
}

// TestCapture_GraphQLWrite_RecordsOneRowPerRequest runs the GraphQL arm through
// the real recording funnel, which is where "exactly one row" is actually
// enforced.
func TestCapture_GraphQLWrite_RecordsOneRowPerRequest(t *testing.T) {
	ctx := context.Background()
	_, stores, _, client := newGithubRecordingClientConn(t, "", true)

	client.RecordGHChannelWrite(ctx, ghwrite.Observation{
		Method: "POST", Path: "/graphql", Status: 200,
		GraphQL: &ghwrite.GraphQLFacts{Fields: []string{"closePullRequest"}, Subject: "PR_kwOne"},
	})

	rows := listExternalActions(t, stores)
	if len(rows) != 1 {
		t.Fatalf("want exactly 1 row for one request, got %d: %+v", len(rows), rows)
	}
	if rows[0].Action != domain.ActionPRClosed || rows[0].Target != "PR_kwOne" {
		t.Errorf("row = %+v, want pr_closed against the node id the request named", rows[0])
	}
	if rows[0].Credential != domain.CredentialGitHubApp {
		t.Errorf("credential = %q, want the org credential the write actually spent", rows[0].Credential)
	}
}

// TestCapture_GraphQLOverCapStillLandsAnAttributedRow is the anti-evasion
// property, asserted where it actually matters: in the store, not at the
// injector.
//
// A body past the buffering cap costs the row its verb — nothing read the
// document, so nothing can name the act. What it must not cost is the row. If
// an oversized payload bought silence, padding a request past the cap would be
// the way to write under the org credential without appearing in the log at
// all, and the size threshold would be the exploit rather than the safeguard.
//
// So the write still lands a row, still attributed to the conversation that
// made it, still carrying the reason it could say no more. That is also why the
// injector logs this case: the row rides a fire-and-forget relay, and this is
// the one outcome whose loss would erase the only trace of a write nobody could
// name.
func TestCapture_GraphQLOverCapStillLandsAnAttributedRow(t *testing.T) {
	ctx := context.Background()
	_, stores, info, client := newGithubRecordingClientConn(t, "", true)

	client.RecordGHChannelWrite(ctx, ghwrite.Observation{
		Method:  "POST",
		Path:    "/graphql",
		Status:  200,
		GraphQL: &ghwrite.GraphQLFacts{Unreadable: ghwrite.GraphQLOverCap},
	})

	rows := listExternalActions(t, stores)
	if len(rows) != 1 {
		t.Fatalf("want exactly 1 row for an over-cap write, got %d: %+v", len(rows), rows)
	}
	row := rows[0]
	if row.Action != domain.ActionGraphQLWrite {
		t.Errorf("action = %q, want graphql_write — the act is unknown, the write is not", row.Action)
	}
	if row.ConversationID != info.RunID {
		t.Errorf("conversation = %q, want the run that made the write %q", row.ConversationID, info.RunID)
	}
	if row.Credential != domain.CredentialGitHubApp {
		t.Errorf("credential = %q, want the org credential it spent", row.Credential)
	}
	if !strings.Contains(row.DetailJSON, `"unreadable":"over_cap"`) {
		t.Errorf("detail_json = %q, want the reason the act went unnamed", row.DetailJSON)
	}

	// A body that stopped arriving is a different fact and keeps its own name,
	// so an operational failure never reads as someone hitting our cap.
	client.RecordGHChannelWrite(ctx, ghwrite.Observation{
		Method:  "POST",
		Path:    "/graphql",
		Status:  200,
		GraphQL: &ghwrite.GraphQLFacts{Unreadable: ghwrite.GraphQLRequestUnread},
	})
	rows = listExternalActions(t, stores)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows after a second write, got %d", len(rows))
	}
	var reasons []string
	for _, r := range rows {
		reasons = append(reasons, r.DetailJSON)
	}
	if strings.Count(strings.Join(reasons, " "), `"unreadable":"request_unread"`) != 1 {
		t.Errorf("rows = %v, want the broken-read reason recorded distinctly", reasons)
	}
}
