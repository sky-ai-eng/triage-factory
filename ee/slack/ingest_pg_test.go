package slack

// pgtest-backed suite for the ingest pipeline's delivery transaction: the
// delivery mark, a mention's thread and the event's queue row commit in one
// admin-pool transaction, so a write that fails after the dedup leaves none
// of them behind and Slack's redelivery publishes the message once.

import (
	"context"
	"errors"
	"testing"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// failAfterWriteTx wraps the real system door and makes the thread write or
// the enqueue fail after it has written, so the rollback has real rows to
// undo.
type failAfterWriteTx struct {
	db.TxRunner
	threadErr, enqueueErr error
}

func (f *failAfterWriteTx) SystemWithTx(ctx context.Context, orgID string, fn func(db.SystemTxStores) error) error {
	return f.TxRunner.SystemWithTx(ctx, orgID, func(tx db.SystemTxStores) error {
		if f.enqueueErr != nil {
			tx.EventQueue = failAfterEnqueue{EventQueueStore: tx.EventQueue, err: f.enqueueErr}
		}
		if f.threadErr != nil {
			bundle := *slackstore.FromSystemTx(tx)
			bundle.Channels = failAfterThread{ChannelRegistryStore: bundle.Channels, err: f.threadErr}
			tx.Ext = map[string]any{slackstore.ExtKey: &bundle}
		}
		return fn(tx)
	})
}

type failAfterEnqueue struct {
	db.EventQueueStore
	err error
}

func (q failAfterEnqueue) Enqueue(ctx context.Context, orgID string, evt domain.Event, traceparent string) (string, error) {
	if _, err := q.EventQueueStore.Enqueue(ctx, orgID, evt, traceparent); err != nil {
		return "", err
	}
	return "", q.err
}

type failAfterThread struct {
	slackstore.ChannelRegistryStore
	err error
}

func (c failAfterThread) FindOrCreateThreadSystem(ctx context.Context, orgID, channelID, threadTS, kind, title, url string) (slackstore.Thread, error) {
	if _, err := c.ChannelRegistryStore.FindOrCreateThreadSystem(ctx, orgID, channelID, threadTS, kind, title, url); err != nil {
		return slackstore.Thread{}, err
	}
	return slackstore.Thread{}, c.err
}

// ingestRows is what a delivery leaves in the database.
type ingestRows struct {
	deliveries, threads, events, queued int
}

func countIngestRows(t *testing.T, h *pgtest.Harness, orgID, apiAppID string) ingestRows {
	t.Helper()
	var r ingestRows
	for _, q := range []struct {
		dst   *int
		query string
		arg   string
	}{
		{&r.deliveries, `SELECT count(*) FROM slack_event_deliveries WHERE api_app_id = $1`, apiAppID},
		{&r.threads, `SELECT count(*) FROM entities WHERE org_id = $1 AND source = 'slack'`, orgID},
		{&r.events, `SELECT count(*) FROM events WHERE org_id = $1 AND event_type = 'slack:message'`, orgID},
		{&r.queued, `SELECT count(*) FROM event_queue WHERE org_id = $1 AND event_type = 'slack:message'`, orgID},
	} {
		if err := h.AdminDB.QueryRow(q.query, q.arg).Scan(q.dst); err != nil {
			t.Fatalf("count (%s): %v", q.query, err)
		}
	}
	return r
}

func TestIngestPipeline_Postgres_FailureAfterDedupRollsBack(t *testing.T) {
	cases := []struct {
		name string
		fail func(tx *failAfterWriteTx, err error)
		// followUp runs the failing delivery as an engaged-thread follow-up
		// to a mention delivered first, rather than as the mention itself.
		followUp bool
	}{
		{name: "mention thread write", fail: func(tx *failAfterWriteTx, err error) { tx.threadErr = err }},
		{name: "mention enqueue", fail: func(tx *failAfterWriteTx, err error) { tx.enqueueErr = err }},
		{name: "follow-up enqueue", fail: func(tx *failAfterWriteTx, err error) { tx.enqueueErr = err }, followUp: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := pgtest.Shared(t)
			h.Reset(t)
			orgID, _, _ := pgtest.SeedOrgWithUser(t, h, "slack-ingest-tx")
			stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
			bundle := slackstore.FromStores(stores)
			tx := &failAfterWriteTx{TxRunner: stores.Tx}
			var published []domain.Event
			p := &ingestPipeline{
				tx:         tx,
				deliveries: bundle.Deliveries,
				channels:   bundle.Channels,
				publish:    func(_ context.Context, evt domain.Event) { published = append(published, evt) },
			}
			ws := testWorkspaceRow(orgID)
			ctx := context.Background()

			const rootTS = "1600000000.000100"
			ev := inboundMention{Type: "app_mention", EventID: "Ev-root", Channel: "C1", User: "U1", Text: "<@U0BOT> look", TS: rootTS}
			before := ingestRows{}
			if tc.followUp {
				if err := p.handleEventCallback(ctx, ws, ev); err != nil {
					t.Fatalf("root mention: %v", err)
				}
				before = ingestRows{deliveries: 1, threads: 1, events: 1, queued: 1}
				ev = inboundMention{Type: "message", EventID: "Ev-reply", Channel: "C1", User: "U2", Text: "more", TS: "1600000000.000200", ThreadTS: rootTS}
			}

			boom := errors.New("boom")
			tc.fail(tx, boom)
			if err := p.handleEventCallback(ctx, ws, ev); !errors.Is(err, boom) {
				t.Fatalf("handleEventCallback = %v; want the injected failure", err)
			}
			if got := countIngestRows(t, h, orgID, ws.APIAppID); got != before {
				t.Fatalf("rows after the failure = %+v; want %+v (the mark, thread and event rolled back together)", got, before)
			}
			if len(published) != before.events {
				t.Fatalf("published %d events after the failure; want %d", len(published), before.events)
			}

			tc.fail(tx, nil)
			for i := range 2 {
				if err := p.handleEventCallback(ctx, ws, ev); err != nil {
					t.Fatalf("redelivery %d: %v", i+1, err)
				}
			}
			want := ingestRows{deliveries: before.deliveries + 1, threads: 1, events: before.events + 1, queued: before.queued + 1}
			if got := countIngestRows(t, h, orgID, ws.APIAppID); got != want {
				t.Errorf("rows after the redeliveries = %+v; want %+v", got, want)
			}
			if len(published) != want.events {
				t.Fatalf("published %d events across the redeliveries; want %d", len(published), want.events)
			}
			last := published[len(published)-1]
			var queuedID string
			if err := h.AdminDB.QueryRow(`SELECT event_id::text FROM event_queue WHERE org_id = $1 AND event_id = $2`, orgID, last.ID).Scan(&queuedID); err != nil {
				t.Errorf("published event %q has no queue row: %v", last.ID, err)
			}
		})
	}
}
