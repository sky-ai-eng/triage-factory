package dbtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// DashboardStoreFactory is what a per-backend test file hands to
// RunDashboardStoreConformance. Returns:
//   - the wired DashboardStore impl,
//   - the orgID to pass to every call,
//   - the viewer's TF user id — a real users row, since the commissioned-by
//     leg is an FK to it on Postgres,
//   - a DashboardSeeder the harness stages fixtures through without coupling
//     to the schema's INSERT shape (SQLite vs Postgres column lists differ).
type DashboardStoreFactory func(t *testing.T) (store db.DashboardStore, orgID, viewerUserID string, seed DashboardSeeder)

// DashboardPRFixture is one seeded pull request. The two non-snapshot fields
// are the columns the read consults beside snapshot_json: a zero fixture is an
// active entity nobody commissioned, which is what an ordinary polled pull
// request looks like.
type DashboardPRFixture struct {
	Snapshot domain.PRSnapshot
	// CommissionedBy stamps entities.commissioned_by_user_id — the "a run I
	// asked for opened this" leg. Empty leaves it NULL.
	CommissionedBy string
	// EntityState overrides entities.state. Empty means 'active'.
	EntityState string
}

// DashboardSeeder is a bag of callbacks the conformance suite stages fixture
// rows through. Backend tests implement them against their own SQL — the
// harness only cares that the rows land where DashboardStore reads from.
type DashboardSeeder struct {
	// PR inserts a github pull-request entity carrying the fixture and
	// returns its entity id.
	PR func(t *testing.T, fx DashboardPRFixture) string
	// User inserts a user row and returns its id. The commissioned-by column
	// is an FK to it on Postgres, so a second commissioner has to be a real
	// row rather than an invented uuid.
	User func(t *testing.T, name string) string
}

