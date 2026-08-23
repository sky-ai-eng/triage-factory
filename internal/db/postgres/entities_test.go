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

	orgA, _ := seedPgEntityOrg(t, h)
	orgB, _ := seedPgEntityOrg(t, h)

	ctx := context.Background()
	ent, _, err := stores.Entities.FindOrCreate(ctx, orgA, "github", "owner/repo#cross", "pr", "T", "")
	if err != nil {
		t.Fatalf("seed entity in orgA: %v", err)
	}
	if _, err := stores.Entities.UpdateDescription(ctx, orgA, ent.ID, "describes A"); err != nil {
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

	// UpdateTitle cross-org returns sql.ErrNoRows (entity exists in orgA
	// but not orgB; the WHERE filter rejects the UPDATE, then the
	// existence probe also misses on org_id=$orgB).
	if _, err := stores.Entities.UpdateTitle(ctx, orgB, ent.ID, "renamed"); err != sql.ErrNoRows {
		t.Errorf("cross-org UpdateTitle err = %v, want sql.ErrNoRows", err)
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
		VALUES ($1, $2, '[{"id":"10000","name":"To Do"}]'::jsonb,
			'[{"id":"10001","name":"In Progress"}]'::jsonb, '{"id":"10001","name":"In Progress"}'::jsonb,
			'[{"id":"10002","name":"Done"}]'::jsonb, '{"id":"10002","name":"Done"}'::jsonb)
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

func newPgEntitySeeder(conn *sql.DB, orgID, userID string) dbtest.EntitySeeder {
	return dbtest.EntitySeeder{
		Team: func(t *testing.T, name string) string {
			t.Helper()
			id := uuid.New().String()
			if _, err := conn.Exec(
				`INSERT INTO teams (id, org_id, slug, name) VALUES ($1, $2, $3, $4)`,
				id, orgID, "seed-"+id[:8], name,
			); err != nil {
				t.Fatalf("seed team %s: %v", name, err)
			}
			return id
		},
	}
}

// TestEntityStore_Postgres_ClearSnapshotsForSourceIsOrgScoped pins the
// isolation the shared conformance suite structurally cannot: SQLite is N=1 and
// asserts a single sentinel org, so the only place cross-tenant scoping can be
// checked is here.
//
// It matters more than most cross-org reads because this one is a WRITE, and it
// runs on the admin pool with RLS bypassed — the WHERE clause is the entire
// boundary. A missing org_id predicate would blank every tenant's snapshots the
// moment one org admin turned a source off, and the damage is silent: the next
// cycle re-seeds quietly by design, so nobody gets an error, they just lose the
// diff baseline for a source they never touched.
func TestEntityStore_Postgres_ClearSnapshotsForSourceIsOrgScoped(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgA, _ := seedPgEntityOrg(t, h)
	orgB, _ := seedPgEntityOrg(t, h)

	entA, _, err := stores.Entities.FindOrCreate(ctx, orgA, "github", "owner/repo#clr-a", "pr", "A", "")
	if err != nil {
		t.Fatalf("seed orgA entity: %v", err)
	}
	entB, _, err := stores.Entities.FindOrCreate(ctx, orgB, "github", "owner/repo#clr-b", "pr", "B", "")
	if err != nil {
		t.Fatalf("seed orgB entity: %v", err)
	}
	for org, id := range map[string]string{orgA: entA.ID, orgB: entB.ID} {
		if _, err := stores.Entities.UpdateSnapshot(ctx, org, id, `{"number":7}`); err != nil {
			t.Fatalf("seed snapshot: %v", err)
		}
	}
	beforeB, err := stores.Entities.Get(ctx, orgB, entB.ID)
	if err != nil || beforeB == nil {
		t.Fatalf("read orgB entity: %v", err)
	}

	cleared, err := stores.Entities.ClearSnapshotsForSourceSystem(ctx, orgA, "github")
	if err != nil {
		t.Fatalf("ClearSnapshotsForSourceSystem: %v", err)
	}
	if cleared != 1 {
		t.Errorf("cleared %d rows, want 1 — only orgA's", cleared)
	}

	gotB, err := stores.Entities.Get(ctx, orgB, entB.ID)
	if err != nil || gotB == nil {
		t.Fatalf("re-read orgB entity: %v", err)
	}
	if gotB.SnapshotJSON == "" {
		t.Error("orgA turning a source off cleared orgB's snapshot")
	}
	if gotB.PollSeq != beforeB.PollSeq {
		t.Errorf("orgB poll_seq moved %d → %d; an untouched tenant's rows must not be written at all",
			beforeB.PollSeq, gotB.PollSeq)
	}
}
