package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestCuratorStore_Postgres_AttributesPerUser pins SKY-298: when Alice
// and Bob each post a message to the same project, the goroutine's
// per-turn SyntheticClaimsWithTx wrap stamps creator_user_id on every
// row that turn produces. We exercise the full write set the goroutine
// would emit (request create + mark running + insert message + insert
// pending_context + complete) for each user separately and verify the
// attribution by reading every row back from the admin pool.
func TestCuratorStore_Postgres_AttributesPerUser(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB)
	ctx := context.Background()

	orgID, alice, _ := seedPgProjectOrg(t, h)
	bob := seedAdditionalUser(t, h, orgID, "bob")

	// Seed the project via the admin pool — pgstore.New here wires
	// Projects against AppDB (tf_app) and a bare Create call from a
	// test context has no JWT claims set, so the projects_insert RLS
	// policy would reject it. The CuratorStore writes that follow
	// drive each through SyntheticClaimsWithTx with explicit claims,
	// which IS the production goroutine code path.
	projectID := seedPgEntityProject(t, h, orgID, alice, "shared")

	// Alice's turn: capture all the writes the goroutine would do.
	aliceReq, aliceMsg := runFullTurn(t, ctx, stores, orgID, alice, projectID)

	// Bob's turn: same project, different user.
	bobReq, bobMsg := runFullTurn(t, ctx, stores, orgID, bob, projectID)

	// Each request row must attribute to its respective user.
	gotAlice := readCuratorRequestCreator(t, h, aliceReq)
	if gotAlice != alice {
		t.Errorf("alice's request creator_user_id = %s, want %s", gotAlice, alice)
	}
	gotBob := readCuratorRequestCreator(t, h, bobReq)
	if gotBob != bob {
		t.Errorf("bob's request creator_user_id = %s, want %s", gotBob, bob)
	}

	// Curator messages: each row's creator_user_id should match.
	if got := readCuratorMessageCreator(t, h, aliceMsg); got != alice {
		t.Errorf("alice's message creator_user_id = %s, want %s", got, alice)
	}
	if got := readCuratorMessageCreator(t, h, bobMsg); got != bob {
		t.Errorf("bob's message creator_user_id = %s, want %s", got, bob)
	}
}

// TestCuratorStore_Postgres_CrossOrgLeakage pins the defense-in-depth
// org filter: even if Alice in orgA somehow submitted a malformed
// request scoped to orgB's project, the RLS policies + per-statement
// org_id binding would refuse the INSERT. We simulate this by trying
// to call CuratorStore methods under Alice's synthetic claims (orgA)
// against an orgB project id — the writes must not land in orgB.
func TestCuratorStore_Postgres_CrossOrgLeakage(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB)
	ctx := context.Background()

	orgA, alice, _ := seedPgProjectOrg(t, h)
	orgB, _, _ := seedPgProjectOrg(t, h)

	// Seed orgA's project via admin — pgstore.New here wires Projects
	// against AppDB (tf_app) and a bare Create call from a test
	// context has no JWT claims set. orgB doesn't need its own
	// project row; the count assertion below just verifies no
	// curator_requests rows land in orgB regardless.
	projectA := seedPgEntityProject(t, h, orgA, alice, "aye")

	// Alice creates a request in orgA — this must work.
	var goodRequestID string
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgA, alice, func(ts db.TxStores) error {
		id, err := ts.Curator.CreateRequest(ctx, orgA, projectA, alice, "hi from alice")
		if err != nil {
			return err
		}
		goodRequestID = id
		return nil
	}); err != nil {
		t.Fatalf("alice valid orgA write: %v", err)
	}
	if goodRequestID == "" {
		t.Fatal("expected non-empty request id from valid write")
	}

	// Cross-org attempt: alice with orgA claims, but binding orgB on
	// the call — the FK against projects(id, org_id) refuses the
	// insert because (projectA, orgB) doesn't exist as a project
	// row; RLS additionally fires on (org_id = current_org_id()).
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgA, alice, func(ts db.TxStores) error {
		_, err := ts.Curator.CreateRequest(ctx, orgB, projectA, alice, "cross-org attempt")
		return err
	}); err == nil {
		t.Error("expected cross-org CreateRequest to fail; got nil error")
	}

	// Verify orgB has zero curator_requests rows attributable to Alice.
	var count int
	if err := h.AdminDB.QueryRow(
		`SELECT COUNT(*) FROM curator_requests WHERE org_id = $1 AND creator_user_id = $2`,
		orgB, alice,
	).Scan(&count); err != nil {
		t.Fatalf("count cross-org rows: %v", err)
	}
	if count != 0 {
		t.Errorf("alice has %d rows in orgB after cross-org attempt, want 0", count)
	}
}

