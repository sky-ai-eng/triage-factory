package postgres_test

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// failWritesTo installs a trigger that raises on every write of the given kind
// to table, for the rest of the test. It is how these tests make the second
// statement of a transaction fail after the first has run.
func failWritesTo(t *testing.T, h *pgtest.Harness, table, op string) {
	t.Helper()
	fn := "tf_test_fail_" + table
	pgtest.MustExec(t, h.AdminDB, `
		CREATE OR REPLACE FUNCTION `+fn+`() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'forced failure'; END $$`)
	pgtest.MustExec(t, h.AdminDB, `CREATE TRIGGER `+fn+` BEFORE `+op+` ON `+table+` FOR EACH ROW EXECUTE FUNCTION `+fn+`()`)
	t.Cleanup(func() {
		pgtest.MustExec(t, h.AdminDB, `DROP TRIGGER IF EXISTS `+fn+` ON `+table)
		pgtest.MustExec(t, h.AdminDB, `DROP FUNCTION IF EXISTS `+fn+`()`)
	})
}

// The intent and the cross-pod signal that hastens it commit together or not
// at all: a signal with no intent behind it would stop an engagement that then
// parks as an idle turn-end, and an intent whose signal was lost only waits
// for the holder's next renewal — but the store promises both or neither.
func TestRequestStopSystem_Postgres_IntentAndSignalCommitTogether(t *testing.T) {
	h := pgtest.Shared(t)
	f := pgClaimLeaseFixture(t, h)
	ctx := context.Background()

	id, _ := f.StageStep(t)
	ok, err := f.Stores.Conversations.RequestStopSystem(ctx, f.OrgID, id, "user-x", "exec-owner", "")
	if err != nil || !ok {
		t.Fatalf("RequestStopSystem = (%v, %v)", ok, err)
	}
	var kind, target string
	if err := h.AdminDB.QueryRow(`SELECT kind, target FROM conversation_signals WHERE conversation_id = $1`, id).Scan(&kind, &target); err != nil {
		t.Fatalf("read the signal: %v", err)
	}
	if kind != string(domain.ConversationSignalCancel) || target != "exec-owner" {
		t.Errorf("signal = (%q, %q), want (cancel, exec-owner)", kind, target)
	}

	// The signal insert fails: the intent written before it in the same
	// transaction must not survive.
	failWritesTo(t, h, "conversation_signals", "INSERT")
	other, _ := f.StageStep(t)
	if ok, err := f.Stores.Conversations.RequestStopSystem(ctx, f.OrgID, other, "user-x", "exec-owner", ""); err == nil || ok {
		t.Fatalf("RequestStopSystem with the signal insert failing = (%v, %v), want an error", ok, err)
	}
	got, err := f.Stores.Conversations.GetSystem(ctx, f.OrgID, other)
	if err != nil || got == nil {
		t.Fatalf("GetSystem: (%v, %v)", got, err)
	}
	if got.StopRequestedAt != nil {
		t.Error("the intent committed without its signal")
	}
}

// The holder's settlement is one transaction: the park, the intent clear and
// the claim release. A release that fails takes the park with it, so no
// window exists in which the row is parked and its claim still live.
func TestParkOpenForClaimSystem_Postgres_ParkAndReleaseAreOneTransaction(t *testing.T) {
	h := pgtest.Shared(t)
	f := pgClaimLeaseFixture(t, h)
	ctx := context.Background()

	id, _ := f.StageStep(t)
	conv, err := f.Stores.ConversationQueue.ClaimNextConversation(ctx, "stop-exec", 1, db.ClaimPlacement{}, db.DefaultClaimLease)
	if err != nil || conv == nil || conv.ID != id {
		t.Fatalf("claim = (%+v, %v)", conv, err)
	}
	if ok, err := f.Stores.Conversations.RequestStopSystem(ctx, f.OrgID, id, "user-x", "", ""); err != nil || !ok {
		t.Fatalf("RequestStopSystem = (%v, %v)", ok, err)
	}

	failWritesTo(t, h, "claims", "UPDATE")
	if _, err := f.Stores.Conversations.ParkOpenForClaimSystem(ctx, f.OrgID, id, conv.ClaimID, db.ParkStopped(domain.ParkReasonUserCancelled, "")); err == nil {
		t.Fatal("park with the claim release failing succeeded")
	}
	got, err := f.Stores.Conversations.GetSystem(ctx, f.OrgID, id)
	if err != nil || got == nil {
		t.Fatalf("GetSystem: (%v, %v)", got, err)
	}
	if got.Status == domain.StatusOpen || got.StopRequestedAt == nil {
		t.Errorf("after the failed settlement = (status %q, intent %v), want not parked and the intent still pending", got.Status, got.StopRequestedAt)
	}
	c, err := f.Stores.ConversationQueue.ClaimByIDSystem(ctx, conv.ClaimID)
	if err != nil || c == nil || c.ReleasedAt != nil {
		t.Errorf("claim after the failed settlement = (%+v, %v), want still live", c, err)
	}
}
