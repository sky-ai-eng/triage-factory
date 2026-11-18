package slack

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
)

// ---------- fakes ----------

// fakeConnectionStore is a minimal slackstore.ConnectionStore fake, keyed on
// (workspace_id, api_app_id) — GetByWorkspaceAppSystem is the only method
// the webhook handler actually calls. The socket manager's reconciler also
// calls ListAllSystem and (in the reconciler tests) mutates the row set
// concurrently with a running reconcile loop, so every method is guarded by
// mu — a no-op for callers that only ever read this fake before starting
// any goroutine, and required for the ones that don't.
type fakeConnectionStore struct {
	mu             sync.Mutex
	byWorkspaceApp map[string]*slackstore.Connection // "workspace_id/api_app_id" -> row
}

var _ slackstore.ConnectionStore = (*fakeConnectionStore)(nil)

// newFakeConnectionStore builds a fake seeded with rows, keyed by
// (workspace_id, api_app_id).
func newFakeConnectionStore(rows ...*slackstore.Connection) *fakeConnectionStore {
	f := &fakeConnectionStore{byWorkspaceApp: map[string]*slackstore.Connection{}}
	for _, r := range rows {
		f.setRow(r)
	}
	return f
}

// setRow inserts or replaces a row — the reconciler tests use this to
// simulate a connect/disconnect happening while the manager is running.
func (f *fakeConnectionStore) setRow(r *slackstore.Connection) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.byWorkspaceApp == nil {
		f.byWorkspaceApp = map[string]*slackstore.Connection{}
	}
	f.byWorkspaceApp[r.WorkspaceID+"/"+r.APIAppID] = r
}

// deleteRow removes a row — simulates the disconnect handler's DELETE.
func (f *fakeConnectionStore) deleteRow(workspaceID, apiAppID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.byWorkspaceApp, workspaceID+"/"+apiAppID)
}

func (f *fakeConnectionStore) Upsert(_ context.Context, c slackstore.Connection) (slackstore.Connection, error) {
	return c, nil
}
func (f *fakeConnectionStore) ListForOrgSystem(ctx context.Context, orgID string) ([]slackstore.Connection, error) {
	return f.ListForOrg(ctx, orgID)
}

func (f *fakeConnectionStore) ListForOrg(context.Context, string) ([]slackstore.Connection, error) {
	return nil, nil
}
func (f *fakeConnectionStore) Get(context.Context, string, string, string) (*slackstore.Connection, error) {
	return nil, nil
}
func (f *fakeConnectionStore) Delete(context.Context, string, string, string) error { return nil }
func (f *fakeConnectionStore) DeleteSystem(_ context.Context, orgID, workspaceID, apiAppID string, updatedAt time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := workspaceID + "/" + apiAppID
	row, ok := f.byWorkspaceApp[key]
	if !ok || row.OrgID != orgID || !row.UpdatedAt.Equal(updatedAt) {
		return false, nil
	}
	delete(f.byWorkspaceApp, key)
	return true, nil
}
func (f *fakeConnectionStore) ListAllSystem(context.Context) ([]slackstore.Connection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]slackstore.Connection, 0, len(f.byWorkspaceApp))
	for _, r := range f.byWorkspaceApp {
		out = append(out, *r)
	}
	return out, nil
}
func (f *fakeConnectionStore) ActiveForWorkspaceSystem(_ context.Context, orgID, workspaceID string) (*slackstore.Connection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.byWorkspaceApp {
		if r.OrgID == orgID && r.WorkspaceID == workspaceID && r.Active {
			return r, nil
		}
	}
	return nil, nil
}
func (f *fakeConnectionStore) GetByWorkspaceAppSystem(_ context.Context, workspaceID, apiAppID string) (*slackstore.Connection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.byWorkspaceApp[workspaceID+"/"+apiAppID], nil
}
func (f *fakeConnectionStore) LockApp(context.Context, string) error { return nil }
func (f *fakeConnectionStore) AppBoundToOtherOrgSystem(context.Context, string, string) (bool, error) {
	return false, nil
}

