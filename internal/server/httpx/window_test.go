package httpx

import (
	"net/url"
	"testing"
	"time"
)

func TestParseWindow(t *testing.T) {
	now := time.Date(2026, 6, 26, 15, 4, 5, 0, time.UTC)
	monthStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		q         map[string]string
		wantSince time.Time
		wantUntil time.Time
		wantErr   bool
	}{
		{"both absent → current month to now", nil, monthStart, now, false},
		{"since RFC3339", map[string]string{"since": "2026-03-01T08:30:00Z"}, time.Date(2026, 3, 1, 8, 30, 0, 0, time.UTC), time.Time{}, false},
		{"until bare date → UTC midnight", map[string]string{"until": "2026-04-01"}, time.Time{}, time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), false},
		{"both bare dates", map[string]string{"since": "2026-03-01", "until": "2026-04-01"}, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), false},
		{"invalid since", map[string]string{"since": "not-a-time"}, time.Time{}, time.Time{}, true},
		{"invalid until", map[string]string{"until": "not-a-time"}, time.Time{}, time.Time{}, true},
		{"since after until", map[string]string{"since": "2026-04-01", "until": "2026-03-01"}, time.Time{}, time.Time{}, true},
		{"since equals until", map[string]string{"since": "2026-03-01", "until": "2026-03-01"}, time.Time{}, time.Time{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q := url.Values{}
			for k, v := range tc.q {
				q.Set(k, v)
			}
			since, until, errMsg := ParseWindow(q, now)
			if tc.wantErr {
				if errMsg == "" {
					t.Fatalf("want error, got since=%v until=%v", since, until)
				}
				return
			}
			if errMsg != "" {
				t.Fatalf("unexpected error: %s", errMsg)
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
