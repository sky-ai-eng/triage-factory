package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestTeamGitHubRepos_ReplaceForTeam_AppPath exercises the production write
// path on real Postgres: team admins registering repositories by GitHub id and
// saving their tracked sets by registry id, each through the claims-bound app
// pool (RLS active). Proves the two-set contract end-to-end under real RLS:
// registering brings a repository into the registry carrying its GitHub id,
// a second team registering it gets the same row, the tracked set is the
// union across teams, untracking shrinks that union while the registry row
// stays, and a team admin cannot write another team's set.
func TestTeamGitHubRepos_ReplaceForTeam_AppPath(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	orgA, alice, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	teamB := pgtest.SeedTeam(t, h, orgA, "team-b")
	carol := pgtest.SeedUser(t, h, "carol")
	pgtest.AddOrgMember(t, h, carol, orgA, teamB, "member", "admin")

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)

	slugs := func(query string) []string {
		t.Helper()
		rows, err := h.AdminDB.Query(query, orgA)
		if err != nil {
			t.Fatalf("query %q: %v", query, err)
		}
		defer rows.Close()
		out := []string{}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, s)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("query %q: %v", query, err)
		}
		return out
	}
	registry := func() []string {
		t.Helper()
		return slugs(`SELECT owner || '/' || repo || '#' || external_id FROM repositories WHERE org_id = $1 ORDER BY 1`)
	}
	tracked := func() []string {
		t.Helper()
		return slugs(`
			SELECT DISTINCT r.owner || '/' || r.repo
			FROM team_github_repos g
			JOIN repositories r ON r.id = g.repository_id
			WHERE r.org_id = $1
			ORDER BY 1`)
	}

	// register runs the registration under user's claims and returns the row id.
	register := func(user, slug, githubID string) string {
		t.Helper()
		var id string
		if err := stores.Tx.WithTx(ctx, orgA, user, func(tx db.TxStores) error {
			ref := domain.RepoRefFromSlug(dbtest.TestGitHubHost, slug)
			ref.ExternalID = githubID
			row, err := tx.Repos.FindOrCreateByExternalID(ctx, orgA, ref)
			id = row.ID
			return err
		}); err != nil {
			t.Fatalf("FindOrCreateByExternalID(%s, %s) as %s: %v", slug, githubID, user, err)
		}
		return id
	}
	save := func(user, team string, ids ...string) error {
		t.Helper()
		return stores.Tx.WithTx(ctx, orgA, user, func(tx db.TxStores) error {
			return tx.TeamGitHubRepos.ReplaceForTeam(ctx, orgA, team, dbtest.TestGitHubHost, ids)
		})
	}

	// alice (teamA admin) registers and tracks two repos.
	api := register(alice, "acme/api", "101")
	web := register(alice, "acme/web", "102")
	if err := save(alice, teamA, api, web); err != nil {
		t.Fatalf("alice saves teamA: %v", err)
	}
	if got := tracked(); !pgStrsEqual(got, []string{"acme/api", "acme/web"}) {
		t.Fatalf("after alice: tracked = %v, want [acme/api acme/web]", got)
	}
	if got := registry(); !pgStrsEqual(got, []string{"acme/api#101", "acme/web#102"}) {
		t.Fatalf("after alice: repositories = %v, want both carrying their GitHub id", got)
	}

	// carol (teamB admin) registers acme/web — under the name GitHub gives it
	// now, which TF has not applied — and acme/infra. The shared repository is
	// one row, found by its id.
	if got := register(carol, "acme/website", "102"); got != web {
		t.Fatalf("carol's registration of GitHub id 102 = row %s, want alice's row %s", got, web)
	}
	infra := register(carol, "acme/infra", "103")
	if err := save(carol, teamB, web, infra); err != nil {
		t.Fatalf("carol saves teamB: %v", err)
	}
	if got := tracked(); !pgStrsEqual(got, []string{"acme/api", "acme/infra", "acme/web"}) {
		t.Fatalf("after carol: tracked = %v, want all three", got)
	}

	// alice drops acme/api. No other team tracks it, so it leaves the tracked
	// set; acme/web stays because carol still tracks it.
	if err := save(alice, teamA, web); err != nil {
		t.Fatalf("alice shrinks teamA: %v", err)
	}
	if got := tracked(); !pgStrsEqual(got, []string{"acme/infra", "acme/web"}) {
		t.Fatalf("after alice shrink: tracked = %v, want [acme/infra acme/web] (acme/api untracked, acme/web survives via teamB)", got)
	}
	// The registry does not shrink with it. Untracking is a statement about
	// what this team works on, not a claim that the repository never existed.
	if got := registry(); !pgStrsEqual(got, []string{"acme/api#101", "acme/infra#103", "acme/web#102"}) {
		t.Fatalf("after alice shrink: repositories = %v, want all three — an untracked repository keeps its registry row", got)
	}

	// alice administers teamA only, so RLS refuses her write to teamB's set,
	// and the refusal rolls back whole.
	if err := save(alice, teamB, api); err == nil {
		t.Fatal("alice wrote teamB's tracked set; RLS should refuse a team admin writing another team")
	}
	if got := tracked(); !pgStrsEqual(got, []string{"acme/infra", "acme/web"}) {
		t.Fatalf("after alice's refused write: tracked = %v, want it unchanged", got)
	}
}

