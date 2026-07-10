package ctlbus_test

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/ctlbus"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
)

// TestListenPublish_RoundTrip pins the real Postgres NOTIFY/LISTEN
// round-trip: a message Published on the pooled admin connection reaches
// a Listen()er on a dedicated connection, with its fields intact.
func TestListenPublish_RoundTrip(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	dsn, err := h.Container.ConnectionString(context.Background(), "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	// The pgtest container's raw ConnectionString connects as the
	// `postgres` role, which the supabase image demotes to non-superuser
	// but still LOGIN-capable — sufficient for LISTEN (no elevated
	// privilege required).

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	received := make(chan ctlbus.Message, 1)
	go ctlbus.Listen(ctx, dsn, func(m ctlbus.Message) { received <- m })

	// Give the listener a moment to establish its LISTEN before publishing
	// — NOTIFY delivers only to sessions already listening.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := ctlbus.Publish(context.Background(), h.AdminDB, ctlbus.Message{Kind: "ping"}); err != nil {
			t.Fatalf("publish ping: %v", err)
		}
		select {
		case <-received:
			goto listening
		case <-time.After(200 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("listener never became ready")
			}
		}
	}
listening:

	want := ctlbus.Message{Kind: "trigger", Manager: "scorer", OrgID: "org-1", Force: true}
	if err := ctlbus.Publish(context.Background(), h.AdminDB, want); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case got := <-received:
		if got != want {
			t.Fatalf("received = %+v, want %+v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the relayed message")
	}
}
