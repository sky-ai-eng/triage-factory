package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zalando/go-keyring"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The deployment App's receiver. Every test here runs in multi mode against
// the SQLite harness: the receiver reads the deployment secret off the Server
// and the binding off the installation mirror, neither of which is
// dialect-specific.

const deploymentWebhookSecret = "whsec-deployment-not-a-real-secret"

// seedDeploymentWebhook gives the server a deployment App with a webhook
// secret to verify against. Only the secret is set — the receiver reads
// nothing else off the App.
func seedDeploymentWebhook(t *testing.T, s *Server) {
	t.Helper()
	keyring.MockInit()
	s.deploymentApp = githubapp.DeploymentApp{WebhookSecret: deploymentWebhookSecret}
}

// bindInstallation records what the bind ceremony leaves behind: a live
// installation row for the org under the given host, and the managed class on
// the org.
func bindInstallation(t *testing.T, s *Server, installationID, host string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.orgs.SetGitHubCredentialClass(ctx, runmode.LocalDefaultOrgID, domain.GitHubCredentialClassManagedApp); err != nil {
		t.Fatalf("set credential class: %v", err)
	}
	if _, err := sqlitestore.New(s.db).GitHubApps.UpsertInstallation(ctx, domain.OrgGitHubAppInstallation{
		InstallationID: installationID,
		OrgID:          runmode.LocalDefaultOrgID,
		AccountType:    "Organization",
		AccountID:      "77",
		AccountLogin:   "acme",
		GitHubHost:     host,
	}); err != nil {
		t.Fatalf("bind installation: %v", err)
	}
}

func signDeployment(body []byte) string { return signWith(deploymentWebhookSecret, body) }

