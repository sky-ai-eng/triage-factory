package httpx

import (
	"strings"
	"time"
)

// ParseWindow resolves the [since, until) read window of a statistic node
// from the query string. Both absent → the current calendar month (UTC) up to
// now. Each present value parses as RFC3339 or YYYY-MM-DD (a bare date → UTC
// midnight). A malformed value, or an until not after since, is recorded on v
// as an INVALID_PARAM naming its parameter, and the handler flushes v. q is
// the request's query: the QueryValues an op hands its handler, or what
// StrictQuery reads.
func ParseWindow(v *Validation, q interface{ Get(string) string }, now time.Time) (since, until time.Time) {
	sinceStr := strings.TrimSpace(q.Get("since"))
	untilStr := strings.TrimSpace(q.Get("until"))
	if sinceStr == "" && untilStr == "" {
		since = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		return since, now
	}
	parsed := true
	if sinceStr != "" {
		t, err := ParseWindowTime(sinceStr)
		if err != nil {
			v.Param("since", "since must be an RFC 3339 timestamp or a YYYY-MM-DD date")
			parsed = false
		}
		since = t
	}
	if untilStr != "" {
		t, err := ParseWindowTime(untilStr)
		if err != nil {
			v.Param("until", "until must be an RFC 3339 timestamp or a YYYY-MM-DD date")
			parsed = false
		}
		until = t
	}
	// When both bounds are supplied, reject a non-positive window (since >=
	// until). The read is half-open [since, until), so since == until is empty
	// and since > until is inverted — both would otherwise return a misleading
	// 200 with no rows and mask a client bug. Single-bound requests leave the
	// other side open (zero), so there's no ordering to check.
	if parsed && !since.IsZero() && !until.IsZero() && !since.Before(until) {
		v.Param("until", "until must be after since")
	}
	return since, until
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
