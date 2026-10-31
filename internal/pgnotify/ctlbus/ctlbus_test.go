package ctlbus

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
)

// captureExecer stands in for the admin pool: Publish's only contact with the
// database is one pg_notify() call, so recording its arguments is the whole of
// what a publish does.
type captureExecer struct {
	channel string
	payload string
	err     error
	calls   int
}

func (c *captureExecer) ExecContext(_ context.Context, _ string, args ...any) (sql.Result, error) {
	c.calls++
	if len(args) == 2 {
		c.channel, _ = args[0].(string)
		c.payload, _ = args[1].(string)
	}
	return nil, c.err
}

// A memory_owed message carries the two field groups its dispatch reads and
// nothing else: the org always, the conversation only when one is named. The
// empty-id form is not a malformed targeted nudge — it is the org-wide re-kick
// a configuration save publishes — so conversation_id must be ABSENT from the
// payload rather than present and empty, which is what omitempty buys and what
// a reader of the wire has to be able to tell apart.
func TestPublish_MemoryOwed(t *testing.T) {
	for _, tc := range []struct {
		name           string
		conversationID string
		wantConvKey    bool
	}{
		{name: "one ended conversation", conversationID: "conv-1", wantConvKey: true},
		{name: "the org-wide re-kick", conversationID: "", wantConvKey: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ex := &captureExecer{}
			if err := Publish(context.Background(), ex, Message{
				Kind: "memory_owed", OrgID: "org-1", ConversationID: tc.conversationID,
			}); err != nil {
				t.Fatalf("Publish: %v", err)
			}
			if ex.channel != Channel {
				t.Errorf("channel = %q, want %q", ex.channel, Channel)
			}
			var raw map[string]any
			if err := json.Unmarshal([]byte(ex.payload), &raw); err != nil {
				t.Fatalf("payload %q is not JSON: %v", ex.payload, err)
			}
			if raw["kind"] != "memory_owed" {
				t.Errorf("kind = %v, want memory_owed", raw["kind"])
			}
			if raw["org_id"] != "org-1" {
				t.Errorf("org_id = %v, want org-1", raw["org_id"])
			}
			got, present := raw["conversation_id"]
			if present != tc.wantConvKey {
				t.Errorf("conversation_id present = %v, want %v (payload %q)", present, tc.wantConvKey, ex.payload)
			}
			if tc.wantConvKey && got != tc.conversationID {
				t.Errorf("conversation_id = %v, want %q", got, tc.conversationID)
			}

			// Round-trips back through the same struct the dispatcher decodes
			// into: the doorbell's two ids must survive the wire intact, since
			// nothing downstream re-reads them from anywhere else.
			var back Message
			if err := json.Unmarshal([]byte(ex.payload), &back); err != nil {
				t.Fatalf("decode as Message: %v", err)
			}
			if back.Kind != "memory_owed" || back.OrgID != "org-1" || back.ConversationID != tc.conversationID {
				t.Errorf("round-trip = %+v, want kind=memory_owed org=org-1 conversation=%q", back, tc.conversationID)
			}
		})
	}
}

// A publish that fails is one dropped doorbell, which the caller is expected to
// absorb — but it must reach the caller as an error rather than as a silent
// success, because publishCtl's decision to log-and-continue is the relay's to
// make, not this function's.
func TestPublish_ErrorReachesTheCaller(t *testing.T) {
	boom := errors.New("connection reset")
	ex := &captureExecer{err: boom}
	err := Publish(context.Background(), ex, Message{Kind: "memory_owed", OrgID: "org-1"})
	if !errors.Is(err, boom) {
		t.Fatalf("Publish error = %v, want it to wrap %v", err, boom)
	}
}
