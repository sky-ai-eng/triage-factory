package ctlbus_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/ctlbus"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/pgnotify"
)

// TestPublish_RoundTrip pins the real Postgres NOTIFY round-trip for
// Publish's wire format: a Message Published on the pooled admin
// connection reaches a tf_ctl listener — a pgnotify.Listener, the same
// primitive internal/app's unified tf_ctl dispatcher consumes through —
// with its fields intact after the JSON round-trip.
func TestPublish_RoundTrip(t *testing.T) {
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

	received := make(chan ctlbus.Message, 8)
	listener := pgnotify.NewListener(dsn, ctlbus.Channel, func(payload string) {
		var m ctlbus.Message
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			t.Errorf("received payload did not unmarshal as ctlbus.Message: %v (payload %q)", err, payload)
			return
		}
		received <- m
	})
	go listener.Run(ctx)

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

	// Drain any extra pings still in flight from the readiness loop before
	// asserting on the trigger message.
	for {
		select {
		case got := <-received:
			if got.Kind == "ping" {
				continue
			}
			if got != want {
				t.Fatalf("received = %+v, want %+v", got, want)
			}
			return
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for the relayed message")
		}
	}
}
