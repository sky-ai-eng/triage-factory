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

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/db"
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
	return signWith(testWebhookSecret, body)
}

// signWith is sign for a delivery keyed by some other secret — what a rotation
// test needs, since the whole question there is which of two secrets the
// receiver is currently verifying against.
func signWith(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
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

// TestGitHubWebhook_NoApp_IndistinguishableFromBadSignature closes the
// org-existence oracle. The receiver is pre-auth, so anything its reply reveals
// is revealed to anyone: a 404 for "this org has no App" against a 401 for
// "your signature is wrong" lets an unauthenticated caller walk a list of org
// ids and learn which workspaces have a GitHub App registered. Both refusals
// have to be the same answer, byte for byte — status, body, and the headers
// that would otherwise differ (a 404 carries a Content-Type the bare 401 does
// not).
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

// TestGitHubWebhook_PATOrg_IndistinguishableFromBadSignature is the same
// invariant for the org that HAS a registered App row but is not in the App
// credential system. It gets the same 401 — the receiver's reply never
// distinguishes which credential system an org is in either.
func TestGitHubWebhook_PATOrg_IndistinguishableFromBadSignature(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedWebhookApp(t, s)
	// Flip the class out from under the registration: a PAT org has no App and
	// so no deliveries to verify, whatever rows exist.
	if err := s.orgs.SetGitHubCredentialClass(context.Background(), runmode.LocalDefaultOrgID, domain.GitHubCredentialClassPAT); err != nil {
		t.Fatalf("set credential class: %v", err)
	}

	body := []byte(`{"action":"created","installation":{"id":1,"account":{"login":"x","type":"User"}}}`)
	rec := postWebhook(s, "installation", sign(body), body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (indistinguishable from a bad signature)", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("401 response has body %q, want empty", rec.Body.String())
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

// --- rate limiting -------------------------------------------------------

// retuneSignedWebhookLimiter re-tunes the server's live signed-webhook limiter
// in place and freezes its clock, so a test can reach the burst boundary in a
// few requests instead of a hundred.
//
// In place, not swapped: routes() passed the limiter POINTER to the wrapper
// when the route was mounted, so assigning a new limiter to the field after New
// would leave the mounted route consulting the old one and the test asserting
// nothing.
func retuneSignedWebhookLimiter(t *testing.T, s *Server, rate, burst float64, clk *time.Time) {
	t.Helper()
	s.signedWebhookLimiter.rate = rate
	s.signedWebhookLimiter.burst = burst
	s.signedWebhookLimiter.now = func() time.Time { return *clk }
}

// TestGitHubWebhook_RateLimited is the ticket's headline: the receiver is
// mounted THROUGH the signed-webhook limiter, so a flood against a valid org
// URL is throttled. The positive half matters as much as the 429 — a delivery
// inside the budget still lands, and the budget refills — because a limiter
// that throttled real deliveries would take webhook ingest down with it.
func TestGitHubWebhook_RateLimited(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	s := newTestServer(t)
	seedWebhookApp(t, s)

	clk := time.Now()
	retuneSignedWebhookLimiter(t, s, signedWebhookRatePerSec, 3, &clk)

	// One account per installation: the mirror holds at most one live row per
	// (org, account_login), so reusing a login across ids would fail the
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

// TestGitHubWebhook_NotRateLimitedInLocalMode asserts the limiter is inert in
// local mode, directly and on this route rather than by reading the wrapper. A
// local install is N=1 — one user, one workspace, their own laptop — so there
// is nothing to throttle and a cap could only cost them deliveries. Driven with
// the PRODUCTION burst untouched: 3x more requests than the multi-mode budget
// would allow, all of them served.
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

// TestGitHubWebhook_SecretCached_UnderFloodOfForgedDeliveries is why the cache
// is in this ticket rather than a later one. Every request to this pre-auth
// route resolves the org's webhook secret BEFORE it can check a signature, so
// without a cache a flood of garbage aimed at one org URL turns into a vault
// read per request — the rate limiter caps the rate, and this caps the cost of
// each one that gets through.
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

// TestGitHubWebhook_NoAppSecretCached_UnderFlood is the same property for the
// org that has nothing to verify against. The negative is worth caching for
// exactly the reason the positive is: an unauthenticated caller picks the org
// id in the URL, so the cheapest flood to mount is one aimed at an org with no
// App at all, and that must not cost a settings read plus a registration read
// per request either.
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
	if _, ok := s.webhookSecretCacheGet(runmode.LocalDefaultOrgID); !ok {
		t.Error("no cached entry after a flood at an org with no App; every request re-read the stores")
	}
}

// TestGitHubWebhook_TeardownInvalidatesCachedSecret is one half of "the cache
// never serves a stale secret past a rotation": the App is torn down through
// the real discard route, and the secret that verified deliveries a moment ago
// stops verifying them immediately — not at the end of the TTL.
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

// TestGitHubWebhook_RegistrationInvalidatesCachedSecret is the other half, and
// the one a cache-only-positives design would fail: a delivery that arrives
// before the App is registered caches "this org verifies nothing", and GitHub
// starts delivering the moment registration completes. Without invalidation on
// the write path, every one of those first deliveries is rejected until the
// entry expires.
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
