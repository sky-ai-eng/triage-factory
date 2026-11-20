package slack

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
)

const (
	deployTestAppID     = "A0DEPLOY01"
	deployTestSecret    = "deployment-signing-secret"
	deployTestAppToken  = "xapp-deployment-token"
	deployTestOrgID     = "org-deploy-1"
	deployTestOtherOrg  = "org-deploy-2"
	deployTestWorkspace = "T0DEPLOY01"
)

// eventsAPIDeploymentApp is a deployment app configured for the Events API.
func eventsAPIDeploymentApp() deploymentApp {
	return deploymentApp{AppID: deployTestAppID, ClientID: "1.2", ClientSecret: "cs", SigningSecret: deployTestSecret, Transport: transportEventsAPI}
}

// socketDeploymentApp is a deployment app configured for Socket Mode.
func socketDeploymentApp() deploymentApp {
	return deploymentApp{AppID: deployTestAppID, ClientID: "1.2", ClientSecret: "cs", AppToken: deployTestAppToken, Transport: transportSocket}
}

// deploymentRow is a connection through the deployment's app.
func deploymentRow(orgID, workspaceID string, active bool) *slackstore.Connection {
	return &slackstore.Connection{
		WorkspaceID: workspaceID, APIAppID: deployTestAppID, OrgID: orgID,
		Source: slackstore.SourceDeployment, Active: active, BotUserID: "U0DEPBOT",
		BotTokenRef: "slack_ws_" + workspaceID + "_" + deployTestAppID + "_bot_token",
	}
}

// entitledOrgs grants the slack feature to the orgs it names and to no other.
type entitledOrgs map[string]bool

func (e entitledOrgs) For(orgID string) entitlements.Entitlements {
	if e[orgID] {
		return entitlements.New([]entitlements.Feature{entitlements.FeatureSlack}, nil)
	}
	return entitlements.New(nil, nil)
}

// countingConnections counts the store reads a receiver makes.
type countingConnections struct {
	slackstore.ConnectionStore
	reads atomic.Int64
	err   error
}

func (c *countingConnections) GetByWorkspaceAppSystem(ctx context.Context, workspaceID, apiAppID string) (*slackstore.Connection, error) {
	c.reads.Add(1)
	if c.err != nil {
		return nil, c.err
	}
	return c.ConnectionStore.GetByWorkspaceAppSystem(ctx, workspaceID, apiAppID)
}

// deploymentIngestRig is one pipeline with both of the deployment app's
// receivers in front of it: POST /api/webhooks/slack, and a Socket Mode
// connection's envelope handling, called directly so a test needs no socket.
type deploymentIngestRig struct {
	webhook    *webhookHandler
	conn       *appConnection
	pipeline   *ingestPipeline
	stores     db.Stores
	reads      *countingConnections
	published  *[]domain.Event
	deliveries *fakeDeliveries
	metrics    *sdkmetric.ManualReader
}

// newDeploymentIngestRig builds the rig over rows, with the slack feature
// granted to deployTestOrgID and deployTestOtherOrg.
func newDeploymentIngestRig(t *testing.T, rows ...*slackstore.Connection) *deploymentIngestRig {
	t.Helper()
	entitlements.RegisterProvider(entitledOrgs{deployTestOrgID: true, deployTestOtherOrg: true})
	t.Cleanup(entitlements.Reset)

	reads := &countingConnections{ConnectionStore: newFakeConnectionStore(rows...)}
	stores := db.Stores{
		Secrets: &fakeSecretStore{values: map[string]string{}},
		Ext:     map[string]any{slackstore.ExtKey: &slackstore.Bundle{Connections: reads}},
	}
	published := &[]domain.Event{}
	deliveries := newFakeDeliveries()
	stats, metrics := newTestStats()
	pipeline := &ingestPipeline{
		connections: reads,
		deliveries:  deliveries,
		channels:    newFakeChannelRegistryFor(newFakeEntities()),
		publish:     func(_ context.Context, evt domain.Event) { *published = append(*published, evt) },
		stats:       stats,
	}
	withFakeIngestTx(pipeline)
	return &deploymentIngestRig{
		webhook:    &webhookHandler{stores: stores, pipeline: pipeline, stats: stats, deployment: eventsAPIDeploymentApp()},
		conn:       &appConnection{appID: deployTestAppID, deployment: true, appToken: deployTestAppToken, sightings: map[string]*sightingView{}},
		pipeline:   pipeline,
		stores:     stores,
		reads:      reads,
		published:  published,
		deliveries: deliveries,
		metrics:    metrics,
	}
}

