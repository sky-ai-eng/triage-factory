package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventbus"
	"github.com/sky-ai-eng/triage-factory/internal/github/githubapp"
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
	return signWith(testWebhookSecret, body)
}

// signWith is sign for a delivery keyed by some other secret.
func signWith(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// postWebhook fires a delivery at the receiver under a fresh delivery GUID, so
// distinct calls are distinct deliveries; postWebhookDelivery replays one.
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

// TestGitHubWebhook_InstallationCreated pins what an installation.created
// delivery mirrors: the account's id and the grant's width when the payload
// names them (unknown when it does not), on the org's GitHub host.
func TestGitHubWebhook_InstallationCreated(t *testing.T) {
	for _, tc := range []struct {
		name          string
		base          string // the org's GitHub URL; "" leaves the test server's
		account       string
		selection     string
		wantAccountID string
		wantHost      string
	}{
		{name: "minimal payload", account: `{"login":"acme","type":"Organization"}`, wantHost: "https://github.com"},
		{name: "account id", account: `{"id":1234,"login":"acme","type":"Organization"}`, wantAccountID: "1234", wantHost: "https://github.com"},
		{name: "selected grant", account: `{"login":"acme","type":"Organization"}`, selection: domain.RepositorySelectionSelected, wantHost: "https://github.com"},
		{name: "GHES org", base: "https://git.example.com/github/", account: `{"login":"acme","type":"Organization"}`, wantHost: "https://git.example.com/github"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runmode.SetForTest(t, runmode.ModeLocal)
			s := newTestServer(t)
			seedWebhookApp(t, s)
			if tc.base != "" {
				setOrgGitHubBase(t, s, tc.base)
			}

			inst := `"id":4242,"account":` + tc.account + `,"created_at":"2026-01-01T00:00:00Z"`
			if tc.selection != "" {
				inst += `,"repository_selection":"` + tc.selection + `"`
			}
			body := []byte(`{"action":"created","installation":{` + inst + `}}`)
			if rec := postWebhook(s, "installation", sign(body), body); rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
			}

			got := onlyInstallation(t, s)
			if got.InstallationID != "4242" || got.AccountLogin != "acme" || got.AccountID != tc.wantAccountID {
				t.Errorf("installation = %+v, want acme/4242 with account id %q", got, tc.wantAccountID)
			}
			if got.RepositorySelection != tc.selection {
				t.Errorf("RepositorySelection = %q, want %q", got.RepositorySelection, tc.selection)
			}
			if tc.selection == domain.RepositorySelectionSelected && got.GrantsEveryRepository() {
				t.Error("GrantsEveryRepository() = true for a selective grant")
			}
			if got.GitHubHost != tc.wantHost {
				t.Errorf("GitHubHost = %q, want %q", got.GitHubHost, tc.wantHost)
			}
		})
	}
}

// TestGitHubWebhook_UnverifiedDeliveryIs401 pins the per-org receiver's refusal:
// a delivery it cannot verify against the org's own App secret is a bare 401
// that writes nothing.
func TestGitHubWebhook_UnverifiedDeliveryIs401(t *testing.T) {
	body := []byte(`{"action":"created","installation":{"id":4242,"account":{"login":"acme","type":"Organization"}}}`)
	for _, tc := range []struct {
		name    string
		seedApp bool                         // register the org's own App and its secret
		class   domain.GitHubCredentialClass // then move the org to this class; "" leaves it
		sig     string
	}{
		{"bad signature", true, "", "sha256=deadbeef"},
		{"missing signature", true, "", ""},
		// A PAT org has no App deliveries to verify, whatever rows exist.
		{"PAT org with an App row", true, domain.GitHubCredentialClassPAT, sign(body)},
		// The deployment App signs every tenant's deliveries with one secret; if
		// this route verified against it, any org id in the path would pass.
		{"managed org, deployment-signed", false, domain.GitHubCredentialClassManagedApp, signWith("whsec_the_deployment_app", body)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runmode.SetForTest(t, runmode.ModeLocal)
			keyring.MockInit()
			s := newTestServer(t)
			if tc.seedApp {
				seedWebhookApp(t, s)
			}
			if tc.class != "" {
				if _, err := s.orgs.SetGitHubCredentialClass(context.Background(), runmode.LocalDefaultOrgID, tc.class); err != nil {
					t.Fatalf("set credential class: %v", err)
				}
			}

			rec := postWebhook(s, "installation", tc.sig, body)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
			}
			if rec.Body.Len() != 0 {
				t.Errorf("401 response has body %q, want empty", rec.Body.String())
			}
			if got := installations(t, s); len(got) != 0 {
				t.Errorf("a refused delivery wrote %d installations, want 0", len(got))
			}
		})
	}
}

