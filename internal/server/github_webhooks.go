package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
// Every refusal that depends on the org — no App, a PAT org, a bad signature —
// is the same bare 401, so the reply carries no information about which orgs
// exist or which of them have an App. The mount is rate-limited (routes(),
// signed-webhook tier) because everything up to that 401 costs reads.
func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("org_id")
	if _, err := uuid.Parse(orgID); err != nil {
		// A 404 here says nothing about any org: the path isn't an org id at
		// all, so this is a statement about the URL rather than about what is
		// behind it. Every org-dependent refusal below is a uniform 401.
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

	// Resolve the secret this delivery has to verify against — credential
	// class, then registration row, then vault entry — behind the short-TTL
	// per-org cache in github_webhook_secret.go, so a flood costs one
	// resolution per org per window rather than three reads per request. An
	// org with nothing to verify against resolves to "", which is answered
	// below exactly as a bad signature is.
	//
	// TODO(TFAC-802): those reads still happen BEFORE verification. Per-org
	// webhook URLs force the order — the secret is reachable only through the
	// org, so org → secret → verify is the only one available. The
	// shared-App receiver inverts it, resolving a deployment-level secret
	// before any org is known.
	secret, err := s.webhookSecretFor(r.Context(), orgID)
	if err != nil {
		internalError(w, "github-webhook", err)
		return
	}
	if secret == "" || !validWebhookSignature(secret, body, sigHeader) {
		// One reply for both, deliberately. Answering "this org has no App"
		// with a 404 and "your signature is wrong" with a 401 would let an
		// unauthenticated caller walk a list of org ids and learn which
		// workspaces have a GitHub App registered — an org-existence oracle on
		// a route that requires no credential to reach. Note the empty secret
		// must short-circuit rather than fall into the HMAC check: verifying
		// against an empty key is a check anyone can pass. Deliberately no
		// body and no payload logging either way.
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	// Signature verified past this point.
	if eventName == "installation" {
		s.handleInstallationEvent(w, r, orgID, body)
		return
	}

	s.publishWebhookEvent(orgID, eventName, r.Header.Get("X-GitHub-Delivery"))
	w.WriteHeader(http.StatusNoContent)
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
func (s *Server) handleInstallationEvent(w http.ResponseWriter, r *http.Request, orgID string, body []byte) {
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
		// Which GitHub this installation lives on is the org's configured base
		// URL — a delivery arrives over the org's own webhook secret, so the
		// deployment that sent it is the one the org is pointed at. Resolved
		// here rather than defaulted in the store, because a GHES org whose
		// host we failed to read would be mirrored as a github.com
		// installation, which is exactly the confusion the column exists to
		// prevent. A read failure fails the delivery instead: GitHub retries,
		// and no row claims a host nobody established.
		host, err := s.orgGitHubHost(r.Context(), orgID)
		if err != nil {
			internalError(w, "github-webhook", err)
			return
		}
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
			GitHubHost:     host,
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
		s.publishWebhookEvent(orgID, "installation", r.Header.Get("X-GitHub-Delivery"))
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