// postDeploymentWebhook posts body to POST /api/webhooks/slack.
func postDeploymentWebhook(t *testing.T, h *webhookHandler, body []byte, timestamp, sigHeader string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/slack", bytes.NewReader(body))
	if timestamp != "" {
		req.Header.Set("X-Slack-Request-Timestamp", timestamp)
	}
	if sigHeader != "" {
		req.Header.Set("X-Slack-Signature", sigHeader)
	}
	rec := httptest.NewRecorder()
	h.handleDeploymentWebhook(rec, req)
	return rec
}

// postSignedDeployment posts payload signed with the deployment app's
// signing secret.
func postSignedDeployment(t *testing.T, h *webhookHandler, payload map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body := mustJSON(t, payload)
	ts := nowTimestamp()
	return postDeploymentWebhook(t, h, body, ts, sign(deployTestSecret, ts, body))
}

// deploymentTransports delivers one event_callback payload through each of
// the deployment app's receivers, reporting whether the receiver acked it:
// a 200 with the empty body, or an acked envelope.
var deploymentTransports = []struct {
	name    string
	deliver func(t *testing.T, r *deploymentIngestRig, payload map[string]any) bool
}{
	{"events_api", func(t *testing.T, r *deploymentIngestRig, payload map[string]any) bool {
		rec := postSignedDeployment(t, r.webhook, payload)
		return rec.Code == http.StatusOK && strings.TrimSpace(rec.Body.String()) == "{}"
	}},
	{"socket", func(t *testing.T, r *deploymentIngestRig, payload map[string]any) bool {
		env := socketEnvelope{Type: "events_api", EnvelopeID: "env-1", Payload: mustJSON(t, payload)}
		return r.conn.processEventsAPIEnvelope(t.Context(), r.stores, r.pipeline, env)
	}},
}

// TestDeploymentDelivery_Lookup runs one case per row of
// deliverDeploymentEvent's table through each transport.
func TestDeploymentDelivery_Lookup(t *testing.T) {
	const unentitledOrg = "org-deploy-unentitled"
	mention := mentionInner("C1", "U1", "1700000000.000100")
	for _, tc := range []struct {
		name    string
		rows    []*slackstore.Connection
		payload map[string]any
		wantOrg string // "" when nothing is published
	}{
		{
			// The row is one the app id check alone keeps the delivery from:
			// with it gone, every later check passes.
			name: "another app's id",
			rows: []*slackstore.Connection{{
				WorkspaceID: deployTestWorkspace, APIAppID: "A0ORGAPP01", OrgID: deployTestOrgID,
				Source: slackstore.SourceDeployment, Active: true,
			}},
			payload: eventCallbackPayload(deployTestWorkspace, "A0ORGAPP01", "Ev-other-app", mention),
		},
		{
			name:    "no row for the workspace",
			rows:    []*slackstore.Connection{deploymentRow(deployTestOrgID, "T0ELSEWHERE", true)},
			payload: eventCallbackPayload(deployTestWorkspace, deployTestAppID, "Ev-no-row", mention),
		},
		{
			name: "an org's own row holding the deployment app's id",
			rows: []*slackstore.Connection{{
				WorkspaceID: deployTestWorkspace, APIAppID: deployTestAppID, OrgID: deployTestOrgID,
				Source: slackstore.SourceOrg, Active: true, Transport: transportEventsAPI, SigningSecretRef: "ref",
			}},
			payload: eventCallbackPayload(deployTestWorkspace, deployTestAppID, "Ev-org-row", mention),
		},
		{
			name:    "inactive connection",
			rows:    []*slackstore.Connection{deploymentRow(deployTestOrgID, deployTestWorkspace, false)},
			payload: eventCallbackPayload(deployTestWorkspace, deployTestAppID, "Ev-inactive", mention),
		},
		{
			name:    "unentitled org",
			rows:    []*slackstore.Connection{deploymentRow(unentitledOrg, deployTestWorkspace, true)},
			payload: eventCallbackPayload(deployTestWorkspace, deployTestAppID, "Ev-unentitled", mention),
		},
		{
			name:    "active connection in an entitled org",
			rows:    []*slackstore.Connection{deploymentRow(deployTestOrgID, deployTestWorkspace, true)},
			payload: eventCallbackPayload(deployTestWorkspace, deployTestAppID, "Ev-delivered", mention),
			wantOrg: deployTestOrgID,
		},
	} {
		for _, tr := range deploymentTransports {
			t.Run(tc.name+"/"+tr.name, func(t *testing.T) {
				r := newDeploymentIngestRig(t, tc.rows...)
				if !tr.deliver(t, r, tc.payload) {
					t.Fatal("delivery not acked; want every case acked")
				}
				if tc.wantOrg == "" {
					if len(*r.published) != 0 || len(r.deliveries.seen) != 0 {
						t.Errorf("published %d events and recorded %d deliveries; want nothing", len(*r.published), len(r.deliveries.seen))
					}
				} else if len(*r.published) != 1 || (*r.published)[0].OrgID != tc.wantOrg {
					t.Errorf("published = %+v; want one event for %s", *r.published, tc.wantOrg)
				}
				if s := r.conn.snapshot().Sightings; len(s) != 0 {
					t.Errorf("sightings = %+v; want none on the deployment app's connection", s)
				}
			})
		}
	}
}