// TestTeamGitHubRepos_Postgres_TracksRepoViewerScoped_RLS pins the
// per-repo, app-pool write/read gate (TFAC-559): under real RLS, a caller
// only "tracks" a repo if one of THEIR teams tracks it — a sibling team
// tracking the same-named repo in the org doesn't count. TracksRepoSystem
// (a specific team, admin pool) is unaffected; this exercises the viewer-
// scoped sibling.
func TestTeamGitHubRepos_Postgres_TracksRepoViewerScoped_RLS(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	orgA, alice, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	teamB := pgtest.SeedTeam(t, h, orgA, "team-b")
	bob := pgtest.SeedUser(t, h, "bob")
	pgtest.AddOrgMember(t, h, bob, orgA, teamB, "member", "member")

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	if err := stores.Tx.WithTx(ctx, orgA, alice, func(tx db.TxStores) error {
		return dbtest.TrackRepos(ctx, tx.Repos, tx.TeamGitHubRepos, orgA, teamA, dbtest.TestGitHubHost, []domain.TeamGitHubRepo{{Owner: "Acme", Repo: "api"}})
	}); err != nil {
		t.Fatalf("track acme/api for teamA: %v", err)
	}

	tracksAs := func(userID, owner, repo string) bool {
		t.Helper()
		var got bool
		if err := stores.Tx.WithTx(ctx, orgA, userID, func(tx db.TxStores) error {
			var e error
			got, e = tx.TeamGitHubRepos.TracksRepoViewerScoped(ctx, orgA, dbtest.TestGitHubHost, owner, repo)
			return e
		}); err != nil {
			t.Fatalf("TracksRepoViewerScoped(%s, %s/%s): %v", userID, owner, repo, err)
		}
		return got
	}

	if !tracksAs(alice, "acme", "api") {
		t.Error("alice's team tracks acme/api; want true")
	}
	if !tracksAs(alice, "ACME", "API") {
		t.Error("match should be case-insensitive")
	}
	if tracksAs(bob, "acme", "api") {
		t.Error("bob's team does not track acme/api; want false even though it exists org-wide")
	}
	if tracksAs(alice, "acme", "ghost") {
		t.Error("acme/ghost is not tracked by anyone; want false")
	}
}

