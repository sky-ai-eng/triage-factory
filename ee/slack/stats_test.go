package slack

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// newTestStats builds an ingestStats recording into an in-memory
// ManualReader, so tests read back exactly what the instrumented code
// recorded — the OTel-idiomatic equivalent of a counter fake.
func newTestStats() (*ingestStats, *sdkmetric.ManualReader) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	return newIngestStats(provider), reader
}

// sumByAttr collects metricName's cumulative datapoints and sums them by
// attrKey's value — {outcome: count}, {transport: count}, {app_id: count}
// depending on the axis under test. A missing metric yields an empty map
// (asserting zero is then just len == 0 / absent key).
func sumByAttr(t *testing.T, reader *sdkmetric.ManualReader, metricName, attrKey string) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != metricName {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("metric %s: data is %T, want Sum[int64]", metricName, m.Data)
			}
			for _, dp := range sum.DataPoints {
				v, ok := dp.Attributes.Value(attribute.Key(attrKey))
				if !ok {
					t.Fatalf("metric %s: datapoint missing attribute %q (attrs: %v)", metricName, attrKey, dp.Attributes.Encoded(attribute.DefaultEncoder()))
				}
				out[v.AsString()] += dp.Value
			}
		}
	}
	return out
}

// TestIngestStats_OneOutcomePerDelivery drives one delivery down every
// pipeline return path and asserts each recorded exactly its own outcome —
// so the outcome sum equals deliveries received, the invariant that makes
// tf_slack_ingest_events_total readable as received/accepted/drop-by-reason
// without a separate "received" counter.
func TestIngestStats_OneOutcomePerDelivery(t *testing.T) {
	stats, reader := newTestStats()
	p, _, _, _ := newTestPipeline()
	p.stats = stats
	ws := testWorkspaceRow("org-1")
	ctx := context.Background()

	rootTS := "1600000000.000100"
	deliveries := []inboundMention{
		// accepted (mention): also mints the engaged thread the follow-ups
		// below land in (root mention → kind="thread").
		{Type: "app_mention", EventID: "Ev1", Channel: "C1", User: "U1", Text: "hi", TS: rootTS},
		// duplicate: same event_id redelivered.
		{Type: "app_mention", EventID: "Ev1", Channel: "C1", User: "U1", Text: "hi", TS: rootTS},
		// malformed: no event_id.
		{Type: "app_mention", Channel: "C1", User: "U1", TS: "1600000001.000100"},
		// self_or_bot: bot-authored mention.
		{Type: "app_mention", EventID: "Ev2", Channel: "C1", BotID: "B1", TS: "1600000002.000100"},
		// unsupported_type: an event this pipeline never ingests.
		{Type: "reaction_added", EventID: "Ev3", Channel: "C1", TS: "1600000003.000100"},
		// accepted (engaged-thread follow-up into Ev1's thread).
		{Type: "message", EventID: "Ev4", Channel: "C1", User: "U2", Text: "follow-up", TS: "1600000004.000100", ThreadTS: rootTS},
		// not_engaged: a thread the bot doesn't own.
		{Type: "message", EventID: "Ev5", Channel: "C1", User: "U2", Text: "x", TS: "1600000005.000100", ThreadTS: "1500000000.000001"},
		// unsupported_subtype: an edit.
		{Type: "message", Subtype: "message_changed", EventID: "Ev6", Channel: "C1", User: "U2", TS: "1600000006.000100", ThreadTS: rootTS},
		// not_thread_reply: root-channel chatter.
		{Type: "message", EventID: "Ev7", Channel: "C1", User: "U2", Text: "x", TS: "1600000007.000100"},
		// mention_dedup: threaded @-mention owned by its app_mention twin.
		{Type: "message", EventID: "Ev8", Channel: "C1", User: "U2", Text: "<@U0BOT> hey", TS: "1600000008.000100", ThreadTS: rootTS},
		// channel_moved: another channel's id changed.
		{Type: "channel_id_changed", EventID: "Ev9", OldChannelID: "G2", NewChannelID: "C2"},
	}
	for i, ev := range deliveries {
		if err := p.handleEventCallback(ctx, ws, ev); err != nil {
			t.Fatalf("delivery %d (%s/%s): %v", i, ev.Type, ev.EventID, err)
		}
	}

	want := map[string]int64{
		outcomeAccepted:        2,
		dropDuplicate:          1,
		dropMalformed:          1,
		dropSelfOrBot:          1,
		dropUnsupportedType:    1,
		dropNotEngaged:         1,
		dropUnsupportedSubtype: 1,
		dropNotThreadReply:     1,
		dropMentionDedup:       1,
		outcomeChannelMoved:    1,
	}
	got := sumByAttr(t, reader, "slack.ingest.events", "outcome")
	var total int64
	for outcome, n := range got {
		total += n
		if want[outcome] != n {
			t.Errorf("outcome %q = %d; want %d", outcome, n, want[outcome])
		}
	}
	for outcome, n := range want {
		if _, ok := got[outcome]; !ok {
			t.Errorf("outcome %q missing; want %d", outcome, n)
		}
	}
	if total != int64(len(deliveries)) {
		t.Errorf("sum over outcomes = %d; want %d (one outcome per delivery received)", total, len(deliveries))
	}

	byApp := sumByAttr(t, reader, "slack.ingest.events", "app_id")
	if byApp[ws.APIAppID] != int64(len(deliveries)) || len(byApp) != 1 {
		t.Errorf("app_id attribution = %v; want all %d under %q", byApp, len(deliveries), ws.APIAppID)
	}
}

