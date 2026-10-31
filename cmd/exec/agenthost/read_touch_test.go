package agenthost

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// These tests prove the durable conversation_memory_entities rows land for both
// halves of the rule: the write funnel (every org-credential write) records
// produced for a write that names the object it changed, and the addressed reads
// record touched. The LocalClient is the one seam the multi daemon dispatches
// through, so proving it here covers both the sandbox and local-mode paths.

// touchRole reads conversation_memory_entities.role for (conversationID, entityID) directly — the
// store interface exposes no role-returning read. "" when no row exists.
func touchRole(t *testing.T, conn *sql.DB, conversationID, entityID string) string {
	t.Helper()
	var role string
	switch err := conn.QueryRow(
		`SELECT role FROM conversation_memory_entities WHERE conversation_id = ? AND entity_id = ?`, conversationID, entityID,
	).Scan(&role); err {
	case nil:
		return role
	case sql.ErrNoRows:
		return ""
	default:
		t.Fatalf("read conversation_memory_entities role: %v", err)
		return ""
	}
}

// startFakeGitHubGetPR answers the PR read, and the identity reads beside it,
// with a minimal PRView body (answerPRRead), or a 500 when fail is set (to
// exercise the failed-read-touches-nothing path).
func startFakeGitHubGetPR(t *testing.T, fail bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if answerPRRead(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// A POST is the comment write (the precedence test); anything else is a read.
		if r.Method == http.MethodPost {
			_, _ = io.WriteString(w, `{"id":777,"html_url":"https://github.com/octo/repo/pull/5#issuecomment-777"}`)
			return
		}
		_, _ = io.WriteString(w, `{"number":5,"title":"hi","head":{"sha":"abc"}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestFunnel_GithubWrite_RecordsProduced: a GitHub comment write (which flows
// through RecordExternalWrite) names the comment it posted, so it persists a
// durable produced row on the PR entity the comment is on, across both write
// paths.
func TestFunnel_GithubWrite_RecordsProduced(t *testing.T) {
	for _, eventTriggered := range []bool{true, false} {
		name := "manual"
		if eventTriggered {
			name = "event-triggered"
		}
		t.Run(name, func(t *testing.T) {
			gh := startFakeGitHubComments(t)
			conn, stores, info, client := newGithubRecordingClientConn(t, gh.URL, eventTriggered)

			if _, err := client.GithubAddComment(context.Background(), "octo", "repo", 1, "lgtm"); err != nil {
				t.Fatalf("GithubAddComment: %v", err)
			}

			ent, err := stores.Entities.GetBySource(context.Background(), runmode.LocalDefaultOrgID, "github", "https://github.com", "octo/repo#1")
			if err != nil || ent == nil {
				t.Fatalf("produced entity missing: ent=%v err=%v", ent, err)
			}
			if ent.Kind != "pr" {
				t.Errorf("entity kind = %q, want pr", ent.Kind)
			}
			if role := touchRole(t, conn, info.ConversationID, ent.ID); role != domain.MemoryRoleProduced {
				t.Errorf("link role = %q, want %q", role, domain.MemoryRoleProduced)
			}
		})
	}
}

// TestFunnel_TouchRecordingFailure_DoesNotFailWrite proves the touch is
// best-effort: with conversation_memory_entities dropped, the record fails but the
// already-applied comment write still succeeds.
func TestFunnel_TouchRecordingFailure_DoesNotFailWrite(t *testing.T) {
	gh := startFakeGitHubComments(t)
	conn, _, _, client := newGithubRecordingClientConn(t, gh.URL, true)
	if _, err := conn.Exec(`DROP TABLE conversation_memory_entities`); err != nil {
		t.Fatalf("drop table: %v", err)
	}

	if _, err := client.GithubAddComment(context.Background(), "octo", "repo", 1, "lgtm"); err != nil {
		t.Fatalf("comment write must not fail when the touch record fails: %v", err)
	}
}

// TestReadTouch_GithubGetPR_TouchesPR pins the addressed-read half: a PR read
// mints the PR stub (when absent) and records a durable touch on it.
func TestReadTouch_GithubGetPR_TouchesPR(t *testing.T) {
	gh := startFakeGitHubGetPR(t, false)
	conn, stores, info, client := newGithubRecordingClientConn(t, gh.URL, true)
	ctx := context.Background()

	if pr, err := client.GithubGetPR(ctx, "octo", "repo", 5, false); err != nil || pr == nil {
		t.Fatalf("GithubGetPR: pr=%v err=%v", pr, err)
	}

	ent, err := stores.Entities.GetBySource(ctx, runmode.LocalDefaultOrgID, "github", "https://github.com", "octo/repo#5")
	if err != nil || ent == nil {
		t.Fatalf("read did not resolve-or-create the PR entity: ent=%v err=%v", ent, err)
	}
	if ent.Kind != "pr" {
		t.Errorf("entity kind = %q, want pr", ent.Kind)
	}
	if ent.SnapshotJSON != "" {
		t.Errorf("expected a snapshot-less stub, got snapshot %q", ent.SnapshotJSON)
	}
	if role := touchRole(t, conn, info.ConversationID, ent.ID); role != domain.MemoryRoleTouched {
		t.Errorf("touch role = %q, want %q", role, domain.MemoryRoleTouched)
	}
}

// TestReadTouch_GithubGetPR_FailedRead_NoTouch proves a failed read touches
// nothing — no entity, no touch row.
func TestReadTouch_GithubGetPR_FailedRead_NoTouch(t *testing.T) {
	gh := startFakeGitHubGetPR(t, true)
	_, stores, _, client := newGithubRecordingClientConn(t, gh.URL, true)
	ctx := context.Background()

	if _, err := client.GithubGetPR(ctx, "octo", "repo", 5, false); err == nil {
		t.Fatal("expected the PR read to fail against the 500 backend")
	}

	ent, err := stores.Entities.GetBySource(ctx, runmode.LocalDefaultOrgID, "github", "https://github.com", "octo/repo#5")
	if err != nil {
		t.Fatalf("GetBySource: %v", err)
	}
	if ent != nil {
		t.Errorf("a failed read must create no entity, got %+v", ent)
	}
}

// TestReadTouch_JiraGetIssue_TouchesIssue mirrors the GitHub read case for the
// addressed Jira read.
func TestReadTouch_JiraGetIssue_TouchesIssue(t *testing.T) {
	jira := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"10123","key":"SKY-123","fields":{"summary":"hi","project":{"id":"10000","key":"SKY","name":"Sky"}}}`)
	}))
	t.Cleanup(jira.Close)
	conn, stores, info := newJiraRecordingStoresConn(t, jira.URL, true)
	client := NewLocal(stores, info)
	ctx := context.Background()

	if issue, err := client.JiraGetIssue(ctx, "SKY-123"); err != nil || issue == nil {
		t.Fatalf("JiraGetIssue: issue=%v err=%v", issue, err)
	}

	ent, err := stores.Entities.GetBySource(ctx, runmode.LocalDefaultOrgID, "jira", jira.URL, "SKY-123")
	if err != nil || ent == nil {
		t.Fatalf("read did not resolve-or-create the issue entity: ent=%v err=%v", ent, err)
	}
	if ent.Kind != "issue" || ent.ExternalID != "10123" {
		t.Errorf("entity = %+v, want an issue carrying the id the response named", ent)
	}
	if p, err := stores.JiraProjects.GetSystem(ctx, runmode.LocalDefaultOrgID, ent.JiraProjectID); err != nil || p == nil || p.ExternalID != "10000" || p.Key != "SKY" || p.Name != "Sky" {
		t.Errorf("entity's project = %+v err=%v, want the project the response named", p, err)
	}
	if role := touchRole(t, conn, info.ConversationID, ent.ID); role != domain.MemoryRoleTouched {
		t.Errorf("touch role = %q, want %q", role, domain.MemoryRoleTouched)
	}
}

