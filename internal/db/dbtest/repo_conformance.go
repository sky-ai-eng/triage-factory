package dbtest

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// RepoStoreFactory is what a per-backend test file hands to
// RunRepoStoreConformance. Returns the wired RepoStore impl and the
// orgID to pass to every call. repo_profiles has no FK to other
// tables (it's a configured-list table, not part of the entity
// graph), so no seeder bag is needed.
type RepoStoreFactory func(t *testing.T) (store db.RepoStore, orgID string)

// RunRepoStoreConformance covers the repo-store contract every
// backend impl must hold:
//
//   - Upsert + Get round-trip across the full field surface.
//   - Upsert preserves user-configured base_branch on a re-profile
//     (the conflict update list explicitly excludes base_branch).
//   - Upsert preserves clone_status/clone_error/clone_error_kind on
//     a re-profile (same reason).
//   - List returns ordered "owner/repo" entries.
//   - ListWithContent filters out rows with empty profile_text.
//   - SetConfigured: new entries get skeleton rows, dropped entries
//     are deleted, existing rows are preserved (profile data,
//     base_branch, clone state).
//   - ListConfiguredNames returns just the "owner/repo" IDs.
//   - CountConfigured returns the row count.
//   - UpdateBaseBranch + UpdateCloneStatus mutate only the targeted
//     fields.
func RunRepoStoreConformance(t *testing.T, mk RepoStoreFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("Upsert_then_Get_round_trips", func(t *testing.T) {
		s, orgID := mk(t)
		now := time.Now().UTC().Truncate(time.Second)
		p := domain.RepoProfile{
			ID: "octo/widget", Owner: "octo", Repo: "widget",
			Description:   "Widget service",
			HasReadme:     true,
			HasClaudeMd:   true,
			HasAgentsMd:   false,
			ProfileText:   "Service profile body",
			CloneURL:      "git@github.com:octo/widget.git",
			DefaultBranch: "main",
			ProfiledAt:    &now,
		}
		if err := s.Upsert(ctx, orgID, p); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		got, err := s.Get(ctx, orgID, "octo/widget")
		if err != nil || got == nil {
			t.Fatalf("Get: got=%v err=%v", got, err)
		}
		if got.ID != "octo/widget" || got.Owner != "octo" || got.Repo != "widget" {
			t.Errorf("id mismatch: %+v", got)
		}
		if got.Description != "Widget service" || got.ProfileText != "Service profile body" {
			t.Errorf("body mismatch: %+v", got)
		}
		if !got.HasReadme || !got.HasClaudeMd || got.HasAgentsMd {
			t.Errorf("flags mismatch: readme=%v claude=%v agents=%v",
				got.HasReadme, got.HasClaudeMd, got.HasAgentsMd)
		}
		if got.DefaultBranch != "main" || got.CloneURL != "git@github.com:octo/widget.git" {
			t.Errorf("clone metadata mismatch: %+v", got)
		}
		if got.ProfiledAt == nil {
			t.Errorf("ProfiledAt should be non-nil after Upsert")
		}
	})

	t.Run("Get_returns_nil_on_miss", func(t *testing.T) {
		s, orgID := mk(t)
		got, err := s.Get(ctx, orgID, "no/such-repo")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got != nil {
			t.Errorf("Get on missing repo should be nil, got %+v", got)
		}
	})

	t.Run("Upsert_preserves_base_branch_on_re_profile", func(t *testing.T) {
		// User-configured base_branch is mutated only by
		// UpdateBaseBranch; the upsert's conflict update list omits it
		// so a re-profile that re-runs Upsert can't clobber the
		// setting. Same goes for clone-status fields.
		s, orgID := mk(t)
		if err := s.Upsert(ctx, orgID, domain.RepoProfile{
			ID: "o/r", Owner: "o", Repo: "r",
			Description: "v1", ProfileText: "v1",
			DefaultBranch: "main",
		}); err != nil {
			t.Fatalf("initial Upsert: %v", err)
		}
		if err := s.UpdateBaseBranch(ctx, orgID, "o/r", "develop"); err != nil {
			t.Fatalf("UpdateBaseBranch: %v", err)
		}
		if err := s.UpdateCloneStatus(ctx, orgID, "o", "r", "ok", "", ""); err != nil {
			t.Fatalf("UpdateCloneStatus: %v", err)
		}

		// Re-profile: same id, refreshed description + profile text.
		if err := s.Upsert(ctx, orgID, domain.RepoProfile{
			ID: "o/r", Owner: "o", Repo: "r",
			Description: "v2", ProfileText: "v2",
			DefaultBranch: "main",
		}); err != nil {
			t.Fatalf("re-Upsert: %v", err)
		}

		got, _ := s.Get(ctx, orgID, "o/r")
		if got == nil {
			t.Fatal("expected row after re-Upsert")
		}
		if got.Description != "v2" || got.ProfileText != "v2" {
			t.Errorf("profile metadata should refresh: desc=%q text=%q", got.Description, got.ProfileText)
		}
		if got.BaseBranch != "develop" {
			t.Errorf("BaseBranch should survive re-Upsert: got %q, want %q", got.BaseBranch, "develop")
		}
		if got.CloneStatus != "ok" {
			t.Errorf("CloneStatus should survive re-Upsert: got %q, want %q", got.CloneStatus, "ok")
		}
	})

	t.Run("List_returns_sorted_entries", func(t *testing.T) {
		s, orgID := mk(t)
		for _, id := range []string{"z/last", "a/first", "m/middle"} {
			owner, repo := id[:1], id[2:]
			if err := s.Upsert(ctx, orgID, domain.RepoProfile{
				ID: id, Owner: owner, Repo: repo,
				DefaultBranch: "main",
			}); err != nil {
				t.Fatalf("Upsert %s: %v", id, err)
			}
		}
		got, err := s.List(ctx, orgID)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		ids := make([]string, len(got))
		for i, p := range got {
			ids[i] = p.ID
		}
		want := []string{"a/first", "m/middle", "z/last"}
		if !equalStringSlice(ids, want) {
			t.Errorf("List order = %v, want %v", ids, want)
		}
	})

	t.Run("ListWithContent_filters_empty_profile_text", func(t *testing.T) {
		s, orgID := mk(t)
		if err := s.Upsert(ctx, orgID, domain.RepoProfile{
			ID: "o/with", Owner: "o", Repo: "with",
			ProfileText: "real content", DefaultBranch: "main",
		}); err != nil {
			t.Fatalf("Upsert with: %v", err)
		}
		if err := s.Upsert(ctx, orgID, domain.RepoProfile{
			ID: "o/empty", Owner: "o", Repo: "empty",
			ProfileText: "", DefaultBranch: "main",
		}); err != nil {
			t.Fatalf("Upsert empty: %v", err)
		}

		got, err := s.ListWithContent(ctx, orgID)
		if err != nil {
			t.Fatalf("ListWithContent: %v", err)
		}
		if len(got) != 1 || got[0].ID != "o/with" {
			t.Errorf("ListWithContent should only return rows with profile_text, got %v", projectIDs(got))
		}
	})

	t.Run("SetConfigured_adds_and_removes_and_preserves", func(t *testing.T) {
		s, orgID := mk(t)
		// Pre-seed an existing row with profile data + user-configured
		// state so we can assert SetConfigured doesn't clobber it.
		if err := s.Upsert(ctx, orgID, domain.RepoProfile{
			ID: "keep/me", Owner: "keep", Repo: "me",
			Description: "kept", ProfileText: "kept body",
			DefaultBranch: "main",
		}); err != nil {
			t.Fatalf("seed kept row: %v", err)
		}
		if err := s.UpdateBaseBranch(ctx, orgID, "keep/me", "develop"); err != nil {
			t.Fatalf("UpdateBaseBranch on kept: %v", err)
		}
		// A row that's going to be dropped.
		if err := s.Upsert(ctx, orgID, domain.RepoProfile{
			ID: "drop/me", Owner: "drop", Repo: "me",
			DefaultBranch: "main",
		}); err != nil {
			t.Fatalf("seed drop row: %v", err)
		}

		// SetConfigured to {keep/me, new/one}. drop/me should be
		// deleted; keep/me's profile + base_branch should survive;
		// new/one should be added as a skeleton.
		if err := s.SetConfigured(ctx, orgID, []string{"keep/me", "new/one"}); err != nil {
			t.Fatalf("SetConfigured: %v", err)
		}

		names, _ := s.ListConfiguredNames(ctx, orgID)
		sort.Strings(names)
		want := []string{"keep/me", "new/one"}
		if !equalStringSlice(names, want) {
			t.Errorf("configured names = %v, want %v", names, want)
		}

		kept, _ := s.Get(ctx, orgID, "keep/me")
		if kept == nil {
			t.Fatal("keep/me was dropped by SetConfigured")
		}
		if kept.Description != "kept" || kept.ProfileText != "kept body" {
			t.Errorf("SetConfigured clobbered profile data: %+v", kept)
		}
		if kept.BaseBranch != "develop" {
			t.Errorf("SetConfigured clobbered base_branch: got %q, want develop", kept.BaseBranch)
		}

		added, _ := s.Get(ctx, orgID, "new/one")
		if added == nil {
			t.Fatal("new/one not added by SetConfigured")
		}
		if added.ProfileText != "" {
			t.Errorf("new skeleton row should have empty ProfileText, got %q", added.ProfileText)
		}

		dropped, _ := s.Get(ctx, orgID, "drop/me")
		if dropped != nil {
			t.Errorf("drop/me should have been deleted by SetConfigured, got %+v", dropped)
		}
	})

	t.Run("SetConfigured_skips_malformed_entries", func(t *testing.T) {
		// "no-slash" and "trailing/" produce empty halves which the
		// impl silently skips. The valid entry alongside still lands.
		s, orgID := mk(t)
		if err := s.SetConfigured(ctx, orgID, []string{"no-slash", "good/repo"}); err != nil {
			t.Fatalf("SetConfigured: %v", err)
		}
		names, _ := s.ListConfiguredNames(ctx, orgID)
		if len(names) != 1 || names[0] != "good/repo" {
			t.Errorf("malformed entry should be skipped, got %v", names)
		}
	})

	t.Run("CountConfigured_reflects_table_size", func(t *testing.T) {
		s, orgID := mk(t)
		n, err := s.CountConfigured(ctx, orgID)
		if err != nil {
			t.Fatalf("CountConfigured initial: %v", err)
		}
		if n != 0 {
			t.Errorf("initial CountConfigured = %d, want 0", n)
		}
		if err := s.SetConfigured(ctx, orgID, []string{"a/b", "c/d", "e/f"}); err != nil {
			t.Fatalf("SetConfigured: %v", err)
		}
		n, _ = s.CountConfigured(ctx, orgID)
		if n != 3 {
			t.Errorf("CountConfigured after SetConfigured(3) = %d, want 3", n)
		}
	})

	t.Run("UpdateBaseBranch_empty_clears_to_null", func(t *testing.T) {
		// Empty string stores NULL — falls back to default_branch at
		// use site. The pre-D2 impl used nullIfEmpty(); the new
		// store does the same via NULLIF / nullIfEmpty.
		s, orgID := mk(t)
		if err := s.Upsert(ctx, orgID, domain.RepoProfile{
			ID: "o/r", Owner: "o", Repo: "r",
			DefaultBranch: "main",
		}); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		if err := s.UpdateBaseBranch(ctx, orgID, "o/r", "feature/abc"); err != nil {
			t.Fatalf("UpdateBaseBranch (set): %v", err)
		}
		if got, _ := s.Get(ctx, orgID, "o/r"); got.BaseBranch != "feature/abc" {
			t.Errorf("BaseBranch = %q, want feature/abc", got.BaseBranch)
		}
		if err := s.UpdateBaseBranch(ctx, orgID, "o/r", ""); err != nil {
			t.Fatalf("UpdateBaseBranch (clear): %v", err)
		}
		if got, _ := s.Get(ctx, orgID, "o/r"); got.BaseBranch != "" {
			t.Errorf("BaseBranch should be empty after clear, got %q", got.BaseBranch)
		}
	})

	t.Run("UpdateBaseBranch_matches_case_insensitively", func(t *testing.T) {
		// GitHub owner/repo identifiers are case-insensitive, and the
		// TFAC-559 repoAccessAllowed handler gate (TracksRepoViewerScoped)
		// matches case-insensitively too. If UpdateBaseBranch's lookup were
		// case-sensitive, a caller reaching the PATCH endpoint with
		// different casing than what's stored could pass the gate yet
		// silently affect 0 rows here. Stored casing ("Acme/Api") is sticky;
		// the update is issued with different casing ("acme/API") and must
		// still find the row.
		s, orgID := mk(t)
		if err := s.Upsert(ctx, orgID, domain.RepoProfile{
			ID: "Acme/Api", Owner: "Acme", Repo: "Api",
			DefaultBranch: "main",
		}); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		if err := s.UpdateBaseBranch(ctx, orgID, "acme/API", "develop"); err != nil {
			t.Fatalf("UpdateBaseBranch (mismatched case): %v", err)
		}
		got, err := s.Get(ctx, orgID, "Acme/Api")
		if err != nil || got == nil {
			t.Fatalf("Get: got=%v err=%v", got, err)
		}
		if got.BaseBranch != "develop" {
			t.Errorf("BaseBranch = %q, want develop (case-insensitive update should have found the row)", got.BaseBranch)
		}
	})

	t.Run("UpdateCloneStatus_records_outcome", func(t *testing.T) {
		s, orgID := mk(t)
		if err := s.Upsert(ctx, orgID, domain.RepoProfile{
			ID: "o/r", Owner: "o", Repo: "r",
			DefaultBranch: "main",
		}); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		// ok path: empty err fields collapse to NULL → empty string on read.
		if err := s.UpdateCloneStatus(ctx, orgID, "o", "r", "ok", "", ""); err != nil {
			t.Fatalf("UpdateCloneStatus ok: %v", err)
		}
		got, _ := s.Get(ctx, orgID, "o/r")
		if got.CloneStatus != "ok" || got.CloneError != "" || got.CloneErrorKind != "" {
			t.Errorf("ok status mismatch: %+v", got)
		}
		// failed path: ssh-kind capture for SSH preflight confirms.
		if err := s.UpdateCloneStatus(ctx, orgID, "o", "r", "failed", "permission denied", "ssh"); err != nil {
			t.Fatalf("UpdateCloneStatus failed: %v", err)
		}
		got, _ = s.Get(ctx, orgID, "o/r")
		if got.CloneStatus != "failed" || got.CloneError != "permission denied" || got.CloneErrorKind != "ssh" {
			t.Errorf("failed status mismatch: %+v", got)
		}
	})

	t.Run("UpdateCloneStatus_no_op_when_repo_absent", func(t *testing.T) {
		// Configured-repos invariant: clone hooks fire after repo
		// selection, but if a repo gets dropped between the hook
		// firing and the UPDATE landing, the row may be gone. The
		// raw SQL UPDATE silently affects 0 rows — store contract
		// mirrors that, no error.
		s, orgID := mk(t)
		if err := s.UpdateCloneStatus(ctx, orgID, "ghost", "repo", "ok", "", ""); err != nil {
			t.Errorf("UpdateCloneStatus on absent repo should be a no-op, got %v", err)
		}
	})

	t.Run("PullsPollState_round_trips", func(t *testing.T) {
		// The GitHub poller stores a per-repo ETag + last-poll time for
		// conditional open-PR discovery. Unset reads as ("", nil);
		// a Set persists both; a re-Set overwrites.
		s, orgID := mk(t)
		if err := s.SetConfigured(ctx, orgID, []string{"octo/widget"}); err != nil {
			t.Fatalf("SetConfigured: %v", err)
		}

		etag, polledAt, err := s.GetPullsPollStateSystem(ctx, orgID, "octo/widget")
		if err != nil {
			t.Fatalf("GetPullsPollStateSystem (unset): %v", err)
		}
		if etag != "" || polledAt != nil {
			t.Errorf("unset poll state = (%q, %v); want (\"\", nil)", etag, polledAt)
		}

		now := time.Now().UTC().Truncate(time.Second)
		if err := s.SetPullsPollStateSystem(ctx, orgID, "octo/widget", `"etag-v1"`, now); err != nil {
			t.Fatalf("SetPullsPollStateSystem: %v", err)
		}
		etag, polledAt, err = s.GetPullsPollStateSystem(ctx, orgID, "octo/widget")
		if err != nil {
			t.Fatalf("GetPullsPollStateSystem: %v", err)
		}
		if etag != `"etag-v1"` {
			t.Errorf("etag = %q; want %q", etag, `"etag-v1"`)
		}
		if polledAt == nil || !polledAt.UTC().Equal(now) {
			t.Errorf("polledAt = %v; want %v", polledAt, now)
		}

		later := now.Add(time.Hour)
		if err := s.SetPullsPollStateSystem(ctx, orgID, "octo/widget", `"etag-v2"`, later); err != nil {
			t.Fatalf("re-Set: %v", err)
		}
		etag, _, _ = s.GetPullsPollStateSystem(ctx, orgID, "octo/widget")
		if etag != `"etag-v2"` {
			t.Errorf("etag after re-Set = %q; want %q", etag, `"etag-v2"`)
		}
	})

	t.Run("PullsPollState_no_op_when_repo_absent", func(t *testing.T) {
		// Configured-repos-only invariant, same as UpdateCloneStatus.
		s, orgID := mk(t)
		if err := s.SetPullsPollStateSystem(ctx, orgID, "ghost/repo", `"x"`, time.Now()); err != nil {
			t.Errorf("Set on absent repo should be a no-op, got %v", err)
		}
		etag, polledAt, err := s.GetPullsPollStateSystem(ctx, orgID, "ghost/repo")
		if err != nil {
			t.Errorf("Get on absent repo should be (\"\", nil, nil), got err %v", err)
		}
		if etag != "" || polledAt != nil {
			t.Errorf("absent repo poll state = (%q, %v); want empty", etag, polledAt)
		}
	})
}

func projectIDs(profiles []domain.RepoProfile) []string {
	out := make([]string, len(profiles))
	for i, p := range profiles {
		out[i] = p.ID
	}
	return out
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