// TestIngestStats_ErrorOutcome: a genuine pipeline failure records
// outcome=error (not a drop reason), keeping the received invariant intact
// while the transport withholds its ack for a redelivery.
func TestIngestStats_ErrorOutcome(t *testing.T) {
	stats, reader := newTestStats()
	p, _, deliveries, _ := newTestPipeline()
	p.stats = stats
	deliveries.err = errors.New("db down")

	ev := inboundMention{Type: "app_mention", EventID: "Ev1", Channel: "C1", User: "U1", TS: "1600000000.000100"}
	if err := p.handleEventCallback(context.Background(), testWorkspaceRow("org-1"), ev); err == nil {
		t.Fatal("handleEventCallback: want error when the delivery store fails")
	}

	got := sumByAttr(t, reader, "slack.ingest.events", "outcome")
	if got[outcomeError] != 1 || len(got) != 1 {
		t.Errorf("outcomes = %v; want exactly {error: 1}", got)
	}
}

// TestIngestStats_NilSafe: a pipeline without stats (every pre-existing
// test, and any future caller that doesn't care) records nothing and
// doesn't panic — same nil-safety contract as identity/channels/permalink.
func TestIngestStats_NilSafe(t *testing.T) {
	var s *ingestStats
	ctx := context.Background()
	s.recordIngest(ctx, "A1", outcomeAccepted)
	s.recordRetry(ctx, "A1", transportEventsAPI)
	s.recordRateLimited(ctx, "A1")
}

// ---------- webhook transport signals ----------

// TestHandleWebhook_RetryDelivery_CountedAndStillProcessed: a signed
// delivery carrying X-Slack-Retry-Num > 0 bumps the retry counter (the
// alertable early warning of the disable cascade) and is then processed
// exactly like a first attempt — the marker is telemetry, never a drop.
func TestHandleWebhook_RetryDelivery_CountedAndStillProcessed(t *testing.T) {
	r := newWebhookRig(t, webhookTestSigningRef, true)
	body := []byte(`{"type":"event_callback","team_id":"` + webhookTestWorkspaceID + `","api_app_id":"` + webhookTestAppID + `","event_id":"EvRetry1","event":{"type":"app_mention","channel":"C1","user":"U1","text":"hi","ts":"1600000000.000100"}}`)
	ts := nowTimestamp()

	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/slack/"+webhookTestOrgID, strings.NewReader(string(body)))
	req.SetPathValue("org_id", webhookTestOrgID)
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", sign(webhookTestSecret, ts, body))
	req.Header.Set("X-Slack-Retry-Num", "1")
	req.Header.Set("X-Slack-Retry-Reason", "http_timeout")
	rec := httptest.NewRecorder()
	r.h.handleWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(*r.published) != 1 {
		t.Errorf("published %d events; want 1 (a retry-marked delivery still processes)", len(*r.published))
	}
	byTransport := sumByAttr(t, r.metrics, "slack.retry.deliveries", "transport")
	if byTransport[transportEventsAPI] != 1 {
		t.Errorf("retry deliveries by transport = %v; want {events_api: 1}", byTransport)
	}
}