// TestCuratorStore_Postgres_CrossOrgRLSDenied pins the production RLS
// layer for curator_requests. Where TestCuratorStore_Postgres_CrossOrgLeakage
// above relies on the projects(id, org_id) FK to reject a cross-org
// write (the orgB caller passes a project id that doesn't exist in
// orgB), this test seeds a real project in orgA and proves the WITH
// CHECK predicate on curator_requests_modify fires BEFORE the FK
// resolution — bob's claims (org_id=orgB) immediately fail the
// org_id = tf.current_org_id() gate, so no orgB-side project seed is
// needed for the rejection to be RLS-attributable. Same-org reads
// succeed; cross-org reads are silently filtered (USING); cross-org
// CreateRequest raises 42501.
func TestCuratorStore_Postgres_CrossOrgRLSDenied(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB)
	ctx := context.Background()

	orgA, alice, _ := seedPgProjectOrg(t, h)
	orgB, bob, _ := seedPgProjectOrg(t, h)

	// Only orgA needs a project seeded — RLS WITH CHECK on the
	// insert fires before the project_id FK is evaluated, so an
	// absent orgB project doesn't change the test outcome. See the
	// outer docstring for the ordering rationale.
	projectA := seedPgEntityProject(t, h, orgA, alice, "rls-a")

	// Seed a request in orgA via alice's synthetic claims so the row
	// exists.
	var requestA string
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgA, alice, func(ts db.TxStores) error {
		id, err := ts.Curator.CreateRequest(ctx, orgA, projectA, alice, "alice rls seed")
		if err != nil {
			return err
		}
		requestA = id
		return nil
	}); err != nil {
		t.Fatalf("alice seed under synth claims: %v", err)
	}

	t.Run("same_org_user_can_read", func(t *testing.T) {
		err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
			got, err := pgstore.NewForTx(tx).Curator.GetRequest(ctx, orgA, requestA)
			if err != nil {
				return fmt.Errorf("GetRequest: %w", err)
			}
			if got == nil {
				t.Errorf("alice GetRequest(orgA, requestA) returned nil; same-org RLS USING filter wrongly excluded the row")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("alice path: %v", err)
		}
	})

	t.Run("cross_org_read_filtered", func(t *testing.T) {
		err := h.WithUser(t, bob, orgB, func(tx *sql.Tx) error {
			got, err := pgstore.NewForTx(tx).Curator.GetRequest(ctx, orgA, requestA)
			if err != nil {
				return fmt.Errorf("GetRequest: %w", err)
			}
			if got != nil {
				t.Errorf("bob GetRequest(orgA, requestA) returned %+v; RLS USING filter leaked orgA's request to orgB", got)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("bob read path: %v", err)
		}
	})

	t.Run("cross_org_write_denied", func(t *testing.T) {
		// bob's claims point at orgB; CreateRequest against orgA on
		// orgA's project would land a row with org_id=orgA. The
		// projectA FK target exists in orgA so the FK doesn't fire
		// first — curator_requests_modify WITH CHECK requires
		// org_id = tf.current_org_id() and raises 42501.
		err := h.WithUser(t, bob, orgB, func(tx *sql.Tx) error {
			_, e := pgstore.NewForTx(tx).Curator.CreateRequest(ctx, orgA, projectA, bob, "bob cross-org")
			return e
		})
		pgtest.AssertRLSViolation(t, err)
	})

	_ = orgB
}

// TestCuratorStore_Postgres_GetRequestRLS pins that GetRequest under
// claims for user X cannot read user Y's request row in the same org
// — curator_requests_select RLS gates on creator_user_id =
// tf.current_user_id(). The goroutine's MarkRequestRunning + GetRequest
// pair runs under the requesting user's claims, so this isolation
// matters when the curator runtime grows per-user sessions later
// (SKY-294 / per-user-vs-per-team direction).
func TestCuratorStore_Postgres_GetRequestRLS(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB)
	ctx := context.Background()

	orgID, alice, _ := seedPgProjectOrg(t, h)
	bob := seedAdditionalUser(t, h, orgID, "bob")

	// Admin-seed the project — see comment in AttributesPerUser.
	projectID := seedPgEntityProject(t, h, orgID, alice, "shared")

	// Alice creates a request.
	var aliceReq string
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, alice, func(ts db.TxStores) error {
		id, err := ts.Curator.CreateRequest(ctx, orgID, projectID, alice, "alice's message")
		if err != nil {
			return err
		}
		aliceReq = id
		return nil
	}); err != nil {
		t.Fatalf("alice create: %v", err)
	}

	// Bob reads with his own claims — RLS hides alice's row.
	var seenByBob *domain.CuratorRequest
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, bob, func(ts db.TxStores) error {
		r, err := ts.Curator.GetRequest(ctx, orgID, aliceReq)
		if err != nil {
			return err
		}
		seenByBob = r
		return nil
	}); err != nil {
		t.Fatalf("bob get: %v", err)
	}
	if seenByBob != nil {
		t.Errorf("bob's claims should hide alice's request row; got %+v", seenByBob)
	}

	// Alice reads with her own claims — she sees it.
	var seenByAlice *domain.CuratorRequest
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, alice, func(ts db.TxStores) error {
		r, err := ts.Curator.GetRequest(ctx, orgID, aliceReq)
		if err != nil {
			return err
		}
		seenByAlice = r
		return nil
	}); err != nil {
		t.Fatalf("alice get own: %v", err)
	}
	if seenByAlice == nil || seenByAlice.ID != aliceReq {
		t.Errorf("alice should see her own request row; got %+v", seenByAlice)
	}
	if seenByAlice != nil && seenByAlice.CreatorUserID != alice {
		t.Errorf("alice's row CreatorUserID = %s, want %s", seenByAlice.CreatorUserID, alice)
	}
}