// TestDeploymentDelivery_StoreFailureNotAcked: a failed row read is the one
// outcome neither transport acks, so Slack redelivers.
func TestDeploymentDelivery_StoreFailureNotAcked(t *testing.T) {
	payload := eventCallbackPayload(deployTestWorkspace, deployTestAppID, "Ev-store-down", mentionInner("C1", "U1", "1.0"))

	r := newDeploymentIngestRig(t, deploymentRow(deployTestOrgID, deployTestWorkspace, true))
	r.reads.err = errors.New("store down")
	if rec := postSignedDeployment(t, r.webhook, payload); rec.Code != http.StatusInternalServerError {
		t.Errorf("events_api status = %d, want 500", rec.Code)
	}
	env := socketEnvelope{Type: "events_api", EnvelopeID: "env-1", Payload: mustJSON(t, payload)}
	if r.conn.processEventsAPIEnvelope(t.Context(), r.stores, r.pipeline, env) {
		t.Error("socket envelope acked after a failed read; want no ack")
	}
	if len(*r.published) != 0 {
		t.Errorf("published %d events; want 0", len(*r.published))
	}
}

// ---------- POST /api/webhooks/slack ----------

func TestDeploymentWebhook_NotEventsAPI_404(t *testing.T) {
	for name, app := range map[string]deploymentApp{
		"not configured": {},
		"socket mode":    socketDeploymentApp(),
	} {
		t.Run(name, func(t *testing.T) {
			r := newDeploymentIngestRig(t, deploymentRow(deployTestOrgID, deployTestWorkspace, true))
			r.webhook.deployment = app
			body := []byte(`{"type":"url_verification","challenge":"c"}`)
			ts := nowTimestamp()
			rec := postDeploymentWebhook(t, r.webhook, body, ts, sign(deployTestSecret, ts, body))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
			}
			if n := r.reads.reads.Load(); n != 0 {
				t.Errorf("store reads = %d; want 0", n)
			}
		})
	}
}

// TestDeploymentWebhook_Unverified_401NoStoreRead: a bad signature, a stale
// timestamp, and an unsigned request are each a 401 before any store read,
// and before the body is parsed, so a malformed body changes nothing.
func TestDeploymentWebhook_Unverified_401NoStoreRead(t *testing.T) {
	event := mustJSON(t, eventCallbackPayload(deployTestWorkspace, deployTestAppID, "Ev-forged", mentionInner("C1", "U1", "1.0")))
	stale := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	for _, tc := range []struct {
		name      string
		body      []byte
		timestamp string
		sig       func(ts string, body []byte) string
	}{
		{"bad signature", event, nowTimestamp(), func(string, []byte) string { return "v0=" + hex.EncodeToString(make([]byte, 32)) }},
		{"another secret", event, nowTimestamp(), func(ts string, body []byte) string { return sign("not-the-deployment-secret", ts, body) }},
		{"stale timestamp", event, stale, func(ts string, body []byte) string { return sign(deployTestSecret, ts, body) }},
		{"unsigned", event, "", func(string, []byte) string { return "" }},
		{"bad signature on a malformed body", []byte("{not json"), nowTimestamp(), func(string, []byte) string { return "v0=00" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newDeploymentIngestRig(t, deploymentRow(deployTestOrgID, deployTestWorkspace, true))
			rec := postDeploymentWebhook(t, r.webhook, tc.body, tc.timestamp, tc.sig(tc.timestamp, tc.body))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body=%s)", rec.Code, rec.Body.String())
			}
			if n := r.reads.reads.Load(); n != 0 {
				t.Errorf("store reads = %d; want 0 before the signature checks out", n)
			}
			if len(*r.published) != 0 {
				t.Errorf("published %d events; want 0", len(*r.published))
			}
		})
	}
}