func TestGitHubWebhook_InstallationDeleted_FiresHook(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedWebhookApp(t, s)

	// Pre-seed an active installation to remove.
	stores := sqlitestore.New(s.db)
	if _, err := stores.GitHubApps.UpsertInstallation(context.Background(), domain.OrgGitHubAppInstallation{
		InstallationID: "4242",
		OrgID:          runmode.LocalDefaultOrgID,
		AccountType:    "Organization",
		AccountLogin:   "acme",
		GitHubHost:     seedRepoHost(t, s),
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

// TestGitHubWebhook_NoApp_IndistinguishableFromBadSignature: the receiver is
// pre-auth, so an org with no App must get the bad signature's answer byte for
// byte (status, body, headers), or callers could probe which orgs have an App.
func TestGitHubWebhook_NoApp_IndistinguishableFromBadSignature(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)

	body := []byte(`{"action":"created","installation":{"id":1,"account":{"login":"x","type":"User"}}}`)

	noApp := newTestServer(t) // no app seeded — the org has nothing to verify against
	noAppRec := postWebhook(noApp, "installation", sign(body), body)

	badSig := newTestServer(t)
	seedWebhookApp(t, badSig)
	badSigRec := postWebhook(badSig, "installation", "sha256=deadbeef", body)

	if noAppRec.Code != http.StatusUnauthorized {
		t.Errorf("no-App status = %d, want 401 (same as a bad signature)", noAppRec.Code)
	}
	if badSigRec.Code != http.StatusUnauthorized {
		t.Errorf("bad-signature status = %d, want 401", badSigRec.Code)
	}
	if noAppRec.Code != badSigRec.Code {
		t.Errorf("statuses differ: no-App %d vs bad-signature %d", noAppRec.Code, badSigRec.Code)
	}
	if noAppRec.Body.String() != badSigRec.Body.String() {
		t.Errorf("bodies differ: no-App %q vs bad-signature %q", noAppRec.Body.String(), badSigRec.Body.String())
	}
	if noAppRec.Body.Len() != 0 {
		t.Errorf("no-App 401 has body %q, want empty", noAppRec.Body.String())
	}
	if got, want := noAppRec.Header(), badSigRec.Header(); !reflect.DeepEqual(got, want) {
		t.Errorf("headers differ: no-App %v vs bad-signature %v", got, want)
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

// TestGitHubWebhook_InstallationSuspend_InvalidatesCachedToken: a token minted
// before a suspension is dead once it lands, so the suspend drops it from the
// real resolver cache (on every host) instead of serving it for up to an hour.
func TestGitHubWebhook_InstallationSuspend_InvalidatesCachedToken(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedWebhookApp(t, s)
	seedInstallation(t, s, 4242, "acme")

	// Prime the cache the way a resolution would: a token good for another
	// hour, which is exactly what makes the stale-token window an hour long.
	// It is primed on two hosts, because the delivery names no host and the
	// installation's tokens die on whichever one minted them.
	const otherHost = "https://ghe.example.com"
	for _, host := range []string{dbtest.TestGitHubHost, otherHost} {
		s.ghTokenCache.Set(runmode.LocalDefaultOrgID, host, "4242", githubapp.Token{
			Value:     "ghs_minted_before_the_suspension",
			ExpiresAt: time.Now().Add(time.Hour),
		})
		if _, ok := s.ghTokenCache.Get(runmode.LocalDefaultOrgID, host, "4242"); !ok {
			t.Fatal("primed token not in the cache; the test asserts nothing")
		}
	}

	body := []byte(`{"action":"suspend","installation":{"id":4242,"account":{"login":"acme","type":"Organization"},` +
		`"suspended_at":"2026-08-15T12:00:00Z","suspended_by":{"login":"octocat"}}}`)
	if rec := postWebhook(s, "installation", sign(body), body); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}

	for _, host := range []string{dbtest.TestGitHubHost, otherHost} {
		if tok, ok := s.ghTokenCache.Get(runmode.LocalDefaultOrgID, host, "4242"); ok {
			t.Errorf("cache still serves %q on %s after a suspend; want a miss", tok.Value, host)
		}
	}
	// Only this installation's token dies with it — another account's
	// installation under the same App keeps minting.
	s.ghTokenCache.Set(runmode.LocalDefaultOrgID, dbtest.TestGitHubHost, "9999", githubapp.Token{Value: "ghs_other", ExpiresAt: time.Now().Add(time.Hour)})
	if rec := postWebhook(s, "installation", sign(body), body); rec.Code != http.StatusNoContent {
		t.Fatalf("second suspend status = %d, want 204", rec.Code)
	}
	if _, ok := s.ghTokenCache.Get(runmode.LocalDefaultOrgID, dbtest.TestGitHubHost, "9999"); !ok {
		t.Error("a suspend dropped another installation's cached token; want it untouched")
	}
}

// TestGitHubWebhook_InstallationSuspend_NoSuspendedAt falls back to the receipt
// time, since a zero timestamp reads as "not suspended" in the mirror.
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

// TestGitHubWebhook_InstallationUnsuspend: a suspend records the payload's
// time and actor on the live row, and an unsuspend clears both, each leaving
// the rest of the row untouched.
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
	suspended := onlyInstallation(t, s)
	if want := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC); !suspended.Suspended() || !suspended.SuspendedAt.Equal(want) {
		t.Errorf("SuspendedAt = %s after a suspend, want the payload's %s", suspended.SuspendedAt, want)
	}
	if suspended.SuspendedBy != "octocat" || suspended.AccountLogin != "acme" {
		t.Errorf("after a suspend SuspendedBy=%q AccountLogin=%q, want octocat and the untouched acme", suspended.SuspendedBy, suspended.AccountLogin)
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

// TestGitHubWebhook_ReinstallAfterSuspendedDelete: suspended, deleted, then
// re-installed, the revived row must not inherit the old suspension, which no
// later gesture would clear.
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

// TestGitHubWebhook_RedeliveredInstallation_AppliesOnce: an operator's
// redelivery of an applied installation.created must not re-run the upsert.
// A delete between the two copies makes a replay that applied visible.
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

// TestGitHubWebhook_RedeliveredEvent_PublishesOnce: a replayed delivery is not
// published again. The bus delivers in publish order, so a distinct delivery
// arriving second proves the replay before it published nothing.
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
			if e.OrgID != runmode.LocalDefaultOrgID {
				t.Errorf("published event org = %q, want %q", e.OrgID, runmode.LocalDefaultOrgID)
			}
		case <-t.Context().Done():
			t.Fatal("timed out waiting for published webhook event")
		}
	}
}