// TestCuratorStore_Postgres_InsertPendingContext_Coalesces pins the
// ON CONFLICT DO NOTHING behavior against the partial-unique index on
// (project_id, curator_session_id, change_type) WHERE consumed_at IS
// NULL. A second insert against the same triple while the first row
// is still unconsumed must be a no-op so the earliest baseline wins.
func TestCuratorStore_Postgres_InsertPendingContext_Coalesces(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB)
	ctx := context.Background()

	orgID, alice, _ := seedPgProjectOrg(t, h)
	projectID := seedPgEntityProject(t, h, orgID, alice, "coalesce")
	setProjectSession(t, h, projectID, "sess-1")

	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, alice, func(ts db.TxStores) error {
		if err := ts.Curator.InsertPendingContext(ctx, orgID, projectID, "sess-1", domain.ChangeTypePinnedRepos, `["a/b"]`); err != nil {
			return err
		}
		return ts.Curator.InsertPendingContext(ctx, orgID, projectID, "sess-1", domain.ChangeTypePinnedRepos, `["c/d"]`)
	}); err != nil {
		t.Fatalf("inserts: %v", err)
	}

	var rows []domain.CuratorPendingContext
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, alice, func(ts db.TxStores) error {
		var e error
		rows, e = ts.Curator.ListPendingContext(ctx, orgID, projectID)
		return e
	}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row after coalesce, got %d (%+v)", len(rows), rows)
	}
	if rows[0].BaselineValue != `["a/b"]` {
		t.Errorf("baseline = %q, want %q (earliest wins)", rows[0].BaselineValue, `["a/b"]`)
	}
}