// fakeSecretStore is a minimal db.SecretStore fake — only GetSystem is
// exercised by the webhook handler. mu guards values for the socket tests,
// which delete a secret while a connection goroutine may read it.
type fakeSecretStore struct {
	mu     sync.Mutex
	values map[string]string // orgID + "/" + key -> value
}

// deleteValue removes a secret, as a disconnect's sweep does.
func (f *fakeSecretStore) deleteValue(orgID, key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.values, orgID+"/"+key)
}

var _ db.SecretStore = (*fakeSecretStore)(nil)

func (f *fakeSecretStore) Put(context.Context, string, string, string, string) error { return nil }
func (f *fakeSecretStore) Get(context.Context, string, string) (string, error)       { return "", nil }
func (f *fakeSecretStore) GetSystem(_ context.Context, orgID, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.values[orgID+"/"+key], nil
}
func (f *fakeSecretStore) PutSystemIfValue(context.Context, string, string, string, string, string) (bool, error) {
	return false, nil
}
func (f *fakeSecretStore) DeleteSystemIfValue(context.Context, string, string, string) (bool, error) {
	return false, nil
}
func (f *fakeSecretStore) Delete(context.Context, string, string) (bool, error) { return false, nil }
func (f *fakeSecretStore) PutUser(context.Context, string, string, string, string, string) error {
	return nil
}
func (f *fakeSecretStore) GetUser(context.Context, string, string, string) (string, error) {
	return "", nil
}
func (f *fakeSecretStore) GetUserSystem(context.Context, string, string, string) (string, error) {
	return "", nil
}
func (f *fakeSecretStore) PutUserSystem(context.Context, string, string, string, string, string) error {
	return nil
}
func (f *fakeSecretStore) PutUserSystemIfValue(context.Context, string, string, string, string, string, string) (bool, error) {
	return false, nil
}
func (f *fakeSecretStore) DeleteUser(context.Context, string, string, string) (bool, error) {
	return false, nil
}
func (f *fakeSecretStore) DeleteUserSystemIfValue(context.Context, string, string, string, string) (bool, error) {
	return false, nil
}

// ---------- rig ----------

const (
	webhookTestOrgID       = "org-webhook-1"
	webhookTestWorkspaceID = "T0WEBHOOK1"
	webhookTestAppID       = "A0WEBHOOK1"
	webhookTestSigningRef  = "slack_ws_T0WEBHOOK1_A0WEBHOOK1_signing_secret"
	webhookTestSecret      = "8f742231b10e8888abcd99yyyzzz85a"
)

type webhookRig struct {
	h          *webhookHandler
	published  *[]domain.Event
	deliveries *fakeDeliveries
	channels   *fakeChannelRegistry
	// metrics reads back what the rig's ingestStats recorded — see
	// newTestStats/sumByAttr (stats_test.go).
	metrics *sdkmetric.ManualReader
}

