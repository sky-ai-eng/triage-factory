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

// The TFAC-623 touch-capture tests prove the durable (run_id, entity_id,
// role='touched') rows land for both halves of the touched-entity rule: the
// write funnel (every org-credential write) and the addressed reads. The
// LocalClient is the one seam the multi daemon dispatches through, so proving
// it here covers both the sandbox and local-mode paths.

// touchRole reads run_memory_entities.role for (runID, entityID) directly — the
// store interface exposes no role-returning read. "" when no row exists.
func touchRole(t *testing.T, conn *sql.DB, runID, entityID string) string {
	t.Helper()
	var role string
	switch err := conn.QueryRow(
		`SELECT role FROM run_memory_entities WHERE conversation_id = ? AND entity_id = ?`, runID, entityID,
	).Scan(&role); err {
	case nil:
		return role
	case sql.ErrNoRows:
		return ""
	default:
		t.Fatalf("read run_memory_entities role: %v", err)
		return ""
	}
}

// startFakeGitHubGetPR answers the PR read with a minimal PRView body, or a 500
// when fail is set (to exercise the failed-read-touches-nothing path).
func startFakeGitHubGetPR(t *testing.T, fail bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
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

// TestFunnel_GithubWrite_RecordsEntityTouch pins Part 1: a standalone GitHub
// comment write (which flows through RecordExternalWrite) persists a durable
// touch row on the PR entity it addressed, across both write paths.
func TestFunnel_GithubWrite_RecordsEntityTouch(t *testing.T) {
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

			ent, err := stores.Entities.GetBySource(context.Background(), runmode.LocalDefaultOrgID, "github", "octo/repo#1")
			if err != nil || ent == nil {
				t.Fatalf("touched entity missing: ent=%v err=%v", ent, err)
			}
			if ent.Kind != "pr" {
				t.Errorf("entity kind = %q, want pr", ent.Kind)
			}
			if role := touchRole(t, conn, info.RunID, ent.ID); role != domain.MemoryRoleTouched {
				t.Errorf("touch role = %q, want %q", role, domain.MemoryRoleTouched)
			}
		})
	}
}

// TestFunnel_TouchRecordingFailure_DoesNotFailWrite proves the touch is
// best-effort: with run_memory_entities dropped, the record fails but the
// already-applied comment write still succeeds.
func TestFunnel_TouchRecordingFailure_DoesNotFailWrite(t *testing.T) {
	gh := startFakeGitHubComments(t)
	conn, _, _, client := newGithubRecordingClientConn(t, gh.URL, true)
	if _, err := conn.Exec(`DROP TABLE run_memory_entities`); err != nil {
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

	ent, err := stores.Entities.GetBySource(ctx, runmode.LocalDefaultOrgID, "github", "octo/repo#5")
	if err != nil || ent == nil {
		t.Fatalf("read did not resolve-or-create the PR entity: ent=%v err=%v", ent, err)
	}
	if ent.Kind != "pr" {
		t.Errorf("entity kind = %q, want pr", ent.Kind)
	}
	if ent.SnapshotJSON != "" {
		t.Errorf("expected a snapshot-less stub, got snapshot %q", ent.SnapshotJSON)
	}
	if role := touchRole(t, conn, info.RunID, ent.ID); role != domain.MemoryRoleTouched {
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

	ent, err := stores.Entities.GetBySource(ctx, runmode.LocalDefaultOrgID, "github", "octo/repo#5")
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
		_, _ = io.WriteString(w, `{"key":"SKY-123","fields":{"summary":"hi"}}`)
	}))
	t.Cleanup(jira.Close)
	conn, stores, info := newJiraRecordingStoresConn(t, jira.URL, true)
	client := NewLocal(stores, info)
	ctx := context.Background()

	if issue, err := client.JiraGetIssue(ctx, "SKY-123"); err != nil || issue == nil {
		t.Fatalf("JiraGetIssue: issue=%v err=%v", issue, err)
	}

	ent, err := stores.Entities.GetBySource(ctx, runmode.LocalDefaultOrgID, "jira", "SKY-123")
	if err != nil || ent == nil {
		t.Fatalf("read did not resolve-or-create the issue entity: ent=%v err=%v", ent, err)
	}
	if ent.Kind != "issue" {
		t.Errorf("entity kind = %q, want issue", ent.Kind)
	}
	if role := touchRole(t, conn, info.RunID, ent.ID); role != domain.MemoryRoleTouched {
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
	ent, err := stores.Entities.GetBySource(ctx, runmode.LocalDefaultOrgID, "jira", "SKY-123")
	if err != nil {
		t.Fatalf("GetBySource: %v", err)
	}
	if ent != nil {
		t.Errorf("a failed read must create no entity, got %+v", ent)
	}
}

// TestTouch_ReadThenWrite_StaysTouched pins the precedence: a read touch
// followed by a write to the SAME entity stays role='touched' (both writers use
// MemoryRoleTouched; write-side role upgrades are a sibling ticket's concern).
func TestTouch_ReadThenWrite_StaysTouched(t *testing.T) {
	gh := startFakeGitHubGetPR(t, false)
	conn, stores, info, client := newGithubRecordingClientConn(t, gh.URL, true)
	ctx := context.Background()

	if _, err := client.GithubGetPR(ctx, "octo", "repo", 5, false); err != nil {
		t.Fatalf("GithubGetPR: %v", err)
	}
	if _, err := client.GithubAddComment(ctx, "octo", "repo", 5, "a note"); err != nil {
		t.Fatalf("GithubAddComment: %v", err)
	}

	ent, err := stores.Entities.GetBySource(ctx, runmode.LocalDefaultOrgID, "github", "octo/repo#5")
	if err != nil || ent == nil {
		t.Fatalf("entity missing: ent=%v err=%v", ent, err)
	}
	if role := touchRole(t, conn, info.RunID, ent.ID); role != domain.MemoryRoleTouched {
		t.Errorf("touch role = %q, want %q (a read touch + a write to the same entity stays touched)", role, domain.MemoryRoleTouched)
	}
}
