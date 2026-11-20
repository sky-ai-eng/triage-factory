package db

import (
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// A bump in the same clock tick as the escalation before it returns a row
// whose escalated_at equals its last_seen_at. The counts say it is a bump.
func TestNotificationUpsertOf_AtAnExactTie(t *testing.T) {
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	row := func(seen, escalatedSeen int) domain.Notification {
		return domain.Notification{FirstSeenAt: at, LastSeenAt: at, EscalatedAt: at, SeenCount: seen, EscalatedSeenCount: escalatedSeen}
	}
	for name, c := range map[string]struct {
		n                 domain.Notification
		opened, escalated bool
	}{
		"insert":                      {row(1, 1), true, false},
		"bump in the insert's tick":   {row(2, 1), false, false},
		"escalation":                  {row(3, 3), false, true},
		"bump in the escalation tick": {row(4, 3), false, false},
	} {
		if u := NotificationUpsertOf(c.n); u.Opened != c.opened || u.Escalated != c.escalated {
			t.Errorf("%s: opened %v escalated %v, want %v %v", name, u.Opened, u.Escalated, c.opened, c.escalated)
		}
	}
}
