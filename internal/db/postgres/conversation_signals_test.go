package postgres_test

import (
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestRunSignalStore_Postgres covers the outbox contract (TFAC-585): insert
// mints an id, unacked signals for a target list oldest-first, ack is
// idempotent, and PurgeAcked only removes rows past the age cutoff.
func TestRunSignalStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := t.Context()
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	conversationID := seedPgArtifactConversation(t, h, orgID, teamID, userID)

	const target = "executor-1"

	id1, err := stores.ConversationSignals.Insert(ctx, orgID, conversationID, domain.ConversationSignalInterrupt, "", target)
	if err != nil {
		t.Fatalf("insert interrupt: %v", err)
	}
	id2, err := stores.ConversationSignals.Insert(ctx, orgID, conversationID, domain.ConversationSignalSteer, `{"text":"hi"}`, target)
	if err != nil {
		t.Fatalf("insert steer: %v", err)
	}
	if id2 <= id1 {
		t.Fatalf("expected monotonically increasing ids, got %d then %d", id1, id2)
	}

	// A signal targeting a different executor must not show up in this scan.
	if _, err := stores.ConversationSignals.Insert(ctx, orgID, conversationID, domain.ConversationSignalCancel, "", "executor-2"); err != nil {
		t.Fatalf("insert for other target: %v", err)
	}

	got, err := stores.ConversationSignals.ListUnackedForTarget(ctx, target)
	if err != nil {
		t.Fatalf("list unacked: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d unacked signals for target, want 2", len(got))
	}
	if got[0].ID != id1 || got[1].ID != id2 {
		t.Errorf("expected ascending id order [%d,%d], got [%d,%d]", id1, id2, got[0].ID, got[1].ID)
	}
	if got[0].Kind != domain.ConversationSignalInterrupt || got[0].Payload != "" {
		t.Errorf("interrupt row mismatch: %+v", got[0])
	}
	if got[1].Kind != domain.ConversationSignalSteer || got[1].Payload == "" {
		t.Errorf("steer row missing payload: %+v", got[1])
	}
	if got[0].ConversationID != conversationID || got[0].OrgID != orgID || got[0].Target != target {
		t.Errorf("signal fields not preserved: %+v", got[0])
	}

	// Ack the first; the scan must drop it but keep the second.
	if err := stores.ConversationSignals.Ack(ctx, id1, domain.ConversationSignalAckOK); err != nil {
		t.Fatalf("ack: %v", err)
	}
	acked, result, err := stores.ConversationSignals.AckStatus(ctx, id1)
	if err != nil {
		t.Fatalf("ack status: %v", err)
	}
	if !acked || result != domain.ConversationSignalAckOK {
		t.Errorf("ack status = (%v, %q), want (true, %q)", acked, result, domain.ConversationSignalAckOK)
	}

	// Idempotent: acking again must not error and must not overwrite the
	// first result — a duplicate ack of an already-acked row is a no-op.
	if err := stores.ConversationSignals.Ack(ctx, id1, "different-result"); err != nil {
		t.Fatalf("second ack: %v", err)
	}
	_, result, err = stores.ConversationSignals.AckStatus(ctx, id1)
	if err != nil {
		t.Fatalf("ack status after second ack: %v", err)
	}
	if result != domain.ConversationSignalAckOK {
		t.Errorf("second ack overwrote the first result: got %q, want %q", result, domain.ConversationSignalAckOK)
	}

	got, err = stores.ConversationSignals.ListUnackedForTarget(ctx, target)
	if err != nil {
		t.Fatalf("list unacked after ack: %v", err)
	}
	if len(got) != 1 || got[0].ID != id2 {
		t.Fatalf("after acking id1, expected only id2 unacked, got %+v", got)
	}

	// AckStatus of a never-acked signal reports acked=false.
	acked, _, err = stores.ConversationSignals.AckStatus(ctx, id2)
	if err != nil {
		t.Fatalf("ack status id2: %v", err)
	}
	if acked {
		t.Error("unacked signal reported acked=true")
	}

	// PurgeAcked only removes rows older than the cutoff.
	n, err := stores.ConversationSignals.PurgeAcked(ctx, 24*time.Hour)
	if err != nil {
		t.Fatalf("purge (too recent): %v", err)
	}
	if n != 0 {
		t.Errorf("purge removed %d rows younger than the cutoff, want 0", n)
	}
	n, err = stores.ConversationSignals.PurgeAcked(ctx, 0)
	if err != nil {
		t.Fatalf("purge (everything): %v", err)
	}
	if n != 1 {
		t.Errorf("purge removed %d rows, want 1 (the acked id1)", n)
	}
}

// TestRunSignalStore_Postgres_AckStatusUnknownID pins that polling a
// nonexistent signal id reports acked=false rather than erroring — the
// waiting control pod's timeout path must not distinguish "never existed"
// from "not yet acked".
func TestRunSignalStore_Postgres_AckStatusUnknownID(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	acked, _, err := stores.ConversationSignals.AckStatus(t.Context(), 999999999)
	if err != nil {
		t.Fatalf("ack status unknown id: %v", err)
	}
	if acked {
		t.Error("unknown signal id reported acked=true")
	}
}
