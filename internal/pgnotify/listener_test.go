package pgnotify_test

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/pgnotify"
)

// TestListener_DeliversNotifications pins the core LISTEN/NOTIFY round
// trip: a Listener on a channel observes payloads published via
// pgnotify.Notify on the SAME channel, and never observes payloads
// published on a DIFFERENT channel. Skips cleanly when Docker isn't
// available (pgtest.Shared).
func TestListener_DeliversNotifications(t *testing.T) {
	h := pgtest.Shared(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dsn, err := h.Container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	const channel = "pgnotify_test_channel"
	const otherChannel = "pgnotify_test_other_channel"
	received := make(chan string, 8)
	l := pgnotify.NewListener(dsn, channel, func(payload string) {
		received <- payload
	})
	go l.Run(ctx)

	// The listener's LISTEN registration races this test's first NOTIFY —
	// retry until it's observed or the deadline expires, exactly the
	// pattern production callers pair with the backstop poll for.
	deadline := time.Now().Add(10 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		if err := pgnotify.Notify(ctx, h.AdminDB, channel, "hello"); err != nil {
			t.Fatalf("notify: %v", err)
		}
		select {
		case got = <-received:
		case <-time.After(300 * time.Millisecond):
			continue
		}
		break
	}
	if got != "hello" {
		t.Fatalf("did not receive the notification before the deadline (got %q)", got)
	}

	// A notification on a different channel must never arrive here.
	if err := pgnotify.Notify(ctx, h.AdminDB, otherChannel, "should-not-arrive"); err != nil {
		t.Fatalf("notify other channel: %v", err)
	}
	select {
	case leaked := <-received:
		t.Fatalf("received a notification from an unsubscribed channel: %q", leaked)
	case <-time.After(500 * time.Millisecond):
	}
}