// Snapshot is the plain case — an ordinary polled pull request, no
// commissioner — which is most of what this suite seeds.
func (s DashboardSeeder) Snapshot(t *testing.T, snap domain.PRSnapshot) {
	t.Helper()
	s.PR(t, DashboardPRFixture{Snapshot: snap})
}

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
//   - PRs unions the author leg with the commissioned-by leg, and the second
//     leg answers for exactly one viewer.
//   - PRs honors the states filter, refuses an unknown state, and answers a
//     count-only read with the same total as the paged one.
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
		store, orgID, _, _ := factory(t)
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
		store, orgID, _, seed := factory(t)
		now := time.Now().UTC()
		// 2 days ago — comfortably inside both the 30-day Stats
		// window and the 14-day MergedOverTime window so the bucket
		// assertion below has a stable target.
		recentTime := now.Add(-2 * 24 * time.Hour)
		recentDateKey := recentTime.Format("2006-01-02")
		recentRFC := recentTime.Format(time.RFC3339)

		seed.Snapshot(t, domain.PRSnapshot{Number: 1, Author: username, State: "MERGED", Merged: true, MergedAt: recentRFC})
		seed.Snapshot(t, domain.PRSnapshot{Number: 2, Author: username, State: "CLOSED", ClosedAt: recentRFC})
		seed.Snapshot(t, domain.PRSnapshot{Number: 3, Author: username, State: "OPEN"})
		seed.Snapshot(t, domain.PRSnapshot{Number: 4, Author: username, State: "OPEN", IsDraft: true})
		// Someone else's open PR — should NOT count toward the user's totals.
		seed.Snapshot(t, domain.PRSnapshot{Number: 5, Author: "someone-else", State: "OPEN"})

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
		store, orgID, _, seed := factory(t)

		// Just after the most recent UTC midnight: inside today's UTC bucket,
		// and — west of UTC — inside yesterday's local one.
		utcNow := time.Now().UTC()
		justAfterUTCMidnight := time.Date(utcNow.Year(), utcNow.Month(), utcNow.Day(), 0, 30, 0, 0, time.UTC)
		todayUTC := justAfterUTCMidnight.Format("2006-01-02")
		seed.Snapshot(t, domain.PRSnapshot{
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
		store, orgID, _, seed := factory(t)
		seed.Snapshot(t, domain.PRSnapshot{
			Number: 10, Author: username, State: "OPEN",
			Reviews: []domain.ReviewState{
				{Author: "reviewer-a"},
				{Author: "reviewer-b"},
				{Author: username}, // self-reviews don't count as "received"
			},
		})
		seed.Snapshot(t, domain.PRSnapshot{
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
		store, orgID, viewerID, seed := factory(t)
		now := time.Now().UTC().Format(time.RFC3339)
		seed.Snapshot(t, domain.PRSnapshot{Number: 20, Author: username, State: "MERGED", Merged: true, MergedAt: now, Repo: "owner/repo"})
		seed.Snapshot(t, domain.PRSnapshot{Number: 21, Author: username, State: "OPEN", Repo: "owner/repo"})
		seed.Snapshot(t, domain.PRSnapshot{Number: 22, Author: "stranger", State: "OPEN", Repo: "owner/repo"})

		prs, total, err := store.PRs(context.Background(), orgID, viewerOf(username, viewerID), db.PRListFilter{}, db.Unwindowed)
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
		store, orgID, viewerID, seed := factory(t)
		for i := range 3 {
			seed.Snapshot(t, domain.PRSnapshot{Number: 30 + i, Author: username, State: "OPEN", Repo: "owner/repo"})
			seed.Snapshot(t, domain.PRSnapshot{Number: 40 + i, Author: "stranger", State: "OPEN", Repo: "owner/repo"})
		}

		seen := map[int]bool{}
		for offset := 0; offset < 4; offset += 2 {
			page, total, err := store.PRs(context.Background(), orgID, viewerOf(username, viewerID), db.PRListFilter{}, db.ListOpts{Limit: 2, Offset: offset})
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

	t.Run("PRs_UnionsTheCommissionedLeg", func(t *testing.T) {
		// The gap the second leg fills: a pull request TF opened is authored
		// by a bot that maps to no TF user, so the author predicate alone
		// never returns the work the viewer themselves asked for.
		store, orgID, viewerID, seed := factory(t)
		other := seed.User(t, "someone else")

		seed.Snapshot(t, domain.PRSnapshot{Number: 50, Author: username, State: "OPEN", Repo: "owner/repo"})
		seed.PR(t, DashboardPRFixture{
			Snapshot:       domain.PRSnapshot{Number: 51, Author: "tf-bot", State: "OPEN", Repo: "owner/repo"},
			CommissionedBy: viewerID,
		})
		// Commissioned by somebody else, and authored by a login that is
		// nobody's: neither leg reaches it.
		seed.PR(t, DashboardPRFixture{
			Snapshot:       domain.PRSnapshot{Number: 52, Author: "tf-bot", State: "OPEN", Repo: "owner/repo"},
			CommissionedBy: other,
		})

		prs, total, err := store.PRs(context.Background(), orgID, viewerOf(username, viewerID), db.PRListFilter{}, db.Unwindowed)
		if err != nil {
			t.Fatalf("PRs: %v", err)
		}
		if len(prs) != 2 || total != 2 {
			t.Fatalf("len(prs)=%d total=%d, want 2/2 (mine by author + mine by commission)", len(prs), total)
		}
		got := map[int]bool{}
		for _, p := range prs {
			got[p.Number] = true
		}
		if !got[50] || !got[51] {
			t.Errorf("returned %v, want #50 (authored) and #51 (commissioned)", got)
		}
		if got[52] {
			t.Error("returned #52 — another viewer's commission is not mine")
		}

		// The same row read as the other viewer: theirs, not mine. Same rows,
		// two answers, which is what makes the leg an attribution rather than
		// a flag.
		theirs, _, err := store.PRs(context.Background(), orgID, viewerOf("", other), db.PRListFilter{}, db.Unwindowed)
		if err != nil {
			t.Fatalf("PRs (other viewer): %v", err)
		}
		if len(theirs) != 1 || theirs[0].Number != 52 {
			t.Fatalf("other viewer got %+v, want exactly #52", theirs)
		}
	})

	t.Run("PRs_StatesFilterAndCountOnly", func(t *testing.T) {
		store, orgID, viewerID, seed := factory(t)
		now := time.Now().UTC().Format(time.RFC3339)
		// open is the ENTITY's state, so the closed rows carry state='closed'
		// exactly as the tracker leaves a terminal pull request.
		seed.Snapshot(t, domain.PRSnapshot{Number: 60, Author: username, State: "OPEN", Repo: "owner/repo"})
		seed.PR(t, DashboardPRFixture{
			Snapshot:    domain.PRSnapshot{Number: 61, Author: username, State: "MERGED", Merged: true, MergedAt: now, Repo: "owner/repo"},
			EntityState: "closed",
		})
		seed.PR(t, DashboardPRFixture{
			Snapshot:    domain.PRSnapshot{Number: 62, Author: username, State: "CLOSED", ClosedAt: now, Repo: "owner/repo"},
			EntityState: "closed",
		})

		viewer := viewerOf(username, viewerID)
		for _, tc := range []struct {
			states []string
			want   []int
		}{
			{nil, []int{60, 61, 62}},
			{[]string{domain.PRStateOpen}, []int{60}},
			{[]string{domain.PRStateMerged}, []int{61}},
			{[]string{domain.PRStateClosed}, []int{62}},
			// closed must not swallow merged: GitHub reports one wire shape of
			// a merged pull request as CLOSED with merged=true.
			{[]string{domain.PRStateMerged, domain.PRStateClosed}, []int{61, 62}},
		} {
			f := db.PRListFilter{States: tc.states}
			prs, total, err := store.PRs(context.Background(), orgID, viewer, f, db.Unwindowed)
			if err != nil {
				t.Fatalf("PRs(states=%v): %v", tc.states, err)
			}
			got := map[int]bool{}
			for _, p := range prs {
				got[p.Number] = true
			}
			if len(prs) != len(tc.want) || total != len(tc.want) {
				t.Errorf("states=%v returned %v (total %d), want %v", tc.states, got, total, tc.want)
			}
			for _, n := range tc.want {
				if !got[n] {
					t.Errorf("states=%v is missing #%d", tc.states, n)
				}
			}
			// The count-only read is the figure a client renders without
			// paying for rows; it must be the same number the page reports.
			empty, countTotal, err := store.PRs(context.Background(), orgID, viewer, f, db.ListOpts{CountOnly: true})
			if err != nil {
				t.Fatalf("PRs(states=%v, count-only): %v", tc.states, err)
			}
			if len(empty) != 0 {
				t.Errorf("states=%v count-only returned %d rows, want none", tc.states, len(empty))
			}
			if countTotal != total {
				t.Errorf("states=%v count-only total=%d, paged total=%d — the figure and the list disagree",
					tc.states, countTotal, total)
			}
		}
	})

	t.Run("PRs_UnknownStateIsRefused", func(t *testing.T) {
		// Never dropped and never widened to "all": a filter the store cannot
		// honor has to fail, or a caller gets a result set for a query they
		// did not ask.
		store, orgID, viewerID, seed := factory(t)
		seed.Snapshot(t, domain.PRSnapshot{Number: 70, Author: username, State: "OPEN", Repo: "owner/repo"})

		_, _, err := store.PRs(context.Background(), orgID, viewerOf(username, viewerID),
			db.PRListFilter{States: []string{"nonsense"}}, db.Unwindowed)
		if !errors.Is(err, db.ErrUnknownPRState) {
			t.Fatalf("PRs(states=[nonsense]) err = %v, want db.ErrUnknownPRState", err)
		}
	})
}

// viewerOf is the identity pair the personal list reads: the caller's GitHub
// login and their TF user id. Most subtests care about only one leg, and
// passing both everywhere is what keeps the other from silently going missing.
func viewerOf(login, userID string) db.PRViewer {
	return db.PRViewer{Login: login, UserID: userID}
}
