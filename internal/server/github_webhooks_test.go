package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/zalando/go-keyring"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

const testWebhookSecret = "s3cr3t-webhook-key"

// seedWebhookApp registers an org_github_apps row + stores its webhook
// secret so the receiver can verify signatures for LocalDefaultOrgID.
func seedWebhookApp(t *testing.T, s *Server) {
	t.Helper()
	keyring.MockInit()
	if _, err := s.db.Exec(`
		INSERT OR IGNORE INTO org_github_apps
			(org_id, app_id, slug, client_id, client_secret_ref, pem_ref, webhook_secret_ref)
		VALUES (?, '999', 'tf', 'Iv1.x', 'cs', 'pem', 'wh_ref')
	`, runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("seed org_github_apps: %v", err)
	}
	if err := s.secrets.Put(context.Background(), runmode.LocalDefaultOrgID, "wh_ref", testWebhookSecret, ""); err != nil {
		t.Fatalf("seed webhook secret: %v", err)
	}
}

func sign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(testWebhookSecret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// postWebhook fires a delivery at the receiver with the given event name,
// signature header, and raw body.
func postWebhook(s *Server, event, sigHeader string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/webhooks/github/"+runmode.LocalDefaultOrgID, bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", "test-delivery-1")
	if sigHeader != "" {
		req.Header.Set("X-Hub-Signature-256", sigHeader)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func installations(t *testing.T, s *Server) []domain.OrgGitHubAppInstallation {
	t.Helper()
	got, err := sqlitestore.New(s.db).GitHubApps.ListInstallationsForOrg(context.Background(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("ListInstallationsForOrg: %v", err)
	}
	return got
}

func TestGitHubWebhook_InstallationCreated(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedWebhookApp(t, s)

	body := []byte(`{"action":"created","installation":{"id":4242,"account":{"login":"acme","type":"Organization"},"created_at":"2026-01-01T00:00:00Z"}}`)
	rec := postWebhook(s, "installation", sign(body), body)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	got := installations(t, s)
	if len(got) != 1 || got[0].InstallationID != "4242" || got[0].AccountLogin != "acme" {
		t.Fatalf("installations = %+v, want one acme/4242 row", got)
	}
}

func TestGitHubWebhook_BadSignature_NoWrite(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedWebhookApp(t, s)

	body := []byte(`{"action":"created","installation":{"id":4242,"account":{"login":"acme","type":"Organization"}}}`)
	rec := postWebhook(s, "installation", "sha256=deadbeef", body)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("401 response has body %q, want empty", rec.Body.String())
	}
	if got := installations(t, s); len(got) != 0 {
		t.Errorf("bad-signature delivery wrote %d installations, want 0", len(got))
	}
}

func TestGitHubWebhook_MissingSignature(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedWebhookApp(t, s)

	body := []byte(`{"action":"created","installation":{"id":1,"account":{"login":"x","type":"User"}}}`)
	rec := postWebhook(s, "installation", "", body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestGitHubWebhook_InstallationDeleted_FiresHook(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedWebhookApp(t, s)

	// Pre-seed an active installation to remove.
	stores := sqlitestore.New(s.db)
	if err := stores.GitHubApps.UpsertInstallation(context.Background(), domain.OrgGitHubAppInstallation{
		InstallationID: "4242",
		OrgID:          runmode.LocalDefaultOrgID,
		AccountType:    "Organization",
		AccountLogin:   "acme",
	}); err != nil {
		t.Fatalf("pre-seed installation: %v", err)
	}

	var (
		mu       sync.Mutex
		firedOrg string
		firedID  string
	)
	s.SetInstallationRemovedHook(func(orgID, installationID string) {
		mu.Lock()
		defer mu.Unlock()
		firedOrg, firedID = orgID, installationID
	})

	body := []byte(`{"action":"deleted","installation":{"id":4242,"account":{"login":"acme","type":"Organization"}}}`)
	rec := postWebhook(s, "installation", sign(body), body)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if got := installations(t, s); len(got) != 0 {
		t.Errorf("after delete got %d active installations, want 0", len(got))
	}
	mu.Lock()
	defer mu.Unlock()
	if firedOrg != runmode.LocalDefaultOrgID || firedID != "4242" {
		t.Errorf("invalidate hook fired with (%q, %q), want (%q, 4242)", firedOrg, firedID, runmode.LocalDefaultOrgID)
	}
}

func TestGitHubWebhook_NonInstallationEvent_PublishesToBus(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedWebhookApp(t, s)

	bus := eventbus.New()
	s.SetEventBus(bus)

	got := make(chan domain.Event, 1)
	bus.Subscribe(eventbus.Subscriber{
		Name:   "test-webhook-capture",
		Filter: []string{"webhook:github:"},
		Handle: func(e domain.Event) { got <- e },
	})

	body := []byte(`{"action":"synchronize","number":7}`)
	rec := postWebhook(s, "pull_request", sign(body), body)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}

	select {
	case e := <-got:
		if e.EventType != "webhook:github:pull_request" {
			t.Errorf("published event type = %q, want webhook:github:pull_request", e.EventType)
		}
		if e.OrgID != runmode.LocalDefaultOrgID {
			t.Errorf("published event org = %q, want %q", e.OrgID, runmode.LocalDefaultOrgID)
		}
	case <-t.Context().Done():
		t.Fatal("timed out waiting for published webhook event")
	}
}

func TestGitHubWebhook_NoAppRegistered_404(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	// No app seeded.

	body := []byte(`{"action":"created","installation":{"id":1,"account":{"login":"x","type":"User"}}}`)
	rec := postWebhook(s, "installation", sign(body), body)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no registered app)", rec.Code)
	}
}
