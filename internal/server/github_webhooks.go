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
// Every refusal that depends on the org — no App, a PAT org, a bad signature —
// is the same bare 401, so the reply carries no information about which orgs
// exist or which of them have an App. The mount is rate-limited (routes(),
// signed-webhook tier) because everything up to that 401 costs reads.
//
// A verified delivery is then deduped on its X-GitHub-Delivery GUID before any
// of the work above runs — see the gate below, which sits inside the rate limit
// and after verification so neither a flood nor a forgery can claim a GUID.
// Structural validation stays ahead of the gate, so a delivery that is bad on
// its face answers 4xx on every attempt rather than 4xx once and then 204 as a
// "duplicate".
func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("org_id")
	if _, err := uuid.Parse(orgID); err != nil {
		// A 404 here says nothing about any org: the path isn't an org id at
		// all, so this is a statement about the URL rather than about what is
		// behind it. Every org-dependent refusal below is a uniform 401.
		http.NotFound(w, r)
		return
	}

	eventName, sigHeader, body, ok := readWebhookDelivery(w, r)
	if !ok {
		return
	}

	// Resolve the secret this delivery has to verify against — credential
	// class, then registration row, then vault entry — behind the short-TTL
	// per-org cache in github_webhook_secret.go, so a flood costs one
	// resolution per org per window rather than three reads per request. An
	// org with nothing to verify against resolves to "", which is answered
	// below exactly as a bad signature is.
	//
	// Those reads happen BEFORE verification, and the per-org URL is what
	// forces the order: the secret is reachable only through the org, so
	// org → secret → verify is the only order available here. The cache and
	// the limiter bound what an unauthenticated caller can spend on it. The
	// deployment receiver (handleDeploymentGitHubWebhook) has the other order
	// — its one secret is in hand before any org is, so nothing there reads a
	// store until the signature has passed.
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
	d, ok := parseVerifiedDelivery(w, r, eventName, body)
	if !ok {
		return
	}
	s.applyWebhookDelivery(w, r, orgID, eventName, d)
}

// readWebhookDelivery performs the checks both receivers run before either
// has a secret in hand: the event name, the presence of a signature header, and
// a bounded read of the body. Every refusal here is about the request's shape
// and depends on nothing behind the route, so it is the same answer on every
// attempt and touches no store. ok=false means a reply has been written.
func readWebhookDelivery(w http.ResponseWriter, r *http.Request) (eventName, sigHeader string, body []byte, ok bool) {
	eventName = r.Header.Get("X-GitHub-Event")
	if eventName == "" {
		badRequest(w, "missing X-GitHub-Event header")
		return "", "", nil, false
	}
	sigHeader = r.Header.Get("X-Hub-Signature-256")
	if sigHeader == "" {
		// No signature to check against — treat as unauthenticated.
		w.WriteHeader(http.StatusUnauthorized)
		return "", "", nil, false
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody+1))
	if err != nil {
		badRequest(w, "could not read request body")
		return "", "", nil, false
	}
	if len(body) > maxWebhookBody {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return "", "", nil, false
	}
	return eventName, sigHeader, body, true
}

// verifiedDelivery is what a delivery is reduced to between the signature
// check and the dedup gate: its GUID, the parsed payload when it is an
// installation event, and the installation id that keys the gate — "" for a
// delivery naming no installation.
type verifiedDelivery struct {
	deliveryID     string
	install        *installationWebhook
	installationID string
}

// parseVerifiedDelivery runs the structural checks a VERIFIED delivery has to
// pass before anything is recorded about it: the delivery GUID, and for an
// installation event the payload itself. Both are properties of the delivery
// alone, so a failure here is a 4xx that repeats on every attempt — which is
// what makes it safe to run ahead of the dedup gate and unsafe to run behind
// it (see applyWebhookDelivery). No store is read. ok=false means a reply has
// been written.
func parseVerifiedDelivery(w http.ResponseWriter, r *http.Request, eventName string, body []byte) (verifiedDelivery, bool) {
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
		return verifiedDelivery{}, false
	}

	// Structural validation of the payload runs BEFORE the gate, so a delivery
	// that is bad on its face keeps its 4xx on every attempt. Recording it
	// first would answer the operator's redelivery with a 204 — the receiver
	// reporting success for a payload it never applied and never could. What
	// this leaves behind the gate is only work that can fail on TF's state
	// rather than on the delivery, and every one of those answers 5xx.
	var install *installationWebhook
	if eventName == "installation" {
		var perr error
		if install, perr = parseInstallationWebhook(body); perr != nil {
			badRequest(w, perr.Error())
			return verifiedDelivery{}, false
		}
	}

	// The installation half of the dedup key. An installation delivery was just
	// parsed in full and its id validated non-zero, so it is already in hand;
	// only the other events pay a narrow sniff of the body for it.
	var installationID string
	if install != nil {
		installationID = strconv.FormatInt(install.Installation.ID, 10)
	} else {
		installationID = deliveryInstallationID(body)
	}
	return verifiedDelivery{deliveryID: deliveryID, install: install, installationID: installationID}, true
}