// postDeploymentWebhookDelivery fires a delivery at the static receiver.
func postDeploymentWebhookDelivery(s *Server, event, sigHeader string, body []byte, deliveryID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/webhooks/github", bytes.NewReader(body))
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

func postDeploymentWebhook(s *Server, event, sigHeader string, body []byte) *httptest.ResponseRecorder {
	return postDeploymentWebhookDelivery(s, event, sigHeader, body, uuid.NewString())
}

// captureWebhookBus subscribes a channel to every webhook:github: publish.
func captureWebhookBus(s *Server) <-chan domain.Event {
	bus := eventbus.New()
	s.SetEventBus(bus)
	got := make(chan domain.Event, 8)
	bus.Subscribe(eventbus.Subscriber{
		Name:   "test-deployment-webhook-capture",
		Filter: []string{"webhook:github:"},
		Handle: func(e domain.Event) { got <- e },
	})
	return got
}

// expectNoPublish fails if anything lands on the bus within a short window —
// the bus fan-out is asynchronous, so "nothing was published" needs a wait.
func expectNoPublish(t *testing.T, got <-chan domain.Event) {
	t.Helper()
	select {
	case e := <-got:
		t.Fatalf("published %s under org %q; want nothing published", e.EventType, e.OrgID)
	case <-time.After(50 * time.Millisecond):
	}
}

func expectPublish(t *testing.T, got <-chan domain.Event, wantType string) domain.Event {
	t.Helper()
	select {
	case e := <-got:
		if e.EventType != wantType {
			t.Fatalf("published event type = %q, want %q", e.EventType, wantType)
		}
		return e
	case <-t.Context().Done():
		t.Fatal("timed out waiting for published webhook event")
	}
	return domain.Event{}
}

func deliveryRows(t *testing.T, s *Server) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM github_webhook_deliveries`).Scan(&n); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	return n
}

// serveExpectingNoStore fires the request with every store the receiver could
// reach unwired. A nil interface panics on its first method call, so any read
// or write at all — on any method, not just the ones a spy remembered to
// count — surfaces as a recovered panic and fails the test. The four are the
// receiver's whole reach: the installation mirror, the dedup table, org
// settings, and the vault.
func serveExpectingNoStore(t *testing.T, s *Server, event, sigHeader string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	s.githubApps, s.githubDeliveries, s.orgs, s.secrets = nil, nil, nil, nil
	var rec *httptest.ResponseRecorder
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("the receiver touched a store before verification: %v", p)
			}
		}()
		rec = postDeploymentWebhook(s, event, sigHeader, body)
	}()
	return rec
}

const boundPRBody = `{"action":"synchronize","number":7,"installation":{"id":4242},"sender":{"login":"octocat","html_url":"https://github.com/octocat"}}`

// TestDeploymentWebhook_BoundInstallation_PublishesUnderOrg is the route
// working: a delivery GitHub signed, for an installation a workspace bound,
// lands on the bus under that workspace's id — an id the payload never
// carried. The same delivery to the per-org URL is still refused, because the
// two receivers coexist and a managed org's deliveries do not arrive there.
func TestDeploymentWebhook_BoundInstallation_PublishesUnderOrg(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	s := newTestServer(t)
	seedDeploymentWebhook(t, s)
	bindInstallation(t, s, "4242", "https://github.com")
	got := captureWebhookBus(s)

	body := []byte(boundPRBody)
	rec := postDeploymentWebhook(s, "pull_request", signDeployment(body), body)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	e := expectPublish(t, got, "webhook:github:pull_request")
	if e.OrgID != runmode.LocalDefaultOrgID {
		t.Errorf("published event org = %q, want the bound workspace %q", e.OrgID, runmode.LocalDefaultOrgID)
	}

	// Coexistence: the per-org URL verifies against the org's OWN secret, and a
	// managed org has none there, so GitHub's signature buys nothing.
	if rec := postWebhook(s, "pull_request", signDeployment(body), body); rec.Code != http.StatusUnauthorized {
		t.Errorf("per-org URL status = %d for a managed org, want 401", rec.Code)
	}
}

// TestDeploymentWebhook_BoundInstallation_AppliesLifecycle: the per-event
// handling is the existing one, under the resolved org — a suspend lands on
// the bound row.
func TestDeploymentWebhook_BoundInstallation_AppliesLifecycle(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	s := newTestServer(t)
	seedDeploymentWebhook(t, s)
	bindInstallation(t, s, "4242", "https://github.com")

	body := []byte(`{"action":"suspend","installation":{"id":4242,"account":{"login":"acme","type":"Organization"},"suspended_at":"2026-02-02T00:00:00Z","suspended_by":{"login":"owner"}},"sender":{"login":"owner","html_url":"https://github.com/owner"}}`)
	rec := postDeploymentWebhook(s, "installation", signDeployment(body), body)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	inst := onlyInstallation(t, s)
	if inst.SuspendedAt.IsZero() || inst.SuspendedBy != "owner" {
		t.Errorf("installation = %+v, want suspended by owner", inst)
	}
}

// TestDeploymentWebhook_UnboundInstallation_AcknowledgedWithoutSideEffect is
// the ordinary-state rule. An installation nobody has bound is not an attack
// and not an error: the delivery is 2xx, nothing is published, nothing is
// written — not an installation row (creation is the bind's job alone) and not
// a dedup row either, which is what lets the same delivery apply once the
// workspace binds the installation and the operator redelivers it.
func TestDeploymentWebhook_UnboundInstallation_AcknowledgedWithoutSideEffect(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	s := newTestServer(t)
	seedDeploymentWebhook(t, s)
	got := captureWebhookBus(s)

	created := []byte(`{"action":"created","installation":{"id":4242,"account":{"login":"acme","type":"Organization"},"created_at":"2026-01-01T00:00:00Z"},"sender":{"login":"octocat","html_url":"https://github.com/octocat"}}`)
	if rec := postDeploymentWebhook(s, "installation", signDeployment(created), created); rec.Code != http.StatusNoContent {
		t.Fatalf("unbound installation.created status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if rows := installations(t, s); len(rows) != 0 {
		t.Fatalf("an unbound installation.created wrote %d installation rows; the webhook must never create a binding", len(rows))
	}

	pr := []byte(boundPRBody)
	const delivery = "gh-delivery-before-bind"
	if rec := postDeploymentWebhookDelivery(s, "pull_request", signDeployment(pr), pr, delivery); rec.Code != http.StatusNoContent {
		t.Fatalf("unbound pull_request status = %d, want 204", rec.Code)
	}
	expectNoPublish(t, got)
	if n := deliveryRows(t, s); n != 0 {
		t.Fatalf("unbound deliveries recorded %d dedup rows, want 0 — acknowledging is not applying", n)
	}

	// The workspace binds, the operator redelivers: it applies now.
	bindInstallation(t, s, "4242", "https://github.com")
	if rec := postDeploymentWebhookDelivery(s, "pull_request", signDeployment(pr), pr, delivery); rec.Code != http.StatusNoContent {
		t.Fatalf("redelivery after bind status = %d, want 204", rec.Code)
	}
	if e := expectPublish(t, got, "webhook:github:pull_request"); e.OrgID != runmode.LocalDefaultOrgID {
		t.Errorf("published event org = %q, want %q", e.OrgID, runmode.LocalDefaultOrgID)
	}
}

// TestDeploymentWebhook_UnboundInstallation_OneOperatorLine is the trace an
// unbound delivery is allowed to leave: exactly one log line, at a level a
// self-hoster reads by default, naming the installation and the account it
// targets — and nothing the payload's author wrote. The operator is the only
// person placed to notice an install that landed nowhere (no tenant surface
// may list it), so the line has to be findable; and the payload is text from
// GitHub's side of the fence, so none of it rides along.
func TestDeploymentWebhook_UnboundInstallation_OneOperatorLine(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	s := newTestServer(t)
	seedDeploymentWebhook(t, s)

	created := []byte(`{"action":"created","installation":{"id":4242,"account":{"login":"acme","type":"Organization"},"created_at":"2026-01-01T00:00:00Z"},"sender":{"login":"octocat","html_url":"https://github.com/octocat"}}`)

	var logbuf bytes.Buffer
	restore := logging.SetOutput(&logbuf)
	rec := postDeploymentWebhookDelivery(s, "installation", signDeployment(created), created, "gh-delivery-unbound-1")
	restore()
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unbound installation.created status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}

	lines := strings.Split(strings.TrimSpace(logbuf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("an unbound delivery logged %d lines, want exactly 1:\n%s", len(lines), logbuf.String())
	}
	line := lines[0]
	for _, want := range []string{"level=INFO", "unbound installation", "installation=4242", "account=acme", "delivery=gh-delivery-unbound-1"} {
		if !strings.Contains(line, want) {
			t.Errorf("the operator line lacks %q: %s", want, line)
		}
	}
	for _, leak := range []string{"octocat", "html_url", `"action"`} {
		if strings.Contains(line, leak) {
			t.Errorf("the operator line carries payload content %q: %s", leak, line)
		}
	}
	if strings.Contains(line, "level=WARN") || strings.Contains(line, "level=ERROR") {
		t.Errorf("an ordinary state logged as a fault: %s", line)
	}

	// A delivery that is not an installation event names no account, and the
	// line says so by omission rather than by inventing one.
	logbuf.Reset()
	restore = logging.SetOutput(&logbuf)
	pr := []byte(boundPRBody)
	rec = postDeploymentWebhookDelivery(s, "pull_request", signDeployment(pr), pr, "gh-delivery-unbound-2")
	restore()
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unbound pull_request status = %d, want 204", rec.Code)
	}
	lines = strings.Split(strings.TrimSpace(logbuf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("an unbound pull_request delivery logged %d lines, want exactly 1:\n%s", len(lines), logbuf.String())
	}
	if !strings.Contains(lines[0], "installation=4242") || strings.Contains(lines[0], "account=") {
		t.Errorf("pull_request line = %s; want the installation named and no account claimed", lines[0])
	}
}

// TestDeploymentWebhook_BadSignature_TouchesNoStore is the property the
// inverted order buys: on this route a forgery costs a hash and nothing else.
// Asserted structurally — every store is unwired, so any read at all panics.
func TestDeploymentWebhook_BadSignature_TouchesNoStore(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	s := newTestServer(t)
	seedDeploymentWebhook(t, s)

	body := []byte(boundPRBody)
	rec := serveExpectingNoStore(t, s, "pull_request", "sha256=deadbeef", body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("401 response has body %q, want empty", rec.Body.String())
	}
}

// TestDeploymentWebhook_NoDeploymentSecret_Is401: a deployment without a
// deployment App has nothing to verify against and refuses. The delivery is
// signed under the EMPTY key — the one signature a receiver that fell into the
// HMAC check with no secret would accept — and no store is reachable, so the
// refusal happens before anything else.
func TestDeploymentWebhook_NoDeploymentSecret_Is401(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	s := newTestServer(t)
	s.deploymentApp = githubapp.DeploymentApp{}

	body := []byte(boundPRBody)
	rec := serveExpectingNoStore(t, s, "pull_request", signWith("", body), body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 — an empty key must never verify", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("401 response has body %q, want empty", rec.Body.String())
	}
}

func TestDeploymentWebhook_MissingSignature_Is401(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	s := newTestServer(t)
	seedDeploymentWebhook(t, s)

	rec := serveExpectingNoStore(t, s, "pull_request", "", []byte(boundPRBody))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestDeploymentWebhook_Redelivery_AppliesOnce: the existing dedup table,
// keyed (installation_id, delivery_id) with no org column, serves this route
// unchanged — a redelivery of an applied delivery is a 204 no-op.
func TestDeploymentWebhook_Redelivery_AppliesOnce(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	s := newTestServer(t)
	seedDeploymentWebhook(t, s)
	bindInstallation(t, s, "4242", "https://github.com")
	got := captureWebhookBus(s)

	body := []byte(boundPRBody)
	const delivery = "gh-delivery-static-pr"
	for i := 0; i < 2; i++ {
		if rec := postDeploymentWebhookDelivery(s, "pull_request", signDeployment(body), body, delivery); rec.Code != http.StatusNoContent {
			t.Fatalf("attempt %d status = %d, want 204", i+1, rec.Code)
		}
	}
	expectPublish(t, got, "webhook:github:pull_request")
	expectNoPublish(t, got)
	if n := deliveryRows(t, s); n != 1 {
		t.Errorf("dedup rows = %d, want 1", n)
	}
}

// TestDeploymentWebhook_HostDisambiguates: the binding key is (host,
// installation id), because two GitHub deployments can issue the same id, and
// the host half is the deployment's default GitHub — never anything the
// payload says. An installation bound under another host is unreachable from
// this route whatever the delivery's sender claims; the same id bound under
// the default applies.
func TestDeploymentWebhook_HostDisambiguates(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	s := newTestServer(t)
	seedDeploymentWebhook(t, s)
	bindInstallation(t, s, "4242", "https://ghe.example.com")
	got := captureWebhookBus(s)

	// Every spelling of the sender — the default host, the binding's own host,
	// none at all — is ignored: the row is under a host that is not the
	// deployment's, so nothing routes to it.
	for name, body := range map[string]string{
		"sender on github.com":     boundPRBody,
		"sender on the bound host": `{"action":"synchronize","number":7,"installation":{"id":4242},"sender":{"login":"octocat","html_url":"https://ghe.example.com/octocat"}}`,
		"no sender":                `{"action":"synchronize","number":7,"installation":{"id":4242}}`,
	} {
		b := []byte(body)
		if rec := postDeploymentWebhook(s, "pull_request", signDeployment(b), b); rec.Code != http.StatusNoContent {
			t.Fatalf("%s: status = %d, want 204", name, rec.Code)
		}
		expectNoPublish(t, got)
	}

	// Re-bound under the deployment default, the same delivery applies.
	bindInstallation(t, s, "4242", ghbase.DefaultBaseURL())
	body := []byte(boundPRBody)
	if rec := postDeploymentWebhook(s, "pull_request", signDeployment(body), body); rec.Code != http.StatusNoContent {
		t.Fatalf("default-host delivery status = %d, want 204", rec.Code)
	}
	if e := expectPublish(t, got, "webhook:github:pull_request"); e.OrgID != runmode.LocalDefaultOrgID {
		t.Errorf("published event org = %q, want %q", e.OrgID, runmode.LocalDefaultOrgID)
	}
}

// TestDeploymentWebhook_KeysOnTheDeploymentDefault is the GHES self-hoster's
// shape: with TF_DEFAULT_GITHUB_HOST naming their GitHub, an installation
// bound under it applies — including for a delivery whose sender URL says
// github.com, which proves the payload plays no part in the host — and the
// same id bound under github.com does not.
func TestDeploymentWebhook_KeysOnTheDeploymentDefault(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	ghbase.SetDefaultBaseURLForTest(t, "https://ghe.example.com")
	s := newTestServer(t)
	seedDeploymentWebhook(t, s)
	got := captureWebhookBus(s)
	body := []byte(boundPRBody) // sender.html_url is on github.com

	bindInstallation(t, s, "4242", "https://github.com")
	if rec := postDeploymentWebhook(s, "pull_request", signDeployment(body), body); rec.Code != http.StatusNoContent {
		t.Fatalf("github.com-bound delivery status = %d, want 204", rec.Code)
	}
	expectNoPublish(t, got)

	bindInstallation(t, s, "4242", "https://ghe.example.com")
	if rec := postDeploymentWebhook(s, "pull_request", signDeployment(body), body); rec.Code != http.StatusNoContent {
		t.Fatalf("default-bound delivery status = %d, want 204", rec.Code)
	}
	if e := expectPublish(t, got, "webhook:github:pull_request"); e.OrgID != runmode.LocalDefaultOrgID {
		t.Errorf("published event org = %q, want %q", e.OrgID, runmode.LocalDefaultOrgID)
	}
}

// TestDeploymentWebhook_MalformedInstallation_StaysBadRequest pins the gate's
// position on this route too: structural validation runs before any store is
// read, so a malformed installation delivery is 400 on every attempt and
// records nothing.
func TestDeploymentWebhook_MalformedInstallation_StaysBadRequest(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	s := newTestServer(t)
	seedDeploymentWebhook(t, s)
	bindInstallation(t, s, "4242", "https://github.com")

	body := []byte(`{"action":"created","installation":{"account":{"login":"acme"}}}`)
	const delivery = "gh-delivery-static-malformed"
	for i := 0; i < 2; i++ {
		if rec := postDeploymentWebhookDelivery(s, "installation", signDeployment(body), body, delivery); rec.Code != http.StatusBadRequest {
			t.Fatalf("attempt %d status = %d, want 400", i+1, rec.Code)
		}
	}
	if n := deliveryRows(t, s); n != 0 {
		t.Errorf("a malformed delivery recorded %d dedup rows, want 0", n)
	}
}

// TestDeploymentWebhook_LocalMode_NotFound: the deployment App is a multi-mode
// credential, so in local mode the route does not exist.
func TestDeploymentWebhook_LocalMode_NotFound(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedDeploymentWebhook(t, s)

	body := []byte(boundPRBody)
	rec := serveExpectingNoStore(t, s, "pull_request", signDeployment(body), body)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
