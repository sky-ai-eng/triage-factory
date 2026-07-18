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
	"strconv"
	"sync"
	"testing"
	"time"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
)

// ---------- fakes ----------

// fakeWorkspaceStore is a minimal slackstore.WorkspaceStore fake, keyed on
// (workspace_id, api_app_id) — GetByWorkspaceAppSystem is the only method
// the webhook handler actually calls. The socket manager's reconciler also
// calls ListAllSystem and (in the reconciler tests) mutates the row set
// concurrently with a running reconcile loop, so every method is guarded by
// mu — a no-op for callers that only ever read this fake before starting
// any goroutine, and required for the ones that don't.
type fakeWorkspaceStore struct {
	mu             sync.Mutex
	byWorkspaceApp map[string]*slackstore.Workspace // "workspace_id/api_app_id" -> row
}

var _ slackstore.WorkspaceStore = (*fakeWorkspaceStore)(nil)

// newFakeWorkspaceStore builds a fake seeded with rows, keyed by
// (workspace_id, api_app_id).
func newFakeWorkspaceStore(rows ...*slackstore.Workspace) *fakeWorkspaceStore {
	f := &fakeWorkspaceStore{byWorkspaceApp: map[string]*slackstore.Workspace{}}
	for _, r := range rows {
		f.setRow(r)
	}
	return f
}

// setRow inserts or replaces a row — the reconciler tests use this to
// simulate a connect/disconnect happening while the manager is running.
func (f *fakeWorkspaceStore) setRow(r *slackstore.Workspace) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.byWorkspaceApp == nil {
		f.byWorkspaceApp = map[string]*slackstore.Workspace{}
	}
	f.byWorkspaceApp[r.WorkspaceID+"/"+r.APIAppID] = r
}

// deleteRow removes a row — simulates the disconnect handler's DELETE.
func (f *fakeWorkspaceStore) deleteRow(workspaceID, apiAppID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.byWorkspaceApp, workspaceID+"/"+apiAppID)
}

func (f *fakeWorkspaceStore) Upsert(context.Context, slackstore.Workspace) error { return nil }
func (f *fakeWorkspaceStore) ListForOrg(context.Context, string) ([]slackstore.Workspace, error) {
	return nil, nil
}
func (f *fakeWorkspaceStore) Get(context.Context, string, string, string) (*slackstore.Workspace, error) {
	return nil, nil
}
func (f *fakeWorkspaceStore) Delete(context.Context, string, string, string) error { return nil }
func (f *fakeWorkspaceStore) ListAllSystem(context.Context) ([]slackstore.Workspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]slackstore.Workspace, 0, len(f.byWorkspaceApp))
	for _, r := range f.byWorkspaceApp {
		out = append(out, *r)
	}
	return out, nil
}
func (f *fakeWorkspaceStore) GetByWorkspaceAppSystem(_ context.Context, workspaceID, apiAppID string) (*slackstore.Workspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.byWorkspaceApp[workspaceID+"/"+apiAppID], nil
}
func (f *fakeWorkspaceStore) LockApp(context.Context, string) error { return nil }
func (f *fakeWorkspaceStore) AppBoundToOtherOrgSystem(context.Context, string, string) (bool, error) {
	return false, nil
}

// fakeSecretStore is a minimal db.SecretStore fake — only GetSystem is
// exercised by the webhook handler.
type fakeSecretStore struct {
	values map[string]string // orgID + "/" + key -> value
}

var _ db.SecretStore = (*fakeSecretStore)(nil)

func (f *fakeSecretStore) Put(context.Context, string, string, string, string) error { return nil }
func (f *fakeSecretStore) Get(context.Context, string, string) (string, error)       { return "", nil }
func (f *fakeSecretStore) GetSystem(_ context.Context, orgID, key string) (string, error) {
	return f.values[orgID+"/"+key], nil
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
func (f *fakeSecretStore) DeleteUser(context.Context, string, string, string) (bool, error) {
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

	ws := &slackstore.Workspace{
		WorkspaceID:      webhookTestWorkspaceID,
		APIAppID:         webhookTestAppID,
		OrgID:            webhookTestOrgID,
		BotUserID:        "U0BOT",
		SigningSecretRef: signingSecretRef,
	}
	wsStore := &fakeWorkspaceStore{byWorkspaceApp: map[string]*slackstore.Workspace{webhookTestWorkspaceID + "/" + webhookTestAppID: ws}}
	secrets := &fakeSecretStore{values: map[string]string{webhookTestOrgID + "/" + webhookTestSigningRef: webhookTestSecret}}
	stores := db.Stores{
		Secrets: secrets,
		Ext:     map[string]any{slackstore.ExtKey: &slackstore.Bundle{Workspaces: wsStore}},
	}

	published := &[]domain.Event{}
	deliveries := newFakeDeliveries()
	pipeline := &ingestPipeline{
		entities:   newFakeEntities(),
		deliveries: deliveries,
		publish:    func(evt domain.Event) { *published = append(*published, evt) },
	}
	return &webhookRig{h: &webhookHandler{stores: stores, pipeline: pipeline}, published: published, deliveries: deliveries}
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
	ws1 := &slackstore.Workspace{WorkspaceID: sharedWorkspaceID, APIAppID: app1, OrgID: org1, BotUserID: "U0BOT1", SigningSecretRef: ref1}
	ws2 := &slackstore.Workspace{WorkspaceID: sharedWorkspaceID, APIAppID: app2, OrgID: org2, BotUserID: "U0BOT2", SigningSecretRef: ref2}
	wsStore := &fakeWorkspaceStore{byWorkspaceApp: map[string]*slackstore.Workspace{
		sharedWorkspaceID + "/" + app1: ws1,
		sharedWorkspaceID + "/" + app2: ws2,
	}}
	secrets := &fakeSecretStore{values: map[string]string{
		org1 + "/" + ref1: secret1,
		org2 + "/" + ref2: secret2,
	}}
	stores := db.Stores{Secrets: secrets, Ext: map[string]any{slackstore.ExtKey: &slackstore.Bundle{Workspaces: wsStore}}}
	published := &[]domain.Event{}
	pipeline := &ingestPipeline{
		entities:   newFakeEntities(),
		deliveries: newFakeDeliveries(),
		publish:    func(evt domain.Event) { *published = append(*published, evt) },
	}
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
