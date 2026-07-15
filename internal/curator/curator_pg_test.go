package curator

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestCurator_Postgres_CancelProject_DrainsQueuedRowsUnderRLS pins
// TFAC-64: the project-delete drain (CancelProject → cancelQueuedRows)
// must move every queued curator_request to a terminal state in
// multi-mode. The drain has no live request/JWT context, so before the
// fix it called the package-level helpers on a raw *sql.DB with no
// claims set — under tf_app + RLS the SELECT saw zero rows and the
// UPDATE was rejected, stranding the rows in `queued` past the project
// FK cascade. Now it routes through the admin-pool …System doors, so
// the rows reach `cancelled`. Cross-tenant: a second org's queued row
// is untouched.
func TestCurator_Postgres_CancelProject_DrainsQueuedRowsUnderRLS(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	orgA, alice, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	orgB, bob, teamB := pgtest.SeedOrgWithUser(t, h, "bob")
	projectA := seedPgProject(t, h, orgA, alice, teamA, "drain-a")
	projectB := seedPgProject(t, h, orgB, bob, teamB, "drain-b")

	// Enqueue two queued requests for tenant A and one for tenant B,
	// each under the requesting user's synthetic claims (the production
	// SendMessage create path). Also land one terminal `done` row in
	// project A to prove the drain leaves already-terminal rows alone.
	queuedA1 := seedRequest(t, ctx, stores, orgA, alice, projectA, "queued-a1")
	queuedA2 := seedRequest(t, ctx, stores, orgA, alice, projectA, "queued-a2")
	doneA := seedRequest(t, ctx, stores, orgA, alice, projectA, "done-a")
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgA, alice, func(ts db.TxStores) error {
		_, err := ts.Curator.CompleteRequest(ctx, orgA, doneA, "done", "", 0.01, 10, 1)
		return err
	}); err != nil {
		t.Fatalf("complete done-a: %v", err)
	}
	queuedB := seedRequest(t, ctx, stores, orgB, bob, projectB, "queued-b")

	// Drive the production project-delete hook. No session was ever
	// started, so this takes the cancelQueuedRows-only branch — which
	// before TFAC-64 ran the package-level helpers on a raw *sql.DB
	// with no claims (RLS-rejected, rows stranded queued) and now goes
	// through the admin-pool …System doors.
	c := New(stores, nil, "")
	c.CancelProject(orgA, projectA, "project deleted")

	if got := requestStatus(t, h, queuedA1); got != "cancelled" {
		t.Errorf("queuedA1 status = %q, want cancelled", got)
	}
	if got := requestStatus(t, h, queuedA2); got != "cancelled" {
		t.Errorf("queuedA2 status = %q, want cancelled", got)
	}
	if got := requestStatus(t, h, doneA); got != "done" {
		t.Errorf("doneA status = %q, want done (already-terminal row must be untouched)", got)
	}
	// Cross-tenant guard: tenant B's queued row is untouched.
	if got := requestStatus(t, h, queuedB); got != "queued" {
		t.Errorf("queuedB status = %q, want queued (cross-tenant row must be untouched)", got)
	}
}

// TestCurator_Postgres_SystemCancel_FlipsRunningRow pins the in-flight
// half of TFAC-64: process shutdown / session teardown
// (projectSession.shutdown) flips a `running` row via the admin-pool
// MarkRequestCancelledIfActiveSystem door. Under RLS with no claims the
// app-pool variant no-ops; the System variant reaches the row and
// cancels it without touching another tenant's running row.
func TestCurator_Postgres_SystemCancel_FlipsRunningRow(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	orgA, alice, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	orgB, bob, teamB := pgtest.SeedOrgWithUser(t, h, "bob")
	projectA := seedPgProject(t, h, orgA, alice, teamA, "inflight-a")
	projectB := seedPgProject(t, h, orgB, bob, teamB, "inflight-b")

	runningA := seedRequest(t, ctx, stores, orgA, alice, projectA, "running-a")
	runningB := seedRequest(t, ctx, stores, orgB, bob, projectB, "running-b")
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgA, alice, func(ts db.TxStores) error {
		return ts.Curator.MarkRequestRunning(ctx, orgA, runningA)
	}); err != nil {
		t.Fatalf("mark running A: %v", err)
	}
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgB, bob, func(ts db.TxStores) error {
		return ts.Curator.MarkRequestRunning(ctx, orgB, runningB)
	}); err != nil {
		t.Fatalf("mark running B: %v", err)
	}

	flipped, err := stores.Curator.MarkRequestCancelledIfActiveSystem(ctx, orgA, runningA, "process shutting down")
	if err != nil {
		t.Fatalf("system cancel running A: %v", err)
	}
	if !flipped {
		t.Fatal("MarkRequestCancelledIfActiveSystem did not flip a running row; admin-pool door should bypass RLS")
	}
	if got := requestStatus(t, h, runningA); got != "cancelled" {
		t.Errorf("runningA status = %q, want cancelled", got)
	}
	if got := requestStatus(t, h, runningB); got != "running" {
		t.Errorf("runningB status = %q, want running (cross-tenant row must be untouched)", got)
	}
}

// seedPgProject inserts a project row via the admin pool (no JWT claims
// in a bare test context, so the app-pool projects_insert RLS would
// reject it). Mirrors seedPgEntityProject in the postgres test package.
func seedPgProject(t *testing.T, h *pgtest.Harness, orgID, userID, teamID, name string) string {
	t.Helper()
	id := uuid.New().String()
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO projects (id, org_id, creator_user_id, team_id, name, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, now(), now())
	`, id, orgID, userID, teamID, name)
	return id
}

// seedRequest creates a queued curator_request under the requesting
// user's synthetic claims — the production SendMessage create path.
func seedRequest(t *testing.T, ctx context.Context, stores db.Stores, orgID, userID, projectID, input string) string {
	t.Helper()
	var id string
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, userID, func(ts db.TxStores) error {
		rid, err := ts.Curator.CreateRequest(ctx, orgID, projectID, userID, "", input)
		if err != nil {
			return err
		}
		id = rid
		return nil
	}); err != nil {
		t.Fatalf("seed request %q: %v", input, err)
	}
	return id
}

// requestStatus reads a curator_request's status via the admin pool,
// bypassing RLS so the assertion sees the true row state regardless of
// which tenant wrote it.
func requestStatus(t *testing.T, h *pgtest.Harness, id string) string {
	t.Helper()
	var status string
	if err := h.AdminDB.QueryRow(
		`SELECT status FROM curator_requests WHERE id = $1`, id,
	).Scan(&status); err != nil {
		t.Fatalf("read status %s: %v", id, err)
	}
	return status
}
