package postgres_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestEntityStore_Postgres runs the shared conformance suite against
// the Postgres EntityStore impl. Wires both pools against AdminDB
// (BYPASSRLS) so the suite reads behavior in isolation from the
// auth path; the dedicated cross-org leakage + RLS subtests below
// exercise the org_id filter through the app pool.
func TestEntityStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	dbtest.RunEntityStoreConformance(t, func(t *testing.T) (db.EntityStore, string, dbtest.EntitySeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID := seedPgEntityOrg(t, h)
		seed := newPgEntitySeeder(h.AdminDB, orgID, userID)
		return stores.Entities, orgID, seed
	})
}

// TestEntityStore_Postgres_CrossOrgLeakage verifies the org_id
// defense-in-depth filter on every read path. RLS would normally
// catch a wrong-org read too, but the conformance harness runs
// against AdminDB (BYPASSRLS); this test asserts the WHERE clauses
// themselves reject cross-org access without relying on RLS.
func TestEntityStore_Postgres_CrossOrgLeakage(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	orgA, userA := seedPgEntityOrg(t, h)
	orgB, _ := seedPgEntityOrg(t, h)
	pidA := seedPgEntityProject(t, h, orgA, userA, "A-proj")

	ctx := context.Background()
	ent, _, err := stores.Entities.FindOrCreate(ctx, orgA, "github", "owner/repo#cross", "pr", "T", "")
	if err != nil {
		t.Fatalf("seed entity in orgA: %v", err)
	}
	if err := stores.Entities.AssignProject(ctx, orgA, ent.ID, &pidA, "rationale"); err != nil {
		t.Fatalf("assign in orgA: %v", err)
	}
	if err := stores.Entities.UpdateDescription(ctx, orgA, ent.ID, "describes A"); err != nil {
		t.Fatalf("describe in orgA: %v", err)
	}

	// Get(orgB, ent.ID) must return nil — even though the id exists, it
	// belongs to orgA.
	gotB, err := stores.Entities.Get(ctx, orgB, ent.ID)
	if err != nil {
		t.Fatalf("Get(orgB): %v", err)
	}
	if gotB != nil {
		t.Errorf("cross-org Get leaked entity: %+v", gotB)
	}

	// GetBySource cross-org returns nil.
	gotSrc, err := stores.Entities.GetBySource(ctx, orgB, "github", "owner/repo#cross")
	if err != nil {
		t.Fatalf("GetBySource(orgB): %v", err)
	}
	if gotSrc != nil {
		t.Errorf("cross-org GetBySource leaked entity: %+v", gotSrc)
	}

	// Descriptions cross-org skips the row.
	descs, err := stores.Entities.Descriptions(ctx, orgB, []string{ent.ID})
	if err != nil {
		t.Fatalf("Descriptions(orgB): %v", err)
	}
	if _, ok := descs[ent.ID]; ok {
		t.Errorf("cross-org Descriptions leaked entity %s", ent.ID)
	}

	// ListActive in orgB must not contain the orgA entity.
	activeB, err := stores.Entities.ListActive(ctx, orgB, "github")
	if err != nil {
		t.Fatalf("ListActive(orgB): %v", err)
	}
	for _, e := range activeB {
		if e.ID == ent.ID {
			t.Errorf("cross-org ListActive leaked entity %s", ent.ID)
		}
	}

	// AssignProject cross-org returns sql.ErrNoRows (entity exists in
	// orgA but not orgB; the WHERE filter rejects the UPDATE, then
	// the existence probe also misses on org_id=$orgB).
	if err := stores.Entities.AssignProject(ctx, orgB, ent.ID, &pidA, "r"); err != sql.ErrNoRows {
		t.Errorf("cross-org AssignProject err = %v, want sql.ErrNoRows", err)
	}
}

