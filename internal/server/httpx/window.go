package httpx

import (
	"net/url"
	"strings"
	"time"
)

// ParseWindow resolves the [since, until) read window of a statistic node
// from the query string. Both absent → the current calendar month (UTC) up to
// now. Each present value parses as RFC3339 or YYYY-MM-DD (a bare date → UTC
// midnight). Returns a non-empty errMsg on a malformed value (the handler 400s
// with it).
func ParseWindow(q url.Values, now time.Time) (since, until time.Time, errMsg string) {
	sinceStr := strings.TrimSpace(q.Get("since"))
	untilStr := strings.TrimSpace(q.Get("until"))
	if sinceStr == "" && untilStr == "" {
		since = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		until = now
		return since, until, ""
	}
	if sinceStr != "" {
		t, err := ParseWindowTime(sinceStr)
		if err != nil {
			return time.Time{}, time.Time{}, "invalid 'since': want RFC3339 or YYYY-MM-DD"
		}
		since = t
	}
	if untilStr != "" {
		t, err := ParseWindowTime(untilStr)
		if err != nil {
			return time.Time{}, time.Time{}, "invalid 'until': want RFC3339 or YYYY-MM-DD"
		}
		until = t
	}
	// When both bounds are supplied, reject a non-positive window (since >=
	// until). The read is half-open [since, until), so since == until is empty
	// and since > until is inverted — both would otherwise return a misleading
	// 200 with no rows and mask a client bug. Single-bound requests leave the
	// other side open (zero), so there's no ordering to check.
	if !since.IsZero() && !until.IsZero() && !since.Before(until) {
		return time.Time{}, time.Time{}, "'since' must be before 'until'"
	}
	return since, until, ""
}

// ParseWindowTime accepts a full RFC3339 timestamp or a bare YYYY-MM-DD date
// (interpreted as UTC midnight). Always returns UTC so the day-bucketing and
// the view's occurred_at compare in one zone.
func ParseWindowTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}
