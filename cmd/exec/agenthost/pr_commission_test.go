package agenthost

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The provenance half of the funnel's attribution stamps: who ASKED for the
// run that opened this pull request. It sits beside the owning-team stamp
// covered in pr_ownership_test.go, and the point of these tests is that the
// two are independent — a run has a team whether or not a human asked.

// commissionerOf reads entities.commissioned_by_user_id. Raw SQL because
// nothing in production reads that column by id: the pull-request list reads
// it as a predicate, and a getter existing only for a test would be an
// interface entry with no caller.
func commissionerOf(t *testing.T, conn *sql.DB, entityID string) string {
	t.Helper()
	var got sql.NullString
	if err := conn.QueryRow(
		`SELECT commissioned_by_user_id FROM entities WHERE id = ?`, entityID,
	).Scan(&got); err != nil {
		t.Fatalf("read commissioned_by_user_id for %s: %v", entityID, err)
	}
	return got.String
}

// TestStampPRCommission_ManualRunStampsBothColumns is the ticket's shape: one
// pull_request capture from a run a human asked for records the team that owns
// the work AND the person who asked, so the PR shows up in their own list even
// though its author is a bot that maps to no TF user.
func TestStampPRCommission_ManualRunStampsBothColumns(t *testing.T) {
	ctx := context.Background()
	conn, stores, info := newCaptureStoresConn(t, false)

	a := botOpenedPR()
	RecordExternalWrite(ctx, stores, info, &a, nil)

	ent := prEntity(t, stores)
	if ent == nil {
		t.Fatal("no entity minted for the opened PR")
	}
	if got := ownerOf(t, stores, ent.ID); got != info.TeamID {
		t.Errorf("owning team = %q, want the run's team %q", got, info.TeamID)
	}
	if got := commissionerOf(t, conn, ent.ID); got != info.UserID {
		t.Errorf("commissioned by = %q, want the conversation's creator %q", got, info.UserID)
	}
}

// TestStampPRCommission_EventTriggeredRunRecordsNobody is the other side of
// the same coin. An event-triggered conversation has no creator — nobody asked
// — so there is nothing to record, and the column stays NULL while the team
// stamp beside it still lands.
func TestStampPRCommission_EventTriggeredRunRecordsNobody(t *testing.T) {
	ctx := context.Background()
	conn, stores, info := newCaptureStoresConn(t, true)
	if info.UserID != "" {
		t.Fatalf("fixture gave an event-triggered run a creator %q", info.UserID)
	}

	a := botOpenedPR()
	RecordExternalWrite(ctx, stores, info, &a, nil)

	ent := prEntity(t, stores)
	if ent == nil {
		t.Fatal("no entity minted for the opened PR")
	}
	if got := ownerOf(t, stores, ent.ID); got != info.TeamID {
		t.Errorf("owning team = %q, want the run's team %q — the skipped user stamp must not disturb it", got, info.TeamID)
	}
	if got := commissionerOf(t, conn, ent.ID); got != "" {
		t.Errorf("an event-triggered run recorded a commissioner: %q", got)
	}
}

// TestStampPRCommission_DoesNotOverwriteExistingCommissioner pins the
// stamp-if-NULL half on this column too: re-delivery of the same PR-open write
// — or a second run touching the same pull request — is a no-op, not a second
// opinion about who asked for it.
func TestStampPRCommission_DoesNotOverwriteExistingCommissioner(t *testing.T) {
	ctx := context.Background()
	conn, stores, info := newCaptureStoresConn(t, false)

	const firstAsker = "33333333-3333-3333-3333-333333333333"
	if _, err := conn.Exec(
		`INSERT INTO users (id, display_name) VALUES (?, 'First Asker')`, firstAsker,
	); err != nil {
		t.Fatalf("seed first asker: %v", err)
	}
	ent, _, err := stores.Entities.FindOrCreateSystem(
		ctx, runmode.LocalDefaultOrgID, "github", "octo/repo#42", "pr", "Fix the thing", "")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if stamped, err := stores.Entities.StampCommissionedByIfUnsetSystem(
		ctx, runmode.LocalDefaultOrgID, ent.ID, firstAsker); err != nil || !stamped {
		t.Fatalf("seed commissioner: stamped=%v err=%v", stamped, err)
	}

	a := botOpenedPR()
	RecordExternalWrite(ctx, stores, info, &a, nil)

	if got := commissionerOf(t, conn, ent.ID); got != firstAsker {
		t.Errorf("commissioned by = %q, want the pre-existing %q", got, firstAsker)
	}
}

// TestStampPRCommission_ReviewArtifactRecordsNothing shares the kind guard
// with the ownership stamp, and needs its own case for the same reason: a
// submitted review carries the identical owner/repo#N target as the pull
// request it reviews. Reviewing someone else's work must not record you as
// having asked for it.
func TestStampPRCommission_ReviewArtifactRecordsNothing(t *testing.T) {
	ctx := context.Background()
	conn, stores, info := newCaptureStoresConn(t, false)

	ent, _, err := stores.Entities.FindOrCreateSystem(
		ctx, runmode.LocalDefaultOrgID, "github", "octo/repo#42", "pr", "Someone else's PR", "")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}

	a := domain.NewSubmittedReviewArtifact(
		"octo/repo", 42, 9001, "APPROVE",
		"https://github.com/octo/repo/pull/42#pullrequestreview-9001", info.ConversationID)
	RecordExternalWrite(ctx, stores, info, &a, nil)

	if got := commissionerOf(t, conn, ent.ID); got != "" {
		t.Errorf("a review recorded a commissioner on the reviewed PR: %q", got)
	}
}