// TestHandleWebhook_RetryHeaderOnUnauthenticatedRequest_NotCounted: the
// retry marker is only trustworthy past the signature check — a forged
// request must not be able to inflate the operator's alert signal.
func TestHandleWebhook_RetryHeaderOnUnauthenticatedRequest_NotCounted(t *testing.T) {
	r := newWebhookRig(t, webhookTestSigningRef, true)
	body := []byte(`{"type":"event_callback","team_id":"` + webhookTestWorkspaceID + `","api_app_id":"` + webhookTestAppID + `","event_id":"EvForged","event":{"type":"app_mention"}}`)

	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/slack/"+webhookTestOrgID, strings.NewReader(string(body)))
	req.SetPathValue("org_id", webhookTestOrgID)
	req.Header.Set("X-Slack-Request-Timestamp", nowTimestamp())
	req.Header.Set("X-Slack-Signature", "v0=deadbeef")
	req.Header.Set("X-Slack-Retry-Num", "3")
	rec := httptest.NewRecorder()
	r.h.handleWebhook(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := sumByAttr(t, r.metrics, "slack.retry.deliveries", "transport"); len(got) != 0 {
		t.Errorf("retry deliveries = %v; want none recorded for an unauthenticated request", got)
	}
}

// TestHandleWebhook_AppRateLimited_CountedAndAcked: the app_rate_limited
// envelope (Slack is dropping this app's deliveries at the source) is
// acked, counted, and processes nothing.
func TestHandleWebhook_AppRateLimited_CountedAndAcked(t *testing.T) {
	r := newWebhookRig(t, webhookTestSigningRef, true)
	body := []byte(`{"type":"app_rate_limited","team_id":"` + webhookTestWorkspaceID + `","api_app_id":"` + webhookTestAppID + `","minute_rate_limited":1518467820}`)
	ts := nowTimestamp()
	rec := postSlackWebhook(t, r.h, webhookTestOrgID, body, ts, sign(webhookTestSecret, ts, body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (ack keeps the subscription healthy); body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out) != 0 {
		t.Errorf("body = %s; want the generic empty ack", rec.Body.String())
	}
	if len(*r.published) != 0 {
		t.Errorf("published %d events; want 0", len(*r.published))
	}
	byApp := sumByAttr(t, r.metrics, "slack.app.rate.limited", "app_id")
	if byApp[webhookTestAppID] != 1 {
		t.Errorf("rate limited by app = %v; want {%s: 1}", byApp, webhookTestAppID)
	}
}

// ---------- socket transport signals ----------

// TestProcessEventsAPIEnvelope_RetryEnvelopeCounted: Socket Mode's
// envelope-level retry_attempt is the transport's counterpart of
// X-Slack-Retry-Num — counted (transport=socket) before any payload
// dispatch, so even a payload this leaf doesn't process still measures the
// ack loop.
func TestProcessEventsAPIEnvelope_RetryEnvelopeCounted(t *testing.T) {
	stats, reader := newTestStats()
	c := &appConnection{appID: "A0SOCKET01", orgID: "org-1", stats: stats}

	env := socketEnvelope{
		Type:         "events_api",
		EnvelopeID:   "env-1",
		RetryAttempt: 2,
		RetryReason:  "timeout",
		// url_verification never occurs on Socket Mode; here it exercises
		// the "nothing to process" early return without needing stores.
		Payload: json.RawMessage(`{"type":"url_verification","api_app_id":"A0SOCKET01"}`),
	}
	if ack := c.processEventsAPIEnvelope(context.Background(), db.Stores{}, nil, env); !ack {
		t.Fatal("processEventsAPIEnvelope: want ack=true for a nothing-to-do payload")
	}

	byTransport := sumByAttr(t, reader, "slack.retry.deliveries", "transport")
	if byTransport[transportSocket] != 1 {
		t.Errorf("retry deliveries by transport = %v; want {socket: 1}", byTransport)
	}
	byApp := sumByAttr(t, reader, "slack.retry.deliveries", "app_id")
	if byApp["A0SOCKET01"] != 1 {
		t.Errorf("retry deliveries by app = %v; want {A0SOCKET01: 1}", byApp)
	}
}

// TestProcessEventsAPIEnvelope_FirstAttemptNotCounted: retry_attempt == 0
// (the normal case) records nothing.
func TestProcessEventsAPIEnvelope_FirstAttemptNotCounted(t *testing.T) {
	stats, reader := newTestStats()
	c := &appConnection{appID: "A0SOCKET01", orgID: "org-1", stats: stats}

	env := socketEnvelope{
		Type:       "events_api",
		EnvelopeID: "env-1",
		Payload:    json.RawMessage(`{"type":"url_verification","api_app_id":"A0SOCKET01"}`),
	}
	if ack := c.processEventsAPIEnvelope(context.Background(), db.Stores{}, nil, env); !ack {
		t.Fatal("processEventsAPIEnvelope: want ack=true")
	}
	if got := sumByAttr(t, reader, "slack.retry.deliveries", "transport"); len(got) != 0 {
		t.Errorf("retry deliveries = %v; want none for a first-attempt envelope", got)
	}
}

// gaugePoint is one datapoint of an int64 gauge: its attributes, as strings,
// and its value.
type gaugePoint struct {
	attrs map[string]string
	value int64
}

// gaugePoints collects metricName's datapoints from reader. A missing metric
// yields none.
func gaugePoints(t *testing.T, reader *sdkmetric.ManualReader, metricName string) []gaugePoint {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	var out []gaugePoint
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != metricName {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				t.Fatalf("metric %s: data is %T, want Gauge[int64]", metricName, m.Data)
			}
			for _, dp := range g.DataPoints {
				p := gaugePoint{attrs: map[string]string{}, value: dp.Value}
				for _, kv := range dp.Attributes.ToSlice() {
					p.attrs[string(kv.Key)] = kv.Value.String()
				}
				out = append(out, p)
			}
		}
	}
	return out
}

