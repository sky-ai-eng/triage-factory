package workspace

import (
	"context"
	"errors"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

func TestListWorkspaces_MissingConversationID(t *testing.T) {
	stores, _ := newTestDB(t)
	if _, err := listWorkspaces(hostFor(stores, "")); !errors.Is(err, errMissingConversationID) {
		t.Errorf("err = %v, want errMissingConversationID", err)
	}
}

func TestListWorkspaces_RunNotFound(t *testing.T) {
	stores, _ := newTestDB(t)
	if _, err := listWorkspaces(hostFor(stores, "missing-run")); !errors.Is(err, errRunNotFound) {
		t.Errorf("err = %v, want errRunNotFound", err)
	}
}

// TestListWorkspaces_GitHubRunAllowed pins the dropped Jira gate (TFAC-498):
// `workspace list` is now run-agnostic and serves a GitHub run too (it used to
// reject one), mirroring `workspace add`.
func TestListWorkspaces_GitHubRunAllowed(t *testing.T) {
	stores, database := newTestDB(t)
	seedGitHubConversation(t, database, "gh-run")
	seedRepository(t, database, "sky", "core", "https://github.com/sky/core.git", "main")

	out, err := listWorkspaces(hostFor(stores, "gh-run"))
	if err != nil {
		t.Fatalf("listWorkspaces on a GitHub run: %v", err)
	}
	// The configured-and-profilable repo surfaces as available.
	found := false
	for _, a := range out.Available {
		if a.Repo == "sky/core" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected sky/core in available, got %+v", out.Available)
	}
}

func TestListWorkspaces_AvailableFiltersOutMaterialized(t *testing.T) {
	stores, database := newTestDB(t)
	seedJiraConversation(t, database, "r1", "SKY-1")
	seedRepository(t, database, "owner", "alpha", "https://x", "main")
	seedRepository(t, database, "owner", "beta", "https://x", "main")
	seedRepository(t, database, "owner", "gamma", "https://x", "main")

	// Materialize one of the three.
	if _, _, err := sqlitestore.New(database.Conn).ConversationWorktrees.Insert(context.Background(), runmode.LocalDefaultOrgID, domain.ConversationWorktree{
		ConversationID: "r1", RepoID: "owner/beta",
		Path: "/tmp/wt/beta", Ref: "@default",
	}); err != nil {
		t.Fatalf("seed materialized: %v", err)
	}

	out, err := listWorkspaces(hostFor(stores, "r1"))
	if err != nil {
		t.Fatalf("listWorkspaces: %v", err)
	}

	// available = configured - materialized.
	availSet := make(map[string]struct{}, len(out.Available))
	for _, a := range out.Available {
		availSet[a.Repo] = struct{}{}
	}
	if _, ok := availSet["owner/beta"]; ok {
		t.Errorf("owner/beta should not appear in available (it's already materialized): %+v", out.Available)
	}
	for _, want := range []string{"owner/alpha", "owner/gamma"} {
		if _, ok := availSet[want]; !ok {
			t.Errorf("expected %q in available, got %+v", want, out.Available)
		}
	}

	// materialized has the one we seeded.
	if len(out.Materialized) != 1 || out.Materialized[0].Repo != "owner/beta" {
		t.Errorf("materialized = %+v, want one entry for owner/beta", out.Materialized)
	}
	if out.Materialized[0].Path != "/tmp/wt/beta" || out.Materialized[0].Ref != "@default" {
		t.Errorf("materialized entry mismatch: %+v", out.Materialized[0])
	}
}

func TestListWorkspaces_NoConfiguredRepos(t *testing.T) {
	stores, database := newTestDB(t)
	seedJiraConversation(t, database, "r1", "SKY-1")

	out, err := listWorkspaces(hostFor(stores, "r1"))
	if err != nil {
		t.Fatalf("listWorkspaces: %v", err)
	}
	if len(out.Available) != 0 {
		t.Errorf("available = %+v, want empty", out.Available)
	}
	if len(out.Materialized) != 0 {
		t.Errorf("materialized = %+v, want empty", out.Materialized)
	}
}

func TestListWorkspaces_ScopedToRun(t *testing.T) {
	// Materialized worktrees from a sibling run must NOT leak into r1's list.
	stores, database := newTestDB(t)
	seedJiraConversation(t, database, "r1", "SKY-1")
	seedJiraConversation(t, database, "r2", "SKY-2")
	seedRepository(t, database, "owner", "shared", "https://x", "main")

	if _, _, err := sqlitestore.New(database.Conn).ConversationWorktrees.Insert(context.Background(), runmode.LocalDefaultOrgID, domain.ConversationWorktree{
		ConversationID: "r2", RepoID: "owner/shared",
		Path: "/tmp/wt/r2/owner/shared", Ref: "@default",
	}); err != nil {
		t.Fatalf("seed r2 materialized: %v", err)
	}

	out, err := listWorkspaces(hostFor(stores, "r1"))
	if err != nil {
		t.Fatalf("listWorkspaces r1: %v", err)
	}
	if len(out.Materialized) != 0 {
		t.Errorf("r1 materialized = %+v, expected empty (r2's row leaked)", out.Materialized)
	}
	// owner/shared should be available for r1 since r1 hasn't materialized it.
	if len(out.Available) != 1 || out.Available[0].Repo != "owner/shared" {
		t.Errorf("r1 available = %+v, want one entry for owner/shared", out.Available)
	}
}

func TestListWorkspaces_AvailableSurfacesDescription(t *testing.T) {
	// Repository rows carry a one-line description from upstream metadata
	// (GitHub's repo description). The agent uses it to disambiguate
	// between configured repos when the ticket text doesn't make the
	// target obvious. profile_text (the LLM-generated full profile) is
	// deliberately NOT exposed — too verbose for a per-call discovery
	// surface.
	stores, database := newTestDB(t)
	seedJiraConversation(t, database, "r1", "SKY-1")

	if _, err := sqlitestore.New(database.Conn).Repos.Upsert(context.Background(), runmode.LocalDefaultOrgID, domain.Repository{
		Owner: "owner", Repo: "alpha",
		Description:   "Core API service",
		ProfileText:   "Long LLM-generated profile text that should NOT appear in workspace list output...",
		CloneURL:      "https://x",
		DefaultBranch: "main",
	}); err != nil {
		t.Fatalf("upsert alpha: %v", err)
	}
	// Skeleton row (configured but profiling hasn't run, no
	// clone_url). MUST be filtered out — `workspace add` rejects
	// no-clone-url profiles, so surfacing them here would lead the
	// agent to options that fail at materialize time.
	if _, err := sqlitestore.New(database.Conn).Repos.Upsert(context.Background(), runmode.LocalDefaultOrgID, domain.Repository{
		Owner: "owner", Repo: "skeleton",
		// CloneURL deliberately empty
		DefaultBranch: "main",
	}); err != nil {
		t.Fatalf("upsert skeleton: %v", err)
	}

	out, err := listWorkspaces(hostFor(stores, "r1"))
	if err != nil {
		t.Fatalf("listWorkspaces: %v", err)
	}

	byRepo := make(map[string]listAvailable, len(out.Available))
	for _, a := range out.Available {
		byRepo[a.Repo] = a
	}
	alpha, ok := byRepo["owner/alpha"]
	if !ok {
		t.Fatalf("owner/alpha missing from available: %+v", out.Available)
	}
	if alpha.Description != "Core API service" {
		t.Errorf("alpha.Description = %q, want %q", alpha.Description, "Core API service")
	}
	if _, found := byRepo["owner/skeleton"]; found {
		t.Errorf("owner/skeleton (no clone_url) should NOT appear in available; got %+v", out.Available)
	}
}