// TestGitHubWebhook_BadSignatureDoesNotClaimDeliveryID: the dedup gate sits
// after verification, so a forged request cannot burn a delivery id and have
// the genuine delivery dropped as a duplicate.
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

// TestGitHubWebhook_MalformedPayload_StaysBadRequestOnRedelivery: structural
// validation runs before the dedup gate, so a redelivered bad payload is 400
// again rather than a 204 "duplicate" of something never applied.
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

// TestGitHubWebhook_MissingDeliveryID refuses a verified delivery with no GUID,
// since there is nothing to dedup it on.
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

// --- rate limiting -------------------------------------------------------

// retuneSignedWebhookLimiter re-tunes the live signed-webhook limiter and
// freezes its clock. It edits the limiter in place because the mounted route
// holds a pointer to it; a replacement assigned after New would go unread.
func retuneSignedWebhookLimiter(t *testing.T, s *Server, rate, burst float64, clk *time.Time) {
	t.Helper()
	s.signedWebhookLimiter.rate = rate
	s.signedWebhookLimiter.burst = burst
	s.signedWebhookLimiter.now = func() time.Time { return *clk }
}

// TestGitHubWebhook_RateLimited: the receiver is mounted through the
// signed-webhook limiter, so a flood is refused with 429 before the handler,
// while deliveries inside the budget land and the budget refills.
func TestGitHubWebhook_RateLimited(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	s := newTestServer(t)
	seedWebhookApp(t, s)

	clk := time.Now()
	retuneSignedWebhookLimiter(t, s, signedWebhookRatePerSec, 3, &clk)

	// One account per installation: the mirror holds at most one live row per
	// (org, account_login) among rows with no account id, which is every row
	// these deliveries write, so reusing a login across ids would fail the
	// upsert for reasons that have nothing to do with rate limiting.
	delivery := func(id int) []byte {
		return []byte(`{"action":"created","installation":{"id":` + strconv.Itoa(id) +
			`,"account":{"login":"acme-` + strconv.Itoa(id) + `","type":"Organization"}}}`)
	}
	post := func(id int) *httptest.ResponseRecorder {
		body := delivery(id)
		return postWebhook(s, "installation", sign(body), body)
	}

	// The burst: legitimate, correctly-signed deliveries pass and are applied.
	for i := 1; i <= 3; i++ {
		if rec := post(i); rec.Code != http.StatusNoContent {
			t.Fatalf("delivery %d = %d, want 204; body=%s", i, rec.Code, rec.Body.String())
		}
	}

	// One past the budget: refused, and refused BEFORE the handler — the
	// installation this delivery describes was never written.
	rec := post(99)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-budget delivery = %d, want 429", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Error("429 carries no Retry-After header")
	}
	for _, inst := range installations(t, s) {
		if inst.InstallationID == "99" {
			t.Error("a throttled delivery reached the handler and wrote installation 99")
		}
	}

	// The budget refills, so a throttled sender recovers rather than being
	// locked out.
	clk = clk.Add(time.Second)
	if rec := post(99); rec.Code != http.StatusNoContent {
		t.Fatalf("delivery after refill = %d, want 204", rec.Code)
	}
}