// TestSocketGauge_OnePointPerConnection: each running connection reports 1
// with its app, whether it is the deployment's, and its state, and a manager
// running none reports nothing.
func TestSocketGauge_OnePointPerConnection(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mgr := newSocketManager(db.Stores{}, nil, http.DefaultClient, nil)
	registerSocketGauge(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)), mgr)

	if got := gaugePoints(t, reader, "slack.socket.connections"); len(got) != 0 {
		t.Fatalf("points with no connections = %v; want none", got)
	}

	mgr.conns["A0DEPLOY01"] = &appConnection{appID: "A0DEPLOY01", deployment: true, status: connStatus{State: stateAuthFailed}}
	mgr.conns["A0ORGAPP01"] = &appConnection{appID: "A0ORGAPP01", orgID: "org-1", status: connStatus{State: stateOpen}}

	got := map[string]gaugePoint{}
	for _, p := range gaugePoints(t, reader, "slack.socket.connections") {
		got[p.attrs["app_id"]] = p
	}
	for app, want := range map[string]map[string]string{
		"A0DEPLOY01": {"app_id": "A0DEPLOY01", "deployment_app": "true", "state": stateAuthFailed},
		"A0ORGAPP01": {"app_id": "A0ORGAPP01", "deployment_app": "false", "state": stateOpen},
	} {
		p, ok := got[app]
		if !ok || p.value != 1 || !reflect.DeepEqual(p.attrs, want) {
			t.Errorf("point for %s = %+v (present %v); want value 1 with %v", app, p, ok, want)
		}
	}
	if len(got) != 2 {
		t.Errorf("points = %v; want one per connection", got)
	}
}

// TestDeploymentAppGauge: no series without a deployment app, 1 for a
// configured app behind HTTPS, and 0 with the reason for a misconfigured one
// or one behind a public URL that is not HTTPS.
func TestDeploymentAppGauge(t *testing.T) {
	app := deploymentApp{AppID: "A0DEPLOY01", ClientID: "1.2", ClientSecret: "s", SigningSecret: "x", Transport: transportEventsAPI}
	for _, tc := range []struct {
		name          string
		app           deploymentApp
		misconfigured bool
		publicURL     string
		want          []gaugePoint
	}{
		{"none", deploymentApp{}, false, "https://tf.example", nil},
		{"configured", app, false, "https://tf.example", []gaugePoint{{map[string]string{"app_id": "A0DEPLOY01"}, 1}}},
		{"insecure public URL", app, false, "http://tf.example", []gaugePoint{{map[string]string{"app_id": "A0DEPLOY01", "reason": deploymentAppInsecureURL}, 0}}},
		{"misconfigured", deploymentApp{}, true, "https://tf.example", []gaugePoint{{map[string]string{"reason": deploymentAppMisconfigured}, 0}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := sdkmetric.NewManualReader()
			registerDeploymentAppGauge(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)), tc.app, tc.misconfigured, staticURL(tc.publicURL))
			if got := gaugePoints(t, reader, "slack.deployment_app.up"); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("points = %+v; want %+v", got, tc.want)
			}
		})
	}
}

// TestLifecycleEnqueue_DropsAndCountsWhenFull: an event the backlog has room
// for is queued, and one it has none for is dropped and counted by type.
func TestLifecycleEnqueue_DropsAndCountsWhenFull(t *testing.T) {
	stats, reader := newTestStats()
	adapter := newLifecycleAdapter(db.Stores{}, slackHTTPClient, staticURL(""), nil)
	adapter.stats = stats
	ch := make(chan domain.Event, 1)

	adapter.enqueue(context.Background(), ch, domain.Event{EventType: "system:conversation:status"})
	adapter.enqueue(context.Background(), ch, domain.Event{EventType: "system:conversation:activity"})

	if len(ch) != 1 || (<-ch).EventType != "system:conversation:status" {
		t.Error("the first event was not queued")
	}
	if got := sumByAttr(t, reader, "slack.lifecycle.dropped", "event_type"); got["system:conversation:activity"] != 1 || len(got) != 1 {
		t.Errorf("lifecycle dropped = %v; want {system:conversation:activity: 1}", got)
	}
}