// TestTeamGitHubRepos_Postgres_TracksRepoViewerAdminScoped_RLS pins the
// mutation gate for org-wide repo configuration: seeing a repo (membership)
// and being allowed to change it (team admin) are separate answers. Repository
// rows carry no team_id, so a member of one tracking team writing the
// row changes behaviour for every tracking team — hence the narrower
// predicate.
func TestTeamGitHubRepos_Postgres_TracksRepoViewerAdminScoped_RLS(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	orgA, alice, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	// bob is a plain member of the SAME team alice admins: he sees
	// acme/api, but must not be able to write it.
	bob := pgtest.SeedUser(t, h, "bob")
	pgtest.AddOrgMember(t, h, bob, orgA, teamA, "member", "member")
	// carol admins a different team that tracks nothing — team admin
	// somewhere in the org is not team admin *of a tracking team*.
	teamB := pgtest.SeedTeam(t, h, orgA, "team-b")
	carol := pgtest.SeedUser(t, h, "carol")
	pgtest.AddOrgMember(t, h, carol, orgA, teamB, "member", "admin")

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	if err := stores.Tx.WithTx(ctx, orgA, alice, func(tx db.TxStores) error {
		return dbtest.TrackRepos(ctx, tx.Repos, tx.TeamGitHubRepos, orgA, teamA, dbtest.TestGitHubHost, []domain.TeamGitHubRepo{{Owner: "Acme", Repo: "api"}})
	}); err != nil {
		t.Fatalf("track acme/api for teamA: %v", err)
	}

	adminTracksAs := func(userID, owner, repo string) bool {
		t.Helper()
		var got bool
		if err := stores.Tx.WithTx(ctx, orgA, userID, func(tx db.TxStores) error {
			var e error
			got, e = tx.TeamGitHubRepos.TracksRepoViewerAdminScoped(ctx, orgA, dbtest.TestGitHubHost, owner, repo)
			return e
		}); err != nil {
			t.Fatalf("TracksRepoViewerAdminScoped(%s, %s/%s): %v", userID, owner, repo, err)
		}
		return got
	}
	memberTracksAs := func(userID, owner, repo string) bool {
		t.Helper()
		var got bool
		if err := stores.Tx.WithTx(ctx, orgA, userID, func(tx db.TxStores) error {
			var e error
			got, e = tx.TeamGitHubRepos.TracksRepoViewerScoped(ctx, orgA, dbtest.TestGitHubHost, owner, repo)
			return e
		}); err != nil {
			t.Fatalf("TracksRepoViewerScoped(%s, %s/%s): %v", userID, owner, repo, err)
		}
		return got
	}

	if !adminTracksAs(alice, "acme", "api") {
		t.Error("alice admins the team tracking acme/api; want true")
	}
	if !adminTracksAs(alice, "ACME", "API") {
		t.Error("match should be case-insensitive")
	}
	if adminTracksAs(alice, "acme", "ghost") {
		t.Error("acme/ghost is tracked by nobody; want false even for a team admin")
	}
	// The split this gate exists for: bob reads the repo, bob cannot write it.
	if !memberTracksAs(bob, "acme", "api") {
		t.Error("bob is a member of the team tracking acme/api; the read gate should pass")
	}
	if adminTracksAs(bob, "acme", "api") {
		t.Error("bob is a plain member, not a team admin; the write gate must reject him")
	}
	if adminTracksAs(carol, "acme", "api") {
		t.Error("carol admins teamB, which doesn't track acme/api; want false")
	}
}

func pgStrsEqual(a, b []string) bool {
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

// A registration under a team admin's claims mints a complete identity row
// carrying the GitHub id from the start, and a bare row TF minted by name for
// the same repository is the row the registration adopts rather than a second.
func TestRepositories_Postgres_FindOrCreateByExternalID_UnderClaims(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	orgA, alice, _ := pgtest.SeedOrgWithUser(t, h, "alice")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)

	register := func(slug, githubID string) domain.Repository {
		t.Helper()
		var out domain.Repository
		if err := stores.Tx.WithTx(ctx, orgA, alice, func(tx db.TxStores) error {
			ref := domain.RepoRefFromSlug(dbtest.TestGitHubHost, slug)
			ref.ExternalID = githubID
			var err error
			out, err = tx.Repos.FindOrCreateByExternalID(ctx, orgA, ref)
			return err
		}); err != nil {
			t.Fatalf("FindOrCreateByExternalID(%s, %s): %v", slug, githubID, err)
		}
		return out
	}

	created := register("acme/api", "201")
	if created.ExternalID != "201" || created.Source != domain.RepoSourceGitHub || created.ProfiledAt != nil {
		t.Errorf("created row = %+v, want a bare github row carrying external id 201", created)
	}

	// An entity create mints a bare row by name; registering the repository
	// it names writes the id onto that row.
	bare, err := stores.Repos.FindOrCreateByRefSystem(ctx, orgA, domain.RepoRefFromSlug(dbtest.TestGitHubHost, "Acme/Web"))
	if err != nil {
		t.Fatalf("mint bare row: %v", err)
	}
	adopted := register("acme/web", "202")
	if adopted.ID != bare.ID || adopted.ExternalID != "202" || adopted.Slug() != "Acme/Web" {
		t.Errorf("adopted = %+v, want bare row %s carrying 202 under its stored casing", adopted, bare.ID)
	}

	var rows int
	if err := h.AdminDB.QueryRow(`SELECT count(*) FROM repositories WHERE org_id = $1`, orgA).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 2 {
		t.Fatalf("repositories rows = %d, want 2", rows)
	}
}

