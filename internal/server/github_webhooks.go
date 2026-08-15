package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// maxWebhookBody caps the request body we read before signature
// verification. GitHub documents 25 MiB as the delivery ceiling; a body
// at the cap still hashes fine, and anything larger is not a legitimate
// delivery, so we read up to the cap + 1 and reject an over-cap read.
const maxWebhookBody = 25 << 20

// handleGitHubWebhook receives per-org GitHub App webhooks at
// POST /api/webhooks/github/{org_id}. It is pre-auth (GitHub has no
// session) and identifies the tenant solely from the URL path — safe
// because the HMAC signature is verified against that org's stored
// webhook secret before any side effect, so a delivery forged for
// another org simply fails verification.
//
// installation.created → UpsertInstallation; installation.deleted →
// MarkInstallationRemoved; installation.suspend / .unsuspend →
// SetInstallationSuspension. deleted and suspend both fire the resolver's
// token-cache invalidate hook, since either leaves the installation's
// already-minted tokens dead. Every other (verified) event is published to
// the bus for downstream content processing and acked with 204. Validation
// failures return 4xx so GitHub doesn't retry a structurally-bad delivery
// indefinitely.
//
// A verified delivery is deduped on its X-GitHub-Delivery GUID before any of
// that runs — see the gate below.
func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("org_id")
	if _, err := uuid.Parse(orgID); err != nil {
		http.NotFound(w, r)
		return
	}

	eventName := r.Header.Get("X-GitHub-Event")
	if eventName == "" {
		badRequest(w, "missing X-GitHub-Event header")
		return
	}
	sigHeader := r.Header.Get("X-Hub-Signature-256")
	if sigHeader == "" {
		// No signature to check against — treat as unauthenticated.
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody+1))
	if err != nil {
		badRequest(w, "could not read request body")
		return
	}
	if len(body) > maxWebhookBody {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}

	// Which secret verifies this delivery follows from the org's credential
	// class. A per-org registration's own webhook secret is the BYO-App answer;
	// a PAT org has no App and therefore no deliveries to verify, so the route
	// does not exist for it. Reading a missing registration row as "PAT" would
	// be an inference that holds only while PAT is the single rowless shape —
	// an org on a deployment-level shared App has no row either, and its
	// deliveries verify against a deployment secret this arm never reaches.
	//
	// System (claims-free) read throughout: the handler is pre-auth and has
	// only a trusted org_id from the URL. An unknown or unreadable class is
	// refused before any secret lookup.
	class, err := s.githubCredentialClass(r.Context(), orgID)
	if err != nil {
		if errors.Is(err, ErrUnknownGitHubCredentialClass) {
			// Nothing to verify against — same answer as a PAT org, and
			// deliberately indistinguishable from one on the wire.
			githubAppLog.Warn("webhook delivery for org with unknown credential class", "org", orgID)
			http.NotFound(w, r)
			return
		}
		internalError(w, "github-webhook", err)
		return
	}
	if class != domain.GitHubCredentialClassBYOApp {
		// A PAT org has no App and so no webhook to receive. Not an error —
		// this endpoint simply doesn't exist for it.
		http.NotFound(w, r)
		return
	}

	app, err := s.githubApps.GetForOrgSystem(r.Context(), orgID)
	if err != nil {
		internalError(w, "github-webhook", err)
		return
	}
	if app == nil || app.WebhookSecretRef == "" {
		// No registration row (or a hookless App) behind the class — there's
		// nothing to verify against, so this endpoint doesn't exist for it.
		http.NotFound(w, r)
		return
	}
	secret, err := s.secrets.GetSystem(r.Context(), orgID, app.WebhookSecretRef)
	if err != nil {
		internalError(w, "github-webhook", err)
		return
	}
	if secret == "" || !validWebhookSignature(secret, body, sigHeader) {
		// Deliberately no body and no payload logging on a mismatch.
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	// Signature verified past this point.

	// Dedup gates ahead of every side effect — the mirror write and the bus
	// publish alike. GitHub never auto-retries a failed delivery, but it
	// redelivers on demand (the Redeliver button; POST
	// /app/hook/deliveries/{delivery_id}/attempts), which is the recovery an
	// operator reaches for after an outage — so a second copy of a delivery is
	// ordinary traffic, and without this it re-runs the upsert and re-publishes.
	deliveryID := r.Header.Get("X-GitHub-Delivery")
	if deliveryID == "" {
		// Every genuine delivery carries a GUID. Without one there is nothing
		// to dedup on, so applying it would quietly void the exactly-once
		// promise for that delivery — refused as structurally bad, like a
		// delivery naming no event.
		badRequest(w, "missing X-GitHub-Delivery header")
		return
	}
	fresh, err := s.githubDeliveries.MarkDeliveredSystem(r.Context(), deliveryInstallationID(body), deliveryID)
	if err != nil {
		internalError(w, "github-webhook", err)
		return
	}
	if !fresh {
		// Losing is terminal for this request and carries no retry: GitHub was
		// already told the first copy succeeded, and reporting failure on the
		// duplicate would provoke exactly the redelivery this dedup exists to
		// absorb. Debug, not warn — an operator replaying a delivery is
		// ordinary, not a fault.
		githubAppLog.Debug("dropping duplicate github webhook delivery",
			"org", orgID, "event", eventName, "delivery", deliveryID)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if eventName == "installation" {
		s.handleInstallationEvent(w, r, orgID, deliveryID, body)
		return
	}

	s.publishWebhookEvent(orgID, eventName, deliveryID)
	w.WriteHeader(http.StatusNoContent)
}

// deliveryInstallationID reads the installation id out of a delivery body for
// the dedup key. Every App-scoped event carries the installation object
// whatever its event name, so this one parse serves the lifecycle events and
// the content events alike.
//
// Returns "" — keying the delivery on its GUID alone — for a delivery that
// names no installation (github_app_authorization, for one) and for a body
// this can't parse. A malformed body is not this function's to report: the
// installation handler already refuses one with a 400, and it is the only
// caller that needs the payload to be well-formed.
func deliveryInstallationID(body []byte) string {
	var envelope struct {
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Installation.ID == 0 {
		return ""
	}
	return strconv.FormatInt(envelope.Installation.ID, 10)
}

// installationWebhook is the subset of the installation event payload the
// lifecycle handler needs. The account block mirrors GitHub's verbatim
// "User" / "Organization" type, which is exactly what the
// org_github_app_installations CHECK constraint accepts, and carries both
// halves of the account's identity: the numeric id the mirror resolves on and
// the login it displays. suspended_at / suspended_by are nullable on the wire;
// encoding/json leaves a null as the zero value, so an unsuspended
// installation reads back as a zero time and an empty login.
type installationWebhook struct {
	Action       string `json:"action"`
	Installation struct {
		ID      int64 `json:"id"`
		Account struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"account"`
		CreatedAt   time.Time `json:"created_at"`
		SuspendedAt time.Time `json:"suspended_at"`
		SuspendedBy struct {
			Login string `json:"login"`
		} `json:"suspended_by"`
	} `json:"installation"`
}

// handleInstallationEvent applies a verified installation event to the
// mirror. created upserts, deleted soft-removes, suspend / unsuspend stamp
// and clear the suspension columns; deleted and suspend additionally fire the
// token-cache invalidate hook, because both leave every token already minted
// from the installation dead while the cache would keep serving one until its
// natural expiry. Any other action (new_permissions_accepted, …) is published
// to the bus and acked — the mirror doesn't track those states.
func (s *Server) handleInstallationEvent(w http.ResponseWriter, r *http.Request, orgID, deliveryID string, body []byte) {
	var p installationWebhook
	if err := json.Unmarshal(body, &p); err != nil {
		badRequest(w, "malformed installation payload")
		return
	}
	if p.Installation.ID == 0 {
		badRequest(w, "installation payload missing installation id")
		return
	}
	installationID := strconv.FormatInt(p.Installation.ID, 10)

	// A payload that omits the account id (0) writes "" and so leaves any
	// stored id alone — the upsert fills the column in opportunistically
	// rather than treating a partial payload as an erasure.
	var accountID string
	if p.Installation.Account.ID != 0 {
		accountID = strconv.FormatInt(p.Installation.Account.ID, 10)
	}

	switch p.Action {
	case "created":
		// No suspension is carried: a just-created installation is not
		// suspended, and the upsert writes the zero verbatim — which is what
		// clears an inherited suspension when this `created` is a RE-install
		// over a row that was suspended before it was removed. The account
		// re-installed the App; they did not re-install its suspension.
		if err := s.githubApps.UpsertInstallation(r.Context(), domain.OrgGitHubAppInstallation{
			InstallationID: installationID,
			OrgID:          orgID,
			AccountType:    p.Installation.Account.Type,
			AccountID:      accountID,
			AccountLogin:   p.Installation.Account.Login,
			InstalledAt:    p.Installation.CreatedAt,
		}); err != nil {
			internalError(w, "github-webhook", err)
			return
		}
	case "deleted":
		if err := s.githubApps.MarkInstallationRemoved(r.Context(), orgID, installationID); err != nil {
			internalError(w, "github-webhook", err)
			return
		}
		s.invalidateInstallationToken(orgID, installationID)
	case "suspend":
		// A suspension is not a removal: the row stays live and the grant
		// survives, so only the suspension columns move. GitHub stamps
		// suspended_at on the payload, but a delivery that omits it still
		// describes an installation that is suspended right now — falling back
		// to the receipt time records the state rather than dropping it, since
		// a zero timestamp IS "not suspended" in the mirror.
		suspendedAt := p.Installation.SuspendedAt
		if suspendedAt.IsZero() {
			suspendedAt = time.Now().UTC()
		}
		if err := s.githubApps.SetInstallationSuspension(r.Context(), orgID, installationID, suspendedAt, p.Installation.SuspendedBy.Login); err != nil {
			internalError(w, "github-webhook", err)
			return
		}
		// Every token minted from a suspended installation is refused from this
		// moment, and the cached one outlives the suspension by up to an hour.
		s.invalidateInstallationToken(orgID, installationID)
	case "unsuspend":
		// Restores the prior state exactly: both columns back to NULL, nothing
		// else on the row touched. No cache work — the installation mints again,
		// and the entry the suspend dropped is simply re-minted on next use.
		if err := s.githubApps.SetInstallationSuspension(r.Context(), orgID, installationID, time.Time{}, ""); err != nil {
			internalError(w, "github-webhook", err)
			return
		}
	default:
		s.publishWebhookEvent(orgID, "installation", deliveryID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// publishWebhookEvent emits a lean signal onto the bus for a verified
// non-lifecycle delivery. The full-payload routing + content parsing is a
// downstream concern; this carries just the GitHub event name and delivery
// GUID so a subscriber can correlate. The "webhook:github:" namespace keeps
// raw deliveries off the "github:"/"jira:" router path (poller-derived,
// catalog-registered events) until the content pipeline opts in.
func (s *Server) publishWebhookEvent(orgID, eventName, deliveryID string) {
	if s.bus == nil {
		return
	}
	meta, _ := json.Marshal(map[string]string{
		"event":       eventName,
		"delivery_id": deliveryID,
	})
	s.bus.Publish(domain.Event{
		OrgID:        orgID,
		EventType:    "webhook:github:" + eventName,
		MetadataJSON: string(meta),
	})
}

// validWebhookSignature checks the X-Hub-Signature-256 header against an
// HMAC-SHA256 of the raw body keyed by the org's webhook secret. The
// comparison is constant-time.
func validWebhookSignature(secret string, body []byte, header string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	want, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(want, mac.Sum(nil))
}
