package slack

// pgtest-backed suite for an app removal Slack reports (app_uninstalled, or
// tokens_revoked naming a bot token): through either transport, for an org's
// own app or the deployment's, the connection's row and secrets are removed,
// licensed or not, unless the row was connected again since.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
)

const (
	removalTestApp    = "A0REMOVE01"
	removalTestSecret = "removal-signing-secret"
)

type removalRig struct {
	*slackWorkspaceRig
	orgID, owner string
	pipeline     *ingestPipeline
	webhook      *webhookHandler
	metrics      *sdkmetric.ManualReader
	published    *[]domain.Event
}

func newRemovalRig(t *testing.T) *removalRig {
	t.Helper()
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-removal")
	bundle := slackstore.FromStores(r.stores)
	stats, metrics := newTestStats()
	published := &[]domain.Event{}
	p := &ingestPipeline{
		tx:          r.stores.Tx,
		connections: bundle.Connections,
		secrets:     r.stores.Secrets,
		deliveries:  bundle.Deliveries,
		channels:    bundle.Channels,
		publish:     func(_ context.Context, evt domain.Event) { *published = append(*published, evt) },
		stats:       stats,
	}
	return &removalRig{
		slackWorkspaceRig: r,
		orgID:             orgID,
		owner:             owner,
		pipeline:          p,
		webhook:           &webhookHandler{stores: r.stores, pipeline: p, stats: stats},
		metrics:           metrics,
		published:         published,
	}
}

// connect connects workspaceID under removalTestApp with botToken. Over the
// Events API it stores all three secrets (a signing secret for the webhook,
// and an app token beside it); over Socket Mode, the bot and app tokens.
func (r *removalRig) connect(workspaceID, botToken, transport string) {
	r.t.Helper()
	r.fake.stageConnectOK(botToken, workspaceID, "Acme", "U0BOT"+workspaceID, removalTestApp, "")
	body := map[string]string{"bot_token": botToken, "app_token": "xapp-" + workspaceID}
	if transport == transportEventsAPI {
		body["signing_secret"] = removalTestSecret
		body["transport"] = transportEventsAPI
	}
	rec := httptest.NewRecorder()
	r.hdl.handleConnect(rec, r.importReq(r.owner, r.orgID, body))
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		r.t.Fatalf("connect %s: status = %d (body=%s)", workspaceID, rec.Code, rec.Body.String())
	}
}

func (r *removalRig) row(workspaceID string) *slackstore.Connection {
	r.t.Helper()
	ws, err := slackstore.FromStores(r.stores).Connections.GetByWorkspaceAppSystem(r.t.Context(), workspaceID, removalTestApp)
	if err != nil {
		r.t.Fatalf("read workspace %s: %v", workspaceID, err)
	}
	return ws
}

// secretValues reads workspaceID's three secrets, in SlackConnectionKeysFor's
// order (bot token, signing secret, app token).
func (r *removalRig) secretValues(workspaceID string) []string {
	r.t.Helper()
	var out []string
	for _, key := range integrations.SlackConnectionKeysFor(workspaceID, removalTestApp).All() {
		v, err := r.stores.Secrets.GetSystem(r.t.Context(), r.orgID, key)
		if err != nil {
			r.t.Fatalf("read secret %s: %v", key, err)
		}
		out = append(out, v)
	}
	return out
}

func (r *removalRig) outcomes() map[string]int64 {
	r.t.Helper()
	return sumByAttr(r.t, r.metrics, "slack.ingest.events", "outcome")
}

// removalPayload is eventCallbackPayload carrying an event_time, which the
// removal's reconnect guard reads.
func removalPayload(workspaceID, eventID string, eventTime time.Time, inner map[string]any) map[string]any {
	p := eventCallbackPayload(workspaceID, removalTestApp, eventID, inner)
	p["event_time"] = eventTime.Unix()
	return p
}

func (r *removalRig) postSigned(workspaceID string, payload map[string]any) *httptest.ResponseRecorder {
	r.t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		r.t.Fatalf("marshal payload: %v", err)
	}
	ts := nowTimestamp()
	return postSlackWebhook(r.t, r.webhook, r.orgID, body, ts, sign(removalTestSecret, ts, body))
}

