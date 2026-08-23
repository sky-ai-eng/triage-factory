package dbtest

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// RepositoryStoreFactory is what a per-backend test file hands to
// RunRepositoryStoreConformance. Returns the wired RepositoryStore impl and the
// orgID to pass to every call. repositories has no FK to other
// tables (it's a configured-list table, not part of the entity
// graph), so no seeder bag is needed.
type RepositoryStoreFactory func(t *testing.T) (store db.RepositoryStore, orgID string)

// RunRepositoryStoreConformance covers the repo-store contract every
// backend impl must hold:
//
//   - Repository.ID is the registry row's id on both dialects, and Slug()
//     renders the display name — so an id is portable between them and a
//     caller cannot mistake one for the other.
//   - The two lookups differ where it matters: Get takes an id and refuses a
//     miss with db.ErrNoSuchRepository (including for a leftover slug, which
//     must be the same answer on both dialects rather than a uuid parse
//     failure on one); GetByRef takes a name and answers a miss with nil.
//   - The id-keyed writers refuse an id no row answers to, so a resolve-then-
//     write caller cannot report success for a write that did nothing.
//   - Upsert + GetByRef round-trip across the full field surface.
//   - Upsert preserves user-configured base_branch on a re-profile
//     (the conflict update list explicitly excludes base_branch).
//   - Upsert preserves clone_status/clone_error/clone_error_kind on
//     a re-profile (same reason).
//   - List returns ordered "owner/repo" entries.
//   - SetConfigured: new entries get skeleton rows, dropped entries
//     are deleted, existing rows are preserved (profile data,
//     base_branch, clone state).
//   - ListConfiguredNames returns just the "owner/repo" names.
//   - CountConfigured returns the row count.
//   - UpdateBaseBranch + UpdateCloneStatusByRef mutate only the targeted
//     fields, and every name-keyed method folds case on owner/repo.
//   - GetOrCreateSystem mints a row for an untracked repository, is
//     idempotent, never duplicates or rewrites an existing (profiled)
//     one, matches case-insensitively, and records an external id.
//   - Every single-row write returns the row it persisted, and that row is
//     the one a point read finds.
func RunRepositoryStoreConformance(t *testing.T, mk RepositoryStoreFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("every_single_row_write_returns_the_stored_row", func(t *testing.T) {
		// The returned-row standard, applied to each converted method in turn.
		// The property is one line — what the write handed back is what a point
		// read finds — and AssertWriteReturnedStoredRow's doc covers what that
		// one line is standing in for (RETURNING semantics, RLS visibility on
		// the update arm, column-list drift).
		s, orgID := mk(t)
		read := func(id string) func() (*domain.Repository, error) {
			return func() (*domain.Repository, error) { return s.Get(ctx, orgID, id) }
		}

		// ProfiledAt is set throughout, and that is not decoration. It is the
		// row's only pointer field, so with it nil the comparison never has to
		// follow one — and a driver that decoded a timestamp differently on the
		// write's RETURNING than on the read's SELECT would sail past every
		// assertion below.
		profiled := time.Now().UTC().Truncate(time.Second)
		created, err := s.Upsert(ctx, orgID, domain.Repository{
			Owner: "Acme", Repo: "Api",
			Description: "v1", ProfileText: "v1",
			CloneURL: "git@github.com:Acme/Api.git", DefaultBranch: "main",
			ExternalID: "1296269", ProfiledAt: &profiled,
		})
		if err != nil {
			t.Fatalf("Upsert (insert arm): %v", err)
		}
		if created.ID == "" {
			t.Fatal("Upsert returned a row with no id — the id is the only handle a caller that has never read this repository can get")
		}
		AssertWriteReturnedStoredRow(t, "Upsert (insert arm)", created, read(created.ID))

		branched, err := s.UpdateBaseBranch(ctx, orgID, created.ID, "develop")
		if err != nil {
			t.Fatalf("UpdateBaseBranch: %v", err)
		}
		if branched.BaseBranch != "develop" {
			t.Errorf("UpdateBaseBranch returned BaseBranch %q, want develop", branched.BaseBranch)
		}
		AssertWriteReturnedStoredRow(t, "UpdateBaseBranch", branched, read(created.ID))

		// The update arm, and the case the standard is really for: this input
		// is wrong about the row in four ways at once — the casing it spells
		// the repository with, the id it does not carry, the base branch it
		// says nothing about, and the row id itself — and the returned row is
		// right about all four.
		reprofiled := profiled.Add(time.Hour)
		updated, err := s.UpsertSystem(ctx, orgID, domain.Repository{
			Owner: "acme", Repo: "api",
			Description: "v2", ProfileText: "v2", DefaultBranch: "main",
			ProfiledAt: &reprofiled,
		})
		if err != nil {
			t.Fatalf("UpsertSystem (update arm): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "UpsertSystem (update arm)", updated, read(created.ID))
		if updated.ID != created.ID || updated.Slug() != "Acme/Api" {
			t.Errorf("update arm returned %s/%s, want the existing row under its stored casing", updated.ID, updated.Slug())
		}
		if updated.ExternalID != "1296269" || updated.BaseBranch != "develop" {
			t.Errorf("returned row echoes the input rather than the row: external_id=%q base_branch=%q",
				updated.ExternalID, updated.BaseBranch)
		}

		stamped, err := s.UpdateCloneStatusByRef(ctx, orgID, domain.RepoRef{Owner: "acme", Repo: "api"}, "failed", "boom", "ssh")
		if err != nil || stamped == nil {
			t.Fatalf("UpdateCloneStatusByRef: got=%v err=%v", stamped, err)
		}
		AssertWriteReturnedStoredRow(t, "UpdateCloneStatusByRef", *stamped, read(created.ID))

		stampedSys, err := s.UpdateCloneStatusByRefSystem(ctx, orgID, domain.RepoRef{Owner: "acme", Repo: "api"}, "ok", "", "")
		if err != nil || stampedSys == nil {
			t.Fatalf("UpdateCloneStatusByRefSystem: got=%v err=%v", stampedSys, err)
		}
		AssertWriteReturnedStoredRow(t, "UpdateCloneStatusByRefSystem", *stampedSys, read(created.ID))
	})

	t.Run("Upsert_then_GetByRef_round_trips", func(t *testing.T) {
		s, orgID := mk(t)
		now := time.Now().UTC().Truncate(time.Second)
		p := domain.Repository{
			Owner: "octo", Repo: "widget",
			Description:   "Widget service",
			HasReadme:     true,
			HasClaudeMd:   true,
			HasAgentsMd:   false,
			ProfileText:   "Service profile body",
			CloneURL:      "git@github.com:octo/widget.git",
			DefaultBranch: "main",
			ProfiledAt:    &now,
		}
		if _, err := s.Upsert(ctx, orgID, p); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		got, err := s.GetByRef(ctx, orgID, repoRef("octo/widget"))
		if err != nil || got == nil {
			t.Fatalf("GetByRef: got=%v err=%v", got, err)
		}
		if got.Slug() != "octo/widget" || got.Owner != "octo" || got.Repo != "widget" {
			t.Errorf("name mismatch: %+v", got)
		}
		// The handle is the row's own id, not the name it renders. Both
		// dialects have to agree on that or the id stops being portable
		// between them: a caller that stored one in local mode and read it
		// back in multi would be holding two different kinds of string.
		if got.ID == "" || got.ID == got.Slug() {
			t.Errorf("ID = %q — want the registry row id, not the slug", got.ID)
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

	t.Run("GetByRef_returns_nil_on_miss", func(t *testing.T) {
		// A name nobody has a row for is an answer, not a fault: it is what
		// `workspace add` reports as "not configured", and what leaves the
		// universal protected-branch set standing. So it stays a nil.
		s, orgID := mk(t)
		got, err := s.GetByRef(ctx, orgID, repoRef("no/such-repo"))
		if err != nil {
			t.Fatalf("GetByRef: %v", err)
		}
		if got != nil {
			t.Errorf("GetByRef on a missing repo should be nil, got %+v", got)
		}
	})

	t.Run("Get_by_id_round_trips_and_a_miss_is_typed", func(t *testing.T) {
		// The other half of the split, and the reason for it. An id is minted
		// by this store and held by every stored reference, so one that stops
		// resolving is a broken invariant rather than an answer — the caller
		// is made to handle it instead of being handed an empty row that reads
		// exactly like "this repository was never configured".
		s, orgID := mk(t)
		if err := s.SetConfigured(ctx, orgID, []string{"octo/widget"}); err != nil {
			t.Fatalf("SetConfigured: %v", err)
		}
		byRef, err := s.GetByRef(ctx, orgID, repoRef("octo/widget"))
		if err != nil || byRef == nil {
			t.Fatalf("GetByRef: got=%v err=%v", byRef, err)
		}
		byID, err := s.Get(ctx, orgID, byRef.ID)
		if err != nil || byID == nil {
			t.Fatalf("Get by id: got=%v err=%v", byID, err)
		}
		if byID.Slug() != "octo/widget" || byID.ID != byRef.ID {
			t.Errorf("Get by id returned %+v, want the same row GetByRef did", byID)
		}
		if sys, err := s.GetSystem(ctx, orgID, byRef.ID); err != nil || sys == nil || sys.ID != byRef.ID {
			t.Errorf("GetSystem by id = (%v, %v), want the same row", sys, err)
		}

		// An id no row answers to. Well-formed, so this is the stale-handle
		// case rather than a malformed one.
		got, err := s.Get(ctx, orgID, unknownRepoID)
		if !errors.Is(err, db.ErrNoSuchRepository) {
			t.Errorf("Get on an unknown id = (%v, %v), want db.ErrNoSuchRepository", got, err)
		}

		// A leftover slug reaching an id-keyed method is the same miss on
		// both dialects — not a uuid parse failure on one and a clean nil on
		// the other.
		if _, err := s.Get(ctx, orgID, "octo/widget"); !errors.Is(err, db.ErrNoSuchRepository) {
			t.Errorf("Get handed a slug = %v, want db.ErrNoSuchRepository", err)
		}
	})

	t.Run("id_keyed_writes_refuse_an_id_no_row_answers_to", func(t *testing.T) {
		// The bug this closes. The PATCH handler resolves its path segment to
		// a row and writes by that row's id; if the row goes away in between,
		// the write must say so rather than affect zero rows while the handler
		// answers "updated".
		s, orgID := mk(t)
		if _, err := s.UpdateBaseBranch(ctx, orgID, unknownRepoID, "develop"); !errors.Is(err, db.ErrNoSuchRepository) {
			t.Errorf("UpdateBaseBranch on an unknown id = %v, want db.ErrNoSuchRepository", err)
		}
	})

	t.Run("Upsert_preserves_base_branch_on_re_profile", func(t *testing.T) {
		// User-configured base_branch is mutated only by
		// UpdateBaseBranch; the upsert's conflict update list omits it
		// so a re-profile that re-runs Upsert can't clobber the
		// setting. Same goes for clone-status fields.
		s, orgID := mk(t)
		if _, err := s.Upsert(ctx, orgID, domain.Repository{
			Owner: "o", Repo: "r",
			Description: "v1", ProfileText: "v1",
			DefaultBranch: "main",
		}); err != nil {
			t.Fatalf("initial Upsert: %v", err)
		}
		if err := setBaseBranch(ctx, s, orgID, "o/r", "develop"); err != nil {
			t.Fatalf("UpdateBaseBranch: %v", err)
		}
		if _, err := s.UpdateCloneStatusByRef(ctx, orgID, domain.RepoRef{Owner: "o", Repo: "r"}, "ok", "", ""); err != nil {
			t.Fatalf("UpdateCloneStatus: %v", err)
		}

		// Re-profile: same id, refreshed description + profile text.
		if _, err := s.Upsert(ctx, orgID, domain.Repository{
			Owner: "o", Repo: "r",
			Description: "v2", ProfileText: "v2",
			DefaultBranch: "main",
		}); err != nil {
			t.Fatalf("re-Upsert: %v", err)
		}

		got, _ := s.GetByRef(ctx, orgID, repoRef("o/r"))
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
			if _, err := s.Upsert(ctx, orgID, domain.Repository{
				ID: id, Owner: owner, Repo: repo,
				DefaultBranch: "main",
			}); err != nil {
				t.Fatalf("Upsert %s: %v", id, err)
			}
		}
		got, total, err := s.List(ctx, orgID, db.ListOpts{Limit: 50})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		ids := make([]string, len(got))
		for i, p := range got {
			ids[i] = p.Slug()
		}
		want := []string{"a/first", "m/middle", "z/last"}
		if !equalStringSlice(ids, want) {
			t.Errorf("List order = %v, want %v", ids, want)
		}
		if total != 3 {
			t.Errorf("total = %d, want 3", total)
		}

		coRows, coTotal, err := s.List(ctx, orgID, db.ListOpts{CountOnly: true})
		if err != nil {
			t.Fatalf("List count-only: %v", err)
		}
		AssertCountOnlyList(t, "repositories.List", len(coRows), coTotal, total)

		// The pages partition that order: a two-row window then a one-row
		// window covers the set exactly once, and each page reports the
		// filtered total rather than its own length.
		first, total, err := s.List(ctx, orgID, db.ListOpts{Limit: 2})
		if err != nil {
			t.Fatalf("List page 1: %v", err)
		}
		second, total2, err := s.List(ctx, orgID, db.ListOpts{Limit: 2, Offset: 2})
		if err != nil {
			t.Fatalf("List page 2: %v", err)
		}
		if len(first) != 2 || len(second) != 1 || total != 3 || total2 != 3 {
			t.Fatalf("pages = %d + %d (totals %d, %d), want 2 + 1 with total 3 on both",
				len(first), len(second), total, total2)
		}
		// Compared by slug, like the whole-set assertion above: ID is the
		// registry handle (TFAC-834), so it says nothing about ORDER, which is
		// what this walk is checking.
		walked := []string{first[0].Slug(), first[1].Slug(), second[0].Slug()}
		if !equalStringSlice(walked, want) {
			t.Errorf("paged walk = %v, want %v", walked, want)
		}

		// The unwindowed read (Limit 0) is the internal callers' escape
		// hatch and must return everything rather than an empty page.
		if all, _, err := s.List(ctx, orgID, db.Unwindowed); err != nil || len(all) != 3 {
			t.Errorf("unwindowed List = %d rows, %v; want all 3", len(all), err)
		}
	})

	t.Run("SetConfigured_adds_and_removes_and_preserves", func(t *testing.T) {
		s, orgID := mk(t)
		// Pre-seed an existing row with profile data + user-configured
		// state so we can assert SetConfigured doesn't clobber it.
		if _, err := s.Upsert(ctx, orgID, domain.Repository{
			Owner: "keep", Repo: "me",
			Description: "kept", ProfileText: "kept body",
			DefaultBranch: "main",
		}); err != nil {
			t.Fatalf("seed kept row: %v", err)
		}
		if err := setBaseBranch(ctx, s, orgID, "keep/me", "develop"); err != nil {
			t.Fatalf("UpdateBaseBranch on kept: %v", err)
		}
		// A row that's going to be dropped.
		if _, err := s.Upsert(ctx, orgID, domain.Repository{
			Owner: "drop", Repo: "me",
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

		kept, _ := s.GetByRef(ctx, orgID, repoRef("keep/me"))
		if kept == nil {
			t.Fatal("keep/me was dropped by SetConfigured")
		}
		if kept.Description != "kept" || kept.ProfileText != "kept body" {
			t.Errorf("SetConfigured clobbered profile data: %+v", kept)
		}
		if kept.BaseBranch != "develop" {
			t.Errorf("SetConfigured clobbered base_branch: got %q, want develop", kept.BaseBranch)
		}

		added, _ := s.GetByRef(ctx, orgID, repoRef("new/one"))
		if added == nil {
			t.Fatal("new/one not added by SetConfigured")
		}
		if added.ProfileText != "" {
			t.Errorf("new skeleton row should have empty ProfileText, got %q", added.ProfileText)
		}

		dropped, _ := s.GetByRef(ctx, orgID, repoRef("drop/me"))
		if dropped != nil {
			t.Errorf("drop/me should have been deleted by SetConfigured, got %+v", dropped)
		}
	})

	t.Run("SetConfigured_resubmission_under_different_casing_keeps_the_row", func(t *testing.T) {
		// SetConfigured is a full-set replace: anything not in the submitted
		// list is deleted. Deciding membership case-SENSITIVELY makes a
		// resubmission under different casing look like "this repo was
		// dropped and a different one added" — the row is deleted and a bare
		// one created in its place, so an AI profile, a user's base branch,
		// the clone outcome and the poller's ETag all disappear because
		// someone capitalized the slug differently. GitHub identifiers are
		// case-insensitive; the two spellings were never two repositories.
		s, orgID := mk(t)
		profiled := time.Now().UTC().Truncate(time.Second)
		if _, err := s.Upsert(ctx, orgID, domain.Repository{
			Owner: "Acme", Repo: "Api",
			Description: "Api service", ProfileText: "accumulated profile",
			CloneURL: "git@github.com:Acme/Api.git", DefaultBranch: "main",
			ExternalID: "1296269", ProfiledAt: &profiled,
			HasReadme: true, HasClaudeMd: true,
		}); err != nil {
			t.Fatalf("seed profiled row: %v", err)
		}
		if err := setBaseBranch(ctx, s, orgID, "Acme/Api", "develop"); err != nil {
			t.Fatalf("UpdateBaseBranch: %v", err)
		}
		if _, err := s.UpdateCloneStatusByRef(ctx, orgID, domain.RepoRef{Owner: "Acme", Repo: "Api"}, "ok", "", ""); err != nil {
			t.Fatalf("UpdateCloneStatus: %v", err)
		}
		etagAt := profiled.Add(time.Hour)
		if err := s.SetPullsPollStateByRefSystem(ctx, orgID, repoRef("Acme/Api"), `"etag-v1"`, etagAt); err != nil {
			t.Fatalf("SetPullsPollStateByRefSystem: %v", err)
		}

		// The same repository, resubmitted lowercased, alongside a genuinely
		// new one — so this exercises the replace path rather than a no-op.
		if err := s.SetConfigured(ctx, orgID, []string{"acme/api", "octo/new"}); err != nil {
			t.Fatalf("SetConfigured: %v", err)
		}

		if n, _ := s.CountConfigured(ctx, orgID); n != 2 {
			t.Fatalf("rows = %d, want 2 — a casing difference is not a drop plus an add", n)
		}
		got, err := s.GetByRef(ctx, orgID, repoRef("acme/api"))
		if err != nil || got == nil {
			t.Fatalf("Get after resubmission: got=%v err=%v", got, err)
		}
		if got.Slug() != "Acme/Api" {
			t.Errorf("slug = %q, want Acme/Api — stored casing is sticky", got.Slug())
		}
		if got.ProfileText != "accumulated profile" || got.Description != "Api service" {
			t.Errorf("profile lost by the resubmission: %+v", got)
		}
		if !got.HasReadme || !got.HasClaudeMd {
			t.Errorf("doc flags lost by the resubmission: readme=%v claude=%v", got.HasReadme, got.HasClaudeMd)
		}
		if got.BaseBranch != "develop" {
			t.Errorf("BaseBranch = %q, want develop — a user setting must survive a re-save", got.BaseBranch)
		}
		if got.CloneURL != "git@github.com:Acme/Api.git" || got.CloneStatus != "ok" {
			t.Errorf("clone state lost by the resubmission: %+v", got)
		}
		if got.ExternalID != "1296269" {
			t.Errorf("ExternalID = %q, want it kept — identity does not survive a delete+create", got.ExternalID)
		}
		if got.ProfiledAt == nil {
			t.Error("ProfiledAt is nil — the repo would re-profile from scratch, ignoring the TTL")
		}
		etag, polledAt, err := s.GetPullsPollStateByRefSystem(ctx, orgID, repoRef("Acme/Api"))
		if err != nil {
			t.Fatalf("GetPullsPollStateByRefSystem: %v", err)
		}
		if etag != `"etag-v1"` || polledAt == nil {
			t.Errorf("poll state = (%q, %v), want it kept — losing it re-lists every open PR", etag, polledAt)
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
		if _, err := s.Upsert(ctx, orgID, domain.Repository{
			Owner: "o", Repo: "r",
			DefaultBranch: "main",
		}); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		if err := setBaseBranch(ctx, s, orgID, "o/r", "feature/abc"); err != nil {
			t.Fatalf("UpdateBaseBranch (set): %v", err)
		}
		if got, _ := s.GetByRef(ctx, orgID, repoRef("o/r")); got.BaseBranch != "feature/abc" {
			t.Errorf("BaseBranch = %q, want feature/abc", got.BaseBranch)
		}
		if err := setBaseBranch(ctx, s, orgID, "o/r", ""); err != nil {
			t.Fatalf("UpdateBaseBranch (clear): %v", err)
		}
		if got, _ := s.GetByRef(ctx, orgID, repoRef("o/r")); got.BaseBranch != "" {
			t.Errorf("BaseBranch should be empty after clear, got %q", got.BaseBranch)
		}
	})

	t.Run("UpdateBaseBranch_matches_case_insensitively", func(t *testing.T) {
		// GitHub owner/repo identifiers are case-insensitive, and the
		// TFAC-559 repoVisible handler gate (TracksRepoViewerScoped)
		// matches case-insensitively too. If UpdateBaseBranch's lookup were
		// case-sensitive, a caller reaching the PATCH endpoint with
		// different casing than what's stored could pass the gate yet
		// silently affect 0 rows here. Stored casing ("Acme/Api") is sticky;
		// the update is issued with different casing ("acme/API") and must
		// still find the row.
		s, orgID := mk(t)
		if _, err := s.Upsert(ctx, orgID, domain.Repository{
			Owner: "Acme", Repo: "Api",
			DefaultBranch: "main",
		}); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		if err := setBaseBranch(ctx, s, orgID, "acme/API", "develop"); err != nil {
			t.Fatalf("UpdateBaseBranch (mismatched case): %v", err)
		}
		got, err := s.GetByRef(ctx, orgID, repoRef("Acme/Api"))
		if err != nil || got == nil {
			t.Fatalf("Get: got=%v err=%v", got, err)
		}
		if got.BaseBranch != "develop" {
			t.Errorf("BaseBranch = %q, want develop (case-insensitive update should have found the row)", got.BaseBranch)
		}
	})

	t.Run("UpdateCloneStatus_records_outcome", func(t *testing.T) {
		s, orgID := mk(t)
		if _, err := s.Upsert(ctx, orgID, domain.Repository{
			Owner: "o", Repo: "r",
			DefaultBranch: "main",
		}); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		// ok path: empty err fields collapse to NULL → empty string on read.
		if _, err := s.UpdateCloneStatusByRef(ctx, orgID, domain.RepoRef{Owner: "o", Repo: "r"}, "ok", "", ""); err != nil {
			t.Fatalf("UpdateCloneStatus ok: %v", err)
		}
		got, _ := s.GetByRef(ctx, orgID, repoRef("o/r"))
		if got.CloneStatus != "ok" || got.CloneError != "" || got.CloneErrorKind != "" {
			t.Errorf("ok status mismatch: %+v", got)
		}
		// failed path: ssh-kind capture for SSH preflight confirms.
		if _, err := s.UpdateCloneStatusByRef(ctx, orgID, domain.RepoRef{Owner: "o", Repo: "r"}, "failed", "permission denied", "ssh"); err != nil {
			t.Fatalf("UpdateCloneStatus failed: %v", err)
		}
		got, _ = s.GetByRef(ctx, orgID, repoRef("o/r"))
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
		if _, err := s.UpdateCloneStatusByRef(ctx, orgID, domain.RepoRef{Owner: "ghost", Repo: "repo"}, "ok", "", ""); err != nil {
			t.Errorf("UpdateCloneStatus on absent repo should be a no-op, got %v", err)
		}
	})

	t.Run("slug_keyed_methods_all_match_case_insensitively", func(t *testing.T) {
		// GitHub identifiers are case-insensitive, so every method that takes
		// a caller-supplied slug has to resolve the same repository — one
		// method disagreeing is a silent miss, not an error, and reads as
		// "that repo isn't configured".
		//
		// The live path is an agent's `tfac exec workspace add owner/repo`,
		// whose argv goes straight to Get; a stale-casing miss there is
		// reported to the agent as not-configured, immediately before a
		// team-tracking gate that WOULD have matched. The rest are pinned
		// alongside it so the family cannot drift apart again.
		s, orgID := mk(t)
		if err := s.SetConfigured(ctx, orgID, []string{"Acme/Api"}); err != nil {
			t.Fatalf("SetConfigured: %v", err)
		}

		got, err := s.GetByRef(ctx, orgID, repoRef("acme/api"))
		if err != nil || got == nil {
			t.Fatalf("Get with different casing: got=%v err=%v", got, err)
		}
		if got.Slug() != "Acme/Api" {
			t.Errorf("GetByRef returned slug %q, want the stored casing Acme/Api", got.Slug())
		}
		if sys, err := s.GetByRefSystem(ctx, orgID, repoRef("ACME/API")); err != nil || sys == nil {
			t.Errorf("GetSystem with different casing: got=%v err=%v", sys, err)
		}

		if _, err := s.UpdateCloneStatusByRef(ctx, orgID, domain.RepoRef{Owner: "acme", Repo: "api"}, "failed", "boom", "ssh"); err != nil {
			t.Fatalf("UpdateCloneStatus: %v", err)
		}
		got, _ = s.GetByRef(ctx, orgID, repoRef("Acme/Api"))
		if got == nil || got.CloneStatus != "failed" || got.CloneErrorKind != "ssh" {
			t.Errorf("clone status after a differently-cased update = %+v, want failed/ssh", got)
		}

		now := time.Now().UTC().Truncate(time.Second)
		if err := s.SetPullsPollStateByRefSystem(ctx, orgID, repoRef("ACME/api"), `"etag-v1"`, now); err != nil {
			t.Fatalf("SetPullsPollStateByRefSystem: %v", err)
		}
		etag, polledAt, err := s.GetPullsPollStateByRefSystem(ctx, orgID, repoRef("acme/API"))
		if err != nil {
			t.Fatalf("GetPullsPollStateByRefSystem: %v", err)
		}
		if etag != `"etag-v1"` || polledAt == nil {
			t.Errorf("poll state through differently-cased slugs = (%q, %v), want the stored pair", etag, polledAt)
		}
	})

	t.Run("every_ref_keyed_method_pins_the_source", func(t *testing.T) {
		// A RepoRef names one repository, and the provider is half of that
		// name — the identity index is (org, source, folded slug), so two
		// providers spelling one slug are two rows. A method that folds the
		// slug but drops the source matches whichever of them the planner
		// reaches first, which is a wrong row rather than a missing one.
		//
		// GitHub is the only provider that issues repositories today, so the
		// symptom is unreachable; the guard is that source is app-validated
		// rather than CHECK-constrained, which makes each method's own
		// normalize call the only thing standing between a typo and a write
		// against a repository nothing resolves. Pinned as a family so they
		// cannot drift apart the way they already did once.
		s, orgID := mk(t)
		if err := s.SetConfigured(ctx, orgID, []string{"octo/widget"}); err != nil {
			t.Fatalf("SetConfigured: %v", err)
		}
		bogus := domain.RepoRef{Source: "gitlob", Owner: "octo", Repo: "widget"}

		if got, err := s.GetByRef(ctx, orgID, bogus); err == nil {
			t.Errorf("GetByRef accepted an unknown source and returned %+v; want an error", got)
		}
		if got, err := s.GetByRefSystem(ctx, orgID, bogus); err == nil {
			t.Errorf("GetByRefSystem accepted an unknown source and returned %+v; want an error", got)
		}
		if _, err := s.UpdateCloneStatusByRef(ctx, orgID, bogus, "failed", "boom", "ssh"); err == nil {
			t.Error("UpdateCloneStatusByRef accepted an unknown source; want an error")
		}
		if _, err := s.UpdateCloneStatusByRefSystem(ctx, orgID, bogus, "failed", "boom", "ssh"); err == nil {
			t.Error("UpdateCloneStatusByRefSystem accepted an unknown source; want an error")
		}
		if err := s.SetPullsPollStateByRefSystem(ctx, orgID, bogus, `"etag"`, time.Now()); err == nil {
			t.Error("SetPullsPollStateByRefSystem accepted an unknown source; want an error")
		}
		if _, _, err := s.GetPullsPollStateByRefSystem(ctx, orgID, bogus); err == nil {
			t.Error("GetPullsPollStateByRefSystem accepted an unknown source; want an error")
		}

		// And none of them touched the GitHub row on the way to refusing.
		got, err := s.GetByRef(ctx, orgID, repoRef("octo/widget"))
		if err != nil || got == nil {
			t.Fatalf("GetByRef after the refusals: got=%v err=%v", got, err)
		}
		if got.CloneStatus == "failed" {
			t.Errorf("clone status = %q — a refused source must not write", got.CloneStatus)
		}
		if etag, _, _ := s.GetPullsPollStateByRefSystem(ctx, orgID, repoRef("octo/widget")); etag != "" {
			t.Errorf("poll state = %q — a refused source must not write", etag)
		}
	})

	t.Run("GetOrCreate_creates_then_is_idempotent", func(t *testing.T) {
		// The core of the primitive: the first call mints the row, every
		// call after it hands back the same one. "Same" is checked by
		// identity (the store surfaces "owner/repo" as the id on both
		// backends) AND by row count, because the failure this guards is a
		// second row for the same repository, which a per-call Get would
		// happily read past.
		s, orgID := mk(t)
		ref := domain.RepoRef{Owner: "octo", Repo: "widget"}

		first, err := s.GetOrCreateSystem(ctx, orgID, ref)
		if err != nil || first == nil {
			t.Fatalf("GetOrCreateSystem (create): got=%v err=%v", first, err)
		}
		if first.Slug() != "octo/widget" || first.Owner != "octo" || first.Repo != "widget" {
			t.Errorf("created row identity = %+v, want octo/widget", first)
		}
		if first.Source != domain.RepoSourceGitHub {
			t.Errorf("created row source = %q, want %q — an unset source means GitHub", first.Source, domain.RepoSourceGitHub)
		}
		if first.ExternalID != "" {
			t.Errorf("created row external id = %q, want empty — nothing fetched one", first.ExternalID)
		}
		if first.ProfileText != "" || first.ProfiledAt != nil {
			t.Errorf("created row should be bare, got %+v", first)
		}

		for i := 0; i < 3; i++ {
			again, err := s.GetOrCreateSystem(ctx, orgID, ref)
			if err != nil || again == nil {
				t.Fatalf("GetOrCreateSystem (repeat %d): got=%v err=%v", i, again, err)
			}
			if again.ID != first.ID {
				t.Errorf("repeat %d returned id %q, want %q", i, again.ID, first.ID)
			}
		}
		if n, err := s.CountConfigured(ctx, orgID); err != nil {
			t.Fatalf("CountConfigured: %v", err)
		} else if n != 1 {
			t.Errorf("rows after 4 get-or-creates = %d, want 1", n)
		}
	})

	t.Run("GetOrCreate_does_not_duplicate_or_rewrite_a_profiled_repo", func(t *testing.T) {
		// The negative space. A repository that has been profiled, given a
		// base branch, and cloned must come back from get-or-create exactly
		// as it stands — one row, same id, every cached column intact.
		s, orgID := mk(t)
		profiled := time.Now().UTC().Truncate(time.Second)
		if _, err := s.Upsert(ctx, orgID, domain.Repository{
			Owner: "octo", Repo: "widget",
			Description: "Widget service", ProfileText: "Service profile body",
			CloneURL: "git@github.com:octo/widget.git", DefaultBranch: "main",
			ProfiledAt: &profiled,
		}); err != nil {
			t.Fatalf("seed profiled row: %v", err)
		}
		if err := setBaseBranch(ctx, s, orgID, "octo/widget", "develop"); err != nil {
			t.Fatalf("UpdateBaseBranch: %v", err)
		}
		if _, err := s.UpdateCloneStatusByRef(ctx, orgID, domain.RepoRef{Owner: "octo", Repo: "widget"}, "ok", "", ""); err != nil {
			t.Fatalf("UpdateCloneStatus: %v", err)
		}

		got, err := s.GetOrCreateSystem(ctx, orgID, domain.RepoRef{Owner: "octo", Repo: "widget"})
		if err != nil || got == nil {
			t.Fatalf("GetOrCreateSystem: got=%v err=%v", got, err)
		}
		if got.Slug() != "octo/widget" {
			t.Errorf("slug = %q, want octo/widget — get-or-create must not re-key an existing row", got.Slug())
		}
		if got.ProfileText != "Service profile body" || got.Description != "Widget service" {
			t.Errorf("profile clobbered: %+v", got)
		}
		if got.BaseBranch != "develop" {
			t.Errorf("BaseBranch = %q, want develop", got.BaseBranch)
		}
		if got.CloneStatus != "ok" {
			t.Errorf("CloneStatus = %q, want ok", got.CloneStatus)
		}
		if got.ProfiledAt == nil {
			t.Error("ProfiledAt is nil — a get-or-create must not un-profile a repo")
		}
		if n, _ := s.CountConfigured(ctx, orgID); n != 1 {
			t.Errorf("rows = %d, want 1 — the profiled repo must not be duplicated", n)
		}
	})

	t.Run("GetOrCreate_matches_case_insensitively", func(t *testing.T) {
		// GitHub identifiers are case-insensitive and the unique index is
		// not, so a caller spelling a tracked repo differently must find the
		// existing row rather than mint a second one for the same repository.
		// Stored casing is sticky, exactly as the tracked-set reconcile
		// leaves it.
		s, orgID := mk(t)
		if err := s.SetConfigured(ctx, orgID, []string{"Acme/Api"}); err != nil {
			t.Fatalf("SetConfigured: %v", err)
		}
		got, err := s.GetOrCreateSystem(ctx, orgID, domain.RepoRef{Owner: "acme", Repo: "API"})
		if err != nil || got == nil {
			t.Fatalf("GetOrCreateSystem (mismatched case): got=%v err=%v", got, err)
		}
		if got.Slug() != "Acme/Api" {
			t.Errorf("slug = %q, want Acme/Api — stored casing is sticky", got.Slug())
		}
		if n, _ := s.CountConfigured(ctx, orgID); n != 1 {
			t.Errorf("rows = %d, want 1 — a casing difference is not a second repository", n)
		}
	})

	t.Run("GetOrCreate_records_the_external_id", func(t *testing.T) {
		// A create carries the id straight onto the new row; a later call
		// that learned one fills a row that has none. An empty id never
		// clears a stored one — a caller without an id has learned nothing.
		s, orgID := mk(t)

		withID, err := s.GetOrCreateSystem(ctx, orgID, domain.RepoRef{
			Owner: "octo", Repo: "created-with-id", ExternalID: "1296269",
		})
		if err != nil || withID == nil {
			t.Fatalf("GetOrCreateSystem (create with id): got=%v err=%v", withID, err)
		}
		if withID.ExternalID != "1296269" {
			t.Errorf("created external id = %q, want 1296269", withID.ExternalID)
		}

		if _, err := s.GetOrCreateSystem(ctx, orgID, domain.RepoRef{Owner: "octo", Repo: "backfilled"}); err != nil {
			t.Fatalf("GetOrCreateSystem (create bare): %v", err)
		}
		filled, err := s.GetOrCreateSystem(ctx, orgID, domain.RepoRef{
			Owner: "octo", Repo: "backfilled", ExternalID: "555",
		})
		if err != nil || filled == nil {
			t.Fatalf("GetOrCreateSystem (fill id): got=%v err=%v", filled, err)
		}
		if filled.ExternalID != "555" {
			t.Errorf("filled external id = %q, want 555", filled.ExternalID)
		}
		// Round-trips through a plain read, not just the return value.
		if got, _ := s.GetByRef(ctx, orgID, repoRef("octo/backfilled")); got == nil || got.ExternalID != "555" {
			t.Errorf("Get after fill = %+v, want external id 555", got)
		}

		kept, err := s.GetOrCreateSystem(ctx, orgID, domain.RepoRef{Owner: "octo", Repo: "backfilled"})
		if err != nil || kept == nil {
			t.Fatalf("GetOrCreateSystem (no id): got=%v err=%v", kept, err)
		}
		if kept.ExternalID != "555" {
			t.Errorf("external id after an id-less call = %q, want 555 — an empty id clears nothing", kept.ExternalID)
		}
	})

	t.Run("FillMissingExternalIDs_only_ever_turns_NULL_into_a_value", func(t *testing.T) {
		// The poller's half of repository identity: it already enumerates each
		// installation's grant every cycle, and that response carries the ids.
		// This is the write, and its whole contract is that it is safe to run
		// on every cycle — it fills what is missing, touches nothing else, and
		// reports how much it filled so the steady state is visibly zero.
		s, orgID := mk(t)
		if err := s.SetConfigured(ctx, orgID, []string{"Acme/Api", "octo/known", "octo/bare"}); err != nil {
			t.Fatalf("SetConfigured: %v", err)
		}
		if _, err := s.Upsert(ctx, orgID, domain.Repository{
			Owner: "octo", Repo: "known",
			ExternalID: "111", DefaultBranch: "main",
		}); err != nil {
			t.Fatalf("seed known id: %v", err)
		}

		filled, err := s.FillMissingExternalIDsSystem(ctx, orgID, []domain.RepoRef{
			// Different casing than stored — the grant's spelling is GitHub's,
			// not necessarily the one tracked.
			{Owner: "acme", Repo: "api", ExternalID: "1296269"},
			// Already has one; a fill must never move it.
			{Owner: "octo", Repo: "known", ExternalID: "999"},
			// No id to record; not a write.
			{Owner: "octo", Repo: "bare"},
			// Not a configured repo; nothing to fill, and nothing created.
			{Owner: "ghost", Repo: "repo", ExternalID: "42"},
		})
		if err != nil {
			t.Fatalf("FillMissingExternalIDsSystem: %v", err)
		}
		if filled != 1 {
			t.Errorf("filled = %d, want 1 — only the NULL one is a write", filled)
		}

		got, _ := s.GetByRef(ctx, orgID, repoRef("Acme/Api"))
		if got == nil || got.ExternalID != "1296269" {
			t.Errorf("case-differing fill = %+v, want external id 1296269", got)
		}
		if got != nil && got.Slug() != "Acme/Api" {
			t.Errorf("fill moved the stored casing to %q; it must only write external_id", got.Slug())
		}
		known, _ := s.GetByRef(ctx, orgID, repoRef("octo/known"))
		if known == nil || known.ExternalID != "111" {
			t.Errorf("known id = %+v, want 111 kept — a fill never overwrites", known)
		}
		bare, _ := s.GetByRef(ctx, orgID, repoRef("octo/bare"))
		if bare == nil || bare.ExternalID != "" {
			t.Errorf("id-less ref wrote %+v, want the row left alone", bare)
		}
		if ghost, _ := s.GetByRef(ctx, orgID, repoRef("ghost/repo")); ghost != nil {
			t.Errorf("fill created a row for an untracked repo: %+v — it writes, never creates", ghost)
		}
		if n, _ := s.CountConfigured(ctx, orgID); n != 3 {
			t.Errorf("rows = %d, want 3 — a fill adds none", n)
		}

		// Steady state: running it again fills nothing.
		filled, err = s.FillMissingExternalIDsSystem(ctx, orgID, []domain.RepoRef{
			{Owner: "Acme", Repo: "Api", ExternalID: "1296269"},
		})
		if err != nil {
			t.Fatalf("re-run: %v", err)
		}
		if filled != 0 {
			t.Errorf("second run filled %d, want 0 — every cycle after the first is a no-op", filled)
		}

		if n, err := s.FillMissingExternalIDsSystem(ctx, orgID, nil); err != nil || n != 0 {
			t.Errorf("empty batch = (%d, %v), want (0, nil)", n, err)
		}
	})

	t.Run("GetOrCreate_refuses_an_unknown_source", func(t *testing.T) {
		// source is app-validated rather than CHECK-constrained, so this
		// function IS the validation. A typo must not reach the row and key
		// a repository under a provider nothing resolves.
		s, orgID := mk(t)
		if _, err := s.GetOrCreateSystem(ctx, orgID, domain.RepoRef{
			Source: "gitlob", Owner: "octo", Repo: "widget",
		}); err == nil {
			t.Error("GetOrCreateSystem accepted an unknown source; want an error")
		}
		if n, _ := s.CountConfigured(ctx, orgID); n != 0 {
			t.Errorf("rows = %d, want 0 — a refused source must not write", n)
		}
	})

	t.Run("Upsert_round_trips_identity_and_refreshes_the_external_id", func(t *testing.T) {
		// The profiler's write path: it learns the id from the same
		// /repos/{owner}/{repo} response it takes the clone URL from, so a
		// re-profile carrying an id refreshes it while one carrying none
		// leaves the stored id alone.
		s, orgID := mk(t)
		if _, err := s.Upsert(ctx, orgID, domain.Repository{
			Owner: "octo", Repo: "widget",
			Source: domain.RepoSourceGitHub, ExternalID: "1296269",
			ProfileText: "v1", DefaultBranch: "main",
		}); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		got, _ := s.GetByRef(ctx, orgID, repoRef("octo/widget"))
		if got == nil || got.Source != domain.RepoSourceGitHub || got.ExternalID != "1296269" {
			t.Fatalf("identity did not round-trip: %+v", got)
		}

		// A re-profile that carries no id keeps the one on the row.
		if _, err := s.Upsert(ctx, orgID, domain.Repository{
			Owner: "octo", Repo: "widget",
			ProfileText: "v2", DefaultBranch: "main",
		}); err != nil {
			t.Fatalf("re-Upsert without id: %v", err)
		}
		got, _ = s.GetByRef(ctx, orgID, repoRef("octo/widget"))
		if got == nil || got.ExternalID != "1296269" {
			t.Errorf("external id after an id-less re-profile = %+v, want 1296269 preserved", got)
		}
		if got.ProfileText != "v2" {
			t.Errorf("profile text = %q, want v2 — the re-profile still lands", got.ProfileText)
		}

		// One that carries a different id takes it: GitHub is authoritative
		// for what the slug resolves to now.
		if _, err := s.Upsert(ctx, orgID, domain.Repository{
			Owner: "octo", Repo: "widget",
			ExternalID: "77", ProfileText: "v3", DefaultBranch: "main",
		}); err != nil {
			t.Fatalf("re-Upsert with a new id: %v", err)
		}
		got, _ = s.GetByRef(ctx, orgID, repoRef("octo/widget"))
		if got == nil || got.ExternalID != "77" {
			t.Errorf("external id after a re-profile that carried one = %+v, want 77", got)
		}
	})

	t.Run("Upsert_with_different_casing_updates_rather_than_duplicating", func(t *testing.T) {
		// Upsert creates as well as updates, so it is a create path too — and
		// the one that would still duplicate if its conflict target were the
		// case-sensitive key rather than the folded identity. The stored
		// casing wins, matching what the tracked-set reconcile does.
		s, orgID := mk(t)
		if _, err := s.Upsert(ctx, orgID, domain.Repository{
			Owner: "Acme", Repo: "Api",
			ProfileText: "v1", DefaultBranch: "main",
		}); err != nil {
			t.Fatalf("seed Upsert: %v", err)
		}
		if err := setBaseBranch(ctx, s, orgID, "Acme/Api", "develop"); err != nil {
			t.Fatalf("UpdateBaseBranch: %v", err)
		}

		if _, err := s.Upsert(ctx, orgID, domain.Repository{
			Owner: "acme", Repo: "api",
			ProfileText: "v2", DefaultBranch: "main", ExternalID: "1296269",
		}); err != nil {
			t.Fatalf("differently-cased Upsert: %v", err)
		}

		if n, _ := s.CountConfigured(ctx, orgID); n != 1 {
			t.Fatalf("rows = %d, want 1 — a differently-cased upsert must not mint a second repository", n)
		}
		got, _ := s.GetByRef(ctx, orgID, repoRef("Acme/Api"))
		if got == nil {
			t.Fatal("expected the original row to survive")
		}
		if got.Slug() != "Acme/Api" || got.Owner != "Acme" || got.Repo != "Api" {
			t.Errorf("stored casing = %+v, want Acme/Api — casing is sticky", got)
		}
		if got.ProfileText != "v2" || got.ExternalID != "1296269" {
			t.Errorf("the upsert did not land on the existing row: %+v", got)
		}
		if got.BaseBranch != "develop" {
			t.Errorf("BaseBranch = %q, want develop — user config survives a re-profile", got.BaseBranch)
		}
	})

	t.Run("Upsert_refuses_an_unknown_source", func(t *testing.T) {
		s, orgID := mk(t)
		if _, err := s.Upsert(ctx, orgID, domain.Repository{
			Owner: "octo", Repo: "widget", Source: "gitlob",
		}); err == nil {
			t.Error("Upsert accepted an unknown source; want an error")
		}
		if n, _ := s.CountConfigured(ctx, orgID); n != 0 {
			t.Errorf("rows = %d, want 0 — a refused source must not write", n)
		}
	})

	t.Run("SetConfigured_creates_rows_with_the_identity_columns", func(t *testing.T) {
		// The tracked-set create path and the get-or-create create path are
		// the same INSERT, so a row minted by tracking is indistinguishable
		// from one minted by get-or-create.
		s, orgID := mk(t)
		if err := s.SetConfigured(ctx, orgID, []string{"octo/widget"}); err != nil {
			t.Fatalf("SetConfigured: %v", err)
		}
		got, err := s.GetByRef(ctx, orgID, repoRef("octo/widget"))
		if err != nil || got == nil {
			t.Fatalf("Get: got=%v err=%v", got, err)
		}
		if got.Source != domain.RepoSourceGitHub {
			t.Errorf("tracked row source = %q, want %q", got.Source, domain.RepoSourceGitHub)
		}
		if got.ExternalID != "" {
			t.Errorf("tracked row external id = %q, want empty — tracking learns no id", got.ExternalID)
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

		etag, polledAt, err := s.GetPullsPollStateByRefSystem(ctx, orgID, repoRef("octo/widget"))
		if err != nil {
			t.Fatalf("GetPullsPollStateByRefSystem (unset): %v", err)
		}
		if etag != "" || polledAt != nil {
			t.Errorf("unset poll state = (%q, %v); want (\"\", nil)", etag, polledAt)
		}

		now := time.Now().UTC().Truncate(time.Second)
		if err := s.SetPullsPollStateByRefSystem(ctx, orgID, repoRef("octo/widget"), `"etag-v1"`, now); err != nil {
			t.Fatalf("SetPullsPollStateByRefSystem: %v", err)
		}
		etag, polledAt, err = s.GetPullsPollStateByRefSystem(ctx, orgID, repoRef("octo/widget"))
		if err != nil {
			t.Fatalf("GetPullsPollStateByRefSystem: %v", err)
		}
		if etag != `"etag-v1"` {
			t.Errorf("etag = %q; want %q", etag, `"etag-v1"`)
		}
		if polledAt == nil || !polledAt.UTC().Equal(now) {
			t.Errorf("polledAt = %v; want %v", polledAt, now)
		}

		later := now.Add(time.Hour)
		if err := s.SetPullsPollStateByRefSystem(ctx, orgID, repoRef("octo/widget"), `"etag-v2"`, later); err != nil {
			t.Fatalf("re-Set: %v", err)
		}
		etag, _, _ = s.GetPullsPollStateByRefSystem(ctx, orgID, repoRef("octo/widget"))
		if etag != `"etag-v2"` {
			t.Errorf("etag after re-Set = %q; want %q", etag, `"etag-v2"`)
		}
	})

	t.Run("PullsPollState_no_op_when_repo_absent", func(t *testing.T) {
		// Configured-repos-only invariant, same as UpdateCloneStatus.
		s, orgID := mk(t)
		if err := s.SetPullsPollStateByRefSystem(ctx, orgID, repoRef("ghost/repo"), `"x"`, time.Now()); err != nil {
			t.Errorf("Set on absent repo should be a no-op, got %v", err)
		}
		etag, polledAt, err := s.GetPullsPollStateByRefSystem(ctx, orgID, repoRef("ghost/repo"))
		if err != nil {
			t.Errorf("Get on absent repo should be (\"\", nil, nil), got err %v", err)
		}
		if etag != "" || polledAt != nil {
			t.Errorf("absent repo poll state = (%q, %v); want empty", etag, polledAt)
		}
	})
}

// unknownRepoID is a well-formed registry id no row carries. Well-formed
// matters: Postgres types the id column as uuid, so an arbitrary string would
// exercise the malformed-handle path rather than the stale-handle one, and
// those are different assertions.
const unknownRepoID = "3f2b1c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"

// repoRef is the suite's edge parser. These cases are written in the
// "owner/repo" a person or an agent would type, because that is the form the
// behaviour under test is about; the ref-keyed store methods take it parsed.
func repoRef(slug string) domain.RepoRef { return domain.RepoRefFromSlug(slug) }

// setBaseBranch is the two-step every caller of the id-keyed writer performs:
// resolve the name to a row, then write by that row's id. The suite goes
// through it so its cases stay written in slugs — the property most of them
// pin is about a repository, not about a handle — while still exercising the
// path the PATCH handler takes. Case folding lives in the resolve half, which
// is what keeps a differently-cased caller finding the row.
func setBaseBranch(ctx context.Context, s db.RepositoryStore, orgID, slug, branch string) error {
	row, err := s.GetByRef(ctx, orgID, repoRef(slug))
	if err != nil {
		return err
	}
	if row == nil {
		return fmt.Errorf("%w: %s", db.ErrNoSuchRepository, slug)
	}
	_, err = s.UpdateBaseBranch(ctx, orgID, row.ID, branch)
	return err
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
