package slack

import "testing"

// TestParseSlackTSParts pins the (seconds, fractional-nanoseconds) decomposition
// parseSlackTSParts uses instead of a float64 parse — see sortMessagesByTS's
// doc for why a parsed float64 risks misordering two close-together Slack
// timestamps.
func TestParseSlackTSParts(t *testing.T) {
	cases := []struct {
		ts           string
		wantSeconds  int64
		wantFracNano int64
	}{
		{"1355517523.000005", 1355517523, 5000},
		{"1700000000.000001", 1700000000, 1000},
		{"1700000000.000002", 1700000000, 2000},
		{"1700000000", 1700000000, 0},
		{"", 0, 0},
		{"not-a-number", 0, 0},
	}
	for _, c := range cases {
		sec, frac := parseSlackTSParts(c.ts)
		if sec != c.wantSeconds || frac != c.wantFracNano {
			t.Errorf("parseSlackTSParts(%q) = (%d, %d), want (%d, %d)", c.ts, sec, frac, c.wantSeconds, c.wantFracNano)
		}
	}
}

// TestSortMessagesByTS_SameSecondDistinctMicroseconds pins that messages
// sent within the same second sort correctly by their fractional part — the
// exact scenario a parsed-float64 comparison risks collapsing/misordering,
// since a 10-digit-seconds + 6-digit-fraction Slack ts sits at float64's
// precision ceiling (~15-17 significant decimal digits).
func TestSortMessagesByTS_SameSecondDistinctMicroseconds(t *testing.T) {
	msgs := []slackMessage{
		{TS: "1700000000.000009", Text: "third"},
		{TS: "1700000000.000001", Text: "first"},
		{TS: "1700000000.000005", Text: "second"},
	}
	sortMessagesByTS(msgs)
	want := []string{"first", "second", "third"}
	for i, m := range msgs {
		if m.Text != want[i] {
			t.Errorf("position %d = %q, want %q (full order: %+v)", i, m.Text, want[i], msgs)
		}
	}
}

// TestSortMessagesByTS_DistinctSeconds pins ordinary cross-second ordering
// still works (the common case, unaffected by the fractional-precision fix).
func TestSortMessagesByTS_DistinctSeconds(t *testing.T) {
	msgs := []slackMessage{
		{TS: "1700000002.000000", Text: "third"},
		{TS: "1700000000.500000", Text: "first"},
		{TS: "1700000001.000000", Text: "second"},
	}
	sortMessagesByTS(msgs)
	want := []string{"first", "second", "third"}
	for i, m := range msgs {
		if m.Text != want[i] {
			t.Errorf("position %d = %q, want %q (full order: %+v)", i, m.Text, want[i], msgs)
		}
	}
}
