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

// newRecorderStores opens an in-memory SQLite, migrates it, and seeds one runs
// row the artifacts FK (run_id) can point at — mirroring the pre-push hook's
// own test fixture so the two writers are exercised against the same store.
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
	if _, err := conn.Exec(`INSERT INTO runs (id, trigger_type, creator_user_id, origin) VALUES ('run-1', 'event', NULL, 'interactive')`); err != nil {
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
	rec := gitPushRecorder(stores, recorderInfo(runID))

	rec(context.Background(), gitproxy.PushedRef{
		Repo:    "octo/repo",
		Ref:     "refs/heads/feature/x",
		NewSHA:  "abc123",
		Created: true,
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
	if a.RunID != runID || a.OrgID != runmode.LocalDefaultOrgID || a.TeamID != runmode.LocalDefaultTeamID {
		t.Errorf("identity not stamped: run=%q org=%q team=%q", a.RunID, a.OrgID, a.TeamID)
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
	rec := gitPushRecorder(stores, recorderInfo(runID))

	// A tag isn't a branch; a 3-segment repo path isn't owner/repo. Both make
	// NewBranchArtifact return ok=false, so the recorder skips them silently.
	rec(context.Background(), gitproxy.PushedRef{Repo: "octo/repo", Ref: "refs/tags/v1", NewSHA: "abc", Created: false})
	rec(context.Background(), gitproxy.PushedRef{Repo: "octo/repo/extra", Ref: "refs/heads/main", NewSHA: "abc", Created: false})

	arts, err := stores.Artifacts.ListByRun(context.Background(), runmode.LocalDefaultOrgID, runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(arts) != 0 {
		t.Fatalf("got %d artifacts, want 0 (tag + malformed repo are skipped)", len(arts))
	}
}

// TestGitPushRecorder_DedupConvergesWithHook proves the proxy backstop and the
// pre-push hook converge on one row: recording the same branch twice (as the
// hook then the proxy would for a single push) upserts in place — updating the
// volatile head — rather than minting a second artifact.
func TestGitPushRecorder_DedupConvergesWithHook(t *testing.T) {
	stores, runID := newRecorderStores(t)
	rec := gitPushRecorder(stores, recorderInfo(runID))

	rec(context.Background(), gitproxy.PushedRef{Repo: "octo/repo", Ref: "refs/heads/main", NewSHA: "aaa", Created: true})
	rec(context.Background(), gitproxy.PushedRef{Repo: "octo/repo", Ref: "refs/heads/main", NewSHA: "bbb", Created: false})

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