var (
	appUninstalledInner = map[string]any{"type": "app_uninstalled"}
	botTokenRevoked     = map[string]any{"type": "tokens_revoked", "tokens": map[string]any{"oauth": []string{"U0PERSON"}, "bot": []string{"U0BOTT0REMOVE1"}}}
)

// TestAppRemoval_Webhook_DisconnectsTheWorkspace: a signed app_uninstalled,
// or a tokens_revoked naming a bot token, deletes the row and every secret
// it held, and a redelivery after the delete finds no row and acks empty.
func TestAppRemoval_Webhook_DisconnectsTheWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name  string
		inner map[string]any
	}{
		{"app_uninstalled", appUninstalledInner},
		{"tokens_revoked with a bot token", botTokenRevoked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRemovalRig(t)
			r.connect("T0REMOVE1", "xoxb-remove1", transportEventsAPI)
			for i, v := range r.secretValues("T0REMOVE1") {
				if v == "" {
					t.Fatalf("secret %d empty after connect; want all three stored", i)
				}
			}

			payload := removalPayload("T0REMOVE1", "Ev-removal", time.Now(), tc.inner)
			rec := r.postSigned("T0REMOVE1", payload)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
			}
			if ws := r.row("T0REMOVE1"); ws != nil {
				t.Errorf("row after the removal = %+v; want deleted", ws)
			}
			for i, v := range r.secretValues("T0REMOVE1") {
				if v != "" {
					t.Errorf("secret %d after the removal = %q; want deleted", i, v)
				}
			}
			if got := r.outcomes(); got[outcomeDisconnected] != 1 || len(got) != 1 {
				t.Errorf("outcomes = %v; want exactly one %q", got, outcomeDisconnected)
			}

			// Slack's redelivery: no row resolves, so the receiver acks it
			// before the pipeline.
			redelivered := r.postSigned("T0REMOVE1", payload)
			if redelivered.Code != http.StatusOK || strings.TrimSpace(redelivered.Body.String()) != "{}" {
				t.Errorf("redelivery = %d %q; want 200 {}", redelivered.Code, redelivered.Body.String())
			}
			if got := r.outcomes(); got[outcomeDisconnected] != 1 || len(got) != 1 {
				t.Errorf("outcomes after the redelivery = %v; want it uncounted", got)
			}
		})
	}
}