// TestGitHubWebhook_NotRateLimitedInLocalMode: a local install has one org and
// nothing to throttle, so three times the production burst is all served.
func TestGitHubWebhook_NotRateLimitedInLocalMode(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedWebhookApp(t, s)

	body := []byte(`{"action":"synchronize","number":7}`)
	for i := 0; i < int(signedWebhookBurst)*3; i++ {
		rec := postWebhook(s, "pull_request", sign(body), body)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("local-mode delivery %d = %d, want 204 (no limiting at N=1)", i+1, rec.Code)
		}
	}
}

// --- the webhook-secret cache --------------------------------------------

// countingSecrets counts GetSystem reads against the wrapped store — the vault
// traffic the receiver's secret cache exists to keep off an unauthenticated
// flood.
type countingSecrets struct {
	db.SecretStore
	mu sync.Mutex
	n  int
}

func (c *countingSecrets) GetSystem(ctx context.Context, orgID, key string) (string, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return c.SecretStore.GetSystem(ctx, orgID, key)
}

func (c *countingSecrets) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// TestGitHubWebhook_SecretCached_UnderFloodOfForgedDeliveries: the pre-auth
// route reads the org's secret before it can check a signature, so the secret
// is cached and a flood of forgeries costs one vault read.
func TestGitHubWebhook_SecretCached_UnderFloodOfForgedDeliveries(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedWebhookApp(t, s)

	counter := &countingSecrets{SecretStore: s.secrets}
	s.secrets = counter

	body := []byte(`{"action":"created","installation":{"id":4242,"account":{"login":"acme","type":"Organization"}}}`)
	for i := 0; i < 50; i++ {
		if rec := postWebhook(s, "installation", "sha256=deadbeef", body); rec.Code != http.StatusUnauthorized {
			t.Fatalf("forged delivery %d = %d, want 401", i+1, rec.Code)
		}
	}
	if got := counter.count(); got != 1 {
		t.Errorf("50 forged deliveries cost %d vault reads, want 1 (the cache miss that filled the entry)", got)
	}
}