// TestDeploymentWebhook_SignatureMismatch_LoggedOnce: a fresh, well-formed
// signature made with another secret, which is what every delivery looks like
// when TF_SLACK_SIGNING_SECRET is not the app's, is counted each time and
// logs one warning naming the variable; a second mismatch straight after logs
// nothing more. A delivery naming the deployment's app and one naming no app
// (Slack's url_verification) are the same case.
func TestDeploymentWebhook_SignatureMismatch_LoggedOnce(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      map[string]any
		wantNames string
	}{
		{"names the deployment app", eventCallbackPayload(deployTestWorkspace, deployTestAppID, "Ev-1", mentionInner("C1", "U1", "1.0")), mismatchNamesDeploymentApp},
		{"names no app", map[string]any{"type": "url_verification", "challenge": "c"}, mismatchNamesNoApp},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureSlackLog(t)
			r := newDeploymentIngestRig(t, deploymentRow(deployTestOrgID, deployTestWorkspace, true))
			body := mustJSON(t, tc.body)

			for range 2 {
				ts := nowTimestamp()
				if rec := postDeploymentWebhook(t, r.webhook, body, ts, sign("another-apps-secret", ts, body)); rec.Code != http.StatusUnauthorized {
					t.Fatalf("status = %d, want 401", rec.Code)
				}
			}

			out := logs.String()
			if n := strings.Count(out, "not signed with TF_SLACK_SIGNING_SECRET"); n != 1 {
				t.Fatalf("logged %d mismatch lines, want 1:\n%s", n, out)
			}
			for _, want := range []string{"level=WARN", "app_id=" + deployTestAppID, "mismatches=1"} {
				if !strings.Contains(out, want) {
					t.Errorf("log lacks %q:\n%s", want, out)
				}
			}
			if got := sumByAttr(t, r.metrics, "slack.webhook.signature.mismatches", "names_app"); got[tc.wantNames] != 2 || len(got) != 1 {
				t.Errorf("signature mismatches by names_app = %v, want {%s: 2}", got, tc.wantNames)
			}
		})
	}
}

// TestDeploymentWebhook_SignatureMismatch_OtherApp: a mismatched delivery
// naming another app logs that app instead of blaming the signing secret, so
// an app left pointing at the endpoint is not read as a wrong secret.
func TestDeploymentWebhook_SignatureMismatch_OtherApp(t *testing.T) {
	logs := captureSlackLog(t)
	r := newDeploymentIngestRig(t, deploymentRow(deployTestOrgID, deployTestWorkspace, true))
	body := mustJSON(t, eventCallbackPayload(deployTestWorkspace, "A0OLDAPP01", "Ev-1", mentionInner("C1", "U1", "1.0")))
	ts := nowTimestamp()
	if rec := postDeploymentWebhook(t, r.webhook, body, ts, sign("old-apps-secret", ts, body)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}

	out := logs.String()
	if strings.Contains(out, "TF_SLACK_SIGNING_SECRET") {
		t.Errorf("blamed the signing secret for another app's delivery:\n%s", out)
	}
	if n := strings.Count(out, "a delivery naming another Slack app"); n != 1 || !strings.Contains(out, "delivery_app_id=A0OLDAPP01") {
		t.Errorf("want one other-app line naming A0OLDAPP01:\n%s", out)
	}
	if got := sumByAttr(t, r.metrics, "slack.webhook.signature.mismatches", "names_app"); got[mismatchNamesOtherApp] != 1 || len(got) != 1 {
		t.Errorf("signature mismatches by names_app = %v, want {other: 1}", got)
	}
}