// applyWebhookDelivery is the tail both receivers share once the tenant is
// known: the dedup gate, then the installation-lifecycle write or the bus
// publish, under orgID. It is the first thing on either route that writes,
// which is why the gate is its first move — a delivery that reaches here has
// been verified and structurally validated, and everything it does from here
// is a side effect the gate has to precede.
func (s *Server) applyWebhookDelivery(w http.ResponseWriter, r *http.Request, orgID, eventName string, d verifiedDelivery) {
	fresh, err := s.githubDeliveries.MarkDeliveredSystem(r.Context(), d.installationID, d.deliveryID)
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
			"org", orgID, "event", eventName, "delivery", d.deliveryID)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if d.install != nil {
		s.applyInstallationEvent(w, r, orgID, d.deliveryID, *d.install)
		return
	}

	s.publishWebhookEvent(orgID, eventName, d.deliveryID)
	w.WriteHeader(http.StatusNoContent)
}

// deliveryInstallationID sniffs the installation id out of a delivery body for
// the dedup key. Every App-scoped event carries the installation object
// whatever its event name, so one narrow envelope serves them all — except the
// installation events themselves, whose caller already holds the fully parsed
// payload and passes its id straight through.
//
// Returns "" — keying the delivery on its GUID alone — for a delivery that
// names no installation (github_app_authorization, for one) and for a body
// this can't parse. A malformed body is not this function's to report: an
// installation delivery is already refused ahead of the gate, and no other
// event requires a parseable payload to be recorded and published.
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
		// repository_selection: "all" or "selected". The latency optimization on
		// top of the reconcile — a fresh install's grant width lands the moment
		// the delivery does instead of at the next poll. The reconcile is what
		// makes it CORRECT, since GitHub never re-sends a delivery it failed to
		// deliver and local mode receives none at all.
		RepositorySelection string `json:"repository_selection"`
	} `json:"installation"`
}

// parseInstallationWebhook decodes and structurally validates an installation
// delivery. Both failures it reports are properties of the payload itself —
// nothing about TF's state can change either answer — so they are identical on
// every attempt, which is what makes this safe to run ahead of the dedup gate
// and unsafe to run behind it.
func parseInstallationWebhook(body []byte) (*installationWebhook, error) {
	var p installationWebhook
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, errors.New("malformed installation payload")
	}
	if p.Installation.ID == 0 {
		return nil, errors.New("installation payload missing installation id")
	}
	return &p, nil
}

// applyInstallationEvent applies a verified, parsed, deduped installation event
// to the mirror. created upserts, deleted soft-removes, suspend / unsuspend
// stamp and clear the suspension columns; deleted and suspend additionally fire
// the token-cache invalidate hook, because both leave every token already
// minted from the installation dead while the cache would keep serving one
// until its natural expiry. Any other action (new_permissions_accepted, …) is
// published to the bus and acked — the mirror doesn't track those states.
//
// Every failure here is a store failure, answered 5xx: the payload was
// validated before the delivery was recorded, so nothing in this function can
// discover that the delivery was bad.
func (s *Server) applyInstallationEvent(w http.ResponseWriter, r *http.Request, orgID, deliveryID string, p installationWebhook) {
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
		// prevent. A read failure fails the delivery instead, so no row claims a
		// host nobody established. Nothing replays that delivery — GitHub does
		// not auto-retry, and the dedup gate above has already recorded it, so
		// a manual redelivery is dropped as a duplicate — but the
		// /app/installations reconcile mints the row on its next pass.
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
		if _, err := s.githubApps.UpsertInstallation(r.Context(), domain.OrgGitHubAppInstallation{
			InstallationID: installationID,
			OrgID:          orgID,
			AccountType:    p.Installation.Account.Type,
			AccountID:      accountID,
			AccountLogin:   p.Installation.Account.Login,
			GitHubHost:     host,
			InstalledAt:    p.Installation.CreatedAt,
			// A payload that omits repository_selection writes "" and so leaves
			// any stored width alone, the same fill-in-only rule the account id
			// takes: a writer that didn't look must not erase what a reconcile
			// established.
			RepositorySelection: p.Installation.RepositorySelection,
		}); err != nil {
			internalError(w, "github-webhook", err)
			return
		}
	case "deleted":
		if _, err := s.githubApps.MarkInstallationRemoved(r.Context(), orgID, installationID); err != nil {
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
		if _, err := s.githubApps.SetInstallationSuspension(r.Context(), orgID, installationID, suspendedAt, p.Installation.SuspendedBy.Login); err != nil {
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
		if _, err := s.githubApps.SetInstallationSuspension(r.Context(), orgID, installationID, time.Time{}, ""); err != nil {
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