// TestCuratorStore_Postgres_ListPendingContext_MixedConsumed verifies the
// list returns both consumed and unconsumed rows ordered by created_at
// — the project-bundle export relies on this to inspect outstanding
// deltas regardless of dispatch state.
func TestCuratorStore_Postgres_ListPendingContext_MixedConsumed(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB)
	ctx := context.Background()

	orgID, alice, _ := seedPgProjectOrg(t, h)
	projectID := seedPgEntityProject(t, h, orgID, alice, "mixed")
	setProjectSession(t, h, projectID, "sess-1")

	// Seed pinned baseline + a request that will consume it.
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, alice, func(ts db.TxStores) error {
		return ts.Curator.InsertPendingContext(ctx, orgID, projectID, "sess-1", domain.ChangeTypePinnedRepos, `["x"]`)
	}); err != nil {
		t.Fatalf("seed pinned: %v", err)
	}
	var requestID string
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, alice, func(ts db.TxStores) error {
		id, err := ts.Curator.CreateRequest(ctx, orgID, projectID, alice, "consume me")
		if err != nil {
			return err
		}
		requestID = id
		_, _, err = ts.Curator.ConsumePendingContext(ctx, orgID, projectID, requestID)
		return err
	}); err != nil {
		t.Fatalf("create+consume: %v", err)
	}
	// Insert a fresh unconsumed row for a different change_type — the
	// partial unique index excludes the consumed row, so this is
	// allowed even though the first one still exists.
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, alice, func(ts db.TxStores) error {
		return ts.Curator.InsertPendingContext(ctx, orgID, projectID, "sess-1", domain.ChangeTypeJiraProjectKey, `null`)
	}); err != nil {
		t.Fatalf("insert jira: %v", err)
	}

	var rows []domain.CuratorPendingContext
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, alice, func(ts db.TxStores) error {
		var e error
		rows, e = ts.Curator.ListPendingContext(ctx, orgID, projectID)
		return e
	}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d (%+v)", len(rows), rows)
	}
	if rows[0].ConsumedAt == nil {
		t.Errorf("first row should be consumed; got %+v", rows[0])
	}
	if rows[1].ConsumedAt != nil {
		t.Errorf("second row should be unconsumed; got %+v", rows[1])
	}
}

// TestCuratorStore_Postgres_DeletePendingContextForSession verifies
// the session-scoped wipe leaves other-session rows untouched.
func TestCuratorStore_Postgres_DeletePendingContextForSession(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB)
	ctx := context.Background()

	orgID, alice, _ := seedPgProjectOrg(t, h)
	projectID := seedPgEntityProject(t, h, orgID, alice, "delete-by-session")
	setProjectSession(t, h, projectID, "sess-1")

	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, alice, func(ts db.TxStores) error {
		if err := ts.Curator.InsertPendingContext(ctx, orgID, projectID, "sess-1", domain.ChangeTypePinnedRepos, `["a"]`); err != nil {
			return err
		}
		return ts.Curator.InsertPendingContext(ctx, orgID, projectID, "sess-2", domain.ChangeTypePinnedRepos, `["b"]`)
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, alice, func(ts db.TxStores) error {
		return ts.Curator.DeletePendingContextForSession(ctx, orgID, projectID, "sess-1")
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}

	var rows []domain.CuratorPendingContext
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, alice, func(ts db.TxStores) error {
		var e error
		rows, e = ts.Curator.ListPendingContext(ctx, orgID, projectID)
		return e
	}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row (sess-2 survivor), got %d (%+v)", len(rows), rows)
	}
	if rows[0].CuratorSessionID != "sess-2" {
		t.Errorf("wrong session survived: %q", rows[0].CuratorSessionID)
	}
}

