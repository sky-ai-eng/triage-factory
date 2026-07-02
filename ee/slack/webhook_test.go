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
	"testing"
	"time"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
)

// ---------- fakes ----------

// fakeWorkspaceStore is a minimal slackstore.WorkspaceStore fake, keyed on
// workspace_id only — GetByWorkspaceIDSystem is the only method the webhook
// handler actually calls.
type fakeWorkspaceStore struct {
	byWorkspaceID map[string]*slackstore.Workspace
}

var _ slackstore.WorkspaceStore = (*fakeWorkspaceStore)(nil)

func (f *fakeWorkspaceStore) Upsert(context.Context, slackstore.Workspace) error { return nil }
func (f *fakeWorkspaceStore) ListForOrg(context.Context, string) ([]slackstore.Workspace, error) {
	return nil, nil
}
func (f *fakeWorkspaceStore) Get(context.Context, string, string) (*slackstore.Workspace, error) {
	return nil, nil
}
func (f *fakeWorkspaceStore) Delete(context.Context, string, string) error { return nil }
func (f *fakeWorkspaceStore) ListAllSystem(context.Context) ([]slackstore.Workspace, error) {
	return nil, nil
}
func (f *fakeWorkspaceStore) GetByWorkspaceIDSystem(_ context.Context, workspaceID string) (*slackstore.Workspace, error) {
	return f.byWorkspaceID[workspaceID], nil
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
	webhookTestSigningRef  = "slack_ws_T0WEBHOOK1_signing_secret"
	webhookTestSecret      = "8f742231b10e8888abcd99yyyzzz85a"
)

type webhookRig struct {
	h         *webhookHandler
	published *[]domain.Event
}

// newWebhookRig builds a webhookHandler wired to fakes: a single connected
// workspace (webhookTestWorkspaceID, bound to webhookTestOrgID, signing
// secret webhookTestSecret) and FeatureSlack licensed for that org.
// signingSecretRef == "" simulates a socket-transport workspace (no signing
// secret stored).
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
		OrgID:            webhookTestOrgID,
		BotUserID:        "U0BOT",
		SigningSecretRef: signingSecretRef,
	}
	wsStore := &fakeWorkspaceStore{byWorkspaceID: map[string]*slackstore.Workspace{webhookTestWorkspaceID: ws}}
	secrets := &fakeSecretStore{values: map[string]string{webhookTestOrgID + "/" + webhookTestSigningRef: webhookTestSecret}}
	stores := db.Stores{
		Secrets: secrets,
		Ext:     map[string]any{slackstore.ExtKey: &slackstore.Bundle{Workspaces: wsStore}},
	}

	published := &[]domain.Event{}
	pipeline := &ingestPipeline{
		entities:   newFakeEntities(),
		deliveries: newFakeDeliveries(),
		publish:    func(evt domain.Event) { *published = append(*published, evt) },
	}
	return &webhookRig{h: &webhookHandler{stores: stores, pipeline: pipeline}, published: published}
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
	body := []byte(`{"type":"event_callback","team_id":"` + webhookTestWorkspaceID + `","event_id":"Ev1","event":{"type":"app_mention"}}`)
	rec := postSlackWebhook(t, r.h, "some-other-org", body, nowTimestamp(), "v0=deadbeef")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (cross-org team_id acks empty); body=%s", rec.Code, rec.Body.String())
	}
	if len(*r.published) != 0 {
		t.Errorf("published %d events; want 0", len(*r.published))
	}
}

// ---------- entitlement ----------

func TestHandleWebhook_LapsedEntitlement_AckEmptyNoPublish(t *testing.T) {
	r := newWebhookRig(t, webhookTestSigningRef, false)
	ts := nowTimestamp()
	body := []byte(`{"type":"event_callback","team_id":"` + webhookTestWorkspaceID + `","event_id":"Ev1","event":{"type":"app_mention","channel":"C1","user":"U1","ts":"1.0"}}`)
	sig := sign(webhookTestSecret, ts, body)
	rec := postSlackWebhook(t, r.h, webhookTestOrgID, body, ts, sig)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (lapsed org acks empty); body=%s", rec.Code, rec.Body.String())
	}
	if len(*r.published) != 0 {
		t.Errorf("published %d events for a lapsed org; want 0", len(*r.published))
	}
}

// ---------- signature ----------

func TestHandleWebhook_SocketOnlyWorkspace_401(t *testing.T) {
	r := newWebhookRig(t, "", true) // no signing secret stored
	body := []byte(`{"type":"url_verification","team_id":"` + webhookTestWorkspaceID + `","challenge":"abc123"}`)
	rec := postSlackWebhook(t, r.h, webhookTestOrgID, body, nowTimestamp(), "v0=irrelevant")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (socket-transport workspace has no signing secret)", rec.Code)
	}
}

func TestHandleWebhook_MissingSignatureHeaders_401(t *testing.T) {
	r := newWebhookRig(t, webhookTestSigningRef, true)
	body := []byte(`{"type":"url_verification","team_id":"` + webhookTestWorkspaceID + `","challenge":"abc123"}`)

	if rec := postSlackWebhook(t, r.h, webhookTestOrgID, body, "", sign(webhookTestSecret, nowTimestamp(), body)); rec.Code != http.StatusUnauthorized {
		t.Errorf("missing timestamp: status = %d, want 401", rec.Code)
	}
	if rec := postSlackWebhook(t, r.h, webhookTestOrgID, body, nowTimestamp(), ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("missing signature: status = %d, want 401", rec.Code)
	}
}

func TestHandleWebhook_BadSignature_401(t *testing.T) {
	r := newWebhookRig(t, webhookTestSigningRef, true)
	body := []byte(`{"type":"url_verification","team_id":"` + webhookTestWorkspaceID + `","challenge":"abc123"}`)
	rec := postSlackWebhook(t, r.h, webhookTestOrgID, body, nowTimestamp(), "v0="+hex.EncodeToString(make([]byte, 32)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (signature mismatch)", rec.Code)
	}
}

func TestHandleWebhook_StaleTimestamp_401(t *testing.T) {
	r := newWebhookRig(t, webhookTestSigningRef, true)
	body := []byte(`{"type":"url_verification","team_id":"` + webhookTestWorkspaceID + `","challenge":"abc123"}`)
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
	body := []byte(`{"type":"url_verification","team_id":"` + webhookTestWorkspaceID + `","challenge":"abc123xyz"}`)
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
	body := []byte(`{"type":"url_verification","team_id":"` + webhookTestWorkspaceID + `","challenge":"abc123xyz"}`)
	rec := postSlackWebhook(t, r.h, webhookTestOrgID, body, "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (unsigned handshake)", rec.Code)
	}
}

// ---------- event_callback ----------

func TestHandleWebhook_EventCallback_PublishesAndAcks(t *testing.T) {
	r := newWebhookRig(t, webhookTestSigningRef, true)
	body := []byte(`{"type":"event_callback","team_id":"` + webhookTestWorkspaceID + `","event_id":"Ev-http-1","event":{"type":"app_mention","channel":"C1","user":"U1","text":"hi there","ts":"1600000000.000100"}}`)
	ts := nowTimestamp()
	rec := postSlackWebhook(t, r.h, webhookTestOrgID, body, ts, sign(webhookTestSecret, ts, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(*r.published) != 1 {
		t.Fatalf("published %d events; want 1", len(*r.published))
	}
	if got := (*r.published)[0]; got.EventType != domain.EventSlackMention || got.OrgID != webhookTestOrgID {
		t.Errorf("published event = %+v; want slack:mention for %s", got, webhookTestOrgID)
	}
}
