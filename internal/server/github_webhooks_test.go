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
	"time"

	"github.com/google/uuid"
	"github.com/zalando/go-keyring"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
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
	// The receiver resolves which secret verifies a delivery from the org's
	// credential class, so a registration row alone isn't the state a
	// registration leaves — the class is written with it.
	seedBYOAppCredentialClass(t, s, runmode.LocalDefaultOrgID)
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
// signature header, and raw body, under a delivery GUID no other call reuses.
// GitHub mints one GUID per delivery, so distinct calls are distinct
// deliveries; a test that means to send the SAME delivery twice says so with
// postWebhookDelivery.
func postWebhook(s *Server, event, sigHeader string, body []byte) *httptest.ResponseRecorder {
	return postWebhookDelivery(s, event, sigHeader, body, uuid.NewString())
}

// postWebhookDelivery is postWebhook with the delivery GUID named explicitly —
// the handle the dedup tests need to replay one delivery.
func postWebhookDelivery(s *Server, event, sigHeader string, body []byte, deliveryID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/webhooks/github/"+runmode.LocalDefaultOrgID, bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	if deliveryID != "" {
		req.Header.Set("X-GitHub-Delivery", deliveryID)
	}
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
	s.SetInstallationTokensInvalidHook(func(orgID, installationID string) {
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

// onlyInstallation returns the org's single live installation, failing on any
// other count.
func onlyInstallation(t *testing.T, s *Server) domain.OrgGitHubAppInstallation {
	t.Helper()
	got := installations(t, s)
	if len(got) != 1 {
		t.Fatalf("installations = %+v, want exactly one live row", got)
	}
	return got[0]
}

// TestGitHubWebhook_InstallationSuspend records the suspension on the mirrored
// row without removing it. The negative half matters as much as the positive:
// a suspension is not an uninstall, so the row stays live and the account
// fields are untouched — what changed is that GitHub will now refuse every
// token minted from this installation.
func TestGitHubWebhook_InstallationSuspend(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedWebhookApp(t, s)
	seedInstallation(t, s, 4242, "acme")

	body := []byte(`{"action":"suspend","installation":{"id":4242,"account":{"login":"acme","type":"Organization"},` +
		`"suspended_at":"2026-08-15T12:00:00Z","suspended_by":{"login":"octocat"}}}`)
	rec := postWebhook(s, "installation", sign(body), body)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	got := onlyInstallation(t, s)
	if !got.Suspended() {
		t.Error("Suspended() = false after a suspend delivery; want true")
	}
	if want := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC); !got.SuspendedAt.Equal(want) {
		t.Errorf("SuspendedAt = %s, want the payload's %s", got.SuspendedAt, want)
	}
	if got.SuspendedBy != "octocat" {
		t.Errorf("SuspendedBy = %q, want %q", got.SuspendedBy, "octocat")
	}
	if got.AccountLogin != "acme" {
		t.Errorf("AccountLogin = %q, want the untouched %q", got.AccountLogin, "acme")
	}
}

// TestGitHubWebhook_InstallationSuspend_InvalidatesCachedToken is the invariant
// the ticket exists for: an installation token minted before the suspension is
// dead the moment it lands, and the cache would otherwise keep serving it for
// up to an hour — presenting a suspension as generic 403s on a row that still
// reads as connected. Asserted through the real wiring (New's hook over the
// resolver's own cache), not a stand-in.
func TestGitHubWebhook_InstallationSuspend_InvalidatesCachedToken(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedWebhookApp(t, s)
	seedInstallation(t, s, 4242, "acme")

	// Prime the cache the way a resolution would: a token good for another
	// hour, which is exactly what makes the stale-token window an hour long.
	s.ghTokenCache.Set(runmode.LocalDefaultOrgID, "4242", githubapp.Token{
		Value:     "ghs_minted_before_the_suspension",
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if _, ok := s.ghTokenCache.Get(runmode.LocalDefaultOrgID, "4242"); !ok {
		t.Fatal("primed token not in the cache; the test asserts nothing")
	}

	body := []byte(`{"action":"suspend","installation":{"id":4242,"account":{"login":"acme","type":"Organization"},` +
		`"suspended_at":"2026-08-15T12:00:00Z","suspended_by":{"login":"octocat"}}}`)
	if rec := postWebhook(s, "installation", sign(body), body); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}

	if tok, ok := s.ghTokenCache.Get(runmode.LocalDefaultOrgID, "4242"); ok {
		t.Errorf("cache still serves %q after a suspend; want a miss", tok.Value)
	}
	// Only this installation's token dies with it — another account's
	// installation under the same App keeps minting.
	s.ghTokenCache.Set(runmode.LocalDefaultOrgID, "9999", githubapp.Token{Value: "ghs_other", ExpiresAt: time.Now().Add(time.Hour)})
	if rec := postWebhook(s, "installation", sign(body), body); rec.Code != http.StatusNoContent {
		t.Fatalf("second suspend status = %d, want 204", rec.Code)
	}
	if _, ok := s.ghTokenCache.Get(runmode.LocalDefaultOrgID, "9999"); !ok {
		t.Error("a suspend dropped another installation's cached token; want it untouched")
	}
}

// TestGitHubWebhook_InstallationSuspend_NoSuspendedAt falls back to the receipt
// time. A delivery that names no timestamp still describes an installation
// that is suspended right now, and a zero timestamp IS "not suspended" in the
// mirror — so recording nothing would silently drop the state.
func TestGitHubWebhook_InstallationSuspend_NoSuspendedAt(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedWebhookApp(t, s)
	seedInstallation(t, s, 4242, "acme")

	body := []byte(`{"action":"suspend","installation":{"id":4242,"account":{"login":"acme","type":"Organization"}}}`)
	if rec := postWebhook(s, "installation", sign(body), body); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if got := onlyInstallation(t, s); !got.Suspended() {
		t.Error("Suspended() = false for a suspend that named no timestamp; want true")
	}
}

// TestGitHubWebhook_InstallationUnsuspend restores exactly the prior state:
// both columns cleared, nothing else on the row disturbed.
func TestGitHubWebhook_InstallationUnsuspend(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedWebhookApp(t, s)
	seedInstallation(t, s, 4242, "acme")

	suspend := []byte(`{"action":"suspend","installation":{"id":4242,"account":{"login":"acme","type":"Organization"},` +
		`"suspended_at":"2026-08-15T12:00:00Z","suspended_by":{"login":"octocat"}}}`)
	if rec := postWebhook(s, "installation", sign(suspend), suspend); rec.Code != http.StatusNoContent {
		t.Fatalf("suspend status = %d, want 204", rec.Code)
	}

	unsuspend := []byte(`{"action":"unsuspend","installation":{"id":4242,"account":{"login":"acme","type":"Organization"}}}`)
	if rec := postWebhook(s, "installation", sign(unsuspend), unsuspend); rec.Code != http.StatusNoContent {
		t.Fatalf("unsuspend status = %d, want 204", rec.Code)
	}

	got := onlyInstallation(t, s)
	if got.Suspended() {
		t.Errorf("Suspended() = true after an unsuspend (SuspendedAt=%s); want false", got.SuspendedAt)
	}
	if got.SuspendedBy != "" {
		t.Errorf("SuspendedBy = %q after an unsuspend; want \"\" (no residue)", got.SuspendedBy)
	}
	if got.AccountLogin != "acme" {
		t.Errorf("AccountLogin = %q, want the untouched %q", got.AccountLogin, "acme")
	}
}

// TestGitHubWebhook_ReinstallAfterSuspendedDelete is the lifecycle arrow with
// the sharp edge: an installation is suspended, then deleted, then the account
// re-installs the App. The revived row must not inherit the suspension of the
// installation it replaced — otherwise a user who re-installed to fix things
// gets a workspace that reads as suspended and no gesture that clears it.
func TestGitHubWebhook_ReinstallAfterSuspendedDelete(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedWebhookApp(t, s)
	seedInstallation(t, s, 4242, "acme")

	for _, delivery := range []string{
		`{"action":"suspend","installation":{"id":4242,"account":{"login":"acme","type":"Organization"},` +
			`"suspended_at":"2026-08-15T12:00:00Z","suspended_by":{"login":"octocat"}}}`,
		`{"action":"deleted","installation":{"id":4242,"account":{"login":"acme","type":"Organization"}}}`,
	} {
		body := []byte(delivery)
		if rec := postWebhook(s, "installation", sign(body), body); rec.Code != http.StatusNoContent {
			t.Fatalf("delivery %s: status = %d, want 204", delivery[:32], rec.Code)
		}
	}
	if got := installations(t, s); len(got) != 0 {
		t.Fatalf("after the delete got %d live installations, want 0", len(got))
	}

	created := []byte(`{"action":"created","installation":{"id":4242,"account":{"login":"acme","type":"Organization"},"created_at":"2026-08-16T00:00:00Z"}}`)
	if rec := postWebhook(s, "installation", sign(created), created); rec.Code != http.StatusNoContent {
		t.Fatalf("created status = %d, want 204", rec.Code)
	}

	got := onlyInstallation(t, s)
	if got.Suspended() {
		t.Errorf("the re-installed installation reads as suspended (SuspendedAt=%s); want the suspension cleared with removed_at", got.SuspendedAt)
	}
	if got.SuspendedBy != "" {
		t.Errorf("SuspendedBy = %q after a re-install; want \"\"", got.SuspendedBy)
	}
}

// TestGitHubWebhook_RedeliveredInstallation_AppliesOnce is the defect the
// dedup table exists for. GitHub never auto-retries, but an operator
// redelivers — and a replayed installation.created used to re-run the upsert,
// reviving a row a later delete had removed.
//
// A byte-identical replay of an idempotent write is invisible on its own, so
// the sequence puts a state change between the two copies: created, deleted,
// then the created delivery again. If the replay applied, the row comes back.
func TestGitHubWebhook_RedeliveredInstallation_AppliesOnce(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedWebhookApp(t, s)

	created := []byte(`{"action":"created","installation":{"id":4242,"account":{"login":"acme","type":"Organization"},"created_at":"2026-01-01T00:00:00Z"}}`)
	deleted := []byte(`{"action":"deleted","installation":{"id":4242,"account":{"login":"acme","type":"Organization"}}}`)
	const createdDelivery = "gh-delivery-created"

	if rec := postWebhookDelivery(s, "installation", sign(created), created, createdDelivery); rec.Code != http.StatusNoContent {
		t.Fatalf("created status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if rec := postWebhook(s, "installation", sign(deleted), deleted); rec.Code != http.StatusNoContent {
		t.Fatalf("deleted status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if got := installations(t, s); len(got) != 0 {
		t.Fatalf("after the delete got %d live installations, want 0", len(got))
	}

	// The redelivery. 204, not an error: GitHub was already told the first
	// copy succeeded, and a failure here would provoke exactly the redelivery
	// the dedup absorbs.
	rec := postWebhookDelivery(s, "installation", sign(created), created, createdDelivery)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("redelivery status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("redelivery response has body %q, want empty", rec.Body.String())
	}
	if got := installations(t, s); len(got) != 0 {
		t.Errorf("the redelivered created revived %d installations, want 0 — the delivery had already been applied", len(got))
	}
}

// TestGitHubWebhook_DistinctDeliveriesSameInstallation_BothApply is the
// negative space, and the half a reader is most likely to get wrong: dedup is
// per delivery, never per subject. One installation produces many deliveries
// over its life, and every one of them has to take effect — a receiver that
// deduped on the installation id would apply the first and silently discard
// the rest, leaving the mirror permanently stale.
func TestGitHubWebhook_DistinctDeliveriesSameInstallation_BothApply(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedWebhookApp(t, s)

	first := []byte(`{"action":"created","installation":{"id":4242,"account":{"id":1234,"login":"acme","type":"Organization"},"created_at":"2026-01-01T00:00:00Z"}}`)
	// The same installation, seen again under the account's new handle — two
	// deliveries, two upserts, one row.
	second := []byte(`{"action":"created","installation":{"id":4242,"account":{"id":1234,"login":"acme-renamed","type":"Organization"},"created_at":"2026-01-01T00:00:00Z"}}`)

	for _, body := range [][]byte{first, second} {
		if rec := postWebhook(s, "installation", sign(body), body); rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
		}
	}

	got := onlyInstallation(t, s)
	if got.AccountLogin != "acme-renamed" {
		t.Errorf("AccountLogin = %q, want %q — the second delivery never reached the mirror", got.AccountLogin, "acme-renamed")
	}
}

// TestGitHubWebhook_RedeliveredEvent_PublishesOnce covers the other side
// effect a duplicate must not have. Ordering carries the assertion: the bus
// hands one subscriber its events in publish order, so a third, distinct
// delivery arriving as the second event proves the replay in between published
// nothing.
func TestGitHubWebhook_RedeliveredEvent_PublishesOnce(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedWebhookApp(t, s)

	bus := eventbus.New()
	s.SetEventBus(bus)

	got := make(chan domain.Event, 4)
	bus.Subscribe(eventbus.Subscriber{
		Name:   "test-webhook-dedup-capture",
		Filter: []string{"webhook:github:"},
		Handle: func(e domain.Event) { got <- e },
	})

	pr := []byte(`{"action":"synchronize","number":7,"installation":{"id":4242}}`)
	push := []byte(`{"ref":"refs/heads/main","installation":{"id":4242}}`)
	const prDelivery = "gh-delivery-pr"

	for _, delivery := range []struct {
		event string
		body  []byte
		id    string
	}{
		{"pull_request", pr, prDelivery},
		{"pull_request", pr, prDelivery}, // the redelivery
		{"push", push, "gh-delivery-push"},
	} {
		if rec := postWebhookDelivery(s, delivery.event, sign(delivery.body), delivery.body, delivery.id); rec.Code != http.StatusNoContent {
			t.Fatalf("%s (%s) status = %d, want 204", delivery.event, delivery.id, rec.Code)
		}
	}

	for _, want := range []string{"webhook:github:pull_request", "webhook:github:push"} {
		select {
		case e := <-got:
			if e.EventType != want {
				t.Fatalf("published event type = %q, want %q — the redelivery published its own copy", e.EventType, want)
			}
		case <-t.Context().Done():
			t.Fatal("timed out waiting for published webhook event")
		}
	}
}

// TestGitHubWebhook_BadSignatureDoesNotClaimDeliveryID is why the gate sits
// after signature verification rather than before it. If an unverified request
// could record a delivery id, anyone who could reach the route would be able to
// burn a GUID and have the genuine delivery dropped as a duplicate.
func TestGitHubWebhook_BadSignatureDoesNotClaimDeliveryID(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedWebhookApp(t, s)

	body := []byte(`{"action":"created","installation":{"id":4242,"account":{"login":"acme","type":"Organization"}}}`)
	const delivery = "gh-delivery-contested"

	if rec := postWebhookDelivery(s, "installation", "sha256=deadbeef", body, delivery); rec.Code != http.StatusUnauthorized {
		t.Fatalf("forged delivery status = %d, want 401", rec.Code)
	}
	if rec := postWebhookDelivery(s, "installation", sign(body), body, delivery); rec.Code != http.StatusNoContent {
		t.Fatalf("genuine delivery status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if got := installations(t, s); len(got) != 1 {
		t.Errorf("installations = %+v, want the genuine delivery applied — a forged copy claimed its delivery id", got)
	}
}

// TestGitHubWebhook_MalformedPayload_StaysBadRequestOnRedelivery pins the
// ordering between structural validation and the dedup gate. A delivery that
// is bad on its face must answer 4xx every time it is sent — if recording it
// came first, the operator's redelivery would be answered 204 as a
// "duplicate", which is the receiver reporting success for a payload it never
// applied and never could.
func TestGitHubWebhook_MalformedPayload_StaysBadRequestOnRedelivery(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedWebhookApp(t, s)

	for _, tc := range []struct {
		name     string
		delivery string
		body     []byte
	}{
		{"unparseable", "gh-delivery-garbage", []byte(`{"action":"created","installation":`)},
		{"no installation id", "gh-delivery-no-id", []byte(`{"action":"created","installation":{"account":{"login":"acme","type":"Organization"}}}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for attempt := 1; attempt <= 2; attempt++ {
				rec := postWebhookDelivery(s, "installation", sign(tc.body), tc.body, tc.delivery)
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("attempt %d: status = %d, want 400 — a structurally-bad delivery does not become a duplicate", attempt, rec.Code)
				}
			}
			if got := installations(t, s); len(got) != 0 {
				t.Errorf("a rejected delivery wrote %d installations, want 0", len(got))
			}
		})
	}
}

// TestGitHubWebhook_MissingDeliveryID refuses a verified delivery that carries
// no GUID. There is nothing to dedup it on, so applying it would quietly void
// the exactly-once promise for that delivery; 4xx keeps it from looking
// applied.
func TestGitHubWebhook_MissingDeliveryID(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedWebhookApp(t, s)

	body := []byte(`{"action":"created","installation":{"id":4242,"account":{"login":"acme","type":"Organization"}}}`)
	rec := postWebhookDelivery(s, "installation", sign(body), body, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := installations(t, s); len(got) != 0 {
		t.Errorf("a delivery with no GUID wrote %d installations, want 0", len(got))
	}
}

// TestGitHubWebhook_InstallationlessDeliveryDedups covers the payload shape
// that has no installation object at all (github_app_authorization, for one):
// it keys on the delivery id alone, and a replay of it is still a duplicate.
func TestGitHubWebhook_InstallationlessDeliveryDedups(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedWebhookApp(t, s)

	bus := eventbus.New()
	s.SetEventBus(bus)

	got := make(chan domain.Event, 4)
	bus.Subscribe(eventbus.Subscriber{
		Name:   "test-webhook-installationless-capture",
		Filter: []string{"webhook:github:"},
		Handle: func(e domain.Event) { got <- e },
	})

	revoked := []byte(`{"action":"revoked","sender":{"login":"acme"}}`)
	const delivery = "gh-delivery-authorization"
	for range 2 {
		if rec := postWebhookDelivery(s, "github_app_authorization", sign(revoked), revoked, delivery); rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
		}
	}

	// Same ordering sentinel as the publish test above: a later, distinct
	// delivery arriving second proves the replay published nothing.
	pr := []byte(`{"action":"synchronize","number":7}`)
	if rec := postWebhook(s, "pull_request", sign(pr), pr); rec.Code != http.StatusNoContent {
		t.Fatalf("sentinel status = %d, want 204", rec.Code)
	}
	for _, want := range []string{"webhook:github:github_app_authorization", "webhook:github:pull_request"} {
		select {
		case e := <-got:
			if e.EventType != want {
				t.Fatalf("published event type = %q, want %q — the redelivery published its own copy", e.EventType, want)
			}
		case <-t.Context().Done():
			t.Fatal("timed out waiting for published webhook event")
		}
	}
}

// TestGitHubWebhook_InstallationCreated_CapturesAccountID pins that the
// account's numeric id rides in from the delivery, since the webhook is one of
// only two writers of it (the reconcile is the other) and is the one that sees
// a brand-new installation first. The sibling test above covers the payload
// that names no id: it stays NULL and the row resolves by login as before.
func TestGitHubWebhook_InstallationCreated_CapturesAccountID(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedWebhookApp(t, s)

	body := []byte(`{"action":"created","installation":{"id":4242,"account":{"id":1234,"login":"acme","type":"Organization"},"created_at":"2026-01-01T00:00:00Z"}}`)
	rec := postWebhook(s, "installation", sign(body), body)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	got := installations(t, s)
	if len(got) != 1 {
		t.Fatalf("installations = %+v, want one row", got)
	}
	if got[0].AccountID != "1234" {
		t.Errorf("AccountID = %q, want %q", got[0].AccountID, "1234")
	}
	if got[0].AccountLogin != "acme" {
		t.Errorf("AccountLogin = %q, want %q", got[0].AccountLogin, "acme")
	}
}