// TestRepositories_Postgres_FindOrCreateByExternalID_AdoptionYieldsToAConcurrentID
// covers the one registration race that surfaces as an error rather than an
// empty result: the id is written onto a bare row while another transaction
// holds the same id, uncommitted, on a row under a different name. The UPDATE
// waits on that row's index entry and, once it commits, fails the unique
// index. The registration has to answer with the committed row, which is the
// repository that id names, and leave the bare row as it was.
func TestRepositories_Postgres_FindOrCreateByExternalID_AdoptionYieldsToAConcurrentID(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	orgA, alice, _ := pgtest.SeedOrgWithUser(t, h, "alice")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)

	bare, err := stores.Repos.FindOrCreateByRefSystem(ctx, orgA, domain.RepoRefFromSlug(dbtest.TestGitHubHost, "acme/web"))
	if err != nil {
		t.Fatalf("mint bare row: %v", err)
	}

	other, err := h.AdminDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin the concurrent writer: %v", err)
	}
	defer func() { _ = other.Rollback() }()
	var heldID string
	if err := other.QueryRowContext(ctx, `
		INSERT INTO repositories (org_id, source, host, owner, repo, external_id)
		VALUES ($1, 'github', $2, 'acme', 'web-next', '301')
		RETURNING id::text
	`, orgA, dbtest.TestGitHubHost).Scan(&heldID); err != nil {
		t.Fatalf("hold id 301 on another row: %v", err)
	}

	type result struct {
		row domain.Repository
		err error
	}
	done := make(chan result, 1)
	go func() {
		var row domain.Repository
		err := stores.Tx.WithTx(ctx, orgA, alice, func(tx db.TxStores) error {
			ref := domain.RepoRefFromSlug(dbtest.TestGitHubHost, "acme/web")
			ref.ExternalID = "301"
			var err error
			row, err = tx.Repos.FindOrCreateByExternalID(ctx, orgA, ref)
			return err
		})
		done <- result{row, err}
	}()

	// The registration is the only other session; wait until its UPDATE is
	// blocked on the uncommitted index entry, or the race is not under test.
	blocked := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		var waiting int
		if err := h.AdminDB.QueryRowContext(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND datname = current_database()
		`).Scan(&waiting); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		if waiting > 0 {
			blocked = true
			break
		}
	}
	if !blocked {
		t.Fatal("the registration never waited on the concurrent writer's row")
	}
	if err := other.Commit(); err != nil {
		t.Fatalf("commit the concurrent writer: %v", err)
	}

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("FindOrCreateByExternalID after the concurrent commit: %v", res.err)
		}
		if res.row.ID != heldID || res.row.ExternalID != "301" {
			t.Errorf("registered = %+v, want the committed row %s carrying 301", res.row, heldID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the registration never returned after the concurrent commit")
	}

	got, err := stores.Repos.GetSystem(ctx, orgA, bare.ID)
	if err != nil {
		t.Fatalf("read the bare row: %v", err)
	}
	if got.ExternalID != "" {
		t.Errorf("bare row after the registration carries %q, want no id", got.ExternalID)
	}
}

// TestTeamGitHubRepos_Postgres_ReplaceForTeam_RefusesAnotherOrgsRepository
// runs on the admin pool, where RLS hides nothing, so the store's own org
// filter is the only thing that can refuse a registry row of another org.
func TestTeamGitHubRepos_Postgres_ReplaceForTeam_RefusesAnotherOrgsRepository(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	orgA, _, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	orgB, _, _ := pgtest.SeedOrgWithUser(t, h, "bob")
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	foreign, err := stores.Repos.FindOrCreateByRefSystem(ctx, orgB, domain.RepoRefFromSlug(dbtest.TestGitHubHost, "acme/api"))
	if err != nil {
		t.Fatalf("register org B's repository: %v", err)
	}
	err = stores.TeamGitHubRepos.ReplaceForTeam(ctx, orgA, teamA, dbtest.TestGitHubHost, []string{foreign.ID})
	if !errors.Is(err, db.ErrNoSuchRepository) {
		t.Fatalf("ReplaceForTeam with org B's repository = %v, want db.ErrNoSuchRepository", err)
	}
	if got, err := stores.TeamGitHubRepos.ListForTeamSystem(ctx, teamA, dbtest.TestGitHubHost); err != nil || len(got) != 0 {
		t.Errorf("org A's tracked set after the refusal = %+v, %v; want none", got, err)
	}
}