// newWebhookRig builds a webhookHandler wired to fakes: a single connected
// (workspace, app) pair (webhookTestWorkspaceID, webhookTestAppID, bound to
// webhookTestOrgID, signing secret webhookTestSecret) and FeatureSlack
// licensed for that org. signingSecretRef == "" simulates a
// socket-transport workspace (no signing secret stored).
func newWebhookRig(t *testing.T, signingSecretRef string, licensed bool) *webhookRig {
	t.Helper()
	if licensed {
		entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureSlack))
	} else {
		entitlements.RegisterProvider(entitlements.Static())
	}
	t.Cleanup(entitlements.Reset)

	ws := &slackstore.Connection{
		WorkspaceID:      webhookTestWorkspaceID,
		APIAppID:         webhookTestAppID,
		OrgID:            webhookTestOrgID,
		Source:           slackstore.SourceOrg,
		Active:           true,
		BotUserID:        "U0BOT",
		SigningSecretRef: signingSecretRef,
	}
	wsStore := &fakeConnectionStore{byWorkspaceApp: map[string]*slackstore.Connection{webhookTestWorkspaceID + "/" + webhookTestAppID: ws}}
	secrets := &fakeSecretStore{values: map[string]string{webhookTestOrgID + "/" + webhookTestSigningRef: webhookTestSecret}}
	stores := db.Stores{
		Secrets: secrets,
		Ext:     map[string]any{slackstore.ExtKey: &slackstore.Bundle{Connections: wsStore}},
	}

	published := &[]domain.Event{}
	deliveries := newFakeDeliveries()
	entities := newFakeEntities()
	channels := newFakeChannelRegistryFor(entities)
	stats, metrics := newTestStats()
	pipeline := &ingestPipeline{
		connections: wsStore,
		deliveries:  deliveries,
		channels:    channels,
		publish:     func(_ context.Context, evt domain.Event) { *published = append(*published, evt) },
		stats:       stats,
	}
	withFakeIngestTx(pipeline)
	return &webhookRig{
		h:          &webhookHandler{stores: stores, pipeline: pipeline, stats: stats},
		published:  published,
		deliveries: deliveries,
		channels:   channels,
		metrics:    metrics,
	}
}

// sign computes a valid X-Slack-Signature for (secret, timestamp, body) per
// Slack's documented v0 scheme — independently reconstructed here (not by
// calling validSlackSignature) so the test actually exercises the wire
// format the handler expects.
func sign(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + timestamp + ":"))
	mac.Write(body)
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

func postSlackWebhook(t *testing.T, h *webhookHandler, orgID string, body []byte, timestamp, sigHeader string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/slack/"+orgID, bytes.NewReader(body))
	req.SetPathValue("org_id", orgID)
	if timestamp != "" {
		req.Header.Set("X-Slack-Request-Timestamp", timestamp)
	}
	if sigHeader != "" {
		req.Header.Set("X-Slack-Signature", sigHeader)
	}
	rec := httptest.NewRecorder()
	h.handleWebhook(rec, req)
	return rec
}

func nowTimestamp() string {
	return strconv.FormatInt(time.Now().Unix(), 10)
}

// ---------- workspace resolution ----------

func TestHandleWebhook_UnknownTeamID_AckEmpty(t *testing.T) {
	r := newWebhookRig(t, webhookTestSigningRef, true)
	body := []byte(`{"type":"event_callback","team_id":"T0UNKNOWN","event_id":"Ev1","event":{"type":"app_mention"}}`)
	rec := postSlackWebhook(t, r.h, webhookTestOrgID, body, nowTimestamp(), "v0=deadbeef")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (unknown team_id acks empty); body=%s", rec.Code, rec.Body.String())
	}
	if len(*r.published) != 0 {
		t.Errorf("published %d events; want 0", len(*r.published))
	}
}

func TestHandleWebhook_CrossOrgTeamID_AckEmpty(t *testing.T) {
	r := newWebhookRig(t, webhookTestSigningRef, true)
	body := []byte(`{"type":"event_callback","team_id":"` + webhookTestWorkspaceID + `","api_app_id":"` + webhookTestAppID + `","event_id":"Ev1","event":{"type":"app_mention"}}`)
	rec := postSlackWebhook(t, r.h, "some-other-org", body, nowTimestamp(), "v0=deadbeef")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (cross-org team_id acks empty); body=%s", rec.Code, rec.Body.String())
	}
	if len(*r.published) != 0 {
		t.Errorf("published %d events; want 0", len(*r.published))
	}
}