// TestReadTouch_JiraGetIssue_FailedRead_NoTouch proves a failed Jira read
// touches nothing.
func TestReadTouch_JiraGetIssue_FailedRead_NoTouch(t *testing.T) {
	// 404 (not 500) so the idempotent-GET retry policy doesn't back off — the
	// read still fails, which is all this asserts.
	jira := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(jira.Close)
	_, stores, info := newJiraRecordingStoresConn(t, jira.URL, true)
	client := NewLocal(stores, info)
	ctx := context.Background()

	if _, err := client.JiraGetIssue(ctx, "SKY-123"); err == nil {
		t.Fatal("expected the Jira read to fail against the 404 backend")
	}
	ent, err := stores.Entities.GetBySource(ctx, runmode.LocalDefaultOrgID, "jira", jira.URL, "SKY-123")
	if err != nil {
		t.Fatalf("GetBySource: %v", err)
	}
	if ent != nil {
		t.Errorf("a failed read must create no entity, got %+v", ent)
	}
}

// TestTouch_ReadThenWrite_EndsProduced pins the precedence: a read touch
// followed by a write to the SAME entity ends role='produced', the higher of
// the two roles.
func TestTouch_ReadThenWrite_EndsProduced(t *testing.T) {
	gh := startFakeGitHubGetPR(t, false)
	conn, stores, info, client := newGithubRecordingClientConn(t, gh.URL, true)
	ctx := context.Background()

	if _, err := client.GithubGetPR(ctx, "octo", "repo", 5, false); err != nil {
		t.Fatalf("GithubGetPR: %v", err)
	}
	if _, err := client.GithubAddComment(ctx, "octo", "repo", 5, "a note"); err != nil {
		t.Fatalf("GithubAddComment: %v", err)
	}

	ent, err := stores.Entities.GetBySource(ctx, runmode.LocalDefaultOrgID, "github", "https://github.com", "octo/repo#5")
	if err != nil || ent == nil {
		t.Fatalf("entity missing: ent=%v err=%v", ent, err)
	}
	if role := touchRole(t, conn, info.ConversationID, ent.ID); role != domain.MemoryRoleProduced {
		t.Errorf("link role = %q, want %q (a read touch then a write to the same entity ends produced)", role, domain.MemoryRoleProduced)
	}
}