// TestGitHubWebhook_NoAppSecretCached_UnderFlood: "this org has no App" is
// cached too, since the caller picks the org id and a flood at an org with no
// App is the cheapest one to mount.
func TestGitHubWebhook_NoAppSecretCached_UnderFlood(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	// No app seeded.

	body := []byte(`{"action":"created","installation":{"id":1,"account":{"login":"x","type":"User"}}}`)
	for i := 0; i < 10; i++ {
		if rec := postWebhook(s, "installation", sign(body), body); rec.Code != http.StatusUnauthorized {
			t.Fatalf("delivery %d to an org with no App = %d, want 401", i+1, rec.Code)
		}
	}
	if _, _, ok := s.webhookSecretCacheGet(runmode.LocalDefaultOrgID); !ok {
		t.Error("no cached entry after a flood at an org with no App; every request re-read the stores")
	}
}

// TestGitHubWebhook_TeardownInvalidatesCachedSecret: once the App is discarded,
// its secret stops verifying deliveries immediately, not at the end of the TTL.
func TestGitHubWebhook_TeardownInvalidatesCachedSecret(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedLocalApp(t, s, false) // staged, so the discard route accepts it

	body := []byte(`{"action":"synchronize","number":7}`)
	sig := signWith("webhook-secret", body) // the secret seedLocalApp stores
	if rec := postWebhook(s, "pull_request", sig, body); rec.Code != http.StatusNoContent {
		t.Fatalf("delivery before teardown = %d, want 204 (the cache is primed by this)", rec.Code)
	}

	rec := doJSON(t, s, http.MethodDelete, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("discard = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	if rec := postWebhook(s, "pull_request", sig, body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("delivery after teardown = %d, want 401 — the destroyed secret is still verifying deliveries", rec.Code)
	}
}

// TestGitHubWebhook_RegistrationInvalidatesCachedSecret: a delivery before
// registration caches "no App", and registering must drop that entry so the
// first deliveries after it verify.
func TestGitHubWebhook_RegistrationInvalidatesCachedSecret(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	// GitHub's manifest-conversion endpoint, handing back the webhook secret
	// the registration will store.
	const registeredSecret = "wh-from-github"
	convStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":4242,"slug":"tf","client_id":"Iv1.x","client_secret":"cs",` +
			`"pem":"-----BEGIN RSA PRIVATE KEY-----\ntest\n-----END RSA PRIVATE KEY-----",` +
			`"webhook_secret":"` + registeredSecret + `"}`))
	}))
	t.Cleanup(convStub.Close)

	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	s.SetDeployConfig("http://localhost:3000", key)
	setOrgGitHubBase(t, s, convStub.URL)

	// A delivery lands while the org still has no App — this is what fills the
	// cache with the negative.
	body := []byte(`{"action":"synchronize","number":7}`)
	if rec := postWebhook(s, "pull_request", signWith(registeredSecret, body), body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("pre-registration delivery = %d, want 401", rec.Code)
	}

	state := appRegisterState{
		OrgID: runmode.LocalDefaultOrgID, OwnerType: "user",
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
	}
	signed, err := state.sign(key)
	if err != nil {
		t.Fatalf("sign state: %v", err)
	}
	if rec := doJSON(t, s, http.MethodGet,
		"/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/register/callback?code=c&state="+signed, nil); rec.Code != http.StatusFound {
		t.Fatalf("register callback = %d, want 302; body=%s", rec.Code, rec.Body.String())
	}

	if rec := postWebhook(s, "pull_request", signWith(registeredSecret, body), body); rec.Code != http.StatusNoContent {
		t.Fatalf("first delivery after registration = %d, want 204 — the pre-registration negative is still cached", rec.Code)
	}
}
