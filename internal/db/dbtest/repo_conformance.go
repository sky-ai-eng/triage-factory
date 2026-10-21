package dbtest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// RepositoryStoreFactory is what a per-backend test file hands to
// RunRepositoryStoreConformance. Returns the wired RepositoryStore impl, the
// orgID to pass to every call, and a RepositorySeeder for the tracking door
// the suite stages repositories through.
type RepositoryStoreFactory func(t *testing.T) (store db.RepositoryStore, orgID string, seed RepositorySeeder)

// RepositorySeeder is the bag of fixtures the suite cannot build through
// RepositoryStore alone: the tracked-set store and the teams to track with, for
// the cases about which registry rows the tracked reads return.
type RepositorySeeder struct {
	// Tracking is the tracked-set store wired against the same backend.
	Tracking db.TeamGitHubReposStore

	// TeamID is a team in orgID whose tracked set the suite writes.
	TeamID string

	// Team inserts a second team in orgID and returns its id. Two teams
	// tracking one repository is how the suite stages two creators of the
	// same row.
	Team func(t *testing.T, slug string) string
}

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
//   - The tracked reads (ListTrackedSystem, ListTrackedByTeamSystem,
//     ListTeamScoped) leave out a registry row no team tracks, which List
//     and ListSystem keep.
//   - UpdateBaseBranch + UpdateCloneStatusByRef mutate only the targeted
//     fields, and every name-keyed method folds case on owner/repo.
//   - The by-name door mints one bare identity row per repository, however
//     often and by however many teams it is tracked, matches an existing row
//     case-insensitively with sticky casing, and never rewrites what the
//     row already holds (profile, base branch, clone state, poll cursor).
//   - A registration by provider id finds the row carrying the id before
//     any row holding the name, writes the id onto a bare row holding the
//     name, creates a row carrying it otherwise, and refuses a name held
//     under another id.
//   - Every single-row write returns the row it persisted, and that row is
//     the one a point read finds.
//   - The GitHub host is part of a repository's identity: the same slug on
//     two hosts is two rows, every ref-keyed and listing method answers only
//     for the host it is given and refuses an empty one with
//     db.ErrRepoHostRequired, and a provider id is unique per host.
func RunRepositoryStoreConformance(t *testing.T, mk RepositoryStoreFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("every_single_row_write_returns_the_stored_row", func(t *testing.T) {
		// The returned-row standard, applied to each converted method in turn.
		// The property is one line — what the write handed back is what a point
		// read finds — and AssertWriteReturnedStoredRow's doc covers what that
		// one line is standing in for (RETURNING semantics, RLS visibility on
		// the update arm, column-list drift).
		s, orgID, _ := mk(t)
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
			Host:  TestGitHubHost,
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
			Host:  TestGitHubHost,
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

		stamped, err := s.UpdateCloneStatusByRef(ctx, orgID, domain.RepoRef{Host: TestGitHubHost, Owner: "acme", Repo: "api"}, "failed", "boom", "ssh")
		if err != nil || stamped == nil {
			t.Fatalf("UpdateCloneStatusByRef: got=%v err=%v", stamped, err)
		}
		AssertWriteReturnedStoredRow(t, "UpdateCloneStatusByRef", *stamped, read(created.ID))

		stampedSys, err := s.UpdateCloneStatusByRefSystem(ctx, orgID, domain.RepoRef{Host: TestGitHubHost, Owner: "acme", Repo: "api"}, "ok", "", "")
		if err != nil || stampedSys == nil {
			t.Fatalf("UpdateCloneStatusByRefSystem: got=%v err=%v", stampedSys, err)
		}
		AssertWriteReturnedStoredRow(t, "UpdateCloneStatusByRefSystem", *stampedSys, read(created.ID))
	})

	t.Run("Upsert_then_GetByRef_round_trips", func(t *testing.T) {
		s, orgID, _ := mk(t)
		now := time.Now().UTC().Truncate(time.Second)
		p := domain.Repository{
			Host:  TestGitHubHost,
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
		s, orgID, _ := mk(t)
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
		s, orgID, seed := mk(t)
		trackRepos(t, s, seed, orgID, seed.TeamID, "octo/widget")
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
		s, orgID, _ := mk(t)
		if _, err := s.UpdateBaseBranch(ctx, orgID, unknownRepoID, "develop"); !errors.Is(err, db.ErrNoSuchRepository) {
			t.Errorf("UpdateBaseBranch on an unknown id = %v, want db.ErrNoSuchRepository", err)
		}
	})

	t.Run("Upsert_preserves_base_branch_on_re_profile", func(t *testing.T) {
		// User-configured base_branch is mutated only by
		// UpdateBaseBranch; the upsert's conflict update list omits it
		// so a re-profile that re-runs Upsert can't clobber the
		// setting. Same goes for clone-status fields.
		s, orgID, _ := mk(t)
		if _, err := s.Upsert(ctx, orgID, domain.Repository{
			Host:  TestGitHubHost,
			Owner: "o", Repo: "r",
			Description: "v1", ProfileText: "v1",
			DefaultBranch: "main",
		}); err != nil {
			t.Fatalf("initial Upsert: %v", err)
		}
		if err := setBaseBranch(ctx, s, orgID, "o/r", "develop"); err != nil {
			t.Fatalf("UpdateBaseBranch: %v", err)
		}
		if _, err := s.UpdateCloneStatusByRef(ctx, orgID, domain.RepoRef{Host: TestGitHubHost, Owner: "o", Repo: "r"}, "ok", "", ""); err != nil {
			t.Fatalf("UpdateCloneStatus: %v", err)
		}

		// Re-profile: same id, refreshed description + profile text.
		if _, err := s.Upsert(ctx, orgID, domain.Repository{
			Host:  TestGitHubHost,
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
		s, orgID, _ := mk(t)
		for _, id := range []string{"z/last", "a/first", "m/middle"} {
			owner, repo := id[:1], id[2:]
			if _, err := s.Upsert(ctx, orgID, domain.Repository{
				Host: TestGitHubHost,
				ID:   id, Owner: owner, Repo: repo,
				DefaultBranch: "main",
			}); err != nil {
				t.Fatalf("Upsert %s: %v", id, err)
			}
		}
		got, total, err := s.List(ctx, orgID, TestGitHubHost, db.ListOpts{Limit: 50})
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

		coRows, coTotal, err := s.List(ctx, orgID, TestGitHubHost, db.ListOpts{CountOnly: true})
		if err != nil {
			t.Fatalf("List count-only: %v", err)
		}
		AssertCountOnlyList(t, "repositories.List", len(coRows), coTotal, total)

		// The pages partition that order: a two-row window then a one-row
		// window covers the set exactly once, and each page reports the
		// filtered total rather than its own length.
		first, total, err := s.List(ctx, orgID, TestGitHubHost, db.ListOpts{Limit: 2})
		if err != nil {
			t.Fatalf("List page 1: %v", err)
		}
		second, total2, err := s.List(ctx, orgID, TestGitHubHost, db.ListOpts{Limit: 2, Offset: 2})
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
		if all, _, err := s.List(ctx, orgID, TestGitHubHost, db.Unwindowed); err != nil || len(all) != 3 {
			t.Errorf("unwindowed List = %d rows, %v; want all 3", len(all), err)
		}
	})

	t.Run("Tracked_reads_list_only_rows_a_team_tracks", func(t *testing.T) {
		// The registry outlives tracking: z/gone keeps its row after its only
		// team drops it. The tracked reads leave it out, the whole-registry
		// reads keep it, and a repository two teams track is one row.
		s, orgID, seed := mk(t)
		teamB := seed.Team(t, "team-b")
		idle := seed.Team(t, "team-idle")
		trackRepos(t, s, seed, orgID, seed.TeamID, "a/one", "a/two", "z/gone")
		trackRepos(t, s, seed, orgID, teamB, "a/two", "b/three")
		trackRepos(t, s, seed, orgID, seed.TeamID, "a/one", "a/two")

		slugs := func(rows []domain.Repository) []string {
			out := make([]string, len(rows))
			for i, r := range rows {
				out[i] = r.Slug()
			}
			return out
		}
		tracked := []string{"a/one", "a/two", "b/three"}

		registry, total, err := s.List(ctx, orgID, TestGitHubHost, db.Unwindowed)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if got, want := slugs(registry), append(append([]string{}, tracked...), "z/gone"); !equalStringSlice(got, want) || total != len(want) {
			t.Errorf("List = %v (total %d), want the whole registry %v", got, total, want)
		}

		rows, total, err := s.ListTrackedSystem(ctx, orgID, TestGitHubHost, db.Unwindowed)
		if err != nil {
			t.Fatalf("ListTrackedSystem: %v", err)
		}
		if got := slugs(rows); !equalStringSlice(got, tracked) || total != len(tracked) {
			t.Errorf("ListTrackedSystem = %v (total %d), want %v", got, total, tracked)
		}
		// Rows, not just names: each is the registry row a point read finds.
		for _, row := range rows {
			stored, err := s.GetByRef(ctx, orgID, repoRef(row.Slug()))
			if err != nil || stored == nil || stored.ID != row.ID || stored.Host != TestGitHubHost {
				t.Errorf("ListTrackedSystem row %s (id %s) differs from the stored row %+v (err %v)", row.Slug(), row.ID, stored, err)
			}
		}
		if names, err := s.ListTrackedNamesSystem(ctx, orgID, TestGitHubHost); err != nil || !equalStringSlice(names, tracked) {
			t.Errorf("ListTrackedNamesSystem = %v, %v; want the same set as ListTrackedSystem %v", names, err, tracked)
		}

		first, total, err := s.ListTrackedSystem(ctx, orgID, TestGitHubHost, db.ListOpts{Limit: 2})
		if err != nil {
			t.Fatalf("ListTrackedSystem page 1: %v", err)
		}
		second, total2, err := s.ListTrackedSystem(ctx, orgID, TestGitHubHost, db.ListOpts{Limit: 2, Offset: 2})
		if err != nil {
			t.Fatalf("ListTrackedSystem page 2: %v", err)
		}
		if walked := append(slugs(first), slugs(second)...); !equalStringSlice(walked, tracked) || total != 3 || total2 != 3 {
			t.Errorf("paged walk = %v (totals %d, %d), want %v with total 3 on both", walked, total, total2, tracked)
		}
		coRows, coTotal, err := s.ListTrackedSystem(ctx, orgID, TestGitHubHost, db.ListOpts{CountOnly: true})
		if err != nil {
			t.Fatalf("ListTrackedSystem count-only: %v", err)
		}
		AssertCountOnlyList(t, "repositories.ListTrackedSystem", len(coRows), coTotal, len(tracked))

		for _, c := range []struct {
			name, teamID string
			want         []string
		}{
			{"seed team", seed.TeamID, []string{"a/one", "a/two"}},
			{"team-b", teamB, []string{"a/two", "b/three"}},
			{"team-idle", idle, []string{}},
		} {
			got, total, err := s.ListTrackedByTeamSystem(ctx, orgID, c.teamID, TestGitHubHost, db.Unwindowed)
			if err != nil {
				t.Fatalf("ListTrackedByTeamSystem(%s): %v", c.name, err)
			}
			if got == nil || !equalStringSlice(slugs(got), c.want) || total != len(c.want) {
				t.Errorf("ListTrackedByTeamSystem(%s) = %v (total %d), want %v", c.name, got, total, c.want)
			}
			// Both pools are the same connection here, so the app-pool read
			// answers as the system one does.
			app, appTotal, err := s.ListTrackedByTeam(ctx, orgID, c.teamID, TestGitHubHost, db.Unwindowed)
			if err != nil {
				t.Fatalf("ListTrackedByTeam(%s): %v", c.name, err)
			}
			if app == nil || !equalStringSlice(slugs(app), c.want) || appTotal != len(c.want) {
				t.Errorf("ListTrackedByTeam(%s) = %v (total %d), want %v", c.name, app, appTotal, c.want)
			}
		}

		// The team read pages like every list: a window of one, the full
		// total on each page, and a count-only read.
		page1, total1, err := s.ListTrackedByTeam(ctx, orgID, teamB, TestGitHubHost, db.ListOpts{Limit: 1})
		if err != nil {
			t.Fatalf("ListTrackedByTeam page 1: %v", err)
		}
		page2, total2, err := s.ListTrackedByTeam(ctx, orgID, teamB, TestGitHubHost, db.ListOpts{Limit: 1, Offset: 1})
		if err != nil {
			t.Fatalf("ListTrackedByTeam page 2: %v", err)
		}
		if walked := append(slugs(page1), slugs(page2)...); !equalStringSlice(walked, []string{"a/two", "b/three"}) || total1 != 2 || total2 != 2 {
			t.Errorf("ListTrackedByTeam paged walk = %v (totals %d, %d), want [a/two b/three] with total 2 on both", walked, total1, total2)
		}
		teamCount, teamCountTotal, err := s.ListTrackedByTeam(ctx, orgID, teamB, TestGitHubHost, db.ListOpts{CountOnly: true})
		if err != nil {
			t.Fatalf("ListTrackedByTeam count-only: %v", err)
		}
		AssertCountOnlyList(t, "repositories.ListTrackedByTeam", len(teamCount), teamCountTotal, 2)

		// The non-admin discovery read never surfaces an untracked row either.
		// Both pools are the same connection here, so in Postgres "the
		// viewer's teams" is every team and the set is the tracked one.
		scoped, _, err := s.ListTeamScoped(ctx, orgID, TestGitHubHost, db.Unwindowed)
		if err != nil {
			t.Fatalf("ListTeamScoped: %v", err)
		}
		if got := slugs(scoped); !equalStringSlice(got, tracked) {
			t.Errorf("ListTeamScoped = %v, want %v", got, tracked)
		}
	})

	t.Run("Tracking_an_already_registered_repository_keeps_its_row", func(t *testing.T) {
		// A repository that has been profiled, given a base branch, cloned and
		// polled must come out of a tracking save exactly as it went in: one
		// row, same id, every cached column intact — and under the stored
		// casing, whatever the save spelled it with. Deciding "already here"
		// case-SENSITIVELY would make a save under different casing mint a
		// bare second row for the same repository, and the AI profile, the
		// user's base branch, the clone outcome and the poller's ETag would
		// all be left behind on a row nothing tracks any more. GitHub
		// identifiers are case-insensitive; the two spellings were never two
		// repositories.
		s, orgID, seed := mk(t)
		profiled := time.Now().UTC().Truncate(time.Second)
		if _, err := s.Upsert(ctx, orgID, domain.Repository{
			Host:  TestGitHubHost,
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
		if _, err := s.UpdateCloneStatusByRef(ctx, orgID, domain.RepoRef{Host: TestGitHubHost, Owner: "Acme", Repo: "Api"}, "ok", "", ""); err != nil {
			t.Fatalf("UpdateCloneStatus: %v", err)
		}
		etagAt := profiled.Add(time.Hour)
		if err := s.SetPullsPollStateByRefSystem(ctx, orgID, repoRef("Acme/Api"), `"etag-v1"`, etagAt); err != nil {
			t.Fatalf("SetPullsPollStateByRefSystem: %v", err)
		}

		before, err := s.GetByRef(ctx, orgID, repoRef("Acme/Api"))
		if err != nil || before == nil {
			t.Fatalf("read the profiled row: got=%v err=%v", before, err)
		}

		// The same repository, tracked lowercased, alongside a genuinely new
		// one — so this exercises a save that mints as well as one that
		// resolves.
		trackRepos(t, s, seed, orgID, seed.TeamID, "acme/api", "octo/new")

		if n := registryRows(t, s, orgID); n != 2 {
			t.Fatalf("rows = %d, want 2 — a casing difference is not a second repository", n)
		}
		got, err := s.GetByRef(ctx, orgID, repoRef("acme/api"))
		if err != nil || got == nil {
			t.Fatalf("Get after tracking: got=%v err=%v", got, err)
		}
		if got.ID != before.ID {
			t.Errorf("id = %q, want %q — tracking must not re-key an existing row", got.ID, before.ID)
		}
		if got.Slug() != "Acme/Api" {
			t.Errorf("slug = %q, want Acme/Api — stored casing is sticky", got.Slug())
		}
		if got.ProfileText != "accumulated profile" || got.Description != "Api service" {
			t.Errorf("profile lost by the save: %+v", got)
		}
		if !got.HasReadme || !got.HasClaudeMd {
			t.Errorf("doc flags lost by the save: readme=%v claude=%v", got.HasReadme, got.HasClaudeMd)
		}
		if got.BaseBranch != "develop" {
			t.Errorf("BaseBranch = %q, want develop — a user setting must survive a save", got.BaseBranch)
		}
		if got.CloneURL != "git@github.com:Acme/Api.git" || got.CloneStatus != "ok" {
			t.Errorf("clone state lost by the save: %+v", got)
		}
		if got.ExternalID != "1296269" {
			t.Errorf("ExternalID = %q, want it kept — a save learns no id and clears none", got.ExternalID)
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

	t.Run("UpdateBaseBranch_empty_clears_to_null", func(t *testing.T) {
		// Empty string stores NULL — falls back to default_branch at
		// use site. The pre-D2 impl used nullIfEmpty(); the new
		// store does the same via NULLIF / nullIfEmpty.
		s, orgID, _ := mk(t)
		if _, err := s.Upsert(ctx, orgID, domain.Repository{
			Host:  TestGitHubHost,
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
		s, orgID, _ := mk(t)
		if _, err := s.Upsert(ctx, orgID, domain.Repository{
			Host:  TestGitHubHost,
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
		s, orgID, _ := mk(t)
		if _, err := s.Upsert(ctx, orgID, domain.Repository{
			Host:  TestGitHubHost,
			Owner: "o", Repo: "r",
			DefaultBranch: "main",
		}); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		// ok path: empty err fields collapse to NULL → empty string on read.
		if _, err := s.UpdateCloneStatusByRef(ctx, orgID, domain.RepoRef{Host: TestGitHubHost, Owner: "o", Repo: "r"}, "ok", "", ""); err != nil {
			t.Fatalf("UpdateCloneStatus ok: %v", err)
		}
		got, _ := s.GetByRef(ctx, orgID, repoRef("o/r"))
		if got.CloneStatus != "ok" || got.CloneError != "" || got.CloneErrorKind != "" {
			t.Errorf("ok status mismatch: %+v", got)
		}
		// failed path: ssh-kind capture for SSH preflight confirms.
		if _, err := s.UpdateCloneStatusByRef(ctx, orgID, domain.RepoRef{Host: TestGitHubHost, Owner: "o", Repo: "r"}, "failed", "permission denied", "ssh"); err != nil {
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
		s, orgID, _ := mk(t)
		if _, err := s.UpdateCloneStatusByRef(ctx, orgID, domain.RepoRef{Host: TestGitHubHost, Owner: "ghost", Repo: "repo"}, "ok", "", ""); err != nil {
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
		s, orgID, seed := mk(t)
		trackRepos(t, s, seed, orgID, seed.TeamID, "Acme/Api")

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

		if _, err := s.UpdateCloneStatusByRef(ctx, orgID, domain.RepoRef{Host: TestGitHubHost, Owner: "acme", Repo: "api"}, "failed", "boom", "ssh"); err != nil {
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
		s, orgID, seed := mk(t)
		trackRepos(t, s, seed, orgID, seed.TeamID, "octo/widget")
		bogus := domain.RepoRef{Source: "gitlob", Host: TestGitHubHost, Owner: "octo", Repo: "widget"}

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

	t.Run("The_by_name_door_mints_one_bare_row_and_every_repeat_is_a_read", func(t *testing.T) {
		// The core of the by-name create path (FindOrCreateByRefSystem, which
		// trackRepos stages through): the first call mints the row, and every
		// call after it — for the same team's save or another's — resolves to
		// the same one. "Same" is checked by id AND by row count, because the
		// failure this guards is a second row for the same repository, which
		// a per-call read would happily read past. The row it mints is a
		// complete identity row from the moment it exists, and nothing more:
		// a name carries no provider id and no profile.
		s, orgID, seed := mk(t)

		trackRepos(t, s, seed, orgID, seed.TeamID, "octo/widget")
		first, err := s.GetByRef(ctx, orgID, repoRef("octo/widget"))
		if err != nil || first == nil {
			t.Fatalf("GetByRef after tracking: got=%v err=%v — the by-name door must create the row immediately", first, err)
		}
		if first.Slug() != "octo/widget" || first.Owner != "octo" || first.Repo != "widget" {
			t.Errorf("created row identity = %+v, want octo/widget", first)
		}
		if first.Source != domain.RepoSourceGitHub {
			t.Errorf("created row source = %q, want %q", first.Source, domain.RepoSourceGitHub)
		}
		if first.ExternalID != "" {
			t.Errorf("created row external id = %q, want empty — a name carries no id, and none is invented", first.ExternalID)
		}
		if first.ProfileText != "" || first.ProfiledAt != nil {
			t.Errorf("created row should be bare until the profiler runs, got %+v", first)
		}

		other := seed.Team(t, "other-team")
		for i, teamID := range []string{seed.TeamID, seed.TeamID, other, seed.TeamID} {
			trackRepos(t, s, seed, orgID, teamID, "octo/widget")
			again, err := s.GetByRef(ctx, orgID, repoRef("octo/widget"))
			if err != nil || again == nil {
				t.Fatalf("GetByRef (repeat %d): got=%v err=%v", i, again, err)
			}
			if again.ID != first.ID {
				t.Errorf("repeat %d resolved to id %q, want %q", i, again.ID, first.ID)
			}
		}
		if n := registryRows(t, s, orgID); n != 1 {
			t.Errorf("rows after 5 saves by 2 teams = %d, want 1", n)
		}
	})

	t.Run("Two_teams_tracking_one_repository_under_two_casings_share_a_row", func(t *testing.T) {
		// GitHub identifiers are case-insensitive and the unique index folds
		// them, so a second team spelling a tracked repository differently
		// resolves to the existing row rather than minting a second one for
		// the same repository. Stored casing is sticky: the first spelling
		// wins and the second save reads it back.
		s, orgID, seed := mk(t)
		trackRepos(t, s, seed, orgID, seed.TeamID, "Acme/Api")
		first, err := s.GetByRef(ctx, orgID, repoRef("Acme/Api"))
		if err != nil || first == nil {
			t.Fatalf("GetByRef after the first save: got=%v err=%v", first, err)
		}

		trackRepos(t, s, seed, orgID, seed.Team(t, "other-team"), "acme/API")
		got, err := s.GetByRef(ctx, orgID, repoRef("acme/API"))
		if err != nil || got == nil {
			t.Fatalf("GetByRef (mismatched case): got=%v err=%v", got, err)
		}
		if got.ID != first.ID {
			t.Errorf("id = %q, want %q — a casing difference is not a second repository", got.ID, first.ID)
		}
		if got.Slug() != "Acme/Api" {
			t.Errorf("slug = %q, want Acme/Api — stored casing is sticky", got.Slug())
		}
		if n := registryRows(t, s, orgID); n != 1 {
			t.Errorf("rows = %d, want 1 — a casing difference is not a second repository", n)
		}
	})

	t.Run("FillMissingExternalIDs_only_ever_turns_NULL_into_a_value", func(t *testing.T) {
		// The poller's half of repository identity: it already enumerates each
		// installation's grant every cycle, and that response carries the ids.
		// This is the write, and its whole contract is that it is safe to run
		// on every cycle — it fills what is missing, touches nothing else, and
		// reports how much it filled so the steady state is visibly zero.
		s, orgID, seed := mk(t)
		trackRepos(t, s, seed, orgID, seed.TeamID, "Acme/Api", "octo/known", "octo/bare")
		if _, err := s.Upsert(ctx, orgID, domain.Repository{
			Host:  TestGitHubHost,
			Owner: "octo", Repo: "known",
			ExternalID: "111", DefaultBranch: "main",
		}); err != nil {
			t.Fatalf("seed known id: %v", err)
		}

		filled, err := s.FillMissingExternalIDsSystem(ctx, orgID, []domain.RepoRef{
			// Different casing than stored — the grant's spelling is GitHub's,
			// not necessarily the one tracked.
			{Host: TestGitHubHost, Owner: "acme", Repo: "api", ExternalID: "1296269"},
			// Already has one; a fill must never move it.
			{Host: TestGitHubHost, Owner: "octo", Repo: "known", ExternalID: "999"},
			// No id to record; not a write.
			{Host: TestGitHubHost, Owner: "octo", Repo: "bare"},
			// Not a configured repo; nothing to fill, and nothing created.
			{Host: TestGitHubHost, Owner: "ghost", Repo: "repo", ExternalID: "42"},
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
		if n := registryRows(t, s, orgID); n != 3 {
			t.Errorf("rows = %d, want 3 — a fill adds none", n)
		}

		// Steady state: running it again fills nothing.
		filled, err = s.FillMissingExternalIDsSystem(ctx, orgID, []domain.RepoRef{
			{Host: TestGitHubHost, Owner: "Acme", Repo: "Api", ExternalID: "1296269"},
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

	t.Run("Upsert_round_trips_identity_and_refreshes_the_external_id", func(t *testing.T) {
		// The profiler's write path: it learns the id from the same
		// /repos/{owner}/{repo} response it takes the clone URL from, so a
		// re-profile carrying an id refreshes it while one carrying none
		// leaves the stored id alone.
		s, orgID, _ := mk(t)
		if _, err := s.Upsert(ctx, orgID, domain.Repository{
			Host:  TestGitHubHost,
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
			Host:  TestGitHubHost,
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
			Host:  TestGitHubHost,
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
		s, orgID, _ := mk(t)
		if _, err := s.Upsert(ctx, orgID, domain.Repository{
			Host:  TestGitHubHost,
			Owner: "Acme", Repo: "Api",
			ProfileText: "v1", DefaultBranch: "main",
		}); err != nil {
			t.Fatalf("seed Upsert: %v", err)
		}
		if err := setBaseBranch(ctx, s, orgID, "Acme/Api", "develop"); err != nil {
			t.Fatalf("UpdateBaseBranch: %v", err)
		}

		if _, err := s.Upsert(ctx, orgID, domain.Repository{
			Host:  TestGitHubHost,
			Owner: "acme", Repo: "api",
			ProfileText: "v2", DefaultBranch: "main", ExternalID: "1296269",
		}); err != nil {
			t.Fatalf("differently-cased Upsert: %v", err)
		}

		if n := registryRows(t, s, orgID); n != 1 {
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
		s, orgID, _ := mk(t)
		if _, err := s.Upsert(ctx, orgID, domain.Repository{
			Host:  TestGitHubHost,
			Owner: "octo", Repo: "widget", Source: "gitlob",
		}); err == nil {
			t.Error("Upsert accepted an unknown source; want an error")
		}
		if n := registryRows(t, s, orgID); n != 0 {
			t.Errorf("rows = %d, want 0 — a refused source must not write", n)
		}
	})

	t.Run("The_same_slug_on_two_hosts_is_two_repositories", func(t *testing.T) {
		// A repository's identity is (org, source, host, owner/repo): GitHub
		// names repeat across deployments, so the same owner/repo on two hosts
		// is two rows, and every ref-keyed and listing read answers only for
		// the host it is asked about.
		s, orgID, seed := mk(t)
		trackReposOnHost(t, s, seed, orgID, seed.TeamID, TestGitHubHost, "octo/widget", "octo/only-dotcom")
		trackReposOnHost(t, s, seed, orgID, seed.TeamID, TestOtherGitHubHost, "octo/widget", "octo/only-ghe")

		dotcom, err := s.GetByRefSystem(ctx, orgID, domain.RepoRefFromSlug(TestGitHubHost, "octo/widget"))
		if err != nil || dotcom == nil {
			t.Fatalf("GetByRefSystem(github.com) = %v, %v", dotcom, err)
		}
		ghe, err := s.GetByRefSystem(ctx, orgID, domain.RepoRefFromSlug(TestOtherGitHubHost, "OCTO/Widget"))
		if err != nil || ghe == nil {
			t.Fatalf("GetByRefSystem(ghe) = %v, %v", ghe, err)
		}
		if dotcom.ID == ghe.ID {
			t.Fatalf("both hosts resolved to row %s; want two rows", dotcom.ID)
		}
		if dotcom.Host != TestGitHubHost || ghe.Host != TestOtherGitHubHost {
			t.Errorf("hosts = (%q, %q), want (%q, %q)", dotcom.Host, ghe.Host, TestGitHubHost, TestOtherGitHubHost)
		}
		if miss, err := s.GetByRefSystem(ctx, orgID, domain.RepoRefFromSlug(TestOtherGitHubHost, "octo/only-dotcom")); err != nil || miss != nil {
			t.Errorf("GetByRefSystem(ghe, octo/only-dotcom) = %+v, %v; want nil — it is tracked on github.com only", miss, err)
		}

		// A write through one host's ref lands on that host's row alone.
		if _, err := s.UpdateCloneStatusByRefSystem(ctx, orgID, domain.RepoRefFromSlug(TestOtherGitHubHost, "octo/widget"), "failed", "boom", "ssh"); err != nil {
			t.Fatalf("UpdateCloneStatusByRefSystem(ghe): %v", err)
		}
		if got, _ := s.Get(ctx, orgID, dotcom.ID); got == nil || got.CloneStatus == "failed" {
			t.Errorf("github.com row after a ghe clone-status write = %+v; want it untouched", got)
		}

		listed := func(rows []domain.Repository, host string) []string {
			t.Helper()
			out := make([]string, 0, len(rows))
			for _, r := range rows {
				if r.Host != host {
					t.Errorf("row %s on %q listed for %q", r.Slug(), r.Host, host)
				}
				out = append(out, r.Slug())
			}
			return out
		}
		for _, c := range []struct {
			host string
			want []string
		}{
			{TestGitHubHost, []string{"octo/only-dotcom", "octo/widget"}},
			{TestOtherGitHubHost, []string{"octo/only-ghe", "octo/widget"}},
		} {
			rows, total, err := s.List(ctx, orgID, c.host, db.Unwindowed)
			if err != nil {
				t.Fatalf("List(%s): %v", c.host, err)
			}
			if got := listed(rows, c.host); !equalStringSlice(got, c.want) || total != len(c.want) {
				t.Errorf("List(%s) = %v (total %d), want %v", c.host, got, total, c.want)
			}
			sys, err := s.ListSystem(ctx, orgID, c.host)
			if err != nil {
				t.Fatalf("ListSystem(%s): %v", c.host, err)
			}
			if got := listed(sys, c.host); !equalStringSlice(got, c.want) {
				t.Errorf("ListSystem(%s) = %v, want %v", c.host, got, c.want)
			}
			names, err := s.ListTrackedNamesSystem(ctx, orgID, c.host)
			if err != nil {
				t.Fatalf("ListTrackedNamesSystem(%s): %v", c.host, err)
			}
			if !equalStringSlice(names, c.want) {
				t.Errorf("ListTrackedNamesSystem(%s) = %v, want %v", c.host, names, c.want)
			}
			trackedRows, trackedTotal, err := s.ListTrackedSystem(ctx, orgID, c.host, db.Unwindowed)
			if err != nil {
				t.Fatalf("ListTrackedSystem(%s): %v", c.host, err)
			}
			if got := listed(trackedRows, c.host); !equalStringSlice(got, c.want) || trackedTotal != len(c.want) {
				t.Errorf("ListTrackedSystem(%s) = %v (total %d), want %v", c.host, got, trackedTotal, c.want)
			}
			teamRows, _, err := s.ListTrackedByTeamSystem(ctx, orgID, seed.TeamID, c.host, db.Unwindowed)
			if err != nil {
				t.Fatalf("ListTrackedByTeamSystem(%s): %v", c.host, err)
			}
			if got := listed(teamRows, c.host); !equalStringSlice(got, c.want) {
				t.Errorf("ListTrackedByTeamSystem(%s) = %v, want %v", c.host, got, c.want)
			}
		}

		// Untracking everything on one host leaves the other host's tracked
		// set exactly as it stands.
		trackReposOnHost(t, s, seed, orgID, seed.TeamID, TestOtherGitHubHost)
		if names, err := s.ListTrackedNamesSystem(ctx, orgID, TestGitHubHost); err != nil || !equalStringSlice(names, []string{"octo/only-dotcom", "octo/widget"}) {
			t.Errorf("github.com tracked names after clearing ghe = %v, %v; want both kept", names, err)
		}
		if names, err := s.ListTrackedNamesSystem(ctx, orgID, TestOtherGitHubHost); err != nil || len(names) != 0 {
			t.Errorf("ghe tracked names after clearing = %v, %v; want none", names, err)
		}

		// Each host's provider ids are its own: the identities a rename
		// detector reads for one host carry that host and none of the other's.
		if filled, err := s.FillMissingExternalIDsSystem(ctx, orgID, []domain.RepoRef{
			{Host: TestGitHubHost, Owner: "octo", Repo: "widget", ExternalID: "100"},
			{Host: TestOtherGitHubHost, Owner: "octo", Repo: "widget", ExternalID: "200"},
		}); err != nil || filled != 2 {
			t.Fatalf("FillMissingExternalIDsSystem = %d, %v; want 2 — one row per host", filled, err)
		}
		for _, c := range []struct{ host, id string }{{TestGitHubHost, "100"}, {TestOtherGitHubHost, "200"}} {
			ids, err := s.ListIdentitiesSystem(ctx, orgID, c.host)
			if err != nil {
				t.Fatalf("ListIdentitiesSystem(%s): %v", c.host, err)
			}
			if len(ids) != 1 || ids[0].Host != c.host || ids[0].Slug() != "octo/widget" || ids[0].ExternalID != c.id {
				t.Errorf("ListIdentitiesSystem(%s) = %+v, want only octo/widget id %s on that host", c.host, ids, c.id)
			}
		}
	})

	t.Run("Every_host_scoped_method_refuses_an_empty_host", func(t *testing.T) {
		// A name and a provider id are each unique only within one host, so
		// a call without one names no repository. It is refused rather than
		// answered as a miss, which would read as "not configured".
		s, orgID, seed := mk(t)
		trackRepos(t, s, seed, orgID, seed.TeamID, "octo/widget")
		noHost := domain.RepoRef{Owner: "octo", Repo: "widget", ExternalID: "1"}

		check := func(name string, err error) {
			t.Helper()
			if !errors.Is(err, db.ErrRepoHostRequired) {
				t.Errorf("%s with no host = %v, want db.ErrRepoHostRequired", name, err)
			}
		}
		_, err := s.GetByRef(ctx, orgID, noHost)
		check("GetByRef", err)
		_, err = s.GetByRefSystem(ctx, orgID, noHost)
		check("GetByRefSystem", err)
		_, err = s.Upsert(ctx, orgID, domain.Repository{Owner: "octo", Repo: "widget", ProfileText: "v2"})
		check("Upsert", err)
		_, err = s.UpsertSystem(ctx, orgID, domain.Repository{Owner: "octo", Repo: "widget", ProfileText: "v2"})
		check("UpsertSystem", err)
		_, err = s.UpdateCloneStatusByRef(ctx, orgID, noHost, "failed", "boom", "ssh")
		check("UpdateCloneStatusByRef", err)
		_, err = s.UpdateCloneStatusByRefSystem(ctx, orgID, noHost, "failed", "boom", "ssh")
		check("UpdateCloneStatusByRefSystem", err)
		check("SetPullsPollStateByRefSystem", s.SetPullsPollStateByRefSystem(ctx, orgID, noHost, `"etag"`, time.Now()))
		_, _, err = s.GetPullsPollStateByRefSystem(ctx, orgID, noHost)
		check("GetPullsPollStateByRefSystem", err)
		_, err = s.FillMissingExternalIDsSystem(ctx, orgID, []domain.RepoRef{noHost})
		check("FillMissingExternalIDsSystem", err)
		_, err = s.RenameSystem(ctx, orgID, domain.RepoRef{Owner: "octo", Repo: "renamed", ExternalID: "1"})
		check("RenameSystem", err)
		_, _, err = s.List(ctx, orgID, "", db.Unwindowed)
		check("List", err)
		_, err = s.ListSystem(ctx, orgID, "")
		check("ListSystem", err)
		_, err = s.ListTrackedNamesSystem(ctx, orgID, "")
		check("ListTrackedNamesSystem", err)
		_, _, err = s.ListTrackedSystem(ctx, orgID, "", db.Unwindowed)
		check("ListTrackedSystem", err)
		_, _, err = s.ListTrackedByTeamSystem(ctx, orgID, seed.TeamID, "", db.Unwindowed)
		check("ListTrackedByTeamSystem", err)
		_, _, err = s.ListTrackedByTeam(ctx, orgID, seed.TeamID, "", db.Unwindowed)
		check("ListTrackedByTeam", err)
		_, err = s.ListIdentitiesSystem(ctx, orgID, "")
		check("ListIdentitiesSystem", err)
		check("ReplaceForTeam", seed.Tracking.ReplaceForTeam(ctx, orgID, seed.TeamID, "", nil))

		// None of the refusals wrote anything.
		got, err := s.GetByRef(ctx, orgID, repoRef("octo/widget"))
		if err != nil || got == nil {
			t.Fatalf("GetByRef after the refusals = %v, %v", got, err)
		}
		if got.CloneStatus == "failed" || got.ExternalID != "" || got.ProfileText != "" {
			t.Errorf("row after the refusals = %+v; a refused call must not write", got)
		}
		if n := registryRows(t, s, orgID); n != 1 {
			t.Errorf("rows = %d, want 1 — a refused tracking save must not mint", n)
		}
	})

	t.Run("A_provider_id_is_unique_per_host", func(t *testing.T) {
		// Repository ids are per-deployment sequences, so the same number on
		// two hosts names two repositories and both rows keep it. Within one
		// host the id names one repository: an upsert carrying an id another
		// row on the host already holds does not write it — that is a rename
		// the poller has not applied — and the returned row says so.
		s, orgID, _ := mk(t)
		read := func(id string) func() (*domain.Repository, error) {
			return func() (*domain.Repository, error) { return s.Get(ctx, orgID, id) }
		}

		dotcom, err := s.Upsert(ctx, orgID, domain.Repository{
			Host: TestGitHubHost, Owner: "octo", Repo: "widget", ExternalID: "4242", DefaultBranch: "main",
		})
		if err != nil {
			t.Fatalf("Upsert(github.com): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Upsert(github.com)", dotcom, read(dotcom.ID))
		ghe, err := s.UpsertSystem(ctx, orgID, domain.Repository{
			Host: TestOtherGitHubHost, Owner: "octo", Repo: "widget", ExternalID: "4242", DefaultBranch: "main",
		})
		if err != nil {
			t.Fatalf("UpsertSystem(ghe): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "UpsertSystem(ghe)", ghe, read(ghe.ID))
		if ghe.ID == dotcom.ID || ghe.Host != TestOtherGitHubHost || ghe.ExternalID != "4242" {
			t.Errorf("ghe row = %+v; want its own row on %s carrying 4242", ghe, TestOtherGitHubHost)
		}
		if dotcom.Host != TestGitHubHost || dotcom.ExternalID != "4242" {
			t.Errorf("github.com row = %+v; want it on %s carrying 4242", dotcom, TestGitHubHost)
		}

		// A new name on github.com claiming the id octo/widget holds there:
		// the row is created, without the id.
		claimant, err := s.Upsert(ctx, orgID, domain.Repository{
			Host: TestGitHubHost, Owner: "octo", Repo: "widget-renamed", ExternalID: "4242", DefaultBranch: "main",
		})
		if err != nil {
			t.Fatalf("Upsert(claimant): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Upsert(claimant)", claimant, read(claimant.ID))
		if claimant.ExternalID != "" {
			t.Errorf("claimant external id = %q, want empty — octo/widget already holds 4242 on this host", claimant.ExternalID)
		}

		// The update arm follows the same rule: a row with its own id keeps it
		// when an upsert carries one another row on the host holds.
		other, err := s.Upsert(ctx, orgID, domain.Repository{
			Host: TestGitHubHost, Owner: "octo", Repo: "other", ExternalID: "5150", DefaultBranch: "main",
		})
		if err != nil {
			t.Fatalf("Upsert(other): %v", err)
		}
		reprofiled, err := s.Upsert(ctx, orgID, domain.Repository{
			Host: TestGitHubHost, Owner: "Octo", Repo: "Other", ExternalID: "4242", ProfileText: "v2", DefaultBranch: "main",
		})
		if err != nil {
			t.Fatalf("Upsert(other, colliding id): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Upsert(other, colliding id)", reprofiled, read(other.ID))
		if reprofiled.ID != other.ID || reprofiled.ExternalID != "5150" || reprofiled.ProfileText != "v2" {
			t.Errorf("update arm returned %+v; want row %s keeping 5150 with the profile refreshed", reprofiled, other.ID)
		}

		// The holders are unmoved, and the fill obeys the same rule.
		for _, c := range []struct {
			host, slug, id string
		}{
			{TestGitHubHost, "octo/widget", "4242"},
			{TestOtherGitHubHost, "octo/widget", "4242"},
		} {
			got, err := s.GetByRefSystem(ctx, orgID, domain.RepoRefFromSlug(c.host, c.slug))
			if err != nil || got == nil || got.ExternalID != c.id {
				t.Errorf("%s on %s = %+v, %v; want external id %s", c.slug, c.host, got, err, c.id)
			}
		}
		if filled, err := s.FillMissingExternalIDsSystem(ctx, orgID, []domain.RepoRef{
			{Host: TestGitHubHost, Owner: "octo", Repo: "widget-renamed", ExternalID: "4242"},
		}); err != nil || filled != 0 {
			t.Errorf("fill of an id another row on the host holds = %d, %v; want 0", filled, err)
		}
	})

	t.Run("FindOrCreateByExternalID_creates_a_row_carrying_the_id", func(t *testing.T) {
		s, orgID, _ := mk(t)
		created, err := s.FindOrCreateByExternalID(ctx, orgID, domain.RepoRef{Host: TestGitHubHost, Owner: "Octo", Repo: "Widget", ExternalID: "7001"})
		if err != nil {
			t.Fatalf("FindOrCreateByExternalID: %v", err)
		}
		if created.ID == "" || created.ExternalID != "7001" || created.Slug() != "Octo/Widget" ||
			created.Source != domain.RepoSourceGitHub || created.Host != TestGitHubHost || created.ProfiledAt != nil {
			t.Errorf("created = %+v, want a bare Octo/Widget row carrying 7001", created)
		}
		AssertWriteReturnedStoredRow(t, "FindOrCreateByExternalID (create)", created, func() (*domain.Repository, error) {
			return s.Get(ctx, orgID, created.ID)
		})
		// Registered is not tracked: nothing about the row puts it in what TF
		// polls.
		if names, err := s.ListTrackedNamesSystem(ctx, orgID, TestGitHubHost); err != nil || len(names) != 0 {
			t.Errorf("tracked names after a registration = %v, %v; want none", names, err)
		}

		again, err := s.FindOrCreateByExternalID(ctx, orgID, domain.RepoRef{Host: TestGitHubHost, Owner: "octo", Repo: "widget", ExternalID: "7001"})
		if err != nil {
			t.Fatalf("second FindOrCreateByExternalID: %v", err)
		}
		if again.ID != created.ID || again.Slug() != "Octo/Widget" {
			t.Errorf("second registration = %s %s, want the first row under its stored casing", again.ID, again.Slug())
		}
		if n := registryRows(t, s, orgID); n != 1 {
			t.Errorf("registry rows = %d, want 1", n)
		}
	})

	t.Run("FindOrCreateByExternalID_finds_the_id_before_the_name", func(t *testing.T) {
		// GitHub renamed octo/widget to octo/gadget and TF has not applied it.
		// A registration naming the repository by its id and its new name is
		// the existing row, under the name TF stores, and no second row.
		s, orgID, _ := mk(t)
		old, err := s.FindOrCreateByExternalID(ctx, orgID, domain.RepoRef{Host: TestGitHubHost, Owner: "octo", Repo: "widget", ExternalID: "7101"})
		if err != nil {
			t.Fatalf("register octo/widget: %v", err)
		}
		got, err := s.FindOrCreateByExternalID(ctx, orgID, domain.RepoRef{Host: TestGitHubHost, Owner: "octo", Repo: "gadget", ExternalID: "7101"})
		if err != nil {
			t.Fatalf("register under the new name: %v", err)
		}
		if got.ID != old.ID || got.Slug() != "octo/widget" {
			t.Errorf("registration under the new name = %s %s, want row %s as stored (octo/widget)", got.ID, got.Slug(), old.ID)
		}
		if n := registryRows(t, s, orgID); n != 1 {
			t.Errorf("registry rows = %d, want 1 — an unapplied rename is not a second repository", n)
		}
	})

	t.Run("FindOrCreateByExternalID_writes_the_id_onto_a_bare_row_holding_the_name", func(t *testing.T) {
		s, orgID, _ := mk(t)
		bare, err := s.FindOrCreateByRefSystem(ctx, orgID, domain.RepoRef{Host: TestGitHubHost, Owner: "Acme", Repo: "Api"})
		if err != nil {
			t.Fatalf("mint bare row: %v", err)
		}
		got, err := s.FindOrCreateByExternalID(ctx, orgID, domain.RepoRef{Host: TestGitHubHost, Owner: "acme", Repo: "api", ExternalID: "7201"})
		if err != nil {
			t.Fatalf("FindOrCreateByExternalID: %v", err)
		}
		if got.ID != bare.ID || got.ExternalID != "7201" || got.Slug() != "Acme/Api" {
			t.Errorf("registration = %+v, want bare row %s carrying 7201 under its stored casing", got, bare.ID)
		}
		AssertWriteReturnedStoredRow(t, "FindOrCreateByExternalID (adopt)", got, func() (*domain.Repository, error) {
			return s.Get(ctx, orgID, bare.ID)
		})
		if n := registryRows(t, s, orgID); n != 1 {
			t.Errorf("registry rows = %d, want 1", n)
		}
	})

	t.Run("FindOrCreateByExternalID_refuses_a_name_held_under_another_id", func(t *testing.T) {
		s, orgID, _ := mk(t)
		held, err := s.FindOrCreateByExternalID(ctx, orgID, domain.RepoRef{Host: TestGitHubHost, Owner: "acme", Repo: "api", ExternalID: "7301"})
		if err != nil {
			t.Fatalf("register acme/api: %v", err)
		}
		_, err = s.FindOrCreateByExternalID(ctx, orgID, domain.RepoRef{Host: TestGitHubHost, Owner: "Acme", Repo: "API", ExternalID: "7302"})
		if !errors.Is(err, db.ErrRepoSlugOccupied) {
			t.Fatalf("registration of another id under a held name = %v, want db.ErrRepoSlugOccupied", err)
		}
		after, err := s.Get(ctx, orgID, held.ID)
		if err != nil || after.ExternalID != "7301" {
			t.Errorf("held row after the refusal = %+v, %v; want it still carrying 7301", after, err)
		}
		if n := registryRows(t, s, orgID); n != 1 {
			t.Errorf("registry rows = %d, want 1 — a refused registration writes nothing", n)
		}
	})

	t.Run("FindOrCreateByExternalID_is_host_scoped_and_requires_its_inputs", func(t *testing.T) {
		s, orgID, _ := mk(t)
		here, err := s.FindOrCreateByExternalID(ctx, orgID, domain.RepoRef{Host: TestGitHubHost, Owner: "acme", Repo: "api", ExternalID: "7401"})
		if err != nil {
			t.Fatalf("register on %s: %v", TestGitHubHost, err)
		}
		there, err := s.FindOrCreateByExternalID(ctx, orgID, domain.RepoRef{Host: TestOtherGitHubHost, Owner: "acme", Repo: "api", ExternalID: "7401"})
		if err != nil {
			t.Fatalf("register on %s: %v", TestOtherGitHubHost, err)
		}
		if here.ID == there.ID || there.Host != TestOtherGitHubHost {
			t.Errorf("one id on two hosts resolved to %s and %s (host %s); want two rows", here.ID, there.ID, there.Host)
		}
		if _, err := s.FindOrCreateByExternalID(ctx, orgID, domain.RepoRef{Owner: "acme", Repo: "api", ExternalID: "7401"}); !errors.Is(err, db.ErrRepoHostRequired) {
			t.Errorf("no host = %v, want db.ErrRepoHostRequired", err)
		}
		if _, err := s.FindOrCreateByExternalID(ctx, orgID, domain.RepoRef{Host: TestGitHubHost, Owner: "acme", Repo: "web"}); err == nil {
			t.Error("no provider id was accepted; a registration by id needs one")
		}
		if n := registryRows(t, s, orgID); n != 1 {
			t.Errorf("registry rows on %s = %d, want 1", TestGitHubHost, n)
		}
	})

	t.Run("PullsPollState_round_trips", func(t *testing.T) {
		// The GitHub poller stores a per-repo ETag + last-poll time for
		// conditional open-PR discovery. Unset reads as ("", nil);
		// a Set persists both; a re-Set overwrites.
		s, orgID, seed := mk(t)
		trackRepos(t, s, seed, orgID, seed.TeamID, "octo/widget")

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
		s, orgID, _ := mk(t)
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

// trackRepos stages slugs as the team's tracked set: each is brought into the
// registry by name through FindOrCreateByRefSystem, the door a bare row comes
// through, and tracked by the id that returns. The slugs are written in the
// "owner/repo" a person would type, the form the cases are about.
func trackRepos(t *testing.T, s db.RepositoryStore, seed RepositorySeeder, orgID, teamID string, slugs ...string) {
	t.Helper()
	trackReposOnHost(t, s, seed, orgID, teamID, TestGitHubHost, slugs...)
}

// trackReposOnHost is trackRepos on a named GitHub host. It replaces the
// team's tracked set on that host only.
func trackReposOnHost(t *testing.T, s db.RepositoryStore, seed RepositorySeeder, orgID, teamID, host string, slugs ...string) {
	t.Helper()
	repos := make([]domain.TeamGitHubRepo, 0, len(slugs))
	for _, slug := range slugs {
		ref := domain.RepoRefFromSlug(host, slug)
		repos = append(repos, domain.TeamGitHubRepo{Owner: ref.Owner, Repo: ref.Repo})
	}
	if err := TrackRepos(context.Background(), s, seed.Tracking, orgID, teamID, host, repos); err != nil {
		t.Fatalf("track %v on %s for team %s: %v", slugs, host, teamID, err)
	}
}

// registryRows counts the registry rows on TestGitHubHost, tracked or not,
// through the count-only List read. The cases that pin "no second row" or "a
// refused write minted nothing" are about the registry, not the tracked set.
func registryRows(t *testing.T, s db.RepositoryStore, orgID string) int {
	t.Helper()
	_, n, err := s.List(context.Background(), orgID, TestGitHubHost, db.ListOpts{CountOnly: true})
	if err != nil {
		t.Fatalf("count registry rows: %v", err)
	}
	return n
}

// unknownRepoID is a well-formed registry id no row carries. Well-formed
// matters: Postgres types the id column as uuid, so an arbitrary string would
// exercise the malformed-handle path rather than the stale-handle one, and
// those are different assertions.
const unknownRepoID = "3f2b1c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"

// repoRef is the suite's edge parser. These cases are written in the
// "owner/repo" a person or an agent would type, because that is the form the
// behaviour under test is about; the ref-keyed store methods take it parsed.
func repoRef(slug string) domain.RepoRef { return domain.RepoRefFromSlug(TestGitHubHost, slug) }

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
