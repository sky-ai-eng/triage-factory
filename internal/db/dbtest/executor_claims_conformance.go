package dbtest

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ExecutorClaimsFactory is what a per-backend test file hands to
// RunExecutorClaimsConformance. Returns the wired ConversationQueueStore and the seeder
// that stages the history the read projects.
type ExecutorClaimsFactory func(t *testing.T) (store db.ConversationQueueStore, seed ExecutorClaimsSeeder)

// ExecutorClaimRow is one claims row the seeder stages verbatim.
type ExecutorClaimRow struct {
	ConversationID string
	ExecutorID     string
	ClaimedAt      time.Time
	// ReleasedAt nil stages a LIVE claim — the shape whose sandbox may still
	// be growing under the operator's eyes.
	ReleasedAt *time.Time
	Outcome    string
	// PeakMemMB / CPUUsec nil stage NULL columns: "not measured" (an
	// unsandboxed run, a pre-5.19 kernel, a crashed teardown), which the read
	// must carry through as nil rather than flattening to zero.
	PeakMemMB *int
	CPUUsec   *int64
}

// ExecutorClaimsSeeder stages the claim history the operator read projects.
// Both callbacks write rows directly rather than driving the store's own
// mutations: the states this read must render (a released claim carrying
// teardown actuals, a live one carrying none, an ordering across claims minted
// inside the same second) accumulate across a run's whole life through several
// subsystems, and no single store call the suite could make reaches them.
type ExecutorClaimsSeeder struct {
	// Run stages one conversation in the given terminal state and returns its
	// id. failureKind may be empty (an unclassified or non-failed run).
	Run func(t *testing.T, status, failureKind string) (conversationID string)

	// Claim stages one claims row and returns its id.
	Claim func(t *testing.T, row ExecutorClaimRow) (claimID string)
}