// TestHandleWebhook_UnknownAPIAppID_AckEmpty: a known team_id paired with an
// unrecognized (or missing) api_app_id acks empty, same as any other
// unresolvable delivery — the composite key means a correct workspace alone
// is no longer enough to resolve a row.
func TestHandleWebhook_UnknownAPIAppID_AckEmpty(t *testing.T) {
	r := newWebhookRig(t, webhookTestSigningRef, true)
	body := []byte(`{"type":"event_callback","team_id":"` + webhookTestWorkspaceID + `","api_app_id":"A0NOTREAL1","event_id":"Ev1","event":{"type":"app_mention"}}`)
	rec := postSlackWebhook(t, r.h, webhookTestOrgID, body, nowTimestamp(), "v0=deadbeef")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (unknown api_app_id acks empty); body=%s", rec.Code, rec.Body.String())
	}
	if len(*r.published) != 0 {
		t.Errorf("published %d events; want 0", len(*r.published))
	}
}

// TestHandleWebhook_MissingAPIAppID_AckEmpty: a payload missing api_app_id
// entirely gets the identical ack-empty treatment as an unknown one.
func TestHandleWebhook_MissingAPIAppID_AckEmpty(t *testing.T) {
	r := newWebhookRig(t, webhookTestSigningRef, true)
	body := []byte(`{"type":"event_callback","team_id":"` + webhookTestWorkspaceID + `","event_id":"Ev1","event":{"type":"app_mention"}}`)
	rec := postSlackWebhook(t, r.h, webhookTestOrgID, body, nowTimestamp(), "v0=deadbeef")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (missing api_app_id acks empty); body=%s", rec.Code, rec.Body.String())
	}
	if len(*r.published) != 0 {
		t.Errorf("published %d events; want 0", len(*r.published))
	}
}

// ---------- entitlement ----------

func TestHandleWebhook_LapsedEntitlement_AckEmptyNoPublish(t *testing.T) {
	r := newWebhookRig(t, webhookTestSigningRef, false)
	ts := nowTimestamp()
	body := []byte(`{"type":"event_callback","team_id":"` + webhookTestWorkspaceID + `","api_app_id":"` + webhookTestAppID + `","event_id":"Ev1","event":{"type":"app_mention","channel":"C1","user":"U1","ts":"1.0"}}`)
	sig := sign(webhookTestSecret, ts, body)
	rec := postSlackWebhook(t, r.h, webhookTestOrgID, body, ts, sig)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (lapsed org acks empty); body=%s", rec.Code, rec.Body.String())
	}
	if len(*r.published) != 0 {
		t.Errorf("published %d events for a lapsed org; want 0", len(*r.published))
	}
}

// TestHandleWebhook_URLVerification_SucceedsRegardlessOfEntitlement: the
// handshake proves the endpoint is live and correctly signed, not that an
// event was processed — a lapsed org must still be able to complete it
// (Slack's app-config UI shouldn't error just because a license lapsed),
// same as a licensed org.
func TestHandleWebhook_URLVerification_SucceedsRegardlessOfEntitlement(t *testing.T) {
	r := newWebhookRig(t, webhookTestSigningRef, false) // NOT licensed
	body := []byte(`{"type":"url_verification","team_id":"` + webhookTestWorkspaceID + `","api_app_id":"` + webhookTestAppID + `","challenge":"lapsed-org-challenge"}`)
	ts := nowTimestamp()
	rec := postSlackWebhook(t, r.h, webhookTestOrgID, body, ts, sign(webhookTestSecret, ts, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (handshake succeeds even for a lapsed org); body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.Challenge != "lapsed-org-challenge" {
		t.Errorf("challenge = %q; want lapsed-org-challenge", out.Challenge)
	}
}

// TestHandleWebhook_BadSignature_NoEntitlementOracle: a bad/missing
// signature must get the identical 401 whether the target org's Slack
// feature is licensed or lapsed — otherwise a caller who merely knows a
// team_id (not itself secret) could infer the org's entitlement state
// without ever forging a valid signature, since only a genuine signature
// holder (Slack) can reach the entitlement check at all.
func TestHandleWebhook_BadSignature_NoEntitlementOracle(t *testing.T) {
	body := []byte(`{"type":"event_callback","team_id":"` + webhookTestWorkspaceID + `","api_app_id":"` + webhookTestAppID + `","event_id":"Ev1","event":{"type":"app_mention","channel":"C1","user":"U1","ts":"1.0"}}`)
	badSig := "v0=" + hex.EncodeToString(make([]byte, 32))

	for _, licensed := range []bool{true, false} {
		r := newWebhookRig(t, webhookTestSigningRef, licensed)
		rec := postSlackWebhook(t, r.h, webhookTestOrgID, body, nowTimestamp(), badSig)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("licensed=%v: status = %d, want 401 regardless of entitlement", licensed, rec.Code)
		}
	}
}

// ---------- signature ----------

func TestHandleWebhook_SocketOnlyWorkspace_401(t *testing.T) {
	r := newWebhookRig(t, "", true) // no signing secret stored
	body := []byte(`{"type":"url_verification","team_id":"` + webhookTestWorkspaceID + `","api_app_id":"` + webhookTestAppID + `","challenge":"abc123"}`)
	rec := postSlackWebhook(t, r.h, webhookTestOrgID, body, nowTimestamp(), "v0=irrelevant")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (socket-transport workspace has no signing secret)", rec.Code)
	}
}