// setProjectSession bumps the project's curator_session_id via the
// admin pool. The CuratorStore writes that follow are gated by the
// partial-unique index on (project_id, curator_session_id, change_type)
// WHERE consumed_at IS NULL — the project needs an active session
// before any pending row makes sense.
func setProjectSession(t *testing.T, h *pgtest.Harness, projectID, sessionID string) {
	t.Helper()
	if _, err := h.AdminDB.Exec(
		`UPDATE projects SET curator_session_id = $1 WHERE id = $2`,
		sessionID, projectID,
	); err != nil {
		t.Fatalf("set project session id: %v", err)
	}
}

// TestCuratorStore_Postgres_CancelOrphanedNonTerminalRequests pins
// the cross-tenant sweep contract documented on the store interface:
// every queued/running curator_request across every org gets
// cancelled. The boot path has no JWT-claims context, so the impl
// routes through the admin pool (BYPASSRLS); this test seeds rows
// under two distinct orgs and asserts both are affected.
//
// This is the TFAC-65 orphan-cleanup audit: it confirms the sweep is
// admin-pooled. The load-bearing wiring is pgstore.New(AdminDB, AppDB) —
// an app-pool variant run with no claims would match zero rows under RLS
// and silently strand a prior process's orphans across every tenant. The
// boot caller (internal/app/subsystems.go) invokes this on exactly that
// admin-pooled store before constructing the Curator.
func TestCuratorStore_Postgres_CancelOrphanedNonTerminalRequests(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB)
	ctx := context.Background()

	orgA, alice, _ := seedPgProjectOrg(t, h)
	orgB, bob, _ := seedPgProjectOrg(t, h)
	projectA := seedPgEntityProject(t, h, orgA, alice, "sweep-a")
	projectB := seedPgEntityProject(t, h, orgB, bob, "sweep-b")

	createReq := func(orgID, userID, projectID, input string) string {
		t.Helper()
		var id string
		if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, userID, func(ts db.TxStores) error {
			rid, err := ts.Curator.CreateRequest(ctx, orgID, projectID, userID, input)
			if err != nil {
				return err
			}
			id = rid
			return nil
		}); err != nil {
			t.Fatalf("create %s: %v", input, err)
		}
		return id
	}

	queuedA := createReq(orgA, alice, projectA, "queued-a")
	runningA := createReq(orgA, alice, projectA, "running-a")
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgA, alice, func(ts db.TxStores) error {
		return ts.Curator.MarkRequestRunning(ctx, orgA, runningA)
	}); err != nil {
		t.Fatalf("mark running A: %v", err)
	}
	doneA := createReq(orgA, alice, projectA, "done-a")
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgA, alice, func(ts db.TxStores) error {
		_, err := ts.Curator.CompleteRequest(ctx, orgA, doneA, "done", "", 0.01, 10, 1)
		return err
	}); err != nil {
		t.Fatalf("complete A: %v", err)
	}

	queuedB := createReq(orgB, bob, projectB, "queued-b")
	runningB := createReq(orgB, bob, projectB, "running-b")
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgB, bob, func(ts db.TxStores) error {
		return ts.Curator.MarkRequestRunning(ctx, orgB, runningB)
	}); err != nil {
		t.Fatalf("mark running B: %v", err)
	}

	n, err := stores.Curator.CancelOrphanedNonTerminalRequests(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 4 {
		t.Errorf("sweep flipped %d rows, want 4 (queued+running across two orgs)", n)
	}

	// Read status via admin pool to bypass RLS.
	getStatus := func(id string) string {
		t.Helper()
		var status string
		if err := h.AdminDB.QueryRow(
			`SELECT status FROM curator_requests WHERE id = $1`, id,
		).Scan(&status); err != nil {
			t.Fatalf("read status %s: %v", id, err)
		}
		return status
	}
	for _, id := range []string{queuedA, runningA, queuedB, runningB} {
		if got := getStatus(id); got != "cancelled" {
			t.Errorf("row %s status = %q, want cancelled", id, got)
		}
	}
	if got := getStatus(doneA); got != "done" {
		t.Errorf("terminal row %s status = %q, want done (untouched)", doneA, got)
	}
}

