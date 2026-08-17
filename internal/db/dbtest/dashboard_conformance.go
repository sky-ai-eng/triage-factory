package dbtest

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// DashboardStoreFactory is what a per-backend test file hands to
// RunDashboardStoreConformance. Returns:
//   - the wired DashboardStore impl,
//   - the orgID to pass to every call,
//   - a seedPRSnapshot hook the harness uses to drop PR snapshots
//     into entities.snapshot_json without coupling to the schema's
//     INSERT shape (SQLite vs Postgres column lists differ).
type DashboardStoreFactory func(t *testing.T) (store db.DashboardStore, orgID string, seedPRSnapshot PRSnapshotSeederForDashboard)

// PRSnapshotSeederForDashboard inserts an entity with the given
// PRSnapshot blob. Backend tests implement against their own
// SQL — the harness only cares that the snapshot lands somewhere
// DashboardStore reads from.
type PRSnapshotSeederForDashboard func(t *testing.T, snap domain.PRSnapshot)

// RunDashboardStoreConformance covers the aggregation contract
// every DashboardStore impl must hold:
//
//   - Stats counts merged/closed/awaiting/draft for the user.
//   - Stats's reviews-given vs reviews-received split is keyed on
//     the snapshot's review author vs the user.
//   - Stats's merged-over-time has 14 buckets and includes the
//     merged PR on its mergedAt date.
//   - PRs returns only the user's PRs and maps merged→"merged" /
//     OPEN→"open" / CLOSED→"closed".
//   - Empty stats path (no snapshots) returns zeroed counts but
//     a populated 14-bucket timeline (skeleton always present).
func RunDashboardStoreConformance(t *testing.T, factory DashboardStoreFactory) {
	t.Helper()

	const username = "aidan"
	// The window the dashboard handler passes by default. Every seeded PR
	// below is inside it, so a count that moves is a counting bug, not a
	// clock one.
	statsSince := time.Now().AddDate(0, 0, -30)

	t.Run("Stats_NoSnapshots_ReturnsZerosWithTimelineSkeleton", func(t *testing.T) {
		store, orgID, _ := factory(t)
		stats, err := store.Stats(context.Background(), orgID, username, statsSince)
		if err != nil {
			t.Fatalf("Stats: %v", err)
		}
		if stats.Merged != 0 || stats.Closed != 0 || stats.Awaiting != 0 || stats.Draft != 0 {
			t.Fatalf("counts non-zero on empty DB: %+v", stats)
		}
		if len(stats.MergedOverTime) != 14 {
			t.Fatalf("timeline len=%d want 14", len(stats.MergedOverTime))
		}
	})

	t.Run("Stats_CountsMergedClosedAwaitingDraft", func(t *testing.T) {
		store, orgID, seed := factory(t)
		now := time.Now().UTC()
		// 2 days ago — comfortably inside both the 30-day Stats
		// window and the 14-day MergedOverTime window so the bucket
		// assertion below has a stable target.
		recentTime := now.Add(-2 * 24 * time.Hour)
		recentDateKey := recentTime.Format("2006-01-02")
		recentRFC := recentTime.Format(time.RFC3339)

		seed(t, domain.PRSnapshot{Number: 1, Author: username, State: "MERGED", Merged: true, MergedAt: recentRFC})
		seed(t, domain.PRSnapshot{Number: 2, Author: username, State: "CLOSED", ClosedAt: recentRFC})
		seed(t, domain.PRSnapshot{Number: 3, Author: username, State: "OPEN"})
		seed(t, domain.PRSnapshot{Number: 4, Author: username, State: "OPEN", IsDraft: true})
		// Someone else's open PR — should NOT count toward the user's totals.
		seed(t, domain.PRSnapshot{Number: 5, Author: "someone-else", State: "OPEN"})

		stats, err := store.Stats(context.Background(), orgID, username, statsSince)
		if err != nil {
			t.Fatalf("Stats: %v", err)
		}
		if stats.Merged != 1 || stats.Closed != 1 || stats.Awaiting != 1 || stats.Draft != 1 {
			t.Fatalf("counts wrong: merged=%d closed=%d awaiting=%d draft=%d (want 1 each)",
				stats.Merged, stats.Closed, stats.Awaiting, stats.Draft)
		}

		// MergedOverTime bucket-level pin. The doc promises "the
		// merged PR shows up on its mergedAt date"; assert it does,
		// AND that no other bucket got accidentally incremented
		// (sum across buckets must equal the merged count, so a
		// future refactor that double-counts or shifts the bucket
		// math fails here).
		if len(stats.MergedOverTime) != 14 {
			t.Fatalf("timeline len=%d want 14", len(stats.MergedOverTime))
		}
		var bucketSum int
		var foundRecentBucket bool
		for _, b := range stats.MergedOverTime {
			bucketSum += b.Count
			if b.Date == recentDateKey {
				foundRecentBucket = true
				if b.Count != 1 {
					t.Errorf("bucket %s count=%d, want 1 (merged PR's mergedAt date)", b.Date, b.Count)
				}
			} else if b.Count != 0 {
				t.Errorf("bucket %s count=%d, want 0 (no merged PR on this day)", b.Date, b.Count)
			}
		}
		if !foundRecentBucket {
			t.Errorf("merged PR's date %s missing from timeline buckets", recentDateKey)
		}
		if bucketSum != stats.Merged {
			t.Errorf("bucket sum=%d != stats.Merged=%d (timeline + count drift)", bucketSum, stats.Merged)
		}
	})

	t.Run("Stats_MergedOverTimeBucketsInUTCDaysNotLocalOnes", func(t *testing.T) {
		// The bucket boundary, not the totals. Stats counts a merge by parsing
		// the snapshot's RFC3339 mergedAt — a UTC instant — and keys it by that
		// instant's UTC day; the 14-bucket skeleton it then looks those keys up
		// in was built from the process's local clock. Wherever the two zones
		// disagree about what day it is, the newest bucket names a day the
		// counting side never produces and a real merge vanishes from the
		// sparkline while still showing in the total beside it.
		//
		// forceDayBoundaryLocalZone makes them disagree on purpose, so this
		// fails on the old behaviour in any runner zone rather than only in the
		// few hours a day when a developer's own zone would have exposed it.
		forceDayBoundaryLocalZone(t)
		store, orgID, seed := factory(t)

		// Just after the most recent UTC midnight: inside today's UTC bucket,
		// and — west of UTC — inside yesterday's local one.
		utcNow := time.Now().UTC()
		justAfterUTCMidnight := time.Date(utcNow.Year(), utcNow.Month(), utcNow.Day(), 0, 30, 0, 0, time.UTC)
		todayUTC := justAfterUTCMidnight.Format("2006-01-02")
		seed(t, domain.PRSnapshot{
			Number: 900, Author: username, State: "MERGED", Merged: true,
			MergedAt: justAfterUTCMidnight.Format(time.RFC3339),
		})

		stats, err := store.Stats(context.Background(), orgID, username, statsSince)
		if err != nil {
			t.Fatalf("Stats: %v", err)
		}
		if len(stats.MergedOverTime) != 14 {
			t.Fatalf("timeline len=%d want 14", len(stats.MergedOverTime))
		}
		keys := make([]string, len(stats.MergedOverTime))
		for i, b := range stats.MergedOverTime {
			keys[i] = b.Date
		}
		assertUTCDayKeys(t, "MergedOverTime", keys)

		// …and the merge actually lands in it. The skeleton being right is only
		// half the contract; a PR merged in the disputed window has to be found
		// through it.
		var bucketed int
		for _, b := range stats.MergedOverTime {
			bucketed += b.Count
			if b.Date == todayUTC && b.Count != 1 {
				t.Errorf("bucket %s count=%d, want 1 — a PR merged at %s is missing from its own day",
					b.Date, b.Count, justAfterUTCMidnight.Format(time.RFC3339))
			}
		}
		if bucketed != stats.Merged {
			t.Errorf("buckets sum to %d but Merged=%d — the panel and the counter disagree, which is "+
				"what a day key that misses looks like from the UI", bucketed, stats.Merged)
		}
	})

	t.Run("Stats_ReviewsSplit", func(t *testing.T) {
		// On our own PR, reviews by someone else count as "received."
		// On someone else's PR, reviews by us count as "given."
		store, orgID, seed := factory(t)
		seed(t, domain.PRSnapshot{
			Number: 10, Author: username, State: "OPEN",
			Reviews: []domain.ReviewState{
				{Author: "reviewer-a"},
				{Author: "reviewer-b"},
				{Author: username}, // self-reviews don't count as "received"
			},
		})
		seed(t, domain.PRSnapshot{
			Number: 11, Author: "someone-else", State: "OPEN",
			Reviews: []domain.ReviewState{
				{Author: username},
				{Author: "another-reviewer"},
			},
		})
		stats, err := store.Stats(context.Background(), orgID, username, statsSince)
		if err != nil {
			t.Fatalf("Stats: %v", err)
		}
		if stats.ReviewsReceived != 2 {
			t.Fatalf("reviews_received=%d want 2", stats.ReviewsReceived)
		}
		if stats.ReviewsGiven != 1 {
			t.Fatalf("reviews_given=%d want 1", stats.ReviewsGiven)
		}
	})

	t.Run("PRs_ReturnsOnlyUserPRs_WithStateMapping", func(t *testing.T) {
		store, orgID, seed := factory(t)
		now := time.Now().UTC().Format(time.RFC3339)
		seed(t, domain.PRSnapshot{Number: 20, Author: username, State: "MERGED", Merged: true, MergedAt: now, Repo: "owner/repo"})
		seed(t, domain.PRSnapshot{Number: 21, Author: username, State: "OPEN", Repo: "owner/repo"})
		seed(t, domain.PRSnapshot{Number: 22, Author: "stranger", State: "OPEN", Repo: "owner/repo"})

		prs, total, err := store.PRs(context.Background(), orgID, username, db.Unwindowed)
		if err != nil {
			t.Fatalf("PRs: %v", err)
		}
		if len(prs) != 2 || total != 2 {
			t.Fatalf("len(prs)=%d total=%d, want 2/2 (stranger's PR should be excluded)", len(prs), total)
		}
		seenStates := map[string]bool{}
		for _, p := range prs {
			seenStates[p.State] = true
		}
		if !seenStates["merged"] || !seenStates["open"] {
			t.Fatalf("expected both 'merged' and 'open' in states, got %+v", seenStates)
		}
	})

	t.Run("PRs_PagesWithinTheAuthorFilter", func(t *testing.T) {
		// The window has to apply to the author's PRs, not to the entity table:
		// the Go-side filter this replaced would have made page 2 of a 3-row
		// author set come back empty whenever a stranger's PR sorted into the
		// first window.
		store, orgID, seed := factory(t)
		for i := range 3 {
			seed(t, domain.PRSnapshot{Number: 30 + i, Author: username, State: "OPEN", Repo: "owner/repo"})
			seed(t, domain.PRSnapshot{Number: 40 + i, Author: "stranger", State: "OPEN", Repo: "owner/repo"})
		}

		seen := map[int]bool{}
		for offset := 0; offset < 4; offset += 2 {
			page, total, err := store.PRs(context.Background(), orgID, username, db.ListOpts{Limit: 2, Offset: offset})
			if err != nil {
				t.Fatalf("PRs(offset=%d): %v", offset, err)
			}
			if total != 3 {
				t.Fatalf("PRs(offset=%d) total=%d, want 3 (the author's, not the table's)", offset, total)
			}
			want := 2
			if offset == 2 {
				want = 1
			}
			if len(page) != want {
				t.Fatalf("PRs(offset=%d) len=%d, want %d", offset, len(page), want)
			}
			for _, p := range page {
				if p.Author != username {
					t.Errorf("page carried a stranger's PR: %+v", p)
				}
				if seen[p.Number] {
					t.Errorf("PR #%d served on two pages — the ORDER BY has no total order", p.Number)
				}
				seen[p.Number] = true
			}
		}
		if len(seen) != 3 {
			t.Fatalf("paging walked %d distinct PRs, want 3", len(seen))
		}
	})
}