// TestEntityStore_Postgres_CrossOrgRLSDenied pins the production RLS
// layer for entities. Where CrossOrgLeakage above wires both pools
// against AdminDB to prove the defense-in-depth WHERE-clause filter
// is intact, this test runs the store through the app pool under
// tf_app with real JWT claims so the actual entities_select /
// entities_modify policies are exercised. Same-org reads succeed;
// cross-org reads are silently filtered (USING); cross-org
// FindOrCreate raises 42501 from entities_modify WITH CHECK.
func TestEntityStore_Postgres_CrossOrgRLSDenied(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, alice := seedPgEntityOrg(t, h)
	orgB, bob := seedPgEntityOrg(t, h)
	_ = bob

	// Seed an entity in orgA via admin so the row exists.
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()
	entA, _, err := stores.Entities.FindOrCreateSystem(ctx, orgA, "github", "owner/repo#rls", "pr", "RLS Entity", "https://example/rls")
	if err != nil {
		t.Fatalf("seed entity in orgA: %v", err)
	}

	t.Run("same_org_user_can_read", func(t *testing.T) {
		err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
			got, err := pgstore.NewForTx(tx, pgtest.SecretKey).Entities.Get(ctx, orgA, entA.ID)
			if err != nil {
				return fmt.Errorf("Get: %w", err)
			}
			if got == nil {
				t.Errorf("alice Get(orgA, entA) returned nil; same-org RLS USING filter wrongly excluded the row")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("alice path: %v", err)
		}
	})

	t.Run("cross_org_read_filtered", func(t *testing.T) {
		err := h.WithUser(t, bob, orgB, func(tx *sql.Tx) error {
			got, err := pgstore.NewForTx(tx, pgtest.SecretKey).Entities.Get(ctx, orgA, entA.ID)
			if err != nil {
				return fmt.Errorf("Get: %w", err)
			}
			if got != nil {
				t.Errorf("bob Get(orgA, entA) returned %+v; RLS USING filter leaked orgA's entity to orgB", got)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("bob read path: %v", err)
		}
	})

	t.Run("cross_org_write_denied", func(t *testing.T) {
		// bob's claims point at orgB; FindOrCreate against orgA
		// would land a row with org_id=orgA. entities_modify
		// WITH CHECK requires org_id = tf.current_org_id(), so 42501
		// is the expected outcome.
		err := h.WithUser(t, bob, orgB, func(tx *sql.Tx) error {
			_, _, e := pgstore.NewForTx(tx, pgtest.SecretKey).Entities.FindOrCreate(
				ctx, orgA, "github", "owner/repo#rls-write", "pr", "Cross-org write", "",
			)
			return e
		})
		pgtest.AssertRLSViolation(t, err)
	})
}

// seedPgJiraRule inserts a fully-configured jira_project_status_rules
// row attaching projectKey to teamID. The members/canonical fields
// satisfy the jpsr_*_populated CHECK constraints; the test only cares
// that the row exists so the membership semi-join can match on
// project_key.
func seedPgJiraRule(t *testing.T, h *pgtest.Harness, teamID, projectKey string) {
	t.Helper()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO jira_project_status_rules
			(team_id, project_key, pickup_members, in_progress_members, in_progress_canonical, done_members, done_canonical)
		VALUES ($1, $2, ARRAY['To Do'], ARRAY['In Progress'], 'In Progress', ARRAY['Done'], 'Done')
	`, teamID, projectKey); err != nil {
		t.Fatalf("seed jira rule %s for team %s: %v", projectKey, teamID, err)
	}
}

// TestEntityStore_Postgres_ListActiveJiraTeamScoped pins the multi-mode
// discovery-read scope under the admin pool: an active Jira entity rides
// the deck iff some team in its org has attached its project (a
// jira_project_status_rules row for the project key exists). Entities
// whose project no team configured are excluded, GitHub entities never
// appear, and a same-key rule in *another* org doesn't pull the entity
// in (the teams-join org binding holds even with RLS bypassed). The
// per-team RLS narrowing is exercised separately in the _RLS test.
func TestEntityStore_Postgres_ListActiveJiraTeamScoped(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, ownerID := seedPgEntityOrg(t, h)
	teamA := firstTeamForOrg(t, h, orgID)
	bob := seedPgMember(t, h, orgID, "bob", "member")
	teamB := seedPgDefaultTeam(t, h, orgID, bob)

	// teamA tracks AAA, teamB tracks BBB; CCC is attached by no team.
	seedPgJiraRule(t, h, teamA, "AAA")
	seedPgJiraRule(t, h, teamB, "BBB")

	// A different org tracks CCC — must not leak CCC-3 into orgID's deck.
	otherOrg, otherOwner := seedPgEntityOrg(t, h)
	otherTeam := firstTeamForOrg(t, h, otherOrg)
	_ = otherOwner
	seedPgJiraRule(t, h, otherTeam, "CCC")

	inA, _, err := stores.Entities.FindOrCreateSystem(ctx, orgID, "jira", "AAA-1", "issue", "A", "")
	if err != nil {
		t.Fatalf("seed AAA-1: %v", err)
	}
	inB, _, err := stores.Entities.FindOrCreateSystem(ctx, orgID, "jira", "BBB-2", "issue", "B", "")
	if err != nil {
		t.Fatalf("seed BBB-2: %v", err)
	}
	unattached, _, err := stores.Entities.FindOrCreateSystem(ctx, orgID, "jira", "CCC-3", "issue", "C", "")
	if err != nil {
		t.Fatalf("seed CCC-3: %v", err)
	}
	// GitHub entity whose "project key" prefix could coincidentally match
	// — the source='jira' filter must keep it out regardless.
	if _, _, err := stores.Entities.FindOrCreateSystem(ctx, orgID, "github", "AAA/repo#9", "pr", "GH", ""); err != nil {
		t.Fatalf("seed github: %v", err)
	}
	_ = ownerID

	got, err := stores.Entities.ListActiveJiraTeamScoped(ctx, orgID, "")
	if err != nil {
		t.Fatalf("ListActiveJiraTeamScoped: %v", err)
	}
	ids := map[string]bool{}
	for _, e := range got {
		if e.Source != "jira" {
			t.Errorf("non-jira entity %s leaked into jira deck", e.ID)
		}
		ids[e.ID] = true
	}
	if !ids[inA.ID] {
		t.Errorf("AAA-1 missing — teamA attached AAA, it should ride the deck")
	}
	if !ids[inB.ID] {
		t.Errorf("BBB-2 missing — teamB attached BBB, it should ride the deck")
	}
	if ids[unattached.ID] {
		t.Errorf("CCC-3 leaked — no team in this org attached CCC (another org's rule must not count)")
	}

	// Per-page team filter (the carry-over deck's selector): narrowing to
	// teamA returns only teamA's tracked projects (AAA-1), not teamB's
	// (BBB-2). Without this the deck switched to one team would still
	// surface — and let you claim — another team's tickets. (Codex P2)
	onlyA, err := stores.Entities.ListActiveJiraTeamScoped(ctx, orgID, teamA)
	if err != nil {
		t.Fatalf("ListActiveJiraTeamScoped(teamA): %v", err)
	}
	aIDs := map[string]bool{}
	for _, e := range onlyA {
		aIDs[e.ID] = true
	}
	if !aIDs[inA.ID] {
		t.Errorf("filter=teamA dropped AAA-1, which teamA tracks")
	}
	if aIDs[inB.ID] {
		t.Errorf("filter=teamA leaked BBB-2 (teamB's project); the deck filter must narrow to the selected team")
	}
}

// TestEntityStore_Postgres_ListActiveJiraTeamScoped_RLS proves the
// per-team narrowing the production app pool gets for free: a viewer
// only sees Jira entities for projects attached to teams they belong
// to. Where the admin-pool test above proves the org-scoped semi-join,
// this drives the read through tf_app with real JWT claims so the
// jira_rules_select policy (team-membership-gated) does the team
// scoping. alice (teamA only) sees AAA but not BBB; bob (teamB only)
// sees the mirror.
func TestEntityStore_Postgres_ListActiveJiraTeamScoped_RLS(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	orgID, alice := seedPgEntityOrg(t, h)
	teamA := firstTeamForOrg(t, h, orgID)
	bob := seedPgMember(t, h, orgID, "bob", "member")
	teamB := seedPgDefaultTeam(t, h, orgID, bob)

	seedPgJiraRule(t, h, teamA, "AAA")
	seedPgJiraRule(t, h, teamB, "BBB")

	admin := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	entA, _, err := admin.Entities.FindOrCreateSystem(ctx, orgID, "jira", "AAA-1", "issue", "A", "")
	if err != nil {
		t.Fatalf("seed AAA-1: %v", err)
	}
	entB, _, err := admin.Entities.FindOrCreateSystem(ctx, orgID, "jira", "BBB-2", "issue", "B", "")
	if err != nil {
		t.Fatalf("seed BBB-2: %v", err)
	}

	scopedFor := func(t *testing.T, userID string) map[string]bool {
		t.Helper()
		ids := map[string]bool{}
		err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
			rows, e := pgstore.NewForTx(tx, pgtest.SecretKey).Entities.ListActiveJiraTeamScoped(ctx, orgID, "")
			if e != nil {
				return e
			}
			for _, r := range rows {
				ids[r.ID] = true
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scoped read for %s: %v", userID, err)
		}
		return ids
	}

	aliceSees := scopedFor(t, alice)
	if !aliceSees[entA.ID] {
		t.Errorf("alice (teamA) can't see AAA-1 — her own team's project is hidden")
	}
	if aliceSees[entB.ID] {
		t.Errorf("alice (teamA) saw BBB-2 — teamB's Jira project leaked across the team boundary")
	}

	bobSees := scopedFor(t, bob)
	if !bobSees[entB.ID] {
		t.Errorf("bob (teamB) can't see BBB-2 — his own team's project is hidden")
	}
	if bobSees[entA.ID] {
		t.Errorf("bob (teamB) saw AAA-1 — teamA's Jira project leaked across the team boundary")
	}
}

// TestEntityStore_Postgres_ClassificationStatusSystem_NoClaims pins the
// fix end-to-end against Postgres. The classification WaitFor
// runs in the background spawner with NO request.jwt.claims, so its read
// must route through the admin pool (the System variant) — the app pool
// (tf_app) would RLS-deny it (current_org_id() is NULL). This drives
// ClassificationStatusSystem with no WithUser wrap (the claims-free
// background-caller condition) and asserts:
//
//   - the read reaches the row at all (the original bug's `?` placeholder
//     would raise 42601 here, and a "fix" that re-routed onto the app
//     pool would be RLS-filtered into a phantom miss);
//   - it keys on classified_at, so a below-threshold entity (classified,
//     no project) reports classified;
//   - org_id defense-in-depth: a different org never observes the row.
//
// The SQLite-only wait_test.go cannot catch either failure mode — `?` is
// valid there and local mode has no RLS.
func TestEntityStore_Postgres_ClassificationStatusSystem_NoClaims(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _ := seedPgEntityOrg(t, h)
	ctx := context.Background()

	// q = admin pool, app = app pool — exactly the production spawner
	// wiring (System reads hit admin). No WithUser wrap anywhere below =
	// the claims-free background caller the bug report describes.
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)

	ent, _, err := stores.Entities.FindOrCreateSystem(ctx, orgID, "github", "owner/repo#cs", "pr", "T", "")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}

	// Fresh: classified_at NULL → not classified, exists. A `?`-placeholder
	// regression would error here instead of returning cleanly.
	classified, exists, err := stores.Entities.ClassificationStatusSystem(ctx, orgID, ent.ID)
	if err != nil {
		t.Fatalf("ClassificationStatusSystem(fresh, no claims): %v", err)
	}
	if classified || !exists {
		t.Errorf("fresh entity: classified=%v exists=%v, want false/true", classified, exists)
	}

	// Below-threshold classify: project_id stays NULL, classified_at set.
	if err := stores.Entities.AssignProjectSystem(ctx, orgID, ent.ID, nil, ""); err != nil {
		t.Fatalf("AssignProjectSystem(nil): %v", err)
	}
	classified, exists, err = stores.Entities.ClassificationStatusSystem(ctx, orgID, ent.ID)
	if err != nil {
		t.Fatalf("ClassificationStatusSystem(classified, no claims): %v", err)
	}
	if !classified || !exists {
		t.Errorf("classified entity: classified=%v exists=%v, want true/true (keys on classified_at, not project_id)", classified, exists)
	}

	// Cross-org isolation: a different org must not observe this row, even
	// through the BYPASSRLS admin pool — the org_id WHERE filter is the
	// defense-in-depth backstop.
	otherOrg, _ := seedPgEntityOrg(t, h)
	classified, exists, err = stores.Entities.ClassificationStatusSystem(ctx, otherOrg, ent.ID)
	if err != nil {
		t.Fatalf("ClassificationStatusSystem(cross-org): %v", err)
	}
	if classified || exists {
		t.Errorf("cross-org read: classified=%v exists=%v, want false/false", classified, exists)
	}
}

// TestEntityStore_Postgres_UpdateSnapshotCASSystem_StaleMissIsNoOp pins the
// TFAC-579 CAS contract directly: a write against a stale (already-bumped)
// poll_seq reports ok=false and leaves the row untouched — the straggler
// ex-leader case the CAS exists for.
func TestEntityStore_Postgres_UpdateSnapshotCASSystem_StaleMissIsNoOp(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, _ := seedPgEntityOrg(t, h)
	entity, _, err := stores.Entities.FindOrCreateSystem(ctx, orgID, "github", "owner/repo#cas1", "pr", "T", "")
	if err != nil {
		t.Fatalf("FindOrCreate: %v", err)
	}
	if entity.PollSeq != 0 {
		t.Fatalf("fresh entity poll_seq = %d, want 0", entity.PollSeq)
	}

	// First writer wins (poll_seq 0 -> 1).
	ok, err := stores.Entities.UpdateSnapshotCASSystem(ctx, orgID, entity.ID, `{"v":1}`, 0)
	if err != nil {
		t.Fatalf("first CAS: %v", err)
	}
	if !ok {
		t.Fatal("first CAS should succeed against a fresh row")
	}

	// A straggler still holding the STALE poll_seq=0 (e.g. an ex-leader's
	// in-flight poll cycle that read the entity before the winner's write)
	// must lose — ok=false, and the winner's snapshot must survive untouched.
	ok, err = stores.Entities.UpdateSnapshotCASSystem(ctx, orgID, entity.ID, `{"v":"stale-should-not-land"}`, 0)
	if err != nil {
		t.Fatalf("stale CAS: %v", err)
	}
	if ok {
		t.Fatal("stale CAS (poll_seq=0 after a winner already bumped it to 1) should report ok=false")
	}

	fresh, err := stores.Entities.GetSystem(ctx, orgID, entity.ID)
	if err != nil {
		t.Fatalf("GetSystem: %v", err)
	}
	// jsonb round-trips with Postgres's own whitespace, so compare parsed
	// values rather than the raw string.
	var got struct {
		V json.Number `json:"v"`
	}
	if err := json.Unmarshal([]byte(fresh.SnapshotJSON), &got); err != nil {
		t.Fatalf("unmarshal snapshot %q: %v", fresh.SnapshotJSON, err)
	}
	if got.V.String() != "1" {
		t.Errorf("snapshot v = %q, want the winner's 1 — stale write must not land", got.V.String())
	}
	if fresh.PollSeq != 1 {
		t.Errorf("poll_seq = %d, want 1 (only the winning write bumps it)", fresh.PollSeq)
	}
}

// TestEntityStore_Postgres_SnapshotCAS_ConcurrentWriters_ExactlyOneWins is
// the pgtest acceptance criterion for TFAC-579 item 5: two concurrent
// snapshot writers (simulating a leader-failover overlap — the outgoing and
// incoming leader both mid-poll-cycle against the same entity) both read
// the entity's poll_seq, then race to CAS their own snapshot in. Exactly
// one must win; the loser must report ok=false (no error) rather than
// silently overwriting the winner's transition.
func TestEntityStore_Postgres_SnapshotCAS_ConcurrentWriters_ExactlyOneWins(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, _ := seedPgEntityOrg(t, h)
	entity, _, err := stores.Entities.FindOrCreateSystem(ctx, orgID, "github", "owner/repo#cas2", "pr", "T", "")
	if err != nil {
		t.Fatalf("FindOrCreate: %v", err)
	}

	const writers = 8
	var wg sync.WaitGroup
	oks := make([]bool, writers)
	errs := make([]error, writers)
	var ready sync.WaitGroup
	ready.Add(writers)
	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ready.Done()
			<-start
			// Every writer read the SAME poll_seq (0) before racing to
			// write — the leader-failover-overlap scenario.
			snap := fmt.Sprintf(`{"writer":%d}`, i)
			ok, err := stores.Entities.UpdateSnapshotCASSystem(ctx, orgID, entity.ID, snap, entity.PollSeq)
			oks[i], errs[i] = ok, err
		}(i)
	}
	ready.Wait()
	close(start)
	wg.Wait()

	winners := 0
	for i, ok := range oks {
		if errs[i] != nil {
			t.Fatalf("writer %d: unexpected error: %v", i, errs[i])
		}
		if ok {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("expected exactly 1 winning CAS, got %d", winners)
	}

	fresh, err := stores.Entities.GetSystem(ctx, orgID, entity.ID)
	if err != nil {
		t.Fatalf("GetSystem: %v", err)
	}
	if fresh.PollSeq != 1 {
		t.Errorf("poll_seq = %d, want 1 (one winning write, no lost/double transition)", fresh.PollSeq)
	}
}

func seedPgEntityOrg(t *testing.T, h *pgtest.Harness) (orgID, userID string) {
	t.Helper()
	orgID = uuid.New().String()
	userID = uuid.New().String()
	email := fmt.Sprintf("entity-%s@test.local", userID[:8])

	h.SeedAuthUser(t, userID, email)
	if _, err := h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`,
		userID, "Entity Conformance User",
	); err != nil {
		t.Fatalf("seed public.users: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "Entity Org "+orgID[:8], "ent-"+orgID[:8], userID,
	); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO org_memberships (org_id, user_id, role) VALUES ($1, $2, 'owner')`,
		orgID, userID,
	); err != nil {
		t.Fatalf("seed org_membership: %v", err)
	}
	seedPgDefaultTeam(t, h, orgID, userID)
	return orgID, userID
}

func seedPgEntityProject(t *testing.T, h *pgtest.Harness, orgID, userID, name string) string {
	t.Helper()
	id := uuid.New().String()
	teamID := firstTeamForOrg(t, h, orgID)
	if _, err := h.AdminDB.Exec(`
		INSERT INTO projects (id, org_id, creator_user_id, team_id, name, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, now(), now())
	`, id, orgID, userID, teamID, name); err != nil {
		t.Fatalf("seed project %s: %v", name, err)
	}
	return id
}

func newPgEntitySeeder(conn *sql.DB, orgID, userID string) dbtest.EntitySeeder {
	return dbtest.EntitySeeder{
		Project: func(t *testing.T, name string) string {
			t.Helper()
			id := uuid.New().String()
			// team_id must satisfy the projects_team_visibility_requires_team
			// CHECK — default visibility is 'team' so we need a real team
			// row. Use the org's default team seeded by seedPgEntityOrg.
			var teamID string
			if err := conn.QueryRow(
				`SELECT id FROM teams WHERE org_id = $1 ORDER BY created_at ASC LIMIT 1`,
				orgID,
			).Scan(&teamID); err != nil {
				t.Fatalf("lookup default team for org %s: %v", orgID, err)
			}
			if _, err := conn.Exec(`
				INSERT INTO projects (id, org_id, creator_user_id, team_id, name, created_at, updated_at)
				VALUES ($1, $2, $3, $4, $5, now(), now())
			`, id, orgID, userID, teamID, name); err != nil {
				t.Fatalf("seed project %s: %v", name, err)
			}
			return id
		},
	}
}