func TestHandleWebhook_MissingSignatureHeaders_401(t *testing.T) {
	r := newWebhookRig(t, webhookTestSigningRef, true)
	body := []byte(`{"type":"url_verification","team_id":"` + webhookTestWorkspaceID + `","api_app_id":"` + webhookTestAppID + `","challenge":"abc123"}`)

	if rec := postSlackWebhook(t, r.h, webhookTestOrgID, body, "", sign(webhookTestSecret, nowTimestamp(), body)); rec.Code != http.StatusUnauthorized {
		t.Errorf("missing timestamp: status = %d, want 401", rec.Code)
	}
	if rec := postSlackWebhook(t, r.h, webhookTestOrgID, body, nowTimestamp(), ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("missing signature: status = %d, want 401", rec.Code)
	}
}

func TestHandleWebhook_BadSignature_401(t *testing.T) {
	r := newWebhookRig(t, webhookTestSigningRef, true)
	body := []byte(`{"type":"url_verification","team_id":"` + webhookTestWorkspaceID + `","api_app_id":"` + webhookTestAppID + `","challenge":"abc123"}`)
	rec := postSlackWebhook(t, r.h, webhookTestOrgID, body, nowTimestamp(), "v0="+hex.EncodeToString(make([]byte, 32)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (signature mismatch)", rec.Code)
	}
}

func TestHandleWebhook_StaleTimestamp_401(t *testing.T) {
	r := newWebhookRig(t, webhookTestSigningRef, true)
	body := []byte(`{"type":"url_verification","team_id":"` + webhookTestWorkspaceID + `","api_app_id":"` + webhookTestAppID + `","challenge":"abc123"}`)
	staleTS := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	rec := postSlackWebhook(t, r.h, webhookTestOrgID, body, staleTS, sign(webhookTestSecret, staleTS, body))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (timestamp outside the 5-minute replay window)", rec.Code)
	}
}

func TestHandleWebhook_GoldenSignature_ValidatesCorrectly(t *testing.T) {
	// Independently-computed HMAC vector (not via validSlackSignature) —
	// pins the exact base-string construction ("v0:" + timestamp + ":" +
	// body) the handler must use.
	secret := "8f742231b10e8888abcd99yyyzzz85a"
	timestamp := "1531420618"
	body := []byte("hello-slack-body")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + timestamp + ":" + string(body)))
	want := "v0=" + hex.EncodeToString(mac.Sum(nil))
	if got := sign(secret, timestamp, body); got != want {
		t.Fatalf("sign() = %q; want %q (helper/vector mismatch)", got, want)
	}
	freshTS := strconv.FormatInt(time.Now().Unix(), 10)
	if !validSlackSignature(secret, body, freshTS, sign(secret, freshTS, body)) {
		t.Error("validSlackSignature rejected a freshly-signed request")
	}
	if validSlackSignature(secret, body, timestamp, want) {
		t.Error("validSlackSignature accepted a signature over a long-stale timestamp")
	}
}

