package dbtest

import (
	"fmt"
	"testing"
	"time"
)

// The per-day sparkline panels (dashboard merges, prompt runs) are built from
// two independently-computed sets of day strings: the database groups rows into
// day buckets, and Go builds a fixed-length skeleton of day keys to look those
// buckets up by. The two have to name the same day for the same instant or the
// lookups miss and the panel reads zero for activity it counted.
//
// They did not. The database's day is UTC — SQLite's DATE() ignores the
// process, and Postgres resolves ::date through the session's TimeZone — while
// the skeletons were built from time.Now() in the process's local zone. The
// mismatch is real for the width of the offset either side of midnight, which
// is why it never showed up: on a UTC CI runner the offset is zero, and on a
// developer's machine it is a few hours a day of quietly-missing data.
//
// Asserting that from a test means arranging for the local day and the UTC day
// to disagree, which no choice of TZ can guarantee — America/New_York and UTC
// name the same day for twenty hours out of twenty-four. So the helpers below
// move time.Local instead, to a zone picked from the current UTC hour so that
// the two days differ every time, in any runner zone.

// forceDayBoundaryLocalZone points time.Local at a fixed zone whose current day
// is NOT the current UTC day, and restores the previous zone when the test
// ends. Which direction it goes depends on the hour, since a zone offset is
// capped at ±14h: before noon UTC the only reachable disagreement is a local
// clock still on yesterday, after noon it is one already on tomorrow. Both are
// real user situations and either one exposes the bug.
//
// This mutates a package-level variable, so a caller must not run in parallel
// with anything else that reads the clock. The conformance suites are
// sequential.
func forceDayBoundaryLocalZone(t *testing.T) {
	t.Helper()
	prev := time.Local
	t.Cleanup(func() { time.Local = prev })

	utcNow := time.Now().UTC()
	var offsetSeconds int
	if utcNow.Hour() < 12 {
		// Rewind past midnight: hour:minute - (hour+1)h is always negative.
		offsetSeconds = -(utcNow.Hour() + 1) * 3600
	} else {
		// Advance past midnight: hour:minute + (24-hour)h is always ≥ 24h.
		offsetSeconds = (24 - utcNow.Hour()) * 3600
	}
	time.Local = time.FixedZone("TEST-DAY-BOUNDARY", offsetSeconds)

	if time.Now().Format("2006-01-02") == time.Now().UTC().Format("2006-01-02") {
		t.Fatalf("forced zone %s still agrees with UTC on the day (%s) — the boundary this test "+
			"exists to cross was not crossed", time.Now().Format(time.RFC3339), utcNow.Format("2006-01-02"))
	}
}

// wantUTCDayKeys is the day skeleton a panel of n buckets ending today must
// have: the last n UTC days, oldest first.
func wantUTCDayKeys(n int) []string {
	now := time.Now().UTC()
	keys := make([]string, 0, n)
	for i := n - 1; i >= 0; i-- {
		keys = append(keys, now.AddDate(0, 0, -i).Format("2006-01-02"))
	}
	return keys
}

// assertUTCDayKeys fails if the skeleton is not the last n UTC days. Called
// under forceDayBoundaryLocalZone, a skeleton built from the local clock is off
// by exactly one day at both ends, so this is a deterministic catch rather than
// one that depends on when the suite happens to run.
func assertUTCDayKeys(t *testing.T, label string, got []string) {
	t.Helper()
	want := wantUTCDayKeys(len(got))
	if len(got) == 0 {
		t.Fatalf("%s: no buckets to check", label)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: bucket %d is %q, want %q — the skeleton is built in the process's local "+
				"zone while the database buckets in UTC, so every lookup on the disputed day misses\n"+
				"  got  %v\n  want %v",
				label, i, got[i], want[i], summarizeEnds(got), summarizeEnds(want))
		}
	}
}

// summarizeEnds renders just the first and last key of a long skeleton, which
// is where an off-by-one-day shows.
func summarizeEnds(keys []string) string {
	if len(keys) <= 2 {
		return fmt.Sprint(keys)
	}
	return fmt.Sprintf("[%s … %s] (%d)", keys[0], keys[len(keys)-1], len(keys))
}
