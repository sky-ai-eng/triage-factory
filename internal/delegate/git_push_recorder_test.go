package delegate

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/gitproxy"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	_ "modernc.org/sqlite"
)

// newRecorderStores opens an in-memory SQLite, migrates it, and seeds one
// conversations row the artifacts FK (conversation_id) can point at —
// mirroring the pre-push hook's own test fixture so the two writers are
// exercised against the same store.
func newRecorderStores(t *testing.T) (db.Stores, string) {
	t.Helper()
	conn, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := db.Migrate(conn, "sqlite3"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := conn.Exec(`INSERT INTO conversations (id, trigger_type, creator_user_id, origin, status) VALUES ('run-1', 'event', NULL, 'interactive', 'running')`); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return sqlitestore.New(conn), "run-1"
}

// recorderInfo is the run identity a delegated run carries — event-triggered,
// so UpsertArtifact takes the admin-pool branch (the common case for the proxy:
// an auto-delegated run has no user identity).
func recorderInfo(runID string) agenthost.RunInfo {
	return agenthost.RunInfo{
		OrgID:            runmode.LocalDefaultOrgID,
		UserID:           runmode.LocalDefaultUserID,
		TeamID:           runmode.LocalDefaultTeamID,
		RunID:            runID,
		IsEventTriggered: true,
	}
}

func TestGitPushRecorder_RecordsBranch(t *testing.T) {
	stores, runID := newRecorderStores(t)
	info := recorderInfo(runID)
	rec := gitPushRecorder(agenthost.NewLocal(stores, info), info)

	rec(context.Background(), gitproxy.PushedRef{
		Repo:    "octo/repo",
		Ref:     "refs/heads/feature/x",
		NewSHA:  "abc123",
		Created: true,
		Status:  200,
	})

	arts, err := stores.Artifacts.ListByRun(context.Background(), runmode.LocalDefaultOrgID, runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("got %d artifacts, want 1", len(arts))
	}
	a := arts[0]
	if a.Provider != domain.ArtifactProviderGit || a.Kind != domain.ArtifactKindBranch {
		t.Errorf("provider/kind = %q/%q, want git/branch", a.Provider, a.Kind)
	}
	if a.Target != "octo/repo" || a.ExternalID != "refs/heads/feature/x" {
		t.Errorf("target/external_id = %q/%q, want octo/repo, refs/heads/feature/x", a.Target, a.ExternalID)
	}
	if want := "git:branch:octo/repo:refs/heads/feature/x"; a.DedupKey != want {
		t.Errorf("dedup_key = %q, want %q", a.DedupKey, want)
	}
	if a.ConversationID != runID || a.OrgID != runmode.LocalDefaultOrgID || a.TeamID != runmode.LocalDefaultTeamID {
		t.Errorf("identity not stamped: run=%q org=%q team=%q", a.ConversationID, a.OrgID, a.TeamID)
	}
	var d struct {
		SHA string `json:"sha"`
		New bool   `json:"new"`
	}
	if err := json.Unmarshal([]byte(a.DetailsJSON), &d); err != nil {
		t.Fatalf("details_json %q: %v", a.DetailsJSON, err)
	}
	if d.SHA != "abc123" || !d.New {
		t.Errorf("details = %+v, want sha=abc123 new=true", d)
	}
}

func TestGitPushRecorder_SkipsNonBranchAndMalformed(t *testing.T) {
	stores, runID := newRecorderStores(t)
	info := recorderInfo(runID)
	rec := gitPushRecorder(agenthost.NewLocal(stores, info), info)

	// A tag isn't a branch; a 3-segment repo path isn't owner/repo. Both make
	// NewBranchArtifact return ok=false, so the recorder skips them silently.
	rec(context.Background(), gitproxy.PushedRef{Repo: "octo/repo", Ref: "refs/tags/v1", NewSHA: "abc", Created: false, Status: 200})
	rec(context.Background(), gitproxy.PushedRef{Repo: "octo/repo/extra", Ref: "refs/heads/main", NewSHA: "abc", Created: false, Status: 200})

	arts, err := stores.Artifacts.ListByRun(context.Background(), runmode.LocalDefaultOrgID, runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(arts) != 0 {
		t.Fatalf("got %d artifacts, want 0 (tag + malformed repo are skipped)", len(arts))
	}
}

// TestGitPushRecorder_RefusedPushRecordsAuditOnly pins the outcome split: a
// push the upstream refused (non-2xx) must NOT mint a branch artifact —
// artifacts track only work that landed — but must leave a branch_push_failed
// external-action audit row carrying the attempt's sha and HTTP status, so the
// audit log never omits an attempted external write. A subsequent successful
// push of the same ref then lands the artifact and the branch_pushed row as
// usual (the failure row, having no dedup key, can't swallow it).
func TestGitPushRecorder_RefusedPushRecordsAuditOnly(t *testing.T) {
	stores, runID := newRecorderStores(t)
	info := recorderInfo(runID)
	rec := gitPushRecorder(agenthost.NewLocal(stores, info), info)
	ctx := context.Background()

	rec(ctx, gitproxy.PushedRef{Repo: "octo/repo", Ref: "refs/heads/feature/x", NewSHA: "abc123", Created: true, Status: 403})

	arts, err := stores.Artifacts.ListByRun(ctx, runmode.LocalDefaultOrgID, runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(arts) != 0 {
		t.Fatalf("got %d artifacts for a refused push, want 0", len(arts))
	}
	acts, _, err := stores.ExternalActions.ListByOrgSystem(ctx, runmode.LocalDefaultOrgID, domain.ExternalActionListOpts{Action: domain.ActionBranchPushFailed})
	if err != nil {
		t.Fatalf("ListByOrgSystem: %v", err)
	}
	if len(acts) != 1 {
		t.Fatalf("got %d branch_push_failed actions, want 1", len(acts))
	}
	a := acts[0]
	if a.Target != "octo/repo" || a.ExternalID != "refs/heads/feature/x" || a.ConversationID != runID {
		t.Errorf("failure row = target %q external_id %q run %q; want octo/repo, refs/heads/feature/x, %s", a.Target, a.ExternalID, a.ConversationID, runID)
	}
	var d struct {
		SHA        string `json:"sha"`
		New        bool   `json:"new"`
		HTTPStatus int    `json:"http_status"`
	}
	if err := json.Unmarshal([]byte(a.DetailJSON), &d); err != nil {
		t.Fatalf("detail_json %q: %v", a.DetailJSON, err)
	}
	if d.SHA != "abc123" || !d.New || d.HTTPStatus != 403 {
		t.Errorf("detail = %+v, want sha=abc123 new=true http_status=403", d)
	}

	// The retry that lands: same (run, ref, sha), now a 2xx — the artifact and
	// the success audit row both appear despite the earlier failure.
	rec(ctx, gitproxy.PushedRef{Repo: "octo/repo", Ref: "refs/heads/feature/x", NewSHA: "abc123", Created: true, Status: 200})
	arts, err = stores.Artifacts.ListByRun(ctx, runmode.LocalDefaultOrgID, runID)
	if err != nil {
		t.Fatalf("ListByRun after retry: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("got %d artifacts after the successful retry, want 1", len(arts))
	}
	pushed, _, err := stores.ExternalActions.ListByOrgSystem(ctx, runmode.LocalDefaultOrgID, domain.ExternalActionListOpts{Action: domain.ActionBranchPushed})
	if err != nil {
		t.Fatalf("ListByOrgSystem (branch_pushed): %v", err)
	}
	if len(pushed) != 1 {
		t.Fatalf("got %d branch_pushed actions after the retry, want 1 (the failure row must not swallow it)", len(pushed))
	}
}

// TestGitPushRecorder_DedupConvergesWithHook proves the proxy backstop and the
// pre-push hook converge on one row: recording the same branch twice (as the
// hook then the proxy would for a single push) upserts in place — updating the
// volatile head — rather than minting a second artifact.
func TestGitPushRecorder_DedupConvergesWithHook(t *testing.T) {
	stores, runID := newRecorderStores(t)
	info := recorderInfo(runID)
	rec := gitPushRecorder(agenthost.NewLocal(stores, info), info)

	rec(context.Background(), gitproxy.PushedRef{Repo: "octo/repo", Ref: "refs/heads/main", NewSHA: "aaa", Created: true, Status: 200})
	rec(context.Background(), gitproxy.PushedRef{Repo: "octo/repo", Ref: "refs/heads/main", NewSHA: "bbb", Created: false, Status: 200})

	arts, err := stores.Artifacts.ListByRun(context.Background(), runmode.LocalDefaultOrgID, runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("got %d artifacts, want 1 (same dedup_key must upsert)", len(arts))
	}
	var d struct {
		SHA string `json:"sha"`
	}
	_ = json.Unmarshal([]byte(arts[0].DetailsJSON), &d)
	if d.SHA != "bbb" {
		t.Errorf("details sha after re-record = %q, want bbb", d.SHA)
	}
}