// runFullTurn replays the per-message write set the goroutine would
// emit for one turn under (orgID, userID). Returns the request id and
// the inserted message id so the caller can spot-check attribution.
func runFullTurn(t *testing.T, ctx context.Context, stores db.Stores, orgID, userID, projectID string) (requestID string, messageID int64) {
	t.Helper()
	// 1. Create request (handler-side, but in the same identity).
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, userID, func(ts db.TxStores) error {
		id, err := ts.Curator.CreateRequest(ctx, orgID, projectID, userID, "msg from "+userID)
		if err != nil {
			return err
		}
		requestID = id
		return nil
	}); err != nil {
		t.Fatalf("create request for %s: %v", userID, err)
	}
	// 2. Mark running.
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, userID, func(ts db.TxStores) error {
		return ts.Curator.MarkRequestRunning(ctx, orgID, requestID)
	}); err != nil {
		t.Fatalf("mark running for %s: %v", userID, err)
	}
	// 3. Insert one streamed message.
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, userID, func(ts db.TxStores) error {
		id, err := ts.Curator.InsertMessage(ctx, orgID, &domain.CuratorMessage{
			RequestID: requestID,
			Role:      "assistant",
			Subtype:   "text",
			Content:   "ack for " + userID,
		})
		if err != nil {
			return err
		}
		messageID = id
		return nil
	}); err != nil {
		t.Fatalf("insert message for %s: %v", userID, err)
	}
	// 4. Complete request.
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, userID, func(ts db.TxStores) error {
		_, err := ts.Curator.CompleteRequest(ctx, orgID, requestID, "done", "", 0.01, 10, 1)
		return err
	}); err != nil {
		t.Fatalf("complete request for %s: %v", userID, err)
	}
	return requestID, messageID
}

func readCuratorRequestCreator(t *testing.T, h *pgtest.Harness, requestID string) string {
	t.Helper()
	var got string
	if err := h.AdminDB.QueryRow(
		`SELECT creator_user_id::text FROM curator_requests WHERE id = $1`,
		requestID,
	).Scan(&got); err != nil {
		t.Fatalf("read curator_requests.creator_user_id for %s: %v", requestID, err)
	}
	return got
}

func readCuratorMessageCreator(t *testing.T, h *pgtest.Harness, messageID int64) string {
	t.Helper()
	var got string
	if err := h.AdminDB.QueryRow(
		`SELECT creator_user_id::text FROM curator_messages WHERE id = $1`,
		messageID,
	).Scan(&got); err != nil {
		t.Fatalf("read curator_messages.creator_user_id for %d: %v", messageID, err)
	}
	return got
}

// seedAdditionalUser adds a second user to an existing org + the
// org's default team so tests can exercise multi-user attribution
// paths against the same project. Returns the new user's id. The
// team-membership row goes into `memberships`, the same table
// seedPgDefaultTeam writes to — there's no separate
// `team_memberships` table, despite the name pattern elsewhere.
func seedAdditionalUser(t *testing.T, h *pgtest.Harness, orgID, label string) string {
	t.Helper()
	userID := seedPgMember(t, h, orgID, label, "member")
	teamID := firstTeamForOrg(t, h, orgID)
	if _, err := h.AdminDB.Exec(
		`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'member')`,
		userID, teamID,
	); err != nil {
		t.Fatalf("seed team membership: %v", err)
	}
	return userID
}
