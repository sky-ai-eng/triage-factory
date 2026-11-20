package sqlite_test

import (
	"testing"
	"time"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
)

// TestParseDBDatetime is SQLite-specific — the COALESCE-loses-type-
// hint quirk that motivates parseDBDatetime is unique to modernc's
// SQLite driver. Postgres' pgx round-trips timestamps cleanly, so
// the helper doesn't exist there. Coverage matters because a row
// with an unrecognized format fails every read that parses it.
//
// Two format families show up in the wild:
//
//   - SQLite-canonical (modernc with _time_format=sqlite, current default):
//     "2006-01-02 15:04:05.999999999-07:00", with the fractional segment
//     dropped when nanos==0 ("2026-04-27 19:02:11+00:00").
//   - Legacy Go time.String() (modernc default before _time_format=sqlite):
//     "2006-01-02 15:04:05.999999999 -0700 MST", optionally with a
//     " m=+..." monotonic clock suffix, optionally with the fractional
//     segment dropped when nanos==0.
//
// Go's time.Parse treats `.999...` as an optional fractional component,
// so a single layout matches both fractional and non-fractional inputs.
// This test pins that behavior so a future layout edit or stdlib change
// can't silently regress the no-fractional path — which would manifest
// as a 500 from every read that parses it the moment any zero-nano
// timestamp is stored.
func TestParseDBDatetime(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want time.Time // zero means "expected to error"
	}{
		{
			name: "sqlite_canonical_with_fractional",
			in:   "2026-04-27 19:02:11.123456789-07:00",
			want: time.Date(2026, 4, 27, 19, 2, 11, 123456789, time.FixedZone("", -7*3600)),
		},
		{
			name: "sqlite_canonical_no_fractional",
			in:   "2026-04-27 19:02:11-07:00",
			want: time.Date(2026, 4, 27, 19, 2, 11, 0, time.FixedZone("", -7*3600)),
		},
		{
			name: "go_string_with_fractional_pdt",
			in:   "2026-04-27 19:02:11.123456789 -0700 PDT",
			want: time.Date(2026, 4, 27, 19, 2, 11, 123456789, time.FixedZone("PDT", -7*3600)),
		},
		{
			name: "go_string_no_fractional_pdt",
			in:   "2026-04-27 19:02:11 -0700 PDT",
			want: time.Date(2026, 4, 27, 19, 2, 11, 0, time.FixedZone("PDT", -7*3600)),
		},
		{
			name: "go_string_no_fractional_utc",
			in:   "2026-04-27 19:02:11 +0000 UTC",
			want: time.Date(2026, 4, 27, 19, 2, 11, 0, time.UTC),
		},
		{
			name: "go_string_with_monotonic_suffix",
			in:   "2026-04-27 19:02:11.123 -0700 PDT m=+1.500",
			want: time.Date(2026, 4, 27, 19, 2, 11, 123000000, time.FixedZone("PDT", -7*3600)),
		},
		{
			name: "go_string_with_negative_monotonic",
			in:   "2026-04-27 19:02:11 -0700 PDT m=-0.250",
			want: time.Date(2026, 4, 27, 19, 2, 11, 0, time.FixedZone("PDT", -7*3600)),
		},
		{
			name: "sqlite_current_timestamp",
			in:   "2026-04-27 19:02:11",
			want: time.Date(2026, 4, 27, 19, 2, 11, 0, time.UTC),
		},
		{
			name: "rfc3339_zulu",
			in:   "2026-04-27T19:02:11Z",
			want: time.Date(2026, 4, 27, 19, 2, 11, 0, time.UTC),
		},
		{
			name: "rfc3339_with_offset",
			in:   "2026-04-27T19:02:11-07:00",
			want: time.Date(2026, 4, 27, 19, 2, 11, 0, time.FixedZone("", -7*3600)),
		},
		{
			name: "empty",
			in:   "",
			want: time.Time{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sqlitestore.ParseDBDatetimeForTest(tc.in)
			if err != nil {
				t.Fatalf("parseDBDatetime(%q): unexpected error: %v", tc.in, err)
			}
			// Use Equal so location/abbreviation differences don't
			// fail the test as long as the instant is the same.
			if !got.Equal(tc.want) {
				t.Errorf("parseDBDatetime(%q) = %v, want %v (equal-instant)", tc.in, got, tc.want)
			}
		})
	}
}