// ---------- handshake ----------

func TestHandleWebhook_URLVerification_SignedEchoesChallenge(t *testing.T) {
	r := newWebhookRig(t, webhookTestSigningRef, true)
	body := []byte(`{"type":"url_verification","team_id":"` + webhookTestWorkspaceID + `","api_app_id":"` + webhookTestAppID + `","challenge":"abc123xyz"}`)
	ts := nowTimestamp()
	rec := postSlackWebhook(t, r.h, webhookTestOrgID, body, ts, sign(webhookTestSecret, ts, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.Challenge != "abc123xyz" {
		t.Errorf("challenge = %q; want abc123xyz", out.Challenge)
	}
}

func TestHandleWebhook_URLVerification_Unsigned401(t *testing.T) {
	r := newWebhookRig(t, webhookTestSigningRef, true)
	body := []byte(`{"type":"url_verification","team_id":"` + webhookTestWorkspaceID + `","api_app_id":"` + webhookTestAppID + `","challenge":"abc123xyz"}`)
	rec := postSlackWebhook(t, r.h, webhookTestOrgID, body, "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (unsigned handshake)", rec.Code)
	}
}

// ---------- event_callback ----------

func TestHandleWebhook_EventCallback_PublishesAndAcks(t *testing.T) {
	r := newWebhookRig(t, webhookTestSigningRef, true)
	body := []byte(`{"type":"event_callback","team_id":"` + webhookTestWorkspaceID + `","api_app_id":"` + webhookTestAppID + `","event_id":"Ev-http-1","event":{"type":"app_mention","channel":"C1","user":"U1","text":"hi there","ts":"1600000000.000100"}}`)
	ts := nowTimestamp()
	rec := postSlackWebhook(t, r.h, webhookTestOrgID, body, ts, sign(webhookTestSecret, ts, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(*r.published) != 1 {
		t.Fatalf("published %d events; want 1", len(*r.published))
	}
	if got := (*r.published)[0]; got.EventType != domain.EventSlackMessage || got.OrgID != webhookTestOrgID {
		t.Errorf("published event = %+v; want slack:message for %s", got, webhookTestOrgID)
	}
}

// TestHandleWebhook_EngagedThreadFollowUp_PublishesUnmentioned is the
// events_api-transport acceptance for the engaged-thread follow-up: a
// root-message @-mention creates the thread entity, then an un-mentioned
// message.channels reply in that same thread reaches the pipeline and publishes
// a second slack:message with Mentioned=false — the full transport → parse →
// engaged-thread-branch → publish path, not just handleThreadMessage in
// isolation.
func TestHandleWebhook_EngagedThreadFollowUp_PublishesUnmentioned(t *testing.T) {
	r := newWebhookRig(t, webhookTestSigningRef, true)

	// Root-message mention (no thread_ts): mints the kind="thread" entity the
	// follow-up's engagement gate looks for.
	rootBody := []byte(`{"type":"event_callback","team_id":"` + webhookTestWorkspaceID + `","api_app_id":"` + webhookTestAppID + `","event_id":"Ev-root","event":{"type":"app_mention","channel":"C1","user":"U1","text":"<@U0BOT> review my PRs","ts":"1600000000.000100"}}`)
	ts := nowTimestamp()
	if rec := postSlackWebhook(t, r.h, webhookTestOrgID, rootBody, ts, sign(webhookTestSecret, ts, rootBody)); rec.Code != http.StatusOK {
		t.Fatalf("root mention: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Un-mentioned reply in the same thread (thread_ts = the root's ts), inner
	// type "message" (message.channels), no subtype, no @-mention.
	followBody := []byte(`{"type":"event_callback","team_id":"` + webhookTestWorkspaceID + `","api_app_id":"` + webhookTestAppID + `","event_id":"Ev-followup","event":{"type":"message","channel":"C1","user":"U1","text":"here are the numbers","ts":"1600000000.000200","thread_ts":"1600000000.000100"}}`)
	ts = nowTimestamp()
	if rec := postSlackWebhook(t, r.h, webhookTestOrgID, followBody, ts, sign(webhookTestSecret, ts, followBody)); rec.Code != http.StatusOK {
		t.Fatalf("follow-up: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	if len(*r.published) != 2 {
		t.Fatalf("published %d events (root + follow-up); want 2", len(*r.published))
	}
	var meta SlackMessageMetadata
	if err := json.Unmarshal([]byte((*r.published)[1].MetadataJSON), &meta); err != nil {
		t.Fatalf("follow-up metadata round-trip: %v", err)
	}
	if meta.Mentioned {
		t.Error("follow-up Mentioned = true; want false (un-mentioned engaged-thread reply)")
	}
	if meta.ThreadTS != "1600000000.000100" {
		t.Errorf("follow-up ThreadTS = %q; want the root ts", meta.ThreadTS)
	}
}

// TestHandleWebhook_ChannelIDChanged_MovesTheChannel: a channel_id_changed
// delivery reaches the pipeline with both ids parsed off the inner event and
// moves the channel for the workspace's org, publishing nothing.
func TestHandleWebhook_ChannelIDChanged_MovesTheChannel(t *testing.T) {
	r := newWebhookRig(t, webhookTestSigningRef, true)
	body := []byte(`{"type":"event_callback","team_id":"` + webhookTestWorkspaceID + `","api_app_id":"` + webhookTestAppID + `","event_id":"Ev-moved","event":{"type":"channel_id_changed","old_channel_id":"G0SHARED1","new_channel_id":"C0SHARED1","event_ts":"1612206778.000000"}}`)
	ts := nowTimestamp()
	if rec := postSlackWebhook(t, r.h, webhookTestOrgID, body, ts, sign(webhookTestSecret, ts, body)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if want := []string{webhookTestOrgID + "/G0SHARED1->C0SHARED1"}; !reflect.DeepEqual(r.channels.sawMoves, want) {
		t.Errorf("moves = %v; want %v", r.channels.sawMoves, want)
	}
	if len(*r.published) != 0 {
		t.Errorf("published %d events; want 0", len(*r.published))
	}
}

// TestHandleWebhook_UnengagedThreadReply_NoPublish is the negative twin: an
// un-mentioned reply in a thread the bot never engaged (no root mention ever
// created an entity) publishes nothing and — critically — records no delivery
// row, so the message firehose can't grow slack_deliveries without bound.
func TestHandleWebhook_UnengagedThreadReply_NoPublish(t *testing.T) {
	r := newWebhookRig(t, webhookTestSigningRef, true)
	body := []byte(`{"type":"event_callback","team_id":"` + webhookTestWorkspaceID + `","api_app_id":"` + webhookTestAppID + `","event_id":"Ev-orphan","event":{"type":"message","channel":"C1","user":"U1","text":"idle channel chatter","ts":"1600000000.000200","thread_ts":"1600000000.000001"}}`)
	ts := nowTimestamp()
	if rec := postSlackWebhook(t, r.h, webhookTestOrgID, body, ts, sign(webhookTestSecret, ts, body)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(*r.published) != 0 {
		t.Errorf("published %d events; want 0 (no engaged thread)", len(*r.published))
	}
	if len(r.deliveries.seen) != 0 {
		t.Errorf("recorded %d deliveries; want 0 — a dropped message must never insert into slack_deliveries", len(r.deliveries.seen))
	}
}

// TestHandleWebhook_TwoAppsOneWorkspace_SignatureSelectionFollowsRow is the
// two-apps-one-workspace acceptance test at the webhook layer: two connected
// apps share a workspace_id but carry distinct api_app_id,
// org, and signing secret. A delivery for app 1 must verify (and route)
// against app 1's row only — app 2's signing secret must not validate it,
// even though both rows share the workspace_id the payload also carries.
func TestHandleWebhook_TwoAppsOneWorkspace_SignatureSelectionFollowsRow(t *testing.T) {
	entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureSlack))
	t.Cleanup(entitlements.Reset)

	const (
		sharedWorkspaceID = "T0TWOAPPS1"
		org1, app1, ref1  = "org-twoapps-1", "A0TWOAPPS1", "slack_ws_T0TWOAPPS1_A0TWOAPPS1_signing_secret"
		org2, app2, ref2  = "org-twoapps-2", "A0TWOAPPS2", "slack_ws_T0TWOAPPS1_A0TWOAPPS2_signing_secret"
		secret1           = "11111111111111111111111111111111"
		secret2           = "22222222222222222222222222222222"
	)
	ws1 := &slackstore.Connection{Source: slackstore.SourceOrg, Active: true, WorkspaceID: sharedWorkspaceID, APIAppID: app1, OrgID: org1, BotUserID: "U0BOT1", SigningSecretRef: ref1}
	ws2 := &slackstore.Connection{Source: slackstore.SourceOrg, Active: true, WorkspaceID: sharedWorkspaceID, APIAppID: app2, OrgID: org2, BotUserID: "U0BOT2", SigningSecretRef: ref2}
	wsStore := &fakeConnectionStore{byWorkspaceApp: map[string]*slackstore.Connection{
		sharedWorkspaceID + "/" + app1: ws1,
		sharedWorkspaceID + "/" + app2: ws2,
	}}
	secrets := &fakeSecretStore{values: map[string]string{
		org1 + "/" + ref1: secret1,
		org2 + "/" + ref2: secret2,
	}}
	stores := db.Stores{Secrets: secrets, Ext: map[string]any{slackstore.ExtKey: &slackstore.Bundle{Connections: wsStore}}}
	published := &[]domain.Event{}
	entities := newFakeEntities()
	pipeline := &ingestPipeline{
		deliveries: newFakeDeliveries(),
		channels:   newFakeChannelRegistryFor(entities),
		publish:    func(_ context.Context, evt domain.Event) { *published = append(*published, evt) },
	}
	withFakeIngestTx(pipeline)
	h := &webhookHandler{stores: stores, pipeline: pipeline}

	body1 := []byte(`{"type":"event_callback","team_id":"` + sharedWorkspaceID + `","api_app_id":"` + app1 + `","event_id":"Ev-app1","event":{"type":"app_mention","channel":"C1","user":"U1","ts":"1.0"}}`)
	ts := nowTimestamp()

	// App 1's own signature succeeds and routes to org 1.
	rec := postSlackWebhook(t, h, org1, body1, ts, sign(secret1, ts, body1))
	if rec.Code != http.StatusOK {
		t.Fatalf("app1 with its own signature: status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(*published) != 1 || (*published)[0].OrgID != org1 {
		t.Fatalf("published after app1 delivery = %+v; want exactly 1 event for %s", *published, org1)
	}

	// The SAME payload signed with app 2's secret must be rejected — the
	// workspace_id alone must never be enough to pick a signing secret.
	*published = nil
	rec = postSlackWebhook(t, h, org1, body1, ts, sign(secret2, ts, body1))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("app1 payload signed with app2's secret: status = %d, want 401", rec.Code)
	}

	// App 2's delivery, correctly signed, routes to org 2 — proving both
	// rows stay independently live and addressable by api_app_id.
	body2 := []byte(`{"type":"event_callback","team_id":"` + sharedWorkspaceID + `","api_app_id":"` + app2 + `","event_id":"Ev-app2","event":{"type":"app_mention","channel":"C1","user":"U2","ts":"2.0"}}`)
	rec = postSlackWebhook(t, h, org2, body2, ts, sign(secret2, ts, body2))
	if rec.Code != http.StatusOK {
		t.Fatalf("app2 with its own signature: status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(*published) != 1 || (*published)[0].OrgID != org2 {
		t.Fatalf("published after app2 delivery = %+v; want exactly 1 event for %s", *published, org2)
	}
}