// TestAppRemoval_Webhook_UserTokensOnly_KeepsTheWorkspace: a tokens_revoked
// naming only user tokens is not about the bot token TF holds.
func TestAppRemoval_Webhook_UserTokensOnly_KeepsTheWorkspace(t *testing.T) {
	r := newRemovalRig(t)
	r.connect("T0REMOVE1", "xoxb-remove1", transportEventsAPI)
	before := r.secretValues("T0REMOVE1")

	inner := map[string]any{"type": "tokens_revoked", "tokens": map[string]any{"oauth": []string{"U0PERSON"}}}
	if rec := r.postSigned("T0REMOVE1", removalPayload("T0REMOVE1", "Ev-oauth", time.Now(), inner)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if r.row("T0REMOVE1") == nil {
		t.Error("row deleted for a user-token revocation; want kept")
	}
	assertSecretsUnchanged(t, before, r.secretValues("T0REMOVE1"))
	if got := r.outcomes(); got[dropUserTokensOnly] != 1 || len(got) != 1 {
		t.Errorf("outcomes = %v; want exactly one %q", got, dropUserTokensOnly)
	}
}

// TestAppRemoval_Webhook_UnlicensedOrg: the entitlement gate still drops a
// lapsed org's mention, and lets its app removal through.
func TestAppRemoval_Webhook_UnlicensedOrg(t *testing.T) {
	r := newRemovalRig(t)
	r.connect("T0REMOVE1", "xoxb-remove1", transportEventsAPI)
	entitlements.RegisterProvider(entitlements.Static())

	mention := map[string]any{"type": "app_mention", "channel": "C1", "user": "U1", "text": "<@U0BOTT0REMOVE1> hi", "ts": "1700000000.000100"}
	if rec := r.postSigned("T0REMOVE1", removalPayload("T0REMOVE1", "Ev-mention", time.Now(), mention)); rec.Code != http.StatusOK {
		t.Fatalf("mention status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(*r.published) != 0 || len(r.outcomes()) != 0 {
		t.Fatalf("a lapsed org's mention reached the pipeline: published %d, outcomes %v", len(*r.published), r.outcomes())
	}

	if rec := r.postSigned("T0REMOVE1", removalPayload("T0REMOVE1", "Ev-removal", time.Now(), appUninstalledInner)); rec.Code != http.StatusOK {
		t.Fatalf("removal status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if ws := r.row("T0REMOVE1"); ws != nil {
		t.Errorf("a lapsed org's row after the removal = %+v; want deleted", ws)
	}
	if got := r.outcomes(); got[outcomeDisconnected] != 1 {
		t.Errorf("outcomes = %v; want one %q", got, outcomeDisconnected)
	}
}

// TestAppRemoval_Webhook_PredatesTheConnection_KeepsTheWorkspace: a removal
// Slack generated before the row's current connection, such as a retry that
// lands after an admin connected the workspace again, keeps the row and its
// secrets.
func TestAppRemoval_Webhook_PredatesTheConnection_KeepsTheWorkspace(t *testing.T) {
	r := newRemovalRig(t)
	r.connect("T0REMOVE1", "xoxb-remove1", transportEventsAPI)
	before := r.secretValues("T0REMOVE1")

	payload := removalPayload("T0REMOVE1", "Ev-late", time.Now().Add(-time.Minute), appUninstalledInner)
	if rec := r.postSigned("T0REMOVE1", payload); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if r.row("T0REMOVE1") == nil {
		t.Error("row deleted by a removal older than its connection; want kept")
	}
	assertSecretsUnchanged(t, before, r.secretValues("T0REMOVE1"))
	if got := r.outcomes(); got[dropReconnected] != 1 || len(got) != 1 {
		t.Errorf("outcomes = %v; want exactly one %q", got, dropReconnected)
	}
}

// TestAppRemoval_ReconnectedAfterTheRead_KeepsTheWorkspace: a connect landing
// between the receiver's read and the delete moves updated_at, so the
// reconnected row and the secrets it wrote stay.
func TestAppRemoval_ReconnectedAfterTheRead_KeepsTheWorkspace(t *testing.T) {
	r := newRemovalRig(t)
	r.connect("T0REMOVE1", "xoxb-remove1", transportEventsAPI)
	stale := r.row("T0REMOVE1")

	r.connect("T0REMOVE1", "xoxb-reinstalled", transportEventsAPI)
	current := r.row("T0REMOVE1")
	if current == nil || current.UpdatedAt.Equal(stale.UpdatedAt) {
		t.Fatalf("reconnect did not move updated_at: before %v, after %+v", stale.UpdatedAt, current)
	}
	before := r.secretValues("T0REMOVE1")

	ev := inboundMention{Type: "app_uninstalled", EventID: "Ev-raced", EventTime: time.Now()}
	if err := r.pipeline.handleEventCallback(t.Context(), *stale, ev); err != nil {
		t.Fatalf("handleEventCallback: %v", err)
	}
	if got := r.row("T0REMOVE1"); got == nil || !got.UpdatedAt.Equal(current.UpdatedAt) {
		t.Errorf("reconnected row after the removal = %+v; want it unchanged", got)
	}
	assertSecretsUnchanged(t, before, r.secretValues("T0REMOVE1"))
	if before[0] != "xoxb-reinstalled" {
		t.Errorf("bot token = %q; want the reconnect's", before[0])
	}
	if got := r.outcomes(); got[dropRowChanged] != 1 || len(got) != 1 {
		t.Errorf("outcomes = %v; want exactly one %q", got, dropRowChanged)
	}
}

// TestAppRemoval_Webhook_OtherWorkspacesKeepTheirConnection: removing the app
// from one workspace leaves the same app's row in the org's other workspace,
// and that row's secrets.
func TestAppRemoval_Webhook_OtherWorkspacesKeepTheirConnection(t *testing.T) {
	r := newRemovalRig(t)
	r.connect("T0REMOVE1", "xoxb-remove1", transportEventsAPI)
	r.connect("T0REMOVE2", "xoxb-remove2", transportEventsAPI)
	before := r.secretValues("T0REMOVE2")

	if rec := r.postSigned("T0REMOVE1", removalPayload("T0REMOVE1", "Ev-removal", time.Now(), appUninstalledInner)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if r.row("T0REMOVE1") != nil {
		t.Error("removed workspace's row is still present")
	}
	if r.row("T0REMOVE2") == nil {
		t.Error("the other workspace's row was deleted; want kept")
	}
	assertSecretsUnchanged(t, before, r.secretValues("T0REMOVE2"))
}

// TestAppRemoval_Socket_DisconnectsAndAcks: the same removal over Socket Mode
// deletes the row and its secrets and is acked, licensed or not; its
// redelivery is acked without being recorded as a sighting of an unconnected
// workspace.
func TestAppRemoval_Socket_DisconnectsAndAcks(t *testing.T) {
	for _, licensed := range []bool{true, false} {
		name := "licensed"
		if !licensed {
			name = "unlicensed"
		}
		t.Run(name, func(t *testing.T) {
			r := newRemovalRig(t)
			r.connect("T0REMOVE1", "xoxb-remove1", transportSocket)
			if !licensed {
				entitlements.RegisterProvider(entitlements.Static())
			}
			c := &appConnection{appID: removalTestApp, orgID: r.orgID, sightings: map[string]*sightingView{}}
			env := socketEnvelope{
				Type: "events_api", EnvelopeID: "env-1",
				Payload: mustJSON(t, removalPayload("T0REMOVE1", "Ev-removal", time.Now(), appUninstalledInner)),
			}

			if !c.processEventsAPIEnvelope(t.Context(), r.stores, r.pipeline, env) {
				t.Fatal("envelope not acked; want acked")
			}
			if ws := r.row("T0REMOVE1"); ws != nil {
				t.Errorf("row after the removal = %+v; want deleted", ws)
			}
			for i, v := range r.secretValues("T0REMOVE1") {
				if v != "" {
					t.Errorf("secret %d after the removal = %q; want deleted", i, v)
				}
			}

			if !c.processEventsAPIEnvelope(t.Context(), r.stores, r.pipeline, env) {
				t.Fatal("redelivered envelope not acked; want acked")
			}
			if s := c.snapshot().Sightings; len(s) != 0 {
				t.Errorf("sightings after the redelivery = %+v; want none", s)
			}
		})
	}
}

type failingDeleteConnections struct {
	slackstore.ConnectionStore
	err error
}

func (f failingDeleteConnections) DeleteSystem(context.Context, string, string, string, time.Time) (bool, error) {
	return false, f.err
}

// TestAppRemoval_Socket_FailedDeleteNotAcked: a delete that fails withholds
// the ack, so Slack redelivers, and leaves the row and its secrets.
func TestAppRemoval_Socket_FailedDeleteNotAcked(t *testing.T) {
	r := newRemovalRig(t)
	r.connect("T0REMOVE1", "xoxb-remove1", transportSocket)
	before := r.secretValues("T0REMOVE1")
	r.pipeline.connections = failingDeleteConnections{ConnectionStore: r.pipeline.connections, err: errors.New("boom")}

	c := &appConnection{appID: removalTestApp, orgID: r.orgID, sightings: map[string]*sightingView{}}
	env := socketEnvelope{
		Type: "events_api", EnvelopeID: "env-1",
		Payload: mustJSON(t, removalPayload("T0REMOVE1", "Ev-removal", time.Now(), appUninstalledInner)),
	}
	if c.processEventsAPIEnvelope(t.Context(), r.stores, r.pipeline, env) {
		t.Fatal("envelope acked after a failed delete; want no ack")
	}
	if r.row("T0REMOVE1") == nil {
		t.Error("row gone after a failed delete; want kept")
	}
	assertSecretsUnchanged(t, before, r.secretValues("T0REMOVE1"))
	if got := r.outcomes(); got[outcomeError] != 1 {
		t.Errorf("outcomes = %v; want one %q", got, outcomeError)
	}
}

// connectDeployment writes an active connection through the deployment's app
// in workspaceID directly, as the install that writes one is not built yet,
// and stores its bot token, the one secret such a connection has.
func (r *removalRig) connectDeployment(workspaceID, botToken string) {
	r.t.Helper()
	keys := integrations.SlackConnectionKeysFor(workspaceID, deployTestAppID)
	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO org_slack_connections (
			workspace_id, api_app_id, org_id, source, active, workspace_name, bot_user_id, bot_token_ref
		) VALUES ($1, $2, $3, 'deployment', true, 'Acme', 'U0DEPBOT', $4)
	`, workspaceID, deployTestAppID, r.orgID, keys.BotToken); err != nil {
		r.t.Fatalf("insert deployment connection: %v", err)
	}
	if err := r.stores.Tx.WithTx(r.t.Context(), r.orgID, r.owner, func(tx db.TxStores) error {
		return tx.Secrets.Put(r.t.Context(), r.orgID, keys.BotToken, botToken, "Slack bot token")
	}); err != nil {
		r.t.Fatalf("store deployment connection's bot token: %v", err)
	}
}

// TestAppRemoval_DeploymentConnection: an app removal through the
// deployment's app, over either transport and licensed or not, deletes the
// connection's row and its bot token, and leaves the same org's connection
// through the app in another workspace.
func TestAppRemoval_DeploymentConnection(t *testing.T) {
	for _, tr := range []string{transportEventsAPI, transportSocket} {
		for _, licensed := range []bool{true, false} {
			name := tr + "/licensed"
			if !licensed {
				name = tr + "/unlicensed"
			}
			t.Run(name, func(t *testing.T) {
				r := newRemovalRig(t)
				r.connectDeployment("T0DEPREM1", "xoxb-deployment-1")
				r.connectDeployment("T0DEPREM2", "xoxb-deployment-2")
				if !licensed {
					entitlements.RegisterProvider(entitlements.Static())
				}
				botToken := func(workspaceID string) string {
					t.Helper()
					v, err := r.stores.Secrets.GetSystem(t.Context(), r.orgID, integrations.SlackConnectionKeysFor(workspaceID, deployTestAppID).BotToken)
					if err != nil {
						t.Fatalf("read bot token: %v", err)
					}
					return v
				}
				row := func(workspaceID string) *slackstore.Connection {
					t.Helper()
					ws, err := slackstore.FromStores(r.stores).Connections.GetByWorkspaceAppSystem(t.Context(), workspaceID, deployTestAppID)
					if err != nil {
						t.Fatalf("read connection: %v", err)
					}
					return ws
				}

				payload := eventCallbackPayload("T0DEPREM1", deployTestAppID, "Ev-deployment-removal", appUninstalledInner)
				payload["event_time"] = time.Now().Unix()
				switch tr {
				case transportEventsAPI:
					wh := &webhookHandler{stores: r.stores, pipeline: r.pipeline, deployment: eventsAPIDeploymentApp()}
					if rec := postSignedDeployment(t, wh, payload); rec.Code != http.StatusOK {
						t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
					}
				case transportSocket:
					c := &appConnection{appID: deployTestAppID, deployment: true, appToken: deployTestAppToken, sightings: map[string]*sightingView{}}
					env := socketEnvelope{Type: "events_api", EnvelopeID: "env-1", Payload: mustJSON(t, payload)}
					if !c.processEventsAPIEnvelope(t.Context(), r.stores, r.pipeline, env) {
						t.Fatal("envelope not acked; want acked")
					}
				}

				if ws := row("T0DEPREM1"); ws != nil {
					t.Errorf("row after the removal = %+v; want deleted", ws)
				}
				if v := botToken("T0DEPREM1"); v != "" {
					t.Errorf("bot token after the removal = %q; want deleted", v)
				}
				if row("T0DEPREM2") == nil || botToken("T0DEPREM2") != "xoxb-deployment-2" {
					t.Error("the other workspace's connection or its bot token is gone; want both kept")
				}
				if got := r.outcomes(); got[outcomeDisconnected] != 1 || len(got) != 1 {
					t.Errorf("outcomes = %v; want exactly one %q", got, outcomeDisconnected)
				}
			})
		}
	}
}

func assertSecretsUnchanged(t *testing.T, before, after []string) {
	t.Helper()
	for i := range before {
		if after[i] != before[i] {
			t.Errorf("secret %d = %q; want it kept as %q", i, after[i], before[i])
		}
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