// TestDeploymentWebhook_SignatureMismatch_MalformedAppID: an api_app_id that
// is not an App ID's shape is treated as naming no app, so the unverified
// value never reaches the log.
func TestDeploymentWebhook_SignatureMismatch_MalformedAppID(t *testing.T) {
	logs := captureSlackLog(t)
	r := newDeploymentIngestRig(t, deploymentRow(deployTestOrgID, deployTestWorkspace, true))
	junk := strings.Repeat("A", 200)
	body := mustJSON(t, eventCallbackPayload(deployTestWorkspace, junk, "Ev-1", mentionInner("C1", "U1", "1.0")))
	ts := nowTimestamp()
	if rec := postDeploymentWebhook(t, r.webhook, body, ts, sign("another-apps-secret", ts, body)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if out := logs.String(); strings.Contains(out, junk) || strings.Contains(out, "another Slack app") {
		t.Errorf("logged the malformed app id as another app:\n%s", out)
	}
	if got := sumByAttr(t, r.metrics, "slack.webhook.signature.mismatches", "names_app"); got[mismatchNamesNoApp] != 1 || len(got) != 1 {
		t.Errorf("signature mismatches by names_app = %v, want {none: 1}", got)
	}
}

// TestDeploymentWebhook_UnsignedOrMalformed_NotLogged: a request Slack would
// not send (unsigned, stale, or with a digest that is not SHA-256's) is a 401
// and logs nothing, so a scanner hitting the endpoint does not read as a wrong
// signing secret. Neither does a delivery that verifies.
func TestDeploymentWebhook_UnsignedOrMalformed_NotLogged(t *testing.T) {
	logs := captureSlackLog(t)
	r := newDeploymentIngestRig(t, deploymentRow(deployTestOrgID, deployTestWorkspace, true))
	body := mustJSON(t, map[string]any{"type": "url_verification", "challenge": "c"})
	stale := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)

	for _, tc := range []struct{ name, timestamp, sig string }{
		{"unsigned", "", ""},
		{"no timestamp", "", sign(deployTestSecret, nowTimestamp(), body)},
		{"stale", stale, sign("another-apps-secret", stale, body)},
		{"short digest", nowTimestamp(), "v0=00"},
		{"not hex", nowTimestamp(), "v0=zz"},
		{"no v0 prefix", nowTimestamp(), "v1=" + hex.EncodeToString(make([]byte, 32))},
	} {
		if rec := postDeploymentWebhook(t, r.webhook, body, tc.timestamp, tc.sig); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", tc.name, rec.Code)
		}
	}
	if rec := postSignedDeployment(t, r.webhook, map[string]any{"type": "url_verification", "challenge": "c"}); rec.Code != http.StatusOK {
		t.Errorf("signed: status = %d, want 200", rec.Code)
	}
	if out := logs.String(); strings.Contains(out, "TF_SLACK_SIGNING_SECRET") {
		t.Errorf("logged a signing-secret line for a request that was not a mismatch:\n%s", out)
	}
	if got := sumByAttr(t, r.metrics, "slack.webhook.signature.mismatches", "names_app"); len(got) != 0 {
		t.Errorf("signature mismatches = %v, want none", got)
	}
}

// TestDeploymentWebhook_SignatureVerdictsCounted: every delivery to the
// deployment endpoint is counted by its signature verdict under the deployment
// app, whichever app the body names, so a clock more than five minutes off
// shows as stale deliveries with none verified.
func TestDeploymentWebhook_SignatureVerdictsCounted(t *testing.T) {
	r := newDeploymentIngestRig(t, deploymentRow(deployTestOrgID, deployTestWorkspace, true))
	body := mustJSON(t, map[string]any{"type": "url_verification", "challenge": "c"})
	otherApp := mustJSON(t, eventCallbackPayload(deployTestWorkspace, "A0OLDAPP01", "Ev-1", mentionInner("C1", "U1", "1.0")))
	fresh := nowTimestamp()
	stale := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	ahead := strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10)

	for _, tc := range []struct {
		name, timestamp, sig string
		body                 []byte
		wantStatus           int
	}{
		{"verified", fresh, sign(deployTestSecret, fresh, body), body, http.StatusOK},
		{"another secret", fresh, sign("another-apps-secret", fresh, body), body, http.StatusUnauthorized},
		{"another app", fresh, sign("old-apps-secret", fresh, otherApp), otherApp, http.StatusUnauthorized},
		{"stale", stale, sign(deployTestSecret, stale, body), body, http.StatusUnauthorized},
		{"ahead of the clock", ahead, sign(deployTestSecret, ahead, body), body, http.StatusUnauthorized},
		{"unsigned", "", "", body, http.StatusUnauthorized},
	} {
		if rec := postDeploymentWebhook(t, r.webhook, tc.body, tc.timestamp, tc.sig); rec.Code != tc.wantStatus {
			t.Errorf("%s: status = %d, want %d", tc.name, rec.Code, tc.wantStatus)
		}
	}

	want := map[string]int64{"verified": 1, "mismatch": 2, "stale": 2, "malformed": 1}
	if got := sumByAttr(t, r.metrics, "slack.webhook.deployment.deliveries", "signature"); !maps.Equal(got, want) {
		t.Errorf("deployment deliveries by signature = %v; want %v", got, want)
	}
	if got := sumByAttr(t, r.metrics, "slack.webhook.deployment.deliveries", "app_id"); !maps.Equal(got, map[string]int64{deployTestAppID: 6}) {
		t.Errorf("deployment deliveries by app = %v; want {%s: 6}", got, deployTestAppID)
	}

	unconfigured := newDeploymentIngestRig(t)
	unconfigured.webhook.deployment = socketDeploymentApp()
	if rec := postDeploymentWebhook(t, unconfigured.webhook, body, fresh, sign(deployTestSecret, fresh, body)); rec.Code != http.StatusNotFound {
		t.Errorf("Socket Mode deployment app: status = %d, want 404", rec.Code)
	}
	if got := sumByAttr(t, unconfigured.metrics, "slack.webhook.deployment.deliveries", "signature"); len(got) != 0 {
		t.Errorf("deployment deliveries with the endpoint off = %v; want none", got)
	}
}