// TestFunnel_JiraTransition_RecordsProduced: a Jira transition leaves the
// issue's entity produced as soon as the write lands, with no end-of-run pass,
// and a read of another issue in the same run leaves that one touched.
func TestFunnel_JiraTransition_RecordsProduced(t *testing.T) {
	jira := startFakeJira(t)
	conn, stores, info := newJiraRecordingStoresConn(t, jira.URL, true)
	client := NewLocal(stores, info)
	ctx := context.Background()

	if err := client.JiraTransitionTo(ctx, "SKY-1", "Done"); err != nil {
		t.Fatalf("JiraTransitionTo: %v", err)
	}
	if issue, err := client.JiraGetIssue(ctx, "SKY-2"); err != nil || issue == nil {
		t.Fatalf("JiraGetIssue: issue=%v err=%v", issue, err)
	}

	for key, want := range map[string]string{"SKY-1": domain.MemoryRoleProduced, "SKY-2": domain.MemoryRoleTouched} {
		ent, err := stores.Entities.GetBySource(ctx, runmode.LocalDefaultOrgID, "jira", jira.URL, key)
		if err != nil || ent == nil {
			t.Fatalf("%s: entity missing: ent=%v err=%v", key, ent, err)
		}
		if role := touchRole(t, conn, info.ConversationID, ent.ID); role != want {
			t.Errorf("%s link role = %q, want %q", key, role, want)
		}
	}
}