// RunExecutorClaimsConformance covers the contract the fleet console's
// per-sandbox breakdown depends on: RecentClaimsForExecutorSystem returns one
// executor's claims newest-first, capped, joined to the driven conversation's
// terminal state, with NULL actuals surviving as nil; and ClaimByIDSystem
// answers the same projection for a single id, reporting an unknown id as
// (nil, nil) so the caller can 404 rather than 500.
func RunExecutorClaimsConformance(t *testing.T, mk ExecutorClaimsFactory) {
	t.Helper()
	ctx := context.Background()
	// A fixed clock so ordering is exact rather than resolution-dependent:
	// SQLite's CURRENT_TIMESTAMP has one-second granularity, so claims minted
	// by a real dispatcher inside one second would tie.
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

	t.Run("newest_first_scoped_to_the_executor_and_capped", func(t *testing.T) {
		store, seed := mk(t)
		conversationID := seed.Run(t, "completed", "")
		// Three claims for ours, one for a neighbour executor on the same box's
		// deployment — the neighbour must not appear.
		var ids []string
		for i, off := range []time.Duration{0, time.Minute, 2 * time.Minute} {
			released := base.Add(off + 30*time.Second)
			ids = append(ids, seed.Claim(t, ExecutorClaimRow{
				ConversationID: conversationID, ExecutorID: "exec-a", ClaimedAt: base.Add(off),
				ReleasedAt: &released, Outcome: "completed", PeakMemMB: iptr(100 + i),
			}))
		}
		seed.Claim(t, ExecutorClaimRow{
			ConversationID: conversationID, ExecutorID: "exec-b", ClaimedAt: base.Add(3 * time.Minute),
		})

		got, err := store.RecentClaimsForExecutorSystem(ctx, "exec-a", 0)
		if err != nil {
			t.Fatalf("RecentClaimsForExecutorSystem: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("expected exec-a's 3 claims (and none of exec-b's), got %d: %+v", len(got), got)
		}
		// Newest first: the last-staged claim leads.
		for i, want := range []string{ids[2], ids[1], ids[0]} {
			if got[i].ID != want {
				t.Fatalf("position %d = %s, want %s (series must be newest-first)", i, got[i].ID, want)
			}
		}

		// The cap is honored, and it keeps the NEWEST rows — an operator
		// looking at a busy box needs what just ran, not the oldest survivors.
		capped, err := store.RecentClaimsForExecutorSystem(ctx, "exec-a", 2)
		if err != nil {
			t.Fatalf("capped read: %v", err)
		}
		if len(capped) != 2 || capped[0].ID != ids[2] || capped[1].ID != ids[1] {
			t.Fatalf("limit=2 must return the two newest, got %+v", capped)
		}
	})

	t.Run("projects_actuals_lifetime_and_conversation_state", func(t *testing.T) {
		store, seed := mk(t)
		conversationID := seed.Run(t, "failed", "memory_limit")
		released := base.Add(90 * time.Second)
		claimID := seed.Claim(t, ExecutorClaimRow{
			ConversationID: conversationID, ExecutorID: "exec-a", ClaimedAt: base,
			ReleasedAt: &released, Outcome: "failed",
			PeakMemMB: iptr(2048), CPUUsec: i64ptr(42_000_000),
		})

		got, err := store.RecentClaimsForExecutorSystem(ctx, "exec-a", 25)
		if err != nil {
			t.Fatalf("RecentClaimsForExecutorSystem: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("expected 1 claim, got %d: %+v", len(got), got)
		}
		c := got[0]
		if c.ID != claimID || c.ConversationID != conversationID {
			t.Fatalf("identity wrong: %+v", c)
		}
		if c.OrgID == "" {
			t.Errorf("org id must be projected (the operator sees claims across orgs): %+v", c)
		}
		if !c.ClaimedAt.Equal(base) {
			t.Errorf("ClaimedAt = %v, want %v", c.ClaimedAt, base)
		}
		if c.ReleasedAt == nil || !c.ReleasedAt.Equal(released) {
			t.Errorf("ReleasedAt = %v, want %v", c.ReleasedAt, released)
		}
		if c.Outcome != "failed" {
			t.Errorf("Outcome = %q, want failed", c.Outcome)
		}
		if c.PeakMemMB == nil || *c.PeakMemMB != 2048 {
			t.Errorf("peak memory lost: %+v", c)
		}
		if c.CPUUsec == nil || *c.CPUUsec != 42_000_000 {
			t.Errorf("cpu time lost: %+v", c)
		}
		// The one join: why the engagement ended the way it did.
		if c.Status != "failed" || c.FailureKind != "memory_limit" {
			t.Errorf("conversation state = (%q, %q), want (failed, memory_limit)", c.Status, c.FailureKind)
		}
	})

	t.Run("unmeasured_and_live_claims_survive_as_null", func(t *testing.T) {
		store, seed := mk(t)
		// A live claim: never released, never measured (the teardown that
		// measures hasn't happened). It is still real occupancy on the box, so
		// it must be listed rather than filtered out for lacking numbers.
		conversationID := seed.Run(t, "running", "")
		seed.Claim(t, ExecutorClaimRow{ConversationID: conversationID, ExecutorID: "exec-a", ClaimedAt: base})

		got, err := store.RecentClaimsForExecutorSystem(ctx, "exec-a", 25)
		if err != nil {
			t.Fatalf("RecentClaimsForExecutorSystem: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("a live, unmeasured claim must still be listed, got %d: %+v", len(got), got)
		}
		c := got[0]
		if c.ReleasedAt != nil {
			t.Errorf("live claim must report ReleasedAt nil, got %v", c.ReleasedAt)
		}
		if c.Outcome != "" {
			t.Errorf("live claim Outcome = %q, want empty", c.Outcome)
		}
		if c.PeakMemMB != nil || c.CPUUsec != nil {
			t.Errorf("unmeasured actuals must scan to nil, not 0: %+v", c)
		}
	})

	t.Run("partially_measured_claim_keeps_the_half_it_has", func(t *testing.T) {
		// The pre-5.19 kernel shape: cpu.stat lands, memory.peak does not.
		store, seed := mk(t)
		conversationID := seed.Run(t, "completed", "")
		released := base.Add(time.Minute)
		seed.Claim(t, ExecutorClaimRow{
			ConversationID: conversationID, ExecutorID: "exec-a", ClaimedAt: base,
			ReleasedAt: &released, Outcome: "completed", CPUUsec: i64ptr(7),
		})

		got, err := store.RecentClaimsForExecutorSystem(ctx, "exec-a", 25)
		if err != nil || len(got) != 1 {
			t.Fatalf("read = (%+v, %v), want 1 claim", got, err)
		}
		if got[0].PeakMemMB != nil {
			t.Errorf("unread peak must stay nil: %+v", got[0])
		}
		if got[0].CPUUsec == nil || *got[0].CPUUsec != 7 {
			t.Errorf("measured cpu lost: %+v", got[0])
		}
	})

	t.Run("empty_for_an_executor_with_no_claims", func(t *testing.T) {
		store, seed := mk(t)
		_ = seed.Run(t, "completed", "")
		got, err := store.RecentClaimsForExecutorSystem(ctx, "control-pod-1", 25)
		if err != nil {
			t.Fatalf("a control pod with no claims must read empty, not error: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("expected no claims, got %+v", got)
		}
	})

	t.Run("ClaimByIDSystem_matches_the_list_projection", func(t *testing.T) {
		store, seed := mk(t)
		conversationID := seed.Run(t, "completed", "")
		released := base.Add(time.Minute)
		claimID := seed.Claim(t, ExecutorClaimRow{
			ConversationID: conversationID, ExecutorID: "exec-a", ClaimedAt: base,
			ReleasedAt: &released, Outcome: "completed",
			PeakMemMB: iptr(512), CPUUsec: i64ptr(1_500_000),
		})

		got, err := store.ClaimByIDSystem(ctx, claimID)
		if err != nil {
			t.Fatalf("ClaimByIDSystem: %v", err)
		}
		if got == nil {
			t.Fatal("ClaimByIDSystem returned nil for a claim that exists")
		}
		listed, err := store.RecentClaimsForExecutorSystem(ctx, "exec-a", 25)
		if err != nil || len(listed) != 1 {
			t.Fatalf("list read = (%+v, %v), want 1 claim", listed, err)
		}
		// Compared field by field: the structs hold pointers, so a direct
		// struct comparison would compare addresses (always unequal across
		// two queries) rather than the values that must agree.
		if !sameExecutorClaim(*got, listed[0]) {
			t.Errorf("single-claim read disagrees with the list:\n got %+v\nwant %+v", *got, listed[0])
		}
	})

	t.Run("ClaimByIDSystem_reports_an_unknown_id_as_absent", func(t *testing.T) {
		store, seed := mk(t)
		_ = seed.Run(t, "completed", "")
		// A well-formed id that names nothing, and a malformed one — the
		// operator can type either into a URL. Both are misses, not faults:
		// on Postgres a non-uuid bind would otherwise surface as a 500.
		for _, id := range []string{"33333333-3333-4333-8333-333333333333", "not-a-uuid"} {
			got, err := store.ClaimByIDSystem(ctx, id)
			if err != nil {
				t.Errorf("ClaimByIDSystem(%q) errored: %v", id, err)
			}
			if got != nil {
				t.Errorf("ClaimByIDSystem(%q) = %+v, want nil", id, got)
			}
		}
	})
}

func sameExecutorClaim(a, b domain.ExecutorClaim) bool {
	return a.ID == b.ID && a.OrgID == b.OrgID && a.ConversationID == b.ConversationID &&
		a.ClaimedAt.Equal(b.ClaimedAt) && sameTimePtr(a.ReleasedAt, b.ReleasedAt) &&
		a.Outcome == b.Outcome && sameIntPtr(a.PeakMemMB, b.PeakMemMB) &&
		sameInt64Ptr(a.CPUUsec, b.CPUUsec) &&
		a.Status == b.Status && a.FailureKind == b.FailureKind
}

func sameTimePtr(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func sameIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func sameInt64Ptr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