// TestCheckSlackSignature_Outcomes: only a fresh, well-formed SHA-256 digest
// that the secret does not produce is a mismatch.
func TestCheckSlackSignature_Outcomes(t *testing.T) {
	body := []byte(`{"type":"event_callback"}`)
	fresh := nowTimestamp()
	stale := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	for _, tc := range []struct {
		name, timestamp, sig string
		want                 signatureCheck
	}{
		{"valid", fresh, sign(deployTestSecret, fresh, body), signatureValid},
		{"another secret", fresh, sign("another-apps-secret", fresh, body), signatureMismatch},
		{"zero digest", fresh, "v0=" + hex.EncodeToString(make([]byte, 32)), signatureMismatch},
		{"stale but right secret", stale, sign(deployTestSecret, stale, body), signatureStale},
		{"stale and another secret", stale, sign("another-apps-secret", stale, body), signatureStale},
		{"no timestamp", "", sign(deployTestSecret, fresh, body), signatureMalformed},
		{"timestamp not a number", "soon", sign(deployTestSecret, fresh, body), signatureMalformed},
		{"no signature", fresh, "", signatureMalformed},
		{"no v0 prefix", fresh, strings.Replace(sign(deployTestSecret, fresh, body), "v0=", "v1=", 1), signatureMalformed},
		{"not hex", fresh, "v0=zz", signatureMalformed},
		{"short digest", fresh, "v0=00", signatureMalformed},
	} {
		if got := checkSlackSignature(deployTestSecret, body, tc.timestamp, tc.sig); got != tc.want {
			t.Errorf("%s: checkSlackSignature = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestSignatureCheck_ZeroValueRefuses: a signatureCheck nothing assigned is
// not a pass.
func TestSignatureCheck_ZeroValueRefuses(t *testing.T) {
	var unset signatureCheck
	if unset == signatureValid {
		t.Fatal("the zero signatureCheck is signatureValid; an unset check must refuse")
	}
}

// TestMismatchReport_FirstThenOnceAnInterval: the first mismatch is logged,
// later ones inside the interval are counted but not logged, and the next one
// past the interval is logged with the count of all since the last line.
func TestMismatchReport_FirstThenOnceAnInterval(t *testing.T) {
	var m mismatchReport
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, step := range []struct {
		at        time.Duration
		wantLog   bool
		wantCount int
	}{
		{0, true, 1},
		{10 * time.Second, false, 0},
		{59 * time.Second, false, 0},
		{61 * time.Second, true, 3},
		{62 * time.Second, false, 0},
		{3 * time.Minute, true, 2},
	} {
		log, count := m.record(t0.Add(step.at))
		if log != step.wantLog || count != step.wantCount {
			t.Errorf("record at +%s = (%v, %d), want (%v, %d)", step.at, log, count, step.wantLog, step.wantCount)
		}
	}
}

func TestDeploymentWebhook_BodyOverTheCap_413(t *testing.T) {
	r := newDeploymentIngestRig(t)
	body := bytes.Repeat([]byte("a"), maxSlackWebhookBody+1)
	ts := nowTimestamp()
	rec := postDeploymentWebhook(t, r.webhook, body, ts, sign(deployTestSecret, ts, body))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	var out struct {
		Errors []struct {
			Reason string `json:"reason"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Errors) != 1 || out.Errors[0].Reason != "PAYLOAD_TOO_LARGE" {
		t.Errorf("body = %s; want the error envelope with PAYLOAD_TOO_LARGE", rec.Body.String())
	}
}

func TestDeploymentWebhook_SignedMalformedBody_400(t *testing.T) {
	r := newDeploymentIngestRig(t)
	body := []byte("{not json")
	ts := nowTimestamp()
	if rec := postDeploymentWebhook(t, r.webhook, body, ts, sign(deployTestSecret, ts, body)); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestDeploymentWebhook_URLVerification(t *testing.T) {
	r := newDeploymentIngestRig(t)
	rec := postSignedDeployment(t, r.webhook, map[string]any{"type": "url_verification", "challenge": "deployment-challenge"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Challenge != "deployment-challenge" {
		t.Errorf("body = %s; want the challenge echoed", rec.Body.String())
	}
}

// TestDeploymentWebhook_DeliversOnceAndDedupes: a signed event for an active
// connection reaches the pipeline once, and Slack's redelivery of the same
// event id is a duplicate.
func TestDeploymentWebhook_DeliversOnceAndDedupes(t *testing.T) {
	r := newDeploymentIngestRig(t, deploymentRow(deployTestOrgID, deployTestWorkspace, true))
	payload := eventCallbackPayload(deployTestWorkspace, deployTestAppID, "Ev-once", mentionInner("C1", "U1", "1700000000.000100"))
	for i := range 2 {
		if rec := postSignedDeployment(t, r.webhook, payload); rec.Code != http.StatusOK {
			t.Fatalf("delivery %d: status = %d, want 200 (body=%s)", i, rec.Code, rec.Body.String())
		}
	}
	if len(*r.published) != 1 || (*r.published)[0].OrgID != deployTestOrgID {
		t.Fatalf("published = %+v; want exactly one event for %s", *r.published, deployTestOrgID)
	}
	var meta SlackMessageMetadata
	if err := json.Unmarshal([]byte((*r.published)[0].MetadataJSON), &meta); err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if meta.WorkspaceID != deployTestWorkspace || meta.APIAppID != deployTestAppID {
		t.Errorf("metadata = %+v; want the deployment connection's workspace and app", meta)
	}
}

// TestOrgWebhook_DeploymentRow_401: the per-org route is not for a connection
// through the deployment's app, which has no signing secret of its own.
func TestOrgWebhook_DeploymentRow_401(t *testing.T) {
	r := newDeploymentIngestRig(t, deploymentRow(deployTestOrgID, deployTestWorkspace, true))
	body := mustJSON(t, eventCallbackPayload(deployTestWorkspace, deployTestAppID, "Ev-per-org", mentionInner("C1", "U1", "1.0")))
	ts := nowTimestamp()
	rec := postSlackWebhook(t, r.webhook, deployTestOrgID, body, ts, sign(deployTestSecret, ts, body))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(*r.published) != 0 {
		t.Errorf("published %d events; want 0", len(*r.published))
	}
}

// ---------- Socket Mode ----------

// newDeploymentSocketRig is newSocketTestRig with the deployment's app
// configured for Socket Mode.
func newDeploymentSocketRig(t *testing.T, rows ...*slackstore.Connection) *socketTestRig {
	t.Helper()
	rig := newSocketTestRig(t, rows...)
	rig.mgr.deployment = socketDeploymentApp()
	return rig
}

func liveAppIDs(m *socketManager) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.conns))
	for id := range m.conns {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// TestDeploymentSocket_NoRows_OneConnection: with no connection rows and no
// org entitled, the deployment app still has its connection, dialed with the
// token from the environment.
func TestDeploymentSocket_NoRows_OneConnection(t *testing.T) {
	withFastSocketTimings(t)
	fake := newFakeSlackSocket(t)
	rig := newDeploymentSocketRig(t)
	entitlements.RegisterProvider(entitlements.Static())
	runManager(t, rig.mgr)

	fake.nextConn(t)
	if got := liveAppIDs(rig.mgr); !slices.Equal(got, []string{deployTestAppID}) {
		t.Fatalf("live connections = %v; want only the deployment app's", got)
	}
	if got, _ := fake.openAuth.Load().(string); got != "Bearer "+deployTestAppToken {
		t.Errorf("apps.connections.open Authorization = %q; want the environment's app token", got)
	}
	select {
	case c := <-fake.conns:
		t.Fatalf("a second connection %v; want exactly one", c)
	case <-time.After(150 * time.Millisecond):
	}
}

// TestDeploymentSocket_AlongsideAnOrgApp: an org's own socket app and the
// deployment app run as two connections.
func TestDeploymentSocket_AlongsideAnOrgApp(t *testing.T) {
	withFastSocketTimings(t)
	fake := newFakeSlackSocket(t)
	rig := newDeploymentSocketRig(t, socketTestRow(socketTestOrgID))
	runManager(t, rig.mgr)

	fake.nextConn(t)
	fake.nextConn(t)
	if got, want := liveAppIDs(rig.mgr), []string{deployTestAppID, socketTestAppID}; !slices.Equal(got, want) {
		t.Fatalf("live connections = %v; want %v", got, want)
	}
}

// TestDeploymentSocket_TwoOrgsOnOneConnection: envelopes for two orgs'
// workspaces arrive on the deployment app's one connection, and each reaches
// its own org's pipeline. An envelope for a workspace with no row is acked
// and records no sighting.
func TestDeploymentSocket_TwoOrgsOnOneConnection(t *testing.T) {
	withFastSocketTimings(t)
	fake := newFakeSlackSocket(t)
	rig := newDeploymentSocketRig(t,
		deploymentRow(deployTestOrgID, "T0DEPORG1", true),
		deploymentRow(deployTestOtherOrg, "T0DEPORG2", true),
	)
	runManager(t, rig.mgr)
	conn := fake.nextConn(t)

	sendEventsAPI(t, conn, "Env-1", eventCallbackPayload("T0DEPORG1", deployTestAppID, "Ev-org1", mentionInner("C1", "U1", "1.0")))
	expectAck(t, conn, "Env-1")
	sendEventsAPI(t, conn, "Env-2", eventCallbackPayload("T0DEPORG2", deployTestAppID, "Ev-org2", mentionInner("C2", "U2", "2.0")))
	expectAck(t, conn, "Env-2")
	sendEventsAPI(t, conn, "Env-3", eventCallbackPayload("T0UNKNOWN", deployTestAppID, "Ev-unknown", mentionInner("C3", "U3", "3.0")))
	expectAck(t, conn, "Env-3")

	if len(*rig.published) != 2 {
		t.Fatalf("published %d events; want 2", len(*rig.published))
	}
	if a, b := (*rig.published)[0].OrgID, (*rig.published)[1].OrgID; a != deployTestOrgID || b != deployTestOtherOrg {
		t.Errorf("published orgs = %s, %s; want %s then %s", a, b, deployTestOrgID, deployTestOtherOrg)
	}
	if status, _ := rig.mgr.StatusFor(deployTestAppID); len(status.Sightings) != 0 {
		t.Errorf("sightings = %+v; want none", status.Sightings)
	}
}

// TestDeploymentSocket_EveryRowDeleted_KeepsRunning: a reconcile pass after
// every connection through the deployment app is deleted leaves its socket
// up. The passes are called directly rather than through run, because
// reconcile returns only once any teardown it decides on has finished, so the
// assertion follows a pass that has certainly happened.
func TestDeploymentSocket_EveryRowDeleted_KeepsRunning(t *testing.T) {
	withFastSocketTimings(t)
	fake := newFakeSlackSocket(t)
	rig := newDeploymentSocketRig(t, deploymentRow(deployTestOrgID, deployTestWorkspace, true))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		rig.mgr.stopAll()
	})
	rig.mgr.reconcile(ctx)
	conn := fake.nextConn(t)

	rig.wsStore.deleteRow(deployTestWorkspace, deployTestAppID)
	rig.mgr.reconcile(ctx)

	if got := liveAppIDs(rig.mgr); !slices.Equal(got, []string{deployTestAppID}) {
		t.Fatalf("live connections = %v; want the deployment app's", got)
	}
	sendEventsAPI(t, conn, "Env-after", eventCallbackPayload(deployTestWorkspace, deployTestAppID, "Ev-after", mentionInner("C1", "U1", "1.0")))
	expectAck(t, conn, "Env-after")
}

func TestDeploymentSocket_RejectedToken_AuthFailed(t *testing.T) {
	withFastSocketTimings(t)
	fake := newFakeSlackSocket(t)
	fake.authFail.Store(true)
	rig := newDeploymentSocketRig(t)
	runManager(t, rig.mgr)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if status, ok := rig.mgr.StatusFor(deployTestAppID); ok && status.State == stateAuthFailed {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the deployment app's connection to report auth_failed")
}

// TestWorkspaceView_DeploymentConnection_StateOnly: the list read shows a
// connection through the deployment's app with its state alone, and an org's
// own app with its full status.
func TestWorkspaceView_DeploymentConnection_StateOnly(t *testing.T) {
	now := time.Now()
	mgr := newSocketManager(db.Stores{}, nil, http.DefaultClient, nil)
	for _, appID := range []string{deployTestAppID, socketTestAppID} {
		mgr.conns[appID] = &appConnection{
			appID:     appID,
			status:    connStatus{State: stateOpen, ConnectedAt: &now, LastEventAt: &now, ConsecutiveFailures: 2, LastError: "boom"},
			sightings: map[string]*sightingView{"T0SEEN": {TeamID: "T0SEEN", Count: 1, LastSeen: now}},
		}
	}

	connectionKeys := func(row *slackstore.Connection) []string {
		t.Helper()
		raw, err := json.Marshal(toWorkspaceView(row, mgr))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var out struct {
			Connection map[string]json.RawMessage `json:"connection"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("unmarshal %s: %v", raw, err)
		}
		keys := make([]string, 0, len(out.Connection))
		for k := range out.Connection {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		return keys
	}

	if got := connectionKeys(deploymentRow(deployTestOrgID, deployTestWorkspace, true)); !slices.Equal(got, []string{"state"}) {
		t.Errorf("deployment connection keys = %v; want [state]", got)
	}
	want := []string{"connected_at", "consecutive_failures", "last_error", "last_event_at", "sightings", "state"}
	if got := connectionKeys(socketTestRow(socketTestOrgID)); !slices.Equal(got, want) {
		t.Errorf("org connection keys = %v; want %v", got, want)
	}
}
