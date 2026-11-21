package httpx

import (
	"net/url"
	"slices"
	"testing"
	"time"
)

func TestParseWindow(t *testing.T) {
	badSince := ErrorItem{Reason: ReasonInvalidParam, Field: "since", Message: "since must be an RFC 3339 timestamp or a YYYY-MM-DD date"}
	badUntil := ErrorItem{Reason: ReasonInvalidParam, Field: "until", Message: "until must be an RFC 3339 timestamp or a YYYY-MM-DD date"}
	inverted := ErrorItem{Reason: ReasonInvalidParam, Field: "until", Message: "until must be after since"}
	now := time.Date(2026, 6, 26, 15, 4, 5, 0, time.UTC)
	monthStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		q         map[string]string
		wantSince time.Time
		wantUntil time.Time
		wantFault []ErrorItem
	}{
		{"both absent → current month to now", nil, monthStart, now, nil},
		{"since RFC3339", map[string]string{"since": "2026-03-01T08:30:00Z"}, time.Date(2026, 3, 1, 8, 30, 0, 0, time.UTC), time.Time{}, nil},
		{"until bare date → UTC midnight", map[string]string{"until": "2026-04-01"}, time.Time{}, time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), nil},
		{"both bare dates", map[string]string{"since": "2026-03-01", "until": "2026-04-01"}, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), nil},
		{"invalid since", map[string]string{"since": "not-a-time"}, time.Time{}, time.Time{}, []ErrorItem{badSince}},
		{"invalid until", map[string]string{"until": "not-a-time"}, time.Time{}, time.Time{}, []ErrorItem{badUntil}},
		{"both invalid", map[string]string{"since": "x", "until": "y"}, time.Time{}, time.Time{}, []ErrorItem{badSince, badUntil}},
		{"invalid since beside a valid until", map[string]string{"since": "x", "until": "2026-03-01"}, time.Time{}, time.Time{}, []ErrorItem{badSince}},
		{"since after until", map[string]string{"since": "2026-04-01", "until": "2026-03-01"}, time.Time{}, time.Time{}, []ErrorItem{inverted}},
		{"since equals until", map[string]string{"since": "2026-03-01", "until": "2026-03-01"}, time.Time{}, time.Time{}, []ErrorItem{inverted}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q := url.Values{}
			for k, v := range tc.q {
				q.Set(k, v)
			}
			var v Validation
			since, until := ParseWindow(&v, q, now)
			if faults := v.Items(); tc.wantFault != nil || len(faults) > 0 {
				if !slices.Equal(faults, tc.wantFault) {
					t.Fatalf("faults = %+v, want %+v", faults, tc.wantFault)
				}
				return
			}
			if !since.Equal(tc.wantSince) {
				t.Errorf("since = %v, want %v", since, tc.wantSince)
			}
			if !until.Equal(tc.wantUntil) {
				t.Errorf("until = %v, want %v", until, tc.wantUntil)
			}
		})
	}
}
